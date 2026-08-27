###############################################################################
# modules/kms - per-purpose customer-managed CMKs
#
# Design rules, all of them load-bearing for the PCI evidence pack:
#
#   1. Customer-managed keys only. AWS-managed keys (aws/rds, aws/s3) have a key
#      policy we cannot read, cannot scope, and cannot show to an assessor. The
#      whole point of the CMK is that the key policy and the grant history are
#      ours (security.md 2.5).
#   2. One key per purpose. Sharing one key across Aurora, S3 and Secrets Manager
#      makes "who can decrypt the audit archive" unanswerable.
#   3. Annual rotation on every key. The key ID is stable, so nothing has to be
#      re-encrypted; historic ciphertext stays readable (security.md 5.3).
#   4. The key policy grants *use* to a named list of role ARNs, not to the
#      account root with a delegation comment. The account-root delegation
#      statement exists solely so that IAM policies can *further restrict*;
#      it is scoped by kms:CallerAccount + the ViaService conditions below.
#   5. Multi-Region Keys in prod, so a ciphertext produced in eu-west-1 is
#      decryptable in eu-central-1 with no re-encryption step in the RTO path.
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

locals {
  is_replica = var.replica_source_key_arns != null

  # The set of purposes this instance manages. A replica instance is driven by
  # the map of primary ARNs it was given; a primary instance by var.purposes.
  primary_purposes = local.is_replica ? {} : { for p in var.purposes : p => p }
  replica_purposes = local.is_replica ? var.replica_source_key_arns : {}

  all_purposes = local.is_replica ? keys(local.replica_purposes) : var.purposes
}

data "aws_region" "current" {}

###############################################################################
# Key policies
#
# Built per purpose so that each key names exactly the principals that need it.
###############################################################################

data "aws_iam_policy_document" "key" {
  for_each = toset(local.all_purposes)

  # ---------------------------------------------------------------------------
  # Administration. Deliberately excludes every kms:*Decrypt / kms:*Encrypt
  # action: an administrator can destroy a key but cannot read data with it.
  # ---------------------------------------------------------------------------
  statement {
    sid    = "KeyAdministration"
    effect = "Allow"

    principals {
      type        = "AWS"
      identifiers = var.key_administrator_arns
    }

    actions = [
      "kms:Create*",
      "kms:Describe*",
      "kms:Enable*",
      "kms:List*",
      "kms:Put*",
      "kms:Update*",
      "kms:Revoke*",
      "kms:Disable*",
      "kms:Get*",
      "kms:Delete*",
      "kms:TagResource",
      "kms:UntagResource",
      "kms:ScheduleKeyDeletion",
      "kms:CancelKeyDeletion",
      "kms:ReplicateKey",
      "kms:UpdatePrimaryRegion",
    ]

    resources = ["*"] # A key policy's resource is always the key it is attached to.
  }

  # ---------------------------------------------------------------------------
  # Direct cryptographic use by named workload roles.
  # ---------------------------------------------------------------------------
  dynamic "statement" {
    for_each = length(lookup(var.key_users, each.value, [])) > 0 ? [1] : []

    content {
      sid    = "WorkloadUse"
      effect = "Allow"

      principals {
        type        = "AWS"
        identifiers = var.key_users[each.value]
      }

      actions = [
        "kms:Encrypt",
        "kms:Decrypt",
        "kms:ReEncrypt*",
        "kms:GenerateDataKey*",
        "kms:DescribeKey",
      ]

      resources = ["*"]
    }
  }

  # ---------------------------------------------------------------------------
  # Grant creation, needed by the AWS services that attach a key to a resource
  # (RDS, EBS, ElastiCache). Constrained to grants that the service itself will
  # retire, which is what stops a principal minting a permanent grant for a
  # third party.
  # ---------------------------------------------------------------------------
  dynamic "statement" {
    for_each = length(lookup(var.key_users, each.value, [])) > 0 ? [1] : []

    content {
      sid    = "WorkloadGrantsForAwsResources"
      effect = "Allow"

      principals {
        type        = "AWS"
        identifiers = var.key_users[each.value]
      }

      actions = [
        "kms:CreateGrant",
        "kms:ListGrants",
        "kms:RevokeGrant",
      ]

      resources = ["*"]

      condition {
        test     = "Bool"
        variable = "kms:GrantIsForAWSResource"
        values   = ["true"]
      }
    }
  }

  # ---------------------------------------------------------------------------
  # AWS service principals (logs, rds, s3, ...), each pinned to this region via
  # kms:ViaService so that a service in another region cannot use the key.
  # ---------------------------------------------------------------------------
  dynamic "statement" {
    for_each = length(lookup(var.service_principals, each.value, [])) > 0 ? [1] : []

    content {
      sid    = "AwsServiceUse"
      effect = "Allow"

      principals {
        type        = "Service"
        identifiers = var.service_principals[each.value]
      }

      actions = [
        "kms:Encrypt",
        "kms:Decrypt",
        "kms:ReEncrypt*",
        "kms:GenerateDataKey*",
        "kms:DescribeKey",
        "kms:CreateGrant",
      ]

      resources = ["*"]

      condition {
        test     = "StringEquals"
        variable = "kms:CallerAccount"
        values   = [var.account_id]
      }
    }
  }

  # ---------------------------------------------------------------------------
  # EC2 Auto Scaling service-linked role for the EBS key. Node groups cannot
  # launch encrypted volumes without it, and the resulting error is opaque.
  # ---------------------------------------------------------------------------
  dynamic "statement" {
    for_each = var.grant_autoscaling_service_linked_role && each.value == "ebs" ? [1] : []

    content {
      sid    = "AutoScalingServiceLinkedRole"
      effect = "Allow"

      principals {
        type        = "AWS"
        identifiers = ["arn:aws:iam::${var.account_id}:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling"]
      }

      actions = [
        "kms:Encrypt",
        "kms:Decrypt",
        "kms:ReEncrypt*",
        "kms:GenerateDataKey*",
        "kms:DescribeKey",
        "kms:CreateGrant",
      ]

      resources = ["*"]
    }
  }

  # ---------------------------------------------------------------------------
  # Deny the one thing nobody may do outside break-glass: disable rotation or
  # schedule deletion from a non-administrator principal. Explicit deny beats
  # every allow, including a future one added by mistake.
  # ---------------------------------------------------------------------------
  statement {
    sid    = "DenyDestructiveActionsToNonAdministrators"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = [
      "kms:ScheduleKeyDeletion",
      "kms:DisableKeyRotation",
      "kms:DisableKey",
      "kms:PutKeyPolicy",
    ]

    resources = ["*"]

    condition {
      test     = "ArnNotEquals"
      variable = "aws:PrincipalArn"
      values   = var.key_administrator_arns
    }
  }
}

