// Package integration holds the cross-cutting integration suite: the assertions that can only be
// made against a real PostgreSQL, Redis and Kafka.
//
// Every file in this package carries `//go:build integration`, so an untagged `go test ./...`
// compiles this doc file and nothing else. That is deliberate: the tag keeps a laptop with no
// services green, and this untagged file keeps `go vet ./tests/...` from reporting a directory
// with no buildable Go files.
//
// Run it with:
//
//	go test -tags integration ./tests/integration/... -race -count=1
//
// Without PP_TEST_POSTGRES_DSN (and, for the event tests, PP_TEST_KAFKA_BROKERS and
// PP_TEST_REDIS_ADDR) every test skips with a message naming the variable it wanted.
package integration
