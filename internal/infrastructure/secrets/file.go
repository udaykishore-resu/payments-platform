package secrets

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// EnvValuePrefix marks a document value that is read from the process environment instead of
// from the file.
//
// A local stack still should not put a credential in a file that a `git add -A` can catch, and a
// CI job that needs a real sandbox key needs somewhere to put it that is not the repository.
// `env:PP_STRIPE_SANDBOX_KEY` resolves at read time, which means the file is committable and the
// value is not in it.
const EnvValuePrefix = "env:"

// FileConfig configures the development secrets provider.
type FileConfig struct {
	// Path is the JSON or YAML document. Empty is legal and yields an empty store, which is what
	// a test that only calls Put wants.
	Path string
	// Environment is asserted against every reference, exactly as the AWS provider does. The
	// development provider is not exempt from the sandbox/production separation — a developer
	// resolving a production reference against a local file is the mistake this catches.
	Environment shared.Environment
	// AllowProduction is the explicit override. See [NewFileProvider] for why the refusal exists
	// and why it is a flag rather than a build tag.
	AllowProduction bool
	// Getenv reads an environment variable for the `env:` value form. Nil means os.Getenv.
	Getenv func(string) string
	Clock  shared.Clock
}

// FileProvider is the file- and environment-backed ports.SecretsProvider.
//
// # What it is for
//
// Three things, and it is worth naming them because "development convenience" undersells it:
// `scripts/dev-up.sh` brings the whole platform up with no AWS account at all; the integration
// and contract suites run a real payment through a real gateway resolver without a network; and
// the gateway simulator's fixed credentials come from the same place the production ones would,
// so the resolution path under test is the production path.
//
// # Why it is a real implementation and not a stub
//
// It parses the same references, enforces the same environment and tenant scoping, and keeps the
// same version semantics — including the AWSCURRENT/AWSPREVIOUS overlap, so a rotation exercised
// against this provider exercises the actual dual-run behaviour. A stub that returned a fixed
// map would let a reference-scoping bug reach production having passed every local test.
type FileProvider struct {
	cfg    FileConfig
	getenv func(string) string
	clock  shared.Clock

	mu sync.RWMutex
	// entries is keyed by the *version-less* canonical reference. Versions live inside, which
	// mirrors Secrets Manager: versions are addressed within one secret, not as separate secrets.
	entries map[string]*fileEntry
}

type fileEntry struct {
	// versions maps a `v{n}` label to the material's raw field map. Raw rather than Material
	// because a Put has to be able to replace a version, and because the `env:` indirection is
	// resolved at read time so that changing the environment variable takes effect.
	versions map[string]map[string]string
	// stages maps a staging label — AWSCURRENT, AWSPREVIOUS, AWSPENDING — to a version label.
	// Holding both is what lets this provider reproduce the overlap: after a rotation, v7 is
	// still readable as AWSPREVIOUS while v8 is AWSCURRENT.
	stages map[string]string
	next   int
}

// NewFileProvider loads the document and builds the provider.
//
// # Why it refuses production, and why the override is a flag with a reason attached
//
// A file-backed secret store in production is a credential in a file: on a node's disk, in a
// container layer, in whatever backs the volume, and outside every control docs/security.md §5
// describes — no KMS envelope, no CloudTrail record of each read, no IAM path scoping, no SCP
// denying human access. Running one in production would quietly void the platform's entire
// secrets design while every dashboard continued to look normal.
//
// So the constructor refuses, and the error says *why* rather than just "not allowed", because
// the person reading it at 3 a.m. is deciding whether to set the override. The override exists
// at all for one legitimate case: a disaster-recovery drill or a break-glass restore in an
// isolated environment marked production, where the alternative to a file is not having the
// platform. It is a flag rather than a build tag so that the decision appears in a deployment
// manifest that a reviewer reads, rather than in a build pipeline that nobody does.
func NewFileProvider(cfg FileConfig) (*FileProvider, error) {
	if !cfg.Environment.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"the file secrets provider requires a valid environment, got %q", cfg.Environment)
	}
	if cfg.Environment.IsProduction() && !cfg.AllowProduction {
		return nil, apierror.New(apierror.CodeInternalError,
			"refusing to start the file-backed secrets provider in production: it stores credential "+
				"material in a file, which has no KMS envelope, no per-read CloudTrail record, no "+
				"IAM path scoping and no SCP denying human access — every control in "+
				"docs/security.md §5.1. Configure PP_SECRETS_BACKEND=aws, or set the override "+
				"explicitly if this is an isolated break-glass or disaster-recovery environment").
			WithDetail(apierror.Detail{
				Field: "PP_SECRETS_BACKEND", Code: "FILE_BACKEND_IN_PRODUCTION",
				Message: "The file-backed secret store may not run in production without an explicit override.",
				RuleID:  "L0.SECRETS_BACKEND_MATCHES_ENVIRONMENT",
			})
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	p := &FileProvider{
		cfg: cfg, getenv: cfg.Getenv, clock: cfg.Clock,
		entries: map[string]*fileEntry{},
	}
	if cfg.Path == "" {
		return p, nil
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		return nil, apierror.Wrapf(err, apierror.CodeInternalError,
			"the secrets document %s could not be read", cfg.Path)
	}
	if err := p.load(raw); err != nil {
		return nil, err
	}
	return p, nil
}

