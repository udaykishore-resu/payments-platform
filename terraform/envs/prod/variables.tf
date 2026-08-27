###############################################################################
# envs/prod/variables.tf
#
# Every variable is typed, described and - where a wrong value is expensive -
# validated. The validations are not decoration: they are the cheapest place to
# stop "someone set backup retention to 1 day in prod" and they run before a
# plan touches AWS.
###############################################################################

variable "environment" {
  description = "Environment name. Fixed to prod in this stack; declared as a variable so the modules that validate on it receive it the same way in every stack."
  type        = string
  default     = "prod"

  validation {
    condition     = var.environment == "prod"
    error_message = "This stack is the prod stack. Changing environment here would apply prod's topology under another name."
  }
}

# --- Tagging ------------------------------------------------------------------

variable "owner" {
  description = "Owning team. Appears on every resource; it is the tag an on-call engineer greps when they find something they do not recognise."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,40}$", var.owner))
    error_message = "owner must be a lower-case team slug."
  }
}

variable "cost_centre" {
  description = "Finance cost centre code. Drives the cost allocation report and the per-environment budget."
  type        = string

  validation {
    condition     = can(regex("^CC-[0-9]{4}$", var.cost_centre))
    error_message = "cost_centre must be of the form CC-1234."
  }
}

variable "data_classification" {
  description = <<-EOT
    Classification of the data the resources in this stack hold. This is not a
    label for tidiness: it is how the scope of an audit is bounded, how DLP
    rules select resources, and how a reviewer decides whether a change needs a
    security sign-off.
  EOT
  type = string

  validation {
    condition     = contains(["restricted", "confidential", "internal", "public"], var.data_classification)
    error_message = "data_classification must be one of: restricted, confidential, internal, public."
  }
}

variable "compliance_scope" {
  description = <<-EOT
    PCI DSS scope marker. The platform holds no cardholder data (baseline A2,
    docs/compliance.md 1) so it is assessed as a service provider with no CDE -
    'pci-dss-service-provider'. Non-prod holds synthetic data only and is
    'pci-dss-out-of-scope'. An assessor reading the tag inventory should be able
    to reconstruct the scope boundary without asking anyone.
  EOT
  type = string

  validation {
    condition     = contains(["pci-dss-service-provider", "pci-dss-connected", "pci-dss-out-of-scope"], var.compliance_scope)
    error_message = "compliance_scope must be one of: pci-dss-service-provider, pci-dss-connected, pci-dss-out-of-scope."
  }
}

# --- Regions and networking ---------------------------------------------------

variable "primary_region" {
  description = "Active region."
  type        = string
  default     = "eu-west-1"

  validation {
    condition     = contains(["eu-west-1", "eu-central-1"], var.primary_region)
    error_message = "primary_region must be an approved region (the SCP denies everything else)."
  }
}

variable "dr_region" {
  description = "Passive region."
  type        = string
  default     = "eu-central-1"

  validation {
    condition     = contains(["eu-west-1", "eu-central-1"], var.dr_region)
    error_message = "dr_region must be an approved region."
  }
}

variable "primary_vpc_cidr" {
  description = "Primary VPC CIDR (deployment.md 2.2)."
  type        = string
  default     = "10.20.0.0/16"

  validation {
    condition     = can(cidrhost(var.primary_vpc_cidr, 0)) && tonumber(split("/", var.primary_vpc_cidr)[1]) == 16
    error_message = "primary_vpc_cidr must be a valid /16."
  }
}

variable "dr_vpc_cidr" {
  description = "DR VPC CIDR. Must not overlap the primary."
  type        = string
  default     = "10.21.0.0/16"

  validation {
    condition     = can(cidrhost(var.dr_vpc_cidr, 0)) && tonumber(split("/", var.dr_vpc_cidr)[1]) == 16
    error_message = "dr_vpc_cidr must be a valid /16."
  }
}

variable "primary_availability_zones" {
  description = "AZs in the primary region."
  type        = list(string)
  default     = ["eu-west-1a", "eu-west-1b", "eu-west-1c"]
}

variable "dr_availability_zones" {
  description = "AZs in the DR region."
  type        = list(string)
  default     = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
}

variable "primary_subnets" {
  description = "Per-tier subnet CIDRs in the primary VPC, one entry per AZ."
  type = object({
    public    = list(string)
    pod       = list(string)
    data      = list(string)
    streaming = list(string)
    egress    = list(string)
    firewall  = list(string)
  })
}

variable "dr_subnets" {
  description = "Per-tier subnet CIDRs in the DR VPC."
  type = object({
    public    = list(string)
    pod       = list(string)
    data      = list(string)
    streaming = list(string)
    egress    = list(string)
    firewall  = list(string)
  })
}

variable "egress_allowlist_domains" {
  description = "Outbound destinations permitted from the egress tier (security.md 2.2). A new entry is a reviewed PR."
  type        = list(string)

  validation {
    condition     = length(var.egress_allowlist_domains) > 0
    error_message = "The allowlist cannot be empty: the orchestrator would have no path to any gateway."
  }
}

# --- EKS ----------------------------------------------------------------------

variable "kubernetes_version" {
  description = "EKS minor version. N-1 of the latest release."
  type        = string
}

variable "eks_addon_versions" {
  description = "Pinned add-on versions."
  type = object({
    vpc_cni            = string
    coredns            = string
    kube_proxy         = string
    ebs_csi            = string
    pod_identity_agent = optional(string)
  })
}

