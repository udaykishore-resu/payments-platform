// Package grpcapi is the platform's internal gRPC surface: the same use cases the REST edge
// exposes, offered to callers inside the mesh over a binary protocol with mTLS peer identity.
//
// # The generated code is committed, and this package is split around it
//
// api/proto/payments/v1/*.proto declares the services. The Go bindings — `paymentsv1.
// PaymentServiceServer` and its siblings — are produced by `buf generate` and are committed
// alongside the .proto files:
//
//	buf generate --template api/proto/buf.gen.yaml api/proto
//
// Committing them was not the original plan, and the reason for the change is worth recording.
// The protoc plugins are not available in every environment where this repository is built, so
// the bindings were left out of the tree and this package was split:
//
//   - [server.go] — no build tag. The harness: interceptor chain, error mapping, health service,
//     keepalive, reflection, graceful stop. It compiles against grpc-go alone and is therefore
//     always built, always vetted and always covered by the race detector.
//   - [services.go] — `//go:build grpc`. The service implementations, written against the
//     generated types. It requires the bindings.
//
// The split is sound and remains. Leaving the bindings *uncommitted* was not, for two reasons
// that only showed up in use:
//
//   - `go mod tidy` loads every package in the module regardless of build tags. A tagged file
//     importing a package that does not exist on disk makes `tidy` fail for every developer,
//     including ones who will never build with `-tags grpc`. `go build ./...` and
//     `go test ./...` do respect build tags, so nothing else catches it.
//   - A tagged file nobody can compile locally drifts from the schema silently. This one had,
//     in eight places: it returned bare resources where the .proto declares response wrappers,
//     and read an `idempotency_key` field that the .proto carries inside `IdempotencyOptions`.
//
// The cost of committing generated code is real — a large diff on every regeneration, in which a
// hand-edit is hard to spot. That is paid for by CI, which regenerates and fails if the working
// tree changes, making a hand-edit a build failure rather than a review burden:
//
//	buf generate --template api/proto/buf.gen.yaml api/proto
//	git diff --exit-code api/proto
//	go build -tags grpc ./internal/transport/grpcapi/...
//	go vet   -tags grpc ./internal/transport/grpcapi/...
//
// # Why a gRPC surface at all, when REST already exists
//
// Not for speed. The reasons are the ones that survive a review:
//
//   - The mesh authenticates workloads with mTLS and SPIFFE identities. gRPC's per-RPC peer
//     identity is the natural carrier for that, whereas the REST edge is built around bearer
//     tokens issued to tenants.
//   - The .proto is a schema with a compatibility discipline — reserved field numbers, no
//     renumbering — that `buf breaking` enforces mechanically. An internal caller that upgrades
//     independently of the server needs exactly that guarantee, and OpenAPI's additive-only rule
//     is enforced by review.
//   - Streaming. The reconciler and the settlement ingest move batches that are natural as
//     streams and awkward as paginated reads.
//
// # The interceptor chain mirrors the HTTP middleware, and the mirroring is not cosmetic
//
// Recovery, tracing, logging, metrics, authentication, tenant, authorization, rate limit — the
// same order, the same failure modes, the same latency budgets from baseline §12. Two surfaces
// with different pipelines is two sets of controls to audit and one of them will be the one
// somebody forgot to update. See [ChainUnaryInterceptors] for the order and the reasoning.
package grpcapi
