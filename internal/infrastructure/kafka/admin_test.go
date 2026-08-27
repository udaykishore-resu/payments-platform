package kafka

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/events"
)

// TestEveryCatalogTopicHasASpec closes the loop between the event registry and the provisioning
// code. A type registered against a topic nobody provisions is a producer that fails at runtime
// against a broker with auto-creation disabled — and with auto-creation enabled it is worse,
// because the topic is created with broker defaults, usually replication factor 1.
func TestEveryCatalogTopicHasASpec(t *testing.T) {
	t.Parallel()
	specs := map[string]TopicSpec{}
	for _, s := range AllTopicSpecs() {
		specs[s.Name] = s
	}
	for _, topic := range events.AllTopics() {
		if _, ok := specs[topic]; !ok {
			t.Errorf("catalog topic %s has no TopicSpec", topic)
		}
		for _, tier := range DefaultTiers {
			if _, ok := specs[topic+tier.Suffix]; !ok {
				t.Errorf("catalog topic %s has no %s sibling", topic, tier.Suffix)
			}
		}
		if _, ok := specs[topic+DLQSuffix]; !ok {
			t.Errorf("catalog topic %s has no dead-letter sibling", topic)
		}
	}
}

func TestEverySpecIsValid(t *testing.T) {
	t.Parallel()
	for _, s := range AllTopicSpecs() {
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
		if s.Why == "" {
			t.Errorf("%s: the spec carries no reasoning", s.Name)
		}
	}
}

// TestSpecsMatchTheDocumentedTable is the table from docs/events.md §5.2.
func TestSpecsMatchTheDocumentedTable(t *testing.T) {
	t.Parallel()
	want := map[string]struct {
		partitions int32
		minISR     int
		retention  time.Duration
		compact    bool
	}{
		"pp.merchants.merchant.v1":   {12, 2, RetentionMerchants, false},
		"pp.config.configuration.v1": {12, 2, RetentionConfig, true},
		"pp.payments.payment.v1":     {48, 2, RetentionPayments, false},
		"pp.gateways.health.v1":      {6, 2, RetentionHealth, true},
		"pp.webhooks.inbound.v1":     {24, 2, RetentionWebhooks, false},
		"pp.audit.v1":                {12, 3, RetentionAudit, false},
	}
	got := map[string]TopicSpec{}
	for _, s := range AllTopicSpecs() {
		got[s.Name] = s
	}
	for name, w := range want {
		s, ok := got[name]
		if !ok {
			t.Errorf("%s is missing", name)
			continue
		}
		if s.Partitions != w.partitions {
			t.Errorf("%s partitions = %d, want %d", name, s.Partitions, w.partitions)
		}
		if s.MinInSyncReplicas != w.minISR {
			t.Errorf("%s min.insync.replicas = %d, want %d", name, s.MinInSyncReplicas, w.minISR)
		}
		if s.Retention != w.retention {
			t.Errorf("%s retention = %v, want %v", name, s.Retention, w.retention)
		}
		if s.Compact != w.compact {
			t.Errorf("%s compact = %v, want %v", name, s.Compact, w.compact)
		}
		if s.ReplicationFactor != 3 {
			t.Errorf("%s replication factor = %d, want 3", name, s.ReplicationFactor)
		}
	}
}

// TestAuditIsTheOnlyTopicThatStallsOnABrokerLoss. min.insync=3 with RF 3 halts production on any
// single broker loss, which is unacceptable on the payment path and correct for audit.
func TestAuditIsTheOnlyTopicThatStallsOnABrokerLoss(t *testing.T) {
	t.Parallel()
	for _, s := range AllTopicSpecs() {
		strict := s.MinInSyncReplicas >= int(s.ReplicationFactor)
		isAudit := strings.HasPrefix(s.Name, "pp.audit.v1")
		if strict && !isAudit {
			t.Errorf("%s has min.insync.replicas == RF; a single broker loss would halt the payment path", s.Name)
		}
		if isAudit && !strict {
			t.Errorf("%s should run min.insync.replicas=3 so an audit gap stalls rather than silently drops", s.Name)
		}
	}
}

func TestConfigsRenderTheDurabilityProperties(t *testing.T) {
	t.Parallel()
	for _, s := range AllTopicSpecs() {
		cfg := s.Configs()
		if cfg[cfgUncleanElection] != "false" {
			t.Errorf("%s allows unclean leader election", s.Name)
		}
		if cfg[cfgMinInSyncReplica] != strconv.Itoa(s.MinInSyncReplicas) {
			t.Errorf("%s min.insync.replicas config = %q", s.Name, cfg[cfgMinInSyncReplica])
		}
		if cfg[cfgRetentionMs] != strconv.FormatInt(s.Retention.Milliseconds(), 10) {
			t.Errorf("%s retention.ms = %q", s.Name, cfg[cfgRetentionMs])
		}
		if got := cfg[cfgMaxMessageBytes]; got != strconv.Itoa(MaxMessageBytes) {
			t.Errorf("%s max.message.bytes = %q", s.Name, got)
		}
		wantPolicy := "delete"
		if s.Compact {
			wantPolicy = "compact,delete"
		}
		if cfg[cfgCleanupPolicy] != wantPolicy {
			t.Errorf("%s cleanup.policy = %q, want %q", s.Name, cfg[cfgCleanupPolicy], wantPolicy)
		}
	}
}

