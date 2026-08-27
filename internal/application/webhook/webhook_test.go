package webhook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	dconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var testEpoch = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

const (
	testTenant   shared.TenantID   = "ten_01HZTESTTENANT00000000000"
	testMerchant shared.MerchantID = "mrc_01HZTESTMERCHANT000000000"
	testGateway  shared.GatewayID  = "gw-a"
)

// fakeVerifier records whether Verify ran and what it was given.
//
// The `parsed` counter is what makes "verify before parsing" an assertion rather than a comment:
// the double refuses to look at the body until Verify has been called, so an ingester that parsed
// first would fail the test rather than merely being wrong.
type fakeVerifier struct {
	mu       sync.Mutex
	event    *spi.WebhookEvent
	err      error
	verified int
	lastRaw  []byte
	secrets  []string
}

func (f *fakeVerifier) ID() shared.GatewayID { return testGateway }

func (f *fakeVerifier) Verify(_ context.Context, raw []byte, _ map[string]string,
	secrets []string, _ time.Time) (*spi.WebhookEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified++
	f.lastRaw = append([]byte(nil), raw...)
	f.secrets = secrets
	if f.err != nil {
		return nil, f.err
	}
	return f.event, nil
}

type verifierSource struct{ v *fakeVerifier }

func (s verifierSource) Verifier(context.Context, shared.GatewayID) (spi.WebhookVerifier, error) {
	return s.v, nil
}

type secretSource struct {
	secrets []string
	err     error
}

func (s secretSource) SigningSecrets(context.Context, shared.GatewayID) ([]string, error) {
	return s.secrets, s.err
}

type queue struct {
	mu  sync.Mutex
	ids []shared.WebhookID
	err error
}

func (q *queue) Enqueue(_ context.Context, id shared.WebhookID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.ids = append(q.ids, id)
	return nil
}

// fakeLedger counts postings per reference, which is how the "replaying an event must not
// double-post" assertion is made.
type fakeLedger struct {
	mu    sync.Mutex
	posts map[string]int
	err   error
}

func newFakeLedger() *fakeLedger { return &fakeLedger{posts: map[string]int{}} }

func (l *fakeLedger) Post(_ context.Context, _ ports.Repositories, e Effect) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.posts[e.Reference]++
	return nil
}

func (l *fakeLedger) count(ref string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.posts[ref]
}

type env struct {
	t        *testing.T
	store    *apptest.Store
	clock    *apptest.Clock
	verifier *fakeVerifier
	queue    *queue
	ledger   *fakeLedger
	ingest   *Ingester
	process  *Processor
}

func newEnv(t *testing.T, event *spi.WebhookEvent) *env {
	t.Helper()
	store := apptest.NewStore()
	clock := apptest.NewClock(testEpoch)
	v := &fakeVerifier{event: event}
	q := &queue{}
	l := newFakeLedger()
	uow := apptest.NewUnitOfWork(store, apptest.NewRecorder())
	src := secretSource{secrets: []string{"whsec_1"}}
	return &env{
		t: t, store: store, clock: clock, verifier: v, queue: q, ledger: l,
		ingest: NewIngester(IngestDeps{
			UoW: uow, Recorder: apptest.NewWebhookRecorder(store),
			Verifiers: verifierSource{v: v}, Secrets: src, Queue: q, Clock: clock,
		}),
		process: NewProcessor(ProcessDeps{
			UoW: uow, Verifiers: verifierSource{v: v}, Secrets: src, Ledger: l, Clock: clock,
		}),
	}
}

func request() InboundRequest {
	return InboundRequest{
		GatewayID: testGateway, TenantID: testTenant, MerchantID: testMerchant,
		Raw:     []byte(`{"id":"evt_1","type":"charge.captured"}`),
		Headers: map[string]string{"X-Sig": "abc"},
	}
}

// --- ingest ------------------------------------------------------------------------------------

