package authn

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
)

// SPIFFEID is a parsed SPIFFE identity: `spiffe://<trust-domain>/ns/<namespace>/sa/<sa>`.
//
// The workload identity in this platform is a URI SAN on the client certificate, not the
// certificate's Common Name and not a header. The distinction is the whole point of the mesh's
// identity model: a CN is a string an operator typed, while a URI SAN issued by the cluster CA
// is a statement the CA is willing to stand behind, and it is bound to the private key that just
// completed the handshake.
type SPIFFEID struct {
	// Raw is the full URI, which is also the Principal's ID: it is the value the mesh
	// AuthorizationPolicy is keyed on, so it is the value an audit record should carry.
	Raw string
	// TrustDomain is the authority component.
	TrustDomain string
	// Namespace and ServiceAccount are the two path segments this platform's naming convention
	// uses. They are parsed out because the SPIFFE-ID-to-service mapping is expressed in those
	// terms and because a namespace is useful on its own for a plane-level check.
	Namespace      string
	ServiceAccount string
}

// String returns the full SPIFFE URI.
func (s SPIFFEID) String() string { return s.Raw }

// ParseSPIFFEID parses and validates a SPIFFE URI.
//
// It is strict about shape. A SPIFFE ID with query parameters, a fragment, userinfo or a port is
// refused rather than normalized away: those components have no meaning in a SPIFFE ID, so a
// certificate carrying one was either issued by something that does not understand the scheme or
// crafted to make two different parsers disagree about the identity — and a parser disagreement
// on an identity is exactly how an authorization policy gets bypassed.
func ParseSPIFFEID(raw string) (SPIFFEID, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return SPIFFEID{}, fmt.Errorf("authn: unparseable SPIFFE ID: %w", err)
	}
	switch {
	case u.Scheme != "spiffe":
		return SPIFFEID{}, fmt.Errorf("authn: SPIFFE ID must use the spiffe scheme, got %q", u.Scheme)
	case u.Host == "":
		return SPIFFEID{}, fmt.Errorf("authn: SPIFFE ID has no trust domain")
	case u.User != nil:
		return SPIFFEID{}, fmt.Errorf("authn: SPIFFE ID must not carry userinfo")
	case u.RawQuery != "" || u.Fragment != "":
		return SPIFFEID{}, fmt.Errorf("authn: SPIFFE ID must not carry a query or fragment")
	case u.Port() != "":
		return SPIFFEID{}, fmt.Errorf("authn: SPIFFE ID must not carry a port")
	case u.Path == "" || u.Path == "/":
		return SPIFFEID{}, fmt.Errorf("authn: SPIFFE ID has no workload path")
	}
	id := SPIFFEID{Raw: raw, TrustDomain: u.Host}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(segments); i += 2 {
		switch segments[i] {
		case "ns":
			id.Namespace = segments[i+1]
		case "sa":
			id.ServiceAccount = segments[i+1]
		}
	}
	return id, nil
}

// ServiceIdentity is what a SPIFFE ID maps to: one of our own deployables.
//
// The mapping is an explicit table (security.md §1.2), not a convention derived from the
// service-account name. A convention would mean that creating a service account in the right
// namespace is enough to obtain a platform identity, which turns a Kubernetes RBAC mistake into
// an authentication bypass. A table means a new workload identity is a reviewed change.
type ServiceIdentity struct {
	// Service is the deployable's name, e.g. "payment-orchestrator".
	Service string
	// Roles are the RBAC roles this workload holds; in practice `svc:internal`.
	Roles []string
	// Scopes are the permissions the workload may exercise. They are narrow on purpose: the
	// matrix grants `svc:internal` a specific, small set, and a workload that needs more is a
	// design conversation.
	Scopes []string
}

// PeerAuthenticator derives a workload Principal from a verified TLS client certificate.
type PeerAuthenticator struct {
	trustDomain string
	services    map[string]ServiceIdentity
}

// NewPeerAuthenticator builds an authenticator for one trust domain.
//
// The trust domain is a constructor argument rather than something read from the certificate
// because it is the boundary being enforced. A certificate from another trust domain may be
// perfectly valid and signed by a CA some chain trusts; it is simply not one of ours, and
// accepting it because it parsed is how a shared or federated CA becomes a cross-organization
// authentication bypass.
func NewPeerAuthenticator(trustDomain string, services map[string]ServiceIdentity) *PeerAuthenticator {
	copied := make(map[string]ServiceIdentity, len(services))
	for k, v := range services {
		v.Roles = append([]string(nil), v.Roles...)
		v.Scopes = append([]string(nil), v.Scopes...)
		copied[k] = v
	}
	return &PeerAuthenticator{trustDomain: trustDomain, services: copied}
}

// FromConnectionState derives the peer identity from a completed TLS handshake.
//
// It reads VerifiedChains rather than PeerCertificates. The difference matters: PeerCertificates
// is whatever the peer sent, verified or not, and on a server configured with
// `tls.RequestClientCert` it is populated even when verification failed. VerifiedChains is
// non-empty only when the certificate chained to a configured root, which is the property the
// whole identity model rests on.
func (a *PeerAuthenticator) FromConnectionState(state *tls.ConnectionState) (*Principal, error) {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return nil, reject(ReasonNoPeerCertificate)
	}
	return a.FromCertificate(state.VerifiedChains[0][0])
}

// FromCertificate derives the peer identity from an already-verified leaf certificate.
//
// It is exported separately from FromConnectionState so that a caller who has performed
// verification another way — an Envoy-forwarded certificate that arrived on a connection from
// the sidecar itself — has a supported path that does not require fabricating a
// tls.ConnectionState.
func (a *PeerAuthenticator) FromCertificate(cert *x509.Certificate) (*Principal, error) {
	if cert == nil {
		return nil, reject(ReasonNoPeerCertificate)
	}
	// Exactly one URI SAN. A certificate carrying several identities is ambiguous, and
	// resolving the ambiguity by taking the first is how a workload obtains a second identity
	// by asking for one.
	if len(cert.URIs) != 1 {
		return nil, reject(ReasonBadSPIFFEID)
	}
	id, err := ParseSPIFFEID(cert.URIs[0].String())
	if err != nil {
		return nil, reject(ReasonBadSPIFFEID)
	}
	if id.TrustDomain != a.trustDomain {
		return nil, reject(ReasonWrongTrustDomain)
	}
	svc, ok := a.services[id.Raw]
	if !ok {
		return nil, reject(ReasonUnknownWorkload)
	}
	return &Principal{
		Method: MethodMTLS,
		Type:   tenantctx.PrincipalWorkload,
		ID:     id.Raw,
		Name:   svc.Service,
		// TenantID is deliberately left zero. A workload identity is not tenant-scoped
		// (security.md §3.5): the tenant travels in the propagated request context or the event
		// envelope, and Principal.TenantContext refuses to invent one. That refusal is what
		// stops `svc:internal` from being an ambient cross-tenant reader.
		Scopes:      append([]string(nil), svc.Scopes...),
		Roles:       append([]string(nil), svc.Roles...),
		TrustDomain: id.TrustDomain,
		Service:     svc.Service,
		ExpiresAt:   cert.NotAfter,
	}, nil
}

// ThumbprintOf returns the RFC 8705 `x5t#S256` value of a certificate, for binding a
// sender-constrained token to the connection it arrived on.
func ThumbprintOf(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	return Thumbprint(cert.Raw)
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-28.
//
// Service-to-service peer authentication over mTLS with SPIFFE identities
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
