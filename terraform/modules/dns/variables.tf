variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "zone_name" {
  description = "Public hosted zone name, e.g. example.com."
  type        = string
}

variable "create_zone" {
  description = "Create the hosted zone here. False when the zone lives in pp-shared-services and this stack only writes records into it."
  type        = bool
  default     = false
}

variable "existing_zone_id" {
  description = "Zone ID to write into when create_zone is false."
  type        = string
  default     = null
}

variable "api_hostname" {
  description = "Fully-qualified API hostname the failover record answers for."
  type        = string
}

variable "record_ttl" {
  description = <<-EOT
    TTL on the failover record. 60 s balances failover speed against resolver
    load; the RTO budget assumes 60-120 s of DNS convergence
    (disaster-recovery.md 1.1). Lowering it further does not help - resolvers
    that ignore short TTLs ignore 30 s as readily as 60 s - and it multiplies
    query volume.
  EOT
  type    = number
  default = 60

  validation {
    condition     = var.record_ttl >= 30 && var.record_ttl <= 300
    error_message = "record_ttl must be between 30 and 300 seconds."
  }
}

variable "endpoints" {
  description = <<-EOT
    Regional endpoints participating in the failover record.

    role: PRIMARY or SECONDARY. Exactly one PRIMARY.
    health_check_fqdn: the region's own ALB hostname, health-checked directly.
      Never the API hostname - a health check that resolves the record it
      controls is a feedback loop that fails closed on the first blip.
  EOT

  type = map(object({
    role              = string
    alb_dns_name      = string
    alb_zone_id       = string
    health_check_fqdn = string
    health_check_path = optional(string, "/healthz")
  }))

  validation {
    condition     = length([for k, v in var.endpoints : k if v.role == "PRIMARY"]) == 1
    error_message = "Exactly one endpoint must have role PRIMARY."
  }

  validation {
    condition     = alltrue([for k, v in var.endpoints : contains(["PRIMARY", "SECONDARY"], v.role)])
    error_message = "role must be PRIMARY or SECONDARY."
  }
}

variable "health_check_regions" {
  description = "Route 53 checker regions. Three or more, geographically spread, so a single checker region's network problem does not look like a regional outage."
  type        = list(string)
  default     = ["eu-west-1", "us-east-1", "ap-southeast-1"]

  validation {
    condition     = length(var.health_check_regions) >= 3
    error_message = "At least three checker regions are required; Route 53 requires a 3-of-N agreement to declare a check unhealthy."
  }
}

variable "health_check_interval" {
  description = "Health check interval in seconds. 10 s (fast) rather than 30 s: three failures at 10 s is ~30 s to unhealthy, which fits the MTTD budget."
  type        = number
  default     = 10

  validation {
    condition     = contains([10, 30], var.health_check_interval)
    error_message = "health_check_interval must be 10 or 30 (the only values Route 53 accepts)."
  }
}

variable "health_check_failure_threshold" {
  description = "Consecutive failures before a check is unhealthy."
  type        = number
  default     = 3

  validation {
    condition     = var.health_check_failure_threshold >= 1 && var.health_check_failure_threshold <= 10
    error_message = "health_check_failure_threshold must be between 1 and 10."
  }
}

variable "enable_dnssec" {
  description = "Sign the zone with DNSSEC. Requires a KMS asymmetric ECC_NIST_P256 key in us-east-1 and a DS record at the registrar - see the module comment."
  type        = bool
  default     = false
}

variable "dnssec_key_arn" {
  description = "KMS asymmetric signing key (ECC_NIST_P256) in us-east-1 for DNSSEC."
  type        = string
  default     = null
}

variable "query_log_group_arn" {
  description = "CloudWatch log group ARN in us-east-1 for public-zone query logging. Empty disables it."
  type        = string
  default     = ""
}