// TestIngestVerifiesBeforeStoringAnything.
//
// A verification failure must leave no row, no queue entry and no parsed body. The first thing an
// unauthenticated request reaches must be a constant-time HMAC, not a decoder — decoders are
// where the interesting bugs are.
func TestIngestVerifiesBeforeStoringAnything(t *testing.T) {
	// Verifies: FR-74, FR-75.
	t.Parallel()
	e := newEnv(t, nil)
	e.verifier.err = errors.New("bad signature")

	_, err := e.ingest.Ingest(context.Background(), request())
	if err == nil {
		t.Fatal("a webhook with an invalid signature was accepted")
	}
	if apierror.CodeOf(err) != apierror.CodeWebhookSignatureInvalid {
		t.Fatalf("got %s, want WEBHOOK_SIGNATURE_INVALID", apierror.CodeOf(err))
	}
	if e.verifier.verified != 1 {
		t.Fatalf("the verifier ran %d times, want 1", e.verifier.verified)
	}
	if n := len(e.store.Webhooks); n != 0 {
		t.Fatalf("%d webhooks were stored despite a failed signature", n)
	}
	if n := len(e.queue.ids); n != 0 {
		t.Fatalf("%d webhooks were enqueued despite a failed signature", n)
	}
}

// TestIngestVerifiesOverTheRawBytes.
//
// An HMAC is computed over the octets the gateway sent. Any round trip through a decoder changes
// them — key order, whitespace, number formatting — so a signature verified against re-serialized
// JSON is a signature verified against something the gateway never signed.
func TestIngestVerifiesOverTheRawBytes(t *testing.T) {
	// Verifies: FR-75.
	t.Parallel()
	e := newEnv(t, &spi.WebhookEvent{GatewayEventID: "evt_1", Kind: spi.KindCaptureSucceeded})
	req := request()

	if _, err := e.ingest.Ingest(context.Background(), req); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !bytes.Equal(e.verifier.lastRaw, req.Raw) {
		t.Fatalf("the verifier saw %q, want the raw body %q", e.verifier.lastRaw, req.Raw)
	}
}

// TestIngestPassesEverySigningSecretSoARotationDoesNotDropDeliveries.
func TestIngestPassesEverySigningSecretSoARotationDoesNotDropDeliveries(t *testing.T) {
	t.Parallel()
	store := apptest.NewStore()
	clock := apptest.NewClock(testEpoch)
	v := &fakeVerifier{event: &spi.WebhookEvent{GatewayEventID: "evt_1"}}
	in := NewIngester(IngestDeps{
		UoW:       apptest.NewUnitOfWork(store, apptest.NewRecorder()),
		Recorder:  apptest.NewWebhookRecorder(store),
		Verifiers: verifierSource{v: v},
		Secrets:   secretSource{secrets: []string{"whsec_old", "whsec_new"}},
		Clock:     clock,
	})
	if _, err := in.Ingest(context.Background(), request()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(v.secrets) != 2 {
		t.Fatalf("the verifier was given %d secrets, want both sides of the rotation", len(v.secrets))
	}
}

// TestIngestDeduplicatesAtTheStorageLayer.
//
// The unique index *is* the check. An in-memory one would not survive a pod restart and would not
// work across replicas, and a webhook processed twice moves money twice.
func TestIngestDeduplicatesAtTheStorageLayer(t *testing.T) {
	// Verifies: FR-77.
	t.Parallel()
	e := newEnv(t, &spi.WebhookEvent{GatewayEventID: "evt_1", Kind: spi.KindCaptureSucceeded})

	first, err := e.ingest.Ingest(context.Background(), request())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if first.Duplicate {
		t.Fatal("the first delivery was reported as a duplicate")
	}
	second, err := e.ingest.Ingest(context.Background(), request())
	if err != nil {
		t.Fatalf("a retried delivery was rejected; the gateway would retry it again: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("the retry was not recognised as a duplicate")
	}
	if n := len(e.store.Webhooks); n != 1 {
		t.Fatalf("%d webhook rows for one event, want 1", n)
	}
	if n := len(e.queue.ids); n != 1 {
		t.Fatalf("the duplicate was enqueued again (%d entries)", n)
	}
}

// TestIngestRefusesAWebhookThatCannotBeDeduplicated.
//
// Without a deduplication key there is no way to recognise a retry, and every retry becomes a
// second application of the same state change.
func TestIngestRefusesAWebhookThatCannotBeDeduplicated(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &spi.WebhookEvent{GatewayEventID: "", Kind: spi.KindCaptureSucceeded})
	if _, err := e.ingest.Ingest(context.Background(), request()); err == nil {
		t.Fatal("a webhook with no event identifier was accepted")
	}
}

// TestIngestSurvivesAQueueFailure.
//
// The durable record is the row; the queue is a latency optimisation over the sweep. Failing the
// accept because the broker is down would make the gateway retry a webhook we are already holding.
func TestIngestSurvivesAQueueFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &spi.WebhookEvent{GatewayEventID: "evt_1", Kind: spi.KindCaptureSucceeded})
	e.queue.err = errors.New("broker unavailable")

	if _, err := e.ingest.Ingest(context.Background(), request()); err != nil {
		t.Fatalf("a queue failure failed the accept: %v", err)
	}
	if n := len(e.store.Webhooks); n != 1 {
		t.Fatalf("%d webhook rows, want 1", n)
	}
	unprocessed, err := e.ingest.ClaimUnprocessed(context.Background(), 10)
	if err != nil {
		t.Fatalf("ClaimUnprocessed: %v", err)
	}
	if len(unprocessed) != 1 {
		t.Fatalf("the sweep found %d unprocessed webhooks, want 1", len(unprocessed))
	}
}

