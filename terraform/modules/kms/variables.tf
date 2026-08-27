variable "environment" {
  description = "Environment name. Drives key aliases and the key policy's environment condition."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "purposes" {
  description = <<-EOT
    Per-purpose CMKs to create. One key per purpose, never a shared "everything"
    key: a key is a blast-radius boundary and a separate grant history in
    CloudTrail. Key names become alias/pp-<env>-<purpose>.
  EOT
  type    = list(string)
  default = ["rds", "s3", "secrets", "eks", "msk", "elasticache", "ebs", "logs", "dynamodb", "backup"]

  validation {
    condition     = length(var.purposes) > 0 && alltrue([for p in var.purposes : can(regex("^[a-z][a-z0-9-]{1,30}$", p))])
    error_message = "Each purpose must be lower-case alphanumeric/hyphen, 2-31 characters."
  }
}

variable "multi_region" {
  description = <<-EOT
    Create Multi-Region Keys. Required in prod: the Aurora Global secondary, the
    Secrets Manager replica secrets and the S3 CRR destination all decrypt
    ciphertext produced in the primary region without re-encryption
    (disaster-recovery.md 4.5). Non-prod has no second region, so single-region
    keys are correct and cheaper.
  EOT
  type    = bool
  default = false
}

variable "replica_source_key_arns" {
  description = <<-EOT
    When set, this module instance creates aws_kms_replica_key resources in its
    own region for each purpose => primary key ARN given, instead of creating
    primary keys. This is how the DR region gets the same key material and the
    same key-id suffix. Leave null for a primary-key instance.
  EOT
  type    = map(string)
  default = null
}

variable "deletion_window_in_days" {
  description = "Pending-deletion window. Long on purpose: key deletion is irreversible and a 30-day window is 30 days of chances to notice."
  type        = number
  default     = 30

  validation {
    condition     = var.deletion_window_in_days >= 7 && var.deletion_window_in_days <= 30
    error_message = "deletion_window_in_days must be between 7 and 30."
  }
}

variable "enable_key_rotation" {
  description = "Annual automatic rotation of the backing material. security.md 5.3 requires 365 d. Never false in prod."
  type        = bool
  default     = true
}

variable "key_administrator_arns" {
  description = <<-EOT
    Principals allowed to administer (not use) the keys: schedule deletion,
    change the policy, create grants. In prod this is the break-glass role and
    the Terraform deployment role only. Never a human's day-to-day role.
  EOT
  type = list(string)

  validation {
    condition     = length(var.key_administrator_arns) > 0
    error_message = "At least one key administrator ARN is required; a key with no administrator cannot be managed."
  }
}

variable "key_users" {
  description = <<-EOT
    purpose => list of IAM principal ARNs that may use the key for cryptographic
    operations. These are the deployable IRSA role ARNs (constructed from their
    deterministic names in the env stack, so there is no dependency cycle
    between this module and the EKS module).

    A purpose absent from this map gets no IAM-principal grant at all and is
    usable only by the AWS service principals listed in service_principals.
  EOT
  type    = map(list(string))
  default = {}
}

variable "service_principals" {
  description = <<-EOT
    purpose => list of AWS service principals granted use of the key through the
    key policy, each constrained by kms:ViaService to this region. This is how
    logs.<region>.amazonaws.com encrypts a log group, or rds.amazonaws.com
    encrypts a snapshot, without any IAM role being involved.
  EOT
  type    = map(list(string))
  default = {}
}

variable "grant_autoscaling_service_linked_role" {
  description = "Grant the EC2 Auto Scaling service-linked role use of the EBS key. Without this, an ASG cannot launch instances with encrypted volumes and the failure surfaces as an opaque capacity error."
  type        = bool
  default     = false
}

variable "account_id" {
  description = "AWS account ID owning the keys."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.account_id))
    error_message = "account_id must be a 12-digit AWS account ID."
  }
}
