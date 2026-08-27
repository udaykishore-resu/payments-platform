###############################################################################
# envs/staging/variables.tf
#
# Staging is production-shaped with synthetic data (deployment.md 6). The
# variable surface is deliberately the same shape as prod's, minus the DR
# region, so that a change made in prod can be reviewed against staging without
# translating between two different vocabularies.
###############################################################################

variable "environment" {
  description = "Environment name."
  type        = string
  default     = "staging"

  validation {
    condition     = var.environment == "staging"
    error_message = "This stack is the staging stack."
  }
}

variable "owner" {
  description = "Owning team."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,40}$", var.owner))
    error_message = "owner must be a lower-case team slug."
  }
}

variable "cost_centre" {
  description = "Finance cost centre code."
  type        = string

  validation {
    condition     = can(regex("^CC-[0-9]{4}$", var.cost_centre))
    error_message = "cost_centre must be of the form CC-1234."
  }
}

variable "data_classification" {
  description = "Classification of the data held here. Staging holds generated data only (deployment.md 6.1) - there is no anonymisation pipeline and no scrubbed production dump."
  type        = string

  validation {
    condition     = contains(["internal", "confidential"], var.data_classification)
    error_message = "Staging holds synthetic data; 'restricted' would be a false claim and 'public' would be a careless one."
  }
}

variable "compliance_scope" {
  description = "PCI scope marker."
  type        = string

  validation {
    condition     = var.compliance_scope == "pci-dss-out-of-scope"
    error_message = "Staging holds synthetic data only and is out of PCI scope. If that stops being true, the change needs a scope review, not a tfvars edit."
  }
}

variable "region" {
  description = "Region."
  type        = string
  default     = "eu-west-1"

  validation {
    condition     = contains(["eu-west-1", "eu-central-1"], var.region)
    error_message = "region must be an approved region."
  }
}

variable "vpc_cidr" {
  description = "VPC CIDR."
  type        = string
  default     = "10.30.0.0/16"

  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0)) && tonumber(split("/", var.vpc_cidr)[1]) == 16
    error_message = "vpc_cidr must be a valid /16."
  }
}

variable "availability_zones" {
  description = "AZs."
  type        = list(string)
  default     = ["eu-west-1a", "eu-west-1b", "eu-west-1c"]
}

variable "subnets" {
  description = "Per-tier subnet CIDRs."
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
  description = "Egress allowlist. Staging talks to gateway SANDBOX endpoints, never live ones."
  type        = list(string)
}

variable "kubernetes_version" {
  description = "EKS minor version. Staging upgrades first (deployment.md 2.3)."
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
  description = "Managed node groups."
  type = map(object({
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
  }))
}

variable "eks_public_access_cidrs" {
  description = "CIDRs allowed to reach the EKS public endpoint."
  type        = list(string)
  default     = []
}

variable "eks_access_entries" {
  description = "SSO-federated roles mapped to cluster access. Developers get read; SRE gets write (deployment.md 6)."
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

variable "aurora_engine_version" {
  description = "Aurora PostgreSQL version. Must match prod: staging exists to find what breaks before prod does."
  type        = string
}

variable "aurora_instance_class" {
  description = "Aurora class."
  type        = string
  default     = "db.r6g.large"
}

variable "aurora_backup_retention_days" {
  description = "Backup retention. Shorter than prod because the data is synthetic and regenerable."
  type        = number
  default     = 7

  validation {
    condition     = var.aurora_backup_retention_days >= 3 && var.aurora_backup_retention_days <= 35
    error_message = "Between 3 and 35 days. Below 3 makes the restore drill untestable."
  }
}

variable "msk_kafka_version" {
  description = "Kafka version. Must match prod."
  type        = string
}

variable "msk_broker_instance_type" {
  description = "Broker class."
  type        = string
  default     = "kafka.m5.large"
}

variable "redis_node_type" {
  description = "Cache node type."
  type        = string
  default     = "cache.r7g.large"
}

variable "zone_name" {
  description = "Public hosted zone."
  type        = string
}

variable "api_hostname" {
  description = "API hostname."
  type        = string
}

variable "route53_zone_id" {
  description = "Hosted zone ID."
  type        = string
}

variable "alert_topic_subscriptions" {
  description = "Alert routing."
  type        = map(string)
  default     = {}
}

variable "budget_monthly_usd" {
  description = "Monthly budget."
  type        = number
}

variable "budget_notification_emails" {
  description = "Budget notification recipients."
  type        = list(string)
  default     = []
}

variable "key_administrator_arns" {
  description = "CMK administrators."
  type        = list(string)
}
