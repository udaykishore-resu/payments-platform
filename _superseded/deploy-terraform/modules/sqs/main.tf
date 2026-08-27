# SQS + DLQ module (ADR-003). One queue per consumer, each with a dead-letter queue and a
# redrive policy — the infrastructure backing the "Messaging / Queue Failures" recovery design in
# docs/04-failure-recovery-design.md.

variable "queue_name" {
  type = string
}

variable "max_receive_count" {
  description = "Deliveries before a message is routed to the DLQ."
  type        = number
  default     = 5
}

variable "visibility_timeout_seconds" {
  type    = number
  default = 30
}

variable "message_retention_seconds" {
  type    = number
  default = 1209600 # 14 days — max SQS retention; gives operators a wide window to redrive per docs/08-runbook.md
}

resource "aws_kms_key" "queue" {
  description         = "CMK for ${var.queue_name} and its DLQ"
  enable_key_rotation = true
}

resource "aws_sqs_queue" "dlq" {
  name                      = "${var.queue_name}-dlq"
  message_retention_seconds = var.message_retention_seconds
  kms_master_key_id         = aws_kms_key.queue.id
}

resource "aws_sqs_queue" "this" {
  name                       = var.queue_name
  visibility_timeout_seconds = var.visibility_timeout_seconds
  message_retention_seconds  = var.message_retention_seconds
  kms_master_key_id          = aws_kms_key.queue.id

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount      = var.max_receive_count
  })
}

# Alarm on any DLQ depth > 0 — feeds the DLQDepthNonZero alert in docs/06-observability.md.
resource "aws_cloudwatch_metric_alarm" "dlq_not_empty" {
  alarm_name          = "${var.queue_name}-dlq-depth-nonzero"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.dlq.name }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_description   = "See docs/08-runbook.md section 5 (DLQ depth > 0 on a consumer queue)."
}

output "queue_url" {
  value = aws_sqs_queue.this.url
}

output "queue_arn" {
  value = aws_sqs_queue.this.arn
}

output "dlq_arn" {
  value = aws_sqs_queue.dlq.arn
}
