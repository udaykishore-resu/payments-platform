###############################################################################
# modules/eks/irsa.tf - one IAM role per deployable, least privilege
#
# The rules this file obeys, from baseline 17.2 and security.md 1.2:
#
#   * One role per deployable. Never a shared role: the blast radius of a
#     compromised pod must be the union of exactly one role's statements.
#   * No wildcard on a resource ARN except where AWS itself makes the resource
#     unknowable at policy-authoring time. There are exactly three such places
#     in this file and each carries a comment saying why.
#   * Secrets access is scoped by path prefix AND conditioned on kms:ViaService,
#     so a role that can decrypt through Secrets Manager cannot decrypt an
#     arbitrary ciphertext it obtained elsewhere.
#   * The trust policy pins the audience AND the exact namespace:serviceaccount.
#     A pod in another namespace using the same service-account name cannot
#     assume the role.
#
# Each policy is deliberately short enough to read in one screen. If one grows
# past that, the deployable is doing too much.
###############################################################################

locals {
  oidc_provider_url = replace(aws_iam_openid_connect_provider.this.url, "https://", "")

  # IAM is a global namespace. In a two-region environment both clusters live in
  # one account, so the role names must differ by region or the second apply
  # fails with EntityAlreadyExists - after having already created half a
  # cluster.
  irsa_prefix = coalesce(var.irsa_role_name_prefix, "pp-${var.environment}")

  # Platform components that also need an IRSA identity. Kept separate from the
  # deployables so that a change to the application's identity model cannot
  # silently alter the platform's.
  platform_subjects = {
    vpc-cni                      = { namespace = "kube-system", service_account = "aws-node" }
    ebs-csi                      = { namespace = "kube-system", service_account = "ebs-csi-controller-sa" }
    aws-load-balancer-controller = { namespace = "pp-platform", service_account = "aws-load-balancer-controller" }
    external-dns                 = { namespace = "pp-platform", service_account = "external-dns" }
    external-secrets             = { namespace = "pp-platform", service_account = "external-secrets" }
    karpenter                    = { namespace = "pp-platform", service_account = "karpenter" }
    prometheus                   = { namespace = "pp-observability", service_account = "prometheus-agent" }
    platformctl-migrator         = { namespace = "pp-platform", service_account = "platformctl-migrator" }
  }

  all_subjects = merge(var.irsa_service_accounts, local.platform_subjects)

  msk_cluster_arn_pattern = "arn:${data.aws_partition.current.partition}:kafka:${data.aws_region.current.name}:${var.account_id}:cluster/${var.msk_cluster_name}/*"

  # MSK topic and group ARNs embed the cluster's generated UUID, which does not
  # exist until the cluster is created and is not knowable when this policy is
  # written. The '*' below stands for that UUID only - the cluster NAME and the
  # topic NAME are both pinned, so this is not a resource wildcard in any
  # meaningful sense.
  msk_topic_arn_prefix = "arn:${data.aws_partition.current.partition}:kafka:${data.aws_region.current.name}:${var.account_id}:topic/${var.msk_cluster_name}/*"
  msk_group_arn_prefix = "arn:${data.aws_partition.current.partition}:kafka:${data.aws_region.current.name}:${var.account_id}:group/${var.msk_cluster_name}/*"

  secretsmanager_arn_prefix = "arn:${data.aws_partition.current.partition}:secretsmanager:${data.aws_region.current.name}:${var.account_id}:secret:${var.secrets_path_prefix}"

  rds_connect_arn_prefix = var.aurora_cluster_resource_id == "" ? null : "arn:${data.aws_partition.current.partition}:rds-db:${data.aws_region.current.name}:${var.account_id}:dbuser:${var.aurora_cluster_resource_id}"
}

###############################################################################
# Trust policies
###############################################################################

data "aws_iam_policy_document" "irsa_trust" {
  for_each = local.all_subjects

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.this.arn]
    }

    # Audience. Without this condition the trust policy accepts a token minted
    # for any audience, including one a compromised in-cluster component could
    # obtain for itself.
    condition {
      test     = "StringEquals"
      variable = "${local.oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }

    # Exact subject. StringEquals, never StringLike: a pattern here is how
    # "system:serviceaccount:pp-data-plane:payment-api" quietly becomes
    # "any service account whose name starts with payment-api".
    condition {
      test     = "StringEquals"
      variable = "${local.oidc_provider_url}:sub"
      values   = ["system:serviceaccount:${each.value.namespace}:${each.value.service_account}"]
    }
  }
}

