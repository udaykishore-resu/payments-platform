// The Temporal adapter is a module of its own, and that is the whole mechanism by which the
// Temporal SDK stays out of the platform's dependency graph.
//
// A build tag is not enough. `go mod tidy` loads every package in the parent module regardless
// of build tags, so a tagged file importing go.temporal.io/sdk forces the parent to require it —
// and once it is in the parent's build list, minimal version selection drags the rest of the
// graph forward with it. Measured, on the first attempt: OpenTelemetry 1.35.0 → 1.43.0,
// grpc-go 1.71.0 → 1.82.1, protobuf 1.36.5 → 1.36.11, golang.org/x/net 0.35 → 0.55, and the go
// directive from 1.24.7 to 1.25.4, which broke every container image at `go mod download`. None
// of that was reviewed and none of it was wanted; it was the transitive cost of one optional
// adapter being visible to `tidy`.
//
// A directory containing a go.mod is excluded from its parent's package loading entirely. So the
// parent keeps its pinned graph and its 1.24.7 toolchain, and this module carries the SDK — and
// whatever the SDK drags with it — alone.
//
// The dependency direction is one-way: this module requires the platform, the platform requires
// nothing here. That is what makes the exclusion safe. `replace` points at the working tree
// because the two are versioned together in one repository; a tagged release of the platform
// would replace it with a version requirement.
//
// Working on this module:
//
//	cd internal/workflows/engine/temporal
//	go mod tidy
//	go build -tags temporal ./...
//	go test  -tags temporal ./...
//
// This module's go directive is 1.25.4, not the platform's 1.24.7, because go.temporal.io/sdk
// v1.48.0 declares 1.25.4 and a module cannot require one that needs a newer toolchain than it
// does. Building this adapter therefore needs Go 1.25.4+; building the platform still does not.
// That asymmetry is only expressible because these are two modules.
//
// The `-tags temporal` constraint is kept even though the module boundary already excludes this
// code from the default build. It is what lets a developer here run `go build ./...` and get a
// fast SDK-free syntax check, and it keeps the tag documented in the source rather than only in
// a CI script.
module github.com/udaykishore-resu/payments-platform/internal/workflows/engine/temporal

go 1.25.4

require (
	github.com/udaykishore-resu/payments-platform v0.0.0
	go.temporal.io/api v1.63.5
	go.temporal.io/sdk v1.48.0
)

replace github.com/udaykishore-resu/payments-platform => ../../../..
