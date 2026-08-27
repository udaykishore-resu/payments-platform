package registry_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/adyen"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/paypal"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/registry"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/simulator"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/stripe"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func testRecord(id shared.GatewayID) registry.Record {
	return registry.Record{
		GatewayID:        id,
		BaseURL:          "https://" + string(id) + ".test",
		APIVersion:       "v1",
		Environment:      shared.EnvironmentSandbox,
		Timeout:          5 * time.Second,
		WebhookTolerance: time.Minute,
		HTTPClient:       httpx.NewRecordingDoer(),
		Clock:            shared.SystemClock{},
	}
}

// TestResolveReturnsRegisteredAdapters is the happy path: a slug goes in, behaviour comes out, and
// nothing in between names a vendor.
func TestResolveReturnsRegisteredAdapters(t *testing.T) {
	r, err := registry.NewWithBuiltIn(nil)
	if err != nil {
		t.Fatalf("NewWithBuiltIn: %v", err)
	}
	for _, id := range []shared.GatewayID{stripe.GatewayID, adyen.GatewayID, paypal.GatewayID, simulator.GatewayID} {
		if err := r.Configure(testRecord(id)); err != nil {
			t.Fatalf("Configure(%s): %v", id, err)
		}
	}
	for _, id := range r.Configured() {
		g, err := r.Resolve(t.Context(), id)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", id, err)
		}
		if g.ID() != id {
			t.Fatalf("Resolve(%s) returned an adapter identifying as %s", id, g.ID())
		}
		if _, err := r.ResolveProvisioner(t.Context(), id); err != nil {
			t.Fatalf("ResolveProvisioner(%s): %v", id, err)
		}
		if _, err := r.ResolveVerifier(t.Context(), id); err != nil {
			t.Fatalf("ResolveVerifier(%s): %v", id, err)
		}
	}
	if len(r.Registered()) != 4 {
		t.Fatalf("Registered() = %v, want four gateways", r.Registered())
	}
}

// TestResolveUnknownGatewayIsActionable proves the failure names what an operator has to do. A bare
// "not found" sends someone looking for a switch statement that does not exist.
func TestResolveUnknownGatewayIsActionable(t *testing.T) {
	r := registry.New()
	_, err := r.Resolve(t.Context(), shared.GatewayID("braintree"))
	if err == nil {
		t.Fatal("resolving an unregistered gateway succeeded")
	}
	if apierror.CodeOf(err) != apierror.CodeGatewayNotConfigured {
		t.Fatalf("code = %s, want %s", apierror.CodeOf(err), apierror.CodeGatewayNotConfigured)
	}
	var e *apierror.Error
	if !errors.As(err, &e) || len(e.Details) == 0 {
		t.Fatal("the error carries no detail telling the operator what to do")
	}
}