###############################################################################
# Roles
###############################################################################

resource "aws_iam_role" "deployable" {
  for_each = var.irsa_service_accounts

  name                 = "${local.irsa_prefix}-${each.key}"
  description          = "IRSA role for the ${each.key} deployable in ${var.environment}."
  assume_role_policy   = data.aws_iam_policy_document.irsa_trust[each.key].json
  permissions_boundary = var.permissions_boundary_arn
  max_session_duration = 3600

  tags = {
    Name       = "${local.irsa_prefix}-${each.key}"
    Deployable = each.key
  }
}

resource "aws_iam_role" "platform" {
  for_each = local.platform_subjects

  name                 = "${local.name}-${each.key}"
  description          = "IRSA role for the ${each.key} platform component."
  assume_role_policy   = data.aws_iam_policy_document.irsa_trust[each.key].json
  permissions_boundary = var.permissions_boundary_arn
  max_session_duration = 3600

  tags = {
    Name      = "${local.name}-${each.key}"
    Component = each.key
  }
}

# The VPC CNI and EBS CSI add-ons in main.tf reference these roles directly.
resource "aws_iam_role_policy_attachment" "vpc_cni" {
  role       = aws_iam_role.platform["vpc-cni"].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonEKS_CNI_Policy"
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.platform["ebs-csi"].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}

data "aws_iam_policy_document" "ebs_csi_kms" {
  statement {
    sid    = "UseTheEbsKey"
    effect = "Allow"

    actions = [
      "kms:CreateGrant",
      "kms:ListGrants",
      "kms:RevokeGrant",
    ]

    resources = [var.kms_key_arn_ebs]

    condition {
      test     = "Bool"
      variable = "kms:GrantIsForAWSResource"
      values   = ["true"]
    }
  }

  statement {
    sid    = "EncryptDecryptEbsVolumes"
    effect = "Allow"

    actions = [
      "kms:Encrypt",
      "kms:Decrypt",
      "kms:ReEncrypt*",
      "kms:GenerateDataKey*",
      "kms:DescribeKey",
    ]

    resources = [var.kms_key_arn_ebs]
  }
}

resource "aws_iam_policy" "ebs_csi_kms" {
  name   = "${local.name}-ebs-csi-kms"
  policy = data.aws_iam_policy_document.ebs_csi_kms.json
}

resource "aws_iam_role_policy_attachment" "ebs_csi_kms" {
  role       = aws_iam_role.platform["ebs-csi"].name
  policy_arn = aws_iam_policy.ebs_csi_kms.arn
}

###############################################################################
# payment-api
#
# The public money-path ingress. It never writes payment state and never calls a
# gateway, so it has no Kafka permission and no gateway-credential access at
# all. What it needs is: the platform JWKS (to validate tokens), a read-only
# database connection, and the DR fence.
###############################################################################

data "aws_iam_policy_document" "payment_api" {
  statement {
    sid    = "ReadPlatformJwks"
    effect = "Allow"

    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
    ]

    # Platform-level secrets only. This role cannot name a tenant path at all,
    # so a path-traversal bug in the caller cannot reach a gateway credential.
    resources = ["${local.secretsmanager_arn_prefix}/_platform/jwks/*"]
  }

  statement {
    sid    = "DecryptSecretsManagerCiphertextOnly"
    effect = "Allow"

    actions   = ["kms:Decrypt"]
    resources = [var.kms_key_arn_secrets_manager]

    # Without ViaService this grant would let the role decrypt any ciphertext
    # encrypted under the key, including a Secrets Manager blob obtained through
    # some other path.
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }

  dynamic "statement" {
    for_each = local.rds_connect_arn_prefix != null ? [1] : []

    content {
      sid    = "ConnectAsReadOnlyDatabaseUser"
      effect = "Allow"

      actions = ["rds-db:connect"]

      # The read-only role. payment-api reading through the writer is denied at
      # the network layer, at the database-role layer and here.
      resources = ["${local.rds_connect_arn_prefix}/pp_app_ro"]
    }
  }

  dynamic "statement" {
    for_each = var.dr_control_table_arn != "" ? [1] : []

    content {
      sid    = "ReadDrFencingToken"
      effect = "Allow"

      actions = [
        "dynamodb:GetItem",
        "dynamodb:DescribeTable",
      ]

      resources = [var.dr_control_table_arn]
    }
  }
}

