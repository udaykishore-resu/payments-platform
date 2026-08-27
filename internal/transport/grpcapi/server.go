package grpcapi

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authz"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Keepalive parameters, and the reasoning for each.
//
// These are the settings that decide whether a long-lived mesh connection survives an idle period
// or is silently torn down by something in the middle, and the failure they prevent is specific:
// an intermediary (a NAT, an NLB, a sidecar) drops an idle TCP connection without telling either
// end, and the next RPC on it fails with an unretryable transport error — on the money path.
const (
	// KeepaliveTime is how often the server pings an idle connection: 30 s.
	//
	// It must be shorter than the shortest idle timeout of anything between two pods. AWS NLB is
	// 350 s, Envoy defaults to 300 s, and a conservative NAT is 120 s. 30 s clears all three
	// with room, at a cost of one small frame per connection per half-minute.
	KeepaliveTime = 30 * time.Second

	// KeepaliveTimeout is how long a ping may go unanswered before the connection is closed: 10 s.
	// It has to exceed a bad-day round trip inside the mesh (single-digit milliseconds) by a wide
	// margin, because closing a healthy connection under momentary CPU starvation would turn a
	// GC pause into a burst of connection errors.
	KeepaliveTimeout = 10 * time.Second

	// MaxConnectionIdle closes a connection with no active RPCs after 15 minutes, so a pool that
	// has scaled down releases its file descriptors instead of holding them until the process
	// restarts.
	MaxConnectionIdle = 15 * time.Minute

	// MaxConnectionAge bounds a connection's lifetime at 30 minutes.
	//
	// This one is load balancing, not hygiene. gRPC multiplexes onto long-lived connections, so
	// without an age bound a client that connected before a scale-out keeps talking to the
	// original pods forever and the new ones stay idle. Recycling connections is what makes the
	// client's resolver re-pick, and it is the difference between an HPA that works and an HPA
	// that adds pods nothing routes to.
	MaxConnectionAge = 30 * time.Minute

	// MaxConnectionAgeGrace lets in-flight RPCs finish after the age limit fires: 30 s. Without
	// it, connection recycling would abort whatever was running, which on this surface means
	// aborting a gateway call and manufacturing a TIMEOUT_UNKNOWN to reconcile.
	MaxConnectionAgeGrace = 30 * time.Second

	// MinClientPingInterval is the floor on how often a client may ping: 10 s. A client pinging
	// faster than this is either misconfigured or hostile, and gRPC's enforcement policy closes
	// it — which is a defence against a cheap denial-of-service that costs the attacker nothing.
	MinClientPingInterval = 10 * time.Second

	// DefaultMaxRecvMsgBytes is 4 MiB, gRPC's own default, stated here so it is a decision rather
	// than an inheritance. It bounds what one RPC can make the server allocate.
	DefaultMaxRecvMsgBytes = 4 << 20

	// DefaultShutdownTimeout bounds GracefulStop before the fallback to Stop.
	DefaultShutdownTimeout = 20 * time.Second
)

// Authenticator resolves a caller's identity from an RPC's peer and metadata.
//
// It is an interface rather than the concrete mTLS authenticator because a process may accept
// both workload identities inside the mesh and bearer tokens from a gateway that bridges REST to
// gRPC, and the composition root chooses.
type Authenticator interface {
	Authenticate(ctx context.Context, method string) (*authn.Principal, error)
}

// MetricsSink records the RED series for an RPC. *telemetry.Registry satisfies it.
type MetricsSink interface {
	ObserveHTTPRequest(ctx context.Context, service, route, method string, status int,
		tier telemetry.TenantTier, d time.Duration)
}

// RateLimiter is the per-tenant token bucket. resilience.Limiter satisfies it.
type RateLimiter interface {
	Allow(ctx context.Context, key string, limit resilience.Limit) (resilience.Decision, error)
}

// Authorizer evaluates RBAC and ABAC. *authz.Policy satisfies it.
type Authorizer interface {
	Evaluate(ctx context.Context, req authz.Request) authz.Decision
}

