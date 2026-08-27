variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "name_suffix" {
  description = "Appended to every bucket name. Empty for the primary region; '-dr' for the replication destinations, since bucket names are globally unique."
  type        = string
  default     = ""
}

variable "kms_key_arn" {
  description = "CMK used for SSE-KMS on every bucket in this module instance. Must be the region-local key: S3 cannot use a key from another region."
  type        = string
}

variable "buckets" {
  description = <<-EOT
    Bucket definitions, keyed by short purpose name. The final bucket name is
    pp-<env>-<key><name_suffix>.

    object_lock_mode:
      COMPLIANCE - nobody, including the account root, can shorten the retention
                   or delete the object before its retention date. This is what
                   makes the audit archive and the KYC evidence admissible.
      GOVERNANCE - a specifically privileged role can override. Correct for
                   operational data (backups, settlement files), wrong for
                   evidence.
      null       - no Object Lock.

    See disaster-recovery.md 4.4 for the authoritative table.
  EOT

  type = map(object({
    description                    = string
    object_lock_mode               = optional(string)
    object_lock_days               = optional(number)
    noncurrent_version_expiry_days = optional(number, 90)
    expiry_days                    = optional(number)
    transition_to_ia_days          = optional(number)
    transition_to_glacier_ir_days  = optional(number)
    abort_multipart_days           = optional(number, 7)
    replicate                      = optional(bool, false)
    replication_time_control       = optional(bool, false)
    data_classification            = optional(string, "restricted")
    allow_log_delivery_service     = optional(bool, false)
    allow_config_service           = optional(bool, false)
    inventory_enabled              = optional(bool, false)
    sse_algorithm                  = optional(string, "aws:kms")
  }))

  validation {
    condition = alltrue([
      for k, v in var.buckets :
      v.object_lock_mode == null || contains(["GOVERNANCE", "COMPLIANCE"], coalesce(v.object_lock_mode, "GOVERNANCE"))
    ])
    error_message = "object_lock_mode must be GOVERNANCE, COMPLIANCE or null."
  }

  validation {
    condition = alltrue([
      for k, v in var.buckets :
      v.object_lock_mode == null || (v.object_lock_days != null && v.object_lock_days >= 1)
    ])
    error_message = "A bucket with object_lock_mode set must also set object_lock_days."
  }

  validation {
    condition = alltrue([
      for k, v in var.buckets :
      contains(["restricted", "confidential", "internal", "public"], v.data_classification)
    ])
    error_message = "data_classification must be one of: restricted, confidential, internal, public."
  }

  validation {
    condition     = alltrue([for k, v in var.buckets : can(regex("^[a-z0-9][a-z0-9-]{1,40}$", k))])
    error_message = "Bucket keys must be lower-case DNS-safe fragments."
  }

  validation {
    condition     = alltrue([for k, v in var.buckets : contains(["aws:kms", "AES256"], v.sse_algorithm)])
    error_message = "sse_algorithm must be aws:kms or AES256. AES256 exists for exactly one reason - ELB access logs do not support a customer-managed key - and should not be used anywhere else."
  }
}

variable "replica_bucket_arns" {
  description = "purpose => destination bucket ARN in the DR region. Required for every bucket with replicate = true. Empty on a replica-side module instance."
  type        = map(string)
  default     = {}
}

variable "replica_kms_key_arn" {
  description = "CMK in the destination region used to re-encrypt replicated objects. Required when any bucket has replicate = true."
  type        = string
  default     = null
}

variable "source_account_id" {
  description = "Account that owns the source buckets. On a replica instance this is used to accept replicated objects and to transfer object ownership to the destination account."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.source_account_id))
    error_message = "source_account_id must be a 12-digit AWS account ID."
  }
}

variable "log_archive_retention_days" {
  description = "Retention of the log archive bucket. 400 d matches the Kafka audit topic so a SIEM outage of up to a year is survivable."
  type        = number
  default     = 400

  validation {
    condition     = var.log_archive_retention_days >= 30 && var.log_archive_retention_days <= 3650
    error_message = "log_archive_retention_days must be between 30 and 3650."
  }
}

variable "enable_access_logging" {
  description = "Write S3 server access logs for every bucket into the log-archive bucket."
  type        = bool
  default     = true
}

variable "access_log_bucket_key" {
  description = "Which key in var.buckets is the server-access-log target."
  type        = string
  default     = "logs"
}
