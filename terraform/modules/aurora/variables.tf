variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "cluster_identifier" {
  description = "Cluster identifier, e.g. pp-prod or pp-prod-secondary."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,58}$", var.cluster_identifier))
    error_message = "cluster_identifier must be 3-59 characters, lower-case, starting with a letter."
  }
}

variable "engine_version" {
  description = "Aurora PostgreSQL engine version, pinned. Never a bare major: a minor upgrade is a maintenance event with a failover, not something to inherit from a provider default."
  type        = string

  validation {
    condition     = can(regex("^1[5-9]\\.[0-9]+$", var.engine_version))
    error_message = "engine_version must be a pinned Aurora PostgreSQL 15.x or later version, e.g. 15.5."
  }
}

variable "instance_class" {
  description = "Instance class for writer and readers. db.serverless selects Aurora Serverless v2 scaling."
  type        = string

  validation {
    condition = contains([
      "db.serverless",
      "db.r6g.large", "db.r6g.xlarge", "db.r6g.2xlarge", "db.r6g.4xlarge", "db.r6g.8xlarge",
      "db.r7g.large", "db.r7g.xlarge", "db.r7g.2xlarge", "db.r7g.4xlarge", "db.r7g.8xlarge",
    ], var.instance_class)
    error_message = "instance_class must be db.serverless or a Graviton r6g/r7g class. Intel classes are ~20% worse price/performance for this workload and are not on the allowlist."
  }
}

variable "reader_count" {
  description = <<-EOT
    Number of reader instances. Prod primary runs 2 (one per remaining AZ) so
    an AZ loss leaves a reader to promote and a reader to serve reads.
  EOT
  type    = number
  default = 2

  validation {
    condition     = var.reader_count >= 0 && var.reader_count <= 15
    error_message = "reader_count must be between 0 and 15."
  }
}

variable "serverless_min_capacity" {
  description = "Aurora Serverless v2 minimum ACU. Only used when instance_class is db.serverless."
  type        = number
  default     = 0.5
}

variable "serverless_max_capacity" {
  description = "Aurora Serverless v2 maximum ACU."
  type        = number
  default     = 4
}

variable "subnet_ids" {
  description = "Data-tier subnet IDs. These subnets have no route to a NAT gateway or the internet."
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) >= 3
    error_message = "At least three subnets, one per AZ, are required for a multi-AZ Aurora cluster."
  }
}

variable "security_group_ids" {
  description = "Security groups for the cluster. Should be the aurora SG, whose only ingress is the node SG."
  type        = list(string)
}

variable "kms_key_arn" {
  description = "CMK for storage, automated backups and Performance Insights. Prod uses a Multi-Region Key so the Global secondary needs no re-encryption at promotion."
  type        = string
}

variable "database_name" {
  description = "Initial database name."
  type        = string
  default     = "payments"
}

variable "master_username" {
  description = "Master username. The password is generated and stored in Secrets Manager by the secrets module; the application never uses this account - it uses IAM database authentication (security.md 5.3)."
  type        = string
  default     = "pp_root"
}

variable "master_password_secret_arn" {
  description = "ARN of the Secrets Manager secret holding the master password. Terraform reads it at plan time rather than generating and storing the value in state."
  type        = string
  default     = null
}

variable "backup_retention_period" {
  description = "Days of automated backups and PITR window."
  type        = number
  default     = 35

  validation {
    condition     = var.backup_retention_period >= 1 && var.backup_retention_period <= 35
    error_message = "backup_retention_period must be between 1 and 35 days."
  }
}

variable "preferred_backup_window" {
  description = "UTC window for the daily snapshot. Chosen to sit inside the lowest-traffic hour and outside every scheduled batch job."
  type        = string
  default     = "02:00-03:00"

  validation {
    condition     = can(regex("^[0-2][0-9]:[0-5][0-9]-[0-2][0-9]:[0-5][0-9]$", var.preferred_backup_window))
    error_message = "preferred_backup_window must be hh:mm-hh:mm in UTC."
  }
}

variable "preferred_maintenance_window" {
  description = "UTC weekly maintenance window. Must not overlap the backup window."
  type        = string
  default     = "sun:04:00-sun:05:00"

  validation {
    condition     = can(regex("^(mon|tue|wed|thu|fri|sat|sun):[0-2][0-9]:[0-5][0-9]-(mon|tue|wed|thu|fri|sat|sun):[0-2][0-9]:[0-5][0-9]$", var.preferred_maintenance_window))
    error_message = "preferred_maintenance_window must be ddd:hh:mm-ddd:hh:mm in UTC."
  }
}

