//go:build tools

// Package tools pins the module's runtime dependencies so that `go mod tidy` keeps them in
// go.mod even while a package that uses one is being written or temporarily commented out.
//
// This is the standard Go idiom for dependency pinning (the same trick used for build-time
// tools), applied here for a slightly different reason: this repository is built incrementally
// and in parallel, and a `go mod tidy` run at a moment when the Kafka adapter happens to be
// half-written would silently drop the Kafka dependency and break the next build with a
// confusing "no required module provides package" rather than an honest compile error.
//
// The `tools` build tag means nothing here is compiled into any binary. Verify that with
// `go build ./...` — this file contributes zero bytes to every artifact.
package tools

import (
	_ "github.com/golang-jwt/jwt/v5"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/prometheus/client_golang/prometheus"
	_ "github.com/redis/go-redis/v9"
	_ "github.com/twmb/franz-go/pkg/kgo"
	_ "go.opentelemetry.io/otel"
	_ "gopkg.in/yaml.v3"
)
