package simulator_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/contract"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/simulator"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/stripe"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The simulator runs the same conformance suite as the three vendor adapters, deliberately. A
// simulator that were exempt from the rules it is used to verify would be a simulator whose green
// tests mean nothing: the platform would be checked against a gateway that is allowed to cheat.

const (
	simBaseURL = "https://simulator.test"
	simAPIKey  = "simkey_contract_suite_0000000000001"
	// The webhook secret is hex so the same engine can emulate Adyen's scheme, whose keys must
	// hex-decode.
	simPrimarySecret = "0011223344556677889900112233445566778899001122334455667788990011"
	simRotatedSecret = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	simUnknownSecret = "1234123412341234123412341234123412341234123412341234123412341234"
)

var simClock = shared.FixedClock{T: contract.WebhookNow}

func simConfig(d spi.HTTPDoer) spi.Config {
	return spi.Config{
		BaseURL:          simBaseURL,
		HTTPClient:       d,
		Clock:            simClock,
		Timeout:          10 * time.Second,
		Environment:      shared.EnvironmentSandbox,
		WebhookTolerance: 5 * time.Minute,
	}
}

func simCredentials() spi.Credentials {
	return spi.Credentials{
		Values:      map[string]string{simulator.CredentialAPIKey: simAPIKey},
		Version:     "v1",
		Environment: shared.EnvironmentSandbox,
	}
}

func simResponse(status, reference string, extra string) string {
	return fmt.Sprintf(`{"status":%q,"reference":%q,"rawStatus":"succeeded"%s}`, status, reference, extra)
}

func simAmountField(name string, m money.Money) string {
	return fmt.Sprintf(`,%q:{"minorUnits":%d,"currency":%q}`, name, m.Amount(), m.Currency())
}

func simResponses() contract.Responses {
	return contract.Responses{
		AuthorizeApproved: func(ref string, amount money.Money, key string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, simResponse("CAPTURED", ref,
				simAmountField("capturedAmount", amount)+`,"authCode":"SIM1","avsResult":"Y","cvvResult":"M","scenario":"approve"`))
		},
		AuthorizeHardDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, `{"status":"DECLINED","reference":"`+ref+
				`","rawStatus":"refused","rawCode":"stolen_card","declineCode":"stolen_card","networkAdviceNoRetry":true,"scenario":"decline_stolen_card"}`)
		},
		AuthorizeSoftDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, `{"status":"DECLINED","reference":"`+ref+
				`","rawStatus":"refused","rawCode":"do_not_honor","declineCode":"do_not_honor","scenario":"decline_do_not_honor"}`)
		},
		AuthorizeUnmappedDecline: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, `{"status":"DECLINED","reference":"`+ref+
				`","rawStatus":"refused","rawCode":"sim_reason_the_adapter_has_never_seen","declineCode":"sim_reason_the_adapter_has_never_seen","scenario":"decline_unmapped"}`)
		},
		AuthorizeAmountMismatch: func(ref string, requested money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(
				`{"status":"CAPTURED","reference":%q,"rawStatus":"succeeded","capturedAmount":{"minorUnits":%d,"currency":"EUR"},"scenario":"amount_mismatch"}`,
				ref, requested.Amount()+7400))
		},
		CaptureAccepted: func(ref string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, simResponse("CAPTURED", ref+"_capture", simAmountField("capturedAmount", amount)))
		},
		RefundAccepted: func(ref string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, simResponse("REFUND_ACCEPTED", ref+"_refund", simAmountField("capturedAmount", amount)))
		},
		VoidAccepted: func(ref string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, simResponse("VOIDED", ref+"_void", ""))
		},
		LookupByRef: func(ref, key string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, simResponse("CAPTURED", ref, simAmountField("capturedAmount", amount)))
		},
		LookupByKey: func(ref, key string, amount money.Money) *spi.HTTPResponse {
			return httpx.JSONResponse(200, simResponse("CAPTURED", ref, simAmountField("capturedAmount", amount)))
		},
		LookupNotFound: func() *spi.HTTPResponse {
			return httpx.JSONResponse(200, `{"status":"NOT_FOUND"}`)
		},
		AuthFailure: func() *spi.HTTPResponse {
			return httpx.JSONResponse(401,
				`{"code":"GATEWAY_AUTHENTICATION_FAILED","message":"simulator: the api key was rejected"}`)
		},
		Provisioned: func(accountID string) *spi.HTTPResponse {
			return httpx.JSONResponse(200, fmt.Sprintf(`{"accountId":%q,"status":"ACTIVE"}`, accountID))
		},
		DeprovisionMissing: func() *spi.HTTPResponse {
			return httpx.JSONResponse(404, `{"code":"PAYMENT_NOT_FOUND","message":"simulator: no such account"}`)
		},
		DeprovisionOK: func(accountID string) *spi.HTTPResponse {
			return &spi.HTTPResponse{StatusCode: 204, Headers: map[string]string{}}
		},
	}
}

