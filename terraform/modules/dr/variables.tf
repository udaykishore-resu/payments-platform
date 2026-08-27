variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "fencing_table_name" {
  description = "Name of the DR control (fencing token) table."
  type        = string
  default     = "pp-dr-control"
}

variable "replica_regions" {
  description = "Regions the fencing table is replicated to as a Global Table. Empty makes it a single-region table (non-prod)."
  type        = list(string)
  default     = []
}

variable "kms_key_arn" {
  description = "CMK for the fencing table in this region."
  type        = string
}

variable "replica_kms_key_arns" {
  description = "region => CMK ARN in that region for the Global Table replicas."
  type        = map(string)
  default     = {}
}

variable "fence_reader_role_arns" {
  description = "Roles allowed to read the fencing item - payment-api and payment-orchestrator, which poll it every 10 s."
  type        = list(string)
  default     = []
}

variable "fence_writer_role_arns" {
  description = <<-EOT
    Roles allowed to increment the epoch. This should be exactly one: the
    break-glass promotion role that the DR runbook uses. No application
    workload may ever write the fence - if it could, a bug could promote a
    region.
  EOT
  type    = list(string)
  default = []

  validation {
    condition     = length(var.fence_writer_role_arns) <= 2
    error_message = "At most two principals may write the fencing token (the promotion role and the game-day role). More than that is a split-brain risk."
  }
}

variable "create_backup_vault" {
  description = "Create an AWS Backup vault and plan in this account."
  type        = bool
  default     = true
}

variable "backup_vault_kms_key_arn" {
  description = "CMK for the backup vault."
  type        = string
}

variable "backup_cross_account_vault_arn" {
  description = <<-EOT
    Vault ARN in the pp-backup-vault account that daily copies are sent to. The
    trust is one-way: a production administrator can write into it and cannot
    delete from it (disaster-recovery.md 5.1). Empty disables the copy.
  EOT
  type    = string
  default = ""
}

variable "backup_schedule_cron" {
  description = "Cron for the daily backup. Sits after the Aurora snapshot window so the two do not contend for I/O."
  type        = string
  default     = "cron(0 4 * * ? *)"
}

variable "backup_retention_days" {
  description = "Retention of backups in the local vault."
  type        = number
  default     = 35

  validation {
    condition     = var.backup_retention_days >= 7 && var.backup_retention_days <= 365
    error_message = "backup_retention_days must be between 7 and 365."
  }
}

variable "backup_cold_storage_after_days" {
  description = "Days before a recovery point moves to cold storage. Null disables the transition."
  type        = number
  default     = null
}

variable "backup_resource_arns" {
  description = "Resources selected into the backup plan. Explicit ARNs rather than a tag selector: a tag selector silently stops backing something up the moment somebody edits a tag."
  type        = list(string)
  default     = []
}

variable "vault_lock_enabled" {
  description = <<-EOT
    Apply AWS Backup Vault Lock in compliance mode. Once the changeable-for
    window expires this is IRREVERSIBLE - nobody, including the account root and
    AWS support, can shorten retention or delete a recovery point. That is the
    point, and it is also why it defaults to false and is enabled deliberately.
  EOT
  type    = bool
  default = false
}

variable "vault_lock_changeable_for_days" {
  description = "Grace period during which the vault lock can still be removed. Three days is the minimum AWS allows and the minimum a mistake needs to be noticed."
  type        = number
  default     = 3

  validation {
    condition     = var.vault_lock_changeable_for_days >= 3
    error_message = "vault_lock_changeable_for_days must be at least 3."
  }
}

variable "backup_notification_sns_topic_arn" {
  description = "SNS topic receiving backup job failure notifications. Owned by the observability module and wired here by the env stack."
  type        = string
  default     = ""
}
