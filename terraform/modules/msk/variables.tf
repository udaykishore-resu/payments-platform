variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "cluster_name" {
  description = "MSK cluster name. Appears in every kafka-cluster:* IAM resource ARN, so it is part of the authorization surface."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z0-9][a-zA-Z0-9-]{0,63}$", var.cluster_name))
    error_message = "cluster_name must be 1-64 alphanumeric/hyphen characters."
  }
}

variable "serverless" {
  description = "Use MSK Serverless instead of provisioned brokers. dev only: serverless has no broker-level configuration, so the correctness-critical settings below cannot be pinned."
  type        = bool
  default     = false
}

variable "kafka_version" {
  description = "Kafka version, pinned."
  type        = string
  default     = "3.6.0"

  validation {
    condition     = can(regex("^[0-9]+\\.[0-9]+\\.[0-9]+$", var.kafka_version))
    error_message = "kafka_version must be a pinned x.y.z version."
  }
}

variable "broker_instance_type" {
  description = "Broker instance type."
  type        = string
  default     = "kafka.m5.2xlarge"

  validation {
    condition = contains([
      "kafka.m5.large", "kafka.m5.xlarge", "kafka.m5.2xlarge", "kafka.m5.4xlarge",
      "kafka.m7g.large", "kafka.m7g.xlarge", "kafka.m7g.2xlarge", "kafka.m7g.4xlarge",
    ], var.broker_instance_type)
    error_message = "broker_instance_type must be an approved m5 or m7g Kafka broker class."
  }
}

variable "broker_count" {
  description = "Number of brokers. Must be a multiple of the subnet count so brokers are AZ-balanced; three across three AZs is the prod topology."
  type        = number
  default     = 3

  validation {
    condition     = var.broker_count >= 3 && var.broker_count <= 30
    error_message = "broker_count must be between 3 and 30."
  }
}

variable "broker_ebs_volume_size_gb" {
  description = "Per-broker EBS volume. Sized for 30 d of pp.payments at peak plus headroom; pp.audit.v1 goes to tiered storage rather than onto this volume."
  type        = number
  default     = 2000

  validation {
    condition     = var.broker_ebs_volume_size_gb >= 100 && var.broker_ebs_volume_size_gb <= 16384
    error_message = "broker_ebs_volume_size_gb must be between 100 and 16384."
  }
}

variable "storage_autoscaling_max_gb" {
  description = "Upper bound for broker storage autoscaling. Null disables autoscaling."
  type        = number
  default     = 4000
}

variable "subnet_ids" {
  description = "Streaming-tier subnet IDs, one per AZ."
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) == 3
    error_message = "Exactly three subnets are required: RF=3 with one broker per AZ."
  }
}

variable "security_group_ids" {
  description = "Security groups for the broker ENIs."
  type        = list(string)
}

variable "kms_key_arn" {
  description = "CMK for encryption at rest."
  type        = string
}

variable "cloudwatch_log_kms_key_arn" {
  description = "CMK for the broker-log CloudWatch group."
  type        = string
}

variable "log_retention_days" {
  description = "Broker log retention in CloudWatch."
  type        = number
  default     = 90
}

variable "enable_iam_auth" {
  description = "SASL/IAM authentication. The target state: no Kafka passwords exist, authorization is IAM, and every connect is a CloudTrail event."
  type        = bool
  default     = true
}

variable "enable_scram_auth" {
  description = "SASL/SCRAM alongside IAM. Present only for a migration window; leaving it on means a stealable password exists."
  type        = bool
  default     = false
}

variable "scram_secret_arns" {
  description = "Secrets Manager ARNs holding SCRAM credentials, required when enable_scram_auth is true. Must be encrypted with a customer-managed key: MSK refuses secrets under the AWS-managed key."
  type        = list(string)
  default     = []
}

variable "num_partitions_default" {
  description = "Default partition count for auto-created topics. Topics are created explicitly by platformctl, so this only bounds the damage of an accidental auto-creation."
  type        = number
  default     = 12
}

variable "log_retention_hours_default" {
  description = "Default topic retention. Per-topic values are set by platformctl from docs/events.md 5.2."
  type        = number
  default     = 720 # 30 days
}

variable "enhanced_monitoring" {
  description = "MSK monitoring level. PER_TOPIC_PER_PARTITION is the only level that exposes per-partition lag, which is the SLI for event-consumer."
  type        = string
  default     = "PER_TOPIC_PER_PARTITION"

  validation {
    condition     = contains(["DEFAULT", "PER_BROKER", "PER_TOPIC_PER_BROKER", "PER_TOPIC_PER_PARTITION"], var.enhanced_monitoring)
    error_message = "enhanced_monitoring must be one of DEFAULT, PER_BROKER, PER_TOPIC_PER_BROKER, PER_TOPIC_PER_PARTITION."
  }
}
