// Package grpcapi is the platform's internal gRPC surface: the same use cases the REST edge
// exposes, offered to callers inside the mesh over a binary protocol with mTLS peer identity.
//
// # The generated code is not in this repository, and this package is built without it
//
// api/proto/payments/v1/*.proto declares the services. The Go bindings — `paymentsv1.
// PaymentServiceServer` and its siblings — are produced by `buf generate` and are deliberately
// not committed:
//
//	buf generate --template api/proto/buf.gen.yaml api/proto
//
// The protoc plugins are not available in every environment where this repository is built, and a
// package whose default build depends on them is a package that fails `go build ./...` on a
// developer laptop for reasons unrelated to the change being made. So the split is:
//
//   - [server.go] — no build tag. The harness: interceptor chain, error mapping, health service,
//     keepalive, reflection, graceful stop. It compiles against grpc-go alone and is therefore
//     always built, always vetted and always covered by the race detector.
//   - [services.go] — `//go:build grpc`. The service implementations, written against the
//     generated types as they will be named. It compiles only after codegen has run.
//
// CI runs codegen and then builds this package with `-tags grpc`, so the tagged file is not
// exempt from the build — it is exempt only from the *default* build:
//
//	buf generate --template api/proto/buf.gen.yaml api/proto
//	go build -tags grpc ./internal/transport/grpcapi/...
//	go vet   -tags grpc ./internal/transport/grpcapi/...
//
// The tagged file being unbuildable locally is a real cost. It is paid because the alternative —
// committing generated code — makes every regeneration a large, unreviewable diff in which a
// hand-edit is invisible, and makes the checked-in bindings drift from the .proto whenever
// somebody edits one and not the other.
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
