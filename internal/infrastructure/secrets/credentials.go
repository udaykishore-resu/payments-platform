package secrets

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// CredentialProvider resolves AWS credentials for the signer.
//
// It is one method because the only thing a caller ever needs is "give me credentials that work
// right now"; refresh, expiry and single-flight are the provider's problem, not the caller's.
// Making them the caller's is how a fleet ends up with every pod refreshing simultaneously on
// the hour.
type CredentialProvider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

// refreshWindow is how long before expiry a cached credential is treated as due for refresh.
//
// Five minutes, against STS's one-hour session: long enough that a refresh has time to fail and
// be retried before anything expires, short enough that a pod is not refreshing continuously. A
// zero window is the classic mistake — the refresh fires when the credential is already dead,
// so every refresh failure is immediately a request failure.
const refreshWindow = 5 * time.Minute

// ChainConfig configures the standard credential chain.
type ChainConfig struct {
	HTTPClient  *http.Client
	STSEndpoint string
	Region      string
	Clock       shared.Clock
	// Env reads an environment variable. Injectable so the chain's ordering can be tested
	// without mutating the process environment, which no parallel test can do safely.
	Env func(string) string
	// ReadFile reads the projected service-account token. Injectable for the same reason.
	ReadFile func(string) ([]byte, error)
}

// chainCredentials resolves credentials in the standard order, caching the result until shortly
// before it expires and collapsing concurrent refreshes into one.
//
// # Why this order
//
// It is the order the AWS SDKs use, and the reason is that each source is more specific than the
// one after it:
//
//  1. **Container credentials** (`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`). Set by ECS and by
//     EKS Pod Identity. If the platform put a credential endpoint in front of this process, that
//     is the credential it is meant to use.
//  2. **IRSA web identity** (`AWS_WEB_IDENTITY_TOKEN_FILE` + `AWS_ROLE_ARN`). The Kubernetes
//     projected service-account token exchanged at STS for a role session. This is what
//     production actually uses (docs/security.md §1: "Service→AWS: IRSA, no static keys"), and
//     it is second only because a container credential endpoint, when present, is more specific.
//  3. **Static environment credentials**. Local development and CI. In production these do not
//     exist, and that is enforced outside this code: an admission policy rejects a pod spec
//     carrying an env var whose name matches the credential pattern (docs/security.md §5.2).
//
// Falling through to the next source only on *absence*, never on failure, is the load-bearing
// detail. A misconfigured IRSA role that fell through to static keys would silently run
// production under whatever credentials happened to be in the environment — the failure mode
// where the least-privilege IAM design is bypassed and nothing reports it.
type chainCredentials struct {
	cfg    ChainConfig
	client *http.Client
	clock  shared.Clock
	getenv func(string) string
	read   func(string) ([]byte, error)

	mu     sync.Mutex
	cached Credentials
	// inflight is the single-flight guard: the first goroutine to find the cache cold owns the
	// refresh and everybody else waits on this channel. Without it, a cold start with a hundred
	// concurrent payments makes a hundred simultaneous AssumeRoleWithWebIdentity calls, which STS
	// throttles — turning a cache miss into an outage.
	inflight chan struct{}
	// err is the outcome of the in-flight refresh, published to the waiters.
	err error
}

// NewChainCredentials builds the standard chain.
func NewChainCredentials(cfg ChainConfig) CredentialProvider {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	if cfg.Env == nil {
		cfg.Env = os.Getenv
	}
	if cfg.ReadFile == nil {
		cfg.ReadFile = os.ReadFile
	}
	if cfg.STSEndpoint == "" && cfg.Region != "" {
		cfg.STSEndpoint = "https://sts." + cfg.Region + ".amazonaws.com"
	}
	return &chainCredentials{
		cfg:    cfg,
		client: cfg.HTTPClient,
		clock:  cfg.Clock,
		getenv: cfg.Env,
		read:   cfg.ReadFile,
	}
}

// Retrieve returns credentials, refreshing them if they are within the refresh window of expiry.
func (c *chainCredentials) Retrieve(ctx context.Context) (Credentials, error) {
	c.mu.Lock()
	if c.fresh() {
		creds := c.cached
		c.mu.Unlock()
		return creds, nil
	}
	if c.inflight != nil {
		// Another goroutine owns the refresh. Wait for it rather than starting a second.
		wait := c.inflight
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return Credentials{}, apierror.Wrap(ctx.Err(), apierror.CodeDependencyFailure,
				"the caller gave up while AWS credentials were being refreshed")
		}
		c.mu.Lock()
		creds, err := c.cached, c.err
		c.mu.Unlock()
		if err != nil {
			return Credentials{}, err
		}
		return creds, nil
	}
	done := make(chan struct{})
	c.inflight = done
	c.mu.Unlock()

	creds, err := c.resolve(ctx)

	c.mu.Lock()
	c.err = err
	if err == nil {
		c.cached = creds
	}
	c.inflight = nil
	c.mu.Unlock()
	close(done)

	if err != nil {
		return Credentials{}, err
	}
	return creds, nil
}

