//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// The double-charge test.
//
// Verifies: baseline §12.3, ADR-013, critical path CP-02, docs/testing.md §6.3 C-1 and §7 FS-1.
//
// This is the test the platform exists to pass. Everything else in this suite checks that the
// system does what it should when things work; this one checks the single case where doing the
// obvious thing loses somebody's money.
//
// The gateway accepts the request and then never answers. From inside the process that is
// indistinguishable from "the gateway refused" — and the two require opposite responses. Refused
// means retry or fail over. Accepted-then-silent means the card may already be authorized, and
// both retrying *and* failing over charge it twice.
//
// The scenario is selected by the amount: `…07` is the simulator's documented timeout trigger, and
// over HTTP it holds the connection until the client's deadline expires — the same failure a real
// gateway produces, through a real socket, which is the only way to exercise a genuine client
// timeout at all.

// TestATimedOutPaymentIsNeverChargedTwice is CP-02 and FS-1.
func TestATimedOutPaymentIsNeverChargedTwice(t *testing.T) {
	// Not parallel. It asserts the *total* number of charges the simulator holds for one
	// idempotency key, and a neighbouring test transacting against the same simulator would be
	// noise in the only number that matters here.
	c := newClient(t)
	sim := newSimulator(t)
	merchant := merchantID(t)
	ctx := ctxFor(t, 3*time.Minute)

	// …07: accepted, then silence.
	const timeoutAmount = 15_007
	key := idempotencyKey("timeout")

	created := c.post(ctx, "/v1/payments", key,
		createPaymentBody(merchant, timeoutAmount, "EUR", "AUTOMATIC"))

	// 1. The caller is told the payment is processing — not that it failed.
	//
	// A 5xx here would be the worst possible answer: the client would retry with a *new* key,
	// which is a second payment, and the API documentation says in as many words never to do that.
	// A 4xx would be worse still, because a client is entitled to treat it as final.
	switch created.Status {
	case http.StatusAccepted, http.StatusCreated:
	default:
		t.Fatalf("a gateway timeout produced %d — %s.\n"+
			"§12.3: the response is 202 with state PROCESSING. An error here tells the client the "+
			"payment did not happen, and the client then makes a second one.",
			created.Status, created.Problem(t))
	}

	var p payment
	created.JSON(t, &p)
	if p.State != "PROCESSING" {
		t.Fatalf("payment %s is %s immediately after a gateway timeout, want PROCESSING", p.ID, p.State)
	}

	// 2. The attempt records TIMEOUT_UNKNOWN, and the payment is flagged for reconciliation.
	//
	// TIMEOUT_UNKNOWN is not a failure and is not terminal. It is the platform saying "we do not
	// know", which is the only honest representation of the situation and the only one from which
	// the right thing can be done next.
	if len(p.Attempts) != 1 {
		t.Fatalf("%d attempts on a timed-out payment, want exactly 1 — %s", len(p.Attempts), p.ID)
	}
	if p.Attempts[0].Outcome != "TIMEOUT_UNKNOWN" {
		t.Fatalf("the attempt on a timed-out payment is %s, want TIMEOUT_UNKNOWN", p.Attempts[0].Outcome)
	}
	if !p.ReconRequired {
		t.Fatal("a payment with an unknown outcome is not flagged for reconciliation. Nothing will " +
			"come looking for it, and it stays PROCESSING until a human notices.")
	}

	// 3. No retry, and no failover. This is the assertion the test exists for.
	//
	// It is made at the *simulator* rather than at the platform, because the number that decides
	// whether the cardholder was charged twice is the number of requests the vendor received. A
	// platform-side count of attempts would be one layer too early: an attempt that was dispatched
	// twice would still be one attempt row.
	charges := sim.chargesFor(ctx, p.ID)
	if charges > 1 {
		t.Fatalf("the simulator holds %d charges for payment %s. The gateway timed out; every "+
			"charge after the first is money taken from a cardholder for one purchase.", charges, p.ID)
	}

	// The platform must also not be *about* to dispatch again. Sampling for a window is what
	// distinguishes "it has not retried yet" from "it will not retry": a retry scheduled for two
	// seconds' time would pass an immediate check and fail the customer.
	testenv.Consistently(t, 10*time.Second,
		fmt.Sprintf("payment %s to remain at one attempt and one charge", p.ID),
		func() bool {
			current := c.get(ctx, "/v1/payments/"+p.ID)
			if current.Status != http.StatusOK {
				return false
			}
			var now payment
			current.JSON(t, &now)
			return len(now.Attempts) <= 1 && sim.chargesFor(ctx, p.ID) <= 1
		})

	// 4. The reconciler resolves it from the gateway's own record.
	//
	// This is what makes "never retry" affordable. The attempt carries a gateway idempotency key
	// that is a pure function of the attempt id, so the reconciler can reproduce it after any
	// crash and ask the gateway what happened — no webhook required, no state held in a process
	// that may be gone.
	resolved := c.awaitState(ctx, p.ID, 2*time.Minute, "CAPTURED", "AUTHORIZED", "FAILED")
	if resolved.State == "FAILED" {
		t.Fatalf("the reconciler resolved an unknown outcome to FAILED. A timeout is not a failure; "+
			"resolving it as one tells the merchant a payment did not happen that may have. "+
			"(payment %s)", p.ID)
	}

	// 5. Still exactly one charge, after resolution.
	if charges := sim.chargesFor(ctx, p.ID); charges != 1 {
		t.Fatalf("the simulator holds %d charges for payment %s after reconciliation, want exactly 1",
			charges, p.ID)
	}

	// 6. A client that retries with the *same* key gets the same payment, not a second one.
	//
	// This is the documented recovery: "Never re-issue a timed-out payment with a new idempotency
	// key. Retry with the same key, or poll."
	replay := c.post(ctx, "/v1/payments", key,
		createPaymentBody(merchant, timeoutAmount, "EUR", "AUTOMATIC"))
	switch replay.Status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
		var again payment
		replay.JSON(t, &again)
		if again.ID != p.ID {
			t.Fatalf("retrying with the same idempotency key created payment %s alongside %s",
				again.ID, p.ID)
		}
	case http.StatusConflict:
		// IDEMPOTENT_REQUEST_IN_PROGRESS is also correct while a lease is live (§1.3 A6): the
		// caller is told to come back rather than being blocked on someone else's lease.
		if code := replay.Problem(t).Code; code != "IDEMPOTENT_REQUEST_IN_PROGRESS" {
			t.Fatalf("a same-key retry returned 409 %s, want IDEMPOTENT_REQUEST_IN_PROGRESS", code)
		}
	default:
		t.Fatalf("a same-key retry after a timeout returned %d — %s", replay.Status, replay.Problem(t))
	}

	if charges := sim.chargesFor(ctx, p.ID); charges != 1 {
		t.Fatalf("the simulator holds %d charges for payment %s after the client's retry, want 1",
			charges, p.ID)
	}
}

