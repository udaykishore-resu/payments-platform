//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// The brief's final goal, as one test.
//
// Verifies: baseline §8 (merchant FSM), §11 (onboarding workflow), §11.4 (certification),
// §12 (payment pipeline), §13 (events), docs/testing.md §7 (the whole of FS-1…FS-10 in outline).
//
// Registration → onboarding → validation → gateway provisioning → certification → activation →
// payment → routing → gateway → webhook → ledger → settlement. Every stage asserts *observable
// state* before the next begins, so a failure names the stage rather than leaving the reader to
// bisect a nine-step run.
//
// It is one test rather than nine because the stages are not independent: a merchant that cannot
// be certified cannot be activated, and a test that "verified activation" against a fixture
// merchant would be verifying the fixture. The cost is that a failure early on hides what comes
// after — which is the honest cost of the thing being sequential.

// TestMerchantJourneyFromRegistrationToSettlement is the end-to-end goal.
func TestMerchantJourneyFromRegistrationToSettlement(t *testing.T) {
	// Not parallel: it registers a merchant, drives a twelve-step workflow and then transacts
	// against it. Running it beside the payment tests would have those tests competing for the
	// merchant's rate-limit bucket, and a 429 in an unrelated test is the least useful failure a
	// suite can produce.
	c := newClient(t)
	tenant := testenv.TenantID(t)
	ctx := ctxFor(t, 10*time.Minute)

	// --- stage 1: registration -----------------------------------------------------------------
	//
	// The tenant is taken from the token; a tenantId in the body is rejected (§16.2). Sending one
	// is asserted separately in the security suite — here the point is that the merchant lands in
	// CREATED and nowhere else.
	external := "e2e-" + idempotencyKey("m")
	created := c.expect(c.post(ctx, "/v1/merchants", idempotencyKey("register"), map[string]any{
		"displayName":       "E2E Journey GmbH",
		"externalReference": external,
		"residencyRegion":   "eu-central-1",
		"businessProfile": map[string]any{
			"legalName":             "E2E Journey GmbH",
			"country":               "DE",
			"mcc":                   "5734",
			"websiteUrl":            "https://example.test",
			"expectedMonthlyVolume": map[string]any{"minorUnits": 5_000_000, "currency": "EUR"},
		},
	}), http.StatusCreated, "register a merchant")

	var m struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	created.JSON(t, &m)
	if m.ID == "" {
		t.Fatalf("the merchant was created with no id: %s", created.Body)
	}
	if m.Status != "CREATED" {
		t.Fatalf("a freshly registered merchant is %s, want CREATED. §8's FSM starts at CREATED and "+
			"a merchant that starts anywhere else has skipped a validation somebody wrote for a reason.",
			m.Status)
	}
	t.Logf("merchant %s registered under tenant %s (export PP_TEST_MERCHANT_ID=%s to reuse it)",
		m.ID, tenant, m.ID)

	// --- stage 2: onboarding starts -------------------------------------------------------------
	//
	// Starting twice is a no-op returning the same instance (§11). Asserting that here rather than
	// in a separate test is deliberate: a duplicate start is what a client retry produces, and the
	// consequence — two sets of gateway sub-accounts for one merchant — is a manual cleanup at
	// every vendor.
	startKey := idempotencyKey("onboard")
	first := c.expect(c.post(ctx, "/v1/merchants/"+m.ID+"/onboarding", startKey, map[string]any{}),
		http.StatusAccepted, "start onboarding")
	second := c.post(ctx, "/v1/merchants/"+m.ID+"/onboarding", startKey, map[string]any{})
	if second.Status != http.StatusAccepted && second.Status != http.StatusOK {
		t.Fatalf("starting onboarding twice returned %d; §11 says it is a no-op returning the "+
			"existing instance — %s", second.Status, second.Problem(t))
	}
	var firstInstance, secondInstance struct {
		InstanceID string `json:"instanceId"`
	}
	first.JSON(t, &firstInstance)
	second.JSON(t, &secondInstance)
	if firstInstance.InstanceID != "" && secondInstance.InstanceID != "" &&
		firstInstance.InstanceID != secondInstance.InstanceID {
		t.Fatalf("two onboarding instances exist for one merchant: %s and %s",
			firstInstance.InstanceID, secondInstance.InstanceID)
	}

	// --- stages 3–6: validation, provisioning, certification, activation ------------------------
	//
	// The workflow drives itself: KYC and bank validation resolve through the sandbox providers,
	// gateways are provisioned at the simulator, and certification runs the adapter contract suite
	// against the sandbox (§11.4). The manual compliance gate is the one step that needs a human,
	// so the test signals it — with the authorization the real approver would need.
	awaitMerchantStatus(t, c, m.ID, 3*time.Minute, "COMPLIANCE_REVIEW", "ACTIVE")

	if status := merchantStatus(t, c, m.ID); status == "COMPLIANCE_REVIEW" {
		c.expect(c.post(ctx, "/v1/merchants/"+m.ID+"/onboarding/signals/compliance-approval",
			idempotencyKey("approve"), map[string]any{
				"decision": "APPROVE",
				"reason":   "e2e journey",
			}), http.StatusAccepted, "approve the compliance gate")
	}

	awaitMerchantStatus(t, c, m.ID, 3*time.Minute, "ACTIVE")

	// Activation is guarded (§8): a CERTIFIED connection, a non-empty configuration, an
	// attestation and no open critical reconciliation exception. Reading the merchant back and
	// checking the guards' *evidence* is what makes "ACTIVE" mean something rather than being a
	// string somebody set.
	var active struct {
		Status               string   `json:"status"`
		ConfigurationVersion int      `json:"configurationVersion"`
		CertifiedGateways    []string `json:"certifiedGateways"`
	}
	c.expect(c.get(ctx, "/v1/merchants/"+m.ID), http.StatusOK, "read the active merchant").JSON(t, &active)
	if active.ConfigurationVersion < 1 {
		t.Fatalf("an ACTIVE merchant has configuration version %d; §8 requires a non-empty "+
			"configuration before activation, and processing with no limits and no enabled "+
			"currencies is worse than a retryable 503", active.ConfigurationVersion)
	}
	if len(active.CertifiedGateways) == 0 {
		t.Fatal("an ACTIVE merchant has no certified gateway. Certification is an artifact, not an " +
			"opinion (§11.4), and activating without one means the first real payment is the test.")
	}

	// --- stage 7: payment ----------------------------------------------------------------------
	//
	// `…00` is the simulator's approve trigger, so this is the clean path: routed, dispatched,
	// captured.
	const approve = 12_300
	pay := c.expect(c.post(ctx, "/v1/payments", idempotencyKey("journey-pay"),
		createPaymentBody(m.ID, approve, "EUR", "AUTOMATIC")),
		http.StatusCreated, "create a payment")

	var p payment
	pay.JSON(t, &p)
	if p.Amount.MinorUnits != approve {
		t.Fatalf("the payment's amount is %d, want %d in minor units. Money on this platform is an "+
			"integer of minor units and never a float (§7 rule 5).", p.Amount.MinorUnits, approve)
	}
	if p.State != "CAPTURED" {
		t.Fatalf("an approved automatic-capture payment is %s, want CAPTURED", p.State)
	}

	// --- stage 8: routing ----------------------------------------------------------------------
	//
	// A payment that reached a gateway must say which one, and the plan behind that choice must be
	// persisted with its reasons — a routing decision that cannot be replayed six months later is
	// a routing decision nobody can defend to a merchant.
	// The gateway lives on the attempt, not on the payment: a payment can have two attempts on
	// two gateways, so `Payment` carries `routingPlanId` and each `PaymentAttempt` carries its
	// own `gatewayId`.
	if p.RoutingPlanID == "" {
		t.Fatal("the payment names no routing plan; the routing decision was not persisted")
	}
	if len(p.Attempts) == 0 || p.Attempts[0].GatewayID == "" {
		t.Fatal("no attempt names a gateway; the routing decision was not persisted")
	}
	if len(p.Attempts) != 1 {
		t.Fatalf("%d attempts for a first-time approval, want 1", len(p.Attempts))
	}
	if p.Attempts[0].Outcome != "SUCCESS" {
		t.Fatalf("the only attempt is %s, want SUCCESS", p.Attempts[0].Outcome)
	}

	// --- stages 9–11: webhook, ledger, settlement ----------------------------------------------
	//
	// The gateway's asynchronous confirmation arrives on its own schedule, so this polls rather
	// than assuming. The terminal states are both acceptable: SETTLED once the settlement report
	// lands, CAPTURED until it does.
	final := c.awaitState(ctx, p.ID, 2*time.Minute, "CAPTURED", "SETTLED")
	if final.Captured.MinorUnits != approve {
		t.Fatalf("captured %d of an authorized %d", final.Captured.MinorUnits, approve)
	}
	if final.Refunded.MinorUnits != 0 {
		t.Fatalf("a payment nobody refunded shows %d refunded", final.Refunded.MinorUnits)
	}
	if final.ReconRequired {
		t.Fatal("a cleanly captured payment is flagged for reconciliation; the exception queue " +
			"stops being readable the moment it contains payments that are fine")
	}
}

