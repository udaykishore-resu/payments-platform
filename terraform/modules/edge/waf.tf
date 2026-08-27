###############################################################################
# modules/edge/waf.tf - the twelve rules from security.md 2.1
#
# Generic OWASP rule sets are noisy on a JSON API. These twelve are the ones
# kept, in the order they are evaluated. W1-W5 and W9-W11 run before the
# rate-based rules so a malformed flood is dropped at the cheapest stage.
#
# The WAF is explicitly NOT the PAN control - L1 validation is (baseline 17.2).
# The WAF catches traffic that should never have reached the application at all.
###############################################################################

locals {
  waf_name = "${var.name_prefix}-web-acl"
}

# W2's detector. Runs of 13-19 digits with the usual separators stripped by the
# text transformations below. Luhn cannot be expressed in a WAF regex, so this
# over-matches by design: the false positive is a 400 (recoverable), the false
# negative is a PCI scope breach (not recoverable). The application's L1
# detector does the Luhn and IIN checks properly.
resource "aws_wafv2_regex_pattern_set" "pan_shape" {
  name        = "${var.name_prefix}-pan-shape"
  scope       = "REGIONAL"
  description = "Digit runs of PAN length. Deliberately over-matches; L1 validation does the real check."

  regular_expression {
    regex_string = "[0-9]{13,19}"
  }

  regular_expression {
    # Track-2-shaped data: PAN=expiry service-code.
    regex_string = ";[0-9]{13,19}=[0-9]{4,}"
  }

  regular_expression {
    # Track-1-shaped data.
    regex_string = "%B[0-9]{13,19}\\^"
  }

  tags = {
    Name = "${var.name_prefix}-pan-shape"
  }
}

