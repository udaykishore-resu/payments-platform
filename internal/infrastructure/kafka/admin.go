package kafka

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/udaykishore-resu/payments-platform/internal/events"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TopicSpec is the desired configuration of one topic.
//
// Topic configuration is code, in this file, rather than a Terraform module or a runbook step,
// for a reason that only shows up during an incident: the numbers below are correctness
// properties, not capacity settings. `min.insync.replicas=2` with `acks=all` is what makes a
// broker loss survivable without data loss, and `unclean.leader.election.enable=false` is what
// stops a leader election from trading `payment.captured.v1` records for availability. A setting
// that lives only in infrastructure-as-code drifts the day someone edits a topic by hand to
// unblock a deploy — which is why VerifyTopics exists and fails readiness.
type TopicSpec struct {
	Name              string
	Partitions        int32
	ReplicationFactor int16
	// MinInSyncReplicas is 2 everywhere except audit, which is 3.
	MinInSyncReplicas int
	// Retention is how long records are kept. Zero means "broker default", which is never used
	// here — every topic states its own.
	Retention time.Duration
	// Compact selects log compaction instead of deletion.
	Compact bool
	// Why documents the choice at the point of the choice.
	Why string
}

// Topic-level configuration keys, spelled once.
const (
	cfgRetentionMs      = "retention.ms"
	cfgCleanupPolicy    = "cleanup.policy"
	cfgMinInSyncReplica = "min.insync.replicas"
	cfgUncleanElection  = "unclean.leader.election.enable"
	cfgMaxMessageBytes  = "max.message.bytes"
	cfgCompressionType  = "compression.type"

	// MaxMessageBytes is 1 MiB: above the 256 KiB envelope cap (rule E8) with margin for
	// compression and batch metadata, and below anything that would let an oversized webhook
	// wedge the relay behind a record the broker will not accept.
	MaxMessageBytes = 1024 * 1024
)

// Retention constants from docs/events.md §5.2.
const (
	RetentionMerchants = 30 * 24 * time.Hour
	RetentionConfig    = 7 * 24 * time.Hour
	RetentionPayments  = 30 * 24 * time.Hour
	RetentionHealth    = 24 * time.Hour
	RetentionWebhooks  = 7 * 24 * time.Hour
	RetentionAudit     = 400 * 24 * time.Hour
	RetentionRetry     = 7 * 24 * time.Hour
	RetentionDLQ       = 30 * 24 * time.Hour
)

// sourceTopics is the topic table from docs/events.md §5.2, before the retry and DLQ siblings are
// derived. Every number carries its reasoning because six months from now the question will be
// "can we reduce the payments partition count", and the answer needs to be in the same file as
// the number.
var sourceTopics = []TopicSpec{
	{
		Name: "pp.merchants.merchant.v1", Partitions: 12, ReplicationFactor: 3,
		MinInSyncReplicas: 2, Retention: RetentionMerchants,
		Why: "50 000 merchants across 500 tenants, changing rarely. Twelve is about ordering domains and consumer parallelism, not volume.",
	},
	{
		Name: "pp.config.configuration.v1", Partitions: 12, ReplicationFactor: 3,
		MinInSyncReplicas: 2, Retention: RetentionConfig, Compact: true,
		Why: "Compacted so a consumer that lost its cache rebuilds the CURRENT configuration for every merchant by reading from the beginning. This is why configuration.published.v1 carries the full document rather than a diff.",
	},
	{
		Name: "pp.payments.payment.v1", Partitions: 48, ReplicationFactor: 3,
		MinInSyncReplicas: 2, Retention: RetentionPayments,
		Why: "Sized from throughput, not broker count: 15 000 TPS peak x ~4 events per payment is ~60 000 events/s; ~2 000 events/s per instance with a database write each means ~30 instances at peak. 48 gives headroom and divides evenly by 2,3,4,6,8,12,16,24. Partitions can be increased but never decreased, and increasing them re-hashes keys and breaks per-key ordering across the change — so this is a three-year number.",
	},
	{
		Name: "pp.gateways.health.v1", Partitions: 6, ReplicationFactor: 3,
		MinInSyncReplicas: 2, Retention: RetentionHealth, Compact: true,
		Why: "A handful of gateways x six operations. More partitions would spread a tiny keyspace thinly and slow compaction for no benefit. Compacted because only the current state matters and a restarting router wants it in one read.",
	},
	{
		Name: "pp.webhooks.inbound.v1", Partitions: 24, ReplicationFactor: 3,
		MinInSyncReplicas: 2, Retention: RetentionWebhooks,
		Why: "Volume tracks payments but each record is larger and processing is heavier. Half of payments' count with the same headroom logic.",
	},
	{
		Name: "pp.audit.v1", Partitions: 12, ReplicationFactor: 3,
		MinInSyncReplicas: 3, Retention: RetentionAudit,
		Why: "min.insync.replicas=3, uniquely: a single broker loss stalls audit production, which is the CORRECT failure mode here because an audit gap is a compliance finding and the audit write path is asynchronous and WAL-buffered, so a stall degrades nothing user-facing. 400 days in Kafka lets a SIEM outage of up to a year be survived without an S3 restore; the 7-year WORM requirement is met by S3 Object Lock, not by this topic.",
	},
}