// --- the simulator's own protocol ----------------------------------------------------------------

// simulator is a read-only client of the gateway simulator.
//
// The tests use it for exactly one thing: counting what the *vendor* received. Every other
// assertion in this suite is made through the platform's public API, and this is the one place
// where looking at the far side is the whole point — "was the card charged twice" is not a
// question the platform can be trusted to answer about itself.
type simulator struct {
	t       *testing.T
	baseURL string
	http    *http.Client
}

func newSimulator(t *testing.T) *simulator {
	t.Helper()
	return &simulator{
		t:       t,
		baseURL: testenv.SimulatorURL(t),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// chargesFor counts the transactions the simulator holds for a payment.
//
// It looks the payment up by its own reference through the simulator's lookup endpoint, which is
// the same endpoint the reconciler uses — the simulator deliberately supports lookup by
// idempotency key alone, because an adapter that could only look up by the gateway's reference
// would be useless after exactly the failure this test is about.
func (s *simulator) chargesFor(ctx context.Context, paymentID string) int {
	s.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.baseURL+"/sim/v1/payments?reference="+paymentID, nil)
	if err != nil {
		s.t.Fatalf("building the simulator lookup: %v", err)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		s.t.Fatalf("simulator lookup for %s: %v", paymentID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusNotFound:
		return 0
	case http.StatusOK:
	default:
		s.t.Fatalf("simulator lookup for %s returned %d: %s", paymentID, resp.StatusCode, body)
	}

	// The lookup answers with either one transaction or a list of them, depending on how the
	// simulator was asked. Both shapes are decoded rather than one being assumed, because a test
	// that mis-decoded the answer would report zero charges — the reassuring number — for a
	// payment that had been charged twice.
	var one struct {
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(body, &one); err == nil && one.Reference != "" {
		return 1
	}
	var many []struct {
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(body, &many); err == nil {
		return len(many)
	}
	s.t.Fatalf("the simulator's answer for %s is neither a transaction nor a list: %s", paymentID, body)
	return 0
}
