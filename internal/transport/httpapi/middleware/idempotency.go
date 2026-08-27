package middleware

import (
	"net/http"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// DefaultMaxSnapshotBytes bounds a buffered response body stored in an idempotency record.
//
// 128 KiB against a largest legitimate response — a payment with its attempts, refunds and an
// expanded routing plan — of a few tens of kilobytes. Past the cap the middleware declines to
// store a snapshot, so a duplicate re-executes instead of replaying. That is a correctness-
// preserving degradation (the operation is still idempotent at the aggregate, which is where it
// actually matters) rather than an eviction, and it is what stops one large export from being
// copied into memory once per worker.
const DefaultMaxSnapshotBytes = 128 << 10

// Idempotency is §12 stages 8 and 17: claim before the handler, snapshot after it.
//
// Budget: 8 ms for the claim, 5 ms for the completion.
// Fails with: 400 IDEMPOTENCY_KEY_REQUIRED, 409 IDEMPOTENT_REQUEST_IN_PROGRESS,
// 422 IDEMPOTENCY_KEY_REUSED.
//
// # The full lifecycle, and why each branch is what it is
//
//	NEW / RECLAIMED  → run the handler, then:
//	                     2xx                      Complete(snapshot)  — future duplicates replay
//	                     non-retryable 4xx/5xx    FailTerminal(snapshot)
//	                     retryable    5xx         Release()
//	REPLAY           → write the stored response verbatim with Idempotent-Replay: true
//	IN_PROGRESS      → 409 + Retry-After, without blocking
//	MISMATCH         → 422
//
// The Release / FailTerminal distinction is the subtle part and it is the reason both exist. A
// declined payment is *terminal*: re-running it produces the same decline, consumes another
// gateway call and another risk evaluation, and looks to the card scheme like retry abuse — so
// the error is stored and replayed. A database timeout is *not* terminal: the next attempt may
// well succeed, and replaying the stored error would strand a client on a failure that has since
// cleared, with no way to retry short of minting a new key — which is exactly what the contract
// tells them never to do on the money path.
//
// # Why the claim is the innermost stage
//
// A key consumed by a request that authentication, tenancy, authorization or the rate limiter
// rejected is a key the client must not reuse and cannot distinguish from one that did work.
// Placing the claim last means only requests that were going to be executed ever touch a record.
//
// # In-progress does not block
//
// Baseline §14.3 and ADR-009. Holding the second caller on the first one's lease turns a
// duplicate into a queue, and a queue on a shared connection pool under retry storm is how a
// duplicate-submit button takes the API down. 409 with Retry-After: 1 is a complete, actionable
// answer that the client waits out in its own process.
func Idempotency(mgr IdempotencyManager, maxSnapshotBytes int) Middleware {
	if maxSnapshotBytes <= 0 {
		maxSnapshotBytes = DefaultMaxSnapshotBytes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			template := httpapi.RouteTemplate(r.Context())
			if !RequiresIdempotencyKey(r.Method, template) {
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get(httpapi.HeaderIdempotencyKey)
			if key == "" {
				httpapi.WriteProblem(w, r, apierror.New(apierror.CodeIdempotencyKeyRequired,
					"this operation requires an Idempotency-Key header").
					WithDetail(apierror.Detail{
						Field:   httpapi.HeaderIdempotencyKey,
						Code:    "MISSING",
						Message: "Generate one key per logical operation, before the first attempt, and reuse it on every retry.",
						RuleID:  "L1.IDEMPOTENCY_KEY_PRESENT",
					}))
				return
			}
			if mgr == nil {
				// Fail closed. Serving an unsafe operation with no idempotency store is how one
				// client retry becomes two payments, and that is not a degradation anybody would
				// consent to.
				httpapi.WriteProblem(w, r, apierror.New(apierror.CodeServiceUnavailable,
					"the idempotency store is not configured; unsafe operations are refused").
					WithRetryAfter(5))
				return
			}

			tc, err := tenantctx.FromContext(r.Context())
			if err != nil {
				httpapi.WriteProblem(w, r, err)
				return
			}
			scope := ports.IdempotencyKey{
				TenantID:     tc.TenantID,
				MerchantID:   shared.MerchantID(merchantFromPath(template, r.URL.EscapedPath())),
				Method:       r.Method,
				PathTemplate: template,
				Key:          key,
			}

			handle, err := mgr.Begin(r.Context(), scope, httpapi.RawBody(r.Context()))
			if err != nil {
				// Begin returns a handle *and* an error for REPLAY-with-unreadable-snapshot,
				// IN_PROGRESS and MISMATCH. All three are answered by rendering the error; none
				// of them owns a claim, so there is nothing to release.
				httpapi.WriteProblem(w, r, err)
				return
			}
			if handle.IsReplay() {
				replay(w, r, handle.Snapshot())
				return
			}

			rec, _ := recorderFor(w)
			rec.Buffer(maxSnapshotBytes)

			// Release is deferred immediately, and is a no-op once Complete or FailTerminal has
			// run. The alternative — clearing a flag on every success path — holds until the
			// third early return is added to this function.
			settled := false
			defer func() {
				if settled {
					return
				}
				// Reached only on a panic below, after the recover middleware has already
				// written a 500. A 500 from a panic is not evidence the operation is
				// impossible, so the claim is released and the client's retry is real.
				ctx, cancel := settleContext(r)
				defer cancel()
				_ = handle.Release(ctx)
			}()

			next.ServeHTTP(rec, r)

			body, complete := rec.Body()
			status := rec.Status()
			settled = true

			// The store write runs on a context detached from the request's cancellation. A
			// client that hangs up after the handler committed must not leave the record
			// IN_FLIGHT: the operation happened, and the next duplicate has to replay it rather
			// than execute it a second time.
			ctx, cancel := settleContext(r)
			defer cancel()

			switch {
			case status >= 200 && status < 300:
				if !complete {
					// Response too large to store. The claim is released rather than completed,
					// because a completed record with no body replays as a dependency failure —
					// strictly worse than re-executing an operation the aggregate will
					// recognise as already done.
					_ = handle.Release(ctx)
					return
				}
				_ = handle.Complete(ctx, status, body, resourceIDOf(w))
			case isRetryableStatus(status):
				_ = handle.Release(ctx)
			default:
				if !complete {
					_ = handle.Release(ctx)
					return
				}
				_ = handle.FailTerminal(ctx, status, body, resourceIDOf(w))
			}
		})
	}
}

