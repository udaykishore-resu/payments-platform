###############################################################################
# modules/msk - Kafka
#
# There is no cross-region replication here, and that is a decision, not a gap.
# Every event that matters is a row in outbox_events in the replicated database;
# Kafka is a transport, not a store of record. MirrorMaker2 and MSK Replicator
# both translate consumer offsets approximately, and an approximately-translated
# offset can *skip* a message - a skipped payment.captured.v1 is a missing
# ledger entry discovered days later. See disaster-recovery.md 4.2.
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
  name = var.cluster_name
}

check "auth_configuration" {
  assert {
    condition     = var.serverless || var.enable_iam_auth || var.enable_scram_auth
    error_message = "At least one authentication mechanism must be enabled. Unauthenticated access to the event backbone is not an option."
  }

  assert {
    condition     = var.enable_scram_auth == false || length(var.scram_secret_arns) > 0
    error_message = "enable_scram_auth requires scram_secret_arns."
  }

  assert {
    condition     = var.environment != "prod" || var.serverless == false
    error_message = "MSK Serverless has no broker configuration, so unclean.leader.election.enable and min.insync.replicas cannot be pinned. Not acceptable on the money path."
  }

  assert {
    condition     = var.broker_count % length(var.subnet_ids) == 0
    error_message = "broker_count must be a multiple of the subnet count so brokers are evenly spread across AZs."
  }
}

###############################################################################
# Broker configuration
#
# Three of these settings are correctness controls, not tuning knobs.
###############################################################################

resource "aws_msk_configuration" "this" {
  count = var.serverless ? 0 : 1

  name           = "${local.name}-config"
  kafka_versions = [var.kafka_version]
  description    = "Broker configuration for ${local.name}. min.insync.replicas and unclean.leader.election are correctness controls."

  server_properties = <<-PROPERTIES
    # --- Durability. These three lines are the reason a broker loss is not a
    # --- lost payment event.
    #
    # A partition must have two in-sync replicas to accept a write. With RF=3
    # that tolerates one broker (one AZ) being down while still requiring the
    # write to exist in two places before it is acknowledged.
    min.insync.replicas=2
    default.replication.factor=3
    #
    # If every in-sync replica for a partition is gone, the partition becomes
    # UNAVAILABLE rather than electing an out-of-sync replica as leader.
    # Unclean election silently discards committed writes. Availability is not
    # worth a lost payment.captured.v1.
    unclean.leader.election.enable=false

    # --- Topic creation is explicit. platformctl creates every topic with the
    # --- partition count and retention from docs/events.md 5.2; auto-creation
    # --- would produce a one-partition topic that silently caps throughput.
    auto.create.topics.enable=false
    delete.topic.enable=true

    num.partitions=${var.num_partitions_default}
    log.retention.hours=${var.log_retention_hours_default}

    # --- Compression at the broker is 'producer': the producer already
    # --- compresses, and re-compressing costs broker CPU and breaks zero-copy
    # --- transfer to consumers.
    compression.type=producer

    # --- Rebalance behaviour. A rolling deploy of a 30-instance consumer group
    # --- must not produce 30 stop-the-world rebalances.
    group.initial.rebalance.delay.ms=3000

    # --- Message size. A payment event is small; an oversized message is a bug
    # --- or an attack, and a broker-side cap is the cheapest place to stop it.
    message.max.bytes=1048576
    replica.fetch.max.bytes=2097152

    # --- Offset topic durability matches data topic durability. An offsets
    # --- topic with RF=1 turns a broker loss into duplicate processing.
    offsets.topic.replication.factor=3
    transaction.state.log.replication.factor=3
    transaction.state.log.min.isr=2
  PROPERTIES

  lifecycle {
    create_before_destroy = true
  }
}

###############################################################################
# Logging
###############################################################################

resource "aws_cloudwatch_log_group" "broker" {
  name              = "/aws/msk/${local.name}/broker"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.cloudwatch_log_kms_key_arn

  tags = {
    Name = "${local.name}-broker-logs"
  }
}

###############################################################################
# Provisioned cluster
###############################################################################

resource "aws_msk_cluster" "this" {
  count = var.serverless ? 0 : 1

  cluster_name           = local.name
  kafka_version          = var.kafka_version
  number_of_broker_nodes = var.broker_count
  enhanced_monitoring    = var.enhanced_monitoring

  broker_node_group_info {
    instance_type   = var.broker_instance_type
    client_subnets  = var.subnet_ids
    security_groups = var.security_group_ids

    storage_info {
      ebs_storage_info {
        volume_size = var.broker_ebs_volume_size_gb

        dynamic "provisioned_throughput" {
          for_each = var.broker_ebs_volume_size_gb >= 1000 ? [1] : []

          content {
            enabled           = true
            volume_throughput = 250
          }
        }
      }
    }
  }

  configuration_info {
    arn      = aws_msk_configuration.this[0].arn
    revision = aws_msk_configuration.this[0].latest_revision
  }

  client_authentication {
    sasl {
      iam   = var.enable_iam_auth
      scram = var.enable_scram_auth
    }

    # No TLS client-certificate authentication and, critically, no
    # `unauthenticated` block: a cluster that accepts unauthenticated
    # connections from inside the VPC is one misplaced pod away from an
    # unaudited producer.
  }

  encryption_info {
    encryption_at_rest_kms_key_arn = var.kms_key_arn

    encryption_in_transit {
      # TLS only. PLAINTEXT is not offered even in-VPC: "it is inside the VPC"
      # is precisely the implicit trust the zero-trust model forbids.
      client_broker = "TLS"
      in_cluster    = true
    }
  }

  logging_info {
    broker_logs {
      cloudwatch_logs {
        enabled   = true
        log_group = aws_cloudwatch_log_group.broker.name
      }
    }
  }

  open_monitoring {
    prometheus {
      jmx_exporter {
        enabled_in_broker = true
      }

      node_exporter {
        enabled_in_broker = true
      }
    }
  }

  tags = {
    Name = local.name
  }

  # The cluster holds in-flight events. Losing it is recoverable - the outbox
  # republishes - but the recovery is measured in minutes of catch-up at 5 000
  # TPS-equivalent volume, and it is never something to do accidentally.
  #
  # To legitimately remove it: drain the outbox to zero backlog, confirm
  # pp_consumer_lag is zero for every group, take the change through the same
  # approved-destroy gate as the database, then delete this block.
  lifecycle {
    prevent_destroy = true

    ignore_changes = [
      # Storage autoscaling adjusts volume_size out of band; Terraform must not
      # try to shrink it back on the next apply.
      broker_node_group_info[0].storage_info[0].ebs_storage_info[0].volume_size,
    ]
  }
}

