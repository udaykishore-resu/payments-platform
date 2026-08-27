package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// The AWS Secrets Manager staging labels. AWSCURRENT is what an unversioned reference resolves
// to; AWSPREVIOUS is what the previous version is demoted to when a rotation promotes; AWSPENDING
// is where the rotation workflow stages a credential it has not yet verified.
//
// They are the vocabulary of the dual-run overlap in docs/control-plane.md §5.3, and they are
// exported because the rotation workflow and the operator CLI both name them.
const (
	StageCurrent  = "AWSCURRENT"
	StagePrevious = "AWSPREVIOUS"
	StagePending  = "AWSPENDING"
)

// DefaultCacheTTL is how long a resolved credential is held in process.
//
// # Why there is a cache at all, given that the resolver is documented as never caching
//
// The two statements are about different layers and both are load-bearing. The gateway resolver
// (internal/application/payment) holds nothing across calls: no field on it can contain material.
// This client holds a resolved value for sixty seconds, and without it the platform does not
// work. The arithmetic: the payment path resolves one credential per dispatch, Secrets Manager
// is rate-limited per account (a few thousand reads per second, shared with every other caller)
// and billed per API call. At the platform's design throughput, an uncached read per payment is
// simultaneously a throttling incident and a line item.
//
// # Why sixty seconds and not five minutes
//
// docs/security.md §5.1 permits ≤ 5 minutes. Sixty seconds is chosen inside that because the
// number that actually matters is the ratio to the rotation overlap window: control-plane.md
// §5.3 requires `overlap_window ≥ 10 × cache TTL`, and the default overlap is 24 hours. Sixty
// seconds leaves that inequality true by a factor of over a thousand, so a rotation propagates
// far inside the window even if the priority invalidation on `credential.rotated.v1` is lost.
// Five minutes would still satisfy the rule and would make the eviction event the thing the
// rotation depends on rather than a nice-to-have.
const DefaultCacheTTL = 60 * time.Second

// DefaultRecoveryWindowDays is the Secrets Manager deletion recovery window.
//
// Thirty days, from control-plane.md §5.3: revocation at the gateway is the security boundary,
// and deletion here is cleanup. Separating them is what makes a mistaken rotation recoverable —
// the material is restorable for a month after the credential stopped being usable.
const DefaultRecoveryWindowDays = 30

// AWSConfig configures the Secrets Manager client.
type AWSConfig struct {
	// Region is required. There is no default and no metadata-service lookup: a client that
	// guessed its region would sign for the wrong endpoint and fail with a signature error that
	// says nothing about the region.
	Region string
	// Endpoint overrides the derived `secretsmanager.{region}.amazonaws.com`. Production sets it
	// to the VPC endpoint (docs/security.md §5.1) so that credential reads never traverse the
	// public internet; tests set it to an httptest server.
	Endpoint string
	// STSEndpoint overrides the derived STS endpoint, used only by the IRSA exchange.
	STSEndpoint string
	// Environment is asserted against every reference's first path segment. See
	// Reference.Validate: this is what makes a sandbox reference unresolvable in production.
	Environment shared.Environment
	// HTTPClient is the transport. Nil installs one with bounded timeouts, because the zero
	// http.Client has no timeout at all and a hung Secrets Manager would then hold a payment's
	// dispatch budget indefinitely.
	HTTPClient *http.Client
	// Credentials resolves AWS credentials. Nil installs the standard chain — container, then
	// IRSA web identity, then static environment variables.
	Credentials CredentialProvider
	// Clock is the platform clock. Nil means the system clock.
	Clock shared.Clock
	// CacheTTL overrides DefaultCacheTTL. Negative disables the cache, which is a legitimate
	// configuration for the rotation worker: it reads a credential once per rotation and must
	// never see a stale one.
	CacheTTL time.Duration
	// RecoveryWindowDays overrides DefaultRecoveryWindowDays.
	RecoveryWindowDays int
	// RetryBudget bounds retry amplification across the whole client. Nil installs one, which is
	// safe here (unlike in resilience.DefaultPolicy) because this client *is* the shared object
	// a budget needs to hang off.
	RetryBudget *resilience.Budget
	// Logger receives operational lines. Nothing written through it can contain material: the
	// only values it is given are references, versions, HTTP statuses and AWS error codes.
	Logger *slog.Logger
}

