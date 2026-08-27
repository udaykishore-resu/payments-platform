// Package httpapi is the public REST edge of the platform: the `/v1` surface declared in
// api/openapi/payments-platform.v1.yaml, plus the probe and metrics endpoints.
//
// The package is deliberately split into three pieces, and the split is the reason the whole
// thing stays reviewable:
//
//   - This package owns the *contract primitives*: the route table, the wire types, strict
//     decoding, RFC 9457 problem rendering, and the server with its timeouts. It imports
//     neither the middleware nor the handlers.
//   - [github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/middleware]
//     owns the §12 pipeline. It imports this package for problem rendering and the request
//     context accessors.
//   - [github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/handlers]
//     owns one file per resource and registers itself into a [Router]. It imports this package
//     for the DTOs and the decoder.
//
// The dependency arrows all point *into* this package, which is what removes the import cycle
// that the obvious layout (a router that knows its handlers, handlers that know how to render
// an error) produces. It also means the composition root is the single place where the chain
// order and the mounted resource set are visible, rather than being distributed across three
// files nobody reads together.
//
// # What a handler is allowed to do
//
// Decode, map to a command, call a service, render. Nothing else. A handler that branches on a
// payment's state, computes an amount, or decides whether an operation is permitted has taken a
// decision that belongs in internal/application, where it is unit-testable without an HTTP
// request and where it applies to the gRPC surface and the workflow engine too.
//
// # Errors
//
// A handler returns `error`, never writes one. [Router.Handle] renders whatever comes back
// through [WriteProblem], which is the only place in the process that turns an *apierror.Error
// into bytes on a socket. One place means one set of rules about what may and may not appear in
// a response body, and [TestProblemNeverLeaksInternalErrorText] asserts them.
//
// See docs/spec/00-design-baseline.md §12 (pipeline), §19 (surface) and §20 (error model).
package httpapi
