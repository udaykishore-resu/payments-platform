package httpapi

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// The server's timeout defaults. Every one of them is a decision, and a zero value for any of
// them is a slow-loris vulnerability rather than an omission — net/http's zero means "no
// deadline", so an unset timeout is an unbounded one.
const (
	// DefaultReadHeaderTimeout is 5 s: the time a client has to finish sending its request
	// line and headers. This is the single most important timeout on the server, because it is
	// the one that bounds a connection which has been opened and is sending one byte per
	// second. Five seconds is generous for a TLS-terminated request from anywhere on earth and
	// ungenerous for an attacker holding ten thousand sockets open.
	DefaultReadHeaderTimeout = 5 * time.Second

	// DefaultReadTimeout is 30 s: headers plus body. It has to exceed ReadHeaderTimeout by
	// enough to upload the largest legitimate body (256 KiB) over a poor mobile link, and it
	// has to stay well under the load balancer's own idle timeout so that *we* are the party
	// that closes a stalled upload and the failure is attributable to us.
	DefaultReadTimeout = 30 * time.Second

	// DefaultWriteTimeout is 35 s: it must be larger than the longest handler, which on this
	// surface is a create-payment with one gateway call (8 s) plus a failover attempt plus the
	// §12 pre- and post-flight budget — comfortably under 20 s at p99.9. The margin over
	// ReadTimeout matters because the write deadline is set relative to the end of the *header*
	// read, so a slow body upload eats into the handler's write budget.
	DefaultWriteTimeout = 35 * time.Second

	// DefaultIdleTimeout is 120 s: how long a keep-alive connection is kept without a request.
	// It must exceed the client SDK's own keep-alive so that connection reuse actually happens
	// — a server that closes first produces a race where the client writes a request onto a
	// socket the server is closing, which surfaces as an unretryable "unexpected EOF" on a
	// money-moving POST. Two minutes is the AWS ALB default plus headroom.
	DefaultIdleTimeout = 120 * time.Second

	// DefaultMaxHeaderBytes is 64 KiB. The largest legitimate header set here is an access
	// token plus tracing headers plus a handful of correlation ids — a few kilobytes. 64 KiB
	// leaves room for a proxy that appends its own and still bounds the per-connection
	// allocation an attacker can force.
	DefaultMaxHeaderBytes = 64 << 10

	// DefaultShutdownTimeout bounds the drain. It is a *bound*, not a target: Shutdown returns
	// as soon as the last in-flight request completes.
	DefaultShutdownTimeout = 15 * time.Second
)

// ServerConfig configures a Server. Every field has a documented default; a zero value produces
// a server that is safe to expose.
type ServerConfig struct {
	// Addr is the listen address, host:port.
	Addr string
	// Name identifies the server in logs — "public", "admin". A process runs more than one.
	Name string

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	ShutdownTimeout   time.Duration

	// TLS, when non-nil, makes this an HTTPS listener. In the mesh deployment TLS is terminated
	// by the sidecar and this stays nil; it is here for the edge deployment and for the
	// integration tests that exercise mTLS peer identity.
	TLS *tls.Config

	// DisableHTTP2 turns off h2. It exists because h2 multiplexes many streams onto one
	// connection, which defeats connection-level load balancing: an ALB that balances
	// connections will pin a chatty client to one pod. Off is occasionally the right answer at
	// the edge; on is the right answer inside the mesh.
	DisableHTTP2 bool

	// Logger is used for lifecycle lines only. Request logging is the middleware's job.
	Logger *slog.Logger
}