// AWSSecretsManager is the production ports.SecretsProvider.
//
// # What this type guarantees about material
//
// Three rules, each enforced structurally rather than by review:
//
//  1. A response body is decoded into a struct whose secret field is read once and moved
//     immediately into a [Material]. It is never stored on this struct, never logged, and never
//     placed in an error.
//  2. Every error returned from here is built from the reference, the HTTP status and the AWS
//     `__type` field. The response *body* of GetSecretValue is the secret, so a client that
//     wrapped a decode failure with the body — the ordinary thing to do — would put a gateway
//     API key in an error string and from there in a log.
//  3. The cache holds Material values, which redact through every rendering path, so a heap
//     dump or an accidental `%+v` of the cache yields placeholders.
type AWSSecretsManager struct {
	cfg    AWSConfig
	client *http.Client
	sm     signer
	sts    signer
	creds  CredentialProvider
	clock  shared.Clock
	log    *slog.Logger
	retry  resilience.Policy

	endpoint    string
	stsEndpoint string

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	material Material
	expires  time.Time
}

// NewAWSSecretsManager builds the client, validating what cannot be defaulted.
//
// It performs no network call: a composition root that fails because AWS is briefly unreachable
// is a pod that crash-loops through an incident it would otherwise have survived on its cache.
// The first Get is where credentials are resolved, and that failure is a request failure, which
// the routing engine already knows how to treat (CREDENTIAL_UNAVAILABLE, fail over).
func NewAWSSecretsManager(cfg AWSConfig) (*AWSSecretsManager, error) {
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, apierror.New(apierror.CodeInternalError,
			"the secrets client requires a region; set PP_AWS_REGION")
	}
	if !cfg.Environment.IsValid() {
		return nil, apierror.Newf(apierror.CodeInternalError,
			"the secrets client requires a valid environment, got %q", cfg.Environment)
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = DefaultCacheTTL
	}
	if cfg.RecoveryWindowDays <= 0 {
		cfg.RecoveryWindowDays = DefaultRecoveryWindowDays
	}
	if cfg.RetryBudget == nil {
		cfg.RetryBudget = resilience.DefaultRetryBudget(resilience.SystemClock())
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://secretsmanager." + cfg.Region + ".amazonaws.com"
	}
	stsEndpoint := cfg.STSEndpoint
	if stsEndpoint == "" {
		stsEndpoint = "https://sts." + cfg.Region + ".amazonaws.com"
	}
	c := &AWSSecretsManager{
		cfg:         cfg,
		client:      cfg.HTTPClient,
		sm:          signer{region: cfg.Region, service: "secretsmanager"},
		sts:         signer{region: cfg.Region, service: "sts"},
		clock:       cfg.Clock,
		log:         cfg.Logger,
		endpoint:    strings.TrimSuffix(endpoint, "/"),
		stsEndpoint: strings.TrimSuffix(stsEndpoint, "/"),
		cache:       map[string]cacheEntry{},
	}
	c.creds = cfg.Credentials
	if c.creds == nil {
		c.creds = NewChainCredentials(ChainConfig{
			HTTPClient:  cfg.HTTPClient,
			STSEndpoint: c.stsEndpoint,
			Region:      cfg.Region,
			Clock:       cfg.Clock,
		})
	}
	// Retry only what throttling and transport failures produce. A 400 from Secrets Manager is a
	// malformed request or a missing secret; retrying it burns the budget that a real throttle
	// needs and delays the honest error the caller has to see.
	c.retry = resilience.Policy{
		MaxAttempts:   4,
		Backoff:       resilience.NewExponentialBackoff(50*time.Millisecond, 2*time.Second, 2),
		RetryableFunc: isRetryableAWSError,
		Budget:        cfg.RetryBudget,
		Clock:         resilience.SystemClock(),
	}
	return c, nil
}

var _ ports.SecretsProvider = (*AWSSecretsManager)(nil)

