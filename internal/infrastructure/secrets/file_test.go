package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

const (
	fileTenant   = "ten_01JB8Z00000000000000000000"
	fileMerchant = "mrc_01JB8Z11111111111111111111"
	fileRef      = "secret://sandbox/" + fileTenant + "/" + fileMerchant + "/stripe"
)

func writeDoc(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	return path
}

// tenantCtx builds a context carrying a verified tenant, which is what the provider's tenant
// scoping reads. It goes through the real tenantctx constructor rather than stuffing a value in,
// so a change to that package's validation is caught here too.
func tenantCtx(t *testing.T, tenant string) context.Context {
	t.Helper()
	ctx, err := tenantctx.WithTenant(context.Background(), tenantctx.TenantContext{
		TenantID:    shared.TenantID(tenant),
		Tier:        shared.TierPooled,
		Environment: shared.EnvironmentSandbox,
		Source:      tenantctx.SourceToken,
		Principal:   tenantctx.Principal{Type: tenantctx.PrincipalMachine, ID: "svc:test"},
	})
	if err != nil {
		t.Fatalf("building the tenant context: %v", err)
	}
	return ctx
}

func TestFileProviderResolvesYAMLAndJSON(t *testing.T) {
	t.Parallel()
	docs := map[string]string{
		"secrets.yaml": "" +
			`"` + fileRef + `":` + "\n" +
			"  api_key: local-key-value\n" +
			"  webhook_hmac: local-hmac-value\n",
		"secrets.json": `{"` + fileRef + `":{"api_key":"local-key-value","webhook_hmac":"local-hmac-value"}}`,
	}
	for name, body := range docs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := NewFileProvider(FileConfig{Path: writeDoc(t, name, body), Environment: shared.EnvironmentSandbox})
			if err != nil {
				t.Fatalf("constructing: %v", err)
			}
			m, err := p.Get(tenantCtx(t, fileTenant), fileRef)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if v, ok := m.Value("api_key"); !ok || v != "local-key-value" {
				t.Errorf("api_key = %q, %v", v, ok)
			}
			if got := m.Fields(); len(got) != 2 {
				t.Errorf("Fields() = %v", got)
			}
			if m.Version() != "v1" {
				t.Errorf("Version() = %q, want v1", m.Version())
			}
		})
	}
}

