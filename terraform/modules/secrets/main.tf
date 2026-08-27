###############################################################################
# modules/secrets - Secrets Manager containers, policies, replicas, rotation
#
# What this module manages: the secret *containers*, their KMS key, their
# resource policies, their cross-region replicas and the rotation lambdas.
#
# What it deliberately does not manage: the secret *values*. A value written by
# Terraform is a value stored in the Terraform state file in plaintext, and the
# state file is then the most valuable object in the estate - readable by
# everyone who can run a plan, versioned forever in S3, and copied into every
# CI job's working directory. Values are written once by an operator through
# `platformctl secrets seed` or minted by the rotation lambda, and Terraform
# never reads them back.
#
# This is the single most common way a "secure" Terraform estate leaks
# credentials, so the module makes it structurally impossible rather than
# discouraged: no aws_secretsmanager_secret_version resource exists here.
###############################################################################

terraform {
  required_version = ">= 1.9.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.60.0, < 6.0.0"
    }
  }
}

data "aws_region" "current" {}

locals {
  name = "pp-${var.environment}"
}

###############################################################################
# Secrets
###############################################################################

resource "aws_secretsmanager_secret" "this" {
  for_each = var.secrets

  name        = "${var.path_prefix}/${each.key}"
  description = each.value.description
  kms_key_id  = var.kms_key_arn

  # 30-day recovery window. Immediate deletion is available and is never used:
  # deleting a gateway credential by mistake and discovering it during the next
  # settlement run is a real failure mode.
  recovery_window_in_days = each.value.recovery_window_days

  dynamic "replica" {
    for_each = var.replica_regions

    content {
      region     = replica.value.region
      kms_key_id = replica.value.kms_key_arn
    }
  }

  tags = {
    Name               = "${var.path_prefix}/${each.key}"
    DataClassification = each.value.data_classification
    RotationDays       = each.value.rotation_days == null ? "none" : tostring(each.value.rotation_days)
  }

  lifecycle {
    # Deleting a secret container orphans every reference to it in
    # gateway_credentials_meta and breaks every payment through that gateway.
    # Removal procedure: disable the credential at the gateway, confirm zero
    # references in the database, then remove the entry from var.secrets and
    # this lifecycle block in one reviewed PR.
    prevent_destroy = true
  }
}

###############################################################################
# Resource policies
#
# The IRSA role policy is the primary control. This resource policy is the
# second, independent one: an over-broad IAM policy is not sufficient, because
# the secret itself also has to agree. Three checks have to fail together for a
# credential to leak - IAM, this policy, and the VPC endpoint policy.
###############################################################################

data "aws_iam_policy_document" "secret" {
  for_each = var.secrets

  # Explicit deny for every human role. Denies are evaluated before allows and
  # cannot be overridden by an account admin adding a permissive IAM policy.
  statement {
    sid    = "DenyHumanPrincipals"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:BatchGetSecretValue",
    ]

    resources = ["*"]

    condition {
      test     = "ArnLike"
      variable = "aws:PrincipalArn"
      values   = [var.denied_human_principal_pattern]
    }
  }

  # Deny any read that did not arrive through the VPC endpoint. A credential
  # fetched from outside the VPC - from a laptop with a leaked role, from a
  # Lambda in another account - is refused even with a valid IAM policy.
  statement {
    sid    = "DenyAccessOutsideVpcEndpoint"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions   = ["secretsmanager:GetSecretValue"]
    resources = ["*"]

    condition {
      test     = "Null"
      variable = "aws:SourceVpce"
      values   = ["true"]
    }

    condition {
      # The rotation lambda and the break-glass role are the two exceptions, and
      # both are named rather than pattern-matched loosely.
      test     = "ArnNotLike"
      variable = "aws:PrincipalArn"

      values = [
        "arn:aws:iam::${var.account_id}:role/${local.name}-secret-rotation-*",
        "arn:aws:iam::${var.account_id}:role/pp-break-glass-*",
      ]
    }
  }

  dynamic "statement" {
    for_each = length(each.value.readers) > 0 ? [1] : []

    content {
      sid    = "AllowNamedWorkloadReaders"
      effect = "Allow"

      principals {
        type        = "AWS"
        identifiers = each.value.readers
      }

      actions = [
        "secretsmanager:GetSecretValue",
        "secretsmanager:DescribeSecret",
        "secretsmanager:ListSecretVersionIds",
      ]

      resources = ["*"]
    }
  }

  dynamic "statement" {
    for_each = each.value.rotation_lambda_name != null ? [1] : []

    content {
      sid    = "AllowRotationLambda"
      effect = "Allow"

      principals {
        type        = "AWS"
        identifiers = ["arn:aws:iam::${var.account_id}:role/${local.name}-secret-rotation-${each.value.rotation_lambda_name}"]
      }

      actions = [
        "secretsmanager:GetSecretValue",
        "secretsmanager:PutSecretValue",
        "secretsmanager:DescribeSecret",
        "secretsmanager:UpdateSecretVersionStage",
      ]

      resources = ["*"]
    }
  }
}

resource "aws_secretsmanager_secret_policy" "this" {
  for_each = var.secrets

  secret_arn = aws_secretsmanager_secret.this[each.key].arn
  policy     = data.aws_iam_policy_document.secret[each.key].json

  # Without this, a policy that denies the caller can be applied and then not be
  # removable. Terraform's own role is exempted by the deny conditions above,
  # but the flag is cheap insurance against a future edit that is not.
  block_public_policy = true
}

###############################################################################
# Rotation lambdas
###############################################################################