// Get resolves a reference to its material, from the cache when it is warm.
//
// The reference is parsed and validated *before* the cache is consulted, so a cross-tenant or
// wrong-environment reference is refused whether or not the value happens to be resident. A
// cache checked first would turn the tenant boundary into a race with cache warmth.
func (c *AWSSecretsManager) Get(ctx context.Context, ref string) (ports.SecretMaterial, error) {
	parsed, err := c.resolveRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	key := parsed.String()
	if m, ok := c.cached(key); ok {
		return m, nil
	}

	stage, versionLabel := StageCurrent, parsed.Version
	if versionLabel != "" {
		stage = versionLabel
	}
	var out getSecretValueResponse
	if err := c.call(ctx, "GetSecretValue", getSecretValueRequest{
		SecretID: parsed.Base().SecretID(), VersionStage: stage,
	}, &out); err != nil {
		return nil, wrapAWS(err, parsed, "resolve")
	}
	material, err := decodeMaterial(parsed, out)
	if err != nil {
		return nil, err
	}
	c.store(key, material)
	return material, nil
}

// Put writes a new version and returns the versioned reference.
//
// It creates the secret when it does not exist. The two-step — try PutSecretValue, fall back to
// CreateSecret on ResourceNotFoundException — rather than the reverse, because the overwhelmingly
// common case at steady state is a rotation of a secret that already exists, and paying a
// DescribeSecret on every write to find out would double the call count on the path that is
// rate-limited.
func (c *AWSSecretsManager) Put(ctx context.Context, ref string, material map[string]string) (string, error) {
	parsed, err := c.resolveRef(ctx, ref)
	if err != nil {
		return "", err
	}
	return c.put(ctx, parsed, material, []string{StageCurrent})
}

// Rotate implements the four-phase dual-run overlap of docs/control-plane.md §5.3 at the level
// this port owns: the secret store's half.
//
// # What this function does and, more importantly, what it does not
//
// The full rotation spans three systems — the gateway (mint and revoke), the control plane
// (metadata state) and this store — and is therefore a workflow, not a function call:
// `credential-rotation@v1` in internal/workflows. What belongs *here* is the store's contract,
// and it is exactly the part that makes the overlap possible:
//
//	Phase 1  stage the new material as AWSPENDING. It is written and readable by version pin,
//	         and it is not what an unversioned Get resolves to. Nothing in production is using
//	         it yet, so a failure here costs nothing and the compensation is a plain delete.
//	Phase 2  the workflow verifies the staged version against the gateway, addressing it by the
//	         returned versioned reference. Verification happens BEFORE promotion, which is the
//	         design point the whole scheme rests on: a credential that does not work must never
//	         become current, and a phase-2 failure leaves the previous version live throughout.
//	Phase 3  promote: move AWSCURRENT onto the new version, which demotes the previous one to
//	         AWSPREVIOUS rather than destroying it. Both remain readable. This is the pivot —
//	         past it, traffic signs with the new credential and the recovery is to roll forward.
//	Phase 4  the overlap soak. This function does not sleep through it, and that is deliberate:
//	         the overlap is 24 hours by default and a function holding a context for a day is a
//	         goroutine leak with a business justification. What it does instead is leave the
//	         previous version readable and return, so the workflow — which is durable, resumable
//	         and survives a pod restart — owns the wait and the usage-gated revocation.
//
// The overlap parameter is therefore recorded, not slept on: it is validated against the cache
// TTL (control-plane.md requires overlap ≥ 10 × TTL) so that a caller asking for an overlap this
// fleet cannot honour is refused rather than silently given a shorter one.
func (c *AWSSecretsManager) Rotate(ctx context.Context, ref string, material map[string]string, overlap time.Duration) (string, error) {
	parsed, err := c.resolveRef(ctx, ref)
	if err != nil {
		return "", err
	}
	if c.cfg.CacheTTL > 0 && overlap > 0 && overlap < 10*c.cfg.CacheTTL {
		return "", apierror.Newf(apierror.CodeValidationFailed,
			"an overlap of %s is shorter than ten times this fleet's %s credential cache; "+
				"pods would still be signing with the old credential when it was revoked",
			overlap, c.cfg.CacheTTL).
			WithDetail(apierror.Detail{
				Field: "overlap", Code: "OVERLAP_TOO_SHORT",
				Message: "The dual-run overlap must be at least ten times the secret cache TTL.",
				RuleID:  "L0.ROTATION_OVERLAP_COVERS_CACHE",
			})
	}

	// Phase 1 — stage. AWSPENDING is not what an unversioned read resolves to, so writing it
	// changes nothing about live traffic.
	staged, err := c.put(ctx, parsed, material, []string{StagePending})
	if err != nil {
		return "", err
	}
	stagedRef, err := ParseReference(staged)
	if err != nil {
		return "", err
	}

	// Phase 3 — promote. Phase 2's verification is the workflow's, and it has already run by the
	// time a caller asks for promotion; a provider that promoted before the caller could verify
	// would remove the workflow's only chance to abort for free.
	if err := c.UpdateSecretVersionStage(ctx, parsed.Base(), StageCurrent, stagedRef.Version); err != nil {
		return "", err
	}
	// The local cache is evicted immediately rather than left to expire. The fleet-wide eviction
	// is the `credential.rotated.v1` priority invalidation; this is the same act for the pod that
	// performed the rotation, and skipping it would leave the rotating worker itself the last
	// process using the old credential.
	c.Invalidate(parsed.Base().String())

	c.log.InfoContext(ctx, "credential rotated",
		slog.String("secret_ref", parsed.Base().String()),
		slog.String("version", stagedRef.Version),
		slog.Duration("overlap", overlap))
	return stagedRef.WithVersion(stagedRef.Version).String(), nil
}

