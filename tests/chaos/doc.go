// Package chaos holds the fault-injection suite: docs/testing.md §6.3's scenarios C-1 … C-28 and
// the failure catalog of baseline §24, expressed as tests.
//
// # What a chaos test is, here
//
// Every test in this package has the same four parts, and a test missing any of them is not a
// chaos test:
//
//  1. A **steady-state hypothesis**: the properties that must hold, stated as code, before the
//     fault, during it, and after it. Not "the system recovered" — the properties.
//  2. A **fault**, injected through a named, composable decorator over a port.
//  3. An **observation** of what the system did while the fault was in force.
//  4. An assertion that the hypothesis **still holds**, sampled throughout rather than checked at
//     the end. A hypothesis checked only afterwards cannot distinguish "the invariant held" from
//     "the invariant broke and the system corrected itself", and the second is exactly the shape
//     of the bug that surfaces later as an unexplained duplicate charge.
//
// # Why most of this runs in-process
//
// The scenarios are driven against the *real* payment orchestrator, the *real* gateway simulator
// and the *real* resilience primitives (breaker, bulkhead, adaptive limiter, retry budget), wired
// to in-memory ports from internal/application/apptest. Nothing is stubbed that the scenario is
// about.
//
// That is a deliberate trade. Pausing a container proves the deployment reacts; decorating a port
// proves the *code* reacts, deterministically, in under a second, on every pull request. The
// scenarios that genuinely need infrastructure to be stopped — an Aurora failover, a broker
// outage, a node loss — are gated behind PP_TEST_CHAOS_INFRA and skip loudly without it, because a
// laptop is not a place to pause a database and a green run that silently skipped them would be a
// statement about the runner rather than about the system.
//
// Run it with:
//
//	go test -tags chaos ./tests/chaos/... -race -count=1
//
// Every file in this package except this one carries `//go:build chaos`, so an untagged
// `go test ./...` compiles this file and nothing else — which keeps `go vet ./tests/...` from
// reporting a directory with no buildable Go files.
package chaos
