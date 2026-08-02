// Package events publishes durable outbox rows to the downstream messaging fabric (Amazon SNS ->
// per-consumer SQS, see ADR-003). The Publisher interface keeps internal/outbox decoupled from
// the AWS SDK, so the relay's retry/backoff/claim logic is unit-testable with a fake publisher.
package events

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type Publisher interface {
	// Publish delivers one event payload to the messaging fabric. Implementations must be safe
	// for concurrent use (the relay calls this from multiple goroutines).
	Publish(ctx context.Context, eventType string, payload []byte) error
}

// SQSPublisher publishes directly to a single SQS queue. In production this queue is normally
// subscribed to an SNS topic for fan-out to multiple consumers (ADR-003); publishing directly to
// SQS here (rather than SNS) is a deliberate simplification for this reference slice — swapping
// to an SNS Publish call is a small, isolated change confined to this file if/when true
// multi-consumer fan-out is wired up.
type SQSPublisher struct {
	client   *sqs.Client
	queueURL string
}

func NewSQSPublisher(ctx context.Context, region, queueURL string) (*SQSPublisher, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("events: load aws config: %w", err)
	}
	return &SQSPublisher{
		client:   sqs.NewFromConfig(cfg),
		queueURL: queueURL,
	}, nil
}

func (p *SQSPublisher) Publish(ctx context.Context, eventType string, payload []byte) error {
	body := string(payload)
	_, err := p.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(p.queueURL),
		MessageBody: aws.String(body),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"event_type": {
				DataType:    aws.String("String"),
				StringValue: aws.String(eventType),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("events: sqs send_message: %w", err)
	}
	return nil
}