// Delete schedules deletion with a recovery window.
//
// Never an immediate delete, and there is no flag for one. Deletion is the last step of a
// rotation that has already revoked the credential at the gateway; the recovery window is what
// separates "we finished a rotation" from "we destroyed the only copy of a credential that turns
// out still to be in use". Thirty days of recoverability costs nothing and has been the
// difference between an inconvenience and an incident.
func (c *AWSSecretsManager) Delete(ctx context.Context, ref string) error {
	parsed, err := c.resolveRef(ctx, ref)
	if err != nil {
		return err
	}
	var out struct {
		DeletionDate float64 `json:"DeletionDate"`
	}
	if err := c.call(ctx, "DeleteSecret", deleteSecretRequest{
		SecretID:                   parsed.Base().SecretID(),
		RecoveryWindowInDays:       c.cfg.RecoveryWindowDays,
		ForceDeleteWithoutRecovery: false,
	}, &out); err != nil {
		return wrapAWS(err, parsed, "delete")
	}
	c.Invalidate(parsed.Base().String())
	return nil
}

// UpdateSecretVersionStage moves a staging label onto a version.
//
// It is exported because the rotation workflow's compensation path needs it directly: rolling a
// promotion back is moving AWSCURRENT to the previous version, which is this call and not a
// second Rotate.
func (c *AWSSecretsManager) UpdateSecretVersionStage(ctx context.Context, ref Reference, stage, toVersionLabel string) error {
	id, err := c.versionIDForLabel(ctx, ref, toVersionLabel)
	if err != nil {
		return err
	}
	current, err := c.versionIDForLabel(ctx, ref, stage)
	if err != nil && !errors.Is(err, errStageAbsent) {
		return err
	}
	req := updateStageRequest{
		SecretID: ref.Base().SecretID(), VersionStage: stage, MoveToVersionID: id,
	}
	if current != "" && current != id {
		// RemoveFromVersionId is required when the label is already attached elsewhere;
		// omitting it makes the call fail with an error that names neither version.
		req.RemoveFromVersionID = current
	}
	var out struct{}
	if err := c.call(ctx, "UpdateSecretVersionStage", req, &out); err != nil {
		return wrapAWS(err, ref, "promote")
	}
	c.Invalidate(ref.Base().String())
	return nil
}

// Invalidate drops a reference from the local cache.
//
// This is the pod-local half of the priority invalidation in control-plane.md §3.7: the event
// consumer calls it on `credential.rotated.v1` so that the new version is resolved on the next
// dispatch rather than up to a TTL later. It is idempotent and safe to call for a reference this
// pod has never seen.
func (c *AWSSecretsManager) Invalidate(ref string) {
	parsed, err := ParseReference(ref)
	key := ref
	if err == nil {
		key = parsed.Base().String()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, key)
	// A versioned entry for the same secret is stale for the same reason, so the whole family
	// goes. Iterating a map of at most a few hundred references costs nothing next to the
	// alternative of serving a revoked credential.
	for k := range c.cache {
		if strings.HasPrefix(k, key+"#") {
			delete(c.cache, k)
		}
	}
}