resource "aws_msk_scram_secret_association" "this" {
  count = !var.serverless && var.enable_scram_auth ? 1 : 0

  cluster_arn     = aws_msk_cluster.this[0].arn
  secret_arn_list = var.scram_secret_arns
}

###############################################################################
# Storage autoscaling
#
# Broker storage filling up takes the partition offline. Autoscaling is a
# safety net, not a capacity plan - the alarm below still fires so somebody
# looks at why retention outgrew the plan.
###############################################################################

resource "aws_appautoscaling_target" "broker_storage" {
  count = !var.serverless && var.storage_autoscaling_max_gb != null ? 1 : 0

  service_namespace  = "kafka"
  resource_id        = aws_msk_cluster.this[0].arn
  scalable_dimension = "kafka:broker-storage:VolumeSize"
  # The floor is the declared volume size, not 1: Application Auto Scaling
  # rejects a target whose minimum is below the current capacity, and EBS
  # volumes cannot shrink anyway.
  min_capacity = var.broker_ebs_volume_size_gb
  max_capacity = var.storage_autoscaling_max_gb
}

resource "aws_appautoscaling_policy" "broker_storage" {
  count = !var.serverless && var.storage_autoscaling_max_gb != null ? 1 : 0

  name               = "${local.name}-broker-storage"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.broker_storage[0].service_namespace
  resource_id        = aws_appautoscaling_target.broker_storage[0].resource_id
  scalable_dimension = aws_appautoscaling_target.broker_storage[0].scalable_dimension

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "KafkaBrokerStorageUtilization"
    }

    target_value = 70
  }
}

###############################################################################
# Serverless cluster (dev)
###############################################################################

resource "aws_msk_serverless_cluster" "this" {
  count = var.serverless ? 1 : 0

  cluster_name = local.name

  vpc_config {
    subnet_ids         = var.subnet_ids
    security_group_ids = var.security_group_ids
  }

  client_authentication {
    sasl {
      iam {
        enabled = true
      }
    }
  }

  tags = {
    Name = local.name
  }
}

###############################################################################
# Alarms
###############################################################################

resource "aws_cloudwatch_metric_alarm" "broker_disk" {
  count = var.serverless ? 0 : 1

  alarm_name          = "${local.name}-broker-disk-high"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  threshold           = 80
  period              = 300
  statistic           = "Maximum"
  namespace           = "AWS/Kafka"
  metric_name         = "KafkaDataLogsDiskUsed"
  treat_missing_data  = "breaching"

  alarm_description = "P2. Broker storage above 80%. A full broker volume takes its partitions offline; with min.insync.replicas=2 the second one takes the topic offline."

  dimensions = {
    "Cluster Name" = local.name
  }

  tags = {
    Name     = "${local.name}-broker-disk-high"
    Severity = "P2"
  }
}

resource "aws_cloudwatch_metric_alarm" "under_min_isr" {
  count = var.serverless ? 0 : 1

  alarm_name          = "${local.name}-under-min-isr"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  threshold           = 0
  period              = 60
  statistic           = "Maximum"
  namespace           = "AWS/Kafka"
  metric_name         = "UnderMinIsrPartitionCount"
  treat_missing_data  = "notBreaching"

  alarm_description = "P1. At least one partition is below min.insync.replicas and is therefore refusing writes. The outbox will back up; that is the intended failure mode, and it needs a human."

  dimensions = {
    "Cluster Name" = local.name
  }

  tags = {
    Name     = "${local.name}-under-min-isr"
    Severity = "P1"
  }
}

resource "aws_cloudwatch_metric_alarm" "offline_partitions" {
  count = var.serverless ? 0 : 1

  alarm_name          = "${local.name}-offline-partitions"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  threshold           = 0
  period              = 60
  statistic           = "Maximum"
  namespace           = "AWS/Kafka"
  metric_name         = "OfflinePartitionsCount"
  treat_missing_data  = "notBreaching"

  alarm_description = "P1. Partitions with no leader. With unclean.leader.election disabled this is the deliberate fail-closed state, not a bug."

  dimensions = {
    "Cluster Name" = local.name
  }

  tags = {
    Name     = "${local.name}-offline-partitions"
    Severity = "P1"
  }
}