// TestMaxMessageBytesExceedsTheEnvelopeCap: below it, an oversized webhook would wedge the relay
// behind a record the broker will not accept.
func TestMaxMessageBytesExceedsTheEnvelopeCap(t *testing.T) {
	t.Parallel()
	if MaxMessageBytes <= events.MaxEnvelopeBytes {
		t.Fatalf("max.message.bytes (%d) must exceed the %d byte envelope cap with margin",
			MaxMessageBytes, events.MaxEnvelopeBytes)
	}
}

// TestRetryAndDLQSiblingsAreNeverCompacted. Compaction keeps only the last record per key, and
// two different failed events for one merchant would silently collapse into one.
func TestRetryAndDLQSiblingsAreNeverCompacted(t *testing.T) {
	t.Parallel()
	for _, s := range AllTopicSpecs() {
		isSibling := strings.Contains(s.Name, retrySuffixPrefix) || strings.HasSuffix(s.Name, DLQSuffix)
		if isSibling && s.Compact {
			t.Errorf("%s is compacted; two failures for one key would collapse into one", s.Name)
		}
		if isSibling {
			want := RetentionRetry
			if strings.HasSuffix(s.Name, DLQSuffix) {
				want = RetentionDLQ
			}
			if s.Retention != want {
				t.Errorf("%s retention = %v, want %v", s.Name, s.Retention, want)
			}
		}
	}
}

// TestSiblingsShareTheParentsPartitionCount: a key must map to the same ordering domain on the
// retry topic as on the source topic, or a retried event lands on a partition unrelated to its
// siblings.
func TestSiblingsShareTheParentsPartitionCount(t *testing.T) {
	t.Parallel()
	byName := map[string]TopicSpec{}
	for _, s := range AllTopicSpecs() {
		byName[s.Name] = s
	}
	for name, s := range byName {
		base := BaseTopic(name)
		if base == name {
			continue
		}
		parent, ok := byName[base]
		if !ok {
			t.Errorf("%s has no parent spec", name)
			continue
		}
		if s.Partitions != parent.Partitions {
			t.Errorf("%s has %d partitions, parent has %d", name, s.Partitions, parent.Partitions)
		}
	}
}

func TestValidateRejectsUnsafeSpecs(t *testing.T) {
	t.Parallel()
	base := TopicSpec{Name: "pp.x.y.v1", Partitions: 3, ReplicationFactor: 3, MinInSyncReplicas: 2, Retention: time.Hour}
	cases := map[string]func(*TopicSpec){
		"no name":             func(s *TopicSpec) { s.Name = "" },
		"no partitions":       func(s *TopicSpec) { s.Partitions = 0 },
		"replication 2":       func(s *TopicSpec) { s.ReplicationFactor = 2 },
		"min isr 1":           func(s *TopicSpec) { s.MinInSyncReplicas = 1 },
		"min isr above rf":    func(s *TopicSpec) { s.MinInSyncReplicas = 4 },
		"inherited retention": func(s *TopicSpec) { s.Retention = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := base
			mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted %s", name)
			}
		})
	}
}

func TestDriftRendersReadably(t *testing.T) {
	t.Parallel()
	d := Drift{Topic: "pp.audit.v1", Key: cfgMinInSyncReplica, Want: "3", Got: "1"}
	s := d.String()
	for _, want := range []string{"pp.audit.v1", cfgMinInSyncReplica, "3", "1"} {
		if !strings.Contains(s, want) {
			t.Fatalf("Drift.String() = %q, missing %q", s, want)
		}
	}
}

func TestTopicsForCatalogCoversOnlyCatalogTopics(t *testing.T) {
	t.Parallel()
	catalog := map[string]struct{}{}
	for _, tpc := range events.AllTopics() {
		catalog[tpc] = struct{}{}
	}
	specs := TopicsForCatalog()
	if len(specs) == 0 {
		t.Fatal("TopicsForCatalog returned nothing")
	}
	for _, s := range specs {
		if _, ok := catalog[BaseTopic(s.Name)]; !ok {
			t.Errorf("%s is not derived from a catalog topic", s.Name)
		}
	}
	// One source + three tiers + one DLQ per catalog topic.
	if want := len(catalog) * (len(DefaultTiers) + 2); len(specs) != want {
		t.Fatalf("TopicsForCatalog returned %d specs, want %d", len(specs), want)
	}
}