variable "deletion_protection" {
  description = "RDS-level deletion protection, independent of Terraform's prevent_destroy. Both exist because they fail differently: one stops an API call, the other stops a plan."
  type        = bool
  default     = true
}

variable "apply_immediately" {
  description = <<-EOT
    Apply modifications at once rather than in the maintenance window. False in
    prod: a parameter change that triggers a reboot or a failover must land in a
    window when the on-call engineer expects it, not at 15:00 on a Tuesday when
    the plan happened to be approved.
  EOT
  type    = bool
  default = false
}

variable "performance_insights_enabled" {
  description = "Performance Insights. On everywhere: the cost is small and the alternative during an incident is guessing."
  type        = bool
  default     = true
}

variable "performance_insights_retention_period" {
  description = "Performance Insights retention in days. 7 is free tier; 731 is the paid long-term option."
  type        = number
  default     = 7

  validation {
    condition     = var.performance_insights_retention_period == 7 || var.performance_insights_retention_period == 731 || (var.performance_insights_retention_period % 31 == 0 && var.performance_insights_retention_period <= 731)
    error_message = "performance_insights_retention_period must be 7, 731, or a multiple of 31 up to 731."
  }
}

variable "monitoring_interval" {
  description = "Enhanced monitoring granularity in seconds. 0 disables. 1 s is worth it on the writer: a 60 s average hides exactly the spikes that cause a p99 breach."
  type        = number
  default     = 1

  validation {
    condition     = contains([0, 1, 5, 10, 15, 30, 60], var.monitoring_interval)
    error_message = "monitoring_interval must be one of 0, 1, 5, 10, 15, 30, 60."
  }
}

variable "global_cluster_identifier" {
  description = "Attach this cluster to an Aurora Global cluster. Null for a standalone cluster."
  type        = string
  default     = null
}

variable "is_global_secondary" {
  description = "True for the DR-region cluster of a Global Database. A secondary has no master credentials, no database name and no backup window of its own."
  type        = bool
  default     = false
}

variable "log_min_duration_statement_ms" {
  description = "Log any statement slower than this. 200 ms against a 250 ms p99 API budget means anything logged here is already a latency problem."
  type        = number
  default     = 200

  validation {
    condition     = var.log_min_duration_statement_ms >= 0 && var.log_min_duration_statement_ms <= 60000
    error_message = "log_min_duration_statement_ms must be between 0 and 60000."
  }
}

variable "idle_in_transaction_session_timeout_ms" {
  description = <<-EOT
    Kill sessions idle inside a transaction. Load-bearing for this platform: an
    idle transaction holds its snapshot, which blocks vacuum, bloats the tables
    the money path reads, and pins the row locks a concurrent payment needs.
    60 s is far longer than any legitimate transaction here (the longest is a
    gateway call, and gateway calls are deliberately outside the transaction).
  EOT
  type    = number
  default = 60000
}

variable "statement_timeout_ms" {
  description = "Server-side statement timeout. Anything slower than 30 s on the money path has already breached every budget above it."
  type        = number
  default     = 30000
}

variable "max_connections" {
  description = "Override for max_connections. Null uses the engine default formula. The application connects through PgBouncer in transaction-pooling mode, so this bounds PgBouncer, not pods."
  type        = number
  default     = null
}

variable "iam_database_authentication_enabled" {
  description = "IAM database authentication. The application uses 15-minute IAM tokens; there is no long-lived database password to steal or rotate (security.md 5.3)."
  type        = bool
  default     = true
}

variable "enabled_cloudwatch_logs_exports" {
  description = "Log types exported to CloudWatch."
  type        = list(string)
  default     = ["postgresql"]
}

variable "cloudwatch_log_retention_days" {
  description = "Retention for the exported PostgreSQL logs."
  type        = number
  default     = 90
}

variable "cloudwatch_log_kms_key_arn" {
  description = "CMK for the CloudWatch log groups holding database logs."
  type        = string
}

variable "snapshot_identifier" {
  description = "Restore the cluster from this snapshot instead of creating it empty. Used by the restore drill (disaster-recovery.md 5.2) and by nothing else."
  type        = string
  default     = null
}

variable "ca_cert_identifier" {
  description = "RDS CA bundle. Pinned because the application verifies the server certificate with sslmode=verify-full; a silent CA change breaks every connection."
  type        = string
  default     = "rds-ca-rsa2048-g1"
}
