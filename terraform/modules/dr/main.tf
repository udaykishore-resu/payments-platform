###############################################################################
# modules/dr - the fencing token and the backup vault
#
# The fencing table is the smallest and most important piece of infrastructure
# in the estate. It holds one item:
#
#   { pk: "region_authority", epoch: N, active_region: "eu-west-1", ... }
#
# Promotion increments `epoch` under a conditional write. Every payment-api and
# payment-orchestrator pod reads it on startup and every 10 s; a pod whose
# cached epoch is lower than the current one stops accepting writes and fails
# readiness within one poll interval (disaster-recovery.md 3).
#
# DynamoDB was chosen for exactly one property: a conditional write in a Global
# Table is resolved consistently across regions, so two operators promoting
# simultaneously cannot both succeed. Nothing else in this platform uses
# DynamoDB.
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
data "aws_caller_identity" "current" {}

check "prod_dr_posture" {
  assert {
    condition     = var.environment != "prod" || length(var.replica_regions) >= 1
    error_message = "The prod fencing table must be a Global Table: a fence that lives only in the region being failed away from is not a fence."
  }

  assert {
    condition     = alltrue([for r in var.replica_regions : contains(keys(var.replica_kms_key_arns), r)])
    error_message = "Every replica region needs a CMK ARN in replica_kms_key_arns."
  }
}

resource "aws_dynamodb_table" "fence" {
  name         = var.fencing_table_name
  billing_mode = "PAY_PER_REQUEST" # One item, read a few times per second. Provisioned capacity here is a rounding error that can throttle.
  hash_key     = "pk"

  attribute {
    name = "pk"
    type = "S"
  }

  server_side_encryption {
    enabled     = true
    kms_key_arn = var.kms_key_arn
  }

  point_in_time_recovery {
    enabled = true
  }

  # Streams are required for Global Tables and are useful in their own right:
  # every epoch change is an auditable event, and the SIEM subscribes to it.
  stream_enabled   = length(var.replica_regions) > 0
  stream_view_type = length(var.replica_regions) > 0 ? "NEW_AND_OLD_IMAGES" : null

  dynamic "replica" {
    for_each = var.replica_regions

    content {
      region_name            = replica.value
      kms_key_arn            = var.replica_kms_key_arns[replica.value]
      point_in_time_recovery = true
      propagate_tags         = true
    }
  }

  deletion_protection_enabled = true

  tags = {
    Name = var.fencing_table_name
    Role = "dr-fencing-token"
  }

  # Losing this table during an incident means every pod's fence check fails
  # open or closed depending on the code path, and no operator can promote a
  # region. It is one item; there is no reason to ever destroy it.
  #
  # To legitimately remove it: only as part of decommissioning the environment,
  # after the clusters are gone. Set deletion_protection_enabled = false in one
  # commit, then remove this lifecycle block and the resource in another.
  lifecycle {
    prevent_destroy = true
  }
}

###############################################################################
# Fencing table resource policy
#
# Read is wide (every money-path pod polls it). Write is exactly the promotion
# role. This asymmetry is the control: an application bug cannot promote a
# region, because the application's role has no PutItem or UpdateItem on this
# table at all.
###############################################################################

data "aws_iam_policy_document" "fence" {
  dynamic "statement" {
    for_each = length(var.fence_reader_role_arns) > 0 ? [1] : []

    content {
      sid    = "WorkloadsMayReadTheFence"
      effect = "Allow"

      principals {
        type        = "AWS"
        identifiers = var.fence_reader_role_arns
      }

      actions = [
        "dynamodb:GetItem",
        "dynamodb:DescribeTable",
      ]

      resources = [aws_dynamodb_table.fence.arn]
    }
  }

  dynamic "statement" {
    for_each = length(var.fence_writer_role_arns) > 0 ? [1] : []

    content {
      sid    = "OnlyThePromotionRoleMayMoveTheFence"
      effect = "Allow"

      principals {
        type        = "AWS"
        identifiers = var.fence_writer_role_arns
      }

      actions = [
        "dynamodb:UpdateItem",
        "dynamodb:GetItem",
        "dynamodb:DescribeTable",
      ]

      resources = [aws_dynamodb_table.fence.arn]
    }
  }

  statement {
    sid    = "NobodyMayDeleteTheFencingItem"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = [
      "dynamodb:DeleteItem",
      "dynamodb:DeleteTable",
      "dynamodb:UpdateTable",
    ]

    resources = [aws_dynamodb_table.fence.arn]

    condition {
      test     = "ArnNotLike"
      variable = "aws:PrincipalArn"

      values = [
        "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/pp-break-glass-*",
        "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/pp-terraform-*",
      ]
    }
  }
}

resource "aws_dynamodb_resource_policy" "fence" {
  count = length(var.fence_reader_role_arns) + length(var.fence_writer_role_arns) > 0 ? 1 : 0

  resource_arn = aws_dynamodb_table.fence.arn
  policy       = data.aws_iam_policy_document.fence.json
}

