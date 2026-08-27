package events

import (
	"fmt"
	"sort"
	"sync"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Registration is everything the platform knows about one published event type.
//
// The set of registrations is the catalog from docs/spec/00-design-baseline.md §13.2, and it is
// code rather than a YAML file for one reason: a producer that emits an unregistered type must
// fail to compile or fail at init, not at 3 a.m. when the event reaches a consumer that has
// never heard of it. A configuration file cannot enforce that; an init-time panic can.
type Registration struct {
	// Type is the published type name, e.g. "payment.captured.v1".
	Type string
	// Topic is where it is published. Every type of one aggregate shares a topic so that the
	// partition key gives per-aggregate ordering across all of them.
	Topic string
	// SchemaFile is the file in api/events/ that defines the `data` object.
	SchemaFile string
	// SchemaURI is what the envelope's dataschema carries. Derived from Type, never typed by
	// hand — see the note on Envelope about the version living in the name.
	SchemaURI string
	// PartitionKeyField names the ordering domain from the catalog ("payment_id",
	// "merchant_id", "gateway_ref", "gateway_id", "tenant_id"). It is documentation with teeth:
	// changing it is a new topic, not a new type, because it silently destroys ordering.
	PartitionKeyField string
	// Aggregate is the aggregate the event is about, for provenance in tooling.
	Aggregate string
}

// registry is the process-wide type registry.
//
// It is guarded by a mutex even though every write happens in init(), before any goroutine
// exists. The lock costs nothing measurable next to the JSON encode that follows every lookup,
// and it means a future test that calls Register from a test helper does not introduce a data
// race that only manifests under -race on a busy CI machine.
var registry = struct {
	mu     sync.RWMutex
	byType map[string]Registration
}{byType: make(map[string]Registration, 32)}

// Register adds a type to the catalog. It is called from init and panics on a duplicate or a
// malformed entry.
//
// Panicking is correct here and only here: this is a programming error detectable before the
// process serves a request, which is exactly the exception the code conventions carve out. The
// alternative — returning an error that init ignores — produces a binary that starts happily and
// cannot publish half its events.
func Register(r Registration) {
	if r.Type == "" || !reType.MatchString(r.Type) {
		panic(fmt.Sprintf("events: invalid event type %q: must be <aggregate>.<verb>.v<major>", r.Type))
	}
	if r.Topic == "" {
		panic("events: registration for " + r.Type + " has no topic")
	}
	if r.PartitionKeyField == "" {
		panic("events: registration for " + r.Type + " has no partition key field")
	}
	r.SchemaFile = SchemaFileFor(r.Type)
	r.SchemaURI = SchemaURIFor(r.Type)

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, dup := registry.byType[r.Type]; dup {
		panic("events: duplicate registration for event type " + r.Type)
	}
	registry.byType[r.Type] = r
}

// Lookup returns the registration for a type.
//
// It fails loudly, and non-retryably. An unknown type on the consume side is either a producer
// that shipped ahead of its schema or a consumer that is behind — both are contract problems,
// and retrying the message six times over twenty-two minutes only delays the alert that says so
// (docs/events.md §9.2). The error routes the message straight to the DLQ.
func Lookup(eventType string) (Registration, error) {
	registry.mu.RLock()
	r, ok := registry.byType[eventType]
	registry.mu.RUnlock()
	if !ok {
		return Registration{}, apierror.Newf(apierror.CodeValidationFailed,
			"unknown event type %q", eventType).
			WithDetail(apierror.Detail{
				Field:   "type",
				Code:    "UNKNOWN_EVENT_TYPE",
				Message: "the event type is not in the platform catalog; a producer is ahead of its schema or this consumer is behind",
				RuleID:  "L6.EVENT_TYPE_REGISTERED",
			})
	}
	return r, nil
}

// MustLookup is Lookup for call sites where the type is a compile-time constant of this package.
// It panics, which is safe because every such call site is exercised by the package's own tests.
func MustLookup(eventType string) Registration {
	r, err := Lookup(eventType)
	if err != nil {
		panic(err)
	}
	return r
}

// TopicFor returns the topic a type is published to.
func TopicFor(eventType string) (string, error) {
	r, err := Lookup(eventType)
	if err != nil {
		return "", err
	}
	return r.Topic, nil
}

// AllRegistered returns every registration, sorted by type.
//
// Sorted because the two things that consume it — the CI test that reconciles the registry
// against api/events/, and the `platformctl events catalog` listing — both produce diffable
// output, and a map's iteration order would make every run's output differ from the last.
func AllRegistered() []Registration {
	registry.mu.RLock()
	out := make([]Registration, 0, len(registry.byType))
	for _, r := range registry.byType {
		out = append(out, r)
	}
	registry.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// AllTopics returns every distinct topic in the catalog, sorted. admin.go builds the topic
// configuration from it, so a new event type on a new topic cannot be shipped without the topic
// appearing in the provisioning path.
func AllTopics() []string {
	seen := map[string]struct{}{}
	for _, r := range AllRegistered() {
		seen[r.Topic] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// The catalog. Twenty-five types, exactly as docs/spec/00-design-baseline.md §13.2,
// docs/events.md §4 and api/events/README.md — registry_test.go asserts all three agree with
// this table and with the schema directory, in both directions.
func init() {
	const (
		topicMerchants = "pp.merchants.merchant.v1"
		topicConfig    = "pp.config.configuration.v1"
		topicPayments  = "pp.payments.payment.v1"
		topicHealth    = "pp.gateways.health.v1"
		topicWebhooks  = "pp.webhooks.inbound.v1"
		topicAudit     = "pp.audit.v1"

		keyMerchant = "merchant_id"
		keyPayment  = "payment_id"
		keyGateway  = "gateway_id"
		keyWebhook  = "gateway_ref"
		keyTenant   = "tenant_id"
	)

	for _, r := range []Registration{
		// 4.1 Merchant lifecycle.
		{Type: "merchant.created.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "merchant"},
		{Type: "merchant.validated.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "merchant"},
		{Type: "merchant.kyc_approved.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "merchant"},
		{Type: "merchant.kyc_failed.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "merchant"},
		{Type: "merchant.bank_validated.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "merchant"},
		{Type: "merchant.gateway_provisioned.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "gateway_connection"},
		{Type: "merchant.certified.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "merchant"},
		{Type: "merchant.activated.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "merchant"},
		{Type: "merchant.suspended.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "merchant"},
		{Type: "merchant.terminated.v1", Topic: topicMerchants, PartitionKeyField: keyMerchant, Aggregate: "merchant"},

		// 4.2 Configuration. Compacted topic: the payload is the full document, not a diff.
		{Type: "configuration.published.v1", Topic: topicConfig, PartitionKeyField: keyMerchant, Aggregate: "configuration"},
		{Type: "configuration.rolled_back.v1", Topic: topicConfig, PartitionKeyField: keyMerchant, Aggregate: "configuration"},

		// 4.3 Payments.
		{Type: "payment.created.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment"},
		{Type: "payment.attempted.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment_attempt"},
		{Type: "payment.authorized.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment"},
		{Type: "payment.captured.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment"},
		{Type: "payment.failed.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment"},
		{Type: "payment.voided.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment"},
		{Type: "payment.refunded.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment"},
		{Type: "payment.settled.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment"},
		{Type: "payment.disputed.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment"},
		{Type: "payment.reconciliation_required.v1", Topic: topicPayments, PartitionKeyField: keyPayment, Aggregate: "payment"},

		// 4.4 Webhooks. Keyed by the gateway's own event reference, which is also the dedup key:
		// the same webhook redelivered by the gateway must land on the same partition as the
		// first delivery or the processor's ordering reasoning is worthless.
		{Type: "webhook.received.v1", Topic: topicWebhooks, PartitionKeyField: keyWebhook, Aggregate: "inbound_webhook"},

		// 4.5 Gateway health. Platform-scoped: no merchantid, tenantid is the platform tenant.
		{Type: "gateway.health_changed.v1", Topic: topicHealth, PartitionKeyField: keyGateway, Aggregate: "gateway_health"},

		// 4.6 Audit.
		{Type: "audit.recorded.v1", Topic: topicAudit, PartitionKeyField: keyTenant, Aggregate: "audit_record"},
	} {
		Register(r)
	}
}
