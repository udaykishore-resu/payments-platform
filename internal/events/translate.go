package events

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
)

// publication is how one domain event type reaches (or does not reach) the wire.
//
// The domain raises more event types than the platform publishes, and that is correct rather
// than an oversight. An aggregate raises an event whenever something worth recording happened;
// a *published* event is a contract with other teams that we promise never to break. Conflating
// the two means either publishing internal bookkeeping that we can then never change, or
// suppressing domain events that the aggregate genuinely needs.
//
// So each domain type maps to exactly one of three outcomes, and the mapping is explicit:
//
//   - Identity: the domain type is also a catalog type.
//   - Remap: the domain type is published *as* a different catalog type, with the payload
//     augmented so that the published shape is honest (e.g. payment.expired.v1 is published as
//     payment.failed.v1 with failureStage=EXPIRY, which is exactly the enum value the catalog
//     defines for it).
//   - Suppressed: the domain type has no published representation. It drives an in-process
//     workflow and has no schema, no topic and no consumer. Publishing it "just in case" would
//     create a contract nobody asked for and V4 forbids us from ever removing.
type publication struct {
	// PublishedType is the catalog type, or "" when suppressed.
	PublishedType string
	// Why documents the mapping decision at the point where it was made. It is not decoration:
	// the next person to read this table needs to know whether a blank PublishedType is a
	// decision or an omission, and the difference is money.
	Why string
	// Derive augments the domain payload so it satisfies the published type's schema. It
	// receives a copy and returns it; it must never mutate the aggregate's map.
	Derive func(map[string]any) map[string]any
}

// paymentPublications maps every payment.EventType. Completeness is asserted at init.
var paymentPublications = map[payment.EventType]publication{
	payment.EventPaymentCreated:   {PublishedType: "payment.created.v1", Why: "catalog type"},
	payment.EventPaymentAttempted: {PublishedType: "payment.attempted.v1", Why: "catalog type"},

	// A 3DS challenge is an attempt that has been dispatched and is waiting for the payer. The
	// catalog models it with payment.attempted.v1's state=DISPATCHED rather than with a separate
	// type, because the routing-feedback and analytics consumers reason about attempts, and a
	// challenge that is never completed must show up in the same denominator as one that is.
	payment.EventPaymentRequiresAction: {
		PublishedType: "payment.attempted.v1",
		Why:           "a challenge is a dispatched attempt awaiting the payer; catalog has no separate type",
		Derive:        withDefaults(map[string]any{"state": "DISPATCHED"}),
	},

	payment.EventPaymentAuthorized: {PublishedType: "payment.authorized.v1", Why: "catalog type"},
	payment.EventPaymentCaptured:   {PublishedType: "payment.captured.v1", Why: "catalog type"},
	payment.EventPaymentFailed: {
		PublishedType: "payment.failed.v1",
		Why:           "catalog type",
		Derive:        withDefaults(map[string]any{"terminal": true}),
	},
	payment.EventPaymentVoided: {PublishedType: "payment.voided.v1", Why: "catalog type"},

	// Cancellation and expiry are terminal failures with a specific stage. payment.failed.v1's
	// failureStage enum carries CANCELED and EXPIRY precisely so these do not need their own
	// types — a consumer that must not miss a terminal outcome subscribes to one type, not three,
	// and cannot forget the third.
	payment.EventPaymentCanceled: {
		PublishedType: "payment.failed.v1",
		Why:           "published as failed with failureStage=CANCELED; the catalog enum exists for this",
		Derive:        withDefaults(map[string]any{"failureStage": "CANCELED", "errorCode": "PAYMENT_CANCELED", "terminal": true}),
	},
	payment.EventPaymentExpired: {
		PublishedType: "payment.failed.v1",
		Why:           "published as failed with failureStage=EXPIRY",
		Derive:        withDefaults(map[string]any{"failureStage": "EXPIRY", "errorCode": "AUTHORIZATION_EXPIRED", "terminal": true}),
	},

	payment.EventPaymentRefunded: {PublishedType: "payment.refunded.v1", Why: "catalog type"},
	payment.EventPaymentSettled:  {PublishedType: "payment.settled.v1", Why: "catalog type"},
	payment.EventPaymentDisputed: {PublishedType: "payment.disputed.v1", Why: "catalog type"},

	// A dispute's resolution is the same dispute with an outcome. The catalog says `outcome` is
	// "absent while open", which is only meaningful if the resolved case is the same type — a
	// separate dispute_resolved type would leave `outcome` on payment.disputed.v1 permanently
	// unset and unexplainable.
	payment.EventPaymentDisputeResolved: {
		PublishedType: "payment.disputed.v1",
		Why:           "a resolution is payment.disputed.v1 carrying `outcome`; see the catalog's note that outcome is absent only while open",
	},

	payment.EventPaymentReconciliationRequired: {
		PublishedType: "payment.reconciliation_required.v1", Why: "catalog type",
	},
}