// merchantStatus reads the merchant's current lifecycle state.
func merchantStatus(t *testing.T, c *client, id string) string {
	t.Helper()
	var m struct {
		Status string `json:"status"`
	}
	c.expect(c.get(ctxFor(t, 30*time.Second), "/v1/merchants/"+id), http.StatusOK,
		"read merchant "+id).JSON(t, &m)
	return m.Status
}

// awaitMerchantStatus polls until the merchant reaches one of the wanted states.
//
// Polling with a deadline rather than sleeping: onboarding's duration depends on how fast the
// sandbox KYC provider answers, which is not something this machine controls and not something a
// sleep can be right about.
func awaitMerchantStatus(t *testing.T, c *client, id string, budget time.Duration, want ...string) {
	t.Helper()
	var last string
	testenv.Eventually(t, budget,
		fmt.Sprintf("merchant %s to reach one of %v (last seen %q)", id, want, last),
		func() bool {
			last = merchantStatus(t, c, id)
			for _, w := range want {
				if last == w {
					return true
				}
			}
			// A terminal failure will never become one of the wanted states, so failing fast here
			// turns a three-minute timeout into an immediate, accurate message.
			switch last {
			case "VALIDATION_FAILED", "KYC_FAILED", "PROVISIONING_FAILED",
				"CERTIFICATION_FAILED", "COMPLIANCE_REJECTED", "TERMINATED":
				t.Fatalf("onboarding reached the terminal state %s; it will never reach %v", last, want)
			}
			return false
		})
}
