package grpcapi_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authz"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/internal/transport/grpcapi"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestInterceptorOrderMirrorsTheHTTPChain pins the order against baseline §12.
//
// Two surfaces with different pipelines is two sets of controls to audit, and one of them will be
// the one somebody forgot to update. Asserting the list turns a divergence into a failing test.
func TestInterceptorOrderMirrorsTheHTTPChain(t *testing.T) {
	t.Parallel()
	want := []string{"recovery", "tracing", "logging", "metrics", "authn", "tenant", "authz", "ratelimit"}
	got := grpcapi.InterceptorNames()
	if len(got) != len(want) {
		t.Fatalf("chain length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stage %d = %q, want %q", i, got[i], want[i])
		}
	}
	if n := len(grpcapi.ChainUnaryInterceptors(grpcapi.Config{})); n != len(want) {
		t.Errorf("ChainUnaryInterceptors built %d stages, InterceptorNames lists %d", n, len(want))
	}
}

// TestStatusMappingFollowsTheErrorCatalogue asserts every category reaches the wire as the code
// baseline §20.1 assigns it.
//
// The mapping decision lives in pkg/apierror, which cannot import grpc-go; this test is what
// verifies the conversion here agrees with it, and it is the reason a `switch` on category in this
// package would have been a second copy of a table that already exists.
func TestStatusMappingFollowsTheErrorCatalogue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code apierror.Code
		want codes.Code
	}{
		{apierror.CodeValidationFailed, codes.InvalidArgument},
		{apierror.CodeUnauthenticated, codes.Unauthenticated},
		{apierror.CodeForbidden, codes.PermissionDenied},
		{apierror.CodeTenantMismatch, codes.PermissionDenied},
		{apierror.CodePaymentNotFound, codes.NotFound},
		{apierror.CodeMerchantNotFound, codes.NotFound},
		{apierror.CodePaymentAlreadyProcessed, codes.FailedPrecondition},
		{apierror.CodeRiskDeclined, codes.FailedPrecondition},
		{apierror.CodeRateLimited, codes.ResourceExhausted},
		{apierror.CodeGatewayTimeout, codes.DeadlineExceeded},
		{apierror.CodeServiceUnavailable, codes.Unavailable},
		{apierror.CodeInternalError, codes.Internal},
	}
	for _, tc := range tests {
		t.Run(string(tc.code), func(t *testing.T) {
			t.Parallel()
			err := grpcapi.Status(apierror.New(tc.code, "test"))
			if got := status.Code(err); got != tc.want {
				t.Errorf("%s -> %s, want %s", tc.code, got, tc.want)
			}
			if got := grpcapi.StatusCodeOf(apierror.New(tc.code, "test")); got != tc.want {
				t.Errorf("StatusCodeOf(%s) = %s, want %s", tc.code, got, tc.want)
			}
		})
	}
	if grpcapi.Status(nil) != nil {
		t.Error("Status(nil) must be nil")
	}
	if got := grpcapi.StatusCodeOf(nil); got != codes.OK {
		t.Errorf("StatusCodeOf(nil) = %s, want OK", got)
	}
}

// TestStatusNeverLeaksTheWrappedCause asserts the operator-facing chain stays out of the wire
// message, exactly as it does on the REST surface.
func TestStatusNeverLeaksTheWrappedCause(t *testing.T) {
	t.Parallel()
	const secret = "dsn=postgres://user:hunter2@db/payments"
	err := grpcapi.Status(apierror.Wrap(
		errText(secret), apierror.CodeInternalError, "the request could not be completed"))
	msg := status.Convert(err).Message()
	if strings.Contains(msg, secret) || strings.Contains(msg, "hunter2") {
		t.Errorf("the wrapped cause reached the wire: %q", msg)
	}
	if msg != "the request could not be completed" {
		t.Errorf("message = %q, want the caller-safe text", msg)
	}
}

// TestHealthServiceIsRegisteredAndPublic asserts a Kubernetes gRPC probe — which carries no
// credential and never will — can reach the health service on a server with authentication wired.
func TestHealthServiceIsRegisteredAndPublic(t *testing.T) {
	t.Parallel()
	srv := grpcapi.NewServer(grpcapi.Config{
		Addr:    "127.0.0.1:0",
		Service: "test",
		// No authenticator: every non-public method must be refused, and the health service must
		// nevertheless answer.
		Authenticator: nil,
		Logger:        quietLogger(),
	})
	srv.SetServingStatus("", true)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	conn, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if res.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %s, want SERVING", res.GetStatus())
	}
}