// merchantPublications maps every merchant.EventType. Completeness is asserted at init.
var merchantPublications = map[merchant.EventType]publication{
	merchant.EventMerchantCreated:            {PublishedType: "merchant.created.v1", Why: "catalog type"},
	merchant.EventMerchantValidated:          {PublishedType: "merchant.validated.v1", Why: "catalog type"},
	merchant.EventMerchantKYCApproved:        {PublishedType: "merchant.kyc_approved.v1", Why: "catalog type"},
	merchant.EventMerchantKYCFailed:          {PublishedType: "merchant.kyc_failed.v1", Why: "catalog type"},
	merchant.EventMerchantBankValidated:      {PublishedType: "merchant.bank_validated.v1", Why: "catalog type"},
	merchant.EventMerchantGatewayProvisioned: {PublishedType: "merchant.gateway_provisioned.v1", Why: "catalog type"},
	merchant.EventMerchantCertified:          {PublishedType: "merchant.certified.v1", Why: "catalog type"},
	merchant.EventMerchantActivated:          {PublishedType: "merchant.activated.v1", Why: "catalog type"},
	merchant.EventMerchantSuspended:          {PublishedType: "merchant.suspended.v1", Why: "catalog type"},
	merchant.EventMerchantTerminated:         {PublishedType: "merchant.terminated.v1", Why: "catalog type"},

	// Reinstatement is activation from SUSPENDED. merchant.activated.v1's previousState enum is
	// documented as "PRODUCTION_READY or SUSPENDED (resume)" for exactly this: the data-plane
	// cache's job on both is identical — start routing to this merchant again — and giving it two
	// types to handle is how one of them ends up unhandled.
	merchant.EventMerchantReinstated: {
		PublishedType: "merchant.activated.v1",
		Why:           "a reinstatement is an activation with previousState=SUSPENDED",
		Derive:        withDefaults(map[string]any{"previousState": "SUSPENDED"}),
	},

	// The onboarding failure events below are suppressed. Every one of them is a step outcome
	// inside the onboarding workflow (BC-3/BC-4): the workflow engine reads them from the same
	// transaction that raised them and decides whether to retry the step, compensate or fail the
	// case. Nothing outside the workflow acts on them, none of them has a schema in api/events/,
	// and publishing them would mint eight permanent public contracts to describe the internal
	// state of one workflow — which V4 would then forbid us from ever changing.
	//
	// The externally meaningful outcomes of onboarding *are* published: merchant.kyc_failed.v1
	// for the one failure a merchant must be told about, and merchant.suspended.v1 /
	// merchant.terminated.v1 for the lifecycle consequences.
	merchant.EventMerchantValidationFailed:     {Why: "onboarding workflow step outcome; no consumer outside the workflow, no schema"},
	merchant.EventMerchantBankValidationFailed: {Why: "onboarding workflow step outcome; the case's failure is reported by the workflow, not the topic"},
	merchant.EventMerchantProvisioningFailed:   {Why: "onboarding workflow step outcome; retried or compensated in-process"},
	merchant.EventMerchantConfigurationFailed:  {Why: "onboarding workflow step outcome; retried or compensated in-process"},
	merchant.EventMerchantCertificationFailed:  {Why: "onboarding workflow step outcome; the certification report is the artifact, not an event"},
	merchant.EventMerchantComplianceRejected:   {Why: "onboarding workflow step outcome; the merchant-visible consequence is merchant.kyc_failed.v1 or merchant.terminated.v1"},
}