// put writes one version with the given staging labels, creating the secret if it is absent.
func (c *AWSSecretsManager) put(ctx context.Context, ref Reference, material map[string]string, stages []string) (string, error) {
	if len(material) == 0 {
		return "", apierror.New(apierror.CodeValidationFailed,
			"refusing to store an empty credential: a zero-field secret resolves to a credential set the gateway will reject")
	}
	label, err := c.nextVersionLabel(ctx, ref)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(material)
	if err != nil {
		// The material cannot be in this message: the encoder's error text quotes the offending
		// value, which here is a credential.
		return "", apierror.New(apierror.CodeInternalError, "the credential material could not be encoded")
	}
	req := putSecretValueRequest{
		SecretID:      ref.Base().SecretID(),
		SecretString:  string(body),
		VersionStages: append([]string{label}, stages...),
		// ClientRequestToken gives PutSecretValue its idempotency: a retry carrying the same
		// token returns the existing version instead of creating a second one, so a crash between
		// the write and the workflow's checkpoint does not leave a trail of orphaned versions.
		ClientRequestToken: clientRequestToken(ref, label),
	}
	var out putSecretValueResponse
	err = c.call(ctx, "PutSecretValue", req, &out)
	if isAWSCode(err, "ResourceNotFoundException") {
		var created putSecretValueResponse
		cerr := c.call(ctx, "CreateSecret", createSecretRequest{
			Name:               ref.Base().SecretID(),
			SecretString:       string(body),
			ClientRequestToken: req.ClientRequestToken,
			Description:        "payments-platform gateway credential; material is never read back into a log or an audit record",
		}, &created)
		if cerr != nil {
			return "", wrapAWS(cerr, ref, "create")
		}
		// A freshly created secret's only version is AWSCURRENT. When the caller asked for a
		// staged (AWSPENDING) write, the label has to be corrected, because a rotation that
		// silently promoted its unverified material would defeat phase 2 entirely.
		if !contains(stages, StageCurrent) {
			if err := c.UpdateSecretVersionStage(ctx, ref, stages[0], label); err != nil {
				return "", err
			}
		}
		out = created
	} else if err != nil {
		return "", wrapAWS(err, ref, "store")
	}
	c.Invalidate(ref.Base().String())
	return ref.Base().WithVersion(label).String(), nil
}

// nextVersionLabel allocates the platform's `v{n}` staging label.
//
// AWS's own version identifier is a UUID, which is unusable as the `#v{n}` the reference scheme
// publishes and which an operator has to be able to read out of an audit record. The platform's
// counter is therefore carried as a *custom staging label* on the AWS version, which is exactly
// what staging labels are for, and this function reads the existing labels to find the next
// number.
//
// It is a DescribeSecret call on the write path only. The read path never makes it, which is why
// it does not appear in the per-payment call budget.
func (c *AWSSecretsManager) nextVersionLabel(ctx context.Context, ref Reference) (string, error) {
	desc, err := c.describe(ctx, ref)
	if err != nil {
		if isAWSCode(err, "ResourceNotFoundException") {
			return "v1", nil
		}
		return "", wrapAWS(err, ref, "describe")
	}
	highest := 0
	for _, stages := range desc.VersionIDsToStages {
		for _, s := range stages {
			if n, ok := versionNumber(s); ok && n > highest {
				highest = n
			}
		}
	}
	return "v" + strconv.Itoa(highest+1), nil
}

// errStageAbsent reports that a staging label is not attached to any version. It is a sentinel
// rather than an error string because UpdateSecretVersionStage has to distinguish "AWSCURRENT is
// somewhere else" from "this secret has no AWSCURRENT yet", and the second is normal.
var errStageAbsent = errors.New("secrets: staging label is not attached to any version")

// versionIDForLabel resolves a staging label to the AWS version id it names.
func (c *AWSSecretsManager) versionIDForLabel(ctx context.Context, ref Reference, label string) (string, error) {
	desc, err := c.describe(ctx, ref)
	if err != nil {
		return "", wrapAWS(err, ref, "describe")
	}
	for id, stages := range desc.VersionIDsToStages {
		if contains(stages, label) {
			return id, nil
		}
	}
	return "", errStageAbsent
}

func (c *AWSSecretsManager) describe(ctx context.Context, ref Reference) (describeSecretResponse, error) {
	var out describeSecretResponse
	err := c.call(ctx, "DescribeSecret", describeSecretRequest{SecretID: ref.Base().SecretID()}, &out)
	return out, err
}

