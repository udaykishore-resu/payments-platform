package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// fakeSM is a Secrets Manager stand-in. It speaks the JSON 1.1 target protocol, so the client
// under test exercises its real signing, encoding and error-classification paths — the only thing
// it does not exercise is AWS's own signature verification, which the SigV4 vectors cover.
type fakeSM struct {
	mu       sync.Mutex
	versions map[string]map[string]string // version label -> material
	stages   map[string]string            // stage label -> version label
	// throttleFirst makes the next N calls answer with a throttling error, so the retry path is
	// driven by the same code AWS would drive it with.
	throttleFirst atomic.Int32
	calls         atomic.Int32
	targets       []string
}

func newFakeSM() *fakeSM {
	return &fakeSM{
		versions: map[string]map[string]string{"v1": {"api_key": awsPlaintext}},
		stages:   map[string]string{StageCurrent: "v1"},
	}
}

func (f *fakeSM) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		target := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "secretsmanager.")
		f.mu.Lock()
		f.targets = append(f.targets, target)
		f.mu.Unlock()

		if n := f.throttleFirst.Load(); n > 0 {
			f.throttleFirst.Add(-1)
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"__type":"com.amazonaws.secretsmanager#ThrottlingException","message":"Rate exceeded"}`))
			return
		}

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()

		switch target {
		case "GetSecretValue":
			stage, _ := body["VersionStage"].(string)
			label, ok := f.stages[stage]
			if !ok {
				label = stage
			}
			mat, ok := f.versions[label]
			if !ok {
				writeAWSError(w, http.StatusBadRequest, "ResourceNotFoundException")
				return
			}
			raw, _ := json.Marshal(mat)
			writeJSON(w, map[string]any{
				"SecretString": string(raw), "VersionId": "id-" + label,
				"VersionStages": []string{label, stage},
			})
		case "DescribeSecret":
			ids := map[string][]string{}
			for label := range f.versions {
				ids["id-"+label] = []string{label}
			}
			for stage, label := range f.stages {
				ids["id-"+label] = append(ids["id-"+label], stage)
			}
			writeJSON(w, map[string]any{"Name": body["SecretId"], "VersionIdsToStages": ids})
		case "PutSecretValue":
			var mat map[string]string
			_ = json.Unmarshal([]byte(body["SecretString"].(string)), &mat)
			label := ""
			for _, s := range body["VersionStages"].([]any) {
				if _, ok := versionNumber(s.(string)); ok {
					label = s.(string)
				}
			}
			f.versions[label] = mat
			for _, s := range body["VersionStages"].([]any) {
				if _, ok := versionNumber(s.(string)); !ok {
					f.stages[s.(string)] = label
				}
			}
			writeJSON(w, map[string]any{"VersionId": "id-" + label, "VersionStages": body["VersionStages"]})
		case "UpdateSecretVersionStage":
			stage := body["VersionStage"].(string)
			to := strings.TrimPrefix(body["MoveToVersionId"].(string), "id-")
			if prev, ok := f.stages[stage]; ok && prev != to {
				f.stages[StagePrevious] = prev
			}
			f.stages[stage] = to
			writeJSON(w, map[string]any{})
		case "DeleteSecret":
			writeJSON(w, map[string]any{"DeletionDate": float64(1)})
		default:
			writeAWSError(w, http.StatusBadRequest, "UnknownOperationException")
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(v)
}

func writeAWSError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"__type":"com.amazonaws.secretsmanager#%s","message":"no"}`, code)
}

// awsPlaintext is the credential every test asserts never renders.
const awsPlaintext = "aws-material-must-never-render"

func newTestClient(t *testing.T, srv *httptest.Server, ttl time.Duration) *AWSSecretsManager {
	t.Helper()
	c, err := NewAWSSecretsManager(AWSConfig{
		Region:      "eu-west-1",
		Endpoint:    srv.URL,
		Environment: shared.EnvironmentSandbox,
		HTTPClient:  srv.Client(),
		Credentials: StaticCredentials(Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "secret", SessionToken: "token"}),
		CacheTTL:    ttl,
		RetryBudget: resilience.NewBudget(10, 1000, time.Second, resilience.SystemClock()),
	})
	if err != nil {
		t.Fatalf("constructing the client: %v", err)
	}
	return c
}

func TestAWSGetResolvesAndSigns(t *testing.T) {
	t.Parallel()
	fake := newFakeSM()
	var sawAuth, sawToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth, sawToken = r.Header.Get("Authorization"), r.Header.Get(headerAmzToken)
		fake.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, DefaultCacheTTL)
	m, err := c.Get(tenantCtx(t, fileTenant), fileRef)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v, ok := m.Value("api_key"); !ok || v != awsPlaintext {
		t.Errorf("api_key = %q, %v", v, ok)
	}
	if !strings.HasPrefix(sawAuth, sigV4Algorithm+" Credential=AKIDTEST/") {
		t.Errorf("the request was not signed: %q", sawAuth)
	}
	if sawToken != "token" {
		t.Errorf("the session token was not sent: %q", sawToken)
	}
}

