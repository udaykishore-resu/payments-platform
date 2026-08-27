variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "path_prefix" {
  description = <<-EOT
    Root of the secret path scheme, normally /<env>. The full scheme is
    /{env}/{tenant_id}/{merchant_id}/{gateway}/{purpose} (security.md 5.1) and
    the per-tenant leaves are created at runtime by control-plane-api, not by
    Terraform. This module creates only the platform-level secrets and the IAM
    surface that constrains the whole tree.
  EOT
  type = string

  validation {
    condition     = can(regex("^/[a-z0-9][a-z0-9-]*$", var.path_prefix))
    error_message = "path_prefix must start with / and contain only lower-case alphanumerics and hyphens."
  }
}

variable "kms_key_arn" {
  description = "CMK encrypting every secret in this module. Multi-Region in prod so the replicas decrypt without re-encryption."
  type        = string
}

variable "replica_regions" {
  description = <<-EOT
    Regions to maintain replica secrets in. Secrets Manager keeps the replica in
    sync itself, so a failover needs no promotion step and the cross-region RTO
    for secrets is 0 (disaster-recovery.md 1.1).
  EOT
  type = list(object({
    region      = string
    kms_key_arn = string
  }))
  default = []
}

variable "secrets" {
  description = <<-EOT
    Platform-level secrets to create. Terraform creates the *container* and its
    access policy; the VALUE is never in Terraform. See terraform/README.md,
    "Not managed here".
  EOT

  type = map(object({
    description          = string
    rotation_days        = optional(number)
    rotation_lambda_name = optional(string)
    recovery_window_days = optional(number, 30)
    readers              = optional(list(string), [])
    data_classification  = optional(string, "restricted")
  }))
  default = {}

  validation {
    condition     = alltrue([for k, v in var.secrets : can(regex("^[a-z0-9][a-z0-9/_-]{0,120}$", k))])
    error_message = "Secret keys must be lower-case path fragments."
  }

  validation {
    condition = alltrue([
      for k, v in var.secrets :
      v.rotation_days == null || (v.rotation_days >= 1 && v.rotation_days <= 365)
    ])
    error_message = "rotation_days must be between 1 and 365."
  }

  validation {
    condition = alltrue([
      for k, v in var.secrets :
      v.rotation_days == null || v.rotation_lambda_name != null
    ])
    error_message = "A secret with rotation_days must name the rotation lambda that rotates it."
  }
}

variable "rotation_lambdas" {
  description = <<-EOT
    Rotation lambda scaffolding. Terraform creates the function, its role, its
    VPC attachment and its log group; the deployment package is built and
    published by CI from cmd/rotation-* and referenced here by S3 key and object
    version, so a rotation lambda is a reviewed artifact like any other.

    Leave the map empty to create the IAM surface without the functions - which
    is the correct state for an environment where rotation has not yet been
    enabled.
  EOT

  type = map(object({
    description       = string
    handler           = optional(string, "bootstrap")
    runtime           = optional(string, "provided.al2023")
    architectures     = optional(list(string), ["arm64"])
    timeout_seconds   = optional(number, 120)
    memory_mb         = optional(number, 256)
    s3_bucket         = string
    s3_key            = string
    s3_object_version = optional(string)
    environment       = optional(map(string), {})
  }))
  default = {}

  validation {
    condition = alltrue([
      for k, v in var.rotation_lambdas : v.timeout_seconds >= 30 && v.timeout_seconds <= 900
    ])
    error_message = "Rotation lambda timeout must be between 30 and 900 seconds; a gateway-side rotation is slow and a 3 s default will fail halfway through."
  }
}

variable "subnet_ids" {
  description = "Subnets for the rotation lambdas' ENIs. Must have a route to the Secrets Manager interface endpoint and, for gateway credential rotation, to the egress path."
  type        = list(string)
  default     = []
}

variable "security_group_ids" {
  description = "Security groups for the rotation lambdas' ENIs."
  type        = list(string)
  default     = []
}

variable "log_retention_days" {
  description = "Retention for rotation lambda logs. Long enough to reconstruct a failed rotation months later during an audit."
  type        = number
  default     = 365
}

variable "cloudwatch_log_kms_key_arn" {
  description = "CMK for the rotation lambda log groups."
  type        = string
}

variable "denied_human_principal_pattern" {
  description = <<-EOT
    ARN pattern for human roles that must never read a secret value. A resource
    policy deny here is the account-local mirror of the organization SCP; both
    exist so that removing one does not open the path (security.md 5.1).
  EOT
  type    = string
  default = "arn:aws:iam::*:role/pp-human-*"
}

variable "account_id" {
  description = "Account ID owning the secrets."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.account_id))
    error_message = "account_id must be a 12-digit AWS account ID."
  }
}
