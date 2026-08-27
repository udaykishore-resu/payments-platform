// Package webhook holds the three halves of the platform's webhook surface: the inbound accept
// path, the asynchronous processor that applies what a gateway told us, and the outbound
// deliverer that tells merchants what happened.
//
// The three are one package because they share one hard-won shape — **store first, process
// later, always** — and separating them would let that shape drift. An endpoint that processes
// synchronously is an endpoint that times out under load, which makes the gateway retry, which
// multiplies the load exactly when it is highest. The ≤50 ms budget on Ingest buys the platform
// the right to be slow at processing without being slow at accepting.
package webhook

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// VerifierSource resolves a gateway slug to the adapter that can authenticate its webhooks.
//
// It is separate from the payment adapter, and the separation has a security consequence rather
// than an aesthetic one: the webhook ingress is the most exposed surface in the platform, and the
// blast radius of compromising it must not include the ability to initiate payments.
type VerifierSource interface {
	Verifier(ctx context.Context, g shared.GatewayID) (spi.WebhookVerifier, error)
}

// SecretSource supplies the signing secrets for a gateway's webhooks.
//
// It returns a *slice* because a rotation has an overlap window: during it, both the old and the
// new secret must verify, or every webhook delivered mid-rotation is rejected and the gateway
// eventually gives up retrying them.
type SecretSource interface {
	SigningSecrets(ctx context.Context, g shared.GatewayID) ([]string, error)
}

// Queue hands an accepted webhook to the asynchronous processor.
//
// Enqueueing is best-effort by design: the durable record is the database row, and the queue is
// only a latency optimisation over the claim-unprocessed sweep. A queue failure must not fail the
// accept — the gateway would retry a webhook we have already stored, and the retry would
// deduplicate against it, which is wasted work at both ends.
type Queue interface {
	Enqueue(ctx context.Context, id shared.WebhookID) error
}

// IngestDeps is what the accept path needs.
//
// Recorder and UoW are both present and they are not redundant. The accept path writes through
// Recorder because a delivery's tenant is not knowable at accept time (see ports.WebhookRecorder);
// the sweep in ClaimUnprocessed runs under the processor's tenant and uses UoW like everything
// else. Keeping them as separate fields is what stops the untenanted write from quietly becoming
// available to the rest of the application layer.
type IngestDeps struct {
	UoW       ports.UnitOfWork
	Recorder  ports.WebhookRecorder
	Verifiers VerifierSource
	Secrets   SecretSource
	Queue     Queue
	Clock     shared.Clock
}

// Ingester is the ≤50 ms accept path.
type Ingester struct {
	deps IngestDeps
}

// NewIngester constructs the accept path.
func NewIngester(d IngestDeps) *Ingester {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	return &Ingester{deps: d}
}

// InboundRequest is one raw delivery, as the transport handed it over.
//
// Raw is the bytes exactly as received. Not a parsed body, not a re-serialized one: an HMAC is
// computed over the octets a gateway sent, and any round trip through a decoder changes them —
// key order, whitespace, number formatting — so a signature verified against re-serialized JSON
// is a signature verified against something the gateway never signed.
type InboundRequest struct {
	GatewayID shared.GatewayID
	Raw       []byte
	Headers   map[string]string
	// TenantID and MerchantID are populated where the ingress can resolve them from the URL. They
	// are not required: a webhook whose merchant we cannot identify is still stored, because the
	// processor can resolve it from the gateway reference and because dropping it would lose the
	// only notification of something that happened to money.
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
}

// Accepted is what the accept path returns to the transport.
type Accepted struct {
	WebhookID shared.WebhookID
	// Duplicate is true when the (gateway, event id) pair had already been recorded. It is a
	// *success*: gateways retry, and the correct answer to a retry of something we already have
	// is 200, not an error that makes them retry again.
	Duplicate bool
	Kind      spi.WebhookKind
}

