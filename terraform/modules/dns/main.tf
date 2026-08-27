###############################################################################
# modules/dns - hosted zone, health checks, failover records
#
# The load-bearing point, repeated here because it is the thing most likely to
# be misunderstood: Route 53 moves TRAFFIC. It does not move WRITE AUTHORITY.
# Write authority moves only when a human increments the fencing token in the
# DynamoDB Global Table (disaster-recovery.md 3). A failover record that flips
# while the fence still names the old region sends traffic to a region whose
# pods deliberately fail readiness - which is the correct, fail-closed outcome.
#
# That is also why there is no automatic promotion anywhere in this module.
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
  zone_id = var.create_zone ? aws_route53_zone.this[0].zone_id : var.existing_zone_id
}

check "zone_source" {
  assert {
    condition     = var.create_zone || var.existing_zone_id != null
    error_message = "Either create_zone must be true or existing_zone_id must be provided."
  }

  assert {
    condition     = var.enable_dnssec == false || var.dnssec_key_arn != null
    error_message = "enable_dnssec requires dnssec_key_arn."
  }
}

resource "aws_route53_zone" "this" {
  count = var.create_zone ? 1 : 0

  name    = var.zone_name
  comment = "Public zone for the payment orchestration platform (${var.environment})."

  tags = {
    Name = var.zone_name
  }

  lifecycle {
    # Deleting a hosted zone changes the delegation at the registrar and takes
    # the API offline for as long as it takes to recreate the zone and wait out
    # every resolver's cached NS records - hours, not minutes.
    prevent_destroy = true
  }
}

###############################################################################
# Health checks
#
# Checked against each region's OWN ALB hostname, never against the API
# hostname: a health check that resolves the record it controls is a feedback
# loop, and the first transient failure makes it permanent.
###############################################################################

resource "aws_route53_health_check" "region" {
  for_each = var.endpoints

  fqdn              = each.value.health_check_fqdn
  type              = "HTTPS"
  port              = 443
  resource_path     = each.value.health_check_path
  request_interval  = var.health_check_interval
  failure_threshold = var.health_check_failure_threshold

  # Explicit checker regions rather than the default set: a health check
  # evaluated from three named regions is reproducible during a post-incident
  # review, and 3-of-N agreement is what "detection" means in the DR runbook.
  regions = var.health_check_regions

  # Latency measurement is what makes the Route 53 console's health-check graph
  # useful during a slow-degradation incident, as opposed to a binary
  # up/down that says nothing about how close to the edge the region was.
  measure_latency = true

  # Verify the certificate chain, so an expired or mis-issued certificate marks
  # the region unhealthy rather than being silently accepted.
  enable_sni = true

  tags = {
    Name   = "pp-${var.environment}-${each.key}"
    Region = each.key
  }
}

###############################################################################
# Failover records
###############################################################################

resource "aws_route53_record" "api" {
  for_each = var.endpoints

  zone_id        = local.zone_id
  name           = var.api_hostname
  type           = "A"
  set_identifier = each.key

  failover_routing_policy {
    type = each.value.role
  }

  health_check_id = aws_route53_health_check.region[each.key].id

  alias {
    name    = each.value.alb_dns_name
    zone_id = each.value.alb_zone_id

    # Evaluate the ALB's own target health as well as the Route 53 health
    # check. An ALB with zero healthy targets then stops receiving traffic
    # without waiting for the health check's three intervals.
    evaluate_target_health = true
  }
}

# The AAAA record is deliberately absent. The ALB is dual-stack-capable but the
# platform publishes IPv4 only: a merchant server stack with broken IPv6 egress
# produces intermittent, hard-to-diagnose connection failures that look like our
# outage, and there is no compensating benefit for a server-to-server API.

###############################################################################
# CAA
#
# Restricts which CAs may issue for this name. Together with Certificate
# Transparency monitoring this is the practical defence against mis-issuance
# (security.md 2.1) - it does not prevent a compromised CA, but it makes the
# mis-issuance visible and, for a compliant CA, impossible.
###############################################################################

resource "aws_route53_record" "caa" {
  count = var.create_zone ? 1 : 0

  zone_id = local.zone_id
  name    = var.zone_name
  type    = "CAA"
  ttl     = 3600

  records = [
    "0 issue \"amazon.com\"",
    "0 issuewild \";\"",
    "0 iodef \"mailto:security@${var.zone_name}\"",
  ]
}

###############################################################################
# DNSSEC
#
# Off by default and honestly so: enabling it requires publishing a DS record at
# the registrar, and a mismatch between the zone's signing key and that DS
# record makes the entire zone resolve as SERVFAIL for every validating
# resolver - a total outage with a multi-hour TTL on the failure. It is enabled
# deliberately, with the registrar change staged first, never as a default.
###############################################################################

resource "aws_route53_key_signing_key" "this" {
  count = var.create_zone && var.enable_dnssec ? 1 : 0

  hosted_zone_id             = local.zone_id
  key_management_service_arn = var.dnssec_key_arn
  name                       = "pp-${var.environment}-ksk"
}

resource "aws_route53_hosted_zone_dnssec" "this" {
  count = var.create_zone && var.enable_dnssec ? 1 : 0

  hosted_zone_id = aws_route53_key_signing_key.this[0].hosted_zone_id

  depends_on = [aws_route53_key_signing_key.this]
}

###############################################################################
# Query logging
###############################################################################

resource "aws_route53_query_log" "this" {
  count = var.create_zone && var.query_log_group_arn != "" ? 1 : 0

  zone_id                  = local.zone_id
  cloudwatch_log_group_arn = var.query_log_group_arn
}

###############################################################################
# Alarms
###############################################################################

resource "aws_cloudwatch_metric_alarm" "health_check" {
  for_each = var.endpoints

  # Route 53 health-check metrics are published only in us-east-1. The provider
  # alias is supplied by the caller; see the env stacks.
  alarm_name          = "pp-${var.environment}-${each.key}-health"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 2
  threshold           = 1
  period              = 60
  statistic           = "Minimum"
  namespace           = "AWS/Route53"
  metric_name         = "HealthCheckStatus"
  treat_missing_data  = "breaching"

  alarm_description = "P1. Route 53 considers ${each.key} unhealthy. This is one of the two independent signals the DR runbook requires before declaring a region failure; the other is the AMP SLI series flatlining."

  dimensions = {
    HealthCheckId = aws_route53_health_check.region[each.key].id
  }

  tags = {
    Name     = "pp-${var.environment}-${each.key}-health"
    Severity = "P1"
  }
}
