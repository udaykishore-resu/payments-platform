###############################################################################
# modules/elasticache - Redis
#
# Nothing in this cache is a system of record (baseline 14.3). A total Redis
# outage costs latency, never correctness: the idempotency claim falls back to
# Postgres, rate limits fall back to per-pod local buckets, config falls back to
# a direct Aurora read. That property is what makes it acceptable for this
# cluster to have no cross-region replication at all.
#
# It is also why this module has no prevent_destroy on the replication group:
# unlike Aurora, MSK or the compliance buckets, losing it destroys nothing.
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
  name = var.replication_group_id

  # Cluster mode is selected by the parameter-group FAMILY, not by a parameter:
  # `cluster-enabled` only exists in the redis<major>.cluster.on family, and
  # setting it in the plain family is rejected at create time.
  family = var.cluster_mode_enabled ? "redis${split(".", var.engine_version)[0]}.cluster.on" : "redis${split(".", var.engine_version)[0]}"

  multi_az = var.replicas_per_node_group > 0
}

check "prod_topology" {
  assert {
    condition     = var.environment != "prod" || (var.cluster_mode_enabled && var.replicas_per_node_group >= 1)
    error_message = "prod requires cluster mode with at least one replica per shard so an AZ loss does not take a shard with it."
  }
}

data "aws_secretsmanager_secret_version" "auth_token" {
  secret_id = var.auth_token_secret_arn
}

resource "aws_elasticache_subnet_group" "this" {
  name       = "${local.name}-subnets"
  subnet_ids = var.subnet_ids

  tags = {
    Name = "${local.name}-subnets"
  }
}

resource "aws_elasticache_parameter_group" "this" {
  name        = "${local.name}-params"
  family      = local.family
  description = "Redis parameters for ${local.name}."

  parameter {
    name = "maxmemory-policy"
    # See variables.tf: volatile-lru protects TTL-less rate-limit buckets from
    # being evicted ahead of TTL-bearing cache entries.
    value = var.maxmemory_policy
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_cloudwatch_log_group" "slow" {
  name              = "/aws/elasticache/${local.name}/slow-log"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.cloudwatch_log_kms_key_arn

  tags = {
    Name = "${local.name}-slow-log"
  }
}

resource "aws_cloudwatch_log_group" "engine" {
  name              = "/aws/elasticache/${local.name}/engine-log"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.cloudwatch_log_kms_key_arn

  tags = {
    Name = "${local.name}-engine-log"
  }
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = local.name
  description          = "Idempotency mirror, rate-limit buckets, config snapshots and the JWKS cache for pp-${var.environment}."

  engine         = "redis"
  engine_version = var.engine_version
  node_type      = var.node_type
  port           = 6379

  parameter_group_name = aws_elasticache_parameter_group.this.name
  subnet_group_name    = aws_elasticache_subnet_group.this.name
  security_group_ids   = var.security_group_ids

  num_node_groups         = var.cluster_mode_enabled ? var.num_node_groups : null
  replicas_per_node_group = var.cluster_mode_enabled ? var.replicas_per_node_group : null
  num_cache_clusters      = var.cluster_mode_enabled ? null : max(1, var.replicas_per_node_group + 1)

  automatic_failover_enabled = local.multi_az
  multi_az_enabled           = local.multi_az

  # Encryption in transit and at rest, both non-negotiable: the cache holds
  # idempotency keys and config snapshots, and "it is only a cache" is not an
  # argument that survives an assessor asking what is in it.
  at_rest_encryption_enabled = true
  kms_key_id                 = var.kms_key_arn
  transit_encryption_enabled = true
  auth_token                 = data.aws_secretsmanager_secret_version.auth_token.secret_string
  auth_token_update_strategy = "ROTATE"

  snapshot_retention_limit = var.snapshot_retention_limit
  snapshot_window          = var.snapshot_window
  maintenance_window       = var.maintenance_window

  auto_minor_version_upgrade = false
  apply_immediately          = var.apply_immediately

  log_delivery_configuration {
    destination      = aws_cloudwatch_log_group.slow.name
    destination_type = "cloudwatch-logs"
    log_format       = "json"
    log_type         = "slow-log"
  }

  log_delivery_configuration {
    destination      = aws_cloudwatch_log_group.engine.name
    destination_type = "cloudwatch-logs"
    log_format       = "json"
    log_type         = "engine-log"
  }

  tags = {
    Name = local.name
  }

  lifecycle {
    ignore_changes = [
      # Rotated by the secret-rotation workflow, not by an apply.
      auth_token,
    ]
  }
}

###############################################################################
# Alarms
###############################################################################

resource "aws_cloudwatch_metric_alarm" "evictions" {
  alarm_name          = "${local.name}-evictions"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  threshold           = 1000
  period              = 300
  statistic           = "Sum"
  namespace           = "AWS/ElastiCache"
  metric_name         = "Evictions"
  treat_missing_data  = "notBreaching"

  alarm_description = "P3. Sustained evictions mean the working set no longer fits. Correctness is unaffected (nothing here is authoritative) but the idempotency fast path is degrading toward Postgres."

  dimensions = {
    ReplicationGroupId = local.name
  }

  tags = {
    Name     = "${local.name}-evictions"
    Severity = "P3"
  }
}

resource "aws_cloudwatch_metric_alarm" "cpu" {
  alarm_name          = "${local.name}-engine-cpu-high"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  threshold           = 75
  period              = 300
  statistic           = "Average"
  namespace           = "AWS/ElastiCache"
  metric_name         = "EngineCPUUtilization"
  treat_missing_data  = "notBreaching"

  alarm_description = "P2. Redis is single-threaded per shard; EngineCPUUtilization, not CPUUtilization, is the metric that matters."

  dimensions = {
    ReplicationGroupId = local.name
  }

  tags = {
    Name     = "${local.name}-engine-cpu-high"
    Severity = "P2"
  }
}