// Ingest verifies, deduplicates, stores and enqueues. Nothing else happens synchronously.
//
// The order is the security property, and it is not negotiable:
//
//  1. **Verify before parsing.** The signature is checked over the raw bytes, by the adapter, in
//     constant time, before a parser touches attacker-controlled input. Reversing these means the
//     first thing an unauthenticated request reaches is a decoder — which is where the
//     interesting bugs are.
//  2. **Deduplicate at the storage layer.** The unique index on (gateway, event id) *is* the
//     check. Doing it in memory would not survive a pod restart and would not work across
//     replicas, and a webhook processed twice moves money twice.
//  3. **Enqueue after the commit.** A queue entry for a row that does not exist is a poison
//     message; a row with no queue entry is picked up by the sweep a few seconds later.
func (i *Ingester) Ingest(ctx context.Context, req InboundRequest) (*Accepted, error) {
	if req.GatewayID.IsZero() {
		return nil, apierror.New(apierror.CodeValidationFailed, "a webhook must name its gateway")
	}

	verifier, err := i.deps.Verifiers.Verifier(ctx, req.GatewayID)
	if err != nil {
		return nil, err
	}
	secrets, err := i.deps.Secrets.SigningSecrets(ctx, req.GatewayID)
	if err != nil {
		// A secret we cannot read is not a signature we can reject: refusing here is correct, but
		// it is an infrastructure failure rather than an authentication one, and the two page
		// different people.
		return nil, apierror.Wrapf(err, apierror.CodeDependencyFailure,
			"could not resolve the signing secrets for gateway %s", req.GatewayID)
	}

	event, err := verifier.Verify(ctx, req.Raw, req.Headers, secrets, i.deps.Clock.Now())
	if err != nil {
		// Deliberately uninformative to the caller. A verification failure that explains *why*
		// turns the endpoint into an oracle for constructing a valid signature.
		return nil, apierror.Wrap(err, apierror.CodeWebhookSignatureInvalid,
			"the webhook signature could not be verified")
	}
	if event == nil {
		return nil, apierror.New(apierror.CodeWebhookSignatureInvalid,
			"the verifier returned neither an event nor an error")
	}
	if event.GatewayEventID == "" {
		// Without a deduplication key there is no way to recognise a retry, and every retry
		// becomes a second application of the same state change.
		return nil, apierror.New(apierror.CodeWebhookUnknownEventType,
			"the webhook carries no gateway event identifier and cannot be deduplicated")
	}

	record := ports.InboundWebhook{
		ID:             shared.WebhookID(ids.NewAt(ids.PrefixInboundWebhook, i.deps.Clock.Now())),
		GatewayID:      req.GatewayID,
		TenantID:       req.TenantID,
		MerchantID:     req.MerchantID,
		GatewayEventID: event.GatewayEventID,
		EventType:      event.EventType,
		Payload:        req.Raw,
		Headers:        req.Headers,
		ReceivedAt:     i.deps.Clock.Now(),
		Status:         "RECEIVED",
	}

	// Written through the recorder rather than a unit of work: at this point the delivery's
	// tenant is genuinely unknown, and the unit of work will not open a transaction without one.
	// The row is stored with a NULL tenant, which is the one case the inbound-webhook RLS policy
	// admits, and the processor fills the tenant in when the payload resolves to a payment.
	if i.deps.Recorder == nil {
		return nil, apierror.New(apierror.CodeInternalError,
			"the webhook ingester has no recorder; the delivery would be verified and then dropped")
	}
	stored, err := i.deps.Recorder.Record(ctx, record)
	if err != nil {
		return nil, err
	}
	if !stored {
		// A replay. Answering 200 is deliberate: the gateway has already been told once that we
		// have it, and returning an error would make them retry a delivery we are holding.
		return &Accepted{WebhookID: record.ID, Duplicate: true, Kind: event.Kind}, nil
	}

	if i.deps.Queue != nil {
		// Best-effort, after the commit. The sweep is the guarantee; this is the latency.
		_ = i.deps.Queue.Enqueue(ctx, record.ID)
	}
	return &Accepted{WebhookID: record.ID, Kind: event.Kind}, nil
}

// ClaimUnprocessed returns webhooks the processor has not finished with.
//
// It exists because the queue is best-effort: a webhook stored during a broker outage would
// otherwise sit unprocessed forever, and a webhook the platform accepted and never applied is
// money that moved without the platform noticing.
func (i *Ingester) ClaimUnprocessed(ctx context.Context, limit int) ([]ports.InboundWebhook, error) {
	var out []ports.InboundWebhook
	err := i.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		out, err = r.Webhooks.ClaimUnprocessed(ctx, limit)
		return err
	})
	return out, err
}

// DefaultReplayWindow is how old a webhook timestamp may be before it is treated as a replay.
//
// Five minutes: long enough to survive clock skew and a slow queue at the gateway's end, short
// enough that a captured request is not indefinitely replayable. The adapters enforce it inside
// Verify, over the signed timestamp; this constant exists so the ingress and the adapters cannot
// disagree about the number.
const DefaultReplayWindow = 5 * time.Minute

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-74, FR-75, FR-76, FR-77.
//
// Inbound webhook ingestion: verify over the raw bytes, reject a replay, deduplicate, persist
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
