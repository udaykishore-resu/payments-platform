//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// The HTTP client every e2e test drives the platform through.
//
// It is deliberately thin — net/http plus encoding/json — because the assertions here are about
// the wire contract, and a client that helpfully normalised a response would be hiding exactly
// what the tests exist to check. It does three things beyond a bare http.Client, and each earns
// its place:
//
//   - It sends an `Idempotency-Key` on every mutating request, because the API requires one and a
//     test that forgot would be exercising the rejection path rather than the behaviour.
//   - It captures the whole response — status, headers and body — before any assertion, so a
//     failure message can print what the server actually said instead of what a decoder made of it.
//   - It decodes `application/problem+json` into a typed error, so an unexpected failure reads as
//     "422 IDEMPOTENCY_KEY_REUSED" rather than as a JSON unmarshalling error forty lines away.

// client is one authenticated caller of the data plane.
type client struct {
	t       *testing.T
	baseURL string
	token   string
	http    *http.Client
}

// newClient builds a client or skips the test when the stack is not configured.
func newClient(t *testing.T) *client {
	t.Helper()
	base := testenv.BaseURL(t)
	token := testenv.AuthToken(t)
	return &client{
		t:       t,
		baseURL: base,
		token:   token,
		// A generous per-request timeout: the timeout scenarios below deliberately provoke an 8 s
		// gateway hold, and the response is still expected promptly *after* it. A client timeout
		// shorter than the server's would make every one of those tests fail on the client side and
		// tell the reader nothing about the platform.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// response is everything the server said.
type response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// JSON decodes the body into v, failing with the raw body when it cannot.
func (r response) JSON(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("response body is not the JSON this test expected (%d): %v\n%s",
			r.Status, err, string(r.Body))
	}
}

// Problem decodes an RFC 9457 problem document.
//
// Every error the platform returns is one of these (baseline §20.2), so a test that received an
// unexpected status can always say *which* error it was, by its stable code, rather than printing
// a status number and leaving the reader to guess.
func (r response) Problem(t *testing.T) problem {
	t.Helper()
	var p problem
	if err := json.Unmarshal(r.Body, &p); err != nil {
		t.Fatalf("a %d response is not problem+json: %v\n%s", r.Status, err, string(r.Body))
	}
	return p
}

type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	Category  string `json:"category"`
	Retryable bool   `json:"retryable"`
	TraceID   string `json:"traceId"`
	RequestID string `json:"requestId"`
}

func (p problem) String() string {
	return fmt.Sprintf("%d %s (%s, retryable=%v): %s", p.Status, p.Code, p.Category, p.Retryable, p.Detail)
}

// do issues one request.
func (c *client) do(ctx context.Context, method, path string, body any, headers map[string]string) response {
	c.t.Helper()

	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("encoding the request body: %v", err)
		}
		payload = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		c.t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		c.t.Fatalf("reading the response to %s %s: %v", method, path, err)
	}
	return response{Status: resp.StatusCode, Headers: resp.Header, Body: raw}
}

// post issues a mutating request with an idempotency key.
func (c *client) post(ctx context.Context, path, idempotencyKey string, body any) response {
	c.t.Helper()
	return c.do(ctx, http.MethodPost, path, body, map[string]string{
		"Idempotency-Key": idempotencyKey,
	})
}

// get issues a read.
func (c *client) get(ctx context.Context, path string) response {
	c.t.Helper()
	return c.do(ctx, http.MethodGet, path, nil, nil)
}

// expect fails unless the response carries the wanted status, printing the problem document when
// it does not.
func (c *client) expect(r response, want int, what string) response {
	c.t.Helper()
	if r.Status == want {
		return r
	}
	if strings.Contains(r.Headers.Get("Content-Type"), "problem+json") {
		c.t.Fatalf("%s: got %d, want %d — %s", what, r.Status, want, r.Problem(c.t))
	}
	c.t.Fatalf("%s: got %d, want %d\n%s", what, r.Status, want, string(r.Body))
	return r
}

// --- payloads --------------------------------------------------------------------------------