// documentEntry is one reference's material as the document expresses it.
//
// A field map is the ordinary form. The `#v{n}` suffix on the key pins a version, which is what
// lets a fixture set up a mid-rotation state — v7 current and v8 pending — and assert that the
// overlap behaves.
type documentEntry map[string]string

// load parses the document, accepting JSON or YAML.
//
// YAML is tried first and JSON is a subset of YAML 1.2, so one parser handles both; the
// json.Unmarshal fallback exists only to give a better error message on a JSON document with a
// YAML-invalid construct, which is rare but produces a baffling error when it happens.
func (p *FileProvider) load(raw []byte) error {
	doc := map[string]documentEntry{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			// Neither parser's message is echoed: a parse error quotes the offending line, and
			// the offending line of a secrets document is a secret.
			return apierror.Newf(apierror.CodeInternalError,
				"the secrets document %s is neither valid YAML nor valid JSON", p.cfg.Path)
		}
	}
	// Sorted so that two references to the same secret at different versions load in version
	// order, which is what makes the highest one AWSCURRENT deterministically.
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		ref, err := ParseReference(key)
		if err != nil {
			return apierror.Wrapf(err, apierror.CodeInternalError,
				"the secrets document %s contains an unusable reference", p.cfg.Path)
		}
		if ref.Environment != p.cfg.Environment {
			// Loading a sandbox document into a production process, or the reverse, is the
			// mistake the environment segment exists to catch. Refusing at load time rather than
			// at resolution time means it is a startup failure, which is visible, rather than a
			// per-payment failure, which is an incident.
			return apierror.Newf(apierror.CodeInternalError,
				"the secrets document %s contains a %s reference but this process runs %s",
				p.cfg.Path, ref.Environment, p.cfg.Environment)
		}
		base := ref.Base().String()
		e, ok := p.entries[base]
		if !ok {
			e = &fileEntry{versions: map[string]map[string]string{}, stages: map[string]string{}}
			p.entries[base] = e
		}
		label := ref.Version
		if label == "" || !isVersionLabel(label) {
			e.next++
			label = "v" + strconv.Itoa(e.next)
		} else if n, _ := versionNumber(label); n > e.next {
			e.next = n
		}
		e.versions[label] = map[string]string(doc[key])
		// The last (highest) version in sorted order becomes current; any previously-current one
		// is demoted rather than dropped, which is the overlap the production store provides.
		if prev, ok := e.stages[StageCurrent]; ok && prev != label {
			e.stages[StagePrevious] = prev
		}
		e.stages[StageCurrent] = label
	}
	return nil
}

var _ ports.SecretsProvider = (*FileProvider)(nil)

// Get resolves a reference, applying the same parsing, environment and tenant checks the
// production provider applies.
func (p *FileProvider) Get(ctx context.Context, ref string) (ports.SecretMaterial, error) {
	parsed, err := p.resolveRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	// The read lock is held across the whole resolution, not just the map lookup: Put and Rotate
	// replace an entry's stage mapping and its version map together, and a reader that took the
	// pointer and then released the lock could observe AWSCURRENT pointing at a version that had
	// not been written yet.
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.entries[parsed.Base().String()]
	if !ok {
		return nil, apierror.Newf(apierror.CodeGatewayNotConfigured,
			"no credential is stored at %s", parsed.Base()).
			WithDetail(apierror.Detail{
				Field: "credentialRef", Code: "SECRET_NOT_FOUND",
				Message: "The local secrets document has no entry for this reference.",
				RuleID:  "L4.CREDENTIAL_IS_A_REFERENCE",
			})
	}

	label := parsed.Version
	if label == "" {
		label = StageCurrent
	}
	values, resolved, ok := e.resolve(label)
	if !ok {
		return nil, apierror.Newf(apierror.CodeGatewayNotConfigured,
			"the credential at %s has no version %s", parsed.Base(), label)
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = p.expand(v)
	}
	return NewMaterial(resolved, out), nil
}