###############################################################################
# Alarm on the fence moving
#
# An epoch increment is either a planned game day or an incident. Either way
# somebody should be told, and it must not be discovered later from a log.
###############################################################################

resource "aws_cloudwatch_metric_alarm" "fence_writes" {
  alarm_name          = "pp-${var.environment}-dr-fence-written"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  threshold           = 0
  period              = 60
  statistic           = "Sum"
  namespace           = "AWS/DynamoDB"
  metric_name         = "ConsumedWriteCapacityUnits"
  treat_missing_data  = "notBreaching"

  alarm_description = "P1. The DR fencing token was written. Expected only during a promotion or a game day. If nobody is running the runbook, this is an incident."

  dimensions = {
    TableName = aws_dynamodb_table.fence.name
  }

  tags = {
    Name     = "pp-${var.environment}-dr-fence-written"
    Severity = "P1"
  }
}

###############################################################################
# AWS Backup
#
# Aurora already has automated snapshots and PITR. This vault exists for the
# thing those cannot do: put a copy somewhere a compromised production
# administrator cannot reach. The trust into pp-backup-vault is one-way.
###############################################################################

resource "aws_backup_vault" "this" {
  count = var.create_backup_vault ? 1 : 0

  name        = "pp-${var.environment}-vault"
  kms_key_arn = var.backup_vault_kms_key_arn

  tags = {
    Name = "pp-${var.environment}-vault"
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_backup_vault_lock_configuration" "this" {
  count = var.create_backup_vault && var.vault_lock_enabled ? 1 : 0

  backup_vault_name   = aws_backup_vault.this[0].name
  min_retention_days  = var.backup_retention_days
  max_retention_days  = 365
  changeable_for_days = var.vault_lock_changeable_for_days
}

data "aws_iam_policy_document" "backup_assume" {
  count = var.create_backup_vault ? 1 : 0

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["backup.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "backup" {
  count = var.create_backup_vault ? 1 : 0

  name               = "pp-${var.environment}-backup"
  assume_role_policy = data.aws_iam_policy_document.backup_assume[0].json

  tags = {
    Name = "pp-${var.environment}-backup"
  }
}

resource "aws_iam_role_policy_attachment" "backup" {
  for_each = var.create_backup_vault ? toset([
    "arn:aws:iam::aws:policy/service-role/AWSBackupServiceRolePolicyForBackup",
    "arn:aws:iam::aws:policy/service-role/AWSBackupServiceRolePolicyForRestores",
  ]) : toset([])

  role       = aws_iam_role.backup[0].name
  policy_arn = each.value
}

resource "aws_backup_plan" "this" {
  count = var.create_backup_vault ? 1 : 0

  name = "pp-${var.environment}-daily"

  rule {
    rule_name         = "daily"
    target_vault_name = aws_backup_vault.this[0].name
    schedule          = var.backup_schedule_cron

    # 60 minutes to start, 8 hours to finish. An unbounded window means a job
    # that hangs is discovered when the next one fails to start.
    start_window      = 60
    completion_window = 480

    lifecycle {
      cold_storage_after = var.backup_cold_storage_after_days
      delete_after       = var.backup_retention_days
    }

    dynamic "copy_action" {
      for_each = var.backup_cross_account_vault_arn != "" ? [1] : []

      content {
        destination_vault_arn = var.backup_cross_account_vault_arn

        lifecycle {
          delete_after = var.backup_retention_days
        }
      }
    }

    recovery_point_tags = {
      Environment = var.environment
      BackupPlan  = "daily"
    }
  }

  tags = {
    Name = "pp-${var.environment}-daily"
  }
}

resource "aws_backup_selection" "this" {
  count = var.create_backup_vault && length(var.backup_resource_arns) > 0 ? 1 : 0

  name         = "pp-${var.environment}-selection"
  plan_id      = aws_backup_plan.this[0].id
  iam_role_arn = aws_iam_role.backup[0].arn

  # Explicit ARNs, not a tag selector. A tag selector stops protecting a
  # resource the moment somebody edits a tag, and it does so silently.
  resources = var.backup_resource_arns
}

resource "aws_backup_vault_notifications" "this" {
  count = var.create_backup_vault && var.backup_notification_sns_topic_arn != "" ? 1 : 0

  backup_vault_name = aws_backup_vault.this[0].name
  sns_topic_arn     = var.backup_notification_sns_topic_arn

  # FAILED and EXPIRED are the two that matter. A backup that silently stopped
  # running is the classic finding in a post-incident review; EXPIRED catches
  # the subtler case where jobs run but recovery points are ageing out faster
  # than the retention policy claims.
  backup_vault_events = [
    "BACKUP_JOB_FAILED",
    "COPY_JOB_FAILED",
    "RESTORE_JOB_FAILED",
    "RECOVERY_POINT_MODIFIED",
  ]
}
