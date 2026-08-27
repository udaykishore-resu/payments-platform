variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "name_prefix" {
  description = "Prefix for edge resource names, e.g. pp-prod-euw1."
  type        = string
}

variable "vpc_id" {
  description = "VPC ID."
  type        = string
}

variable "public_subnet_ids" {
  description = "Public subnets for the ALB."
  type        = list(string)

  validation {
    condition     = length(var.public_subnet_ids) >= 2
    error_message = "An ALB needs subnets in at least two AZs."
  }
}

variable "private_subnet_ids" {
  description = "Pod subnets for the internal NLB."
  type        = list(string)
}

variable "alb_security_group_ids" {
  description = "Security groups for the ALB."
  type        = list(string)
}

variable "domain_name" {
  description = "Primary API hostname, e.g. api.example.com."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9.-]+\\.[a-z]{2,}$", var.domain_name))
    error_message = "domain_name must be a valid DNS name."
  }
}

variable "subject_alternative_names" {
  description = "Additional names on the ACM certificate."
  type        = list(string)
  default     = []
}

variable "route53_zone_id" {
  description = "Hosted zone used for ACM DNS validation."
  type        = string
}

variable "ssl_policy" {
  description = <<-EOT
    ALB TLS policy. The default permits TLS 1.2 and 1.3 with AEAD suites only:
    ECDHE for forward secrecy, GCM only (no CBC, therefore no Lucky13/BEAST
    class), no static RSA key exchange (security.md 2.1).
  EOT
  type    = string
  default = "ELBSecurityPolicy-TLS13-1-2-Res-2021-06"

  validation {
    condition     = can(regex("^ELBSecurityPolicy-TLS13-1-2", var.ssl_policy))
    error_message = "Only TLS13-1-2 policy families are permitted. Anything admitting TLS 1.0/1.1 is rejected here rather than in review."
  }
}

variable "access_logs_bucket" {
  description = "S3 bucket receiving ALB access logs."
  type        = string
}

variable "access_logs_prefix" {
  description = "Key prefix for ALB access logs."
  type        = string
  default     = "alb"
}

variable "deletion_protection" {
  description = "ELB deletion protection."
  type        = bool
  default     = true
}

variable "idle_timeout_seconds" {
  description = <<-EOT
    ALB idle timeout. Must exceed the longest legitimate request: an 8 s gateway
    call plus two retries plus the pipeline is about 30 s (deployment.md 1.8).
    60 s leaves headroom without holding dead connections open for minutes.
  EOT
  type    = number
  default = 60

  validation {
    condition     = var.idle_timeout_seconds >= 30 && var.idle_timeout_seconds <= 300
    error_message = "idle_timeout_seconds must be between 30 and 300."
  }
}

variable "waf_rate_limit_payments" {
  description = "W6: requests per 5 minutes per source IP on /v1/payments before blocking."
  type        = number
  default     = 2000

  validation {
    condition     = var.waf_rate_limit_payments >= 100 && var.waf_rate_limit_payments <= 2000000
    error_message = "waf_rate_limit_payments must be between 100 and 2000000."
  }
}

variable "waf_rate_limit_token" {
  description = "W7: requests per 5 minutes per source IP on /oauth2/token before blocking."
  type        = number
  default     = 100

  validation {
    condition     = var.waf_rate_limit_token >= 100 && var.waf_rate_limit_token <= 2000000
    error_message = "waf_rate_limit_token must be at least 100 (the AWS WAF minimum for a rate-based rule)."
  }
}

variable "waf_max_body_bytes" {
  description = "W1: body-size cap on /v1/payments*. A payment instruction is under 8 KB; 256 KB is generous."
  type        = number
  default     = 262144
}

variable "blocked_country_codes" {
  description = "W11: ISO-3166 alpha-2 codes blocked at the edge, mirroring the platform sanctions list."
  type        = list(string)
  default     = ["KP", "IR", "SY", "CU"]

  validation {
    condition     = alltrue([for c in var.blocked_country_codes : can(regex("^[A-Z]{2}$", c))])
    error_message = "Country codes must be upper-case ISO-3166 alpha-2."
  }
}

variable "waf_log_destination_arn" {
  description = "Kinesis Firehose or CloudWatch log group ARN for WAF logs. Empty disables WAF logging, which is never correct in prod."
  type        = string
  default     = ""
}

variable "enable_shield_advanced" {
  description = <<-EOT
    Register the ALB and its EIPs with Shield Advanced. The Shield Advanced
    *subscription* is an organization-level, 1-year commitment made in the
    management account and is deliberately NOT managed here: a `terraform
    destroy` must never be able to cancel it, and its billing is not this
    stack's concern. This flag only creates the per-resource protections.
  EOT
  type    = bool
  default = false
}

variable "enable_nlb" {
  description = "Create the internal NLB used by the mesh ingress gateway for cross-cluster gRPC."
  type        = bool
  default     = true
}

variable "cloudfront_origin_secret_header_name" {
  description = "Header CloudFront adds and the ALB requires, so the ALB cannot be reached directly even if its DNS name is discovered (security.md 2.1)."
  type        = string
  default     = "x-pp-origin-verify"
}

variable "cloudfront_origin_secret_arn" {
  description = "Secrets Manager ARN holding the origin-verify header value. Null skips the origin-verify listener rule (non-prod, where there is no CloudFront in front)."
  type        = string
  default     = null
}

variable "validation_record_suffix" {
  description = <<-EOT
    Disambiguates the ACM DNS-validation record resource ADDRESSES when two
    regional edge stacks request a certificate for the same hostname in the same
    hosted zone. It does not change the record name.

    Known caveat, stated rather than hidden: ACM normally returns the same
    validation CNAME for the same domain in the same account, in which case both
    regions write an identical record and the writes are idempotent. If ACM
    returns distinct tokens, the second apply overwrites the first
    (allow_overwrite = true) and one certificate stays PENDING_VALIDATION until
    its record is restored. The resolution is to give the DR certificate its own
    regional hostname and put the shared hostname on CloudFront - see
    terraform/README.md, "Known caveats".
  EOT
  type    = string
  default = ""
}
