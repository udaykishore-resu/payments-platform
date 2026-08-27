package authn

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// Key is one verification key resolved from a JWKS document.
//
// Algorithm is carried alongside the key material because the token's header alg is
// attacker-controlled and the key's declared alg is not. Comparing the two is what stops an
// ES256 key from being used to verify an RS256 token, which would otherwise be a second route
// to the key-confusion attack the algorithm allowlist blocks head-on.
type Key struct {
	ID        string
	Algorithm string
	Public    crypto.PublicKey
}

// Fetcher retrieves a JWKS document. It is an interface so the cache is testable without a
// network and so the production implementation can be the one thing that knows about the egress
// proxy, the TLS policy and the response bound.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// JWKSConfig configures the key cache. Every field has a default; the values are the ones in
// security.md §3.3.
type JWKSConfig struct {
	// TTL is how long a fetched set is considered fresh.
	TTL time.Duration
	// RefreshInterval is the background refresh cadence. It is deliberately shorter than TTL so
	// a set is normally replaced before it goes stale, and the request path never waits.
	RefreshInterval time.Duration
	// StaleIfError is how long a set keeps being served after refreshes start failing.
	StaleIfError time.Duration
	// MinRefreshInterval is the per-issuer fetch rate limit.
	MinRefreshInterval time.Duration
	// NegativeTTL is how long an unknown `kid` is remembered as unknown.
	NegativeTTL time.Duration
	// MaxKeys bounds the number of keys accepted from one document.
	MaxKeys int
	// MaxBytes bounds the accepted document size.
	MaxBytes int
	// Clock is injected so every timing rule here is testable without sleeping.
	Clock shared.Clock
}

// The JWKS cache defaults, from security.md §3.3.
const (
	DefaultJWKSTTL                = 10 * time.Minute
	DefaultJWKSRefreshInterval    = 5 * time.Minute
	DefaultJWKSStaleIfError       = 24 * time.Hour
	DefaultJWKSMinRefreshInterval = 30 * time.Second
	DefaultJWKSNegativeTTL        = 30 * time.Second
	DefaultJWKSMaxKeys            = 10
	DefaultJWKSMaxBytes           = 64 << 10
)

// JWKS is a per-issuer verification-key cache with background refresh and stale-if-error.
//
// # Why the request path never fetches
//
// A synchronous JWKS fetch on a cache miss is a self-inflicted denial of service. Rotate a key,
// take a burst of traffic, and every in-flight request discovers the miss at the same instant
// and stampedes the identity provider — which then rate-limits us, which turns a routine
// rotation into a total authentication outage. Worse, the miss is attacker-triggerable: a
// stream of tokens carrying random `kid` values would be a free amplifier pointed at the IdP.
//
// So: refresh happens on a background timer, an unknown `kid` is negative-cached, and a fetch is
// rate-limited to one per issuer per MinRefreshInterval no matter how many requests want one.
// The cost is that a key rotated between two background refreshes is briefly unresolvable; the
// two-key publication window (30 days, security.md §5.3) makes that window irrelevant, because
// the previous key is still published and still verifies.
//
// # Why stale-if-error, and why it is not fail-open
//
// If the IdP is unreachable, the choice is between rejecting every request and continuing to
// verify with the last known good key set. Rejecting everything converts a dependency's outage
// into ours — for a component whose availability target is lower than ours. Continuing is safe
// because a key that was valid five minutes ago is still cryptographically valid: an expired
// *token* is still rejected by the claims checks, and a *revoked* one is still rejected by the
// revocation cache. Stale keys extend the window in which a compromised signing key remains
// usable, which is why the window is bounded at 24 hours and why emergency key revocation is a
// deploy-time issuer-list change rather than something we wait for a JWKS refresh to deliver.
type JWKS struct {
	fetch Fetcher
	cfg   JWKSConfig

	mu      sync.RWMutex
	issuers map[string]*issuerKeys

	// life guards the background goroutine's lifecycle. A mutex rather than a pair of
	// sync.Once values because Start-after-Stop and double-Stop both have to be safe, and
	// expressing that with Once produces a double-close on `done` at the first surprise.
	life    sync.Mutex
	running bool
	stopped bool
	stop    chan struct{}
	done    chan struct{}
	// refreshed is signalled after every background refresh cycle so a test can synchronise
	// without sleeping. It is buffered and written non-blockingly, so nothing depends on
	// anyone reading it.
	refreshed chan struct{}
}