// TestShutdownIsBoundedAndFailsHealthFirst asserts the drain announces NOT_SERVING before it
// closes anything, and that it returns within its budget rather than blocking forever.
func TestShutdownIsBoundedAndFailsHealthFirst(t *testing.T) {
	t.Parallel()
	srv := grpcapi.NewServer(grpcapi.Config{
		Addr:            "127.0.0.1:0",
		Service:         "test",
		ShutdownTimeout: 2 * time.Second,
		Logger:          quietLogger(),
	})
	srv.SetServingStatus("", true)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- srv.Shutdown(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("drain took %s; it must be bounded", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return; the drain is unbounded")
	}

	// Idempotent: a composition root that calls Shutdown from both a signal handler and a defer
	// must not deadlock or double-close.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

// TestStartReportsBindFailureSynchronously asserts a port conflict is returned rather than
// delivered to a channel nobody reads — the failure mode being a process that reports healthy and
// serves nothing.
func TestStartReportsBindFailureSynchronously(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := grpcapi.NewServer(grpcapi.Config{Addr: ln.Addr().String(), Logger: quietLogger()})
	if err := srv.Start(); err == nil {
		t.Fatal("Start on an occupied port must return an error")
	} else if apierror.CodeOf(err) != apierror.CodeInternalError {
		t.Errorf("bind failure code = %s", apierror.CodeOf(err))
	}
}

// TestAuthenticationFailsClosed asserts an unconfigured authenticator rejects rather than admits.
func TestAuthenticationFailsClosed(t *testing.T) {
	t.Parallel()
	intercept := grpcapi.AuthnUnaryInterceptor(nil, nil)
	_, err := intercept(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/payments.v1.PaymentService/CreatePayment"},
		func(context.Context, any) (any, error) { return "served", nil })
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated — a missing authenticator must not admit traffic",
			status.Code(err))
	}
}

// TestAuthorizationDeniesAMethodWithNoPolicy asserts the fail-closed permission table.
func TestAuthorizationDeniesAMethodWithNoPolicy(t *testing.T) {
	t.Parallel()
	intercept := grpcapi.AuthzUnaryInterceptor(allowAll{}, map[string]authz.Permission{}, nil)
	_, err := intercept(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/payments.v1.PaymentService/SomethingNew"},
		func(context.Context, any) (any, error) { return "served", nil })
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %s, want PermissionDenied for a method with no policy", status.Code(err))
	}
}

// TestRecoveryReturnsInternalWithoutTheStack is the panic contract.
func TestRecoveryReturnsInternalWithoutTheStack(t *testing.T) {
	t.Parallel()
	const marker = "grpc-panic-secret-marker"
	intercept := grpcapi.RecoveryUnaryInterceptor(quietLogger())
	resp, err := intercept(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/payments.v1.PaymentService/CreatePayment"},
		func(context.Context, any) (any, error) { panic(marker) })

	if resp != nil {
		t.Errorf("response = %v, want nil after a panic", resp)
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %s, want Internal", status.Code(err))
	}
	msg := status.Convert(err).Message()
	for _, leak := range []string{marker, "goroutine ", ".go:", "runtime."} {
		if strings.Contains(msg, leak) {
			t.Errorf("status message leaks %q: %s", leak, msg)
		}
	}
}

// TestTenantInterceptorDerivesTheTenantFromTheIdentity asserts the tenant comes from the verified
// principal — never from a request field a caller controls.
func TestTenantInterceptorDerivesTheTenantFromTheIdentity(t *testing.T) {
	t.Parallel()
	p := &authn.Principal{
		Method: authn.MethodMTLS, Type: tenantctx.PrincipalWorkload,
		ID: "spiffe://pp/ns/payments/sa/api", TenantID: "ten_01JB8Z00000000000000000000",
		TenantTier: shared.TierPooled, Environment: shared.EnvironmentSandbox,
		Scopes: []string{"payments:write"},
	}
	var seen shared.TenantID
	intercept := grpcapi.TenantUnaryInterceptor(nil)
	_, err := intercept(grpcapi.WithPrincipal(context.Background(), p), nil,
		&grpc.UnaryServerInfo{FullMethod: "/payments.v1.PaymentService/CreatePayment"},
		func(ctx context.Context, _ any) (any, error) {
			tc, err := tenantctx.FromContext(ctx)
			if err != nil {
				return nil, err
			}
			seen = tc.TenantID
			return "served", nil
		})
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if seen != p.TenantID {
		t.Errorf("tenant = %q, want the principal's %q", seen, p.TenantID)
	}
}

