package events

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
)

// schemaFilesOnDisk returns the event-type schema files in api/events/, excluding the envelope
// (which describes the wrapper, not a type) and the prose.
func schemaFilesOnDisk(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		t.Fatalf("reading %s: %v", schemaDir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".schema.json") || name == "envelope.schema.json" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestEveryRegisteredTypeHasASchemaAndViceVersa is the check that keeps the catalog, the code and
// the schema directory from drifting.
//
// It matters in both directions and for different reasons. A registered type with no schema is a
// producer that can emit something no consumer can validate — the poison-message generator of
// docs/events.md §9.6. A schema with no registration is a contract we published and then cannot
// produce, which is worse than useless: a consumer team builds against it and waits.
func TestEveryRegisteredTypeHasASchemaAndViceVersa(t *testing.T) {
	t.Parallel()

	registered := map[string]bool{}
	for _, r := range AllRegistered() {
		registered[r.SchemaFile] = true
	}
	onDisk := map[string]bool{}
	for _, f := range schemaFilesOnDisk(t) {
		onDisk[f] = true
	}

	for file := range registered {
		if !onDisk[file] {
			t.Errorf("registered event type has no schema file: api/events/%s", file)
		}
	}
	for file := range onDisk {
		if !registered[file] {
			t.Errorf("schema file has no registry entry: api/events/%s", file)
		}
	}
	if len(registered) != len(onDisk) {
		t.Errorf("registry has %d types, api/events/ has %d schema files", len(registered), len(onDisk))
	}
}

// TestCatalogSize pins the count from the baseline. A silent change to it means somebody added or
// removed a public contract, which is a thing to notice in review rather than in production.
func TestCatalogSize(t *testing.T) {
	t.Parallel()
	if got, want := len(AllRegistered()), 25; got != want {
		t.Fatalf("catalog has %d types, baseline §13.2 says %d", got, want)
	}
}

func TestRegistrationsAreSelfConsistent(t *testing.T) {
	t.Parallel()
	for _, r := range AllRegistered() {
		if r.SchemaURI != SchemaURIFor(r.Type) {
			t.Errorf("%s: dataschema URI %q is not derived from the type", r.Type, r.SchemaURI)
		}
		if r.SchemaFile != SchemaFileFor(r.Type) {
			t.Errorf("%s: schema file %q is not derived from the type", r.Type, r.SchemaFile)
		}
		if !strings.HasPrefix(r.Topic, "pp.") {
			t.Errorf("%s: topic %q does not follow pp.<context>.<aggregate>.v<major>", r.Type, r.Topic)
		}
		if r.Aggregate == "" || r.PartitionKeyField == "" {
			t.Errorf("%s: incomplete registration %+v", r.Type, r)
		}
	}
}

// TestTopicsMatchTheCatalogTable pins the partition key per topic. Changing a partition key is
// the one schema change docs/events.md classifies as worse than breaking, because it silently
// destroys ordering rather than failing loudly — so it gets its own assertion.
func TestTopicsMatchTheCatalogTable(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"pp.merchants.merchant.v1":   "merchant_id",
		"pp.config.configuration.v1": "merchant_id",
		"pp.payments.payment.v1":     "payment_id",
		"pp.gateways.health.v1":      "gateway_id",
		"pp.webhooks.inbound.v1":     "gateway_ref",
		"pp.audit.v1":                "tenant_id",
	}
	for _, r := range AllRegistered() {
		key, known := want[r.Topic]
		if !known {
			t.Errorf("%s publishes to unknown topic %s", r.Type, r.Topic)
			continue
		}
		if r.PartitionKeyField != key {
			t.Errorf("%s on %s keys by %s, catalog says %s", r.Type, r.Topic, r.PartitionKeyField, key)
		}
	}
	got := AllTopics()
	if len(got) != len(want) {
		t.Errorf("AllTopics returned %v, catalog has %d topics", got, len(want))
	}
}

func TestLookupFailsLoudlyOnAnUnknownType(t *testing.T) {
	t.Parallel()
	_, err := Lookup("payment.teleported.v1")
	if err == nil {
		t.Fatal("Lookup accepted an unregistered type")
	}
	if !strings.Contains(err.Error(), "unknown event type") {
		t.Fatalf("error does not name the problem: %v", err)
	}
	if _, err := TopicFor("nope.nope.v1"); err == nil {
		t.Fatal("TopicFor accepted an unregistered type")
	}
}