// replay writes a stored response verbatim.
//
// Verbatim matters: the stored bytes are what the first caller received, and re-rendering from
// the current aggregate would produce a *different* document — a payment that has since been
// captured would replay as CAPTURED to a client whose original response said AUTHORIZED. The
// point of a replay is that the duplicate is indistinguishable from the original, and that
// property is only true if the bytes are the same bytes.
func replay(w http.ResponseWriter, r *http.Request, snap *ports.ResponseSnapshot) {
	if snap == nil {
		httpapi.WriteProblem(w, r, apierror.New(apierror.CodeDependencyFailure,
			"the stored response for this idempotency key could not be read"))
		return
	}
	h := w.Header()
	h.Set(httpapi.HeaderContentType, httpapi.MediaJSON)
	h.Set(httpapi.HeaderCacheControl, "no-store")
	h.Set(httpapi.HeaderIdempotentReply, "true")
	if snap.ResourceID != "" {
		h.Set(httpapi.HeaderETag, `"replay"`)
		h.Del(httpapi.HeaderETag)
	}
	w.WriteHeader(snap.StatusCode)
	_, _ = w.Write(snap.Body)
}

// resourceIDOf recovers the created resource's identifier from the Location header the handler
// set, so a client that only wants the id need not parse the stored body on replay.
//
// Reading it from the header rather than from the body is what keeps this middleware ignorant of
// every response schema on the surface. A middleware that parsed the body to find `id` would
// have to know that a refund's identifier lives at `.id` and an onboarding signal's at
// `.caseId`, which is the resource knowledge the layering exists to keep out of here.
func resourceIDOf(w http.ResponseWriter) string {
	loc := w.Header().Get(httpapi.HeaderLocation)
	if loc == "" {
		return ""
	}
	for i := len(loc) - 1; i >= 0; i-- {
		if loc[i] == '/' {
			return loc[i+1:]
		}
	}
	return loc
}

// isRetryableStatus reports whether a status means "try again", per baseline §19.3: 502, 503 and
// 504 are explicitly retryable, everything else is not.
//
// 500 is deliberately absent. An INTERNAL_ERROR is a bug, and a bug reproduces: replaying the
// stored 500 to a retrying client is both honest and cheaper than running the failing code path
// again on every retry of every client that hit it.
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}