func simEventBody(at time.Time) []byte {
	return []byte(fmt.Sprintf(
		`{"id":"sim_evt_contract_0001","type":"payment.captured","created":%d,"reference":%q,"idempotencyKey":%q,"paymentId":%q,"status":"CAPTURED","amount":{"minorUnits":2500,"currency":"USD"}}`,
		at.Unix(), contract.SuiteGatewayRef, contract.SuiteIdempotencyKey, contract.SuitePaymentID))
}

func simWebhookFixture() contract.WebhookFixture {
	return contract.WebhookFixture{
		Secret:        simPrimarySecret,
		RotatedSecret: simRotatedSecret,
		UnknownSecret: simUnknownSecret,
		Build: func(secret string, at time.Time) ([]byte, map[string]string) {
			body := simEventBody(at)
			return body, map[string]string{simulator.SignatureHeader: simulator.Sign(body, secret, at)}
		},
		BuildInvalidJSON: func(secret string, at time.Time) ([]byte, map[string]string) {
			body := []byte(`{"id":"sim_evt_contract_0001","type":`)
			return body, map[string]string{simulator.SignatureHeader: simulator.Sign(body, secret, at)}
		},
		Tamper: func(body []byte) []byte {
			return []byte(strings.Replace(string(body), `"minorUnits":2500`, `"minorUnits":990000`, 1))
		},
	}
}

func simSubject() contract.Subject {
	creds := simCredentials()
	engine := simulator.NewEngine(simulator.EngineOptions{
		Clock:         simClock,
		WebhookSecret: simPrimarySecret,
	})
	return contract.Subject{
		Name:        "simulator",
		GatewayID:   simulator.GatewayID,
		Credentials: creds,
		NewGateway: func(d spi.HTTPDoer) (spi.PaymentGateway, error) {
			return simulator.NewAdapter(simConfig(d))
		},
		NewProvisioner: func(d spi.HTTPDoer) (spi.GatewayProvisioner, error) {
			p, err := simulator.NewProvisionerAdapter(simConfig(d))
			if err != nil {
				return nil, err
			}
			return p.WithCredentials(creds), nil
		},
		// Verification needs the signing scheme and the secret, both of which live on the engine that
		// emitted the event. There is no network call, so the doer is unused.
		NewVerifier: func(d spi.HTTPDoer) (spi.WebhookVerifier, error) {
			return engine, nil
		},
		Responses: simResponses(),
		Webhook:   simWebhookFixture(),
		IdempotencyKeyOf: func(r httpx.RecordedRequest) string {
			return r.Header(simulator.IdempotencyHeader)
		},
		SupportsVoid: true,
	}
}

// TestSimulatorContract runs the shared conformance suite against the simulator's HTTP adapter.
func TestSimulatorContract(t *testing.T) {
	contract.RunSuite(t, simSubject())
}