// TestAWSCacheCollapsesRepeatedResolution is the reason the cache exists: the payment path
// resolves a credential per dispatch, and Secrets Manager is rate-limited and billed per call.
func TestAWSCacheCollapsesRepeatedResolution(t *testing.T) {
	t.Parallel()
	fake := newFakeSM()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := newTestClient(t, srv, DefaultCacheTTL)
	ctx := tenantCtx(t, fileTenant)

	for i := 0; i < 5; i++ {
		if _, err := c.Get(ctx, fileRef); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("five resolutions made %d API calls, want 1", got)
	}

	// The priority invalidation must take effect immediately, not at the TTL: it is what makes a
	// rotation propagate inside the overlap window.
	c.Invalidate(fileRef)
	if _, err := c.Get(ctx, fileRef); err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if got := fake.calls.Load(); got != 2 {
		t.Errorf("the invalidation did not force a re-read: %d calls", got)
	}
}

func TestAWSRetriesThrottlingAndGivesUpBounded(t *testing.T) {
	t.Parallel()
	fake := newFakeSM()
	fake.throttleFirst.Store(2)
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := newTestClient(t, srv, -1)

	if _, err := c.Get(tenantCtx(t, fileTenant), fileRef); err != nil {
		t.Fatalf("a throttle that clears was not retried through: %v", err)
	}
	if got := fake.calls.Load(); got != 3 {
		t.Errorf("made %d calls, want 3 (two throttles then a success)", got)
	}

	// A throttle that never clears must stop at the attempt bound rather than hammering.
	fake.calls.Store(0)
	fake.throttleFirst.Store(100)
	if _, err := c.Get(tenantCtx(t, fileTenant), fileRef); err == nil {
		t.Fatal("a permanent throttle eventually succeeded")
	}
	if got := fake.calls.Load(); got > 4 {
		t.Errorf("a permanent throttle made %d calls; the retry is not bounded", got)
	}
}

// TestAWSDoesNotRetryANonThrottleError: retrying a 400 amplifies a malformed request into four
// and delays the honest error the caller has to see.
func TestAWSDoesNotRetryANonThrottleError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeAWSError(w, http.StatusBadRequest, "InvalidParameterException")
	}))
	defer srv.Close()
	c := newTestClient(t, srv, -1)

	if _, err := c.Get(tenantCtx(t, fileTenant), fileRef); err == nil {
		t.Fatal("a 400 was reported as success")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a 400 was retried %d times", got)
	}
}

// TestAWSRotateStagesThenPromotes walks the store's half of the four-phase overlap and asserts
// the property the whole design rests on: after promotion, the previous version is still
// readable, because in-flight requests signed with it have not finished.
func TestAWSRotateStagesThenPromotes(t *testing.T) {
	t.Parallel()
	fake := newFakeSM()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := newTestClient(t, srv, DefaultCacheTTL)
	ctx := tenantCtx(t, fileTenant)

	versioned, err := c.Rotate(ctx, fileRef, map[string]string{"api_key": "rotated-value"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if !strings.HasSuffix(versioned, "#v2") {
		t.Errorf("Rotate returned %q, want a #v2 pin", versioned)
	}

	current, err := c.Get(ctx, fileRef)
	if err != nil {
		t.Fatalf("Get after rotation: %v", err)
	}
	if v, _ := current.Value("api_key"); v != "rotated-value" {
		t.Errorf("the current version is %q, want the rotated one", v)
	}
	previous, err := c.Get(ctx, fileRef+"#"+StagePrevious)
	if err != nil {
		t.Fatalf("the previous version is not readable during the overlap: %v", err)
	}
	if v, _ := previous.Value("api_key"); v != awsPlaintext {
		t.Errorf("AWSPREVIOUS resolves to %q, want the pre-rotation credential", v)
	}
}

// TestAWSRotateRefusesAnOverlapShorterThanTheCache pins control-plane.md §5.3's inequality. An
// overlap the fleet cannot honour would revoke a credential pods are still signing with.
func TestAWSRotateRefusesAnOverlapShorterThanTheCache(t *testing.T) {
	t.Parallel()
	fake := newFakeSM()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := newTestClient(t, srv, DefaultCacheTTL)

	_, err := c.Rotate(tenantCtx(t, fileTenant), fileRef, map[string]string{"api_key": "x"}, 30*time.Second)
	if err == nil {
		t.Fatal("an overlap shorter than ten cache TTLs was accepted")
	}
	if !strings.Contains(err.Error(), "cache") {
		t.Errorf("the refusal does not explain the constraint: %v", err)
	}
	if fake.calls.Load() != 0 {
		t.Error("the refusal happened after a write; nothing should have been staged")
	}
}

// TestAWSDeleteAlwaysUsesARecoveryWindow: deletion is cleanup after a revocation that already
// happened, and a mistaken rotation must stay recoverable.
func TestAWSDeleteAlwaysUsesARecoveryWindow(t *testing.T) {
	t.Parallel()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, map[string]any{"DeletionDate": float64(1)})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, -1)

	if err := c.Delete(tenantCtx(t, fileTenant), fileRef); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := body["RecoveryWindowInDays"]; got != float64(DefaultRecoveryWindowDays) {
		t.Errorf("RecoveryWindowInDays = %v, want %d", got, DefaultRecoveryWindowDays)
	}
	if got := body["ForceDeleteWithoutRecovery"]; got != false {
		t.Errorf("ForceDeleteWithoutRecovery = %v, want false", got)
	}
}