// init proves the mapping tables are total and consistent, and panics if they are not.
//
// This is the check the deliverable exists for. A domain event type with no entry here would
// otherwise be discovered at runtime, by a repository draining an aggregate's events into the
// outbox and finding nothing to write — which loses the event silently, in production, on the
// one code path where losing an event means the ledger and the payments table disagree forever.
// Failing at init means the binary does not start and the test suite goes red on the commit that
// added the type.
func init() {
	var missing []string
	for _, t := range payment.AllEventTypes {
		p, ok := paymentPublications[t]
		if !ok {
			missing = append(missing, "payment."+string(t))
			continue
		}
		if p.PublishedType != "" {
			if _, err := Lookup(p.PublishedType); err != nil {
				panic(fmt.Sprintf("events: payment event %q maps to unregistered published type %q", t, p.PublishedType))
			}
		}
		if p.Why == "" {
			panic(fmt.Sprintf("events: payment event %q has an undocumented publication decision", t))
		}
	}
	for _, t := range merchant.AllEventTypes {
		p, ok := merchantPublications[t]
		if !ok {
			missing = append(missing, "merchant."+string(t))
			continue
		}
		if p.PublishedType != "" {
			if _, err := Lookup(p.PublishedType); err != nil {
				panic(fmt.Sprintf("events: merchant event %q maps to unregistered published type %q", t, p.PublishedType))
			}
		}
		if p.Why == "" {
			panic(fmt.Sprintf("events: merchant event %q has an undocumented publication decision", t))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		panic(fmt.Sprintf(
			"events: %d domain event type(s) have no publication mapping: %v — add an entry to "+
				"translate.go, either a published type or an explicitly suppressed one with a reason",
			len(missing), missing))
	}
}

// PublicationForPayment reports how a payment domain event reaches the wire.
//
// The second return is false for a type with no mapping at all, which cannot happen in a binary
// that started (see init) but is returned rather than panicked so that a test can assert the
// contract without recovering from a panic.
func PublicationForPayment(t payment.EventType) (published string, suppressed bool, known bool) {
	p, ok := paymentPublications[t]
	if !ok {
		return "", false, false
	}
	return p.PublishedType, p.PublishedType == "", true
}

// PublicationForMerchant reports how a merchant domain event reaches the wire.
func PublicationForMerchant(t merchant.EventType) (published string, suppressed bool, known bool) {
	p, ok := merchantPublications[t]
	if !ok {
		return "", false, false
	}
	return p.PublishedType, p.PublishedType == "", true
}

// TranslatePayment converts a payment domain event into an outbox message.
//
// The boolean reports whether the event is published at all. A suppressed event returns
// (zero, false, nil) — not an error, because it is not one: the repository draining the
// aggregate's events simply has nothing to append for it.
func TranslatePayment(ctx context.Context, ev payment.Event) (ports.OutboxMessage, bool, error) {
	p, ok := paymentPublications[ev.Type]
	if !ok {
		// Unreachable in a started binary; kept because "unreachable" and "unreached" differ,
		// and this is the money path.
		return ports.OutboxMessage{}, false, unmappedError("payment", string(ev.Type))
	}
	if p.PublishedType == "" {
		return ports.OutboxMessage{}, false, nil
	}
	// The domain event carries the correlation of the request that caused it, which is more
	// specific than whatever is ambient in this goroutine — a relay republishing on behalf of an
	// older request would otherwise stamp its own.
	if ev.Correlation != "" {
		ctx = ContextWithProvenance(ctx, Provenance{CorrelationID: ev.Correlation})
	}
	msg, err := EncodeFact(ctx, paymentFact{ev: ev, pub: p})
	if err != nil {
		return ports.OutboxMessage{}, false, err
	}
	return msg, true, nil
}

// TranslateMerchant converts a merchant domain event into an outbox message.
func TranslateMerchant(ctx context.Context, ev merchant.Event) (ports.OutboxMessage, bool, error) {
	p, ok := merchantPublications[ev.Type]
	if !ok {
		return ports.OutboxMessage{}, false, unmappedError("merchant", string(ev.Type))
	}
	if p.PublishedType == "" {
		return ports.OutboxMessage{}, false, nil
	}
	msg, err := EncodeFact(ctx, merchantFact{ev: ev, pub: p})
	if err != nil {
		return ports.OutboxMessage{}, false, err
	}
	return msg, true, nil
}

// TranslatePaymentEvents converts a whole drained event list, dropping suppressed types.
//
// The repository calls this inside the state-change transaction and appends the result to the
// outbox in the same transaction — one call, one slice, so there is no way to append some of an
// aggregate's events and forget the rest.
func TranslatePaymentEvents(ctx context.Context, evs []payment.Event) ([]ports.OutboxMessage, error) {
	out := make([]ports.OutboxMessage, 0, len(evs))
	for _, ev := range evs {
		msg, published, err := TranslatePayment(ctx, ev)
		if err != nil {
			return nil, err
		}
		if published {
			out = append(out, msg)
		}
	}
	return out, nil
}

// TranslateMerchantEvents converts a whole drained event list, dropping suppressed types.
func TranslateMerchantEvents(ctx context.Context, evs []merchant.Event) ([]ports.OutboxMessage, error) {
	out := make([]ports.OutboxMessage, 0, len(evs))
	for _, ev := range evs {
		msg, published, err := TranslateMerchant(ctx, ev)
		if err != nil {
			return nil, err
		}
		if published {
			out = append(out, msg)
		}
	}
	return out, nil
}

// --- fact adapters ------------------------------------------------------------------------------

// paymentFact adapts a payment.Event to Fact. It exists so that the domain type needs no
// knowledge of partition keys or CloudEvents; see the note on the Fact interface.
type paymentFact struct {
	ev  payment.Event
	pub publication
}

// EventType is the published catalog type this domain event is emitted as.
func (f paymentFact) EventType() string { return f.pub.PublishedType }

// Subject is the attempt for attempt-scoped events and the payment otherwise. This is the one
// place the envelope's subject and aggregateid legitimately differ (docs/events.md §1.1): an
// attempt event is *about* the attempt but is ordered and versioned under the payment.
func (f paymentFact) Subject() string {
	if f.pub.PublishedType == "payment.attempted.v1" {
		if id, ok := f.ev.Payload["attemptId"].(string); ok && id != "" {
			return id
		}
	}
	return f.ev.PaymentID.String()
}

// The remaining Fact methods are mechanical projections of the domain event onto the envelope's
// fields. A payment event is always ordered and versioned under its payment, which is why
// AggregateID and PartitionKey are the same value and Subject (above) is the only one that varies.
func (f paymentFact) AggregateID() string     { return f.ev.PaymentID.String() }
func (f paymentFact) AggregateVersion() int64 { return int64(f.ev.Version) }
func (f paymentFact) PartitionKey() string    { return f.ev.PaymentID.String() }
func (f paymentFact) OccurredAt() time.Time   { return f.ev.OccurredAt }
func (f paymentFact) TenantID() string        { return f.ev.TenantID.String() }
func (f paymentFact) MerchantID() string      { return f.ev.MerchantID.String() }
func (f paymentFact) Data() map[string]any    { return derive(f.pub, f.ev.Payload) }

// merchantFact adapts a merchant.Event to Fact.
type merchantFact struct {
	ev  merchant.Event
	pub publication
}

// The Fact methods for a merchant event are all the aggregate root: a merchant event is about the
// merchant, is ordered under the merchant and is keyed by the merchant, so subject, aggregate id
// and partition key coincide.
func (f merchantFact) EventType() string       { return f.pub.PublishedType }
func (f merchantFact) Subject() string         { return f.ev.MerchantID.String() }
func (f merchantFact) AggregateID() string     { return f.ev.MerchantID.String() }
func (f merchantFact) AggregateVersion() int64 { return int64(f.ev.Version) }
func (f merchantFact) PartitionKey() string    { return f.ev.MerchantID.String() }
func (f merchantFact) OccurredAt() time.Time   { return f.ev.OccurredAt }
func (f merchantFact) TenantID() string        { return f.ev.TenantID.String() }
func (f merchantFact) MerchantID() string      { return f.ev.MerchantID.String() }
func (f merchantFact) Data() map[string]any    { return derive(f.pub, f.ev.Payload) }

// --- helpers -------------------------------------------------------------------------------------

// derive copies the aggregate's payload and applies the mapping's augmentation.
//
// The copy is not optional. The map belongs to the aggregate's event list, the caller may still
// be holding that aggregate, and a translation that mutated it would change what the domain
// believes it raised — a bug that shows up as an event whose payload does not match the
// aggregate's state, days later, in a reconciliation report.
func derive(p publication, payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload)+4)
	for k, v := range payload {
		out[k] = v
	}
	if p.Derive != nil {
		out = p.Derive(out)
	}
	return out
}

// withDefaults returns a Derive that fills fields the domain did not set.
//
// Fills, never overwrites: if the aggregate already said failureStage=GATEWAY, the mapping's
// default must not silently rewrite it to CANCELED. A default that overwrites is not a default,
// it is a lie about what happened.
func withDefaults(defaults map[string]any) func(map[string]any) map[string]any {
	return func(m map[string]any) map[string]any {
		for k, v := range defaults {
			if _, present := m[k]; !present {
				m[k] = v
			}
		}
		return m
	}
}

func unmappedError(boundedContext, eventType string) error {
	return fmt.Errorf(
		"events: %s domain event type %q has no publication mapping; this is a programming error "+
			"that init should have caught", boundedContext, eventType)
}