// Config configures a [Server]. Every security-relevant field fails closed when absent.
type Config struct {
	// Addr is the listen address, host:port.
	Addr string
	// Service names this binary in logs and metrics.
	Service string

	// TLS carries the server certificate and the client CA. Inside the mesh the sidecar
	// terminates and this is nil; at a boundary that terminates its own mTLS it is required, and
	// it is what makes [Authenticator] able to see a peer certificate at all.
	TLS *tls.Config

	// Authenticator, Authorizer, RateLimiter are the §12 controls. A nil Authenticator rejects
	// every RPC on a non-public method; a nil Authorizer denies every RPC.
	Authenticator Authenticator
	Authorizer    Authorizer
	RateLimiter   RateLimiter

	// Permissions maps a fully-qualified method — "/payments.v1.PaymentService/CreatePayment" —
	// to the permission it requires. A method absent from the map is denied, so a newly added
	// RPC is closed until somebody decides what it needs.
	Permissions map[string]authz.Permission

	// PublicMethods bypass authentication: the health service and reflection. It is an explicit
	// set for the same reason the HTTP chain's is — a prefix rule is one careless registration
	// away from exposing a service.
	PublicMethods map[string]bool

	// TenantLimit is the per-tenant rate limit applied to every RPC. A zero value disables
	// limiting, which is correct only for a mesh-internal server behind another limiter.
	TenantLimit resilience.Limit

	// EnableReflection turns on server reflection. It is off by default and behind a flag
	// because reflection publishes the full service and message schema to any caller that can
	// open a connection — which is exactly what an operator wants with grpcurl in staging and
	// exactly what an attacker wants in production.
	EnableReflection bool

	// MaxRecvMsgBytes bounds one received message. Zero means DefaultMaxRecvMsgBytes.
	MaxRecvMsgBytes int

	// ShutdownTimeout bounds the drain. Zero means DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// Metrics and Logger are the observability sinks. Both may be nil in a test.
	Metrics MetricsSink
	Logger  *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Service == "" {
		c.Service = "grpc"
	}
	if c.MaxRecvMsgBytes <= 0 {
		c.MaxRecvMsgBytes = DefaultMaxRecvMsgBytes
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.PublicMethods == nil {
		c.PublicMethods = DefaultPublicMethods()
	}
}

// DefaultPublicMethods is the bypass set: the health service and reflection.
//
// The health service is public because a Kubernetes gRPC probe carries no credential and cannot
// be given one. Reflection is public *when enabled at all*, which is the point of gating it
// behind a flag rather than behind authentication: an authenticated reflection endpoint in
// production still publishes the schema to anyone who obtains any credential.
func DefaultPublicMethods() map[string]bool {
	return map[string]bool{
		healthpb.Health_Check_FullMethodName:                             true,
		healthpb.Health_Watch_FullMethodName:                             true,
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo":      true,
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo": true,
	}
}

// Server is the platform's gRPC server with its interceptor chain and a bounded drain.
//
// It owns its listener so [Server.Start] can report a bind failure synchronously. A server that
// binds inside its own goroutine reports "address already in use" through a channel the
// composition root has to remember to read, and the failure mode of forgetting is a process that
// reports healthy and serves nothing.
type Server struct {
	cfg    Config
	srv    *grpc.Server
	health *health.Server
	ln     net.Listener
	done   chan struct{}
	err    error
}

// NewServer builds the server, registers the health service, and installs the interceptor chain.
// It does not bind; see Start.
//
// Registering services is the caller's job and must happen before Start: grpc-go panics on a
// registration after serving has begun, deliberately, because a service that appears mid-flight
// is a service some clients saw and others did not.
func NewServer(cfg Config) *Server {
	cfg.applyDefaults()

	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(cfg.MaxRecvMsgBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:                  KeepaliveTime,
			Timeout:               KeepaliveTimeout,
			MaxConnectionIdle:     MaxConnectionIdle,
			MaxConnectionAge:      MaxConnectionAge,
			MaxConnectionAgeGrace: MaxConnectionAgeGrace,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime: MinClientPingInterval,
			// PermitWithoutStream is true because this platform's clients hold idle connections
			// between bursts and need them kept alive; refusing pings on a stream-less connection
			// would let the idle connection die and reintroduce the failure keepalive exists to
			// prevent.
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(ChainUnaryInterceptors(cfg)...),
		grpc.ChainStreamInterceptor(RecoveryStreamInterceptor(cfg.Logger)),
	}
	if cfg.TLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(cfg.TLS)))
	}

	srv := grpc.NewServer(opts...)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	if cfg.EnableReflection {
		reflection.Register(srv)
	}
	return &Server{cfg: cfg, srv: srv, health: hs, done: make(chan struct{})}
}

