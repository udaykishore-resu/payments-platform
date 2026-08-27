//go:build integration

// Package-level integration tests. They need a real broker and are excluded from the default
// build; CI runs them with `go test -tags integration ./...` against the docker-compose cluster,
// and `go vet -tags integration` keeps them compiling even when nobody runs them — a
// build-tagged test that does not compile is a test that silently stops existing.
package kafka

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/events"
	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/ids"
)

// integrationConfig points at the local broker. It skips rather than fails when there is none, so
// a developer running the whole suite without docker-compose gets a clear skip instead of a
// mysterious timeout.
func integrationConfig(t *testing.T) Config {
	t.Helper()
	brokers := os.Getenv(EnvBrokers)
	if brokers == "" {
		t.Skip("set KAFKA_BROKERS to run the Kafka integration tests")
	}
	cfg := DefaultConfig()
	cfg.Brokers = splitAndTrim(brokers)
	cfg.ClientID = "kafka-integration-test"
	cfg.Environment = "local"
	cfg.Protocol = SecurityProtocol(strings.ToUpper(orDefault(os.Getenv(EnvProtocol), string(ProtocolPlaintext))))
	if cfg.usesSASL() {
		cfg.Username = os.Getenv(EnvUsername)
		cfg.Password = secret.New(os.Getenv(EnvPassword))
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("integration config: %v", err)
	}
	return cfg
}

func integrationMessage(topic string) ports.OutboxMessage {
	id := string(ids.New(ids.PrefixEvent))
	key := string(ids.New(ids.PrefixPayment))
	return ports.OutboxMessage{
		ID:           shared.EventID(id),
		TenantID:     shared.TenantID(testTenantID),
		Topic:        topic,
		Type:         "payment.captured.v1",
		AggregateID:  key,
		PartitionKey: key,
		Payload:      []byte(`{"specversion":"1.0","type":"payment.captured.v1"}`),
		Headers: map[string]string{
			events.HeaderEventID:   id,
			events.HeaderEventType: "payment.captured.v1",
			events.HeaderTenantID:  testTenantID,
		},
		OccurredAt: time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestIntegrationEnsureAndVerifyTopics(t *testing.T) {
	cfg := integrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := NewAdmin(cfg)
	if err != nil {
		t.Fatalf("NewAdmin: %v", err)
	}
	defer func() { _ = admin.Close() }()

	// A single-broker development cluster cannot satisfy RF 3, so the local specs are the
	// production shape with the replication reduced — which is exactly the drift VerifyTopics is
	// designed to catch, and why this test asserts on the drift rather than on success.
	specs := localSpecs()
	if err := admin.EnsureTopics(ctx, specs); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}
	// Idempotent: EnsureTopics runs on every local start-up.
	if err := admin.EnsureTopics(ctx, specs); err != nil {
		t.Fatalf("EnsureTopics is not idempotent: %v", err)
	}

	drifts, err := admin.VerifyTopics(ctx, specs)
	if err != nil {
		t.Fatalf("VerifyTopics: %v", err)
	}
	for _, d := range drifts {
		t.Logf("drift: %s", d)
	}

	// A spec the cluster cannot possibly satisfy must be reported, not silently accepted.
	bogus := append([]TopicSpec(nil), specs...)
	bogus = append(bogus, TopicSpec{
		Name: "pp.does-not-exist.v1", Partitions: 3, ReplicationFactor: 3,
		MinInSyncReplicas: 2, Retention: time.Hour, Why: "negative control",
	})
	drifts, err = admin.VerifyTopics(ctx, bogus)
	if err != nil {
		t.Fatalf("VerifyTopics: %v", err)
	}
	var sawMissing bool
	for _, d := range drifts {
		if d.Topic == "pp.does-not-exist.v1" && d.Key == "exists" {
			sawMissing = true
		}
	}
	if !sawMissing {
		t.Fatal("a missing topic was not reported as drift; readiness would pass against an unprovisioned cluster")
	}
}

func TestIntegrationProduceAndConsume(t *testing.T) {
	cfg := integrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := NewAdmin(cfg)
	if err != nil {
		t.Fatalf("NewAdmin: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.EnsureTopics(ctx, localSpecs()); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}

	producer, err := NewProducer(cfg)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	defer func() { _ = producer.Close() }()

	if err := producer.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	msg := integrationMessage(testTopic)
	if err := producer.Publish(ctx, msg); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	received := make(chan ports.OutboxMessage, 16)
	consumer, err := NewConsumer(cfg, WithConcurrency(2), WithMaxPollRecords(10))
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	consumeCtx, stop := context.WithCancel(ctx)
	defer stop()

	handler := ports.EventHandler(handlerFunc(func(_ context.Context, m ports.OutboxMessage) error {
		select {
		case received <- m:
		default:
		}
		return nil
	}))

	done := make(chan error, 1)
	go func() {
		done <- consumer.Subscribe(consumeCtx, []string{testTopic}, "pp.integration.projection.v1", handler)
	}()

	deadline := time.After(60 * time.Second)
	for {
		select {
		case got := <-received:
			if got.ID == msg.ID {
				stop()
				if err := <-done; err != nil {
					t.Fatalf("Subscribe returned %v", err)
				}
				if err := consumer.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				return
			}
		case <-deadline:
			stop()
			<-done
			t.Fatal("the published event was not consumed within 60s")
		}
	}
}

func TestIntegrationRetryRouterRepublishes(t *testing.T) {
	cfg := integrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := NewAdmin(cfg)
	if err != nil {
		t.Fatalf("NewAdmin: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.EnsureTopics(ctx, localSpecs()); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}

	producer, err := NewProducer(cfg)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	defer func() { _ = producer.Close() }()

	router, err := NewRetryRouter(producer, "pp.integration.projection.v1")
	if err != nil {
		t.Fatalf("NewRetryRouter: %v", err)
	}

	rec := testRecord(testTopic, nil)
	if err := router.Route(ctx, rec, apierror.New(apierror.CodeServiceUnavailable, "down")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if err := router.Route(ctx, rec, apierror.New(apierror.CodeValidationFailed, "poison")); err != nil {
		t.Fatalf("Route to DLQ: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------------------------

// localSpecs is the production topic table with replication reduced to what a single-broker
// development cluster can provide. It is a test-only relaxation and never reachable from
// production code, which is why it lives behind the integration tag.
func localSpecs() []TopicSpec {
	out := make([]TopicSpec, 0, 8)
	for _, s := range TopicsForCatalog() {
		if BaseTopic(s.Name) != testTopic {
			continue
		}
		// A one-broker development cluster cannot hold three replicas. EnsureTopics uses the
		// weaker validateForCreate for exactly this case; the strict Validate still governs the
		// declared production specs.
		s.ReplicationFactor = 1
		s.MinInSyncReplicas = 1
		out = append(out, s)
	}
	return out
}

type handlerFunc func(context.Context, ports.OutboxMessage) error

func (f handlerFunc) Handle(ctx context.Context, m ports.OutboxMessage) error { return f(ctx, m) }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
