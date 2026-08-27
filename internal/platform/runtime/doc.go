// Package runtime is the shared mechanism that every composition root calls, and nothing else.
//
// # Why this package exists at all
//
// docs/lld.md §2.2 chose manual constructor injection over a DI framework, and accepted that
// `cmd/<binary>/main.go` would be 200–350 lines of explicit wiring. That decision holds. What it
// does not require is nine copies of the *same* thirty lines: the signal handling, the ordered
// drain, the build stamp, the redacted configuration dump. Those are mechanism, not architecture,
// and duplicating them nine times means a fix to the drain ordering lands in eight binaries and
// is forgotten in the ninth — which is exactly the class of bug the explicit wiring was chosen to
// prevent.
//
// So the split is: this package owns *how* a process starts and stops; each `main.go` owns *what*
// that process is. A reader of main.go sees the list of dependencies and the order they are
// constructed in; a reader of this package sees the lifecycle. Neither hides the other.
//
// There is deliberately no `NewEverything()`. Every constructor a binary needs is called by name
// in that binary's own file.
//
// # The drain sequence
//
// [Lifecycle.Run] implements docs/lld.md §2.5 exactly, and the ordering rule is the whole point:
//
//  1. SIGTERM arrives (concurrently with — not after — endpoint removal).
//  2. Fail readiness immediately, before anything is closed. A pod that closed its listener
//     first would refuse connections that kube-proxy on some nodes is still routing to it,
//     producing connection resets clients see as 502s on every deploy.
//  3. Wait the drain delay, so endpoint propagation wins the race.
//  4. Stop accepting new work and let in-flight work finish, under a per-binary budget.
//  5. Close resources in reverse dependency order.
//  6. Flush telemetry last, so every step above is observable.
//
// The budgets come from docs/deployment.md §1.8 and are stated per binary in [Budgets].
package runtime