// Grpc exposes the underlying server so the composition root can register services onto it.
//
// It is deliberately not a Register* method per service: this package must not import the
// generated bindings (see the package comment), so registration happens in the composition root
// where the generated types are already in scope.
func (s *Server) Grpc() *grpc.Server { return s.srv }

// Health exposes the health service so readiness can be reflected to gRPC clients.
//
// It is a separate signal from the HTTP /readyz probe and both are needed: Kubernetes reads the
// HTTP probe, and a gRPC client's own load balancer reads the health service. A pod that fails
// one and not the other sheds traffic from one direction only.
func (s *Server) Health() *health.Server { return s.health }

// SetServingStatus marks a service serving or not serving. The empty service name is the
// server-wide status that most clients watch.
func (s *Server) SetServingStatus(service string, serving bool) {
	st := healthpb.HealthCheckResponse_NOT_SERVING
	if serving {
		st = healthpb.HealthCheckResponse_SERVING
	}
	s.health.SetServingStatus(service, st)
}

// Start binds the listener and begins serving in a goroutine it owns.
func (s *Server) Start() error {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", s.cfg.Addr)
	if err != nil {
		return apierror.Wrapf(err, apierror.CodeInternalError,
			"grpc server could not bind %s", s.cfg.Addr)
	}
	s.ln = ln
	s.cfg.Logger.Info("grpc server listening",
		slog.String("service", s.cfg.Service),
		slog.String("addr", ln.Addr().String()),
		slog.Bool("reflection", s.cfg.EnableReflection),
		slog.Bool("tls", s.cfg.TLS != nil),
	)
	go func() {
		defer close(s.done)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.err = err
			s.cfg.Logger.Error("grpc server stopped unexpectedly",
				slog.String(telemetry.KeyErrorMessage, err.Error()))
		}
	}()
	return nil
}

// Addr returns the bound address, meaningful only after Start. Tests bind :0 and read it back.
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.cfg.Addr
	}
	return s.ln.Addr().String()
}

// Shutdown drains in-flight RPCs under a bounded deadline, falling back to a hard stop.
//
// # Why the fallback is not optional
//
// GracefulStop blocks until every in-flight RPC returns, and an RPC blocked on a wedged
// downstream returns never. Without a bound, the drain outlives the pod's termination grace
// period, Kubernetes SIGKILLs the process, and the outcome is the worst of both: the in-flight
// RPCs die uncleanly *and* the drain consumed the whole grace period first. The bound converts
// that into a clean stop at a time we chose.
//
// Health is set NOT_SERVING first, before anything closes, so a client's load balancer stops
// picking this backend while it can still finish what it has. That ordering is the gRPC analogue
// of failing /readyz before closing the HTTP listener, and it exists for the same reason:
// endpoint propagation is asynchronous, so the announcement has to come first.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.ln == nil {
		return nil
	}
	s.health.Shutdown()

	ctx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		s.srv.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-ctx.Done():
		s.cfg.Logger.Warn("grpc drain did not complete within its budget; stopping hard",
			slog.Duration("budget", s.cfg.ShutdownTimeout))
		s.srv.Stop()
		<-stopped
	}
	<-s.done
	return s.err
}

