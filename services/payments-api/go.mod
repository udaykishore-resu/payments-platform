module github.com/example/payments-platform/services/payments-api

go 1.22

require (
	github.com/aws/aws-sdk-go-v2 v1.27.0
	github.com/aws/aws-sdk-go-v2/config v1.27.11
	github.com/aws/aws-sdk-go-v2/service/sqs v1.31.4
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.5.5
	github.com/prometheus/client_golang v1.19.0
	go.opentelemetry.io/otel v1.24.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.24.0
	go.opentelemetry.io/otel/sdk v1.24.0
	go.opentelemetry.io/otel/trace v1.24.0
	golang.org/x/time v0.5.0
)

// NOTE: go.sum is intentionally not committed from this environment (no network access to the
// Go module proxy was available while authoring this repo). The CI pipeline's first step runs
// `go mod tidy && go mod verify` before build/test, which will generate and commit a verified
// go.sum on the first successful run. See docs/09-production-checklist.md.
