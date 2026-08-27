###############################################################################
# modules/eks - cluster, node groups, add-ons, OIDC provider
###############################################################################

terraform {
  required_version = ">= 1.9.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.60.0, < 6.0.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = ">= 4.0.5, < 5.0.0"
    }
  }
}

data "aws_region" "current" {}
data "aws_partition" "current" {}

locals {
  name = var.cluster_name

  # Public endpoint access is enabled only if a non-empty allowlist was given.
  public_access_enabled = length(var.public_access_cidrs) > 0
}

check "prod_endpoint_posture" {
  assert {
    condition     = var.environment != "prod" || length(var.public_access_cidrs) <= 4
    error_message = "The prod cluster's public endpoint allowlist should be the CI egress IPs and the break-glass bastion, nothing more. More than four entries needs a written justification."
  }

  assert {
    condition     = var.environment != "prod" || var.permissions_boundary_arn != null
    error_message = "A permissions boundary is mandatory on prod IRSA roles."
  }
}

###############################################################################
# Cluster IAM role
###############################################################################

data "aws_iam_policy_document" "cluster_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "cluster" {
  name               = "${local.name}-cluster"
  assume_role_policy = data.aws_iam_policy_document.cluster_assume.json

  tags = {
    Name = "${local.name}-cluster"
  }
}

resource "aws_iam_role_policy_attachment" "cluster" {
  for_each = toset([
    "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonEKSClusterPolicy",
    "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonEKSVPCResourceController",
  ])

  role       = aws_iam_role.cluster.name
  policy_arn = each.value
}

###############################################################################
# Control-plane logs
#
# Created explicitly so the group carries our CMK and our retention. EKS creates
# it implicitly otherwise, with the AWS-managed key and never-expiring
# retention.
###############################################################################

resource "aws_cloudwatch_log_group" "cluster" {
  name              = "/aws/eks/${local.name}/cluster"
  retention_in_days = var.cluster_log_retention_days
  kms_key_id        = var.kms_key_arn_logs

  tags = {
    Name = "${local.name}-control-plane-logs"
  }
}

###############################################################################
# Cluster
###############################################################################

resource "aws_eks_cluster" "this" {
  name     = local.name
  role_arn = aws_iam_role.cluster.arn
  version  = var.kubernetes_version

  enabled_cluster_log_types = var.cluster_log_types

  vpc_config {
    subnet_ids              = var.subnet_ids
    security_group_ids      = [var.cluster_security_group_id]
    endpoint_private_access = true
    endpoint_public_access  = local.public_access_enabled
    public_access_cidrs     = local.public_access_enabled ? var.public_access_cidrs : null
  }

  # Envelope-encrypt Kubernetes Secrets with our own CMK. Without this an etcd
  # snapshot is a credential dump; with it, the snapshot is ciphertext and the
  # key policy decides who can read it.
  encryption_config {
    provider {
      key_arn = var.kms_key_arn_secrets
    }

    resources = ["secrets"]
  }

  access_config {
    # API mode only: the aws-auth ConfigMap is not consulted at all. Its failure
    # mode - a malformed edit locking every principal out of a running cluster,
    # with no way back in except AWS support - is not acceptable in production.
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = false
  }

  kubernetes_network_config {
    ip_family = "ipv4"
  }

  upgrade_policy {
    # STANDARD, not EXTENDED: staying on a supported version is the policy
    # (deployment.md 2.3), and extended support is a paid way to defer that.
    support_type = "STANDARD"
  }

  tags = {
    Name = local.name
  }

  depends_on = [
    aws_iam_role_policy_attachment.cluster,
    aws_cloudwatch_log_group.cluster,
  ]

  # The cluster itself is cattle (disaster-recovery.md 4.6) - but recreating it
  # means recreating the OIDC provider, which invalidates every IRSA trust
  # relationship in the account until they are re-applied, and re-syncing every
  # ArgoCD Application. That is a 9-12 minute planned operation and a much
  # longer unplanned one.
  #
  # To legitimately replace it: create the replacement cluster alongside, move
  # traffic with Route 53, then destroy the old one in a separate change.
  lifecycle {
    prevent_destroy = true
  }
}

###############################################################################
# OIDC provider for IRSA
###############################################################################

data "tls_certificate" "oidc" {
  url = aws_eks_cluster.this.identity[0].oidc[0].issuer
}

resource "aws_iam_openid_connect_provider" "this" {
  url             = aws_eks_cluster.this.identity[0].oidc[0].issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.oidc.certificates[0].sha1_fingerprint]

  tags = {
    Name = "${local.name}-oidc"
  }
}

###############################################################################
# Node IAM role
#
# One role shared by the node groups. This is the *node* identity - kubelet,
# the CNI and the container runtime - and it is deliberately minimal: every
# application permission is on an IRSA role instead, so a pod that escapes its
# service account inherits almost nothing.
###############################################################################