// ChainUnaryInterceptors returns the interceptor chain in order, outermost first.
//
// The order mirrors the HTTP middleware chain of baseline §12, and the mirroring is the point: two
// surfaces with different pipelines is two sets of controls to audit, and one of them will be the
// one somebody forgot to update.
//
//	recovery   → a panic must not take the process down, and must not reach the client.
//	tracing    → the span encloses authentication, so "the Unauthenticated RPCs are slow" is
//	             answerable.
//	logging    → observes the final status, including the one a later interceptor produced.
//	metrics    → separate from logging because logs are sampled and metrics are not.
//	authn      → §12 stage 3, from the mTLS peer identity.
//	tenant     → §12 stage 4, strictly after authn: the tenant comes from the identity.
//	authz      → §12 stage 5, strictly after tenant: ABAC compares resource tenant to principal.
//	ratelimit  → §12 stage 6, last, so a rejected RPC has already been attributed to a tenant.
//
// Idempotency has no interceptor. On this surface it is the service implementation's concern,
// because the key arrives in a request message field rather than in a header and the interceptor
// would have to know every message type to find it — which is the resource knowledge the layering
// exists to keep out of the transport.
func ChainUnaryInterceptors(cfg Config) []grpc.UnaryServerInterceptor {
	cfg.applyDefaults()
	return []grpc.UnaryServerInterceptor{
		RecoveryUnaryInterceptor(cfg.Logger),
		TracingUnaryInterceptor(cfg.Service),
		LoggingUnaryInterceptor(),
		MetricsUnaryInterceptor(cfg.Metrics, cfg.Service),
		AuthnUnaryInterceptor(cfg.Authenticator, cfg.PublicMethods),
		TenantUnaryInterceptor(cfg.PublicMethods),
		AuthzUnaryInterceptor(cfg.Authorizer, cfg.Permissions, cfg.PublicMethods),
		RateLimitUnaryInterceptor(cfg.RateLimiter, cfg.TenantLimit, cfg.PublicMethods),
	}
}

// InterceptorNames returns the chain's stage names in order, for the startup log line and for the
// test that pins the order against §12.
func InterceptorNames() []string {
	return []string{"recovery", "tracing", "logging", "metrics", "authn", "tenant", "authz", "ratelimit"}
}

// RecoveryUnaryInterceptor converts a panic into an Internal status and keeps the process alive.
//
// The stack goes to the log and never to the client, for the same reason as the HTTP chain: a Go
// stack names package paths, file names and line numbers, and occasionally a value that happened
// to be in an argument register.
func RecoveryUnaryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic in grpc handler",
					slog.String("method", info.FullMethod),
					slog.String(telemetry.KeyStack, string(debug.Stack())),
				)
				err = status.Error(codes.Internal, "the request could not be completed")
				resp = nil
			}
		}()
		return handler(ctx, req)
	}
}

// RecoveryStreamInterceptor is the streaming counterpart.
func RecoveryStreamInterceptor(logger *slog.Logger) grpc.StreamServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo,
		handler grpc.StreamHandler) (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic in grpc stream handler",
					slog.String("method", info.FullMethod),
					slog.String(telemetry.KeyStack, string(debug.Stack())),
				)
				err = status.Error(codes.Internal, "the request could not be completed")
			}
		}()
		return handler(srv, ss)
	}
}

// TracingUnaryInterceptor starts a span named with the fully-qualified method.
//
// The method name is already a bounded label — there are tens of them and they are compiled in —
// so unlike an HTTP path it carries no cardinality risk and needs no templating.
func TracingUnaryInterceptor(service string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		ctx, span := telemetry.StartSpan(ctx, info.FullMethod)
		defer span.End()

		resp, err := handler(ctx, req)
		if err != nil {
			telemetry.RecordError(span, err)
		}
		return resp, err
	}
}

// LoggingUnaryInterceptor writes one structured line per RPC.
//
// No request message and no response message. A payment creation request contains a token and a
// merchant identifier; logging the message would put both in the log pipeline, and "we log
// requests only in staging" is a configuration away from being false.
func LoggingUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)

		lg := telemetry.Logger(ctx).With(
			slog.String("method", info.FullMethod),
			slog.String(telemetry.KeyStatus, code.String()),
			slog.Int64(telemetry.KeyDurationMS, time.Since(start).Milliseconds()),
		)
		switch {
		case code == codes.Internal || code == codes.Unknown || code == codes.DataLoss:
			lg.Error("grpc request", slog.String(telemetry.KeyErrorMessage, errText(err)))
		case err != nil:
			// A business rejection is not an operational event. Logging it at INFO turns a
			// client's validation loop into a log-volume incident that hides the real errors.
			lg.Debug("grpc request")
		default:
			lg.Info("grpc request")
		}
		return resp, err
	}
}

// errText renders an error for the log, never for the client.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// MetricsUnaryInterceptor records the RED series, reusing the HTTP histogram so a dashboard can
// compare the two surfaces without joining across metric families.
func MetricsUnaryInterceptor(sink MetricsSink, service string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if sink == nil {
			return handler(ctx, req)
		}
		start := time.Now()
		resp, err := handler(ctx, req)
		sink.ObserveHTTPRequest(ctx, service, info.FullMethod, "GRPC",
			httpStatusOf(err), tierOf(ctx), time.Since(start))
		return resp, err
	}
}