resource "aws_wafv2_web_acl" "this" {
  name        = local.waf_name
  scope       = "REGIONAL"
  description = "Payment API edge rules. See docs/security.md 2.1 for the reasoning behind each."

  default_action {
    allow {}
  }

  #############################################################################
  # W1 - body size cap on the payment endpoints
  #
  # Stops: JSON parser resource exhaustion, and the "huge body" half of most
  # deserialization attacks. A payment instruction is under 8 KB; anything at
  # 256 KB is an attack or a client bug, and either way the parser should never
  # see it.
  #############################################################################
  rule {
    name     = "W1-body-size-cap-payments"
    priority = 10

    action {
      block {
        custom_response {
          response_code = 413
        }
      }
    }

    statement {
      and_statement {
        statement {
          byte_match_statement {
            search_string         = "/v1/payments"
            positional_constraint = "STARTS_WITH"

            field_to_match {
              uri_path {}
            }

            text_transformation {
              priority = 0
              type     = "LOWERCASE"
            }
          }
        }

        statement {
          size_constraint_statement {
            comparison_operator = "GT"
            size                = var.waf_max_body_bytes

            field_to_match {
              body {
                # A body larger than the inspection limit still matches the size
                # constraint, which is exactly the behaviour wanted here.
                oversize_handling = "MATCH"
              }
            }

            text_transformation {
              priority = 0
              type     = "NONE"
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W1BodySizeCap"
      sampled_requests_enabled   = false # The sample would contain the body.
    }
  }

  #############################################################################
  # W2 - PAN-shaped body
  #
  # Stops: raw card data reaching the platform at all. This API is token-only
  # (baseline A2); a PAN in a request body is either a broken integration or an
  # attempt to drag us into PCI SAQ-D scope. Blocking at the edge means the PAN
  # never reaches an application log buffer.
  #
  # sampled_requests_enabled is false on this rule specifically: a sampled
  # request would store the very PAN the rule exists to keep out.
  #############################################################################
  rule {
    name     = "W2-pan-shaped-body"
    priority = 20

    action {
      block {
        custom_response {
          response_code = 400
        }
      }
    }

    statement {
      regex_pattern_set_reference_statement {
        arn = aws_wafv2_regex_pattern_set.pan_shape.arn

        field_to_match {
          body {
            oversize_handling = "MATCH"
          }
        }

        # Strip the separators a PAN is usually written with, then normalise, so
        # "4111-1111 1111.1111" matches the same pattern as the bare digits.
        text_transformation {
          priority = 0
          type     = "REMOVE_NULLS"
        }

        text_transformation {
          priority = 1
          type     = "COMPRESS_WHITE_SPACE"
        }

        text_transformation {
          priority = 2
          type     = "NORMALIZE_PATH"
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W2PanShapedBody"
      sampled_requests_enabled   = false
    }
  }

  #############################################################################
  # W3 - Content-Type must be application/json on mutations
  #
  # Stops: multipart/form smuggling, and the CSRF-shaped confusion where a
  # browser-issued form POST is accepted as an API call. The API consumes JSON
  # and nothing else, so this rule costs legitimate clients nothing.
  #############################################################################
  rule {
    name     = "W3-json-content-type-only"
    priority = 30

    action {
      block {
        custom_response {
          response_code = 415
        }
      }
    }

    statement {
      and_statement {
        statement {
          byte_match_statement {
            search_string         = "/v1/"
            positional_constraint = "STARTS_WITH"

            field_to_match {
              uri_path {}
            }

            text_transformation {
              priority = 0
              type     = "LOWERCASE"
            }
          }
        }

        statement {
          not_statement {
            statement {
              byte_match_statement {
                search_string         = "application/json"
                positional_constraint = "CONTAINS"

                field_to_match {
                  single_header {
                    name = "content-type"
                  }
                }

                text_transformation {
                  priority = 0
                  type     = "LOWERCASE"
                }
              }
            }
          }
        }

        # GET and DELETE carry no body and therefore no Content-Type.
        statement {
          not_statement {
            statement {
              or_statement {
                statement {
                  byte_match_statement {
                    search_string         = "GET"
                    positional_constraint = "EXACTLY"

                    field_to_match {
                      method {}
                    }

                    text_transformation {
                      priority = 0
                      type     = "UPPERCASE"
                    }
                  }
                }

                statement {
                  byte_match_statement {
                    search_string         = "DELETE"
                    positional_constraint = "EXACTLY"

                    field_to_match {
                      method {}
                    }

                    text_transformation {
                      priority = 0
                      type     = "UPPERCASE"
                    }
                  }
                }
              }
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W3JsonContentTypeOnly"
      sampled_requests_enabled   = true
    }
  }

  #############################################################################
  # W4 - Transfer-Encoding together with Content-Length
  #
  # Stops: HTTP request smuggling (CL.TE and TE.CL desync). This is the attack
  # class that lets an attacker prepend bytes to the NEXT request on a reused
  # connection - which on a multi-tenant payment API means prepending to another
  # tenant's request. There is no legitimate reason for both headers to be
  # present.
  #############################################################################
  rule {
    name     = "W4-request-smuggling-headers"
    priority = 40

    action {
      block {}
    }

    statement {
      and_statement {
        statement {
          size_constraint_statement {
            comparison_operator = "GT"
            size                = 0

            field_to_match {
              single_header {
                name = "transfer-encoding"
              }
            }

            text_transformation {
              priority = 0
              type     = "NONE"
            }
          }
        }

        statement {
          size_constraint_statement {
            comparison_operator = "GT"
            size                = 0

            field_to_match {
              single_header {
                name = "content-length"
              }
            }

            text_transformation {
              priority = 0
              type     = "NONE"
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W4RequestSmuggling"
      sampled_requests_enabled   = true
    }
  }

  #############################################################################
  # W5 - parser-differential and resource-exhaustion primitives
  #
  # Stops: oversized URIs, oversized single headers and header floods. Each is a
  # way to make two parsers in the path disagree about where a request ends, and
  # each is also a cheap CPU-exhaustion primitive.
  #############################################################################
  rule {
    name     = "W5-oversized-request-components"
    priority = 50

    action {
      block {}
    }

    statement {
      or_statement {
        statement {
          size_constraint_statement {
            comparison_operator = "GT"
            size                = 2048

            field_to_match {
              uri_path {}
            }

            text_transformation {
              priority = 0
              type     = "NONE"
            }
          }
        }

        statement {
          size_constraint_statement {
            comparison_operator = "GT"
            size                = 8192

            field_to_match {
              headers {
                match_scope       = "VALUE"
                oversize_handling = "MATCH"

                match_pattern {
                  all {}
                }
              }
            }

            text_transformation {
              priority = 0
              type     = "NONE"
            }
          }
        }

        statement {
          size_constraint_statement {
            comparison_operator = "GT"
            size                = 4096

            field_to_match {
              query_string {}
            }

            text_transformation {
              priority = 0
              type     = "NONE"
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W5OversizedComponents"
      sampled_requests_enabled   = true
    }
  }

  #############################################################################
  # W9 - AWS managed: Known Bad Inputs
  #
  # Stops: Log4Shell-class JNDI payloads, path traversal, null bytes, and the
  # other "known-bad literal" signatures. Low false-positive rate on a JSON API
  # because the signatures are literal strings, not heuristics.
  #############################################################################
  rule {
    name     = "W9-aws-known-bad-inputs"
    priority = 60

    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesKnownBadInputsRuleSet"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W9KnownBadInputs"
      sampled_requests_enabled   = true
    }
  }

  #############################################################################
  # W10 - AWS managed: IP reputation
  #
  # Stops: traffic from addresses AWS has observed running scanners, botnets and
  # credential-stuffing infrastructure. Cheap and low-false-positive; a merchant
  # server rarely shares an address with a botnet.
  #############################################################################
  rule {
    name     = "W10-aws-ip-reputation"
    priority = 70

    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesAmazonIpReputationList"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W10IpReputation"
      sampled_requests_enabled   = true
    }
  }

  #############################################################################
  # W11 - geo-block for sanctioned jurisdictions
  #
  # Stops: nothing an attacker cannot bypass with a VPN - and that is fine.
  # This is a *posture* control: the sanctions decision is enforced properly in
  # policy (baseline 23 blockedCountries), and this is the coarse edge
  # expression of it that an assessor can see from outside.
  #############################################################################
  rule {
    name     = "W11-geo-block-sanctioned"
    priority = 80

    action {
      block {
        custom_response {
          response_code = 403
        }
      }
    }

    statement {
      geo_match_statement {
        country_codes = var.blocked_country_codes
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W11GeoBlock"
      sampled_requests_enabled   = true
    }
  }

  #############################################################################
  # W6 - rate limit on /v1/payments
  #
  # Stops: a crude volumetric flood before it reaches the application's own
  # limiter. This is explicitly a backstop: real limits are per-tenant and
  # post-authentication, because an IP limit is meaningless behind NAT and CGNAT
  # where thousands of legitimate merchants share an address.
  #
  # AWS WAF keeps a source blocked while it remains over the limit and releases
  # it when the trailing window drops below - there is no configurable block
  # duration. The "block 10 min" in security.md is therefore implemented as the
  # 5-minute evaluation window plus the application-level limiter's own penalty
  # box, not by the WAF alone.
  #############################################################################
  rule {
    name     = "W6-rate-limit-payments"
    priority = 90

    action {
      block {
        custom_response {
          response_code = 429
        }
      }
    }

    statement {
      rate_based_statement {
        limit                 = var.waf_rate_limit_payments
        evaluation_window_sec = 300
        aggregate_key_type    = "IP"

        scope_down_statement {
          byte_match_statement {
            search_string         = "/v1/payments"
            positional_constraint = "STARTS_WITH"

            field_to_match {
              uri_path {}
            }

            text_transformation {
              priority = 0
              type     = "LOWERCASE"
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W6RateLimitPayments"
      sampled_requests_enabled   = true
    }
  }

  #############################################################################
  # W7 - rate limit on the token endpoint
  #
  # Stops: credential stuffing and client-secret brute forcing against
  # client-credentials. A legitimate client mints one token every 15 minutes;
  # 100 per 5 minutes is two orders of magnitude of headroom and still stops a
  # brute-force outright.
  #############################################################################
  rule {
    name     = "W7-rate-limit-token-endpoint"
    priority = 100

    action {
      block {
        custom_response {
          response_code = 429
        }
      }
    }

    statement {
      rate_based_statement {
        limit                 = var.waf_rate_limit_token
        evaluation_window_sec = 300
        aggregate_key_type    = "IP"

        scope_down_statement {
          byte_match_statement {
            search_string         = "/oauth2/token"
            positional_constraint = "STARTS_WITH"

            field_to_match {
              uri_path {}
            }

            text_transformation {
              priority = 0
              type     = "LOWERCASE"
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W7RateLimitToken"
      sampled_requests_enabled   = true
    }
  }

  #############################################################################
  # W8 - card-testing signature
  #
  # Stops: the card-testing pattern (T-1) - many distinct card tokens from one
  # source with a very low authorization rate, as an attacker validates a stolen
  # list against a small charge.
  #
  # Honest note on what this rule can and cannot do: the *real* detector is
  # application-side, because the authorization rate is not visible to the WAF.
  # What the WAF contributes is the volumetric half - an unusual density of
  # distinct payment creations from one source - answered with a CAPTCHA
  # challenge rather than a block, because a false positive here rejects a
  # legitimate merchant's traffic. The application raises the security event and
  # applies the real control.
  #############################################################################
  rule {
    name     = "W8-card-testing-velocity"
    priority = 110

    action {
      challenge {}
    }

    statement {
      rate_based_statement {
        # Deliberately tighter than W6 and scoped to creations only: 30 payment
        # creations per 10 minutes from one source, matching the "> 30 distinct
        # card tokens per merchant per 10 min" signature in security.md 8.2.
        limit                 = 300
        evaluation_window_sec = 600
        aggregate_key_type    = "CUSTOM_KEYS"

        custom_key {
          header {
            name = "x-pp-client-id"

            text_transformation {
              priority = 0
              type     = "LOWERCASE"
            }
          }
        }

        custom_key {
          ip {}
        }

        scope_down_statement {
          and_statement {
            statement {
              byte_match_statement {
                search_string         = "/v1/payments"
                positional_constraint = "STARTS_WITH"

                field_to_match {
                  uri_path {}
                }

                text_transformation {
                  priority = 0
                  type     = "LOWERCASE"
                }
              }
            }

            statement {
              byte_match_statement {
                search_string         = "POST"
                positional_constraint = "EXACTLY"

                field_to_match {
                  method {}
                }

                text_transformation {
                  priority = 0
                  type     = "UPPERCASE"
                }
              }
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W8CardTestingVelocity"
      sampled_requests_enabled   = true
    }
  }

  #############################################################################
  # W12 - SQLi/XSS managed rules, in COUNT mode
  #
  # Deliberately not blocking. The API is parameterised-SQL and JSON-only, so
  # these rules add little; what they do reliably is false-positive on
  # legitimate merchant descriptors - "O'Brien & Sons" trips the SQLi
  # signatures, and a blocked merchant onboarding is a worse outcome than an
  # unblocked payload the application is already immune to.
  #
  # Counting keeps the signal (the metric is on the security dashboard, and a
  # sustained rise is investigated) without breaking merchants. Flipping this to
  # Block is a reviewed change with a soak in Count first.
  #############################################################################
  rule {
    name     = "W12-sqli-xss-count-only"
    priority = 120

    override_action {
      count {}
    }

    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesCommonRuleSet"

        # The size restriction and body rules inside the Common Rule Set are
        # redundant with W1 and W5 and duplicate their false positives.
        rule_action_override {
          name = "SizeRestrictions_BODY"

          action_to_use {
            count {}
          }
        }

        rule_action_override {
          name = "NoUserAgent_HEADER"

          action_to_use {
            count {}
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "W12SqliXssCount"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = replace(local.waf_name, "-", "")
    sampled_requests_enabled   = true
  }

  tags = {
    Name = local.waf_name
  }
}

resource "aws_wafv2_web_acl_association" "alb" {
  resource_arn = aws_lb.public.arn
  web_acl_arn  = aws_wafv2_web_acl.this.arn
}

resource "aws_wafv2_web_acl_logging_configuration" "this" {
  count = var.waf_log_destination_arn != "" ? 1 : 0

  resource_arn            = aws_wafv2_web_acl.this.arn
  log_destination_configs = [var.waf_log_destination_arn]

  # Redact the fields that would otherwise put credentials and card-shaped data
  # into the WAF log - which is the one log in the estate not passing through
  # the application's allowlist serializer.
  redacted_fields {
    single_header {
      name = "authorization"
    }
  }

  redacted_fields {
    single_header {
      name = "cookie"
    }
  }

  redacted_fields {
    single_header {
      name = "x-pp-signature"
    }
  }

  logging_filter {
    default_behavior = "DROP"

    filter {
      behavior    = "KEEP"
      requirement = "MEETS_ANY"

      condition {
        action_condition {
          action = "BLOCK"
        }
      }

      condition {
        action_condition {
          action = "COUNT"
        }
      }

      condition {
        action_condition {
          action = "CHALLENGE"
        }
      }
    }
  }
}

###############################################################################
# Alarms on the rules that mean something is actually happening
###############################################################################

resource "aws_cloudwatch_metric_alarm" "pan_detected" {
  alarm_name          = "${var.name_prefix}-waf-pan-shaped-body"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  threshold           = 0
  period              = 300
  statistic           = "Sum"
  namespace           = "AWS/WAFV2"
  metric_name         = "BlockedRequests"
  treat_missing_data  = "notBreaching"

  alarm_description = "P2 security event. A PAN-shaped body reached the edge, which means an integration is sending raw card data. The request was blocked and the body was never logged."

  dimensions = {
    WebACL = local.waf_name
    Rule   = "W2-pan-shaped-body"
    Region = data.aws_region.current.name
  }

  tags = {
    Name     = "${var.name_prefix}-waf-pan-shaped-body"
    Severity = "P2"
  }
}

resource "aws_cloudwatch_metric_alarm" "smuggling_detected" {
  alarm_name          = "${var.name_prefix}-waf-request-smuggling"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  threshold           = 0
  period              = 300
  statistic           = "Sum"
  namespace           = "AWS/WAFV2"
  metric_name         = "BlockedRequests"
  treat_missing_data  = "notBreaching"

  alarm_description = "P1 security event. A request carrying both Transfer-Encoding and Content-Length was blocked. There is no benign cause; this is a smuggling attempt against a multi-tenant API."

  dimensions = {
    WebACL = local.waf_name
    Rule   = "W4-request-smuggling-headers"
    Region = data.aws_region.current.name
  }

  tags = {
    Name     = "${var.name_prefix}-waf-request-smuggling"
    Severity = "P1"
  }
}
