###############################################################################
# envs/dev/variables.tf
#
# Dev has relaxed guardrails and hard cost caps (deployment.md 6). "Relaxed"
# means smaller, cheaper and faster to iterate on - it does not mean unencrypted
# or publicly reachable. The controls that are dropped here are the ones whose
# cost is real and whose absence is survivable; the encryption, the tagging and
# the least-privilege IAM are identical to prod's, because those are the ones
# that are impossible to retrofit.
###############################################################################

variable "environment" {
  description = "Environment name."
  type        = string
  default     = "dev"

  validation {
    condition     = var.environment == "dev"
    error_message = "This stack is the dev stack."
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
  description = "Classification. Dev holds generated data and fake credentials only."
  type        = string
  default     = "internal"

  validation {
    condition     = contains(["internal", "public"], var.data_classification)
    error_message = "Dev must not be classified above internal. If it needs to be, the data does not belong there."
  }
}

variable "compliance_scope" {
  description = "PCI scope marker."
  type        = string
  default     = "pci-dss-out-of-scope"

  validation {
    condition     = var.compliance_scope == "pci-dss-out-of-scope"
    error_message = "Dev is out of scope and must stay out of scope."
  }
}

variable "region" {
  description = "Region."
  type        = string
  default     = "eu-west-1"
}

variable "vpc_cidr" {
  description = "VPC CIDR."
  type        = string
  default     = "10.40.0.0/16"

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
  description = "Egress allowlist. Dev talks to the gateway simulator and holds no gateway credentials at all (deployment.md 6.2)."
  type        = list(string)
  default     = []
}

variable "single_nat_gateway" {
  description = <<-EOT
    Collapse to one NAT gateway. Saves roughly USD 70/month and voids AZ
    independence for egress - acceptable in dev, rejected by the network module
    in staging and prod.
  EOT
  type    = bool
  default = true
}

variable "enable_network_firewall" {
  description = <<-EOT
    Network Firewall costs about USD 300/month per endpoint plus per-GB
    processing. Dev runs without it and relies on the in-cluster egress proxy
    alone. Stated as a priced trade rather than an oversight.
  EOT
  type    = bool
  default = false
}

variable "kubernetes_version" {
  description = "EKS minor version."
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
  description = "Cluster access. Developers get admin in dev (deployment.md 6)."
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
  description = "Aurora PostgreSQL version. Matches prod - a dev cluster on a different engine version finds different bugs."
  type        = string
}

variable "aurora_serverless_min_acu" {
  description = "Serverless v2 floor. 0.5 ACU is the minimum and is what makes the out-of-hours scale-down worth anything."
  type        = number
  default     = 0.5
}

variable "aurora_serverless_max_acu" {
  description = "Serverless v2 ceiling. Also the cost cap: an unbounded max is how a runaway test suite produces a five-figure bill."
  type        = number
  default     = 4

  validation {
    condition     = var.aurora_serverless_max_acu <= 16
    error_message = "Dev's Serverless v2 ceiling is capped at 16 ACU. Anything above that is a load test, and load tests belong in staging."
  }
}

variable "msk_kafka_version" {
  description = "Kafka version (unused when MSK Serverless is selected, but kept so the variable surface matches the other stacks)."
  type        = string
}

variable "redis_node_type" {
  description = "Cache node type."
  type        = string
  default     = "cache.t4g.micro"
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
  description = "Alert routing. Dev alerts go to a Slack channel, never to PagerDuty."
  type        = map(string)
  default     = {}
}

variable "budget_monthly_usd" {
  description = "Monthly budget. The dev account has a hard AWS Budget action that stops preview-environment creation at 100 percent (deployment.md 6.2)."
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