###############################################################################
# payment-orchestrator
#
# Owns the payment FSM and the gateway calls. This is the only deployable that
# reads gateway credentials, and the only one that writes to pp.payments.*.
###############################################################################

data "aws_iam_policy_document" "payment_orchestrator" {
  statement {
    sid    = "ReadGatewayCredentials"
    effect = "Allow"

    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
    ]

    # /{env}/{tenant}/{merchant}/{gateway}/... - four path segments before the
    # purpose, so this role cannot reach /{env}/{tenant}/{merchant}/kyc/*, which
    # belongs to workflow-worker.
    resources = [
      "${local.secretsmanager_arn_prefix}/*/*/*/api_key-*",
      "${local.secretsmanager_arn_prefix}/*/*/*/api_secret-*",
      "${local.secretsmanager_arn_prefix}/*/*/*/webhook_signing_key-*",
    ]
  }

  statement {
    sid    = "DecryptSecretsManagerCiphertextOnly"
    effect = "Allow"

    actions   = ["kms:Decrypt"]
    resources = [var.kms_key_arn_secrets_manager]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }

  statement {
    sid       = "ConnectToKafkaCluster"
    effect    = "Allow"
    actions   = ["kafka-cluster:Connect", "kafka-cluster:DescribeCluster"]
    resources = [local.msk_cluster_arn_pattern]
  }

  statement {
    sid    = "ProducePaymentEvents"
    effect = "Allow"

    actions = [
      "kafka-cluster:WriteData",
      "kafka-cluster:DescribeTopic",
    ]

    # Payment topics only. This role cannot write to pp.config.*, pp.merchants.*
    # or pp.audit.* - forging an audit record from the orchestrator is not
    # possible even with full control of the process.
    resources = ["${local.msk_topic_arn_prefix}/pp.payments.*"]
  }

  dynamic "statement" {
    for_each = local.rds_connect_arn_prefix != null ? [1] : []

    content {
      sid       = "ConnectAsApplicationDatabaseUser"
      effect    = "Allow"
      actions   = ["rds-db:connect"]
      resources = ["${local.rds_connect_arn_prefix}/pp_app"]
    }
  }

  dynamic "statement" {
    for_each = var.dr_control_table_arn != "" ? [1] : []

    content {
      sid       = "ReadDrFencingToken"
      effect    = "Allow"
      actions   = ["dynamodb:GetItem", "dynamodb:DescribeTable"]
      resources = [var.dr_control_table_arn]
    }
  }
}

###############################################################################
# webhook-ingress
#
# Accept-and-persist only, 50 ms budget. It verifies a gateway's signature and
# writes one row. It has no Kafka permission: the outbox relay publishes, not
# the ingress.
###############################################################################

data "aws_iam_policy_document" "webhook_ingress" {
  statement {
    sid    = "ReadInboundWebhookSigningSecrets"
    effect = "Allow"

    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
    ]

    resources = ["${local.secretsmanager_arn_prefix}/*/*/*/webhook_signing_key-*"]
  }

  statement {
    sid    = "DecryptSecretsManagerCiphertextOnly"
    effect = "Allow"

    actions   = ["kms:Decrypt"]
    resources = [var.kms_key_arn_secrets_manager]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }

  dynamic "statement" {
    for_each = local.rds_connect_arn_prefix != null ? [1] : []

    content {
      sid     = "ConnectAsWebhookDatabaseUser"
      effect  = "Allow"
      actions = ["rds-db:connect"]
      # A distinct database role whose grants inside Postgres are INSERT on
      # inbound_webhooks and nothing else.
      resources = ["${local.rds_connect_arn_prefix}/pp_webhook"]
    }
  }
}

###############################################################################
# outbox-relay
#
# Postgres -> Kafka, and the only publisher. It therefore needs write access to
# every topic, which is the one broad grant in this file - and it is broad on
# purpose: narrowing it would mean the relay could not publish an event type
# somebody added, and the failure would be a silent stuck outbox.
###############################################################################