func TestRegisterRejectsDuplicatesAndMalformedTypes(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]Registration{
		"duplicate":      {Type: "payment.captured.v1", Topic: "pp.payments.payment.v1", PartitionKeyField: "payment_id"},
		"unversioned":    {Type: "payment.captured", Topic: "pp.payments.payment.v1", PartitionKeyField: "payment_id"},
		"no topic":       {Type: "payment.teleported.v1", PartitionKeyField: "payment_id"},
		"no partitioner": {Type: "payment.teleported.v1", Topic: "pp.payments.payment.v1"},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("Register accepted %s", name)
				}
			}()
			Register(r)
		})
	}
}

// TestEveryDomainEventTypeHasATranslation is the assertion the translation table exists for.
//
// The init() in translate.go already panics on a gap, so a binary with one does not start. This
// test states the same contract in a form that fails on the commit that introduces the gap and
// names the missing type, rather than producing a panic in whatever unrelated test happened to
// import this package first.
func TestEveryDomainEventTypeHasATranslation(t *testing.T) {
	t.Parallel()

	for _, dt := range payment.AllEventTypes {
		published, suppressed, known := PublicationForPayment(dt)
		if !known {
			t.Errorf("payment domain event %q has no publication mapping", dt)
			continue
		}
		if suppressed {
			continue
		}
		if _, err := Lookup(published); err != nil {
			t.Errorf("payment domain event %q maps to unregistered type %q", dt, published)
		}
	}

	for _, dt := range merchant.AllEventTypes {
		published, suppressed, known := PublicationForMerchant(dt)
		if !known {
			t.Errorf("merchant domain event %q has no publication mapping", dt)
			continue
		}
		if suppressed {
			continue
		}
		if _, err := Lookup(published); err != nil {
			t.Errorf("merchant domain event %q maps to unregistered type %q", dt, published)
		}
	}
}

// TestPublishedTypesAreReachableFromTheDomain closes the other direction for the two aggregates
// this package translates: a payment or merchant type in the catalog that no domain event can
// produce is a contract we cannot honour.
func TestPublishedTypesAreReachableFromTheDomain(t *testing.T) {
	t.Parallel()

	reachable := map[string]bool{}
	for _, dt := range payment.AllEventTypes {
		if p, _, ok := PublicationForPayment(dt); ok && p != "" {
			reachable[p] = true
		}
	}
	for _, dt := range merchant.AllEventTypes {
		if p, _, ok := PublicationForMerchant(dt); ok && p != "" {
			reachable[p] = true
		}
	}

	// Types produced by other bounded contexts, which have no payment/merchant domain event.
	// Listed explicitly so that adding one is a deliberate act rather than a silent gap.
	producedElsewhere := map[string]string{
		"configuration.published.v1":   "BC-5, from config.MerchantConfig",
		"configuration.rolled_back.v1": "BC-5, from config.MerchantConfig",
		"webhook.received.v1":          "BC-7, from ports.InboundWebhook at the ingress",
		"gateway.health_changed.v1":    "BC-4, from gateway.Health",
		"audit.recorded.v1":            "BC-9, from audit.Record",
	}

	for _, r := range AllRegistered() {
		if reachable[r.Type] || producedElsewhere[r.Type] != "" {
			continue
		}
		t.Errorf("catalog type %s is produced by no payment or merchant domain event and is not "+
			"listed as produced elsewhere", r.Type)
	}
}

// TestSchemaFilesParseAndNameThemselves guards the file/URI correspondence CI relies on.
func TestSchemaFilesParseAndNameThemselves(t *testing.T) {
	t.Parallel()
	for _, f := range schemaFilesOnDisk(t) {
		s := loadSchema(t, f)
		if s.Type != "object" {
			t.Errorf("%s: payload schema must describe an object, got %q", f, s.Type)
		}
		typeName := strings.TrimSuffix(f, ".schema.json")
		r, err := Lookup(typeName)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if r.SchemaURI != SchemaBaseURI+typeName+".json" {
			t.Errorf("%s: URI mapping is wrong: %s", f, r.SchemaURI)
		}
		if _, err := os.Stat(filepath.Join(schemaDir, r.SchemaFile)); err != nil {
			t.Errorf("%s: registration points at a missing file: %v", f, err)
		}
	}
}