// AllTopicSpecs returns the source topics plus their retry-tier and DLQ siblings.
//
// The siblings are derived rather than listed so that adding a source topic cannot leave its DLQ
// unprovisioned — a failure mode that stays invisible until the first poison message, at which
// point the router cannot publish and the partition halts.
func AllTopicSpecs() []TopicSpec {
	out := make([]TopicSpec, 0, len(sourceTopics)*(len(DefaultTiers)+2))
	for _, s := range sourceTopics {
		out = append(out, s)
		for _, t := range DefaultTiers {
			out = append(out, TopicSpec{
				Name: s.Name + t.Suffix, Partitions: s.Partitions, ReplicationFactor: s.ReplicationFactor,
				MinInSyncReplicas: s.MinInSyncReplicas, Retention: RetentionRetry,
				// Never compacted, even when the parent is: compaction keeps only the last record
				// per key, and two different failed events for one merchant would silently
				// collapse into one retry.
				Compact: false,
				Why:     "delay tier " + t.Suffix + " of " + s.Name + "; same partition count so a key maps to the same ordering domain on both",
			})
		}
		out = append(out, TopicSpec{
			Name: s.Name + DLQSuffix, Partitions: s.Partitions, ReplicationFactor: s.ReplicationFactor,
			MinInSyncReplicas: s.MinInSyncReplicas, Retention: RetentionDLQ, Compact: false,
			Why: "dead letter for " + s.Name + "; 30 days so a poison message survives a holiday period long enough to be triaged",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Configs renders the spec as the broker's topic-level configuration map.
func (s TopicSpec) Configs() map[string]string {
	policy := "delete"
	if s.Compact {
		// "compact,delete" rather than plain "compact": compaction preserves the last record per
		// key forever, and the delete half bounds how much *history* accumulates behind it. Plain
		// compaction on a 12-partition topic with 50 000 keys grows without limit.
		policy = "compact,delete"
	}
	return map[string]string{
		cfgRetentionMs:      strconv.FormatInt(s.Retention.Milliseconds(), 10),
		cfgCleanupPolicy:    policy,
		cfgMinInSyncReplica: strconv.Itoa(s.MinInSyncReplicas),
		// Everywhere, without exception. Unclean election trades data loss for availability, and
		// on a topic carrying payment.captured.v1 that trade is never acceptable.
		cfgUncleanElection: "false",
		cfgMaxMessageBytes: strconv.Itoa(MaxMessageBytes),
		cfgCompressionType: "producer",
	}
}

// Validate rejects a spec that would provision a topic we could not safely use.
func (s TopicSpec) Validate() error {
	switch {
	case s.Name == "":
		return apierror.New(apierror.CodeConfigurationInvalid, "kafka: topic spec has no name")
	case s.Partitions < 1:
		return apierror.Newf(apierror.CodeConfigurationInvalid, "kafka: topic %s needs at least one partition", s.Name)
	case s.ReplicationFactor < 3:
		// RF 2 cannot tolerate a broker loss during a rolling restart, and a rolling restart is a
		// routine operation.
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"kafka: topic %s has replication factor %d; 3 is required so the replicas land in 3 AZs",
			s.Name, s.ReplicationFactor)
	case s.MinInSyncReplicas < 2 || s.MinInSyncReplicas > int(s.ReplicationFactor):
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"kafka: topic %s has min.insync.replicas %d, which must be between 2 and the replication factor",
			s.Name, s.MinInSyncReplicas)
	case s.Retention <= 0:
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"kafka: topic %s must state its own retention rather than inheriting a broker default", s.Name)
	}
	return nil
}

// validateForCreate is the weaker check EnsureTopics uses.
//
// It deliberately does not enforce replication factor 3 or min.insync.replicas 2, because
// EnsureTopics is a local-development tool and a one-broker docker-compose cluster physically
// cannot satisfy either — a create path that refused would mean local development could not
// provision its own topics, and the usual outcome of that is somebody creating them by hand with
// no configuration at all. The strict Validate still governs the declared specs (asserted by
// admin_test.go) and VerifyTopics still fails readiness on the real cluster, so production keeps
// the guarantee.
func (s TopicSpec) validateForCreate() error {
	switch {
	case s.Name == "":
		return apierror.New(apierror.CodeConfigurationInvalid, "kafka: topic spec has no name")
	case s.Partitions < 1:
		return apierror.Newf(apierror.CodeConfigurationInvalid, "kafka: topic %s needs at least one partition", s.Name)
	case s.ReplicationFactor < 1:
		return apierror.Newf(apierror.CodeConfigurationInvalid, "kafka: topic %s needs at least one replica", s.Name)
	case s.MinInSyncReplicas < 1 || s.MinInSyncReplicas > int(s.ReplicationFactor):
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"kafka: topic %s has min.insync.replicas %d, which must be between 1 and the replication factor",
			s.Name, s.MinInSyncReplicas)
	case s.Retention <= 0:
		return apierror.Newf(apierror.CodeConfigurationInvalid,
			"kafka: topic %s must state its own retention", s.Name)
	}
	return nil
}