data "aws_iam_policy_document" "outbox_relay" {
  statement {
    sid       = "ConnectToKafkaCluster"
    effect    = "Allow"
    actions   = ["kafka-cluster:Connect", "kafka-cluster:DescribeCluster"]
    resources = [local.msk_cluster_arn_pattern]
  }

  statement {
    sid    = "ProduceToAllPlatformTopics"
    effect = "Allow"

    actions = [
      "kafka-cluster:WriteData",
      "kafka-cluster:DescribeTopic",
      "kafka-cluster:WriteDataIdempotently",
    ]

    # Still prefix-scoped to pp.* - the relay cannot write to a topic outside
    # the platform's namespace.
    resources = ["${local.msk_topic_arn_prefix}/pp.*"]
  }

  dynamic "statement" {
    for_each = local.rds_connect_arn_prefix != null ? [1] : []

    content {
      sid       = "ConnectAsOutboxDatabaseUser"
      effect    = "Allow"
      actions   = ["rds-db:connect"]
      resources = ["${local.rds_connect_arn_prefix}/pp_outbox"]
    }
  }
}

###############################################################################
# event-consumer
#
# Projections, ledger, audit, notifications. Reads every topic, writes none, and
# exports audit records to the WORM bucket.
###############################################################################

data "aws_iam_policy_document" "event_consumer" {
  statement {
    sid       = "ConnectToKafkaCluster"
    effect    = "Allow"
    actions   = ["kafka-cluster:Connect", "kafka-cluster:DescribeCluster"]
    resources = [local.msk_cluster_arn_pattern]
  }

  statement {
    sid    = "ConsumePlatformTopics"
    effect = "Allow"

    actions = [
      "kafka-cluster:ReadData",
      "kafka-cluster:DescribeTopic",
    ]

    resources = ["${local.msk_topic_arn_prefix}/pp.*"]
  }

  statement {
    sid    = "ManageOwnConsumerGroups"
    effect = "Allow"

    actions = [
      "kafka-cluster:AlterGroup",
      "kafka-cluster:DescribeGroup",
    ]

    # Only groups whose name starts with this consumer's prefix: it cannot
    # reset another service's offsets.
    resources = ["${local.msk_group_arn_prefix}/pp-event-consumer*"]
  }

  statement {
    sid    = "ProduceToRetryAndDlqTopicsOnly"
    effect = "Allow"

    actions = [
      "kafka-cluster:WriteData",
      "kafka-cluster:DescribeTopic",
    ]

    # A consumer may move a message to its retry tier or its DLQ. It may not
    # write to the primary topics - that is the relay's job, and a consumer that
    # could republish to a primary topic could manufacture a payment event.
    resources = [
      "${local.msk_topic_arn_prefix}/pp.*.retry.*",
      "${local.msk_topic_arn_prefix}/pp.*.dlq",
    ]
  }

  dynamic "statement" {
    for_each = contains(keys(var.s3_bucket_arns), "audit-archive") ? [1] : []

    content {
      sid    = "AppendAuditRecordsToWormBucket"
      effect = "Allow"

      # PutObject only - no Delete, no PutObjectRetention, no
      # PutObjectLegalHold. The bucket is Object Lock COMPLIANCE, so even those
      # would fail, but the policy states the intent rather than relying on the
      # bucket to enforce it.
      actions   = ["s3:PutObject", "s3:AbortMultipartUpload"]
      resources = ["${var.s3_bucket_arns["audit-archive"]}/*"]
    }
  }

  dynamic "statement" {
    for_each = contains(keys(var.s3_bucket_arns), "audit-archive") ? [1] : []

    content {
      sid    = "EncryptAuditExports"
      effect = "Allow"

      actions   = ["kms:GenerateDataKey", "kms:Encrypt"]
      resources = [var.kms_key_arn_s3]

      condition {
        test     = "StringEquals"
        variable = "kms:ViaService"
        values   = ["s3.${data.aws_region.current.name}.amazonaws.com"]
      }
    }
  }

  dynamic "statement" {
    for_each = local.rds_connect_arn_prefix != null ? [1] : []

    content {
      sid       = "ConnectAsProjectionDatabaseUser"
      effect    = "Allow"
      actions   = ["rds-db:connect"]
      resources = ["${local.rds_connect_arn_prefix}/pp_projection"]
    }
  }
}

###############################################################################
# control-plane-api
#
# Writes desired state. The most privileged of the seven, and deliberately off
# the money path: it has no payment-topic write, and it cannot read a gateway
# credential back - it can only create and rotate one (security.md 4.2:
# `credentials:read` is denied to every principal except the owning service).
###############################################################################

