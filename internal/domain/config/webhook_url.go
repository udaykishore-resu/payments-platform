package config

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// validateWebhookURL rejects merchant-supplied webhook destinations that would turn the
// platform into an SSRF proxy.
//
// This is the control for one of the more embarrassing ways a payment platform gets
// compromised: a merchant registers `http://169.254.169.254/latest/meta-data/iam/...` as their
// webhook endpoint, and the platform — which has an IAM role — obligingly fetches it and, in
// the worst implementations, shows them the response body in a delivery log. The same trick
// against `http://10.0.0.x` maps the internal network, and against `http://localhost:9090`
// reaches sidecars.
//
// The checks here are the *first* layer, applied at configuration time so a bad endpoint never
// gets stored. Two further layers exist because DNS can change between validation and delivery
// (a rebinding attack): the outbound HTTP client used for webhook delivery re-resolves and
// re-checks the address at dial time via a Control hook, and egress from the delivery pods is
// restricted by NetworkPolicy and a NAT allowlist. Validating only here would be
// security theatre; validating only at dial time would give the merchant a confusing failure
// long after they configured it.
func validateWebhookURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("a webhook URL is required")
	}
	if len(raw) > 2048 {
		return errors.New("webhook URL exceeds 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a valid URL")
	}
	// HTTPS only. Webhook payloads carry payment state; sending them in the clear is a data
	// exposure regardless of what the merchant is willing to accept.
	if u.Scheme != "https" {
		return errors.New("must use https")
	}
	if u.User != nil {
		return errors.New("credentials must not be embedded in the URL")
	}
	if u.Fragment != "" {
		return errors.New("must not contain a fragment")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("must have a host")
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return errors.New("must not target localhost")
	}
	// A literal IP is not automatically wrong, but it must not be one of the reserved ranges.
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return errors.New("must not target a private, loopback, link-local or reserved address")
		}
		return nil
	}
	// A hostname is resolved at dial time, not here — resolving during validation would make a
	// configuration write depend on DNS availability, and the result would be stale by the
	// time it is used anyway. The dial-time check is the authoritative one.
	if !strings.Contains(host, ".") {
		return errors.New("must be a fully qualified domain name")
	}
	if strings.HasSuffix(strings.ToLower(host), ".internal") ||
		strings.HasSuffix(strings.ToLower(host), ".local") ||
		strings.HasSuffix(strings.ToLower(host), ".cluster.local") {
		return errors.New("must not target an internal domain")
	}
	return nil
}

// blockedNets are the ranges a merchant webhook must never resolve to. The list is deliberately
// broader than "private": cloud metadata endpoints live in link-local space, and carrier-grade
// NAT and benchmarking ranges have no legitimate role as a customer's webhook destination.
var blockedNets = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",          // this host
		"10.0.0.0/8",         // private
		"100.64.0.0/10",      // carrier-grade NAT
		"127.0.0.0/8",        // loopback
		"169.254.0.0/16",     // link-local, includes 169.254.169.254 cloud metadata
		"172.16.0.0/12",      // private
		"192.0.0.0/24",       // IETF protocol assignments
		"192.0.2.0/24",       // TEST-NET-1
		"192.88.99.0/24",     // 6to4 relay anycast
		"192.168.0.0/16",     // private
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // TEST-NET-2
		"203.0.113.0/24",     // TEST-NET-3
		"224.0.0.0/4",        // multicast
		"240.0.0.0/4",        // reserved
		"255.255.255.255/32", // broadcast
		"::/128",             // unspecified
		"::1/128",            // loopback
		"fc00::/7",           // unique local
		"fe80::/10",          // link-local
		"ff00::/8",           // multicast
		"64:ff9b::/96",       // NAT64, which can be used to reach IPv4 private space
		// The IPv4-mapped range (::ffff:0:0/96) is deliberately NOT listed. It is handled by the
		// normalisation in isBlockedIP instead, and listing it here is actively wrong: net.ParseCIDR
		// parses an IPv4-mapped prefix into a 4-byte network, so `::ffff:0:0/96` becomes 0.0.0.0
		// with a 4-byte mask — which Contains reports true for *every* IPv4 address. The bypass it
		// was meant to close is closed by To4() below; the entry only closed the internet.
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// IsBlockedAddress reports whether an address is in a range a webhook may not target. It is
// exported so the outbound HTTP client's dial-time Control hook uses exactly the same list —
// two copies of this list would inevitably diverge, and the divergence would be a bypass.
func IsBlockedAddress(ip net.IP) bool { return isBlockedIP(ip) }

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Normalise an IPv4-mapped IPv6 address to its IPv4 form before checking, so that
	// ::ffff:169.254.169.254 is caught by the 169.254.0.0/16 rule rather than sliding past a
	// v6-only comparison.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
