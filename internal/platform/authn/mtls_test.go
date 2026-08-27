package authn_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

const orchestratorID = "spiffe://pp.internal/ns/pp-data/sa/payment-orchestrator"

func peerAuth() *authn.PeerAuthenticator {
	return authn.NewPeerAuthenticator("pp.internal", map[string]authn.ServiceIdentity{
		orchestratorID: {
			Service: "payment-orchestrator",
			Roles:   []string{"svc:internal"},
			Scopes:  []string{"payments:write", "credentials:read"},
		},
	})
}

func certWithURI(t *testing.T, uris ...string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "workload"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	for _, u := range uris {
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, parsed)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestParseSPIFFEID(t *testing.T) {
	t.Parallel()
	id, err := authn.ParseSPIFFEID(orchestratorID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case id.TrustDomain != "pp.internal":
		t.Fatalf("trust domain = %q", id.TrustDomain)
	case id.Namespace != "pp-data":
		t.Fatalf("namespace = %q", id.Namespace)
	case id.ServiceAccount != "payment-orchestrator":
		t.Fatalf("service account = %q", id.ServiceAccount)
	case id.String() != orchestratorID:
		t.Fatalf("String() = %q", id.String())
	}

	bad := []struct{ name, raw string }{
		{"wrong scheme", "https://pp.internal/ns/pp-data/sa/x"},
		{"no trust domain", "spiffe:///ns/pp-data/sa/x"},
		{"no path", "spiffe://pp.internal"},
		{"root path only", "spiffe://pp.internal/"},
		{"userinfo", "spiffe://user@pp.internal/ns/a/sa/b"},
		{"port", "spiffe://pp.internal:443/ns/a/sa/b"},
		{"query", "spiffe://pp.internal/ns/a/sa/b?x=1"},
		{"fragment", "spiffe://pp.internal/ns/a/sa/b#f"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := authn.ParseSPIFFEID(tc.raw); err == nil {
				t.Fatalf("%q must be refused", tc.raw)
			}
		})
	}
}

func TestPeerAuthentication(t *testing.T) {
	t.Parallel()
	a := peerAuth()

	t.Run("known workload", func(t *testing.T) {
		t.Parallel()
		p, err := a.FromCertificate(certWithURI(t, orchestratorID))
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case p.Method != authn.MethodMTLS:
			t.Fatalf("method = %q", p.Method)
		case p.Type != tenantctx.PrincipalWorkload:
			t.Fatalf("type = %q", p.Type)
		case p.ID != orchestratorID:
			t.Fatalf("id = %q", p.ID)
		case p.Service != "payment-orchestrator":
			t.Fatalf("service = %q", p.Service)
		case !p.HasRole("svc:internal"):
			t.Fatalf("roles = %v", p.Roles)
		case !p.HasScope("payments:write"):
			t.Fatalf("scopes = %v", p.Scopes)
		}
		// A workload identity is not tenant-scoped, and must refuse to invent a tenant.
		if !p.TenantID.IsZero() {
			t.Fatalf("a SPIFFE identity must not carry a tenant, got %q", p.TenantID)
		}
		if _, err := p.TenantContext("req", "cor"); err == nil {
			t.Fatal("a workload principal must not be able to establish a tenant context on its own")
		} else if apierror.CodeOf(err) != apierror.CodeMissingTenantContext {
			t.Fatalf("want MISSING_TENANT_CONTEXT, got %v", err)
		}
	})

	cases := []struct {
		name string
		cert *x509.Certificate
		want authn.Reason
	}{
		{"no certificate", nil, authn.ReasonNoPeerCertificate},
		{"no URI SAN", certWithURI(t), authn.ReasonBadSPIFFEID},
		{"two identities", certWithURI(t, orchestratorID, "spiffe://pp.internal/ns/pp-data/sa/payment-api"), authn.ReasonBadSPIFFEID},
		{"not a SPIFFE ID", certWithURI(t, "https://pp.internal/ns/pp-data/sa/payment-orchestrator"), authn.ReasonBadSPIFFEID},
		{"another trust domain", certWithURI(t, "spiffe://evil.example/ns/pp-data/sa/payment-orchestrator"), authn.ReasonWrongTrustDomain},
		{"unmapped workload", certWithURI(t, "spiffe://pp.internal/ns/pp-data/sa/not-a-service"), authn.ReasonUnknownWorkload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := a.FromCertificate(tc.cert)
			if got := reasonOf(t, err); got != tc.want {
				t.Fatalf("reason = %s, want %s", got, tc.want)
			}
		})
	}
}

// The identity must come from the *verified* chain. PeerCertificates is whatever the peer sent.
func TestPeerAuthenticationRequiresAVerifiedChain(t *testing.T) {
	// Verifies: NFR-28.
	t.Parallel()
	a := peerAuth()
	cert := certWithURI(t, orchestratorID)

	if _, err := a.FromConnectionState(nil); reasonOf(t, err) != authn.ReasonNoPeerCertificate {
		t.Fatal("a nil connection state must be refused")
	}
	// A certificate presented but not verified: PeerCertificates is populated, VerifiedChains
	// is not. This is exactly the shape of a server configured with RequestClientCert.
	unverified := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if _, err := a.FromConnectionState(unverified); reasonOf(t, err) != authn.ReasonNoPeerCertificate {
		t.Fatal("an unverified peer certificate must not produce an identity")
	}
	verified := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
		VerifiedChains:   [][]*x509.Certificate{{cert}},
	}
	p, err := a.FromConnectionState(verified)
	if err != nil {
		t.Fatalf("a verified chain must produce an identity: %v", err)
	}
	if p.ID != orchestratorID {
		t.Fatalf("id = %q", p.ID)
	}
}

func TestThumbprintOf(t *testing.T) {
	t.Parallel()
	cert := certWithURI(t, orchestratorID)
	if got, want := authn.ThumbprintOf(cert), authn.Thumbprint(cert.Raw); got != want {
		t.Fatalf("ThumbprintOf = %q, want %q", got, want)
	}
	if authn.ThumbprintOf(nil) != "" {
		t.Fatal("a nil certificate has no thumbprint")
	}
	// The thumbprint is base64url, unpadded, of a SHA-256 — 43 characters.
	if len(authn.ThumbprintOf(cert)) != 43 {
		t.Fatalf("thumbprint = %q", authn.ThumbprintOf(cert))
	}
}

func TestPeerAuthenticatorCopiesItsTable(t *testing.T) {
	t.Parallel()
	scopes := []string{"payments:write"}
	table := map[string]authn.ServiceIdentity{
		orchestratorID: {Service: "payment-orchestrator", Scopes: scopes},
	}
	a := authn.NewPeerAuthenticator("pp.internal", table)
	scopes[0] = "secrets:read"

	p, err := a.FromCertificate(certWithURI(t, orchestratorID))
	if err != nil {
		t.Fatal(err)
	}
	if p.HasScope("secrets:read") {
		t.Fatal("the identity table must be copied, not aliased to the caller's slice")
	}
}