// --- process ------------------------------------------------------------------------------------

func processingPayment(t *testing.T, e *env) *dpayment.Payment {
	t.Helper()
	p, err := dpayment.New(dpayment.NewPaymentParams{
		TenantID: testTenant, MerchantID: testMerchant, Amount: money.MustNew(8450, "EUR"),
		PaymentMethod:  shared.MethodCard,
		MethodRef:      dpayment.PaymentMethodReference{Token: "tok"},
		IdempotencyKey: "k",
	}, e.clock)
	if err != nil {
		t.Fatalf("payment.New: %v", err)
	}
	if err := p.MarkProcessing(e.clock); err != nil {
		t.Fatalf("MarkProcessing: %v", err)
	}
	e.store.PutPayment(p)
	return p
}

// TestProcessAppliesTheTransitionAndPostsTheLedgerInOneTransaction.
func TestProcessAppliesTheTransitionAndPostsTheLedger(t *testing.T) {
	// Verifies: FR-78.
	t.Parallel()
	e := newEnv(t, nil)
	p := processingPayment(t, e)
	amount := money.MustNew(8450, "EUR")
	e.verifier.event = &spi.WebhookEvent{
		GatewayEventID: "evt_cap", Kind: spi.KindCaptureSucceeded,
		PaymentID: p.ID(), GatewayRef: "gwref", Amount: &amount, OccurredAt: testEpoch,
	}

	acc, err := e.ingest.Ingest(context.Background(), request())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	res, err := e.process.Process(context.Background(), acc.WebhookID)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Applied {
		t.Fatalf("the webhook was not applied: %s", res.Outcome)
	}
	if got := e.store.Payment(p.ID()); got.State() != dpayment.StateCaptured {
		t.Fatalf("state = %s, want CAPTURED", got.State())
	}
	if n := e.ledger.count("evt_cap"); n != 1 {
		t.Fatalf("the ledger was posted %d times, want 1", n)
	}
	if w := e.store.Webhooks[acc.WebhookID]; w.ProcessedAt == nil {
		t.Fatal("the webhook was not marked processed")
	}
}

// TestReplayingAWebhookDoesNotDoublePost.
//
// At-least-once delivery is the contract; effectively-once business semantics is the requirement.
// The webhook row's processed marker and the ledger's own idempotency on the gateway event
// identifier are the two halves of that.
func TestReplayingAWebhookDoesNotDoublePost(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	p := processingPayment(t, e)
	amount := money.MustNew(8450, "EUR")
	e.verifier.event = &spi.WebhookEvent{
		GatewayEventID: "evt_cap", Kind: spi.KindCaptureSucceeded,
		PaymentID: p.ID(), GatewayRef: "gwref", Amount: &amount,
	}

	acc, err := e.ingest.Ingest(context.Background(), request())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.process.Process(context.Background(), acc.WebhookID); err != nil {
			t.Fatalf("Process %d: %v", i, err)
		}
	}
	if n := e.ledger.count("evt_cap"); n != 1 {
		t.Fatalf("the ledger was posted %d times across three processings, want 1", n)
	}
}