// TestSimulatorTriggerTable pins the documented amount-derived behaviour.
//
// The table is a published interface: integration tests, load tests and local development all pick
// amounts from it, and silently changing what 2501 means would break a test suite in another
// package with no compile error to warn anyone.
func TestSimulatorTriggerTable(t *testing.T) {
	tests := []struct {
		minorUnits int64
		want       simulator.Scenario
	}{
		{2500, simulator.ScenarioApprove},
		{2501, simulator.ScenarioDeclineInsufficientFunds},
		{2502, simulator.ScenarioDeclineDoNotHonor},
		{2503, simulator.ScenarioDeclineStolenCard},
		{2504, simulator.ScenarioDeclineUnmapped},
		{2505, simulator.ScenarioRequiresAction},
		{2506, simulator.ScenarioPending},
		{2507, simulator.ScenarioTimeout},
		{2508, simulator.ScenarioMalformed},
		{2509, simulator.ScenarioAmountMismatch},
		{2510, simulator.ScenarioDuplicateWebhook},
		{2511, simulator.ScenarioSlow},
		{2512, simulator.ScenarioGatewayError},
		{2513, simulator.ScenarioAuthFailure},
		{2599, simulator.ScenarioApprove},
	}
	for _, tc := range tests {
		got := simulator.ScenarioForAmount(money.MustNew(tc.minorUnits, "USD"))
		if got != tc.want {
			t.Errorf("ScenarioForAmount(%d) = %q, want %q", tc.minorUnits, got, tc.want)
		}
	}

	// The metadata override wins over the amount, so a test that needs a specific amount for an
	// unrelated reason can still choose its behaviour.
	got := simulator.ResolveScenario(
		map[string]string{simulator.ScenarioMetadataKey: string(simulator.ScenarioPending)},
		money.MustNew(2500, "USD"))
	if got != simulator.ScenarioPending {
		t.Errorf("the metadata override was ignored: got %q", got)
	}
	// An unrecognised override falls back to the amount rather than erroring, so a misspelled
	// scenario produces a clear assertion failure about the outcome instead of a simulator error
	// that looks like a transport problem.
	got = simulator.ResolveScenario(
		map[string]string{simulator.ScenarioMetadataKey: "not-a-scenario"},
		money.MustNew(2501, "USD"))
	if got != simulator.ScenarioDeclineInsufficientFunds {
		t.Errorf("an unknown override did not fall back to the amount trigger: got %q", got)
	}
}

// TestSimulatorEngineIdempotency proves the engine replays the identical stored response.
//
// This is the behaviour that makes the simulator useful for testing the orchestrator's retry path:
// an adapter or a workflow that re-derives its result from the request rather than from the
// response returns a different reference the second time, and this catches it.
func TestSimulatorEngineIdempotency(t *testing.T) {
	e := simulator.NewEngine(simulator.EngineOptions{Clock: simClock})
	req := spi.AuthorizeRequest{
		IdempotencyKey: contract.SuiteIdempotencyKey,
		Credentials:    simCredentials(),
		PaymentID:      shared.PaymentID(contract.SuitePaymentID),
		AttemptID:      shared.AttemptID(contract.SuiteAttemptID),
		Amount:         money.MustNew(2500, "USD"),
		Method:         shared.MethodCard,
		Capture:        true,
	}
	first, err := e.Authorize(t.Context(), req)
	if err != nil {
		t.Fatalf("first Authorize: %v", err)
	}
	second, err := e.Authorize(t.Context(), req)
	if err != nil {
		t.Fatalf("second Authorize: %v", err)
	}
	if first.GatewayRef != second.GatewayRef {
		t.Fatalf("a replayed key produced reference %q where the first produced %q", second.GatewayRef, first.GatewayRef)
	}

	// A different key is a different transaction, which is the other half of the property: an
	// idempotency store that returned the same answer for every key would also pass the check above.
	req.IdempotencyKey = contract.SuiteAltIdempotencyKey
	third, err := e.Authorize(t.Context(), req)
	if err != nil {
		t.Fatalf("third Authorize: %v", err)
	}
	if third.GatewayRef == first.GatewayRef {
		t.Fatalf("a different idempotency key returned the same reference %q", third.GatewayRef)
	}
}