// TestRegisteredButUnconfiguredIsDistinct separates "this binary has no adapter for that gateway"
// from "this environment has no endpoint for it". They have different fixes and must not share an
// error.
func TestRegisteredButUnconfiguredIsDistinct(t *testing.T) {
	r := registry.New()
	if err := r.Register(stripe.NewFactory()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := r.Resolve(t.Context(), stripe.GatewayID)
	if err == nil {
		t.Fatal("resolving an unconfigured gateway succeeded")
	}
	if !strings.Contains(err.Error(), "no configuration") {
		t.Fatalf("the error does not distinguish an unconfigured gateway from an unregistered one: %v", err)
	}
}

// TestDuplicateRegistrationIsRejected proves the registry refuses to let two factories fight over a
// slug. Letting the last one win makes the answer depend on initialisation order, which behaves
// differently in a test binary than in production.
func TestDuplicateRegistrationIsRejected(t *testing.T) {
	r := registry.New()
	if err := r.Register(stripe.NewFactory()); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(stripe.NewFactory()); err == nil {
		t.Fatal("a duplicate registration was accepted")
	}
}

// TestReconfigureInvalidatesTheCachedAdapter proves a base-URL change actually takes effect. An
// accepted-then-ignored reconfiguration is worse than a rejected one: the operator believes traffic
// has moved and it has not.
func TestReconfigureInvalidatesTheCachedAdapter(t *testing.T) {
	r, err := registry.NewWithBuiltIn(nil)
	if err != nil {
		t.Fatalf("NewWithBuiltIn: %v", err)
	}
	rec := testRecord(simulator.GatewayID)
	if err := r.Configure(rec); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	first, err := r.Resolve(t.Context(), simulator.GatewayID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	rec.BaseURL = "https://simulator-two.test"
	if err := r.Configure(rec); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	second, err := r.Resolve(t.Context(), simulator.GatewayID)
	if err != nil {
		t.Fatalf("Resolve after reconfigure: %v", err)
	}
	if first == second {
		t.Fatal("the registry returned the adapter built from the previous record; a base-URL change would be accepted and ignored")
	}
}

// TestProvisioningBaseURLIsSeparate covers the Adyen-shaped case: the onboarding APIs live on a
// different hostname from the payment API, and deriving one from the other by string surgery is a
// guess that works in sandbox and fails in a live account.
func TestProvisioningBaseURLIsSeparate(t *testing.T) {
	rec := testRecord(adyen.GatewayID)
	rec.ProvisioningBaseURL = "https://management.adyen.test"
	if got := rec.Config().BaseURL; got != rec.BaseURL {
		t.Fatalf("Config().BaseURL = %q, want the payment endpoint %q", got, rec.BaseURL)
	}
	if got := rec.ProvisioningConfig().BaseURL; got != rec.ProvisioningBaseURL {
		t.Fatalf("ProvisioningConfig().BaseURL = %q, want %q", got, rec.ProvisioningBaseURL)
	}

	// A vendor that serves both from one host leaves the field empty and gets the payment endpoint.
	rec.ProvisioningBaseURL = ""
	if got := rec.ProvisioningConfig().BaseURL; got != rec.BaseURL {
		t.Fatalf("with no provisioning URL, ProvisioningConfig().BaseURL = %q, want %q", got, rec.BaseURL)
	}
}

// TestConfigureRejectsAnAdapterWithoutTransport pins the rule that keeps adapters inside the
// resilience envelope. An adapter that builds its own http.Client is outside the bulkhead, outside
// the circuit breaker and outside the connection-pool budget, and it is invariably the one that
// takes the service down.
func TestConfigureRejectsAnAdapterWithoutTransport(t *testing.T) {
	r, err := registry.NewWithBuiltIn(nil)
	if err != nil {
		t.Fatalf("NewWithBuiltIn: %v", err)
	}
	rec := testRecord(stripe.GatewayID)
	rec.HTTPClient = nil
	if err := r.Configure(rec); err == nil {
		t.Fatal("a record with no HTTP client was accepted")
	}
	rec = testRecord(stripe.GatewayID)
	rec.Environment = ""
	if err := r.Configure(rec); err == nil {
		t.Fatal("a record with no environment was accepted; the wrong default charges a real card")
	}
}

// --- the structural assertion -------------------------------------------------------------------

// gatewaySlugs are the vendor names a switch statement outside the adapter layer would branch on.
var gatewaySlugs = map[string]struct{}{
	"stripe": {}, "adyen": {}, "paypal": {}, "simulator": {},
	"braintree": {}, "checkout": {}, "worldpay": {}, "mollie": {},
}

// TestNoGatewayNameSwitchOutsideAdapters is the assertion this whole package exists to make
// enforceable.
//
// Every payments platform grows a `switch gatewayID { case "stripe": ... }`, usually in more than
// one place, and each copy is a place a new gateway gets forgotten — classically the refund path,
// discovered months later by a merchant who cannot get their money back. Registration cannot be
// forgotten in the same way: an unregistered gateway fails loudly at resolve time, at startup,
// rather than silently taking a wrong branch on one code path out of five.
//
// The check parses every Go file under internal/ and cmd/, excluding the adapter layer itself —
// where naming a vendor is the entire job — and fails on any switch whose case clauses contain a
// gateway slug as a string literal.
func TestNoGatewayNameSwitchOutsideAdapters(t *testing.T) {
	root := moduleRoot(t)
	scanned := 0
	var offenders []string

	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// The adapter layer is exactly where a vendor may be named. Everything above it must
				// reach a gateway through the registry.
				if strings.Contains(filepath.ToSlash(path), "internal/adapters/gateway") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			scanned++
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				// A file that does not parse is another agent's work in progress, not a finding.
				return nil //nolint:nilerr // returning nil continues the WalkDir; a file that does not parse is another agent's work in progress, not a finding
			}
			ast.Inspect(file, func(n ast.Node) bool {
				sw, ok := n.(*ast.SwitchStmt)
				if !ok || sw.Body == nil {
					return true
				}
				for _, stmt := range sw.Body.List {
					clause, ok := stmt.(*ast.CaseClause)
					if !ok {
						continue
					}
					for _, expr := range clause.List {
						lit, ok := expr.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						value := strings.Trim(lit.Value, "`\"")
						if _, bad := gatewaySlugs[strings.ToLower(value)]; bad {
							rel, _ := filepath.Rel(root, path)
							offenders = append(offenders,
								rel+":"+fset.Position(lit.Pos()).String()[strings.LastIndex(fset.Position(lit.Pos()).String(), ":")+1:]+
									" switches on "+value)
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}

	if scanned == 0 {
		t.Fatal("the scan found no Go files, so it proves nothing; the module root detection is wrong")
	}
	if len(offenders) > 0 {
		t.Fatalf("a gateway-name switch exists outside the adapter layer:\n  %s\n\n"+
			"Adding a gateway must be a registration in internal/adapters/gateway/registry, not a branch. "+
			"A switch is a place the next gateway gets forgotten on one code path out of five.",
			strings.Join(offenders, "\n  "))
	}
}

// TestBuiltInCoversEveryShippedAdapter proves the wiring list and the adapters agree.
//
// Without it an adapter can be written, reviewed and merged while never being reachable, because
// the one line that registers it was left out of the pull request.
func TestBuiltInCoversEveryShippedAdapter(t *testing.T) {
	got := map[shared.GatewayID]bool{}
	for _, f := range registry.BuiltIn(nil) {
		if got[f.ID()] {
			t.Fatalf("BuiltIn lists %s twice", f.ID())
		}
		got[f.ID()] = true
	}
	for _, want := range []shared.GatewayID{stripe.GatewayID, adyen.GatewayID, paypal.GatewayID, simulator.GatewayID} {
		if !got[want] {
			t.Errorf("BuiltIn does not include %s; the adapter exists but nothing can reach it", want)
		}
	}
}

// TestFactoriesNeverReturnNilWithoutError covers the registry's own version of the SPI's
// never-nil-nil obligation, one level up: a factory that answered (nil, nil) would put a nil
// interface into the cache and panic on the next payment.
func TestFactoriesNeverReturnNilWithoutError(t *testing.T) {
	cfg := spi.Config{
		BaseURL:     "https://example.test",
		HTTPClient:  httpx.NewRecordingDoer(),
		Clock:       shared.SystemClock{},
		Environment: shared.EnvironmentSandbox,
	}
	for _, f := range registry.BuiltIn(nil) {
		g, err := f.NewGateway(cfg)
		if g == nil && err == nil {
			t.Errorf("%s: NewGateway returned nil with no error", f.ID())
		}
		p, err := f.NewProvisioner(cfg)
		if p == nil && err == nil {
			t.Errorf("%s: NewProvisioner returned nil with no error", f.ID())
		}
		v, err := f.NewVerifier(cfg)
		if v == nil && err == nil {
			t.Errorf("%s: NewVerifier returned nil with no error", f.ID())
		}
	}
}

// moduleRoot walks up from the test's working directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the working directory")
		}
		dir = parent
	}
}