// httpStatusOf projects a gRPC outcome onto an HTTP status so both surfaces share one histogram.
func httpStatusOf(err error) int {
	if err == nil {
		return 200
	}
	if e := apierror.From(err); e != nil {
		return e.HTTPStatus()
	}
	return 500
}

func tierOf(ctx context.Context) telemetry.TenantTier {
	tc, err := tenantctx.FromContext(ctx)
	if err != nil {
		return telemetry.TierPooled
	}
	if tc.Tier == shared.TierSiloed {
		return telemetry.TierSiloed
	}
	return telemetry.TierPooled
}

// AuthnUnaryInterceptor resolves the caller's identity, failing closed when unconfigured.
func AuthnUnaryInterceptor(a Authenticator, public map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if public[info.FullMethod] {
			return handler(ctx, req)
		}
		if a == nil {
			return nil, Status(apierror.New(apierror.CodeUnauthenticated,
				"this method requires authentication and no authenticator is configured"))
		}
		p, err := a.Authenticate(ctx, info.FullMethod)
		if err != nil {
			return nil, Status(err)
		}
		if p == nil {
			return nil, Status(apierror.New(apierror.CodeUnauthenticated,
				"the call carried no usable credential"))
		}
		return handler(WithPrincipal(ctx, p), req)
	}
}

// TenantUnaryInterceptor derives the tenant context from the authenticated principal.
//
// The tenant comes from the verified identity, never from a request field. A request that carries
// a tenant a caller controls is a request that can name a tenant it does not own, and the failure
// is silent: the query runs against the attacker's chosen row-level-security scope.
func TenantUnaryInterceptor(public map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if public[info.FullMethod] {
			return handler(ctx, req)
		}
		p := PrincipalFrom(ctx)
		if p == nil {
			return nil, Status(apierror.New(apierror.CodeMissingTenantContext,
				"no authenticated principal from which to derive a tenant"))
		}
		tc, err := p.TenantContext(requestIDFrom(ctx), correlationIDFrom(ctx))
		if err != nil {
			return nil, Status(err)
		}
		ctx, err = tenantctx.WithTenant(ctx, tc)
		if err != nil {
			return nil, Status(err)
		}
		return handler(ctx, req)
	}
}

// AuthzUnaryInterceptor evaluates RBAC and ABAC from a method-to-permission table.
//
// A method absent from the table is denied. Deriving the permission from a table rather than from
// the service implementation means a new RPC with no entry is closed until somebody decides what
// it needs — rather than open until somebody notices.
func AuthzUnaryInterceptor(a Authorizer, perms map[string]authz.Permission,
	public map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if public[info.FullMethod] {
			return handler(ctx, req)
		}
		perm, ok := perms[info.FullMethod]
		if !ok {
			return nil, Status(apierror.Newf(apierror.CodeForbidden,
				"no authorization policy is defined for %s", info.FullMethod))
		}
		if a == nil {
			return nil, Status(apierror.New(apierror.CodeForbidden,
				"no authorization policy engine is configured"))
		}
		tc, _ := tenantctx.FromContext(ctx)
		d := a.Evaluate(ctx, authz.Request{
			Principal:      PrincipalFrom(ctx),
			Permission:     perm,
			Operation:      info.FullMethod,
			Resource:       authz.Resource{TenantID: tc.TenantID},
			PeerThumbprint: authn.PeerThumbprint(ctx),
		})
		if d.Denied() {
			return nil, Status(d.Error())
		}
		return handler(ctx, req)
	}
}

// RateLimitUnaryInterceptor applies a per-tenant token bucket and publishes the budget as
// response metadata.
//
// A limiter failure admits the RPC, the same deliberate fail-open as the HTTP chain: the failure
// of the limiter is not evidence that capacity is exhausted, and failing closed would convert a
// Redis blip into a total outage of an internal API.
func RateLimitUnaryInterceptor(l RateLimiter, limit resilience.Limit,
	public map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if l == nil || public[info.FullMethod] || limit.Rate <= 0 {
			return handler(ctx, req)
		}
		tc, err := tenantctx.FromContext(ctx)
		if err != nil {
			return handler(ctx, req)
		}
		key := "grpc:" + tc.TenantID.String() + ":" + info.FullMethod
		d, err := l.Allow(ctx, key, limit)
		if err != nil {
			return handler(ctx, req)
		}
		// The budget travels as header metadata under the same names the REST surface uses, so a
		// client library can read one set of names on both transports.
		md := metadata.New(d.Headers())
		_ = grpc.SetHeader(ctx, md)
		if !d.Allowed {
			return nil, Status(d.Err())
		}
		return handler(ctx, req)
	}
}