// TestSimulatorEngineScenarios walks the scenarios that change the *outcome*, in process.
func TestSimulatorEngineScenarios(t *testing.T) {
	tests := []struct {
		minorUnits int64
		wantStatus spi.Status
		wantReason payment.DeclineReason
		wantErr    bool
		unknown    bool
	}{
		{2500, spi.StatusCaptured, "", false, false},
		{2501, spi.StatusDeclined, payment.DeclineInsufficientFunds, false, false},
		{2502, spi.StatusDeclined, payment.DeclineDoNotHonorSoft, false, false},
		{2503, spi.StatusDeclined, payment.DeclineStolenCard, false, false},
		{2504, spi.StatusDeclined, payment.DeclineUnknown, false, false},
		{2505, spi.StatusRequiresAction, "", false, false},
		{2506, spi.StatusPending, "", false, false},
		{2507, "", "", true, true},
		{2512, "", "", true, true},
		{2513, "", "", true, false},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("amount_%d", tc.minorUnits), func(t *testing.T) {
			e := simulator.NewEngine(simulator.EngineOptions{Clock: simClock})
			res, err := e.Authorize(t.Context(), spi.AuthorizeRequest{
				IdempotencyKey: fmt.Sprintf("key-%d", tc.minorUnits),
				Credentials:    simCredentials(),
				Amount:         money.MustNew(tc.minorUnits, "USD"),
				Method:         shared.MethodCard,
				Capture:        true,
			})
			if res == nil && err == nil {
				t.Fatal("nil result and nil error")
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got status %q", res.Status)
				}
				if tc.unknown && !isUnknown(err) {
					t.Fatalf("expected an unknown outcome, got %v", err)
				}
				if !tc.unknown && isUnknown(err) {
					t.Fatalf("did not expect an unknown outcome, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", res.Status, tc.wantStatus)
			}
			if tc.wantReason != "" && res.DeclineReason != tc.wantReason {
				t.Fatalf("decline reason = %q, want %q", res.DeclineReason, tc.wantReason)
			}
		})
	}

	// The mismatched-echo scenario must be surfaced as an error, not absorbed.
	e := simulator.NewEngine(simulator.EngineOptions{Clock: simClock})
	_, err := e.Authorize(t.Context(), spi.AuthorizeRequest{
		IdempotencyKey: "key-mismatch",
		Credentials:    simCredentials(),
		Amount:         money.MustNew(2509, "USD"),
		Method:         shared.MethodCard,
		Capture:        true,
	})
	if err == nil {
		t.Fatal("the amount-mismatch scenario was accepted silently")
	}
	if apierror.CodeOf(err) != apierror.CodeGatewayContractViolation {
		t.Fatalf("amount mismatch produced code %s, want %s", apierror.CodeOf(err), apierror.CodeGatewayContractViolation)
	}
}

// isUnknown is the single question the orchestrator asks of every error on a money-moving call:
// may money have moved? errors.Is is the right instrument because the sentinel is wrapped in an
// *apierror.Error whose code carries the transport classification, and the two answers are
// independent.
func isUnknown(err error) bool { return errors.Is(err, spi.ErrOutcomeUnknown) }