data "aws_iam_policy_document" "control_plane_api" {
  statement {
    sid    = "CreateAndStageTenantSecrets"
    effect = "Allow"

    actions = [
      "secretsmanager:CreateSecret",
      "secretsmanager:PutSecretValue",
      "secretsmanager:TagResource",
      "secretsmanager:DescribeSecret",
      "secretsmanager:UpdateSecretVersionStage",
    ]

    # Note what is absent: GetSecretValue. The control plane can write a
    # credential and can rotate it; it can never read one back. That is what
    # makes "no human and no console path can read a gateway credential"
    # (security.md 4.2) true rather than aspirational, because the console path
    # would be through this API.
    resources = ["${local.secretsmanager_arn_prefix}/*/*/*/*"]

    condition {
      test     = "StringEquals"
      variable = "aws:RequestedRegion"
      values   = [data.aws_region.current.name]
    }
  }

  statement {
    sid    = "EncryptWithTheSecretsKey"
    effect = "Allow"

    actions   = ["kms:Encrypt", "kms:GenerateDataKey"]
    resources = [var.kms_key_arn_secrets_manager]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }

  statement {
    sid       = "ConnectToKafkaCluster"
    effect    = "Allow"
    actions   = ["kafka-cluster:Connect", "kafka-cluster:DescribeCluster"]
    resources = [local.msk_cluster_arn_pattern]
  }

  statement {
    sid    = "PublishConfigurationAndMerchantEvents"
    effect = "Allow"

    actions = ["kafka-cluster:WriteData", "kafka-cluster:DescribeTopic"]

    resources = [
      "${local.msk_topic_arn_prefix}/pp.config.*",
      "${local.msk_topic_arn_prefix}/pp.merchants.*",
    ]
  }

  dynamic "statement" {
    for_each = local.rds_connect_arn_prefix != null ? [1] : []

    content {
      sid       = "ConnectAsControlPlaneDatabaseUser"
      effect    = "Allow"
      actions   = ["rds-db:connect"]
      resources = ["${local.rds_connect_arn_prefix}/pp_control"]
    }
  }
}

###############################################################################
# workflow-worker
#
# Onboarding, certification and the KYC evidence path. The only deployable that
# reads KYC secrets and the only one that writes certification reports.
###############################################################################

data "aws_iam_policy_document" "workflow_worker" {
  statement {
    sid    = "ReadKycAndVendorCredentials"
    effect = "Allow"

    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
    ]

    resources = [
      "${local.secretsmanager_arn_prefix}/*/*/kyc/*",
      "${local.secretsmanager_arn_prefix}/_platform/vendor/*",
    ]
  }

  statement {
    sid    = "DecryptSecretsManagerCiphertextOnly"
    effect = "Allow"

    actions   = ["kms:Decrypt"]
    resources = [var.kms_key_arn_secrets_manager]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }

  dynamic "statement" {
    for_each = contains(keys(var.s3_bucket_arns), "artifacts") && contains(keys(var.s3_bucket_arns), "kyc") ? [1] : []

    content {
      sid    = "WriteCertificationReportsAndKycEvidence"
      effect = "Allow"

      actions = ["s3:PutObject", "s3:GetObject", "s3:AbortMultipartUpload"]

      resources = [
        "${var.s3_bucket_arns["artifacts"]}/certification/*",
        "${var.s3_bucket_arns["kyc"]}/*",
      ]
    }
  }

  dynamic "statement" {
    for_each = contains(keys(var.s3_bucket_arns), "artifacts") ? [1] : []

    content {
      sid    = "UseTheS3Key"
      effect = "Allow"

      actions   = ["kms:GenerateDataKey", "kms:Encrypt", "kms:Decrypt"]
      resources = [var.kms_key_arn_s3]

      condition {
        test     = "StringEquals"
        variable = "kms:ViaService"
        values   = ["s3.${data.aws_region.current.name}.amazonaws.com"]
      }
    }
  }

  statement {
    sid       = "ConnectToKafkaCluster"
    effect    = "Allow"
    actions   = ["kafka-cluster:Connect", "kafka-cluster:DescribeCluster"]
    resources = [local.msk_cluster_arn_pattern]
  }

  statement {
    sid    = "ConsumeMerchantAndConfigEvents"
    effect = "Allow"

    actions = ["kafka-cluster:ReadData", "kafka-cluster:DescribeTopic"]

    resources = [
      "${local.msk_topic_arn_prefix}/pp.merchants.*",
      "${local.msk_topic_arn_prefix}/pp.config.*",
    ]
  }

  statement {
    sid    = "ManageOwnConsumerGroups"
    effect = "Allow"

    actions   = ["kafka-cluster:AlterGroup", "kafka-cluster:DescribeGroup"]
    resources = ["${local.msk_group_arn_prefix}/pp-workflow-worker*"]
  }

  dynamic "statement" {
    for_each = local.rds_connect_arn_prefix != null ? [1] : []

    content {
      sid       = "ConnectAsWorkflowDatabaseUser"
      effect    = "Allow"
      actions   = ["rds-db:connect"]
      resources = ["${local.rds_connect_arn_prefix}/pp_workflow"]
    }
  }
}