// TestFileProviderResolvesEnvironmentIndirection covers the half that makes the document
// committable: a value of `env:NAME` is read from the process environment at resolution time, so
// a CI job can supply a real sandbox key without it living in a file.
func TestFileProviderResolvesEnvironmentIndirection(t *testing.T) {
	t.Parallel()
	doc := `{"` + fileRef + `":{"api_key":"env:PP_TEST_SANDBOX_KEY"}}`
	p, err := NewFileProvider(FileConfig{
		Path:        writeDoc(t, "secrets.json", doc),
		Environment: shared.EnvironmentSandbox,
		Getenv: func(k string) string {
			if k == "PP_TEST_SANDBOX_KEY" {
				return "from-the-environment"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("constructing: %v", err)
	}
	m, err := p.Get(tenantCtx(t, fileTenant), fileRef)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v, _ := m.Value("api_key"); v != "from-the-environment" {
		t.Errorf("api_key = %q", v)
	}
}

// TestFileProviderHonoursVersionPins is what makes the rotation testable locally: a document may
// declare several versions of one reference, and a pinned read resolves exactly the pinned one
// while an unpinned read resolves the current.
func TestFileProviderHonoursVersionPins(t *testing.T) {
	t.Parallel()
	doc := "" +
		`"` + fileRef + `#v1":` + "\n  api_key: first\n" +
		`"` + fileRef + `#v2":` + "\n  api_key: second\n"
	p, err := NewFileProvider(FileConfig{Path: writeDoc(t, "secrets.yaml", doc), Environment: shared.EnvironmentSandbox})
	if err != nil {
		t.Fatalf("constructing: %v", err)
	}
	ctx := tenantCtx(t, fileTenant)

	cases := map[string]string{
		fileRef:                       "second",
		fileRef + "#v1":               "first",
		fileRef + "#v2":               "second",
		fileRef + "#" + StagePrevious: "first",
		fileRef + "#" + StageCurrent:  "second",
	}
	for ref, want := range cases {
		m, err := p.Get(ctx, ref)
		if err != nil {
			t.Fatalf("Get(%s): %v", ref, err)
		}
		if v, _ := m.Value("api_key"); v != want {
			t.Errorf("Get(%s) api_key = %q, want %q", ref, v, want)
		}
	}
	if _, err := p.Get(ctx, fileRef+"#v9"); err == nil {
		t.Error("a pin at a version that does not exist resolved")
	}
}

// TestFileProviderRotationKeepsThePreviousVersionReadable is the overlap property itself: after
// a rotation the old version must still resolve, because in-flight requests signed with it are
// still in the gateway's retry queue. A provider that dropped it would make every local rotation
// test pass while production produced a burst of 401s.
func TestFileProviderRotationKeepsThePreviousVersionReadable(t *testing.T) {
	t.Parallel()
	doc := `{"` + fileRef + `":{"api_key":"v1-key"}}`
	p, err := NewFileProvider(FileConfig{Path: writeDoc(t, "secrets.json", doc), Environment: shared.EnvironmentSandbox})
	if err != nil {
		t.Fatalf("constructing: %v", err)
	}
	ctx := tenantCtx(t, fileTenant)

	versioned, err := p.Rotate(ctx, fileRef, map[string]string{"api_key": "v2-key"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if !strings.HasSuffix(versioned, "#v2") {
		t.Errorf("Rotate returned %q, want a #v2 pin", versioned)
	}

	current, err := p.Get(ctx, fileRef)
	if err != nil {
		t.Fatalf("Get current: %v", err)
	}
	if v, _ := current.Value("api_key"); v != "v2-key" {
		t.Errorf("the current version is %q, want the rotated one", v)
	}
	previous, err := p.Get(ctx, fileRef+"#"+StagePrevious)
	if err != nil {
		t.Fatalf("the previous version is not readable during the overlap: %v", err)
	}
	if v, _ := previous.Value("api_key"); v != "v1-key" {
		t.Errorf("AWSPREVIOUS resolves to %q, want the pre-rotation credential", v)
	}
}

// TestFileProviderRefusesProduction is the control that keeps a file full of credentials out of
// production. The error must say *why*, because the person reading it is deciding whether to set
// the override.
func TestFileProviderRefusesProduction(t *testing.T) {
	t.Parallel()
	_, err := NewFileProvider(FileConfig{Environment: shared.EnvironmentProduction})
	if err == nil {
		t.Fatal("the file provider started in production")
	}
	msg := err.Error()
	for _, want := range []string{"KMS", "CloudTrail", "IAM", "PP_SECRETS_BACKEND"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so it does not say what the operator is giving up:\n%s", want, msg)
		}
	}

	p, err := NewFileProvider(FileConfig{Environment: shared.EnvironmentProduction, AllowProduction: true})
	if err != nil {
		t.Fatalf("the explicit override was refused: %v", err)
	}
	if p == nil {
		t.Fatal("the override returned no provider")
	}
}

// TestFileProviderRefusesACrossEnvironmentDocument: loading a sandbox document into a production
// process is the mistake the environment segment exists to catch, and catching it at startup
// makes it a visible failure rather than a per-payment one.
func TestFileProviderRefusesACrossEnvironmentDocument(t *testing.T) {
	t.Parallel()
	doc := `{"secret://production/` + fileTenant + `/` + fileMerchant + `/stripe":{"api_key":"x"}}`
	_, err := NewFileProvider(FileConfig{
		Path: writeDoc(t, "secrets.json", doc), Environment: shared.EnvironmentSandbox,
	})
	if err == nil {
		t.Fatal("a production document loaded into a sandbox process")
	}
}

// TestFileProviderEnforcesTenantScoping: the development provider applies the same tenant
// boundary as the production one. Without this, a scoping bug would pass every local test.
func TestFileProviderEnforcesTenantScoping(t *testing.T) {
	t.Parallel()
	doc := `{"` + fileRef + `":{"api_key":"x"}}`
	p, err := NewFileProvider(FileConfig{Path: writeDoc(t, "secrets.json", doc), Environment: shared.EnvironmentSandbox})
	if err != nil {
		t.Fatalf("constructing: %v", err)
	}
	_, err = p.Get(tenantCtx(t, "ten_01JB8Z22222222222222222222"), fileRef)
	if err == nil {
		t.Fatal("another tenant resolved this reference")
	}
	if apierror.CodeOf(err) != apierror.CodeTenantMismatch {
		t.Errorf("code = %s, want TENANT_MISMATCH", apierror.CodeOf(err))
	}
}

func TestFileProviderPutAndDelete(t *testing.T) {
	t.Parallel()
	p, err := NewFileProvider(FileConfig{Environment: shared.EnvironmentSandbox})
	if err != nil {
		t.Fatalf("constructing: %v", err)
	}
	ctx := tenantCtx(t, fileTenant)

	if _, err := p.Put(ctx, fileRef, nil); err == nil {
		t.Error("an empty credential was stored")
	}
	ref, err := p.Put(ctx, fileRef, map[string]string{"api_key": "written"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasSuffix(ref, "#v1") {
		t.Errorf("Put returned %q, want a #v1 pin", ref)
	}
	if got := p.References(); len(got) != 1 || got[0] != fileRef {
		t.Errorf("References() = %v", got)
	}
	if err := p.Delete(ctx, fileRef); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, fileRef); err == nil {
		t.Error("a deleted reference still resolves")
	}
}

func TestParseBackendRefusesATypo(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"awss", "secretsmanager", "vault", "FILE "} {
		if _, err := ParseBackend(in); err == nil && in != "FILE " {
			t.Errorf("ParseBackend(%q) was accepted", in)
		}
	}
	for _, in := range []string{"", "aws", "AWS", " file "} {
		if _, err := ParseBackend(in); err != nil {
			t.Errorf("ParseBackend(%q): %v", in, err)
		}
	}
}
