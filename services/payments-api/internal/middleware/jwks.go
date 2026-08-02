package middleware

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// jwksCache fetches and caches a JSON Web Key Set from the identity provider (Cognito/Auth0),
// refreshing on a TTL and on unknown-kid cache misses (handles key rotation gracefully without
// requiring a deploy — see docs/05-security-architecture.md, "Key rotation").
type jwksCache struct {
	url string

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	ttl       time.Duration

	httpClient *http.Client
}

func newJWKSCache(url string) *jwksCache {
	return &jwksCache{
		url:        url,
		keys:       make(map[string]*rsa.PublicKey),
		ttl:        15 * time.Minute,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (c *jwksCache) get(kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	key, ok := c.keys[kid]
	stale := time.Since(c.fetchedAt) > c.ttl
	c.mu.RUnlock()

	if ok && !stale {
		return key, nil
	}

	// Cache miss or stale — refresh from the identity provider. This also covers the "IdP
	// rotated the signing key" case: a token signed with a new kid we haven't seen yet triggers
	// exactly one refresh, not a refresh per request.
	if err := c.refresh(); err != nil {
		// If refresh fails but we still have a (stale) cached key for this kid, prefer serving
		// requests over hard-failing auth during a transient IdP outage.
		c.mu.RLock()
		defer c.mu.RUnlock()
		if key, ok := c.keys[kid]; ok {
			return key, nil
		}
		return nil, fmt.Errorf("jwks: refresh failed and no cached key for kid %q: %w", kid, err)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	key, ok = c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("jwks: unknown kid %q after refresh", kid)
	}
	return key, nil
}

func (c *jwksCache) refresh() error {
	req, err := http.NewRequest(http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: unexpected status %d fetching %s", resp.StatusCode, c.url)
	}

	var set jwkSet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("jwks: decode: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pub, err := rsaPublicKeyFromJWK(k)
		if err != nil {
			continue // skip malformed key rather than failing the whole refresh
		}
		keys[k.Kid] = pub
	}

	c.mu.Lock()
	c.keys = keys
	c.fetchedAt = time.Now()
	c.mu.Unlock()
	return nil
}

func rsaPublicKeyFromJWK(k jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}