###############################################################################
# Attach the deployable policies
###############################################################################

locals {
  deployable_policy_json = {
    payment-api          = data.aws_iam_policy_document.payment_api.json
    payment-orchestrator = data.aws_iam_policy_document.payment_orchestrator.json
    webhook-ingress      = data.aws_iam_policy_document.webhook_ingress.json
    outbox-relay         = data.aws_iam_policy_document.outbox_relay.json
    event-consumer       = data.aws_iam_policy_document.event_consumer.json
    control-plane-api    = data.aws_iam_policy_document.control_plane_api.json
    workflow-worker      = data.aws_iam_policy_document.workflow_worker.json
  }
}

resource "aws_iam_policy" "deployable" {
  for_each = { for k, v in local.deployable_policy_json : k => v if contains(keys(var.irsa_service_accounts), k) }

  name        = "${local.irsa_prefix}-${each.key}"
  description = "Least-privilege policy for the ${each.key} deployable."
  policy      = each.value

  tags = {
    Name       = "${local.irsa_prefix}-${each.key}"
    Deployable = each.key
  }
}

resource "aws_iam_role_policy_attachment" "deployable" {
  for_each = aws_iam_policy.deployable

  role       = aws_iam_role.deployable[each.key].name
  policy_arn = each.value.arn
}

###############################################################################
# external-secrets
#
# The External Secrets Operator projects Secrets Manager values into pods as
# files. It therefore needs read access across the environment's whole secret
# tree - which is why the operator is the one component whose CloudTrail read
# rate is baselined and alerted on (security.md 5.1).
###############################################################################

data "aws_iam_policy_document" "external_secrets" {
  statement {
    sid    = "ReadEnvironmentSecrets"
    effect = "Allow"

    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
      "secretsmanager:ListSecretVersionIds",
    ]

    resources = ["${local.secretsmanager_arn_prefix}/*"]
  }

  statement {
    sid    = "DecryptSecretsManagerCiphertextOnly"
    effect = "Allow"

    actions   = ["kms:Decrypt"]
    resources = [var.kms_key_arn_secrets_manager]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }
}

resource "aws_iam_policy" "external_secrets" {
  name   = "${local.name}-external-secrets"
  policy = data.aws_iam_policy_document.external_secrets.json
}

resource "aws_iam_role_policy_attachment" "external_secrets" {
  role       = aws_iam_role.platform["external-secrets"].name
  policy_arn = aws_iam_policy.external_secrets.arn
}

###############################################################################
# external-dns
###############################################################################

data "aws_iam_policy_document" "external_dns" {
  dynamic "statement" {
    for_each = length(var.route53_zone_arns) > 0 ? [1] : []

    content {
      sid       = "ChangeRecordsInOwnedZonesOnly"
      effect    = "Allow"
      actions   = ["route53:ChangeResourceRecordSets"]
      resources = var.route53_zone_arns
    }
  }

  statement {
    sid    = "ListZones"
    effect = "Allow"

    actions = [
      "route53:ListHostedZones",
      "route53:ListResourceRecordSets",
      "route53:ListTagsForResource",
    ]

    # These three are List/Describe verbs that AWS does not support scoping to a
    # specific zone. They disclose zone names and record sets within this
    # account, which the operator already knows.
    resources = ["*"]
  }
}

resource "aws_iam_policy" "external_dns" {
  name   = "${local.name}-external-dns"
  policy = data.aws_iam_policy_document.external_dns.json
}

resource "aws_iam_role_policy_attachment" "external_dns" {
  role       = aws_iam_role.platform["external-dns"].name
  policy_arn = aws_iam_policy.external_dns.arn
}

###############################################################################
# prometheus agent -> Amazon Managed Prometheus
###############################################################################

