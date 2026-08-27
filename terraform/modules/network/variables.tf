variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "region_role" {
  description = "Whether this VPC is the active (primary) region or the DR (secondary) region. Used only for naming and tagging."
  type        = string
  default     = "primary"

  validation {
    condition     = contains(["primary", "secondary"], var.region_role)
    error_message = "region_role must be primary or secondary."
  }
}

variable "vpc_cidr" {
  description = <<-EOT
    VPC CIDR, /16. Non-overlapping across every environment and region so any
    pair can be peered or attached to a Transit Gateway without renumbering
    (deployment.md 2.2). Note that peering between environments is forbidden by
    policy - the non-overlap is about keeping the option, not exercising it.
  EOT
  type = string

  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0))
    error_message = "vpc_cidr must be a valid IPv4 CIDR block."
  }

  validation {
    condition     = tonumber(split("/", var.vpc_cidr)[1]) == 16
    error_message = "vpc_cidr must be a /16. The pod subnets alone need /20 per AZ; anything smaller runs out during a scale-out."
  }
}

variable "availability_zones" {
  description = "AZ names, in order. Exactly three: the AZ-loss survival arithmetic in the baseline assumes three."
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) == 3
    error_message = "Exactly three availability zones are required."
  }
}

variable "public_subnet_cidrs" {
  description = "Public subnets (ALB and NAT only). One per AZ."
  type        = list(string)

  validation {
    condition     = alltrue([for c in var.public_subnet_cidrs : can(cidrhost(c, 0))])
    error_message = "Every public subnet CIDR must be a valid IPv4 CIDR block."
  }
}

variable "pod_subnet_cidrs" {
  description = <<-EOT
    EKS node and pod subnets. /20 per AZ: the VPC CNI assigns real VPC IPs per
    pod and, with prefix delegation, a /28 per ENI. CNI IP exhaustion during a
    scale-up presents as a scheduling failure and is an outage
    (deployment.md 2.2).
  EOT
  type = list(string)

  validation {
    condition     = alltrue([for c in var.pod_subnet_cidrs : can(cidrhost(c, 0))])
    error_message = "Every pod subnet CIDR must be a valid IPv4 CIDR block."
  }

  validation {
    condition     = alltrue([for c in var.pod_subnet_cidrs : tonumber(split("/", c)[1]) <= 20])
    error_message = "Pod subnets must be /20 or larger (numerically <= 20)."
  }
}

variable "data_subnet_cidrs" {
  description = "Aurora and ElastiCache subnets. No NAT, no IGW, no route off the VPC except VPC endpoints."
  type        = list(string)

  validation {
    condition     = alltrue([for c in var.data_subnet_cidrs : can(cidrhost(c, 0))])
    error_message = "Every data subnet CIDR must be a valid IPv4 CIDR block."
  }
}

variable "streaming_subnet_cidrs" {
  description = "MSK broker subnets. No NAT."
  type        = list(string)

  validation {
    condition     = alltrue([for c in var.streaming_subnet_cidrs : can(cidrhost(c, 0))])
    error_message = "Every streaming subnet CIDR must be a valid IPv4 CIDR block."
  }
}

variable "egress_subnet_cidrs" {
  description = "NAT-attached subnets. The only subnets with a default route to the internet, used for gateway/vendor calls."
  type        = list(string)

  validation {
    condition     = alltrue([for c in var.egress_subnet_cidrs : can(cidrhost(c, 0))])
    error_message = "Every egress subnet CIDR must be a valid IPv4 CIDR block."
  }
}

variable "firewall_subnet_cidrs" {
  description = <<-EOT
    Dedicated subnets for the AWS Network Firewall endpoints. Required only when
    enable_network_firewall is true. /28 is the AWS-documented minimum and is
    sufficient - one endpoint ENI per AZ.
  EOT
  type    = list(string)
  default = []

  validation {
    condition     = alltrue([for c in var.firewall_subnet_cidrs : can(cidrhost(c, 0))])
    error_message = "Every firewall subnet CIDR must be a valid IPv4 CIDR block."
  }
}

variable "single_nat_gateway" {
  description = <<-EOT
    Collapse to one NAT gateway instead of one per AZ. A cost lever for dev only:
    it makes egress from two AZs depend on a third AZ's NAT, which voids AZ
    independence. Must be false in staging and prod.
  EOT
  type    = bool
  default = false
}

variable "enable_network_firewall" {
  description = <<-EOT
    Route egress-subnet traffic through AWS Network Firewall with a stateful
    domain allowlist. See modules/network/firewall.tf for the rule group and
    README.md for the honest discussion of what this does and does not replace.
  EOT
  type    = bool
  default = false
}

variable "egress_allowlist_domains" {
  description = <<-EOT
    Exact SNI/HTTP-host values permitted outbound from the egress subnets.
    security.md 2.2 enumerates them: gateway APIs, the KYC vendor, the bank
    validation vendor and the IdP JWKS host. A new destination is a reviewed PR,
    not a runtime decision. Leading '.' means "and subdomains".
  EOT
  type    = list(string)
  default = []
}

variable "flow_logs_bucket_arn" {
  description = "S3 bucket ARN for VPC flow logs (all traffic, ACCEPT included). Exfiltration is usually visible in flow logs and DNS before it is visible anywhere else."
  type        = string

  validation {
    condition     = can(regex("^arn:aws:s3:::", var.flow_logs_bucket_arn))
    error_message = "flow_logs_bucket_arn must be an S3 bucket ARN."
  }
}

variable "flow_logs_prefix" {
  description = "Key prefix inside the flow-log bucket."
  type        = string
  default     = "vpc-flow-logs"
}

variable "dns_query_log_kms_key_arn" {
  description = "CMK for the Route 53 Resolver query-log CloudWatch group."
  type        = string
}

variable "dns_query_log_retention_days" {
  description = "Retention for Resolver query logs."
  type        = number
  default     = 90

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], var.dns_query_log_retention_days)
    error_message = "dns_query_log_retention_days must be one of the CloudWatch Logs retention values."
  }
}

variable "eks_cluster_name" {
  description = "Cluster name, used for the kubernetes.io/cluster/<name> subnet tags the AWS Load Balancer Controller and Karpenter discover subnets by."
  type        = string
}

variable "interface_endpoint_services" {
  description = <<-EOT
    Interface (PrivateLink) endpoints to create. Every AWS API the platform calls
    must be here: with no NAT route in the data tier and a domain allowlist on
    the egress tier, a missing endpoint is a hard failure, not a slow path.
  EOT
  type = list(string)
  default = [
    "ecr.api",
    "ecr.dkr",
    "secretsmanager",
    "kms",
    "sts",
    "logs",
    "monitoring",
    "sqs",
    "kafka",
    "elasticache",
    "aps-workspaces",
    "ssm",
    "ssmmessages",
    "ec2messages",
    "eks",
  ]

  validation {
    condition     = length(var.interface_endpoint_services) == length(distinct(var.interface_endpoint_services))
    error_message = "interface_endpoint_services must not contain duplicates."
  }
}

variable "vpc_endpoint_allowed_principal_arns" {
  description = <<-EOT
    Principals permitted by the interface endpoint policies. The endpoint policy
    is a second, non-IAM check on the same call (security.md 2.2): compromising
    an IAM policy is not sufficient to use the endpoint.
  EOT
  type    = list(string)
  default = []
}

variable "secrets_path_prefix" {
  description = "Secrets Manager path prefix this VPC's endpoint policy permits, e.g. /prod. A staging pod that somehow obtained a prod role still cannot read a prod secret through this endpoint."
  type        = string
}
