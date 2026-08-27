package paypal

import (
	"context"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TokenPath is the OAuth2 client-credentials endpoint.
const TokenPath = "/v1/oauth2/token"

// refreshFraction is the point in a token's life at which it is replaced.
//
// 0.8 rather than 1.0 because a token refreshed exactly at expiry is a token that expires
// mid-flight for every request already in the transport. PayPal issues nine-hour tokens, so
// refreshing at 80% leaves nearly two hours of overlap — far more than any request's deadline —
// and costs one extra exchange per working day.
const refreshFraction = 0.8

// minTokenLifetime guards against a token endpoint answering with a tiny or zero `expires_in`.
// Treating such a token as immediately stale produces a refresh storm; treating it as long-lived
// produces a wall of 401s. Clamping to a floor keeps the failure bounded and visible.
const minTokenLifetime = 30 * time.Second

// tokenCache holds PayPal bearer tokens, one per (environment, client id).
//
// Keyed rather than single-slot because a deployment can legitimately carry more than one OAuth
// client — a live client and a sandbox client during certification, or two clients mid-rotation —
// and a single-slot cache would thrash between them and re-exchange on every request.
//
// The concurrency design is the part worth reading. The obvious implementation holds a mutex
// across the HTTP exchange: correct as a single-flight, but every waiter blocks uninterruptibly, so
// a slow token endpoint makes an entire burst of payments miss their deadlines together. This
// implementation publishes an in-flight channel instead. The first caller performs the exchange;
// the others wait on the channel *while still honouring their own context*, so one exchange serves
// the burst and a caller whose deadline expires gives up alone.
type tokenCache struct {
	mu      sync.Mutex
	entries map[string]*tokenEntry
	clock   shared.Clock
}

type tokenEntry struct {
	token     string
	expiresAt time.Time
	// inflight is non-nil while a refresh is running. It is closed, and the field cleared, when the
	// exchange finishes — successfully or not.
	inflight chan struct{}
}

func newTokenCache(clock shared.Clock) *tokenCache {
	return &tokenCache{entries: make(map[string]*tokenEntry), clock: clock}
}

// cacheKey includes the environment so a sandbox token can never be presented to the live
// endpoint. That is the same class of mistake spi.Credentials.Environment exists to prevent, and it
// is cheap enough to defend twice.
func cacheKey(creds spi.Credentials, clientID string) string {
	return string(creds.Environment) + "|" + clientID
}

// token returns a valid bearer token for these credentials, exchanging one if needed.
//
// The single-flight guard is what stops a cold start stampeding PayPal's token endpoint: a pod
// coming up under load has every in-flight authorization needing a token at the same instant, and
// PayPal rate-limits that endpoint far more aggressively than the payment endpoints. One exchange
// serves the whole burst.
func (c *tokenCache) token(ctx context.Context, doer spi.HTTPDoer, baseURL string, creds spi.Credentials) (string, error) {
	clientID, err := credential(creds, CredentialClientID)
	if err != nil {
		return "", err
	}
	key := cacheKey(creds, clientID)

	for {
		if err := ctx.Err(); err != nil {
			return "", contextError(err)
		}

		c.mu.Lock()
		e, ok := c.entries[key]
		switch {
		case ok && e.inflight == nil && e.token != "" && c.clock.Now().Before(e.expiresAt):
			tok := e.token
			c.mu.Unlock()
			return tok, nil
		case ok && e.inflight != nil:
			wait := e.inflight
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", contextError(ctx.Err())
			case <-wait:
				// Loop rather than reading the entry here. By the time this caller wakes the token
				// may already have been invalidated by a 401 observed elsewhere, and re-entering the
				// loop is the only way to observe that.
				continue
			}
		}

		// This goroutine owns the refresh.
		done := make(chan struct{})
		c.entries[key] = &tokenEntry{inflight: done}
		c.mu.Unlock()

		tok, ttl, exchangeErr := exchange(ctx, doer, baseURL, creds)

		c.mu.Lock()
		if exchangeErr != nil {
			// Remove the entry rather than caching the failure. A token exchange that failed because
			// *this* caller's context was cancelled must not poison a healthy caller; the waiters
			// woken below find no entry and one of them attempts its own exchange.
			delete(c.entries, key)
		} else {
			c.entries[key] = &tokenEntry{token: tok, expiresAt: c.clock.Now().Add(ttl)}
		}
		c.mu.Unlock()
		close(done)

		if exchangeErr != nil {
			return "", exchangeErr
		}
		return tok, nil
	}
}

// invalidate drops the cached token for these credentials.
//
// Called when PayPal answers 401 to a request carrying a token the cache believed was live, which
// happens after an operator revokes a client secret mid-rotation. Without it the adapter would keep
// presenting a dead token until its nominal expiry — up to nine hours of hard failure on a
// merchant's traffic.
func (c *tokenCache) invalidate(creds spi.Credentials) {
	clientID, err := credential(creds, CredentialClientID)
	if err != nil {
		return
	}
	key := cacheKey(creds, clientID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && e.inflight == nil {
		delete(c.entries, key)
	}
}

// exchange performs the client-credentials grant.
//
// The client id and secret travel in the Authorization header as HTTP Basic, which is what PayPal
// requires. They are assembled and consumed on adjacent lines and are never stored in a field,
// rendered into an error, or logged. The body is the fixed `grant_type=client_credentials`; nothing
// in it is caller-derived, which is why it is a constant rather than a builder.
func exchange(ctx context.Context, doer spi.HTTPDoer, baseURL string, creds spi.Credentials) (string, time.Duration, error) {
	clientID, err := credential(creds, CredentialClientID)
	if err != nil {
		return "", 0, err
	}
	clientSecret, err := credential(creds, CredentialClientSecret)
	if err != nil {
		return "", 0, err
	}
	resp, err := doer.Do(&spi.HTTPRequest{
		Ctx:    ctx,
		Method: http.MethodPost,
		URL:    baseURL + TokenPath,
		Headers: map[string]string{
			"Authorization":       "Basic " + base64.StdEncoding.EncodeToString([]byte(clientID+":"+clientSecret)),
			"Content-Type":        "application/x-www-form-urlencoded",
			"Accept":              "application/json",
			httpx.OperationHeader: "oauth",
		},
		Body: []byte("grant_type=client_credentials"),
	})
	if err != nil {
		return "", 0, err
	}
	if resp == nil {
		return "", 0, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the transport returned neither a response nor an error")
	}
	if resp.Timeout {
		// A token exchange moves no money, so a timeout here is a plain retryable failure. It must
		// not become an unknown outcome: doing so would park a payment in reconciliation over a
		// failure that provably never reached a payment endpoint.
		return "", 0, apierror.New(apierror.CodeGatewayTimeout, "paypal: the token exchange timed out")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", 0, apierror.Wrap(spi.ErrCredentialsInvalid, apierror.CodeGatewayAuthenticationFailed,
			"paypal: the OAuth client credentials were rejected")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, apierror.Newf(apierror.CodeGatewayUnavailable,
			"paypal: the token endpoint returned HTTP %d", resp.StatusCode)
	}
	var tr tokenResponse
	if err := decode(resp.Body, &tr); err != nil {
		return "", 0, err
	}
	if tr.AccessToken == "" {
		return "", 0, apierror.New(apierror.CodeGatewayContractViolation,
			"paypal: the token response carries no access_token")
	}
	ttl := time.Duration(float64(tr.ExpiresIn) * refreshFraction * float64(time.Second))
	if ttl < minTokenLifetime {
		ttl = minTokenLifetime
	}
	return tr.AccessToken, ttl, nil
}
