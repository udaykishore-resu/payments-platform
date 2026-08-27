package secrets

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
)

// leakCanary is the material every backend in this file is loaded with. It is deliberately a
// single high-entropy-looking token with no vendor prefix so that a substring search for it is
// unambiguous, and so that the literal itself matches no credential detector.
const leakCanary = "CANARY-0f3a91c47d5e-MATERIAL"

// TestNoProviderPathLeaksMaterialIntoAnError is the control docs/security.md §5.2 states as "no
// secret in an error message", asserted rather than asserted-to.
//
// # Why a *failing* backend is the interesting case
//
// The success path is easy to keep clean, because nobody writes the material into an error when
// there is no error. Every real leak in a client like this comes from the failure path, and from
// one habit in particular: wrapping a decode failure or a non-2xx response with the body, which
// is the ordinary, idiomatic, correct-looking thing to do — and which, for GetSecretValue,
// *is* the credential. The backends below therefore fail in every way that produces an error
// whose natural construction would include the body:
//
//   - a 500 whose body is the material (an origin misconfiguration);
//   - a 400 whose AWS error message field quotes the request, which contained the material;
//   - a 200 whose body is the material but is not valid JSON, so the decoder errors while
//     holding it;
//   - a connection closed mid-body.
//
// Every exported method of both providers is driven against each of them, and the assertion is
// made on `%+v` of the returned error — the verb that expands wrapped causes and details, which
// is what a structured logger and a panic handler use.
func TestNoProviderPathLeaksMaterialIntoAnError(t *testing.T) {
	t.Parallel()

	backends := map[string]http.HandlerFunc{
		"500 whose body is the material": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"SecretString":"{\"api_key\":\"%s\"}"}`, leakCanary)
		},
		"400 whose message quotes the request": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w,
				`{"__type":"InvalidRequestException","message":"the request body was {\"SecretString\":\"%s\"}"}`,
				leakCanary)
		},
		"200 with an undecodable body that is the material": func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"SecretString": %s`, leakCanary)
		},
		"200 with a valid envelope but a broken secret string": func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"SecretString":"{oops %s","VersionId":"id-v1","VersionStages":["v1"]}`, leakCanary)
		},
		"connection closed mid-body": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "4096")
			_, _ = fmt.Fprintf(w, `{"SecretString":"%s"`, leakCanary)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		},
	}

	for name, handler := range backends {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewUnstartedServer(handler)
			srv.Config.ErrorLog = quietLogger()
			srv.Start()
			defer srv.Close()

			c, err := NewAWSSecretsManager(AWSConfig{
				Region:      "eu-west-1",
				Endpoint:    srv.URL,
				Environment: shared.EnvironmentSandbox,
				HTTPClient:  srv.Client(),
				Credentials: StaticCredentials(Credentials{AccessKeyID: "AKID", SecretAccessKey: leakCanary}),
				CacheTTL:    -1,
				RetryBudget: resilience.NewBudget(10, 1000, time.Second, resilience.SystemClock()),
			})
			if err != nil {
				t.Fatalf("constructing: %v", err)
			}
			ctx := tenantCtx(t, fileTenant)
			assertNoLeak(t, "Get", func() error { _, e := c.Get(ctx, fileRef); return e })
			assertNoLeak(t, "Put", func() error {
				_, e := c.Put(ctx, fileRef, map[string]string{"api_key": leakCanary})
				return e
			})
			assertNoLeak(t, "Rotate", func() error {
				_, e := c.Rotate(ctx, fileRef, map[string]string{"api_key": leakCanary}, 24*time.Hour)
				return e
			})
			assertNoLeak(t, "Delete", func() error { return c.Delete(ctx, fileRef) })
			assertNoLeak(t, "UpdateSecretVersionStage", func() error {
				ref, _ := ParseReference(fileRef)
				return c.UpdateSecretVersionStage(ctx, ref, StageCurrent, "v2")
			})
		})
	}
}

// TestFileProviderPathsDoNotLeakMaterial runs the same assertion against the development
// provider. It matters as much as the AWS one: the file provider is what every test and every
// developer's local stack uses, so a leak here shows up in the log the whole team reads.
func TestFileProviderPathsDoNotLeakMaterial(t *testing.T) {
	t.Parallel()

	// A document whose values are the canary, and a set of references that fail for every reason
	// this provider can fail for: absent, wrong tenant, wrong environment, malformed, and a
	// version pin that does not exist.
	doc := `{"` + fileRef + `":{"api_key":"` + leakCanary + `"}}`
	p, err := NewFileProvider(FileConfig{
		Path: writeDoc(t, "secrets.json", doc), Environment: shared.EnvironmentSandbox,
	})
	if err != nil {
		t.Fatalf("constructing: %v", err)
	}
	otherTenant := tenantCtx(t, "ten_01JB8Z22222222222222222222")
	ownTenant := tenantCtx(t, fileTenant)

	refs := []string{
		fileRef,
		fileRef + "#v99",
		"secret://sandbox/" + fileTenant + "/" + fileMerchant + "/absent",
		"secret://production/" + fileTenant + "/" + fileMerchant + "/stripe",
		"secret://sandbox/" + fileTenant + "/../" + fileMerchant + "/stripe",
		"not-a-reference",
	}
	for _, ref := range refs {
		for _, ctx := range []context.Context{ownTenant, otherTenant, context.Background()} {
			assertNoLeak(t, "Get "+ref, func() error { _, e := p.Get(ctx, ref); return e })
			assertNoLeak(t, "Put "+ref, func() error {
				_, e := p.Put(ctx, ref, map[string]string{"api_key": leakCanary})
				return e
			})
			assertNoLeak(t, "Rotate "+ref, func() error {
				_, e := p.Rotate(ctx, ref, map[string]string{"api_key": leakCanary}, time.Hour)
				return e
			})
			assertNoLeak(t, "Delete "+ref, func() error { return p.Delete(ctx, ref) })
		}
	}

	// The canary must also be absent from the reference listing, which the composition roots log
	// at startup.
	for _, r := range p.References() {
		if strings.Contains(r, leakCanary) {
			t.Errorf("References() leaked material: %s", r)
		}
	}
}

// assertNoLeak runs one provider call and inspects its error through every rendering a logger,
// a panic handler or a `fmt.Errorf("%w")` chain would use.
func assertNoLeak(t *testing.T, what string, call func() error) {
	t.Helper()
	err := call()
	if err == nil {
		return
	}
	renderings := map[string]string{
		"Error()": err.Error(),
		"%v":      fmt.Sprintf("%v", err),
		"%+v":     fmt.Sprintf("%+v", err),
		"%#v":     fmt.Sprintf("%#v", err),
		"%s":      fmt.Sprintf("%s", err),
		"%q":      fmt.Sprintf("%q", err),
	}
	for verb, got := range renderings {
		if strings.Contains(got, leakCanary) {
			t.Errorf("%s: the error's %s rendering contains the credential material:\n%s", what, verb, got)
		}
	}
}

// quietLogger silences httptest's "connection reset" noise for the deliberately-aborted handler,
// so a failing assertion is readable.
func quietLogger() *log.Logger { return log.New(&discard{}, "", 0) }

type discard struct{}

func (*discard) Write(p []byte) (int, error) { return len(p), nil }