func (c *ServerConfig) applyDefaults() {
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = DefaultReadTimeout
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.MaxHeaderBytes <= 0 {
		c.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.Name == "" {
		c.Name = "http"
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Server is an http.Server with this platform's timeouts and a bounded drain.
//
// It owns its listener so that Start can report a bind failure synchronously. A server that
// binds inside its own goroutine reports "address already in use" through a channel that the
// composition root has to remember to read, and the failure mode of forgetting is a process
// that reports healthy and serves nothing.
type Server struct {
	cfg  ServerConfig
	srv  *http.Server
	ln   net.Listener
	done chan struct{}
	err  error
}

// NewServer builds the server. It does not bind; see Start.
func NewServer(cfg ServerConfig, h http.Handler) *Server {
	cfg.applyDefaults()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		TLSConfig:         cfg.TLS,
		// BaseContext is not set: the request context must be cancelled by the request's own
		// deadline and by the client disconnecting, never by process shutdown. Wiring the root
		// context in here would abort in-flight gateway calls on SIGTERM, and an aborted
		// gateway call is a TIMEOUT_UNKNOWN we then have to reconcile — see docs/lld.md §2.5,
		// "never abort a gateway call at shutdown".
		ErrorLog: slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelWarn),
	}
	if !cfg.DisableHTTP2 {
		// h2c is deliberately *not* enabled: cleartext HTTP/2 upgrade on a public listener has
		// a long history of request-smuggling differentials between the upgrade path and the
		// proxy in front of it. Inside the mesh the sidecar terminates TLS and h2 is negotiated
		// over ALPN, which is the configured path here.
		_ = http2.ConfigureServer(srv, &http2.Server{
			// IdleTimeout on the h2 server bounds a connection with no open streams. Without
			// it, h2's own idle handling is independent of http.Server.IdleTimeout and a
			// connection can outlive the setting an operator thought they had changed.
			IdleTimeout: cfg.IdleTimeout,
			// MaxConcurrentStreams bounds per-connection multiplexing. Unbounded, one client's
			// single connection can occupy every worker in the process, which is a
			// denial-of-service with no packets to rate limit.
			MaxConcurrentStreams: 250,
		})
	}
	return &Server{cfg: cfg, srv: srv, done: make(chan struct{})}
}

// Start binds the listener and begins serving in a goroutine it owns.
//
// The bind is synchronous and its error is returned, so a composition root can fail fast with
// an actionable message. The serve loop is the goroutine; Shutdown is what joins it.
func (s *Server) Start() error {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", s.cfg.Addr)
	if err != nil {
		return apierror.Wrapf(err, apierror.CodeInternalError,
			"%s server could not bind %s", s.cfg.Name, s.cfg.Addr)
	}
	s.ln = ln
	s.cfg.Logger.Info("http server listening",
		slog.String("server", s.cfg.Name),
		slog.String("addr", ln.Addr().String()),
	)
	go func() {
		defer close(s.done)
		var serveErr error
		if s.cfg.TLS != nil {
			serveErr = s.srv.ServeTLS(ln, "", "")
		} else {
			serveErr = s.srv.Serve(ln)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			s.err = serveErr
			s.cfg.Logger.Error("http server stopped unexpectedly",
				slog.String("server", s.cfg.Name),
				slog.String(telemetry.KeyErrorMessage, serveErr.Error()),
			)
		}
	}()
	return nil
}

// Addr returns the bound address, which is only meaningful after Start. Tests bind :0 and read
// it back.
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.cfg.Addr
	}
	return s.ln.Addr().String()
}

// Shutdown stops accepting connections and drains in-flight requests under a bounded deadline.
//
// The deadline is this server's own ShutdownTimeout intersected with the caller's context,
// whichever is sooner, and the fallback on expiry is Close — not a hang. A drain that can block
// forever is a pod that Kubernetes eventually SIGKILLs, which is the outcome the graceful
// shutdown existed to avoid: in-flight requests die uncleanly *and* the drain took the full
// grace period first.
//
// It is safe to call on a server that was never started and safe to call twice.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.ln == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()

	err := s.srv.Shutdown(ctx)
	if err != nil {
		s.cfg.Logger.Warn("http drain did not complete within its budget; closing",
			slog.String("server", s.cfg.Name),
			slog.String(telemetry.KeyErrorMessage, err.Error()),
		)
		_ = s.srv.Close()
	}
	<-s.done
	if s.err != nil {
		return s.err
	}
	return err
}
