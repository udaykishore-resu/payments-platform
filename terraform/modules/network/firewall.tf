###############################################################################
# modules/network/firewall.tf - egress domain allowlist
#
# What this implements
# -------------------
# AWS Network Firewall in the egress path, with a *stateful* rule group that
# permits TLS only to an enumerated set of SNI hostnames and drops everything
# else, plus a stateless group that drops non-443 egress before it reaches the
# stateful engine.
#
# What it does NOT do, stated honestly
# ------------------------------------
# 1. SNI is client-asserted. A compromised process that speaks TLS to an
#    allowlisted SNI while actually resolving elsewhere, or that tunnels over
#    an allowlisted host, is not stopped here. Network Firewall raises the cost
#    of exfiltration; it does not make it impossible.
# 2. It is not a substitute for the in-cluster egress proxy described in
#    security.md 2.2. That proxy enforces the allowlist *per service account*,
#    which the firewall cannot do because at this layer the source is a pod IP.
#    The two are complementary: the proxy answers "which workload", the firewall
#    answers "which destination", and the firewall keeps working if the proxy is
#    bypassed. The proxy is a Kubernetes workload and is therefore ArgoCD's, not
#    Terraform's (see terraform/README.md, "Not managed here").
# 3. It costs roughly USD 300/month per firewall endpoint plus per-GB
#    processing. dev runs without it (enable_network_firewall = false) and
#    relies on the proxy alone. That is a deliberate, priced trade.
###############################################################################

locals {
  # sync_states carries one entry per AZ with the endpoint ENI to route through.
  firewall_endpoint_ids = local.firewall_enabled ? [
    for az in var.availability_zones : one([
      for s in tolist(aws_networkfirewall_firewall.this[0].firewall_status[0].sync_states) :
      s.attachment[0].endpoint_id
      if s.availability_zone == az
    ])
  ] : []
}

resource "aws_networkfirewall_rule_group" "stateless_drop_non_tls" {
  count = local.firewall_enabled ? 1 : 0

  name        = "${local.name}-egress-stateless"
  type        = "STATELESS"
  capacity    = 100
  description = "Forward 443 to the stateful engine; drop every other outbound port at the cheapest possible stage."

  rule_group {
    rules_source {
      stateless_rules_and_custom_actions {
        stateless_rule {
          priority = 10

          rule_definition {
            actions = ["aws:forward_to_sfe"]

            match_attributes {
              protocols = [6] # TCP

              source {
                address_definition = var.vpc_cidr
              }

              destination {
                address_definition = "0.0.0.0/0"
              }

              destination_port {
                from_port = 443
                to_port   = 443
              }
            }
          }
        }

        stateless_rule {
          priority = 20

          rule_definition {
            actions = ["aws:drop"]

            match_attributes {
              source {
                address_definition = "0.0.0.0/0"
              }

              destination {
                address_definition = "0.0.0.0/0"
              }
            }
          }
        }
      }
    }
  }

  tags = {
    Name = "${local.name}-egress-stateless"
  }
}

resource "aws_networkfirewall_rule_group" "stateful_domain_allowlist" {
  count = local.firewall_enabled ? 1 : 0

  name        = "${local.name}-egress-allowlist"
  type        = "STATEFUL"
  capacity    = 200
  description = "Allow TLS only to the enumerated gateway/vendor/IdP hosts (security.md 2.2). Everything else is denied."

  rule_group {
    rule_variables {
      ip_sets {
        key = "HOME_NET"

        ip_set {
          definition = var.egress_subnet_cidrs
        }
      }
    }

    rules_source {
      rules_source_list {
        generated_rules_type = "ALLOWLIST"
        target_types         = ["TLS_SNI", "HTTP_HOST"]
        targets              = var.egress_allowlist_domains
      }
    }

    stateful_rule_options {
      # Strict order: the allowlist is evaluated as written and the policy's
      # default action (drop) applies to anything that falls through. Under the
      # default "action order" the semantics of an allowlist plus a drop-all are
      # ambiguous, which is exactly the kind of ambiguity an egress control must
      # not have.
      rule_order = "STRICT_ORDER"
    }
  }

  tags = {
    Name = "${local.name}-egress-allowlist"
  }
}

resource "aws_networkfirewall_firewall_policy" "this" {
  count = local.firewall_enabled ? 1 : 0

  name        = "${local.name}-egress-policy"
  description = "Default-deny egress policy for the ${var.environment} VPC."

  firewall_policy {
    stateless_default_actions          = ["aws:forward_to_sfe"]
    stateless_fragment_default_actions = ["aws:drop"]

    stateless_rule_group_reference {
      priority     = 10
      resource_arn = aws_networkfirewall_rule_group.stateless_drop_non_tls[0].arn
    }

    stateful_engine_options {
      rule_order = "STRICT_ORDER"
    }

    stateful_default_actions = [
      "aws:drop_strict",
      "aws:alert_strict",
    ]

    stateful_rule_group_reference {
      priority     = 10
      resource_arn = aws_networkfirewall_rule_group.stateful_domain_allowlist[0].arn
    }
  }

  tags = {
    Name = "${local.name}-egress-policy"
  }
}

resource "aws_networkfirewall_firewall" "this" {
  count = local.firewall_enabled ? 1 : 0

  name                = "${local.name}-egress-fw"
  firewall_policy_arn = aws_networkfirewall_firewall_policy.this[0].arn
  vpc_id              = aws_vpc.this.id

  # Protects against an `apply` that removes the firewall and silently opens
  # general egress. Removing it is a two-step change: flip this to false in one
  # reviewed commit, then remove the resource in another.
  delete_protection                 = var.environment == "prod"
  firewall_policy_change_protection = var.environment == "prod"
  subnet_change_protection          = var.environment == "prod"

  dynamic "subnet_mapping" {
    for_each = aws_subnet.firewall

    content {
      subnet_id = subnet_mapping.value.id
    }
  }

  tags = {
    Name = "${local.name}-egress-fw"
  }
}

resource "aws_cloudwatch_log_group" "firewall_alert" {
  count = local.firewall_enabled ? 1 : 0

  name              = "/aws/network-firewall/${local.name}/alert"
  retention_in_days = var.dns_query_log_retention_days
  kms_key_id        = var.dns_query_log_kms_key_arn

  tags = {
    Name = "${local.name}-fw-alert"
  }
}

resource "aws_cloudwatch_log_group" "firewall_flow" {
  count = local.firewall_enabled ? 1 : 0

  name              = "/aws/network-firewall/${local.name}/flow"
  retention_in_days = var.dns_query_log_retention_days
  kms_key_id        = var.dns_query_log_kms_key_arn

  tags = {
    Name = "${local.name}-fw-flow"
  }
}

resource "aws_networkfirewall_logging_configuration" "this" {
  count = local.firewall_enabled ? 1 : 0

  firewall_arn = aws_networkfirewall_firewall.this[0].arn

  logging_configuration {
    log_destination_config {
      log_type             = "ALERT"
      log_destination_type = "CloudWatchLogs"

      log_destination = {
        logGroup = aws_cloudwatch_log_group.firewall_alert[0].name
      }
    }

    log_destination_config {
      log_type             = "FLOW"
      log_destination_type = "CloudWatchLogs"

      log_destination = {
        logGroup = aws_cloudwatch_log_group.firewall_flow[0].name
      }
    }
  }
}