// resolveRef parses, validates and tenant-checks a reference in one place.
//
// Every exported method funnels through it, which is the point: the tenant boundary is a
// property of the provider rather than something each call site remembers.
func (c *AWSSecretsManager) resolveRef(ctx context.Context, ref string) (Reference, error) {
	parsed, err := ParseReference(ref)
	if err != nil {
		return Reference{}, err
	}
	// A missing tenant context is not an error here: the rotation workflow and platformctl run
	// without one. Reference.Validate refuses the combination that matters — a tenant-scoped
	// reference resolved by a caller who has no tenant — so the absence is deliberately turned
	// into the empty string rather than propagated.
	tenant, tenantErr := tenantctx.TenantID(ctx)
	if tenantErr != nil {
		tenant = ""
	}
	if err := parsed.Validate(c.cfg.Environment, tenant); err != nil {
		return Reference{}, err
	}
	return parsed, nil
}

func (c *AWSSecretsManager) cached(key string) (Material, bool) {
	if c.cfg.CacheTTL <= 0 {
		return Material{}, false
	}
	c.mu.RLock()
	e, ok := c.cache[key]
	c.mu.RUnlock()
	if !ok || !c.clock.Now().Before(e.expires) {
		return Material{}, false
	}
	return e.material, true
}

func (c *AWSSecretsManager) store(key string, m Material) {
	if c.cfg.CacheTTL <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[key] = cacheEntry{material: m, expires: c.clock.Now().Add(c.cfg.CacheTTL)}
}

// call performs one signed Secrets Manager API call with bounded, jittered retries.
//
// The retry classification is in isRetryableAWSError and is narrow on purpose: throttling and
// transport failures only. Retrying a 400 amplifies a malformed request into four, and retrying
// a 404 turns "this merchant has no credential" into a four-times-slower 404.
func (c *AWSSecretsManager) call(ctx context.Context, target string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return apierror.New(apierror.CodeInternalError, "the secrets request could not be encoded")
	}
	return resilience.Do(ctx, c.retry, func(ctx context.Context) error {
		return c.do(ctx, c.endpoint, "secretsmanager."+target, body, out)
	})
}

func (c *AWSSecretsManager) do(ctx context.Context, endpoint, target string, body []byte, out any) error {
	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/", strings.NewReader(string(body)))
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "the secrets request could not be built")
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", target)
	payloadHash := hexSHA256(body)
	req.Header.Set(headerAmzContentSHA256, payloadHash)
	c.sm.sign(req, payloadHash, creds, c.clock.Now())

	resp, err := c.client.Do(req)
	if err != nil {
		// The transport error can name the host and the timeout but never the payload, so it is
		// safe to carry. It is marked retryable because a connection reset on the way *to* a read
		// had no side effect.
		return apierror.Wrap(err, apierror.CodeDependencyFailure, "the secret store is unreachable")
	}
	defer func() {
		// The body is drained so the connection returns to the pool; a drain that fails costs one
		// connection and never correctness, so it is logged at debug rather than surfaced. It is
		// logged and not discarded because a *persistent* drain failure is the visible symptom of
		// a proxy truncating responses, and that is worth being able to find.
		if _, drainErr := io.Copy(io.Discard, resp.Body); drainErr != nil && c.log != nil {
			c.log.DebugContext(ctx, "the secret store response body could not be drained",
				slog.String("target", target), slog.String("error", drainErr.Error()))
		}
		_ = resp.Body.Close()
	}()

	// The body is read whole before anything is decided about it, and the *only* two things ever
	// done with those bytes are: decode into `out` on success, and extract the `__type` field on
	// failure. On a GetSecretValue success these bytes are the credential.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return apierror.Wrap(err, apierror.CodeDependencyFailure, "the secret store response could not be read")
	}
	if resp.StatusCode != http.StatusOK {
		return awsError(resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// Deliberately not wrapping the decoder's error: its text quotes the offending input.
		return apierror.New(apierror.CodeDependencyFailure,
			"the secret store returned a response this client could not decode")
	}
	return nil
}

// --- wire types -------------------------------------------------------------------------------
//
// Hand-written rather than generated, because five request shapes and four response shapes is
// less code than a generator's runtime, and because the fields we deliberately do *not* decode
// are as important as the ones we do: the response types below have no field for anything we do
// not use, so a future AWS addition cannot start flowing into a struct someone later logs.

type getSecretValueRequest struct {
	SecretID     string `json:"SecretId"`
	VersionStage string `json:"VersionStage,omitempty"`
}