// fresh reports whether the cached credential is usable without a refresh. Must be called with
// the mutex held.
func (c *chainCredentials) fresh() bool {
	if c.cached.AccessKeyID == "" {
		return false
	}
	if c.cached.Expires.IsZero() {
		// Static credentials never expire. Re-reading the environment on every request would be
		// pure syscall overhead on the payment path.
		return true
	}
	return c.clock.Now().Add(refreshWindow).Before(c.cached.Expires)
}

// resolve walks the chain. See the type comment for why the order is what it is and why an
// absent source falls through while a failing one does not.
func (c *chainCredentials) resolve(ctx context.Context) (Credentials, error) {
	if uri := c.getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); uri != "" {
		return c.fromContainer(ctx, "http://169.254.170.2"+uri)
	}
	if uri := c.getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"); uri != "" {
		return c.fromContainer(ctx, uri)
	}
	tokenFile := c.getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
	roleARN := c.getenv("AWS_ROLE_ARN")
	if tokenFile != "" && roleARN != "" {
		return c.fromWebIdentity(ctx, tokenFile, roleARN, c.getenv("AWS_ROLE_SESSION_NAME"))
	}
	if id := c.getenv("AWS_ACCESS_KEY_ID"); id != "" {
		key := c.getenv("AWS_SECRET_ACCESS_KEY")
		if key == "" {
			return Credentials{}, apierror.New(apierror.CodeInternalError,
				"AWS_ACCESS_KEY_ID is set without AWS_SECRET_ACCESS_KEY")
		}
		return Credentials{
			AccessKeyID:     id,
			SecretAccessKey: key,
			SessionToken:    c.getenv("AWS_SESSION_TOKEN"),
		}, nil
	}
	// Naming all three sources is the point of this message: the operator reading it in a
	// crash-looping pod's log needs to know which of the three they were supposed to configure,
	// and "no credentials found" has sent people to the wrong one many times.
	return Credentials{}, apierror.New(apierror.CodeInternalError,
		"no AWS credentials: expected AWS_CONTAINER_CREDENTIALS_RELATIVE_URI, "+
			"AWS_WEB_IDENTITY_TOKEN_FILE with AWS_ROLE_ARN, or AWS_ACCESS_KEY_ID with AWS_SECRET_ACCESS_KEY")
}

// containerCredentialResponse is the ECS/Pod-Identity credential endpoint's payload.
type containerCredentialResponse struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

func (containerCredentialResponse) String() string   { return "containerCredentials{[REDACTED]}" }
func (containerCredentialResponse) GoString() string { return "containerCredentials{[REDACTED]}" }

// fromContainer reads the credential endpoint the container runtime provides.
func (c *chainCredentials) fromContainer(ctx context.Context, endpoint string) (Credentials, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Credentials{}, apierror.Wrap(err, apierror.CodeInternalError,
			"the container credential endpoint is not a usable URL")
	}
	// The authorization token, where the runtime sets one, is what stops another workload on the
	// same host from reading this pod's credentials off the link-local address.
	if tok := c.getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"); tok != "" {
		req.Header.Set("Authorization", tok)
	} else if f := c.getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); f != "" {
		b, rerr := c.read(f)
		if rerr != nil {
			return Credentials{}, apierror.Wrap(rerr, apierror.CodeInternalError,
				"the container credential authorization token file could not be read")
		}
		req.Header.Set("Authorization", strings.TrimSpace(string(b)))
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return Credentials{}, apierror.Wrap(err, apierror.CodeDependencyFailure,
			"the container credential endpoint is unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return Credentials{}, apierror.Wrap(err, apierror.CodeDependencyFailure,
			"the container credential response could not be read")
	}
	if resp.StatusCode != http.StatusOK {
		// Status only. The body of a credential endpoint is a credential.
		return Credentials{}, apierror.Newf(apierror.CodeDependencyFailure,
			"the container credential endpoint returned HTTP %d", resp.StatusCode)
	}
	var out containerCredentialResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Credentials{}, apierror.New(apierror.CodeDependencyFailure,
			"the container credential response could not be decoded")
	}
	creds := Credentials{
		AccessKeyID:     out.AccessKeyID,
		SecretAccessKey: out.SecretAccessKey,
		SessionToken:    out.Token,
	}
	if t, err := time.Parse(time.RFC3339, out.Expiration); err == nil {
		creds.Expires = t.UTC()
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return Credentials{}, apierror.New(apierror.CodeDependencyFailure,
			"the container credential endpoint returned an incomplete credential")
	}
	return creds, nil
}