type issuerKeys struct {
	url  string
	keys map[string]*Key
	// fetchedAt is when the current set was successfully retrieved; the staleness ladder is
	// measured from it.
	fetchedAt time.Time
	// lastAttempt drives the rate limit, and is updated on failures too — otherwise a failing
	// issuer would be retried as fast as requests arrive, which is the stampede in a different
	// costume.
	lastAttempt time.Time
	lastErr     error
	// unknownKIDs is the negative cache. Values are the instant the entry expires.
	unknownKIDs map[string]time.Time
}

// NewJWKS builds a key cache. Registration of issuers is separate so that the allowlist is an
// explicit, reviewable list rather than something that grows at runtime.
func NewJWKS(fetch Fetcher, cfg JWKSConfig) *JWKS {
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultJWKSTTL
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = DefaultJWKSRefreshInterval
	}
	if cfg.StaleIfError <= 0 {
		cfg.StaleIfError = DefaultJWKSStaleIfError
	}
	if cfg.MinRefreshInterval <= 0 {
		cfg.MinRefreshInterval = DefaultJWKSMinRefreshInterval
	}
	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = DefaultJWKSNegativeTTL
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = DefaultJWKSMaxKeys
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultJWKSMaxBytes
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	return &JWKS{
		fetch:     fetch,
		cfg:       cfg,
		issuers:   map[string]*issuerKeys{},
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		refreshed: make(chan struct{}, 1),
	}
}

// Register adds an issuer to the allowlist and records where its keys live.
//
// The allowlist is what makes "resolve the issuer before fetching a key" enforceable: an
// unknown `iss` never reaches this map, so it can never produce an outbound request. That is
// the SSRF defence, and it is why the JWKS URL is configuration rather than something read from
// the token or discovered from the issuer.
func (j *JWKS) Register(issuer, jwksURL string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.issuers[issuer] = &issuerKeys{url: jwksURL, keys: map[string]*Key{}, unknownKIDs: map[string]time.Time{}}
}

// Start launches the single background refresh goroutine.
//
// One goroutine for the whole cache, not one per issuer: the issuer count is small, the fetches
// are cheap, and a goroutine per issuer is a leak per misconfigured issuer. Start is idempotent
// and Stop is safe to call without Start, so a partially-constructed process can shut down
// cleanly.
func (j *JWKS) Start(ctx context.Context) {
	j.life.Lock()
	defer j.life.Unlock()
	if j.running || j.stopped {
		return
	}
	j.running = true
	go j.loop(ctx)
}

// Stop halts the background refresh and waits for the goroutine to exit. It is safe to call
// more than once, safe to call if Start never ran, and it is what keeps `go test -race` free of
// a goroutine outliving the test that created it.
func (j *JWKS) Stop() {
	j.life.Lock()
	if j.stopped {
		j.life.Unlock()
		<-j.done
		return
	}
	j.stopped = true
	close(j.stop)
	if !j.running {
		close(j.done)
	}
	j.life.Unlock()
	<-j.done
}

// Refreshed returns a channel signalled after each background refresh cycle. It exists so tests
// can observe the cache without sleeping; production code has no reason to read it.
func (j *JWKS) Refreshed() <-chan struct{} { return j.refreshed }

func (j *JWKS) loop(ctx context.Context) {
	defer close(j.done)
	ticker := time.NewTicker(j.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-j.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.RefreshAll(ctx)
			select {
			case j.refreshed <- struct{}{}:
			default:
			}
		}
	}
}

// RefreshAll refreshes every registered issuer, subject to the per-issuer rate limit. Exported
// so that a process can warm the cache at startup — a pod that begins life with no keys would
// otherwise reject every request until the first tick.
func (j *JWKS) RefreshAll(ctx context.Context) {
	j.mu.RLock()
	names := make([]string, 0, len(j.issuers))
	for name := range j.issuers {
		names = append(names, name)
	}
	j.mu.RUnlock()
	for _, name := range names {
		// Errors are intentionally not propagated: a failing issuer must not stop the others
		// from refreshing, and the failure is already recorded on the issuer entry where the
		// staleness ladder can see it.
		_ = j.Refresh(ctx, name)
	}
}

// ErrRateLimited is returned by Refresh when the per-issuer fetch budget is exhausted. It is a
// normal outcome, not a failure: it is the mechanism that bounds outbound load.
var ErrRateLimited = errors.New("authn: jwks refresh rate limit")