// payment is the subset of the API's Payment resource these tests assert on.
//
// A subset rather than the whole schema, deliberately: adding a field to a response is an additive,
// permitted change (baseline §13.1), and a fixture that decoded strictly would turn every such
// change into a red build. The fields below are the ones the API declares required.
type payment struct {
	ID            string  `json:"id"`
	MerchantID    string  `json:"merchantId"`
	State         string  `json:"state"`
	Amount        amount  `json:"amount"`
	Authorized    *amount `json:"authorizedAmount"`
	Captured      amount  `json:"capturedAmount"`
	Refunded      amount  `json:"refundedAmount"`
	PaymentMethod string  `json:"paymentMethod"`
	CaptureMode   string  `json:"captureMode"`
	ReconRequired bool    `json:"reconciliationRequired"`
	Version       int64   `json:"version"`
	// The Payment schema has no top-level gateway. The gateway that handled a payment is a
	// property of an attempt — a payment can have two attempts on two gateways — so read it from
	// Attempts, and `routingPlanId` for the plan that chose them.
	RoutingPlanID string `json:"routingPlanId"`
	NextAction    *struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"nextAction"`
	Attempts []attempt `json:"attempts"`
}

type attempt struct {
	ID        string `json:"id"`
	GatewayID string `json:"gatewayId"`
	Number    int    `json:"attemptNumber"`
	Outcome   string `json:"outcome"`
	Operation string `json:"operation"`
}

// amount is the platform's money shape: integer minor units, never a float or a decimal string
// (baseline §7 rule 5). The test decodes it as an int64 on purpose — a response that started
// sending `10.50` would fail to decode here, which is the correct outcome.
//
// The wire field is `amount`, not `minorUnits`: the OpenAPI `Money` schema is
// `{"amount": 1050, "currency": "USD"}` with `required: [amount, currency]` and
// `additionalProperties: false`, and `httpapi.Money` matches it. The Go field keeps the name
// MinorUnits because that is what the integer *is* — the tag is what the contract fixes.
type amount struct {
	MinorUnits int64  `json:"amount"`
	Currency   string `json:"currency"`
}

// createPaymentBody builds a request whose amount selects the simulator's behaviour.
//
// Every field here is fixed by `api/openapi/payments-platform.v1.yaml` and by
// `internal/transport/httpapi.CreatePaymentRequest`, and CreatePaymentRequest is
// `additionalProperties: false`, so an extra field is a 400 rather than a field the server
// ignores. The four things this shape gets right, each of which the API would otherwise reject:
//
//  1. `amount` is `{"amount": <minor units>, "currency": "EUR"}` — the `Money` schema's field is
//     `amount`, not `minorUnits`.
//  2. `paymentMethodReference` carries its `type` discriminator. The schema is a closed `oneOf`
//     over GATEWAY_TOKEN / NETWORK_TOKEN_REF / STORED_INSTRUMENT with `discriminator.propertyName:
//     type`; without it nothing selects a variant.
//  3. GATEWAY_TOKEN requires `gatewayCode` (a gateway token is only meaningful to its issuer), and
//     the display fields are `expiryMonth`/`expiryYear`, not `expMonth`/`expYear`. `brand` is the
//     uppercase enum.
//  4. There is no `customer` object and no `country` on the reference. The payer's country is
//     `payerCountry` at the top level.
func createPaymentBody(merchantID string, minorUnits int64, currency, captureMode string) map[string]any {
	return map[string]any{
		"merchantId":    merchantID,
		"amount":        map[string]any{"amount": minorUnits, "currency": currency},
		"paymentMethod": "CARD",
		"paymentMethodReference": map[string]any{
			"type":        "GATEWAY_TOKEN",
			"gatewayCode": simulatorGatewayCode,
			"token":       "tok_e2e_visa",
			"brand":       "VISA",
			"last4":       "4242",
			"expiryMonth": 12,
			"expiryYear":  time.Now().UTC().Year() + 2,
		},
		"captureMode":  captureMode,
		"payerCountry": "DE",
	}
}

// simulatorGatewayCode is the code the gateway simulator registers under
// (`internal/adapters/gateway/simulator.GatewayID`). A gateway token names the gateway that minted
// it, so this has to be the gateway the local stack actually routes to.
const simulatorGatewayCode = "simulator"

// --- polling ---------------------------------------------------------------------------------

// awaitState polls a payment until it reaches one of the wanted states or the budget expires.
//
// Polling with a deadline, never a sleep. A sleep is a bet that this machine is no slower than the
// one the test was written on, and it loses that bet on the day CI is busy — producing a failure
// that looks like a product bug and gets re-run away. This produces a message naming the state the
// payment was actually in, which is a bug report.
func (c *client) awaitState(ctx context.Context, id string, budget time.Duration, want ...string) payment {
	c.t.Helper()
	var last payment
	deadline := time.Now().Add(budget)
	for {
		r := c.get(ctx, "/v1/payments/"+id)
		if r.Status == http.StatusOK {
			r.JSON(c.t, &last)
			for _, w := range want {
				if last.State == w {
					return last
				}
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("payment %s is %s after %s, want one of %v", id, last.State, budget, want)
		}
		select {
		case <-ctx.Done():
			c.t.Fatalf("payment %s: %v while waiting for one of %v", id, ctx.Err(), want)
		case <-time.After(testenv.DefaultPollInterval):
		}
	}
}

// idempotencyKey mints a fresh key. Fresh per logical operation, and reused verbatim when a test
// is deliberately replaying one.
func idempotencyKey(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("e2e: cannot read randomness for an idempotency key: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// ctxFor returns a context bounded by the test's own budget.
func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return c
}

// merchant returns the merchant the e2e suite transacts against.
//
// It is taken from the environment rather than created per test. Creating one would mean driving a
// twelve-step onboarding workflow before every payment test, which would make a failure in step
// four look like a failure in the payment path — and the journey test below already covers
// onboarding end to end, once, where a failure in it means what it says.
func merchantID(t *testing.T) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("PP_TEST_MERCHANT_ID")); v != "" {
		return v
	}
	testenv.Skip(t, "PP_TEST_MERCHANT_ID",
		"an ACTIVE merchant in the tenant the token is scoped to",
		"Run TestMerchantJourneyFromRegistrationToSettlement first and export the merchant id it "+
			"prints, or export one created by scripts/dev-up.sh.")
	return ""
}

// awaitStateOrGiveUp polls like awaitState but returns the last payment it saw instead of failing.
//
// It exists for the flows whose trigger the simulator may not be configured to emit — a dispute
// above all. A test that cannot tell "the platform mishandled the notification" from "no
// notification was sent" must not assert either, and returning the last state lets the caller skip
// with a message that says which of the two it is.
func (c *client) awaitStateOrGiveUp(ctx context.Context, id string, budget time.Duration, want ...string) payment {
	c.t.Helper()
	var last payment
	deadline := time.Now().Add(budget)
	for {
		r := c.get(ctx, "/v1/payments/"+id)
		if r.Status == http.StatusOK {
			r.JSON(c.t, &last)
			for _, w := range want {
				if last.State == w {
					return last
				}
			}
		}
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(testenv.DefaultPollInterval):
		}
	}
}