data "aws_iam_policy_document" "node_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name                 = "${local.name}-node"
  assume_role_policy   = data.aws_iam_policy_document.node_assume.json
  permissions_boundary = var.permissions_boundary_arn

  tags = {
    Name = "${local.name}-node"
  }
}

resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset([
    "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonEKSWorkerNodePolicy",
    "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
    # SSM Session Manager is the only node-access mechanism: there is no SSH
    # daemon and no key pair (security.md 2.2).
    "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore",
  ])

  role       = aws_iam_role.node.name
  policy_arn = each.value
}

# Note: AmazonEKS_CNI_Policy is deliberately NOT attached to the node role. It
# is attached to the VPC CNI's own IRSA role below, so that a compromised node
# cannot manipulate ENIs and IP assignments for pods it does not host.

###############################################################################
# Launch template
#
# Explicit rather than EKS-default, because three of its settings are security
# controls: IMDSv2 with a hop limit of 1, an encrypted root volume under our
# CMK, and no SSH key pair.
###############################################################################

resource "aws_launch_template" "node" {
  for_each = var.node_groups

  name_prefix = "${local.name}-${each.key}-"
  description = "Node launch template for the ${each.key} node group."

  vpc_security_group_ids = [var.node_security_group_id]

  block_device_mappings {
    device_name = "/dev/xvda"

    ebs {
      volume_size           = each.value.disk_size_gb
      volume_type           = "gp3"
      iops                  = 3000
      throughput            = 125
      encrypted             = true
      kms_key_id            = var.kms_key_arn_ebs
      delete_on_termination = true
    }
  }

  metadata_options {
    http_endpoint = "enabled"
    # IMDSv2 only. IMDSv1's unauthenticated GET is the second half of every
    # SSRF-to-instance-credentials chain.
    http_tokens = "required"
    # Hop limit 1: a container cannot reach IMDS through the node's network
    # namespace, because the packet has already taken its one hop.
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }

  monitoring {
    enabled = true
  }

  tag_specifications {
    resource_type = "instance"

    tags = {
      Name      = "${local.name}-${each.key}"
      NodeGroup = each.key
    }
  }

  tag_specifications {
    resource_type = "volume"

    tags = {
      Name      = "${local.name}-${each.key}"
      NodeGroup = each.key
    }
  }

  lifecycle {
    create_before_destroy = true
  }
}

###############################################################################
# Managed node groups
#
# These are the guaranteed, always-warm floor. Karpenter provides burst above
# them (deployment.md 2.3); the Karpenter NodePool and EC2NodeClass are
# Kubernetes objects and therefore ArgoCD's, not Terraform's - Terraform only
# creates the IAM role and the SQS interruption queue Karpenter needs.
###############################################################################

resource "aws_eks_node_group" "this" {
  for_each = var.node_groups

  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "${local.name}-${each.key}"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = var.subnet_ids

  instance_types = each.value.instance_types
  capacity_type  = each.value.capacity_type
  ami_type       = each.value.ami_type

  scaling_config {
    min_size     = each.value.min_size
    max_size     = each.value.max_size
    desired_size = each.value.desired_size
  }

  update_config {
    max_unavailable_percentage = each.value.max_unavailable_percentage
  }

  launch_template {
    id      = aws_launch_template.node[each.key].id
    version = aws_launch_template.node[each.key].latest_version
  }

  labels = merge(
    {
      "pp.node-group" = each.key
    },
    each.value.labels,
  )

  dynamic "taint" {
    for_each = each.value.taints

    content {
      key    = taint.value.key
      value  = taint.value.value
      effect = taint.value.effect
    }
  }

  tags = {
    Name      = "${local.name}-${each.key}"
    NodeGroup = each.key
  }

  lifecycle {
    # The HPA and Karpenter move desired_size at runtime. Terraform reverting it
    # on the next apply would scale the money path down mid-traffic.
    ignore_changes = [scaling_config[0].desired_size]
  }

  depends_on = [aws_iam_role_policy_attachment.node]
}

###############################################################################
# Add-ons, all versions pinned
###############################################################################

resource "aws_eks_addon" "vpc_cni" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "vpc-cni"
  addon_version               = var.addon_versions.vpc_cni
  service_account_role_arn    = aws_iam_role.platform["vpc-cni"].arn
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  configuration_values = jsonencode({
    env = {
      # Prefix delegation: a /28 per ENI. Raises pod density per node and cuts
      # the EC2 API calls a scale-out makes, which is what turns a CNI IP
      # exhaustion event from "outage" into "non-event" (deployment.md 2.2).
      ENABLE_PREFIX_DELEGATION = tostring(var.enable_prefix_delegation)
      WARM_PREFIX_TARGET       = "1"
      # In-CNI NetworkPolicy enforcement, so the default-deny policies are
      # enforced by the data path rather than by a separate agent.
      ENABLE_NETWORK_POLICY = "true"
      # Pods keep their own SG rules where a SecurityGroupPolicy applies (the
      # egress proxy).
      ENABLE_POD_ENI                    = "true"
      POD_SECURITY_GROUP_ENFORCING_MODE = "standard"
    }
  })

  tags = {
    Name = "${local.name}-vpc-cni"
  }
}

