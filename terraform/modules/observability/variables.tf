variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "name_prefix" {
  description = "Prefix for observability resource names."
  type        = string
}

variable "kms_key_arn_logs" {
  description = "CMK for every CloudWatch log group created here."
  type        = string
}

variable "log_groups" {
  description = <<-EOT
    Additional CloudWatch log groups this stack owns. CloudWatch Logs is used
    ONLY for AWS-service logs (EKS control plane, VPC flow logs, RDS, WAF).
    Application logs go fluent-bit -> Loki -> S3 and never enter CloudWatch:
    the per-GB ingest cost at this volume is an order of magnitude above S3, and
    the query language is worse.
  EOT
  type = map(object({
    retention_days = number
  }))
  default = {}

  validation {
    condition = alltrue([
      for k, v in var.log_groups :
      contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], v.retention_days)
    ])
    error_message = "retention_days must be a valid CloudWatch Logs retention value."
  }
}

variable "create_prometheus_workspace" {
  description = "Create an Amazon Managed Prometheus workspace."
  type        = bool
  default     = true
}

variable "prometheus_retention_days" {
  description = "AMP retention. 150 days covers a full quarter plus the comparison window a capacity review needs."
  type        = number
  default     = 150
}

variable "create_grafana_workspace" {
  description = "Create an Amazon Managed Grafana workspace. Usually only in the primary region: a second Grafana is not a DR requirement (disaster-recovery.md 1.1)."
  type        = bool
  default     = false
}

variable "grafana_sso_admin_group_ids" {
  description = "IdP group IDs mapped to the Grafana ADMIN role."
  type        = list(string)
  default     = []
}

variable "alert_topic_subscriptions" {
  description = "protocol => endpoint for the alert SNS topic, e.g. https => the PagerDuty integration URL."
  type        = map(string)
  default     = {}
}

variable "sns_kms_key_arn" {
  description = "CMK for the alert topics."
  type        = string
}

variable "alb_arn_suffix" {
  description = "ALB ARN suffix for the edge alarms. Empty skips them."
  type        = string
  default     = ""
}

variable "budget_monthly_usd" {
  description = "Monthly cost budget for this environment. Alarms fire at 80/100/120 percent and route to the platform team, not to finance."
  type        = number

  validation {
    condition     = var.budget_monthly_usd > 0
    error_message = "budget_monthly_usd must be positive."
  }
}

variable "budget_notification_emails" {
  description = "Recipients of the budget notifications."
  type        = list(string)
  default     = []
}