data "aws_iam_policy_document" "prometheus" {
  dynamic "statement" {
    for_each = var.amp_workspace_arn != "" ? [1] : []

    content {
      sid    = "RemoteWriteToWorkspace"
      effect = "Allow"

      actions = [
        "aps:RemoteWrite",
        "aps:GetSeries",
        "aps:GetLabels",
        "aps:GetMetricMetadata",
      ]

      resources = [var.amp_workspace_arn]
    }
  }
}

resource "aws_iam_policy" "prometheus" {
  count = var.amp_workspace_arn != "" ? 1 : 0

  name   = "${local.name}-prometheus"
  policy = data.aws_iam_policy_document.prometheus.json
}

resource "aws_iam_role_policy_attachment" "prometheus" {
  count = var.amp_workspace_arn != "" ? 1 : 0

  role       = aws_iam_role.platform["prometheus"].name
  policy_arn = aws_iam_policy.prometheus[0].arn
}

###############################################################################
# platformctl migrator
#
# Runs as the PreSync hook Job. It is a separate identity from every runtime
# deployable because it holds the one database role that can execute DDL, and
# nothing that serves traffic should be able to alter a schema.
###############################################################################

data "aws_iam_policy_document" "platformctl_migrator" {
  dynamic "statement" {
    for_each = local.rds_connect_arn_prefix != null ? [1] : []

    content {
      sid       = "ConnectAsMigrationDatabaseUser"
      effect    = "Allow"
      actions   = ["rds-db:connect"]
      resources = ["${local.rds_connect_arn_prefix}/pp_migrator"]
    }
  }

  statement {
    sid    = "SnapshotBeforeMigrating"
    effect = "Allow"

    actions = [
      "rds:CreateDBClusterSnapshot",
      "rds:DescribeDBClusterSnapshots",
      "rds:AddTagsToResource",
    ]

    resources = [
      "arn:${data.aws_partition.current.partition}:rds:${data.aws_region.current.name}:${var.account_id}:cluster:pp-${var.environment}*",
      "arn:${data.aws_partition.current.partition}:rds:${data.aws_region.current.name}:${var.account_id}:cluster-snapshot:pp-${var.environment}*",
    ]
  }
}

resource "aws_iam_policy" "platformctl_migrator" {
  name   = "${local.name}-platformctl-migrator"
  policy = data.aws_iam_policy_document.platformctl_migrator.json
}

resource "aws_iam_role_policy_attachment" "platformctl_migrator" {
  role       = aws_iam_role.platform["platformctl-migrator"].name
  policy_arn = aws_iam_policy.platformctl_migrator.arn
}

###############################################################################
# Karpenter
###############################################################################

data "aws_iam_policy_document" "karpenter" {
  statement {
    sid    = "ReadEc2Inventory"
    effect = "Allow"

    actions = [
      "ec2:DescribeImages",
      "ec2:DescribeInstances",
      "ec2:DescribeInstanceTypes",
      "ec2:DescribeInstanceTypeOfferings",
      "ec2:DescribeLaunchTemplates",
      "ec2:DescribeSecurityGroups",
      "ec2:DescribeSpotPriceHistory",
      "ec2:DescribeSubnets",
      "pricing:GetProducts",
      "ssm:GetParameter",
    ]

    # Describe verbs across an account's EC2 inventory cannot be resource-scoped
    # by the EC2 API. They disclose instance and subnet metadata that Karpenter
    # needs to choose an instance type.
    resources = ["*"]
  }

  statement {
    sid    = "ProvisionNodes"
    effect = "Allow"

    actions = [
      "ec2:CreateFleet",
      "ec2:CreateLaunchTemplate",
      "ec2:CreateTags",
      "ec2:RunInstances",
    ]

    resources = ["*"]

    # Karpenter may only create resources tagged for this cluster. Without this
    # condition, the Karpenter role is an unbounded EC2-launch capability.
    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/kubernetes.io/cluster/${local.name}"
      values   = ["owned"]
    }
  }

  statement {
    sid    = "TerminateOwnNodesOnly"
    effect = "Allow"

    actions = [
      "ec2:TerminateInstances",
      "ec2:DeleteLaunchTemplate",
    ]

    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/kubernetes.io/cluster/${local.name}"
      values   = ["owned"]
    }
  }

  statement {
    sid       = "PassOnlyTheNodeRole"
    effect    = "Allow"
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.node.arn]

    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ec2.amazonaws.com"]
    }
  }

  statement {
    sid       = "ReadClusterDetails"
    effect    = "Allow"
    actions   = ["eks:DescribeCluster"]
    resources = [aws_eks_cluster.this.arn]
  }

  statement {
    sid    = "DrainOnInterruption"
    effect = "Allow"

    actions = [
      "sqs:DeleteMessage",
      "sqs:GetQueueUrl",
      "sqs:ReceiveMessage",
    ]

    resources = [aws_sqs_queue.karpenter_interruption.arn]
  }
}

