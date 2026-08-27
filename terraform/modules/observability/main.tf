###############################################################################
# modules/observability - AMP, Managed Grafana, log groups, alarms, budgets
#
# Scope note: the recording rules, alerting rules and dashboards are code, and
# they live in deployments/observability/, applied by ArgoCD. What Terraform
# owns is the *workspaces* those rules run in, the log groups AWS services write
# to, the alarm plumbing, and the budget guardrails.
#
# Deliberately absent: X-Ray. Tracing is OTel-native into Tempo on EKS, because
# tail sampling needs a collector we control and X-Ray would add a trace-format
# translation on every span (deployment.md 2.6).
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

###############################################################################
# Amazon Managed Prometheus
###############################################################################

resource "aws_prometheus_workspace" "this" {
  count = var.create_prometheus_workspace ? 1 : 0

  alias = "${var.name_prefix}-amp"

  logging_configuration {
    log_group_arn = "${aws_cloudwatch_log_group.amp[0].arn}:*"
  }

  tags = {
    Name = "${var.name_prefix}-amp"
  }
}

resource "aws_cloudwatch_log_group" "amp" {
  count = var.create_prometheus_workspace ? 1 : 0

  name              = "/aws/prometheus/${var.name_prefix}"
  retention_in_days = 30
  kms_key_id        = var.kms_key_arn_logs

  tags = {
    Name = "${var.name_prefix}-amp"
  }
}

# AMP retention is a workspace-level setting on the AMP API surface, not an
# argument on the workspace resource in this provider major. It is applied by
# `platformctl observability set-retention --days=<n>` from the same pipeline
# that applies this stack, and asserted by the drift job - see
# terraform/README.md, "Not managed here". var.prometheus_retention_days is the
# declared value the drift job compares against.
#
#   Declared retention: var.prometheus_retention_days

resource "aws_prometheus_alert_manager_definition" "this" {
  count = var.create_prometheus_workspace ? 1 : 0

  workspace_id = aws_prometheus_workspace.this[0].id

  definition = <<-YAML
    alertmanager_config: |
      route:
        # Group by the alert and the service, not by instance: an AZ loss that
        # fires the same alert on forty pods must be one page, not forty.
        group_by: ['alertname', 'service', 'severity']
        group_wait: 30s
        group_interval: 5m
        repeat_interval: 4h
        receiver: platform-default
        routes:
          - matchers:
              - severity = "P1"
            receiver: pagerduty-p1
            group_wait: 0s
            repeat_interval: 30m
          - matchers:
              - severity = "P2"
            receiver: pagerduty-p2
      receivers:
        - name: platform-default
          sns_configs:
            - topic_arn: ${aws_sns_topic.alerts.arn}
              sigv4:
                region: ${data.aws_region.current.name}
        - name: pagerduty-p1
          sns_configs:
            - topic_arn: ${aws_sns_topic.alerts.arn}
              sigv4:
                region: ${data.aws_region.current.name}
        - name: pagerduty-p2
          sns_configs:
            - topic_arn: ${aws_sns_topic.alerts.arn}
              sigv4:
                region: ${data.aws_region.current.name}
  YAML
}

###############################################################################
# Amazon Managed Grafana
###############################################################################