// Admin performs topic provisioning and verification.
//
// It uses kmsg requests through a plain kgo client rather than the kadm helper package, because
// kadm is a separate Go module and this repository's go.mod is deliberately small — an admin
// convenience layer is not worth a dependency on a module we would then have to keep in step with
// the client.
type Admin struct {
	client    *kgo.Client
	closeOnce sync.Once
	timeout   time.Duration
}

// NewAdmin connects an admin client.
func NewAdmin(cfg Config) (*Admin, error) {
	opts, err := cfg.ClientOptions()
	if err != nil {
		return nil, err
	}
	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeDependencyFailure, "kafka: creating admin client")
	}
	return &Admin{client: client, timeout: 30 * time.Second}, nil
}

// Close shuts the admin client down. Idempotent and immediate — an admin client buffers nothing.
func (a *Admin) Close() error {
	a.closeOnce.Do(func() { a.client.Close() })
	return nil
}

// EnsureTopics creates every topic that does not exist.
//
// For local development and CI only. It never alters an existing topic, and that restriction is
// deliberate: silently widening a partition count in production would re-hash every key and break
// per-key ordering across the change — the one operation in this file that destroys data
// integrity without erroring. Production topics are provisioned by the platform team through
// infrastructure-as-code, and VerifyTopics is what catches the difference.
//
// An already-existing topic is not an error: EnsureTopics is expected to run on every local
// start-up.
func (a *Admin) EnsureTopics(ctx context.Context, specs []TopicSpec) error {
	if len(specs) == 0 {
		return nil
	}
	req := kmsg.NewCreateTopicsRequest()
	req.TimeoutMillis = int32(a.timeout.Milliseconds())
	for _, s := range specs {
		if err := s.validateForCreate(); err != nil {
			return err
		}
		t := kmsg.NewCreateTopicsRequestTopic()
		t.Topic = s.Name
		t.NumPartitions = s.Partitions
		t.ReplicationFactor = s.ReplicationFactor
		for k, v := range s.Configs() {
			c := kmsg.NewCreateTopicsRequestTopicConfig()
			c.Name = k
			value := v
			c.Value = &value
			t.Configs = append(t.Configs, c)
		}
		// Deterministic order so a request is diffable between runs.
		sort.Slice(t.Configs, func(i, j int) bool { return t.Configs[i].Name < t.Configs[j].Name })
		req.Topics = append(req.Topics, t)
	}

	resp, err := req.RequestWith(ctx, a.client)
	if err != nil {
		return apierror.Wrap(err, apierror.CodeDependencyFailure, "kafka: create topics request failed")
	}
	var problems []string
	for _, t := range resp.Topics {
		if t.ErrorCode == 0 {
			continue
		}
		e := kerr.ErrorForCode(t.ErrorCode)
		if kerr.TopicAlreadyExists.Code == t.ErrorCode {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: %v", t.Topic, e))
	}
	if len(problems) > 0 {
		return apierror.Newf(apierror.CodeDependencyFailure,
			"kafka: %d topic(s) could not be created: %s", len(problems), strings.Join(problems, "; "))
	}
	return nil
}

// Drift is one difference between a topic's desired and actual configuration.
type Drift struct {
	Topic string
	Key   string
	Want  string
	Got   string
}

// String renders a drift for an operator: what topic, which setting, what it should be and what
// it actually is — everything needed to write the fix without re-running the check.
func (d Drift) String() string {
	return fmt.Sprintf("%s: %s is %q, want %q", d.Topic, d.Key, d.Got, d.Want)
}

// VerifyTopics reports every drift between the desired configuration and the cluster.
//
// It is called at startup and its result gates readiness. That is a strong stance and it is the
// right one: a process that starts happily against a topic with `min.insync.replicas=1` is a
// process that will acknowledge a payment event that a single broker restart then loses, and
// nothing about that failure is visible until a reconciliation months later. Failing readiness
// makes the misconfiguration an immediate, obvious, blocking problem — which is what a
// misconfiguration of a durability setting deserves.
//
// The checks are deliberately asymmetric:
//
//   - A missing topic is drift. Producing to an auto-created topic gets broker defaults —
//     usually RF 1 — which is silent data loss waiting to happen.
//   - A partition count that is *higher* than desired is drift but not fatal on its own; it is
//     reported so an operator can reconcile the spec, because partitions cannot be reduced.
//   - A *lower* partition count is fatal: the throughput sizing assumed the higher number.
//   - Retention that is longer than desired is reported, not fatal. Longer retention costs money,
//     never correctness. Shorter retention is fatal: it silently shortens the replay window the
//     dedup retention was sized against.
func (a *Admin) VerifyTopics(ctx context.Context, specs []TopicSpec) ([]Drift, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	byName := make(map[string]TopicSpec, len(specs))
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	sort.Strings(names)

	actualPartitions, err := a.partitionCounts(ctx, names)
	if err != nil {
		return nil, err
	}
	actualConfigs, err := a.topicConfigs(ctx, names)
	if err != nil {
		return nil, err
	}

	var drifts []Drift
	for _, name := range names {
		spec := byName[name]

		got, exists := actualPartitions[name]
		if !exists {
			drifts = append(drifts, Drift{Topic: name, Key: "exists", Want: "true", Got: "false"})
			continue
		}
		if got != spec.Partitions {
			drifts = append(drifts, Drift{
				Topic: name, Key: "partitions",
				Want: strconv.Itoa(int(spec.Partitions)), Got: strconv.Itoa(int(got)),
			})
		}

		want := spec.Configs()
		have := actualConfigs[name]
		for _, key := range sortedKeys(want) {
			// compression.type=producer is the default on most clusters and brokers report it
			// inconsistently; it is not a durability property, so it is not verified.
			if key == cfgCompressionType {
				continue
			}
			if have[key] != want[key] {
				drifts = append(drifts, Drift{Topic: name, Key: key, Want: want[key], Got: have[key]})
			}
		}
	}
	return drifts, nil
}

// VerifyPlatformTopics verifies the whole catalog and returns an error when anything drifted.
//
// It is the readiness-probe entry point: one call, one verdict, and the error names every drift
// so an operator does not have to run the check again with more logging.
func (a *Admin) VerifyPlatformTopics(ctx context.Context) error {
	drifts, err := a.VerifyTopics(ctx, AllTopicSpecs())
	if err != nil {
		return err
	}
	if len(drifts) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(drifts))
	for _, d := range drifts {
		msgs = append(msgs, d.String())
	}
	return apierror.Newf(apierror.CodeConfigurationInvalid,
		"kafka: %d topic configuration drift(s): %s", len(drifts), strings.Join(msgs, "; "))
}

