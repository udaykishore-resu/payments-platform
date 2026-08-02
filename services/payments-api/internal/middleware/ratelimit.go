package middleware

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimit enforces a per-client token bucket, keyed on the authenticated client_id (not IP —
// IP-based limiting would punish every client behind a shared NAT/gateway for one bad actor's
// behavior). This is the app-level backstop described in docs/05-security-architecture.md,
// "Rate Limiting & Abuse Prevention" — the WAF's coarse IP-based rate rule sits in front of this
// at the edge; this is the fair, identity-aware layer behind it. It is also the primary defense
// against retry storms / thundering herd from a single misbehaving client (see
// docs/04-failure-recovery-design.md).
//
// Must run AFTER the Auth middleware in the chain, since it depends on ClientFromContext.
func RateLimit(rps float64, burst int) func(http.Handler) http.Handler {
	limiters := &clientLimiters{
		byClient: make(map[string]*rate.Limiter),
		rps:      rate.Limit(rps),
		burst:    burst,
	}
	// Periodically evict idle clients' limiters so this map doesn't grow unboundedly for a
	// service with many distinct clients over its lifetime (a small, deliberate leak-prevention
	// detail that's easy to skip and then page someone six months later).
	go limiters.evictLoop()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientID := "anonymous"
			if claims, ok := ClientFromContext(r.Context()); ok {
				clientID = claims.ClientID
			}
			if !limiters.forClient(clientID).Allow() {
				w.Header().Set("Retry-After", "1")
				writeJSONError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, back off and retry")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type clientLimiters struct {
	mu       sync.Mutex
	byClient map[string]*rate.Limiter
	lastUsed map[string]time.Time
	rps      rate.Limit
	burst    int
}

func (c *clientLimiters) forClient(clientID string) *rate.Limiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastUsed == nil {
		c.lastUsed = make(map[string]time.Time)
	}
	l, ok := c.byClient[clientID]
	if !ok {
		l = rate.NewLimiter(c.rps, c.burst)
		c.byClient[clientID] = l
	}
	c.lastUsed[clientID] = time.Now()
	return l
}

func (c *clientLimiters) evictLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		c.mu.Lock()
		for id, last := range c.lastUsed {
			if time.Since(last) > 30*time.Minute {
				delete(c.byClient, id)
				delete(c.lastUsed, id)
			}
		}
		c.mu.Unlock()
	}
}