resource "aws_eks_addon" "coredns" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "coredns"
  addon_version               = var.addon_versions.coredns
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  configuration_values = jsonencode({
    # Three replicas minimum: DNS latency shows up directly in the p99, and a
    # two-replica CoreDNS during a node roll is a one-replica CoreDNS.
    replicaCount = 3

    resources = {
      requests = { cpu = "200m", memory = "128Mi" }
      limits   = { memory = "256Mi" }
    }

    podDisruptionBudget = {
      maxUnavailable = 1
    }
  })

  tags = {
    Name = "${local.name}-coredns"
  }

  depends_on = [aws_eks_node_group.this]
}

resource "aws_eks_addon" "kube_proxy" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "kube-proxy"
  addon_version               = var.addon_versions.kube_proxy
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  configuration_values = jsonencode({
    # IPVS: iptables mode rewrites the whole ruleset on every Service change,
    # and at this Service count that is measurable in the p99.
    ipvs = {
      scheduler = "rr"
    }

    mode = "ipvs"
  })

  tags = {
    Name = "${local.name}-kube-proxy"
  }
}

resource "aws_eks_addon" "ebs_csi" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "aws-ebs-csi-driver"
  addon_version               = var.addon_versions.ebs_csi
  service_account_role_arn    = aws_iam_role.platform["ebs-csi"].arn
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  tags = {
    Name = "${local.name}-ebs-csi"
  }

  depends_on = [aws_eks_node_group.this]
}

resource "aws_eks_addon" "pod_identity_agent" {
  count = var.addon_versions.pod_identity_agent != null ? 1 : 0

  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "eks-pod-identity-agent"
  addon_version               = var.addon_versions.pod_identity_agent
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  tags = {
    Name = "${local.name}-pod-identity-agent"
  }

  depends_on = [aws_eks_node_group.this]
}

###############################################################################
# Access entries
###############################################################################

resource "aws_eks_access_entry" "this" {
  for_each = var.access_entries

  cluster_name      = aws_eks_cluster.this.name
  principal_arn     = each.value.principal_arn
  kubernetes_groups = each.value.kubernetes_groups
  type              = each.value.type

  tags = {
    Name = "${local.name}-access-${each.key}"
  }
}

resource "aws_eks_access_policy_association" "this" {
  for_each = { for k, v in var.access_entries : k => v if v.policy_arn != null }

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value.principal_arn
  policy_arn    = each.value.policy_arn

  access_scope {
    type       = each.value.access_scope_type
    namespaces = each.value.access_scope_type == "namespace" ? each.value.namespaces : null
  }

  depends_on = [aws_eks_access_entry.this]
}

###############################################################################
# Karpenter interruption queue
#
# Karpenter itself is an ArgoCD-managed workload; this is the AWS-side plumbing
# it needs: an SQS queue receiving Spot interruption and instance-rebalance
# notices so a node can be drained before it disappears.
###############################################################################

resource "aws_sqs_queue" "karpenter_interruption" {
  name                      = "${local.name}-karpenter"
  message_retention_seconds = 300
  sqs_managed_sse_enabled   = true

  tags = {
    Name = "${local.name}-karpenter"
  }
}

data "aws_iam_policy_document" "karpenter_interruption_queue" {
  statement {
    sid    = "AllowEventBridgeToSend"
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com", "sqs.amazonaws.com"]
    }

    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.karpenter_interruption.arn]
  }

  statement {
    sid    = "DenyInsecureTransport"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions   = ["sqs:*"]
    resources = [aws_sqs_queue.karpenter_interruption.arn]

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_sqs_queue_policy" "karpenter_interruption" {
  queue_url = aws_sqs_queue.karpenter_interruption.id
  policy    = data.aws_iam_policy_document.karpenter_interruption_queue.json
}

resource "aws_cloudwatch_event_rule" "karpenter" {
  for_each = {
    spot_interruption = { source = ["aws.ec2"], detail_type = ["EC2 Spot Instance Interruption Warning"] }
    rebalance         = { source = ["aws.ec2"], detail_type = ["EC2 Instance Rebalance Recommendation"] }
    state_change      = { source = ["aws.ec2"], detail_type = ["EC2 Instance State-change Notification"] }
    health            = { source = ["aws.health"], detail_type = ["AWS Health Event"] }
  }

  name        = "${local.name}-karpenter-${each.key}"
  description = "Karpenter interruption handling: ${each.key}."

  event_pattern = jsonencode({
    source = each.value.source
    "detail-type" = each.value.detail_type
  })

  tags = {
    Name = "${local.name}-karpenter-${each.key}"
  }
}

resource "aws_cloudwatch_event_target" "karpenter" {
  for_each = aws_cloudwatch_event_rule.karpenter

  rule = each.value.name
  arn  = aws_sqs_queue.karpenter_interruption.arn
}
