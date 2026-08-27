package main

import (
	"context"
	"log/slog"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/registry"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/simulator"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	appwebhook "github.com/udaykishore-resu/payments-platform/internal/application/webhook"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// webhookReceivedType is the event this consumer exists to act on.
const webhookReceivedType = "webhook.received.v1"

// newGatewayRegistry builds the adapter set whose verifiers re-authenticate a stored delivery.
func newGatewayRegistry(withSimulator bool) (*registry.Registry, error) {
	var engine *simulator.Engine
	if withSimulator {
		engine = simulator.NewEngine(simulator.EngineOptions{})
	}
	return registry.NewWithBuiltIn(engine)
}

// webhookProjection applies `webhook.received.v1` by running the asynchronous processor.
//
// # Why this is the consumer's job and not the ingress's
//
// The ingress answers in 50 ms and does nothing but verify, store and publish, because every
// major gateway retries a slow webhook with escalating concurrency and several disable an
// endpoint that stays slow. Interpretation — matching the event to a payment, transitioning it,
// posting the ledger entry — is unbounded work and belongs here, where being slow costs consumer
// lag rather than an endpoint the gateway has switched off.
//
// # Why the processor re-verifies rather than trusting the stored record
//
// The stored row says a signature was checked at ingest. Re-verifying costs one HMAC and closes
// the gap where a row was written or edited by something other than the ingress. It is the reason
// this projection needs a secrets provider at all, and the reason this deployable could not be
// wired before one existed.
//
// # Why an unhandled type is acknowledged
//
// Returning an error for an event this group does not project would block the partition for every
// *other* type on it, turning "somebody published a new event" into an outage of an unrelated
// projection. Acknowledging with a DEBUG line makes the unhandled traffic visible instead.
type webhookProjection struct {
	processor *appwebhook.Processor
	group     string
	logger    *slog.Logger
}

// Handle processes one message.
func (w webhookProjection) Handle(ctx context.Context, msg ports.OutboxMessage) error {
	if msg.Type != webhookReceivedType {
		w.logger.Debug("event acknowledged with no registered projection",
			slog.String(telemetry.KeyEventType, msg.Type),
			slog.String(telemetry.KeyTopic, msg.Topic),
			slog.String("group", w.group))
		return nil
	}
	id := shared.WebhookID(msg.AggregateID)
	if id.String() == "" {
		// A malformed aggregate id is not retryable: redelivering it produces the same malformed
		// id forever and blocks the partition behind it. It is acknowledged and reported, which
		// is the shape a poison message needs.
		w.logger.Error("webhook event carries no usable aggregate id",
			slog.String(telemetry.KeyEventType, msg.Type),
			slog.String("aggregate_id", msg.AggregateID))
		return nil
	}
	res, err := w.processor.Process(ctx, id)
	if err != nil {
		// Returned rather than swallowed: a processing failure is genuinely retryable — the
		// payment may be locked, the database may be failing over — and the consumer's own retry
		// and DLQ machinery is what bounds it. Acknowledging here would lose a payment outcome.
		return apierror.Wrapf(err, apierror.CodeOf(err), "processing webhook %s", id)
	}
	w.logger.Info("webhook processed",
		slog.String("webhook_id", id.String()),
		slog.String("kind", string(res.Kind)),
		slog.String("outcome", res.Outcome),
		slog.Bool("applied", res.Applied))
	return nil
}