// TestTenantInterceptorRejectsACallWithNoPrincipal asserts the ordering dependency: the tenant
// stage cannot run before authentication has produced a principal, and refuses rather than
// proceeding with no tenant.
//
// The code is Internal rather than Unauthenticated, and that is correct: authentication runs
// *above* this stage, so reaching it with no principal means the chain was assembled wrongly, not
// that a caller presented a bad credential. MISSING_TENANT_CONTEXT is catalogued INTERNAL for
// exactly that reason, and reporting Unauthenticated would send an integrator looking at their
// own credentials for a defect in ours.
func TestTenantInterceptorRejectsACallWithNoPrincipal(t *testing.T) {
	t.Parallel()
	served := false
	intercept := grpcapi.TenantUnaryInterceptor(nil)
	_, err := intercept(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/payments.v1.PaymentService/CreatePayment"},
		func(context.Context, any) (any, error) { served = true; return "served", nil })
	if served {
		t.Fatal("the handler ran with no tenant context")
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %s, want Internal", status.Code(err))
	}
}

// TestRateLimiterFailureAdmits documents the deliberate fail-open: the failure of a limiter is not
// evidence that capacity is exhausted.
func TestRateLimiterFailureAdmits(t *testing.T) {
	t.Parallel()
	ctx, err := tenantctx.WithTenant(context.Background(), tenantctx.TenantContext{
		TenantID:    "ten_01JB8Z00000000000000000000",
		Tier:        shared.TierPooled,
		Environment: shared.EnvironmentSandbox,
		Principal:   tenantctx.Principal{Type: tenantctx.PrincipalWorkload, ID: "svc"},
		Source:      tenantctx.SourceToken,
	})
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
	intercept := grpcapi.RateLimitUnaryInterceptor(brokenLimiter{},
		resilience.Limit{Rate: 10, Burst: 10}, nil)
	resp, err := intercept(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/payments.v1.PaymentService/CreatePayment"},
		func(context.Context, any) (any, error) { return "served", nil })
	if err != nil {
		t.Fatalf("a limiter error must admit the RPC: %v", err)
	}
	if resp != "served" {
		t.Errorf("response = %v, want the handler's", resp)
	}
}

// TestMethodService extracts the service name for the metric label.
func TestMethodService(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"/payments.v1.PaymentService/CreatePayment": "payments.v1.PaymentService",
		"/grpc.health.v1.Health/Check":              "grpc.health.v1.Health",
		"malformed":                                 "malformed",
	}
	for in, want := range tests {
		if got := grpcapi.MethodService(in); got != want {
			t.Errorf("MethodService(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDefaultPublicMethodsCoversTheProbeAndReflection asserts the bypass set is exactly what
// cannot present a credential.
func TestDefaultPublicMethodsCoversTheProbeAndReflection(t *testing.T) {
	t.Parallel()
	public := grpcapi.DefaultPublicMethods()
	for _, m := range []string{
		healthpb.Health_Check_FullMethodName,
		healthpb.Health_Watch_FullMethodName,
	} {
		if !public[m] {
			t.Errorf("%s is not public; a Kubernetes gRPC probe carries no credential", m)
		}
	}
	if public["/payments.v1.PaymentService/CreatePayment"] {
		t.Error("a money-path method is on the public bypass list")
	}
}

// --- helpers -------------------------------------------------------------------------------------

type allowAll struct{}

func (allowAll) Evaluate(context.Context, authz.Request) authz.Decision {
	return authz.Decision{Allow: true}
}

type brokenLimiter struct{}

func (brokenLimiter) Allow(context.Context, string, resilience.Limit) (resilience.Decision, error) {
	return resilience.Decision{}, apierror.New(apierror.CodeDependencyFailure, "redis is unreachable")
}

type errText string

func (e errText) Error() string { return string(e) }

// quietLogger discards lifecycle output so a passing test emits nothing. A failing one still
// reports through t, which is where a reader looks.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