// assumeRoleResponse is the subset of the STS XML response this client needs. STS speaks XML in
// the query protocol; the alternative — the JSON protocol — needs a signed request, and the
// point of AssumeRoleWithWebIdentity is that it is the one call that needs no credentials.
type assumeRoleResponse struct {
	XMLName xml.Name `xml:"AssumeRoleWithWebIdentityResponse"`
	Result  struct {
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"Credentials"`
	} `xml:"AssumeRoleWithWebIdentityResult"`
}

func (assumeRoleResponse) String() string   { return "assumeRoleResponse{[REDACTED]}" }
func (assumeRoleResponse) GoString() string { return "assumeRoleResponse{[REDACTED]}" }

// fromWebIdentity performs the IRSA exchange: a projected Kubernetes service-account token for a
// one-hour STS role session.
//
// The token file is re-read on every refresh rather than cached, and that is the whole reason
// IRSA is safe: kubelet rotates the projected token in place, and a client holding the first
// copy would start failing an hour into the pod's life with an "invalid identity token" that
// looks like an IAM misconfiguration.
func (c *chainCredentials) fromWebIdentity(ctx context.Context, tokenFile, roleARN, sessionName string) (Credentials, error) {
	token, err := c.read(tokenFile)
	if err != nil {
		return Credentials{}, apierror.Wrapf(err, apierror.CodeInternalError,
			"the IRSA web-identity token file %s could not be read", tokenFile)
	}
	if sessionName == "" {
		// A session name appears in CloudTrail and in the IRSA session tag that the siloed-tier
		// tenant IAM condition matches on (docs/security.md §5.1), so an empty one would make
		// every pod's reads indistinguishable in an audit.
		sessionName = "payments-platform"
	}
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {roleARN},
		"RoleSessionName":  {sessionName},
		"WebIdentityToken": {strings.TrimSpace(string(token))},
	}
	endpoint := c.cfg.STSEndpoint
	if endpoint == "" {
		endpoint = "https://sts.amazonaws.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/", strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, apierror.Wrap(err, apierror.CodeInternalError,
			"the STS endpoint is not a usable URL")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/xml")

	resp, err := c.client.Do(req)
	if err != nil {
		return Credentials{}, apierror.Wrap(err, apierror.CodeDependencyFailure, "STS is unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return Credentials{}, apierror.Wrap(err, apierror.CodeDependencyFailure,
			"the STS response could not be read")
	}
	if resp.StatusCode != http.StatusOK {
		// The status and nothing else. An STS error body echoes the web identity token, which is
		// a bearer credential for this pod's identity.
		return Credentials{}, apierror.Newf(apierror.CodeDependencyFailure,
			"STS refused the web-identity exchange for %s: HTTP %d", roleARN, resp.StatusCode)
	}
	var out assumeRoleResponse
	if err := xml.Unmarshal(raw, &out); err != nil {
		return Credentials{}, apierror.New(apierror.CodeDependencyFailure,
			"the STS response could not be decoded")
	}
	creds := Credentials{
		AccessKeyID:     out.Result.Credentials.AccessKeyID,
		SecretAccessKey: out.Result.Credentials.SecretAccessKey,
		SessionToken:    out.Result.Credentials.SessionToken,
	}
	if t, err := time.Parse(time.RFC3339, out.Result.Credentials.Expiration); err == nil {
		creds.Expires = t.UTC()
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.SessionToken == "" {
		return Credentials{}, apierror.New(apierror.CodeDependencyFailure,
			"STS returned an incomplete web-identity session")
	}
	return creds, nil
}

// StaticCredentials is a fixed credential set, for tests and for the local stack.
//
// It is exported so a test can wire a signer without an environment, and it deliberately has no
// production use: a production deployment resolving credentials from a literal would be one that
// had put a secret in a config map, which docs/security.md §5.2 forbids and an admission policy
// rejects.
func StaticCredentials(c Credentials) CredentialProvider { return staticCredentials{c: c} }

type staticCredentials struct{ c Credentials }

func (s staticCredentials) Retrieve(context.Context) (Credentials, error) { return s.c, nil }

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-32.
//
// The AWS credential chain — container endpoint, IRSA web identity, static environment — with
// refresh ahead of expiry and a single-flight guard against a cold-start refresh storm
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
