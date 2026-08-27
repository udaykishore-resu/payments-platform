###############################################################################
# modules/aurora - Aurora PostgreSQL, optionally a member of a Global Database
#
# One module instance per cluster. The prod stack instantiates it twice:
# a primary in eu-west-1 and a secondary in eu-central-1, both attached to the
# same aws_rds_global_cluster.
#
# The parameters below are not defaults with the edges filed off - each one is
# there because something in the platform depends on it. The reasoning is in the
# comments next to each parameter, because the next person to "tidy up" this
# parameter group will read this file and nothing else.
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

locals {
  name          = var.cluster_identifier
  is_serverless = var.instance_class == "db.serverless"
  family        = "aurora-postgresql${split(".", var.engine_version)[0]}"
}

check "prod_guardrails" {
  assert {
    condition     = var.environment != "prod" || var.deletion_protection
    error_message = "deletion_protection must be true in prod."
  }

  assert {
    condition     = var.environment != "prod" || var.apply_immediately == false
    error_message = "apply_immediately must be false in prod: modifications must land in the maintenance window, when someone is expecting a failover."
  }

  assert {
    condition     = var.environment != "prod" || var.backup_retention_period == 35
    error_message = "prod requires the maximum 35-day backup retention (disaster-recovery.md 5.1)."
  }
}

###############################################################################
# Subnet group and parameter groups
###############################################################################

resource "aws_db_subnet_group" "this" {
  name       = "${local.name}-subnets"
  subnet_ids = var.subnet_ids

  tags = {
    Name = "${local.name}-subnets"
  }
}