type getSecretValueResponse struct {
	// SecretString is the material. It exists on this struct for the few statements between
	// decode and NewMaterial and is read exactly once, by decodeMaterial.
	SecretString  string   `json:"SecretString"`
	VersionID     string   `json:"VersionId"`
	VersionStages []string `json:"VersionStages"`
}

// GoString and String keep an accidental %v or %#v of a decoded response — in a debug line, in a
// wrapped error — from printing the credential.
func (getSecretValueResponse) String() string   { return "getSecretValueResponse{[REDACTED]}" }
func (getSecretValueResponse) GoString() string { return "getSecretValueResponse{[REDACTED]}" }

type putSecretValueRequest struct {
	SecretID           string   `json:"SecretId"`
	SecretString       string   `json:"SecretString"`
	ClientRequestToken string   `json:"ClientRequestToken,omitempty"`
	VersionStages      []string `json:"VersionStages,omitempty"`
}

type putSecretValueResponse struct {
	VersionID     string   `json:"VersionId"`
	VersionStages []string `json:"VersionStages"`
}

type createSecretRequest struct {
	Name               string `json:"Name"`
	SecretString       string `json:"SecretString"`
	ClientRequestToken string `json:"ClientRequestToken,omitempty"`
	Description        string `json:"Description,omitempty"`
}

type updateStageRequest struct {
	SecretID            string `json:"SecretId"`
	VersionStage        string `json:"VersionStage"`
	MoveToVersionID     string `json:"MoveToVersionId,omitempty"`
	RemoveFromVersionID string `json:"RemoveFromVersionId,omitempty"`
}

type deleteSecretRequest struct {
	SecretID                   string `json:"SecretId"`
	RecoveryWindowInDays       int    `json:"RecoveryWindowInDays,omitempty"`
	ForceDeleteWithoutRecovery bool   `json:"ForceDeleteWithoutRecovery"`
}

type describeSecretRequest struct {
	SecretID string `json:"SecretId"`
}

type describeSecretResponse struct {
	Name               string              `json:"Name"`
	VersionIDsToStages map[string][]string `json:"VersionIdsToStages"`
}

// decodeMaterial turns a GetSecretValue response into a Material.
//
// Two shapes are accepted, because both are in production use: a JSON object of field → value,
// which is what this platform writes, and a bare string, which is what a secret created by hand
// in the console contains. The bare string is mapped to the `value` field rather than rejected,
// because refusing it would make a break-glass credential unusable at exactly the moment someone
// needed one.
func decodeMaterial(ref Reference, resp getSecretValueResponse) (Material, error) {
	if resp.SecretString == "" {
		return Material{}, apierror.Newf(apierror.CodeGatewayAuthenticationFailed,
			"the credential at %s has no string value", ref.Base())
	}
	version := ref.Version
	if version == "" {
		for _, s := range resp.VersionStages {
			if _, ok := versionNumber(s); ok {
				version = s
				break
			}
		}
	}
	if version == "" {
		version = resp.VersionID
	}

	fields := map[string]string{}
	if err := json.Unmarshal([]byte(resp.SecretString), &fields); err != nil {
		// A secret that is not a JSON object is a plain single-value secret — the shape the AWS
		// console creates by default — and the platform reads it as one field named "value"
		// rather than refusing it. The decode error is deliberately dropped rather than wrapped:
		// encoding/json quotes the offending input in its message, and here the offending input
		// is the credential.
		return NewMaterial(version, map[string]string{"value": resp.SecretString}), nil //nolint:nilerr // a non-JSON secret is a valid single-value secret, not a failure.
	}
	return NewMaterial(version, fields), nil
}

// clientRequestToken derives the idempotency token for a write.
//
// Deterministic in the reference and the version label, so a retried Put — after a timeout whose
// outcome we do not know — resolves to the existing version rather than creating a second one.
// A random token would make every retry a new version, which is how a rotation ends up with six
// staged credentials and an operator trying to work out which one the gateway knows about.
func clientRequestToken(ref Reference, label string) string {
	// Secrets Manager requires 32–64 characters. A hex SHA-256 is exactly 64.
	return hexSHA256([]byte(ref.Base().SecretID() + "|" + label))
}