data "aws_iam_policy_document" "rotation_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "rotation" {
  for_each = var.rotation_lambdas

  name               = "${local.name}-secret-rotation-${each.key}"
  assume_role_policy = data.aws_iam_policy_document.rotation_assume.json

  tags = {
    Name = "${local.name}-secret-rotation-${each.key}"
  }
}

data "aws_iam_policy_document" "rotation" {
  for_each = var.rotation_lambdas

  statement {
    sid    = "RotateSecretsUnderThisPathOnly"
    effect = "Allow"

    actions = [
      "secretsmanager:DescribeSecret",
      "secretsmanager:GetSecretValue",
      "secretsmanager:PutSecretValue",
      "secretsmanager:UpdateSecretVersionStage",
      "secretsmanager:GetRandomPassword",
    ]

    # Scoped to this environment's path prefix. The rotation lambda for staging
    # cannot touch a production secret even if its role were assumed.
    resources = ["arn:aws:secretsmanager:${data.aws_region.current.name}:${var.account_id}:secret:${var.path_prefix}/*"]

    condition {
      test     = "StringEquals"
      variable = "secretsmanager:resource/AllowRotationLambdaArn"
      values   = ["arn:aws:lambda:${data.aws_region.current.name}:${var.account_id}:function:${local.name}-rotate-${each.key}"]
    }
  }

  statement {
    sid    = "UseTheSecretsKey"
    effect = "Allow"

    actions = [
      "kms:Decrypt",
      "kms:GenerateDataKey",
    ]

    resources = [var.kms_key_arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }

  statement {
    sid    = "WriteOwnLogs"
    effect = "Allow"

    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]

    resources = ["arn:aws:logs:${data.aws_region.current.name}:${var.account_id}:log-group:/aws/lambda/${local.name}-rotate-${each.key}:*"]
  }

  statement {
    sid    = "ManageOwnEni"
    effect = "Allow"

    # Lambda-in-VPC requires these and they cannot be resource-scoped: the ENI
    # does not exist when the permission is evaluated. The condition narrows it
    # to ENIs in this account's subnets, which is the tightest available form.
    actions = [
      "ec2:CreateNetworkInterface",
      "ec2:DescribeNetworkInterfaces",
      "ec2:DeleteNetworkInterface",
      "ec2:AssignPrivateIpAddresses",
      "ec2:UnassignPrivateIpAddresses",
    ]

    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:RequestedRegion"
      values   = [data.aws_region.current.name]
    }
  }
}

resource "aws_iam_policy" "rotation" {
  for_each = var.rotation_lambdas

  name   = "${local.name}-secret-rotation-${each.key}"
  policy = data.aws_iam_policy_document.rotation[each.key].json
}

resource "aws_iam_role_policy_attachment" "rotation" {
  for_each = var.rotation_lambdas

  role       = aws_iam_role.rotation[each.key].name
  policy_arn = aws_iam_policy.rotation[each.key].arn
}

resource "aws_cloudwatch_log_group" "rotation" {
  for_each = var.rotation_lambdas

  name              = "/aws/lambda/${local.name}-rotate-${each.key}"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.cloudwatch_log_kms_key_arn

  tags = {
    Name = "${local.name}-rotate-${each.key}"
  }
}

resource "aws_lambda_function" "rotation" {
  for_each = var.rotation_lambdas

  function_name = "${local.name}-rotate-${each.key}"
  description   = each.value.description
  role          = aws_iam_role.rotation[each.key].arn

  s3_bucket         = each.value.s3_bucket
  s3_key            = each.value.s3_key
  s3_object_version = each.value.s3_object_version

  handler       = each.value.handler
  runtime       = each.value.runtime
  architectures = each.value.architectures
  timeout       = each.value.timeout_seconds
  memory_size   = each.value.memory_mb

  # In the VPC, so Secrets Manager is reached over the interface endpoint and a
  # gateway-side rotation call goes out through the allowlisted egress path.
  vpc_config {
    subnet_ids         = var.subnet_ids
    security_group_ids = var.security_group_ids
  }

  environment {
    variables = merge(
      {
        PP_ENV         = var.environment
        PP_SECRET_PATH = var.path_prefix
      },
      each.value.environment,
    )
  }

  # Reserve concurrency at 2: rotation is not throughput work, and an unbounded
  # rotation lambda that starts looping can exhaust the gateway's own API rate
  # limit and lock the platform out of its own credentials.
  reserved_concurrent_executions = 2

  tracing_config {
    mode = "Active"
  }

  tags = {
    Name = "${local.name}-rotate-${each.key}"
  }

  depends_on = [
    aws_iam_role_policy_attachment.rotation,
    aws_cloudwatch_log_group.rotation,
  ]
}

resource "aws_lambda_permission" "rotation" {
  for_each = var.rotation_lambdas

  statement_id   = "AllowSecretsManagerInvoke"
  action         = "lambda:InvokeFunction"
  function_name  = aws_lambda_function.rotation[each.key].function_name
  principal      = "secretsmanager.amazonaws.com"
  source_account = var.account_id
}

resource "aws_secretsmanager_secret_rotation" "this" {
  for_each = { for k, v in var.secrets : k => v if v.rotation_days != null && contains(keys(var.rotation_lambdas), coalesce(v.rotation_lambda_name, "")) }

  secret_id           = aws_secretsmanager_secret.this[each.key].id
  rotation_lambda_arn = aws_lambda_function.rotation[each.value.rotation_lambda_name].arn

  rotation_rules {
    automatically_after_days = each.value.rotation_days
  }

  # Do not rotate the moment the resource is created: the first rotation should
  # be triggered deliberately by an operator who is watching, not by an apply.
  rotate_immediately = false

  depends_on = [aws_lambda_permission.rotation]
}