resource "aws_rds_cluster_parameter_group" "this" {
  name        = "${local.name}-cluster"
  family      = local.family
  description = "Cluster parameters for ${local.name}. See comments: every entry is load-bearing."

  # --- Transport security ----------------------------------------------------
  parameter {
    name = "rds.force_ssl"
    # Refuse non-TLS connections at the server. The application already uses
    # sslmode=verify-full; this makes a misconfigured client fail rather than
    # silently downgrade.
    value        = "1"
    apply_method = "pending-reboot"
  }

  # --- Logging ---------------------------------------------------------------
  parameter {
    name = "log_min_duration_statement"
    # Anything slower than this is already a latency problem against the 250 ms
    # p99 API budget (baseline 18).
    value        = tostring(var.log_min_duration_statement_ms)
    apply_method = "immediate"
  }

  parameter {
    name         = "log_statement"
    value        = "ddl" # DDL only. `all` on a 5 000 TPS money path is a log bill and a PII hazard.
    apply_method = "immediate"
  }

  parameter {
    name = "log_lock_waits"
    # A migration queueing behind a long reader is the classic "one ALTER took
    # the site down" incident. This is how it becomes visible.
    value        = "1"
    apply_method = "immediate"
  }

  parameter {
    name         = "log_temp_files"
    value        = "1024" # kB. A query spilling to disk is a plan regression waiting to page someone.
    apply_method = "immediate"
  }

  parameter {
    name         = "log_autovacuum_min_duration"
    value        = "1000"
    apply_method = "immediate"
  }

  parameter {
    name = "log_connections"
    # PgBouncer means connections are long-lived and few; logging them is cheap
    # and answers "did the pool actually reconnect after the failover".
    value        = "1"
    apply_method = "immediate"
  }

  parameter {
    name         = "log_disconnections"
    value        = "1"
    apply_method = "immediate"
  }

  # --- Timeouts --------------------------------------------------------------
  parameter {
    name = "idle_in_transaction_session_timeout"
    # An idle transaction pins its snapshot: vacuum stops reclaiming, the tables
    # the money path reads bloat, and row locks a concurrent payment needs stay
    # held. No legitimate transaction here is long-lived, because gateway calls
    # are deliberately outside the transaction boundary.
    value        = tostring(var.idle_in_transaction_session_timeout_ms)
    apply_method = "immediate"
  }

  parameter {
    name         = "statement_timeout"
    value        = tostring(var.statement_timeout_ms)
    apply_method = "immediate"
  }

  parameter {
    name = "lock_timeout"
    # 5 s. A migration that cannot take its lock in five seconds must give up
    # rather than queue - because writers then queue behind *it*
    # (deployment.md 5.2).
    value        = "5000"
    apply_method = "immediate"
  }

  # --- Correctness -----------------------------------------------------------
  parameter {
    name = "row_security"
    # Row-Level Security is the last line of tenant isolation (multi-tenancy.md).
    # The application role is not BYPASSRLS; this parameter makes sure the
    # feature itself is not globally disabled.
    value        = "1"
    apply_method = "immediate"
  }

  parameter {
    name         = "default_transaction_isolation"
    value        = "read committed"
    apply_method = "immediate"
  }

  parameter {
    name = "synchronous_commit"
    # `on`. Aurora acknowledges after quorum in the local region's 6-way storage,
    # which is what makes the in-region RPO exactly 0
    # (disaster-recovery.md 4.1.1). Turning this off to chase write latency
    # would trade a correctness guarantee for a few hundred microseconds.
    value        = "on"
    apply_method = "immediate"
  }

  # --- Observability ---------------------------------------------------------
  parameter {
    name = "shared_preload_libraries"
    # pg_stat_statements underpins the "is anything still referencing this
    # column" check in the drop-column runbook (deployment.md 5.5).
    value        = "pg_stat_statements,auto_explain"
    apply_method = "pending-reboot"
  }

  parameter {
    name         = "pg_stat_statements.track"
    value        = "all"
    apply_method = "immediate"
  }

  parameter {
    name         = "auto_explain.log_min_duration"
    value        = "1000"
    apply_method = "immediate"
  }

  parameter {
    name         = "auto_explain.log_analyze"
    value        = "0" # ANALYZE re-executes the plan; on a money path that is not free.
    apply_method = "immediate"
  }

  parameter {
    name         = "track_io_timing"
    value        = "1"
    apply_method = "immediate"
  }

  # --- Connections -----------------------------------------------------------
  dynamic "parameter" {
    for_each = var.max_connections != null ? [1] : []

    content {
      name         = "max_connections"
      value        = tostring(var.max_connections)
      apply_method = "pending-reboot"
    }
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_db_parameter_group" "instance" {
  name        = "${local.name}-instance"
  family      = local.family
  description = "Instance parameters for ${local.name}."

  parameter {
    name         = "log_rotation_age"
    value        = "60"
    apply_method = "immediate"
  }

  lifecycle {
    create_before_destroy = true
  }
}

###############################################################################
# Enhanced monitoring role
###############################################################################

data "aws_iam_policy_document" "monitoring_assume" {
  count = var.monitoring_interval > 0 ? 1 : 0

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["monitoring.rds.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "monitoring" {
  count = var.monitoring_interval > 0 ? 1 : 0

  name               = "${local.name}-rds-monitoring"
  assume_role_policy = data.aws_iam_policy_document.monitoring_assume[0].json

  tags = {
    Name = "${local.name}-rds-monitoring"
  }
}

resource "aws_iam_role_policy_attachment" "monitoring" {
  count = var.monitoring_interval > 0 ? 1 : 0

  role       = aws_iam_role.monitoring[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}

###############################################################################
# CloudWatch log group
#
# Created ahead of the cluster so that it carries our CMK and our retention.
# RDS will otherwise create it implicitly with never-expiring retention and the
# AWS-managed key.
###############################################################################

resource "aws_cloudwatch_log_group" "postgresql" {
  for_each = toset(var.enabled_cloudwatch_logs_exports)

  name              = "/aws/rds/cluster/${local.name}/${each.value}"
  retention_in_days = var.cloudwatch_log_retention_days
  kms_key_id        = var.cloudwatch_log_kms_key_arn

  tags = {
    Name = "${local.name}-${each.value}"
  }
}

###############################################################################
# Master password
#
# Read from Secrets Manager rather than generated here: a generated password
# lands in the Terraform state file in plaintext, and the state file is then a
# credential store. The value itself is created out-of-band (see
# terraform/README.md, "Not managed here").
###############################################################################

data "aws_secretsmanager_secret_version" "master" {
  count = var.is_global_secondary || var.master_password_secret_arn == null ? 0 : 1

  secret_id = var.master_password_secret_arn
}

###############################################################################
# Cluster
###############################################################################

resource "aws_rds_cluster" "this" {
  cluster_identifier = local.name
  engine             = "aurora-postgresql"
  engine_version     = var.engine_version
  engine_mode        = "provisioned"

  # A Global Database secondary inherits the database and credentials from the
  # primary; setting them here is an error.
  database_name   = var.is_global_secondary ? null : var.database_name
  master_username = var.is_global_secondary ? null : var.master_username
  master_password = var.is_global_secondary || var.master_password_secret_arn == null ? null : data.aws_secretsmanager_secret_version.master[0].secret_string

  global_cluster_identifier = var.global_cluster_identifier

  db_subnet_group_name            = aws_db_subnet_group.this.name
  vpc_security_group_ids          = var.security_group_ids
  db_cluster_parameter_group_name = aws_rds_cluster_parameter_group.this.name

  port = 5432

  storage_encrypted = true
  kms_key_id        = var.kms_key_arn

  iam_database_authentication_enabled = var.iam_database_authentication_enabled

  # Backups and PITR. The secondary's backups are the primary's; a secondary
  # cannot set its own retention.
  backup_retention_period      = var.is_global_secondary ? null : var.backup_retention_period
  preferred_backup_window      = var.is_global_secondary ? null : var.preferred_backup_window
  preferred_maintenance_window = var.preferred_maintenance_window
  copy_tags_to_snapshot        = true

  # PITR is inherent to Aurora's continuous backup and is bounded by
  # backup_retention_period above; there is no separate switch. Backtrack is
  # deliberately NOT enabled: it is incompatible with Global Database, and PITR
  # plus a restore drill is the mechanism this platform commits to
  # (disaster-recovery.md 4.1).

  deletion_protection = var.deletion_protection
  apply_immediately   = var.apply_immediately

  enabled_cloudwatch_logs_exports = var.enabled_cloudwatch_logs_exports

  snapshot_identifier = var.snapshot_identifier

  # Skipping the final snapshot would make an accidental destroy unrecoverable.
  skip_final_snapshot       = false
  final_snapshot_identifier = "${local.name}-final-${formatdate("YYYYMMDDhhmmss", timestamp())}"

  dynamic "serverlessv2_scaling_configuration" {
    for_each = local.is_serverless ? [1] : []

    content {
      min_capacity = var.serverless_min_capacity
      max_capacity = var.serverless_max_capacity
    }
  }

  tags = {
    Name        = local.name
    ClusterRole = var.is_global_secondary ? "global-secondary" : "primary"
  }

  # This cluster holds every payment, every ledger entry and the hash-chained
  # audit record. Destroying it is not recoverable from anything Terraform
  # knows about.
  #
  # To legitimately remove it:
  #   1. Open a change record naming the cluster and the retention obligation it
  #      is under (docs/compliance.md 4.5 - financial records are the carve-out
  #      to crypto-shredding and cannot simply be deleted).
  #   2. Take a manual snapshot and copy it to pp-backup-vault; verify the copy
  #      restores (the monthly restore drill procedure).
  #   3. Set deletion_protection = false and apply. That is a separate,
  #      separately-approved change, and an SCP requires it to have happened in
  #      an earlier API call than the delete.
  #   4. Delete this lifecycle block in its own reviewed commit and apply. CI's
  #      "plan destroys a stateful resource" gate then demands the
  #      approved-destroy label and two approvals.
  lifecycle {
    prevent_destroy = true

    ignore_changes = [
      # Rolls on every plan; only the value at destroy time matters.
      final_snapshot_identifier,
      # A restore is a one-off operation; the cluster is not recreated from the
      # snapshot on every subsequent apply.
      snapshot_identifier,
      # The password is rotated out-of-band by the rotation Lambda.
      master_password,
    ]
  }
}

###############################################################################
# Instances
###############################################################################

resource "aws_rds_cluster_instance" "writer" {
  count = var.is_global_secondary ? 0 : 1

  identifier         = "${local.name}-writer"
  cluster_identifier = aws_rds_cluster.this.id
  instance_class     = var.instance_class
  engine             = aws_rds_cluster.this.engine
  engine_version     = aws_rds_cluster.this.engine_version

  db_subnet_group_name    = aws_db_subnet_group.this.name
  db_parameter_group_name = aws_db_parameter_group.instance.name
  ca_cert_identifier      = var.ca_cert_identifier

  # The writer must never be a candidate to be demoted below a reader during a
  # failover; tier 0 is the highest priority.
  promotion_tier = 0

  performance_insights_enabled          = var.performance_insights_enabled
  performance_insights_kms_key_id       = var.performance_insights_enabled ? var.kms_key_arn : null
  performance_insights_retention_period = var.performance_insights_enabled ? var.performance_insights_retention_period : null

  monitoring_interval = var.monitoring_interval
  monitoring_role_arn = var.monitoring_interval > 0 ? aws_iam_role.monitoring[0].arn : null

  auto_minor_version_upgrade = false # A minor upgrade causes a failover. It is scheduled, never inherited.
  apply_immediately          = var.apply_immediately
  publicly_accessible        = false

  tags = {
    Name         = "${local.name}-writer"
    InstanceRole = "writer"
  }
}

resource "aws_rds_cluster_instance" "reader" {
  count = var.reader_count

  identifier         = "${local.name}-reader-${count.index + 1}"
  cluster_identifier = aws_rds_cluster.this.id
  instance_class     = var.instance_class
  engine             = aws_rds_cluster.this.engine
  engine_version     = aws_rds_cluster.this.engine_version

  db_subnet_group_name    = aws_db_subnet_group.this.name
  db_parameter_group_name = aws_db_parameter_group.instance.name
  ca_cert_identifier      = var.ca_cert_identifier

  # All readers at the same tier: on an AZ loss, Aurora picks the reader with
  # the largest instance class, and they are identical here, so the choice comes
  # down to which one is healthy. That is the behaviour we want.
  promotion_tier = 1

  performance_insights_enabled          = var.performance_insights_enabled
  performance_insights_kms_key_id       = var.performance_insights_enabled ? var.kms_key_arn : null
  performance_insights_retention_period = var.performance_insights_enabled ? var.performance_insights_retention_period : null

  monitoring_interval = var.monitoring_interval
  monitoring_role_arn = var.monitoring_interval > 0 ? aws_iam_role.monitoring[0].arn : null

  auto_minor_version_upgrade = false
  apply_immediately          = var.apply_immediately
  publicly_accessible        = false

  tags = {
    Name         = "${local.name}-reader-${count.index + 1}"
    InstanceRole = "reader"
  }
}

###############################################################################
# Alarms that belong to the database rather than to the observability stack,
# because they must keep working when the observability stack does not
# (disaster-recovery.md 1.1: DR of observability is not a prerequisite for DR of
# the platform).
###############################################################################

resource "aws_cloudwatch_metric_alarm" "replication_lag" {
  count = var.is_global_secondary ? 1 : 0

  alarm_name          = "${local.name}-global-replication-lag"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  datapoints_to_alarm = 2
  threshold           = 5000 # milliseconds - the 5 s RPO budget (baseline 18)
  period              = 60
  statistic           = "Maximum"
  namespace           = "AWS/RDS"
  metric_name         = "AuroraGlobalDBReplicationLag"
  treat_missing_data  = "breaching" # Silence here means replication stopped, which is worse than high lag.

  alarm_description = "P1. Cross-region replication lag is at or above the RPO budget. See docs/disaster-recovery.md 4.1.2."

  dimensions = {
    DBClusterIdentifier = local.name
  }

  tags = {
    Name     = "${local.name}-global-replication-lag"
    Severity = "P1"
  }
}

resource "aws_cloudwatch_metric_alarm" "cpu" {
  alarm_name          = "${local.name}-cpu-high"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  threshold           = 80
  period              = 300
  statistic           = "Average"
  namespace           = "AWS/RDS"
  metric_name         = "CPUUtilization"
  treat_missing_data  = "notBreaching"

  alarm_description = "P2. Sustained CPU above 80% on ${local.name}."

  dimensions = {
    DBClusterIdentifier = local.name
  }

  tags = {
    Name     = "${local.name}-cpu-high"
    Severity = "P2"
  }
}

resource "aws_cloudwatch_metric_alarm" "free_local_storage" {
  alarm_name          = "${local.name}-free-local-storage-low"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 2
  threshold           = 10737418240 # 10 GiB
  period              = 300
  statistic           = "Minimum"
  namespace           = "AWS/RDS"
  metric_name         = "FreeLocalStorage"
  treat_missing_data  = "notBreaching"

  alarm_description = "P2. Local (temp) storage is running out - usually a query spilling to disk, occasionally a runaway sort in a migration."

  dimensions = {
    DBClusterIdentifier = local.name
  }

  tags = {
    Name     = "${local.name}-free-local-storage-low"
    Severity = "P2"
  }
}