func TestAWSEnforcesTenantAndEnvironmentScoping(t *testing.T) {
	t.Parallel()
	fake := newFakeSM()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := newTestClient(t, srv, -1)

	if _, err := c.Get(tenantCtx(t, "ten_01JB8Z22222222222222222222"), fileRef); apierror.CodeOf(err) != apierror.CodeTenantMismatch {
		t.Errorf("a cross-tenant reference resolved with %v", err)
	}
	prodRef := "secret://production/" + fileTenant + "/" + fileMerchant + "/stripe"
	if _, err := c.Get(tenantCtx(t, fileTenant), prodRef); err == nil {
		t.Error("a production reference resolved in a sandbox process")
	}
	if fake.calls.Load() != 0 {
		t.Error("a refused reference still reached the store")
	}
}

// TestCredentialChainOrderAndSingleFlight covers the two properties the chain is built for: the
// documented precedence, and that a cold start with many concurrent callers makes one STS call
// rather than one per caller.
func TestCredentialChainOrderAndSingleFlight(t *testing.T) {
	t.Parallel()
	var stsCalls atomic.Int32
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stsCalls.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>` +
			`<AccessKeyId>AKIDIRSA</AccessKeyId><SecretAccessKey>irsa-secret</SecretAccessKey>` +
			`<SessionToken>irsa-token</SessionToken><Expiration>2999-01-01T00:00:00Z</Expiration>` +
			`</Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`))
	}))
	defer sts.Close()

	env := map[string]string{
		"AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/token",
		"AWS_ROLE_ARN":                "arn:aws:iam::1:role/payment-orchestrator",
		// Static credentials are present and must NOT win: IRSA is more specific.
		"AWS_ACCESS_KEY_ID":     "AKIDSTATIC",
		"AWS_SECRET_ACCESS_KEY": "static-secret",
	}
	p := NewChainCredentials(ChainConfig{
		HTTPClient:  sts.Client(),
		STSEndpoint: sts.URL,
		Env:         func(k string) string { return env[k] },
		ReadFile:    func(string) ([]byte, error) { return []byte("projected-token"), nil },
	})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			creds, err := p.Retrieve(context.Background())
			if err != nil {
				t.Errorf("Retrieve: %v", err)
				return
			}
			if creds.AccessKeyID != "AKIDIRSA" {
				t.Errorf("resolved %q; IRSA must win over static environment credentials", creds.AccessKeyID)
			}
		}()
	}
	wg.Wait()
	if got := stsCalls.Load(); got != 1 {
		t.Errorf("32 concurrent cold resolutions made %d STS calls; the single-flight guard is not working", got)
	}

	// With IRSA absent, the static credentials are the correct answer.
	delete(env, "AWS_WEB_IDENTITY_TOKEN_FILE")
	static := NewChainCredentials(ChainConfig{Env: func(k string) string { return env[k] }})
	creds, err := static.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("static Retrieve: %v", err)
	}
	if creds.AccessKeyID != "AKIDSTATIC" {
		t.Errorf("static resolution = %q", creds.AccessKeyID)
	}
}

// TestCredentialsRedact covers the exception documented on the Credentials type: the secret key
// is a bare string for the signer's sake, so every rendering path has to be closed by hand.
func TestCredentialsRedact(t *testing.T) {
	t.Parallel()
	c := Credentials{AccessKeyID: "AKID", SecretAccessKey: awsPlaintext, SessionToken: awsPlaintext}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(verb, c); strings.Contains(got, awsPlaintext) {
			t.Errorf("%s leaked the secret key: %s", verb, got)
		}
	}
}