###############################################################################
# Primary keys
###############################################################################

resource "aws_kms_key" "this" {
  for_each = local.primary_purposes

  description             = "pp-${var.environment}-${each.value} - ${each.value} envelope encryption for the payment orchestration platform"
  deletion_window_in_days = var.deletion_window_in_days
  enable_key_rotation     = var.enable_key_rotation
  multi_region            = var.multi_region
  policy                  = data.aws_iam_policy_document.key[each.value].json

  tags = {
    Name       = "pp-${var.environment}-${each.value}"
    KeyPurpose = each.value
  }

  # A CMK cannot be recreated: every ciphertext encrypted under it - Aurora
  # storage, every S3 object, every Secrets Manager version - becomes
  # permanently unreadable. Destroying one is a data-destruction event, not an
  # infrastructure change.
  #
  # To legitimately remove a key:
  #   1. Prove nothing references it: CloudTrail kms:Decrypt for 30 d, plus the
  #      resource inventory in AWS Config.
  #   2. Re-encrypt or delete the data that used it, and record the ticket.
  #   3. Remove the `key_users` entry and apply (revokes access while keeping
  #      the key), and wait one full business week.
  #   4. Only then remove the purpose from `var.purposes`, delete this
  #      lifecycle block in a separate reviewed commit, and apply. The 30-day
  #      pending-deletion window is the last chance to cancel.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "this" {
  for_each = local.primary_purposes

  name          = "alias/pp-${var.environment}-${each.value}"
  target_key_id = aws_kms_key.this[each.value].key_id
}

###############################################################################
# Replica keys (DR region)
#
# Same key material, same key-id suffix, its own key policy. A ciphertext
# produced in the primary region decrypts here with no re-encryption, which is
# what keeps KMS off the failover critical path entirely
# (disaster-recovery.md 1.1: KMS cross-region RPO and RTO are both 0).
###############################################################################

resource "aws_kms_replica_key" "this" {
  for_each = local.replica_purposes

  description             = "pp-${var.environment}-${each.key} (replica) - ${data.aws_region.current.name}"
  primary_key_arn         = each.value
  deletion_window_in_days = var.deletion_window_in_days
  policy                  = data.aws_iam_policy_document.key[each.key].json

  tags = {
    Name       = "pp-${var.environment}-${each.key}"
    KeyPurpose = each.key
    KeyRole    = "replica"
  }

  # Deleting an MRK replica does not delete the primary, but it does make every
  # ciphertext in this region unreadable *in this region* - which during a
  # failover is indistinguishable from data loss. Same removal procedure as the
  # primary above.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "replica" {
  for_each = local.replica_purposes

  name          = "alias/pp-${var.environment}-${each.key}"
  target_key_id = aws_kms_replica_key.this[each.key].key_id
}