variable "node_groups" {
  description = "Managed node groups per region-role. Taints and labels must match deployment.md 1.2."
  type = map(map(object({
    instance_types = list(string)
    capacity_type  = optional(string, "ON_DEMAND")
    min_size       = number
    max_size       = number
    desired_size   = number
    disk_size_gb   = optional(number, 100)
    ami_type       = optional(string, "AL2023_ARM_64_STANDARD")
    labels         = optional(map(string), {})
    taints = optional(list(object({
      key    = string
      value  = string
      effect = string
    })), [])
    max_unavailable_percentage = optional(number, 25)
  })))
}

variable "eks_public_access_cidrs" {
  description = "CIDRs allowed to reach the EKS public API endpoint: the CI runners' NAT addresses and the break-glass bastion. Never 0.0.0.0/0."
  type        = list(string)
  default     = []
}

variable "eks_access_entries" {
  description = "SSO-federated roles mapped to cluster access."
  type = map(object({
    principal_arn     = string
    kubernetes_groups = optional(list(string), [])
    type              = optional(string, "STANDARD")
    policy_arn        = optional(string)
    access_scope_type = optional(string, "cluster")
    namespaces        = optional(list(string), [])
  }))
  default = {}
}

# --- Aurora -------------------------------------------------------------------

variable "aurora_engine_version" {
  description = "Aurora PostgreSQL version, pinned."
  type        = string
}

variable "aurora_instance_class" {
  description = "Writer and reader class in the primary region."
  type        = string
  default     = "db.r6g.4xlarge"
}

variable "aurora_dr_instance_class" {
  description = <<-EOT
    Secondary class. Deliberately half the primary (disaster-recovery.md 4.1.4):
    the secondary is resized as part of failback preparation, not during a
    failover, because resizing mid-incident adds 5-10 minutes and running at
    half class for the first hour costs only latency headroom the degradation
    ladder can absorb. Documented here so nobody "helpfully" matches them.
  EOT
  type    = string
  default = "db.r6g.2xlarge"
}

variable "aurora_backup_retention_days" {
  description = "Automated backup and PITR window."
  type        = number
  default     = 35

  validation {
    condition     = var.aurora_backup_retention_days == 35
    error_message = "prod requires the maximum 35 days (disaster-recovery.md 5.1)."
  }
}

# --- MSK ----------------------------------------------------------------------

variable "msk_kafka_version" {
  description = "Kafka version, pinned."
  type        = string
}

variable "msk_broker_instance_type" {
  description = "Broker class."
  type        = string
  default     = "kafka.m5.2xlarge"
}

variable "msk_broker_ebs_gb" {
  description = "Per-broker storage."
  type        = number
  default     = 2000
}

# --- ElastiCache --------------------------------------------------------------

variable "redis_node_type" {
  description = "Cache node type."
  type        = string
  default     = "cache.r7g.large"
}

variable "redis_shards" {
  description = "Shard count."
  type        = number
  default     = 3
}

# --- Edge and DNS -------------------------------------------------------------

variable "zone_name" {
  description = "Public hosted zone."
  type        = string
}

variable "api_hostname" {
  description = "API hostname the failover record answers for."
  type        = string
}

variable "route53_zone_id" {
  description = "Hosted zone ID. The zone itself lives in pp-shared-services; this stack only writes records."
  type        = string
}

variable "blocked_country_codes" {
  description = "WAF W11 geo-block list."
  type        = list(string)
  default     = ["KP", "IR", "SY", "CU"]
}

variable "enable_shield_advanced" {
  description = "Register the ALBs with Shield Advanced. The subscription itself is org-level and not managed here."
  type        = bool
  default     = true
}

# --- Observability and cost ---------------------------------------------------

variable "alert_topic_subscriptions" {
  description = "protocol => endpoint for alert routing (PagerDuty, Slack)."
  type        = map(string)
  default     = {}
}

variable "budget_monthly_usd" {
  description = "Monthly budget for the environment. Alarms at 80/100/120 percent."
  type        = number
}

variable "budget_notification_emails" {
  description = "Budget notification recipients."
  type        = list(string)
  default     = []
}

variable "grafana_sso_admin_group_ids" {
  description = "IdP groups mapped to Grafana ADMIN."
  type        = list(string)
  default     = []
}

# --- IAM ----------------------------------------------------------------------

variable "key_administrator_arns" {
  description = "Principals permitted to administer the CMKs: the break-glass role and the Terraform deploy role, and nothing else."
  type        = list(string)

  validation {
    condition     = length(var.key_administrator_arns) >= 1 && length(var.key_administrator_arns) <= 3
    error_message = "Between one and three key administrators. More than that and 'who can destroy a key' stops being answerable."
  }
}

variable "backup_vault_account_arn" {
  description = "Cross-account backup vault ARN in pp-backup-vault. Empty disables the daily copy."
  type        = string
  default     = ""
}

variable "rotation_lambda_artifact_bucket" {
  description = "S3 bucket holding the rotation lambda packages published by CI."
  type        = string
  default     = ""
}

variable "rotation_lambda_artifacts" {
  description = "rotation lambda key => { s3_key, s3_object_version }. Empty creates no rotation functions, which is the correct state until CI has published them."
  type = map(object({
    description       = string
    s3_key            = string
    s3_object_version = optional(string)
    rotation_days     = number
  }))
  default = {}
}