// TestSimulatorEmitsVerifiableStripeWebhooks proves the simulator's Stripe emulation produces
// signatures the *real* Stripe verifier accepts.
//
// This is what makes the simulator worth having for webhook testing: the ingress path exercised
// end-to-end is the production verifier, not a simulator-shaped stand-in that could be wrong in
// the same way twice.
func TestSimulatorEmitsVerifiableStripeWebhooks(t *testing.T) {
	const secret = "whsec_simulator_emulation_00000001"
	e := simulator.NewEngine(simulator.EngineOptions{
		Clock:         simClock,
		Scheme:        simulator.SchemeStripe,
		WebhookSecret: secret,
	})
	res, err := e.Authorize(t.Context(), spi.AuthorizeRequest{
		IdempotencyKey: contract.SuiteIdempotencyKey,
		Credentials:    simCredentials(),
		Amount:         money.MustNew(2500, "USD"),
		Method:         shared.MethodCard,
		Capture:        true,
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	emitted, err := e.EmitWebhook(res.GatewayRef, "payment.captured", true)
	if err != nil {
		t.Fatalf("EmitWebhook: %v", err)
	}
	if len(emitted) != 2 {
		t.Fatalf("a duplicate emission produced %d events, want 2", len(emitted))
	}
	// A real gateway's redelivery is byte-identical, including the signature. A "duplicate" that
	// differed would not test deduplication at all.
	if string(emitted[0].Body) != string(emitted[1].Body) {
		t.Fatal("the duplicate emission differs from the original body")
	}

	v, err := stripe.NewVerifier(spi.Config{WebhookTolerance: 5 * time.Minute})
	if err != nil {
		t.Fatalf("stripe.NewVerifier: %v", err)
	}
	first, err := v.Verify(t.Context(), emitted[0].Body, emitted[0].Headers, []string{secret}, simClock.Now())
	if err != nil {
		t.Fatalf("the real Stripe verifier rejected the simulator's Stripe-scheme webhook: %v", err)
	}
	second, err := v.Verify(t.Context(), emitted[1].Body, emitted[1].Headers, []string{secret}, simClock.Now())
	if err != nil {
		t.Fatalf("the duplicate was rejected: %v", err)
	}
	if first.GatewayEventID != second.GatewayEventID {
		t.Fatalf("the duplicate carries event id %q, the original %q; deduplication would not suppress it",
			second.GatewayEventID, first.GatewayEventID)
	}
}

// TestSimulatorServerEndToEnd points the real HTTP adapter, through a real transport, at a real
// socket serving the simulator.
//
// This is the assertion the in-process Engine cannot make: connection pooling, deadline propagation
// and the classification of a genuine client timeout all live below the SPI, and the only way to
// exercise them is with a socket that actually stops answering.
func TestSimulatorServerEndToEnd(t *testing.T) {
	engine := simulator.NewEngine(simulator.EngineOptions{Clock: simClock, WebhookSecret: simPrimarySecret})
	srv := httptest.NewServer(simulator.NewServer(engine, simulator.ServerOptions{
		APIKey: simAPIKey,
		Clock:  simClock,
	}))
	defer srv.Close()

	client := httpx.New(httpx.Options{
		GatewayID: simulator.GatewayID,
		Timeout:   2 * time.Second,
	})
	g, err := simulator.NewAdapter(spi.Config{
		BaseURL:     srv.URL,
		HTTPClient:  client,
		Clock:       simClock,
		Environment: shared.EnvironmentSandbox,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	req := spi.AuthorizeRequest{
		IdempotencyKey: "e2e-key-0001",
		Credentials:    simCredentials(),
		PaymentID:      shared.PaymentID(contract.SuitePaymentID),
		Amount:         money.MustNew(2500, "USD"),
		Method:         shared.MethodCard,
		Capture:        true,
	}
	res, err := g.Authorize(t.Context(), req)
	if err != nil {
		t.Fatalf("Authorize over HTTP: %v", err)
	}
	if res.Status != spi.StatusCaptured {
		t.Fatalf("status = %q, want CAPTURED", res.Status)
	}
	if res.GatewayRef == "" {
		t.Fatal("no gateway reference over HTTP")
	}

	// The same key over a real socket must return the same stored answer.
	again, err := g.Authorize(t.Context(), req)
	if err != nil {
		t.Fatalf("repeated Authorize over HTTP: %v", err)
	}
	if again.GatewayRef != res.GatewayRef {
		t.Fatalf("the replayed call returned reference %q, want %q", again.GatewayRef, res.GatewayRef)
	}

	// A lookup by idempotency key alone, over the wire.
	found, err := g.Lookup(t.Context(), spi.LookupRequest{
		Credentials:    simCredentials(),
		IdempotencyKey: "e2e-key-0001",
		Operation:      shared.OpAuthorize,
	})
	if err != nil {
		t.Fatalf("Lookup by key over HTTP: %v", err)
	}
	if found.Status == spi.StatusNotFound {
		t.Fatal("a lookup by key alone found nothing over HTTP")
	}

	// A genuine client timeout: the server holds the connection until the caller's deadline expires,
	// and the adapter must classify that as an unknown outcome.
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	_, err = g.Authorize(ctx, spi.AuthorizeRequest{
		IdempotencyKey: "e2e-key-timeout",
		Credentials:    simCredentials(),
		Amount:         money.MustNew(2507, "USD"), // trigger 07: timeout
		Method:         shared.MethodCard,
		Capture:        true,
	})
	if err == nil {
		t.Fatal("a held connection produced no error")
	}
	if !isUnknown(err) {
		t.Fatalf("a real client timeout was classified as %v, want an unknown outcome", err)
	}
}

// TestSimulatorProtocolRejectsUnknownFields pins the one place in the repository where strict JSON
// decoding is correct: a protocol both ends of which we ship from the same commit.
func TestSimulatorProtocolRejectsUnknownFields(t *testing.T) {
	engine := simulator.NewEngine(simulator.EngineOptions{Clock: simClock})
	srv := httptest.NewServer(simulator.NewServer(engine, simulator.ServerOptions{Clock: simClock}))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"operation":           "authorize",
		"idempotencyKey":      "strict-0001",
		"amount":              map[string]any{"minorUnits": 2500, "currency": "USD"},
		"aFieldFromTheFuture": "which means two of our own binaries disagree",
	})
	d := httpx.New(httpx.Options{GatewayID: simulator.GatewayID, Timeout: 2 * time.Second})
	resp, err := d.Do(&spi.HTTPRequest{
		Ctx:    t.Context(),
		Method: "POST",
		URL:    srv.URL + simulator.PathPrefix + "/payments",
		Headers: map[string]string{
			"Content-Type":              "application/json",
			simulator.IdempotencyHeader: "strict-0001",
		},
		Body: body,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown protocol field was accepted with HTTP %d; version skew between our own binaries must fail loudly",
			resp.StatusCode)
	}
}
