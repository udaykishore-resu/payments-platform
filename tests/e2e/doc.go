// Package e2e drives the whole platform over HTTP, exactly as a merchant's server would.
//
// Nothing in this package imports internal/. That is the point of it: every other suite in tests/
// reaches inside the process, and a suite that can see the aggregate can accidentally assert
// something no client could ever observe. These tests hold themselves to the published contract —
// the REST API in api/openapi/payments-platform.v1.yaml, the error catalog in
// api/errors/catalog.yaml, and the gateway simulator's own protocol — because that contract is
// what the platform actually promises.
//
// # What is asserted where
//
// The gateway's behaviour is selected by the payment's amount rather than by a control API. The
// simulator's documented trigger is the last two digits of the amount in *minor units*
// (internal/adapters/gateway/simulator/scenario.go): `…00` approves, `…02` soft-declines, `…05`
// returns a 3-D Secure challenge, `…07` holds the connection until the client's deadline expires,
// `…12` answers 500. Driving scenarios that way keeps these tests free of any back channel into
// the system under test, which matters: a test that can reach in and reconfigure a gateway is a
// test that can accidentally assert against a state no production request could produce.
//
// # Running it
//
//	make up                     # docker compose: services + simulator + postgres + redis + redpanda
//	export PP_TEST_BASE_URL=http://localhost:8080
//	export PP_TEST_SIMULATOR_URL=http://localhost:9090
//	export PP_TEST_AUTH_TOKEN="$(scripts/dev-token.sh)"
//	export PP_TEST_TENANT_ID=ten_...
//	go test -tags e2e ./tests/e2e/... -count=1
//
// Without those variables every test skips with a message naming the one it wanted. Every file
// except this one carries `//go:build e2e`, so an untagged `go test ./...` compiles this file and
// nothing else.
package e2e
