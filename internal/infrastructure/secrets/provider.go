package secrets

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Backend names a secrets implementation.
type Backend string

const (
	// BackendAWS is AWS Secrets Manager. The only backend permitted in production.
	BackendAWS Backend = "aws"
	// BackendFile is the file/environment store used by the local stack, the test suites and the
	// gateway simulator.
	BackendFile Backend = "file"
	// BackendAuto picks by environment: file in sandbox, AWS in production. It is the default
	// because it makes the common cases right without configuration and still cannot select the
	// file backend for production — the file provider's own constructor refuses that.
	BackendAuto Backend = ""
)

// Config is everything a composition root needs to build a provider.
//
// It is one struct rather than a discriminated union because a composition root reads it from
// the environment, where every field is a string anyway, and because a union would push the
// "which backend am I building" branch into every main.go instead of keeping it here.
type Config struct {
	// Backend selects the implementation. Empty means BackendAuto.
	Backend Backend
	// Environment is asserted against every reference by both implementations.
	Environment shared.Environment

	// Region, Endpoint and STSEndpoint configure the AWS backend.
	Region      string
	Endpoint    string
	STSEndpoint string
	// CacheTTL overrides DefaultCacheTTL. See that constant for why the number is what it is.
	CacheTTL time.Duration
	// RecoveryWindowDays overrides DefaultRecoveryWindowDays.
	RecoveryWindowDays int

	// Path is the file backend's document.
	Path string
	// AllowFileInProduction is the deliberate, reasoned override described on NewFileProvider.
	AllowFileInProduction bool

	Clock  shared.Clock
	Logger *slog.Logger
}

// New builds the provider this deployment should use.
//
// # Why the choice is made here and not in each main.go
//
// Nine binaries need a secrets provider and they must all make the same choice, for the same
// reason the infrastructure constructors live in internal/platform/runtime: a backend selected
// correctly in eight composition roots and wrongly in the ninth is a defect that appears only in
// the deployable nobody exercises locally, and it appears as a credential outage.
//
// The one decision this function refuses to make implicitly is running the file backend in
// production. BackendAuto never selects it there, an explicit `file` still has to pass
// NewFileProvider's refusal, and both paths lead to an error that says what is missing.
func New(cfg Config) (ports.SecretsProvider, error) {
	if !cfg.Environment.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"the secrets provider requires a valid environment, got %q", cfg.Environment)
	}
	backend := cfg.Backend
	if backend == BackendAuto {
		backend = BackendFile
		if cfg.Environment.IsProduction() {
			backend = BackendAWS
		}
	}
	switch backend {
	case BackendFile:
		return NewFileProvider(FileConfig{
			Path:            cfg.Path,
			Environment:     cfg.Environment,
			AllowProduction: cfg.AllowFileInProduction,
			Clock:           cfg.Clock,
		})
	case BackendAWS:
		return NewAWSSecretsManager(AWSConfig{
			Region:             cfg.Region,
			Endpoint:           cfg.Endpoint,
			STSEndpoint:        cfg.STSEndpoint,
			Environment:        cfg.Environment,
			Clock:              cfg.Clock,
			CacheTTL:           cfg.CacheTTL,
			RecoveryWindowDays: cfg.RecoveryWindowDays,
			Logger:             cfg.Logger,
		})
	case BackendAuto:
		// Unreachable: the resolution above replaces Auto with a concrete backend. It is spelled
		// out rather than folded into the default so that adding a backend to the enum without
		// adding it here fails the exhaustiveness check instead of falling through to an error at
		// process start — a compile-time complaint beats a crash-loop in production.
		return nil, apierror.New(apierror.CodeInternalError,
			"the secrets backend was still \"auto\" after resolution, which is a wiring defect")
	default:
		return nil, apierror.Newf(apierror.CodeInternalError,
			"unknown secrets backend %q: expected \"aws\", \"file\" or empty for automatic", backend)
	}
}

// ParseBackend validates a configured backend name, refusing an unknown one rather than
// defaulting.
//
// Defaulting a typo would be the worst possible behaviour here: `PP_SECRETS_BACKEND=awss` in a
// production manifest would silently select the file backend and the platform would come up
// serving payments against an empty local document.
func ParseBackend(s string) (Backend, error) {
	switch b := Backend(strings.ToLower(strings.TrimSpace(s))); b {
	case BackendAuto, BackendAWS, BackendFile:
		return b, nil
	default:
		return "", apierror.Newf(apierror.CodeValidationFailed,
			"unknown secrets backend %q: expected \"aws\", \"file\" or empty for automatic", s)
	}
}

// RotationPhase names a step of the four-phase dual-run rotation in docs/control-plane.md §5.3.
//
// The phases are a type rather than strings so that an audit record, a metric label and a
// workflow step name cannot spell them differently — which matters because the audit trail of a
// rotation is read months later, by someone reconstructing whether a credential was ever live.
type RotationPhase string

const (
	// PhaseStage writes the new material as AWSPENDING. Fully compensatable: nothing is using it.
	PhaseStage RotationPhase = "STAGE"
	// PhaseVerify is the workflow's L3 probe against the gateway, addressed by version pin. A
	// failure here costs nothing — the previous version was live throughout.
	PhaseVerify RotationPhase = "VERIFY"
	// PhasePromote moves AWSCURRENT, demoting the previous version to AWSPREVIOUS. This is the
	// pivot: past it, recovery is to roll forward.
	PhasePromote RotationPhase = "PROMOTE"
	// PhaseSoak is the overlap window, owned by the durable workflow rather than by any process
	// holding a context. Revocation is gated on observed usage, not on the clock alone.
	PhaseSoak RotationPhase = "SOAK"
)

// Rotator is the subset of a provider that a rotation workflow drives.
//
// It is declared here, next to the phases, because the workflow needs promotion and staged reads
// as separate operations — ports.SecretsProvider's Rotate is the composed convenience, and a
// workflow that could only call the composed form could not put its verification step between
// phase 1 and phase 3, which is the entire point of the design.
type Rotator interface {
	ports.SecretsProvider
	// UpdateSecretVersionStage moves a staging label, which is how phase 3 promotes and how the
	// compensation rolls a promotion back.
	UpdateSecretVersionStage(ctx context.Context, ref Reference, stage, toVersionLabel string) error
	// Invalidate evicts a reference from the local cache, the pod-local half of the
	// `credential.rotated.v1` priority invalidation.
	Invalidate(ref string)
}

var _ Rotator = (*AWSSecretsManager)(nil)

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-11, FR-40, NFR-32.
//
// Backend selection by environment, with the file backend structurally unable to be chosen for
// production, and the vocabulary of the four-phase dual-run rotation
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
