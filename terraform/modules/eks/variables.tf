variable "environment" {
  description = "Environment name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "cluster_name" {
  description = "EKS cluster name."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z0-9][a-zA-Z0-9-_]{0,99}$", var.cluster_name))
    error_message = "cluster_name must be 1-100 alphanumeric/hyphen/underscore characters."
  }
}

variable "kubernetes_version" {
  description = "EKS minor version. Policy is N-1 of the latest release, upgraded quarterly, staging first, one minor at a time (deployment.md 2.3)."
  type        = string

  validation {
    condition     = can(regex("^1\\.(2[6-9]|3[0-9])$", var.kubernetes_version))
    error_message = "kubernetes_version must be a pinned 1.26-1.39 minor version."
  }
}

variable "vpc_id" {
  description = "VPC ID."
  type        = string
}

variable "subnet_ids" {
  description = "Subnets for the cluster ENIs and the node groups - the pod tier."
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) >= 2
    error_message = "At least two subnets in different AZs are required by EKS."
  }
}

variable "cluster_security_group_id" {
  description = "Security group for the cluster's control-plane ENIs."
  type        = string
}

variable "node_security_group_id" {
  description = "Security group attached to every node. Referenced by the Aurora, Redis, MSK and endpoint security groups."
  type        = string
}

variable "public_access_cidrs" {
  description = <<-EOT
    CIDRs permitted to reach the public API endpoint. The endpoint is private
    first; the public half exists only for the CI runners and the break-glass
    bastion, and an empty list disables it entirely.
  EOT
  type    = list(string)
  default = []

  validation {
    condition     = alltrue([for c in var.public_access_cidrs : can(cidrhost(c, 0))])
    error_message = "Every entry must be a valid IPv4 CIDR."
  }

  validation {
    condition     = !contains(var.public_access_cidrs, "0.0.0.0/0")
    error_message = "0.0.0.0/0 on the EKS public endpoint is never acceptable, in any environment."
  }
}

variable "kms_key_arn_secrets" {
  description = "CMK for envelope encryption of Kubernetes Secrets. Without it, a Secret is base64 in etcd and an etcd snapshot is a credential dump."
  type        = string
}

variable "kms_key_arn_ebs" {
  description = "CMK for node root volumes and EBS CSI volumes."
  type        = string
}

variable "kms_key_arn_logs" {
  description = "CMK for the control-plane log group."
  type        = string
}

variable "cluster_log_types" {
  description = <<-EOT
    Control-plane log types. All five, always: 'audit' alone answers who did
    what, but 'authenticator' is what answers how they got in, and the three
    controller logs are what answer why the cluster did something nobody asked
    for.
  EOT
  type    = list(string)
  default = ["api", "audit", "authenticator", "controllerManager", "scheduler"]

  validation {
    condition = alltrue([
      for t in var.cluster_log_types :
      contains(["api", "audit", "authenticator", "controllerManager", "scheduler"], t)
    ])
    error_message = "cluster_log_types entries must be valid EKS control-plane log types."
  }
}

variable "cluster_log_retention_days" {
  description = "Retention of the control-plane logs in CloudWatch before the SIEM takes over."
  type        = number
  default     = 90
}

variable "node_groups" {
  description = <<-EOT
    Managed node groups. The taints and labels must match the nodeSelector and
    tolerations in deployment.md 1.2: a mismatch is silent - pods simply stay
    Pending - and it is the single most common cause of a "the cluster is
    broken" page after a node-group change.

    The `data` group carries pp.plane=data:NoSchedule and runs the money path
    only; `control` carries pp.plane=control:NoSchedule; `general` is untainted
    and runs platform and observability workloads.
  EOT

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

  validation {
    condition = alltrue([
      for k, v in var.node_groups : v.min_size <= v.desired_size && v.desired_size <= v.max_size
    ])
    error_message = "For every node group: min_size <= desired_size <= max_size."
  }

  validation {
    condition = alltrue([
      for k, v in var.node_groups : contains(["ON_DEMAND", "SPOT"], v.capacity_type)
    ])
    error_message = "capacity_type must be ON_DEMAND or SPOT."
  }

  validation {
    condition = alltrue(flatten([
      for k, v in var.node_groups : [
        for t in v.taints : contains(["NO_SCHEDULE", "PREFER_NO_SCHEDULE", "NO_EXECUTE"], t.effect)
      ]
    ]))
    error_message = "Taint effects must be NO_SCHEDULE, PREFER_NO_SCHEDULE or NO_EXECUTE (the EKS API spelling, not the Kubernetes one)."
  }
}

