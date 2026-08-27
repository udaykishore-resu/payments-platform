// Package middleware implements the cross-cutting HTTP concerns described in
// docs/05-security-architecture.md: authentication, rate limiting, panic recovery, and request
// correlation. Each middleware is a plain `func(http.Handler) http.Handler` so they compose via
// simple wrapping in internal/api/router.go with no framework dependency.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

type contextKeyClient struct{}

// ClientClaims is the subset of the verified JWT we care about downstream.
type ClientClaims struct {
	ClientID string
	Scopes   []string
}

func ClientFromContext(ctx context.Context) (ClientClaims, bool) {
	v, ok := ctx.Value(contextKeyClient{}).(ClientClaims)
	return v, ok
}

// Auth validates the bearer JWT against the configured JWKS, issuer, and audience (OAuth2/OIDC —
// see docs/05-security-architecture.md, "Client -> API"). It never trusts a client-supplied role
// or scope claim without a valid signature over it: signature verification happens before any
// claim is read.
//
// disabled should be true ONLY in local development (enforced at startup in internal/config —
// config.validate() refuses to start with AuthDisabled=true when Env=="production").
func Auth(jwksURL, issuer, audience string, disabled bool) func(http.Handler) http.Handler {
	cache := newJWKSCache(jwksURL)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if disabled {
				ctx := context.WithValue(r.Context(), contextKeyClient{}, ClientClaims{ClientID: "local-dev"})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			authHeader := r.Header.Get("Authorization")
			tokenString, ok := strings.CutPrefix(authHeader, "Bearer ")
			if !ok || tokenString == "" {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}

			claims := jwt.MapClaims{}
			_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
				kid, _ := t.Header["kid"].(string)
				return cache.get(kid)
			},
				jwt.WithValidMethods([]string{"RS256"}),
				jwt.WithIssuer(issuer),
				jwt.WithAudience(audience),
			)
			if err != nil {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
				return
			}

			clientID, _ := claims["sub"].(string)
			if clientID == "" {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", "token missing subject")
				return
			}

			var scopes []string
			if raw, ok := claims["scope"].(string); ok {
				scopes = strings.Fields(raw)
			}

			ctx := context.WithValue(r.Context(), contextKeyClient{}, ClientClaims{ClientID: clientID, Scopes: scopes})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireScope enforces least-privilege authorization (ABAC-style scope check) on top of the
// authentication already performed by Auth. See docs/05-security-architecture.md,
// "Authorization model".
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClientFromContext(r.Context())
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", "no authenticated client")
				return
			}
			hasScope := false
			for _, s := range claims.Scopes {
				if s == scope {
					hasScope = true
					break
				}
			}
			if !hasScope {
				writeJSONError(w, http.StatusForbidden, "forbidden", "missing required scope: "+scope)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