// TestOutOfOrderWebhookIsANoOpNotAnError.
//
// Gateways retry, deliveries arrive out of order, and a captured payment receiving a second
// capture notification is the normal case. Treating it as an error would put a healthy platform's
// most common event on the failure dashboard.
func TestOutOfOrderWebhookIsANoOpNotAnError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	p := processingPayment(t, e)
	if err := p.MarkCaptured(money.MustNew(8450, "EUR"), e.clock); err != nil {
		t.Fatalf("MarkCaptured: %v", err)
	}
	e.store.PutPayment(p)

	amount := money.MustNew(8450, "EUR")
	e.verifier.event = &spi.WebhookEvent{
		GatewayEventID: "evt_auth_late", Kind: spi.KindAuthorizationSucceeded,
		PaymentID: p.ID(), GatewayRef: "gwref", Amount: &amount,
	}
	acc, err := e.ingest.Ingest(context.Background(), request())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	res, err := e.process.Process(context.Background(), acc.WebhookID)
	if err != nil {
		t.Fatalf("an out-of-order webhook was reported as an error: %v", err)
	}
	if res.Applied {
		t.Fatal("a late authorization webhook moved an already-captured payment")
	}
	if res.Outcome != "NO_OP" {
		t.Fatalf("outcome = %q, want NO_OP", res.Outcome)
	}
	if got := e.store.Payment(p.ID()); got.State() != dpayment.StateCaptured {
		t.Fatalf("state = %s, want it left at CAPTURED", got.State())
	}
}

// TestWebhookForAnUnknownPaymentOpensAReconciliationException.
//
// This is the most alarming thing the processor can see: money moved for something the platform
// did not think existed. Dropping it makes that invisible.
func TestWebhookForAnUnknownPaymentOpensAReconciliationException(t *testing.T) {
	// Verifies: FR-79.
	t.Parallel()
	e := newEnv(t, nil)
	e.verifier.event = &spi.WebhookEvent{
		GatewayEventID: "evt_orphan", Kind: spi.KindCaptureSucceeded,
		PaymentID: shared.PaymentID("pay_01HZNOTOURSPAYMENT000000"), GatewayRef: "gwref",
	}

	acc, err := e.ingest.Ingest(context.Background(), request())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	res, err := e.process.Process(context.Background(), acc.WebhookID)
	if err != nil {
		// An error would roll back the transaction the exception was written in, erasing the only
		// record of the thing that alarmed us, and would leave the webhook to be parked again by
		// every subsequent sweep.
		t.Fatalf("parking a webhook was reported as a failure: %v", err)
	}
	if res.Applied {
		t.Fatal("a webhook for an unknown payment was applied")
	}
	if res.Outcome != "PARKED" {
		t.Fatalf("outcome = %q, want PARKED", res.Outcome)
	}
	ex := e.store.OpenExceptions()
	if len(ex) != 1 {
		t.Fatalf("got %d reconciliation exceptions, want 1", len(ex))
	}
	if ex[0].Kind != "WEBHOOK_FOR_UNKNOWN_PAYMENT" {
		t.Fatalf("exception kind = %q", ex[0].Kind)
	}
}

// TestUnmodelledEventKindIsIgnoredNotFailed.
//
// Gateways send far more event types than the platform models. Erroring on one turns a vendor's
// feature launch into an incident on our side.
func TestUnmodelledEventKindIsIgnoredNotFailed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &spi.WebhookEvent{GatewayEventID: "evt_x", Kind: spi.KindIgnored})
	acc, err := e.ingest.Ingest(context.Background(), request())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	res, err := e.process.Process(context.Background(), acc.WebhookID)
	if err != nil {
		t.Fatalf("an unmodelled event kind failed processing: %v", err)
	}
	if res.Outcome != "IGNORED" {
		t.Fatalf("outcome = %q, want IGNORED", res.Outcome)
	}
}

// TestProcessReVerifiesTheStoredPayload.
//
// The row is read minutes later, possibly by a different process. The only thing that makes its
// payload trustworthy is the signature, and a processor that trusted a database row would trust
// whatever an operator had edited into it.
func TestProcessReVerifiesTheStoredPayload(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	p := processingPayment(t, e)
	e.verifier.event = &spi.WebhookEvent{
		GatewayEventID: "evt_cap", Kind: spi.KindCaptureSucceeded, PaymentID: p.ID(),
	}
	acc, err := e.ingest.Ingest(context.Background(), request())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	before := e.verifier.verified

	e.verifier.err = errors.New("no longer verifies")
	if _, err := e.process.Process(context.Background(), acc.WebhookID); err == nil {
		t.Fatal("a stored webhook that no longer verifies was applied")
	}
	if e.verifier.verified <= before {
		t.Fatal("the processor did not re-verify the stored payload")
	}
}

// --- outbound delivery ---------------------------------------------------------------------------

