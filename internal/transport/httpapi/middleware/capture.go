package middleware

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

// ResponseRecorder wraps an http.ResponseWriter so the chain can observe — and, for the
// idempotency middleware, store — what a handler produced.
//
// # Why the interface assertions matter
//
// A naive wrapper is a struct embedding http.ResponseWriter, and it silently destroys two
// capabilities. Embedding promotes methods, so the wrapper *appears* to implement
// http.Flusher and http.Hijacker whenever the underlying writer does — but Go's interface
// satisfaction is structural on the wrapper type, so `w.(http.Flusher)` succeeds and calls a
// method whose receiver forwards to a writer that may not implement it, or fails at a type
// assertion inside net/http. The correct shape is an explicit, non-embedding wrapper with
// Flush and Hijack implemented as guarded forwards, which is what this is.
//
// Flush matters because a streaming response that never flushes is a response that arrives in
// one lump at the end; Hijack matters because without it no upgrade — WebSocket, h2c — can
// ever work through the chain, and the failure is a 500 at connection-upgrade time that looks
// like a client bug.
type ResponseRecorder struct {
	w http.ResponseWriter

	// status is the code passed to WriteHeader, defaulting to 200 because net/http implies it
	// on the first Write. A recorder that reports 0 for an implicit 200 makes every metric on
	// the happy path a lie.
	status int
	// bytes counts what was written, for the access log's response_size.
	bytes int
	// wroteHeader guards against the double-WriteHeader panic that net/http logs and swallows.
	wroteHeader bool

	// body, when non-nil, accumulates a copy of what was written. Only the idempotency
	// middleware turns this on, and only for the routes that store a snapshot: buffering every
	// response would double the memory cost of the whole surface for the benefit of a handful
	// of endpoints.
	body *capBuffer
}

// NewRecorder wraps w. Buffering is off; call [ResponseRecorder.Buffer] to turn it on.
func NewRecorder(w http.ResponseWriter) *ResponseRecorder {
	return &ResponseRecorder{w: w, status: http.StatusOK}
}

// Buffer starts capturing the response body, capped at maxBytes.
//
// The cap is not a nicety. Without it a handler that streams a large export would be copied
// into memory in full purely so an idempotency record could store it, and one such request per
// worker is an out-of-memory kill. Past the cap the buffer records the overflow and the
// idempotency middleware declines to store a snapshot, which degrades a replay into a genuine
// re-execution — correct, just slower — rather than into an eviction.
func (r *ResponseRecorder) Buffer(maxBytes int) {
	r.body = &capBuffer{max: maxBytes}
}

// Header returns the underlying header map.
func (r *ResponseRecorder) Header() http.Header { return r.w.Header() }

// WriteHeader records the status and forwards it exactly once.
func (r *ResponseRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.w.WriteHeader(code)
}

// Write forwards the bytes, counting them and copying them into the buffer when one is armed.
func (r *ResponseRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.w.Write(p)
	r.bytes += n
	if r.body != nil {
		r.body.Write(p[:n])
	}
	return n, err
}

// Status returns the response status, 200 when the handler wrote a body without a status.
func (r *ResponseRecorder) Status() int { return r.status }

// Bytes returns the number of body bytes written.
func (r *ResponseRecorder) Bytes() int { return r.bytes }

// Wrote reports whether anything reached the client yet. The idempotency middleware needs this
// to decide whether a late failure can still be turned into a problem document.
func (r *ResponseRecorder) Wrote() bool { return r.wroteHeader }

// Body returns the captured bytes and whether the capture is complete. A false second return
// means the response outgrew the cap and the snapshot must not be stored.
func (r *ResponseRecorder) Body() ([]byte, bool) {
	if r.body == nil {
		return nil, false
	}
	return r.body.buf, !r.body.overflowed
}

// Flush forwards to the underlying writer when it supports flushing, and is a no-op otherwise.
//
// A no-op rather than a panic: a handler that flushes is asking for a latency property, not
// asserting a capability, and failing the request because the test's httptest recorder does
// not flush would be a self-inflicted outage in the test suite.
func (r *ResponseRecorder) Flush() {
	if f, ok := r.w.(http.Flusher); ok {
		f.Flush()
	}
}

// errNotHijackable is returned when the underlying writer cannot be hijacked. It is a real
// error rather than a panic because the caller — an upgrade handler — can and should report it
// to the client.
var errNotHijackable = errors.New("httpapi: the underlying ResponseWriter does not support hijacking")

// Hijack forwards to the underlying writer when it supports hijacking.
func (r *ResponseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.w.(http.Hijacker)
	if !ok {
		return nil, nil, errNotHijackable
	}
	return h.Hijack()
}

// Unwrap exposes the wrapped writer, which is what http.ResponseController uses to find
// capabilities through a chain of wrappers in Go 1.20+.
func (r *ResponseRecorder) Unwrap() http.ResponseWriter { return r.w }

// capBuffer is an append-only byte buffer that stops at a ceiling and remembers that it did.
type capBuffer struct {
	buf        []byte
	max        int
	overflowed bool
}

func (b *capBuffer) Write(p []byte) {
	if b.overflowed {
		return
	}
	if len(b.buf)+len(p) > b.max {
		b.overflowed = true
		b.buf = nil
		return
	}
	b.buf = append(b.buf, p...)
}