// ErrUnknownIssuer is returned for an issuer that is not on the allowlist.
var ErrUnknownIssuer = errors.New("authn: unknown issuer")

// Refresh fetches one issuer's key set, honouring the rate limit.
func (j *JWKS) Refresh(ctx context.Context, issuer string) error {
	j.mu.Lock()
	entry, ok := j.issuers[issuer]
	if !ok {
		j.mu.Unlock()
		return ErrUnknownIssuer
	}
	now := j.cfg.Clock.Now()
	if !entry.lastAttempt.IsZero() && now.Sub(entry.lastAttempt) < j.cfg.MinRefreshInterval {
		j.mu.Unlock()
		return ErrRateLimited
	}
	entry.lastAttempt = now
	url := entry.url
	j.mu.Unlock()

	// The fetch happens outside the lock so a slow or hanging issuer cannot block every
	// verification in the process behind it.
	body, err := j.fetch.Fetch(ctx, url)
	if err == nil && len(body) > j.cfg.MaxBytes {
		err = fmt.Errorf("authn: jwks document exceeds %d bytes", j.cfg.MaxBytes)
	}
	var keys map[string]*Key
	if err == nil {
		keys, err = parseJWKS(body, j.cfg.MaxKeys)
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if err != nil {
		// Keep the previous set. This is stale-if-error: the ladder in Key decides how long it
		// remains usable.
		entry.lastErr = err
		return err
	}
	entry.keys = keys
	entry.fetchedAt = j.cfg.Clock.Now()
	entry.lastErr = nil
	// A successful refresh clears the negative cache: the whole point of remembering an unknown
	// kid was to avoid re-asking, and we have just asked.
	entry.unknownKIDs = map[string]time.Time{}
	return nil
}

// Key resolves (issuer, kid) to a verification key.
//
// It never performs a fetch. The staleness ladder it applies:
//
//   - Fresh set (within TTL): serve.
//   - Stale set, refresh failing, within StaleIfError: serve, because the alternative is
//     rejecting valid traffic during someone else's outage.
//   - Beyond StaleIfError: refuse. The bound is what makes this degradation rather than drift.
//   - Never fetched at all: refuse. There is no key to be confident about, and inventing one is
//     not an option.
func (j *JWKS) Key(_ context.Context, issuer, kid string) (*Key, error) {
	j.mu.RLock()
	entry, ok := j.issuers[issuer]
	if !ok {
		j.mu.RUnlock()
		return nil, ErrUnknownIssuer
	}
	now := j.cfg.Clock.Now()
	key, present := entry.keys[kid]
	fetchedAt, hasSet := entry.fetchedAt, len(entry.keys) > 0
	negUntil, negative := entry.unknownKIDs[kid]
	j.mu.RUnlock()

	if !hasSet || fetchedAt.IsZero() {
		return nil, fmt.Errorf("authn: no key set for issuer %q", issuer)
	}
	if now.Sub(fetchedAt) > j.cfg.StaleIfError {
		// Past the bound the set is not evidence any more. Note that this is checked before the
		// key lookup succeeds, so an expired set cannot verify anything.
		return nil, fmt.Errorf("authn: key set for issuer %q is beyond the stale-if-error bound", issuer)
	}
	if present {
		return key, nil
	}
	if negative && now.Before(negUntil) {
		return nil, fmt.Errorf("authn: kid %q is known-unknown for issuer %q", kid, issuer)
	}
	j.mu.Lock()
	entry.unknownKIDs[kid] = now.Add(j.cfg.NegativeTTL)
	j.mu.Unlock()
	return nil, fmt.Errorf("authn: unknown kid %q for issuer %q", kid, issuer)
}

// SnapshotAge reports how long ago an issuer's key set was last successfully fetched. Exported
// for the readiness probe: a pod whose key set is beyond the stale-if-error bound cannot
// authenticate anyone and should not receive traffic.
func (j *JWKS) SnapshotAge(issuer string) (time.Duration, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	entry, ok := j.issuers[issuer]
	if !ok || entry.fetchedAt.IsZero() {
		return 0, false
	}
	return j.cfg.Clock.Now().Sub(entry.fetchedAt), true
}

// --- JWKS document parsing ---------------------------------------------------------------------

type jwkDocument struct {
	Keys []jwkEntry `json:"keys"`
}

type jwkEntry struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS converts a JWKS document into verification keys.
//
// Keys without a `kid`, without an `alg`, or whose `alg` is outside the platform's allowlist are
// dropped rather than rejected wholesale: an identity provider legitimately publishes keys for
// purposes other than ours (encryption keys, keys for a different algorithm), and refusing the
// whole document because of one would make our authentication depend on the IdP never adding
// anything. What is *not* tolerated is a key we would then use: an unparseable RSA modulus or a
// P-256 point is an error for that key, and the key is skipped.
func parseJWKS(body []byte, maxKeys int) (map[string]*Key, error) {
	var doc jwkDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("authn: jwks document is not decodable: %w", err)
	}
	if len(doc.Keys) > maxKeys {
		return nil, fmt.Errorf("authn: jwks document declares %d keys, limit is %d", len(doc.Keys), maxKeys)
	}
	out := make(map[string]*Key, len(doc.Keys))
	for _, e := range doc.Keys {
		if e.Kid == "" || !allowedAlgorithms[e.Alg] {
			continue
		}
		if e.Use != "" && e.Use != "sig" {
			continue
		}
		pub, err := e.publicKey()
		if err != nil {
			continue
		}
		out[e.Kid] = &Key{ID: e.Kid, Algorithm: e.Alg, Public: pub}
	}
	if len(out) == 0 {
		return nil, errors.New("authn: jwks document contains no usable signing key")
	}
	return out, nil
}