data "aws_iam_policy_document" "grafana_assume" {
  count = var.create_grafana_workspace ? 1 : 0

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["grafana.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "grafana" {
  count = var.create_grafana_workspace ? 1 : 0

  name               = "${var.name_prefix}-grafana"
  assume_role_policy = data.aws_iam_policy_document.grafana_assume[0].json

  tags = {
    Name = "${var.name_prefix}-grafana"
  }
}

data "aws_iam_policy_document" "grafana" {
  count = var.create_grafana_workspace ? 1 : 0

  statement {
    sid    = "QueryPrometheus"
    effect = "Allow"

    actions = [
      "aps:ListWorkspaces",
      "aps:DescribeWorkspace",
      "aps:QueryMetrics",
      "aps:GetLabels",
      "aps:GetSeries",
      "aps:GetMetricMetadata",
    ]

    resources = var.create_prometheus_workspace ? [aws_prometheus_workspace.this[0].arn] : ["*"]
  }

  statement {
    sid    = "QueryCloudWatch"
    effect = "Allow"

    actions = [
      "cloudwatch:DescribeAlarmsForMetric",
      "cloudwatch:DescribeAlarmHistory",
      "cloudwatch:DescribeAlarms",
      "cloudwatch:ListMetrics",
      "cloudwatch:GetMetricData",
      "cloudwatch:GetMetricStatistics",
      "logs:DescribeLogGroups",
      "logs:GetLogGroupFields",
      "logs:StartQuery",
      "logs:StopQuery",
      "logs:GetQueryResults",
      "tag:GetResources",
    ]

    # CloudWatch's query APIs are account-scoped and do not accept a resource
    # ARN. Read-only, and the data is metrics, not secrets.
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "grafana" {
  count = var.create_grafana_workspace ? 1 : 0

  name   = "${var.name_prefix}-grafana"
  role   = aws_iam_role.grafana[0].id
  policy = data.aws_iam_policy_document.grafana[0].json
}

resource "aws_grafana_workspace" "this" {
  count = var.create_grafana_workspace ? 1 : 0

  name                     = "${var.name_prefix}-grafana"
  account_access_type      = "CURRENT_ACCOUNT"
  authentication_providers = ["AWS_SSO"]
  permission_type          = "SERVICE_MANAGED"
  role_arn                 = aws_iam_role.grafana[0].arn

  data_sources = ["PROMETHEUS", "CLOUDWATCH"]

  configuration = jsonencode({
    plugins = {
      # Only reviewed plugins. A Grafana plugin runs with the dashboard's
      # datasource credentials.
      pluginAdminEnabled = false
    }
    unifiedAlerting = {
      # Alerting lives in AMP's rule groups, which are Terraform-managed and
      # reviewable. Grafana alerting would be a second, unreviewed alerting
      # system whose rules live in a UI.
      enabled = false
    }
  })

  tags = {
    Name = "${var.name_prefix}-grafana"
  }
}

resource "aws_grafana_role_association" "admin" {
  count = var.create_grafana_workspace && length(var.grafana_sso_admin_group_ids) > 0 ? 1 : 0

  role         = "ADMIN"
  group_ids    = var.grafana_sso_admin_group_ids
  workspace_id = aws_grafana_workspace.this[0].id
}

###############################################################################
# Alert routing
###############################################################################

resource "aws_sns_topic" "alerts" {
  name              = "${var.name_prefix}-alerts"
  kms_master_key_id = var.sns_kms_key_arn

  tags = {
    Name = "${var.name_prefix}-alerts"
  }
}

data "aws_iam_policy_document" "alerts_topic" {
  statement {
    sid    = "AllowCloudWatchAlarmsToPublish"
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["cloudwatch.amazonaws.com", "aps.amazonaws.com", "budgets.amazonaws.com"]
    }

    actions   = ["sns:Publish"]
    resources = [aws_sns_topic.alerts.arn]

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }

  statement {
    sid    = "DenyInsecureTransport"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions   = ["sns:Publish"]
    resources = [aws_sns_topic.alerts.arn]

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_sns_topic_policy" "alerts" {
  arn    = aws_sns_topic.alerts.arn
  policy = data.aws_iam_policy_document.alerts_topic.json
}

resource "aws_sns_topic_subscription" "alerts" {
  for_each = var.alert_topic_subscriptions

  topic_arn = aws_sns_topic.alerts.arn
  protocol  = each.key
  endpoint  = each.value

  # Raw delivery so PagerDuty receives the alarm JSON rather than an SNS
  # envelope wrapping a JSON string it then has to unwrap.
  raw_message_delivery = each.key == "https" ? true : false
}

###############################################################################
# Log groups for AWS-service logs
###############################################################################

resource "aws_cloudwatch_log_group" "this" {
  for_each = var.log_groups

  name              = each.key
  retention_in_days = each.value.retention_days
  kms_key_id        = var.kms_key_arn_logs

  tags = {
    Name = each.key
  }
}

###############################################################################
# Edge alarms
###############################################################################

resource "aws_cloudwatch_metric_alarm" "alb_5xx" {
  count = var.alb_arn_suffix != "" ? 1 : 0

  alarm_name          = "${var.name_prefix}-alb-5xx"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  threshold           = 10
  period              = 60
  statistic           = "Sum"
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HTTPCode_ELB_5XX_Count"
  treat_missing_data  = "notBreaching"

  alarm_description = "P1. The ALB itself is returning 5xx - not the application. Usually zero healthy targets or a listener/target-group misconfiguration."

  dimensions = {
    LoadBalancer = var.alb_arn_suffix
  }

  alarm_actions = [aws_sns_topic.alerts.arn]
  ok_actions    = [aws_sns_topic.alerts.arn]

  tags = {
    Name     = "${var.name_prefix}-alb-5xx"
    Severity = "P1"
  }
}

resource "aws_cloudwatch_metric_alarm" "alb_unhealthy_hosts" {
  count = var.alb_arn_suffix != "" ? 1 : 0

  alarm_name          = "${var.name_prefix}-alb-unhealthy-hosts"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  threshold           = 0
  period              = 60
  statistic           = "Maximum"
  namespace           = "AWS/ApplicationELB"
  metric_name         = "UnHealthyHostCount"
  treat_missing_data  = "notBreaching"

  alarm_description = "P2. At least one target is failing /healthz. Expected briefly during a rollout; sustained means a dependency is down."

  dimensions = {
    LoadBalancer = var.alb_arn_suffix
  }

  alarm_actions = [aws_sns_topic.alerts.arn]

  tags = {
    Name     = "${var.name_prefix}-alb-unhealthy-hosts"
    Severity = "P2"
  }
}

###############################################################################
# Budgets
#
# Cost is an SLI (deployment.md 2.7). These route to the platform team, because
# the team that can change the spend is the team that should hear about it.
###############################################################################

resource "aws_budgets_budget" "monthly" {
  name         = "${var.name_prefix}-monthly"
  budget_type  = "COST"
  limit_amount = tostring(var.budget_monthly_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  cost_filter {
    name   = "TagKeyValue"
    values = [format("user:Environment$%s", var.environment)]
  }

  dynamic "notification" {
    for_each = [80, 100, 120]

    content {
      comparison_operator = "GREATER_THAN"
      threshold           = notification.value
      threshold_type      = "PERCENTAGE"
      # FORECASTED at 80%, ACTUAL above: a forecast breach at 80% is early
      # enough to act on, and an actual breach at 100% is a fact worth paging on.
      notification_type = notification.value == 80 ? "FORECASTED" : "ACTUAL"

      subscriber_sns_topic_arns  = [aws_sns_topic.alerts.arn]
      subscriber_email_addresses = var.budget_notification_emails
    }
  }

  tags = {
    Name = "${var.name_prefix}-monthly"
  }
}