// TestSignatureCoversTheTimestampAndTheBody.
//
// The timestamp is inside the signed material rather than merely alongside it, and that is the
// whole replay defence: a captured delivery replayed an hour later still carries its original
// timestamp, and an attacker who updates it invalidates the signature.
func TestSignatureCoversTheTimestampAndTheBody(t *testing.T) {
	// Verifies: FR-76.
	t.Parallel()
	body := []byte(`{"type":"payment.captured.v1"}`)
	sig := Sign("whsec", 1000, body)

	if !Verify("whsec", 1000, body, sig) {
		t.Fatal("a signature did not verify against its own inputs")
	}
	if Verify("whsec", 1001, body, sig) {
		t.Fatal("changing the timestamp did not invalidate the signature")
	}
	if Verify("whsec", 1000, []byte(`{"type":"payment.refunded.v1"}`), sig) {
		t.Fatal("changing the body did not invalidate the signature")
	}
	if Verify("other", 1000, body, sig) {
		t.Fatal("a different secret produced the same signature")
	}
}

// TestEventGlobMatching.
func TestEventGlobMatching(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern, event string
		want           bool
	}{
		{"*", "payment.captured.v1", true},
		{"payment.*", "payment.captured.v1", true},
		{"payment.*", "merchant.activated.v1", false},
		{"payment.captured.v1", "payment.captured.v1", true},
		{"payment.captured.v1", "payment.refunded.v1", false},
		{"merchant.*", "merchant.suspended.v1", true},
		{"", "payment.captured.v1", false},
	}
	for _, tc := range tests {
		if got := Matches(tc.pattern, tc.event); got != tc.want {
			t.Fatalf("Matches(%q, %q) = %v, want %v", tc.pattern, tc.event, got, tc.want)
		}
	}
}

// TestDeliverOnlySendsToEndpointsThatSubscribed.
func TestDeliverOnlySendsToEndpointsThatSubscribed(t *testing.T) {
	t.Parallel()
	http := &recordingDoer{status: 200}
	d := deliverer(http)

	out := Outbound{
		EventID: "evt_1", EventType: "payment.captured.v1", Payload: []byte(`{}`),
		Endpoints: []dconfig.WebhookEndpoint{
			{URL: "https://a.example/hook", Events: []string{"payment.*"}, SecretRef: "secret://a", Active: true},
			{URL: "https://b.example/hook", Events: []string{"merchant.*"}, SecretRef: "secret://a", Active: true},
			{URL: "https://c.example/hook", Events: []string{"*"}, SecretRef: "secret://a", Active: false},
		},
	}
	results, err := d.Deliver(context.Background(), out)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("delivered to %d endpoints, want 1", len(results))
	}
	if results[0].URL != "https://a.example/hook" {
		t.Fatalf("delivered to %s", results[0].URL)
	}
	if !results[0].Delivered {
		t.Fatalf("the delivery failed: %v", results[0].Err)
	}
	if got := http.requests[0].Header.Get(HeaderSignature); got == "" {
		t.Fatal("the delivery carried no signature")
	}
}

// TestDeliverRetriesTransientFailuresAndGivesUpOnAClientError.
//
// A 4xx other than 408 and 429 is the endpoint telling us the request is wrong; retrying an
// unchanged request against it is pure load on an endpoint that has already answered.
func TestDeliverRetriesTransientFailuresAndGivesUpOnAClientError(t *testing.T) {
	t.Parallel()
	t.Run("a 5xx is retried until it succeeds", func(t *testing.T) {
		t.Parallel()
		doer := &recordingDoer{statuses: []int{500, 503, 200}}
		res, err := deliverer(doer).Deliver(context.Background(), outbound())
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if !res[0].Delivered {
			t.Fatalf("the delivery never succeeded: %v", res[0].Err)
		}
		if res[0].Attempts != 3 {
			t.Fatalf("attempts = %d, want 3", res[0].Attempts)
		}
	})
	t.Run("a 400 is not retried", func(t *testing.T) {
		t.Parallel()
		doer := &recordingDoer{status: 400}
		res, err := deliverer(doer).Deliver(context.Background(), outbound())
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if res[0].Attempts != 1 {
			t.Fatalf("a 400 was retried %d times", res[0].Attempts-1)
		}
	})
	t.Run("a 429 is retried", func(t *testing.T) {
		t.Parallel()
		doer := &recordingDoer{statuses: []int{429, 200}}
		res, err := deliverer(doer).Deliver(context.Background(), outbound())
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if !res[0].Delivered || res[0].Attempts != 2 {
			t.Fatalf("a 429 was not retried: delivered=%v attempts=%d", res[0].Delivered, res[0].Attempts)
		}
	})
}