// partitionCounts reads the live partition count per topic.
func (a *Admin) partitionCounts(ctx context.Context, names []string) (map[string]int32, error) {
	req := kmsg.NewMetadataRequest()
	// Never auto-create from a verification path: a check that provisions what it was checking
	// for always passes, which makes it worse than no check.
	req.AllowAutoTopicCreation = false
	for _, n := range names {
		t := kmsg.NewMetadataRequestTopic()
		name := n
		t.Topic = &name
		req.Topics = append(req.Topics, t)
	}
	resp, err := req.RequestWith(ctx, a.client)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeDependencyFailure, "kafka: metadata request failed")
	}
	out := make(map[string]int32, len(resp.Topics))
	for _, t := range resp.Topics {
		if t.Topic == nil {
			continue
		}
		if t.ErrorCode != 0 {
			// UNKNOWN_TOPIC_OR_PARTITION means it does not exist, which VerifyTopics reports as
			// drift rather than as a request failure.
			continue
		}
		out[*t.Topic] = int32(len(t.Partitions))
	}
	return out, nil
}

// topicConfigs reads the live topic-level configuration.
func (a *Admin) topicConfigs(ctx context.Context, names []string) (map[string]map[string]string, error) {
	req := kmsg.NewDescribeConfigsRequest()
	for _, n := range names {
		r := kmsg.NewDescribeConfigsRequestResource()
		r.ResourceType = kmsg.ConfigResourceTypeTopic
		r.ResourceName = n
		req.Resources = append(req.Resources, r)
	}
	resp, err := req.RequestWith(ctx, a.client)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeDependencyFailure, "kafka: describe configs request failed")
	}
	out := make(map[string]map[string]string, len(resp.Resources))
	for _, res := range resp.Resources {
		if res.ErrorCode != 0 {
			continue
		}
		cfg := make(map[string]string, len(res.Configs))
		for _, c := range res.Configs {
			if c.Value != nil {
				cfg[c.Name] = *c.Value
			}
		}
		out[res.ResourceName] = cfg
	}
	return out, nil
}

// TopicsForCatalog returns the specs for exactly the topics the event registry publishes to,
// plus their siblings. It exists so that a deployable that consumes only one context can verify
// only what it uses, rather than failing readiness over a topic it never touches.
func TopicsForCatalog() []TopicSpec {
	wanted := map[string]struct{}{}
	for _, t := range events.AllTopics() {
		wanted[t] = struct{}{}
	}
	var out []TopicSpec
	for _, s := range AllTopicSpecs() {
		if _, ok := wanted[BaseTopic(s.Name)]; ok {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