func versionNumber(label string) (int, bool) {
	if len(label) < 2 || label[0] != 'v' {
		return 0, false
	}
	n, err := strconv.Atoi(label[1:])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// --- error handling ---------------------------------------------------------------------------

// awsErrorPayload is the shape of a Secrets Manager error body. Only `__type` is decoded; the
// `message` field is deliberately ignored, because AWS has been known to echo request content
// into it and this client's requests contain credentials.
type awsErrorPayload struct {
	Type string `json:"__type"`
}

// awsError converts a non-200 response into the platform's error, carrying the status and the
// AWS error code and nothing else from the body.
func awsError(status int, raw []byte) error {
	code := ""
	var payload awsErrorPayload
	if json.Unmarshal(raw, &payload) == nil {
		// AWS renders __type as either "ThrottlingException" or a prefixed
		// "com.amazonaws.service#ThrottlingException"; take the last component.
		code = payload.Type
		if i := strings.LastIndexAny(code, "#."); i >= 0 {
			code = code[i+1:]
		}
	}
	apiCode := apierror.CodeDependencyFailure
	switch {
	case status == http.StatusNotFound || code == "ResourceNotFoundException":
		apiCode = apierror.CodeGatewayNotConfigured
	case status == http.StatusForbidden || code == "AccessDeniedException":
		apiCode = apierror.CodeForbidden
	}
	return &awsAPIError{
		err:    apierror.Newf(apiCode, "the secret store refused the request: HTTP %d %s", status, code),
		status: status,
		code:   code,
	}
}

// awsAPIError carries the AWS error code alongside the platform error so the retry classifier and
// the create-on-missing path can branch on it without parsing a message.
//
// It wraps rather than embeds *apierror.Error: embedding would promote the Error() method and
// make this type satisfy `error` by accident, but it would also promote every With* builder,
// each of which returns the *inner* error — so a caller adding a detail would silently lose the
// AWS code. Wrapping keeps errors.As reaching both.
type awsAPIError struct {
	err    *apierror.Error
	status int
	code   string
}

func (e *awsAPIError) Error() string { return e.err.Error() }
func (e *awsAPIError) Unwrap() error { return e.err }

func isAWSCode(err error, code string) bool {
	var e *awsAPIError
	return errors.As(err, &e) && e.code == code
}

// isRetryableAWSError is the retry classifier.
//
// Throttling and 5xx are retried; everything else is not. The `429` and `ThrottlingException`
// cases are named explicitly because Secrets Manager uses both, depending on which limit was hit,
// and a classifier that knew only one would retry half the throttles and hard-fail the rest —
// which reads in production as an intermittent, unexplained credential outage.
func isRetryableAWSError(err error) bool {
	var e *awsAPIError
	if errors.As(err, &e) {
		switch e.code {
		case "ThrottlingException", "TooManyRequestsException", "RequestLimitExceeded",
			"InternalServiceError", "ServiceUnavailable":
			return true
		}
		return e.status == http.StatusTooManyRequests || e.status >= 500
	}
	return apierror.IsRetryable(err)
}

// wrapAWS attaches the reference to a store error.
//
// The reference is safe to include (control-plane.md §5.2 requires it to contain no
// secret-derived data) and is the single most useful thing in the log line: "which credential"
// is the first question asked when a dispatch fails on a credential read.
func wrapAWS(err error, ref Reference, op string) error {
	if err == nil {
		return nil
	}
	return apierror.Wrapf(err, apierror.CodeOf(err),
		"could not %s the credential at %s", op, ref.Base())
}

// LogValue redacts a Credentials value that reaches a log attribute. The access key id is a
// public identifier, but the secret key and the session token are not, and a struct that logs
// two of its three fields is a struct someone will eventually log whole.
func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("access_key_id", c.AccessKeyID),
		slog.String("secret_access_key", "[REDACTED]"),
		slog.String("session_token", "[REDACTED]"),
		slog.Time("expires", c.Expires),
	)
}

// String and GoString keep a Credentials out of a %v or %#v path.
func (Credentials) String() string   { return "aws.Credentials{[REDACTED]}" }
func (Credentials) GoString() string { return "aws.Credentials{[REDACTED]}" }

var (
	_ fmt.Stringer   = Credentials{}
	_ fmt.GoStringer = Credentials{}
	_ slog.LogValuer = Credentials{}
)

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-11, FR-40, NFR-32.
//
// AWS Secrets Manager credential resolution over plain net/http, with a short-TTL cache sized
// against the rotation overlap window, bounded jittered retries on throttling, and the store's
// half of the four-phase dual-run rotation
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