// Status converts a platform error into a gRPC status.
//
// # The mapping lives in apierror, not here
//
// [apierror.Error.GRPCCode] returns the numeric code, and this function is the only place that
// turns it into a status. Keeping the *decision* in the error package and the *conversion* here
// is what lets pkg/apierror stay stdlib-only — it cannot import grpc-go without becoming
// unusable from the domain — while still owning the classification. The alternative, a switch on
// category in this file, would be a second copy of a table that already exists and would drift
// from it.
//
// The message is the caller-safe one. apierror.Error.Message is the field written for a caller;
// the wrapped cause reaches the log through the logging interceptor and never the wire.
func Status(err error) error {
	if err == nil {
		return nil
	}
	e := apierror.From(err)
	if e == nil {
		return status.Error(codes.Internal, "the request could not be completed")
	}
	return status.Error(codes.Code(e.GRPCCode()), e.Message)
}

// StatusCodeOf reports the gRPC code a platform error maps to, for tests and for the metrics
// label, without allocating a status.
func StatusCodeOf(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	if e := apierror.From(err); e != nil {
		return codes.Code(e.GRPCCode())
	}
	return codes.Internal
}

// --- context carriers ------------------------------------------------------------------------

type ctxKey int

const ctxKeyPrincipal ctxKey = iota

// WithPrincipal stores the authenticated identity for the duration of an RPC.
func WithPrincipal(ctx context.Context, p *authn.Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

// PrincipalFrom returns the authenticated identity, or nil before authentication has run.
func PrincipalFrom(ctx context.Context) *authn.Principal {
	p, _ := ctx.Value(ctxKeyPrincipal).(*authn.Principal)
	return p
}

// MetadataRequestIDKey and MetadataCorrelationIDKey are the metadata keys carrying the same
// correlation identifiers the REST surface carries in headers. gRPC lowercases metadata keys, so
// these are spelled lowercase to match what a server actually receives.
const (
	MetadataRequestIDKey     = "x-request-id"
	MetadataCorrelationIDKey = "x-correlation-id"
)

func requestIDFrom(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(MetadataRequestIDKey); len(v) > 0 {
		return v[0]
	}
	return ""
}

func correlationIDFrom(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return requestIDFrom(ctx)
	}
	if v := md.Get(MetadataCorrelationIDKey); len(v) > 0 {
		return v[0]
	}
	return requestIDFrom(ctx)
}

// PeerAuthenticator adapts internal/platform/authn's mTLS peer authenticator to [Authenticator].
//
// It reads the verified chain from the connection's TLS state and derives a SPIFFE workload
// identity. There is no fallback to an unverified certificate and none to a header: a workload
// identity asserted in metadata is a workload identity any caller can assert.
type PeerAuthenticator struct {
	Peers *authn.PeerAuthenticator
}

// Authenticate resolves the caller from its peer certificate.
func (a PeerAuthenticator) Authenticate(ctx context.Context, _ string) (*authn.Principal, error) {
	if a.Peers == nil {
		return nil, apierror.New(apierror.CodeUnauthenticated, "no peer authenticator is configured")
	}
	pr, ok := peer.FromContext(ctx)
	if !ok {
		return nil, apierror.New(apierror.CodeUnauthenticated, "the call carried no peer information")
	}
	tlsInfo, ok := pr.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, apierror.New(apierror.CodeUnauthenticated,
			"the call was not made over mTLS; this server accepts workload identities only")
	}
	return a.Peers.FromConnectionState(&tlsInfo.State)
}

// MethodService extracts the service name from a fully-qualified method, for a metric label that
// groups by service rather than by RPC.
func MethodService(fullMethod string) string {
	trimmed := strings.TrimPrefix(fullMethod, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}