func (e jwkEntry) publicKey() (crypto.PublicKey, error) {
	switch e.Kty {
	case "RSA":
		n, err := b64uBigInt(e.N)
		if err != nil {
			return nil, err
		}
		eb, err := base64.RawURLEncoding.DecodeString(e.E)
		if err != nil || len(eb) == 0 || len(eb) > 8 {
			return nil, errors.New("authn: bad RSA exponent")
		}
		var exp int
		for _, b := range eb {
			exp = exp<<8 | int(b)
		}
		// A 2048-bit minimum. A key shorter than that is not a key we are willing to trust a
		// payment authorization to, whatever the issuer thinks.
		if n.BitLen() < 2048 {
			return nil, errors.New("authn: RSA key is shorter than 2048 bits")
		}
		return &rsa.PublicKey{N: n, E: exp}, nil
	case "EC":
		if e.Crv != "P-256" {
			return nil, fmt.Errorf("authn: unsupported curve %q", e.Crv)
		}
		x, err := b64uBigInt(e.X)
		if err != nil {
			return nil, err
		}
		y, err := b64uBigInt(e.Y)
		if err != nil {
			return nil, err
		}
		curve := elliptic.P256()
		// Reject a point that is not on the curve. An off-curve point is the entry point for
		// invalid-curve attacks, and the check costs one field operation.
		if !curve.IsOnCurve(x, y) {
			return nil, errors.New("authn: EC point is not on P-256")
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	default:
		return nil, fmt.Errorf("authn: unsupported key type %q", e.Kty)
	}
}

func b64uBigInt(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("authn: bad base64url integer")
	}
	return new(big.Int).SetBytes(b), nil
}

// --- HTTP fetcher --------------------------------------------------------------------------------

// HTTPFetcher retrieves JWKS documents over HTTP.
//
// It exists as a named type rather than an inline http.Get because every bound in it is a
// control: the timeout stops a hanging IdP from occupying a goroutine indefinitely, the response
// limit bounds what a malicious or compromised issuer can make us allocate, and redirects are
// refused because following one would let the issuer point us at an internal address — the
// classic SSRF pivot that the egress allowlist exists to prevent and that a redirect would walk
// straight around.
type HTTPFetcher struct {
	Client   *http.Client
	MaxBytes int64
}

// NewHTTPFetcher builds a fetcher with the security.md §3.3 bounds: a 2 second timeout, a 64 KB
// response limit, and no redirects.
func NewHTTPFetcher() *HTTPFetcher {
	return &HTTPFetcher{
		Client: &http.Client{
			Timeout: 2 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("authn: jwks fetch must not follow redirects")
			},
		},
		MaxBytes: DefaultJWKSMaxBytes,
	}
}

// Fetch retrieves the document at url.
func (f *HTTPFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authn: jwks fetch returned %d", resp.StatusCode)
	}
	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultJWKSMaxBytes
	}
	// Read one byte past the limit so an over-long body is detected rather than silently
	// truncated into a document that might still parse.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("authn: jwks document exceeds %d bytes", maxBytes)
	}
	return body, nil
}
