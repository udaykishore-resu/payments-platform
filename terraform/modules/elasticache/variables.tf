variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "replication_group_id" {
  description = "Replication group identifier."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,39}$", var.replication_group_id))
    error_message = "replication_group_id must be 1-40 lower-case alphanumeric/hyphen characters starting with a letter."
  }
}

variable "engine_version" {
  description = "Redis engine version, pinned."
  type        = string
  default     = "7.1"

  validation {
    condition     = can(regex("^[0-9]+\\.[0-9]+$", var.engine_version))
    error_message = "engine_version must be a pinned x.y version."
  }
}

variable "node_type" {
  description = "Cache node type."
  type        = string
  default     = "cache.r7g.large"

  validation {
    condition = contains([
      "cache.t4g.micro", "cache.t4g.small", "cache.t4g.medium",
      "cache.r6g.large", "cache.r6g.xlarge", "cache.r6g.2xlarge",
      "cache.r7g.large", "cache.r7g.xlarge", "cache.r7g.2xlarge",
    ], var.node_type)
    error_message = "node_type must be on the approved Graviton allowlist."
  }
}

variable "cluster_mode_enabled" {
  description = "Cluster mode (sharding). True in prod; a single-shard replication group in staging; a single node in dev."
  type        = bool
  default     = true
}

variable "num_node_groups" {
  description = "Shard count when cluster mode is enabled."
  type        = number
  default     = 3

  validation {
    condition     = var.num_node_groups >= 1 && var.num_node_groups <= 90
    error_message = "num_node_groups must be between 1 and 90."
  }
}

variable "replicas_per_node_group" {
  description = "Replicas per shard. One per shard, in a different AZ, is enough: nothing in this cache is authoritative, so the replica exists to avoid a latency cliff, not to protect data."
  type        = number
  default     = 1

  validation {
    condition     = var.replicas_per_node_group >= 0 && var.replicas_per_node_group <= 5
    error_message = "replicas_per_node_group must be between 0 and 5."
  }
}

variable "subnet_ids" {
  description = "Data-tier subnet IDs."
  type        = list(string)
}

variable "security_group_ids" {
  description = "Security groups."
  type        = list(string)
}

variable "kms_key_arn" {
  description = "CMK for encryption at rest."
  type        = string
}

variable "auth_token_secret_arn" {
  description = <<-EOT
    Secrets Manager secret holding the Redis AUTH token. Read at plan time
    rather than generated here: a generated token would sit in the Terraform
    state in plaintext, and the state file is not a secret store.
  EOT
  type = string
}

variable "maxmemory_policy" {
  description = <<-EOT
    Eviction policy. volatile-lru, NOT allkeys-lru: keys without a TTL are the
    rate-limit buckets, and evicting those ahead of cache entries that have a
    TTL would silently reset every limit under memory pressure - exactly when
    limits matter (deployment.md 2.4).
  EOT
  type    = string
  default = "volatile-lru"

  validation {
    condition     = contains(["volatile-lru", "volatile-lfu", "volatile-ttl", "volatile-random", "noeviction"], var.maxmemory_policy)
    error_message = "maxmemory_policy must be a volatile-* policy or noeviction. allkeys-* policies evict TTL-less rate-limit state and are not permitted."
  }
}

variable "snapshot_retention_limit" {
  description = "Days of automatic snapshots. Non-zero even though nothing here is authoritative: a snapshot makes a cold-start after a total loss a warm-start instead."
  type        = number
  default     = 1

  validation {
    condition     = var.snapshot_retention_limit >= 0 && var.snapshot_retention_limit <= 35
    error_message = "snapshot_retention_limit must be between 0 and 35."
  }
}

variable "snapshot_window" {
  description = "UTC snapshot window."
  type        = string
  default     = "03:00-04:00"
}

variable "maintenance_window" {
  description = "UTC maintenance window."
  type        = string
  default     = "sun:05:00-sun:06:00"
}

variable "cloudwatch_log_kms_key_arn" {
  description = "CMK for the slow-log and engine-log CloudWatch groups."
  type        = string
}

variable "log_retention_days" {
  description = "Retention for Redis logs."
  type        = number
  default     = 30
}

variable "apply_immediately" {
  description = "Apply modifications at once. False in prod."
  type        = bool
  default     = false
}