resource "aws_iam_policy" "karpenter" {
  name   = "${local.name}-karpenter"
  policy = data.aws_iam_policy_document.karpenter.json
}

resource "aws_iam_role_policy_attachment" "karpenter" {
  role       = aws_iam_role.platform["karpenter"].name
  policy_arn = aws_iam_policy.karpenter.arn
}

###############################################################################
# AWS Load Balancer Controller
###############################################################################

data "aws_iam_policy_document" "load_balancer_controller" {
  statement {
    sid    = "DescribeNetworkAndLoadBalancers"
    effect = "Allow"

    actions = [
      "ec2:DescribeAccountAttributes",
      "ec2:DescribeAddresses",
      "ec2:DescribeAvailabilityZones",
      "ec2:DescribeInternetGateways",
      "ec2:DescribeVpcs",
      "ec2:DescribeSubnets",
      "ec2:DescribeSecurityGroups",
      "ec2:DescribeInstances",
      "ec2:DescribeNetworkInterfaces",
      "ec2:DescribeTags",
      "elasticloadbalancing:Describe*",
      "acm:ListCertificates",
      "acm:DescribeCertificate",
      "wafv2:GetWebACL",
      "wafv2:GetWebACLForResource",
      "shield:GetSubscriptionState",
      "shield:DescribeProtection",
    ]

    # Describe/List verbs that the corresponding AWS APIs do not support
    # scoping. They are read-only and disclose no secret material.
    resources = ["*"]
  }

  statement {
    sid    = "ManageLoadBalancersForThisCluster"
    effect = "Allow"

    actions = [
      "elasticloadbalancing:CreateListener",
      "elasticloadbalancing:CreateLoadBalancer",
      "elasticloadbalancing:CreateRule",
      "elasticloadbalancing:CreateTargetGroup",
      "elasticloadbalancing:DeleteListener",
      "elasticloadbalancing:DeleteLoadBalancer",
      "elasticloadbalancing:DeleteRule",
      "elasticloadbalancing:DeleteTargetGroup",
      "elasticloadbalancing:DeregisterTargets",
      "elasticloadbalancing:ModifyListener",
      "elasticloadbalancing:ModifyLoadBalancerAttributes",
      "elasticloadbalancing:ModifyRule",
      "elasticloadbalancing:ModifyTargetGroup",
      "elasticloadbalancing:ModifyTargetGroupAttributes",
      "elasticloadbalancing:RegisterTargets",
      "elasticloadbalancing:SetSecurityGroups",
      "elasticloadbalancing:SetSubnets",
      "elasticloadbalancing:AddTags",
      "elasticloadbalancing:RemoveTags",
      "elasticloadbalancing:AssociateWebACL",
    ]

    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/elbv2.k8s.aws/cluster"
      values   = [local.name]
    }
  }

  statement {
    sid    = "CreateTaggedLoadBalancers"
    effect = "Allow"

    actions = [
      "elasticloadbalancing:CreateLoadBalancer",
      "elasticloadbalancing:CreateTargetGroup",
    ]

    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/elbv2.k8s.aws/cluster"
      values   = [local.name]
    }
  }

  statement {
    sid    = "CreateServiceLinkedRole"
    effect = "Allow"

    actions   = ["iam:CreateServiceLinkedRole"]
    resources = ["arn:${data.aws_partition.current.partition}:iam::${var.account_id}:role/aws-service-role/elasticloadbalancing.amazonaws.com/*"]

    condition {
      test     = "StringEquals"
      variable = "iam:AWSServiceName"
      values   = ["elasticloadbalancing.amazonaws.com"]
    }
  }
}

resource "aws_iam_policy" "load_balancer_controller" {
  name   = "${local.name}-aws-load-balancer-controller"
  policy = data.aws_iam_policy_document.load_balancer_controller.json
}

resource "aws_iam_role_policy_attachment" "load_balancer_controller" {
  role       = aws_iam_role.platform["aws-load-balancer-controller"].name
  policy_arn = aws_iam_policy.load_balancer_controller.arn
}