variable "addon_versions" {
  description = <<-EOT
    Pinned add-on versions. Not `latest`, and not omitted: omitting the version
    makes EKS pick the current default, which means two clusters created a month
    apart run different CNI versions and only one of them has the bug.
  EOT

  type = object({
    vpc_cni            = string
    coredns            = string
    kube_proxy         = string
    ebs_csi            = string
    pod_identity_agent = optional(string)
  })

  validation {
    condition = alltrue([
      for v in compact([
        var.addon_versions.vpc_cni,
        var.addon_versions.coredns,
        var.addon_versions.kube_proxy,
        var.addon_versions.ebs_csi,
        coalesce(var.addon_versions.pod_identity_agent, "v1.0.0-eksbuild.1"),
      ]) : can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+.*-eksbuild\\.[0-9]+$", v))
    ])
    error_message = "Add-on versions must be fully pinned, e.g. v1.18.3-eksbuild.2."
  }
}

variable "enable_prefix_delegation" {
  description = "VPC CNI prefix delegation: a /28 per ENI instead of individual IPs. Raises pod density and cuts EC2 API churn during scale-out (deployment.md 2.2)."
  type        = bool
  default     = true
}

# --- Inputs the IRSA policies are scoped against ------------------------------

variable "account_id" {
  description = "Account ID."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.account_id))
    error_message = "account_id must be a 12-digit AWS account ID."
  }
}

variable "secrets_path_prefix" {
  description = "Secrets Manager path prefix for this environment, e.g. /prod."
  type        = string
}

variable "kms_key_arn_secrets_manager" {
  description = "CMK that Secrets Manager uses, so the deployables can be granted kms:Decrypt scoped by kms:ViaService."
  type        = string
}

variable "kms_key_arn_s3" {
  description = "CMK for the S3 buckets, granted to the deployables that read or write objects."
  type        = string
}

variable "msk_cluster_name" {
  description = "MSK cluster name, used to build kafka-cluster:* resource ARNs. Passed as a name rather than an ARN so this module does not depend on the MSK module."
  type        = string
}

variable "aurora_cluster_resource_id" {
  description = "Aurora cluster resource ID (cluster-XXXX) for rds-db:connect grants. Empty disables the grants."
  type        = string
  default     = ""
}

variable "s3_bucket_arns" {
  description = "purpose => bucket ARN, for the per-deployable object-level grants."
  type        = map(string)
  default     = {}
}

variable "dr_control_table_arn" {
  description = "DynamoDB fencing table ARN. payment-api and payment-orchestrator read it every 10 s; nothing but platformctl writes it."
  type        = string
  default     = ""
}

variable "amp_workspace_arn" {
  description = "Amazon Managed Prometheus workspace ARN for the remote_write grant."
  type        = string
  default     = ""
}

variable "route53_zone_arns" {
  description = "Hosted zone ARNs ExternalDNS may write. Empty means ExternalDNS gets no write permission at all, which is the correct state for a cluster that does not own DNS."
  type        = list(string)
  default     = []
}

variable "permissions_boundary_arn" {
  description = <<-EOT
    Permissions boundary attached to every IRSA role. It caps what any of these
    roles can ever do, independently of the policy attached to them, so a future
    over-broad policy edit still cannot grant iam:*, organizations:* or
    kms:ScheduleKeyDeletion. Null disables the boundary (dev only).
  EOT
  type    = string
  default = null
}

variable "irsa_service_accounts" {
  description = <<-EOT
    deployable => { namespace, service_account }. Must match the SPIFFE ID and
    namespace table in security.md 1.2. The trust policy pins BOTH the audience
    and the exact namespace:serviceaccount, so a pod in another namespace that
    happens to use the same service-account name cannot assume the role.
  EOT

  type = map(object({
    namespace       = string
    service_account = string
  }))
}

variable "access_entries" {
  description = <<-EOT
    EKS Access Entries: SSO-federated role ARN => access policy. Replaces the
    aws-auth ConfigMap, whose failure mode (a typo locks everyone out of a
    cluster with no way back in) is not acceptable on a production cluster.
  EOT

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

variable "irsa_role_name_prefix" {
  description = <<-EOT
    Prefix for the per-deployable IRSA role and policy names. IAM is a GLOBAL
    namespace: two clusters in the same account cannot both create a role called
    pp-prod-payment-api, so a multi-region environment must give each region its
    own prefix (pp-prod-euw1-, pp-prod-euc1-).

    The env stack constructs the same prefix when it builds the role ARNs it
    feeds to the KMS key policies and the DynamoDB fence policy, and a check
    block there asserts the two agree.

    Null defaults to pp-<environment>, which is correct for a single-cluster
    environment.
  EOT
  type    = string
  default = null

  validation {
    condition     = var.irsa_role_name_prefix == null || can(regex("^[a-z][a-z0-9-]{2,48}$", var.irsa_role_name_prefix))
    error_message = "irsa_role_name_prefix must be a lower-case name fragment."
  }
}