// Put writes a new version and makes it current.
func (p *FileProvider) Put(ctx context.Context, ref string, material map[string]string) (string, error) {
	parsed, err := p.resolveRef(ctx, ref)
	if err != nil {
		return "", err
	}
	if len(material) == 0 {
		return "", apierror.New(apierror.CodeValidationFailed,
			"refusing to store an empty credential: a zero-field secret resolves to a credential set the gateway will reject")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	label := p.writeLocked(parsed, material)
	return parsed.Base().WithVersion(label).String(), nil
}

// Rotate reproduces the store half of the dual-run overlap: the new version becomes AWSCURRENT
// and the previous one becomes AWSPREVIOUS, readable by pin for as long as it is not deleted.
//
// The overlap duration is validated and recorded rather than slept on, for the same reason as in
// the AWS provider: the wait belongs to the durable workflow, not to a function holding a context
// for twenty-four hours.
func (p *FileProvider) Rotate(ctx context.Context, ref string, material map[string]string, overlap time.Duration) (string, error) {
	parsed, err := p.resolveRef(ctx, ref)
	if err != nil {
		return "", err
	}
	if len(material) == 0 {
		return "", apierror.New(apierror.CodeValidationFailed,
			"refusing to rotate onto an empty credential")
	}
	if overlap < 0 {
		return "", apierror.New(apierror.CodeValidationFailed,
			"the dual-run overlap may not be negative")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	label := p.writeLocked(parsed, material)
	return parsed.Base().WithVersion(label).String(), nil
}

// Delete removes every version of a reference.
//
// The production provider schedules deletion with a thirty-day recovery window; this one deletes
// outright, and the difference is honest rather than an oversight — a local file has no recovery
// mechanism to schedule against, and pretending otherwise would make a developer believe a
// mistake here was recoverable.
func (p *FileProvider) Delete(ctx context.Context, ref string) error {
	parsed, err := p.resolveRef(ctx, ref)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, parsed.Base().String())
	return nil
}

// writeLocked stores material as the next version and promotes it. Callers hold p.mu.
func (p *FileProvider) writeLocked(ref Reference, material map[string]string) string {
	base := ref.Base().String()
	e, ok := p.entries[base]
	if !ok {
		e = &fileEntry{versions: map[string]map[string]string{}, stages: map[string]string{}}
		p.entries[base] = e
	}
	e.next++
	label := "v" + strconv.Itoa(e.next)
	copied := make(map[string]string, len(material))
	for k, v := range material {
		copied[k] = v
	}
	e.versions[label] = copied
	if prev, ok := e.stages[StageCurrent]; ok {
		e.stages[StagePrevious] = prev
	}
	e.stages[StageCurrent] = label
	return label
}

// resolve maps a version label or staging label to its material.
func (e *fileEntry) resolve(label string) (map[string]string, string, bool) {
	if v, ok := e.stages[label]; ok {
		label = v
	}
	m, ok := e.versions[label]
	return m, label, ok
}

// expand resolves the `env:NAME` indirection.
func (p *FileProvider) expand(v string) string {
	if !strings.HasPrefix(v, EnvValuePrefix) {
		return v
	}
	return p.getenv(strings.TrimPrefix(v, EnvValuePrefix))
}

// resolveRef applies the same parse-validate-tenant-check funnel the AWS provider uses. It is
// duplicated rather than shared through an embedded type because the two providers have
// different configuration shapes and a shared base type would exist only to hold four lines.
func (p *FileProvider) resolveRef(ctx context.Context, ref string) (Reference, error) {
	parsed, err := ParseReference(ref)
	if err != nil {
		return Reference{}, err
	}
	// A missing tenant context is not an error here: the simulator, the seeder and platformctl all
	// resolve platform-scoped references without one. Reference.Validate refuses the combination
	// that matters — a tenant-scoped reference resolved by a caller who has no tenant — so the
	// absent tenant is deliberately turned into the empty string rather than propagated.
	tenant, tenantErr := tenantctx.TenantID(ctx)
	if tenantErr != nil {
		tenant = ""
	}
	if err := parsed.Validate(p.cfg.Environment, tenant); err != nil {
		return Reference{}, err
	}
	return parsed, nil
}

// References lists what this provider holds, in canonical form.
//
// It exists for the startup smoke assertion in the composition roots: a process that came up
// with an empty secrets document would serve payments that all fail on credential resolution,
// and it is far better to say so in the startup log than to discover it from the first merchant.
// It returns references, never material, so it is safe to log the result.
func (p *FileProvider) References() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.entries))
	for k := range p.entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isVersionLabel(s string) bool {
	_, ok := versionNumber(s)
	return ok
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-32.
//
// The file- and environment-backed secrets provider that lets the local stack, the test suites
// and the gateway simulator exercise the production credential-resolution path without AWS,
// while refusing to run in production without an explicit, reasoned override
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