// TestDeliverRefusesAnEndpointWithNoSigningSecret.
//
// An unsigned outbound webhook cannot be authenticated by the merchant, which makes it an
// instruction anyone who learns the URL can forge.
func TestDeliverRefusesAnEndpointWithNoSigningSecret(t *testing.T) {
	t.Parallel()
	doer := &recordingDoer{status: 200}
	out := outbound()
	out.Endpoints[0].SecretRef = ""

	res, err := deliverer(doer).Deliver(context.Background(), out)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res[0].Delivered {
		t.Fatal("an unsigned webhook was delivered")
	}
	if len(doer.requests) != 0 {
		t.Fatal("an unsigned webhook reached the network")
	}
}

// TestDialTimeGuardCatchesDNSRebinding.
//
// This is the layer that actually closes the SSRF hole. The configuration-time check rejects a
// bad URL when it is saved; it cannot see a hostname that resolved to a public address then and
// resolves to the cloud metadata endpoint now. Only a check after resolution and before connect
// sees the address that is about to be dialled.
func TestDialTimeGuardCatchesDNSRebinding(t *testing.T) {
	t.Parallel()
	blocked := []string{
		"169.254.169.254:80",          // cloud metadata, the classic target
		"127.0.0.1:8080",              // loopback: sidecars
		"10.0.0.5:443",                // private
		"172.16.4.4:443",              // private
		"192.168.1.1:443",             // private
		"100.64.0.1:443",              // carrier-grade NAT
		"[::1]:443",                   // loopback, v6
		"[fd00::1]:443",               // unique local, v6
		"[::ffff:169.254.169.254]:80", // IPv4-mapped: the classic bypass
		"0.0.0.0:80",
	}
	for _, addr := range blocked {
		if err := GuardAddress("tcp", addr); err == nil {
			t.Fatalf("the dial guard permitted %s", addr)
		}
	}
	if err := GuardAddress("tcp", "93.184.216.34:443"); err != nil {
		t.Fatalf("the dial guard blocked a public address: %v", err)
	}
	// A non-TCP network is not a webhook delivery.
	if err := GuardAddress("udp", "93.184.216.34:443"); err == nil {
		t.Fatal("the dial guard permitted a non-TCP dial")
	}
	// An unresolved host reaching the Control hook means something upstream did not resolve.
	if err := GuardAddress("tcp", "metadata.internal:80"); err == nil {
		t.Fatal("the dial guard permitted an unresolved host")
	}
}

// TestGuardedTransportInstallsTheHook is a wiring check: a transport built without the Control
// hook would pass every test above and still be vulnerable.
func TestGuardedTransportInstallsTheHook(t *testing.T) {
	t.Parallel()
	tr := NewGuardedTransport(nil)
	if tr.DialContext == nil {
		t.Fatal("the guarded transport has no DialContext; the guard is not installed")
	}
	_, err := tr.DialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil {
		t.Fatal("the guarded transport dialled the metadata endpoint")
	}
}

func outbound() Outbound {
	return Outbound{
		EventID: "evt_1", EventType: "payment.captured.v1", Payload: []byte(`{}`),
		Endpoints: []dconfig.WebhookEndpoint{
			{URL: "https://a.example/hook", Events: []string{"*"}, SecretRef: "secret://a", Active: true},
		},
	}
}

func deliverer(doer Doer) *Deliverer {
	secrets := apptest.NewSecrets()
	secrets.Seed("secret://a", map[string]string{"signingSecret": "whsec_test"})
	return NewDeliverer(DeliverDeps{
		HTTP: doer, Clock: apptest.NewClock(testEpoch), Secrets: secrets,
		// Milliseconds rather than seconds: the ladder's *shape* is under test, not its wall time,
		// and a test that waits out a production backoff is a test nobody runs.
		InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
		MaxAttempts: 4, Timeout: time.Second,
	})
}

// recordingDoer answers with a scripted status sequence and keeps the requests.
type recordingDoer struct {
	mu       sync.Mutex
	status   int
	statuses []int
	requests []*http.Request
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, req)
	status := d.status
	if len(d.statuses) > 0 {
		status = d.statuses[0]
		if len(d.statuses) > 1 {
			d.statuses = d.statuses[1:]
		}
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}
