###############################################################################
# modules/edge - ACM, ALB, internal NLB, Shield Advanced protections
#
# API Gateway is deliberately absent from the money path (deployment.md 2.5): it
# adds 10-20 ms, imposes a 29 s hard integration timeout that conflicts with an
# 8 s gateway budget plus retries, and its throttling and usage-plan features
# duplicate - less expressively - what the request pipeline already does
# per-tenant.
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

check "prod_edge_posture" {
  assert {
    condition     = var.environment != "prod" || var.waf_log_destination_arn != ""
    error_message = "WAF logging must be configured in prod: an unlogged block is an incident nobody can investigate."
  }

  assert {
    condition     = var.environment != "prod" || var.deletion_protection
    error_message = "ELB deletion protection is required in prod."
  }
}

###############################################################################
# Certificate
#
# Dual RSA-2048 + ECDSA P-256 is described in security.md 2.1; ACM issues one
# certificate per request, so the module issues the RSA certificate here and the
# ECDSA one is attached as an additional listener certificate. Both are
# DNS-validated, CT-logged, and renewed automatically by ACM.
###############################################################################

resource "aws_acm_certificate" "rsa" {
  domain_name               = var.domain_name
  subject_alternative_names = var.subject_alternative_names
  validation_method         = "DNS"
  key_algorithm             = "RSA_2048"

  tags = {
    Name = "${var.name_prefix}-rsa"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_acm_certificate" "ecdsa" {
  domain_name               = var.domain_name
  subject_alternative_names = var.subject_alternative_names
  validation_method         = "DNS"
  key_algorithm             = "EC_prime256v1"

  tags = {
    Name = "${var.name_prefix}-ecdsa"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "validation" {
  for_each = {
    for dvo in aws_acm_certificate.rsa.domain_validation_options :
    "${dvo.domain_name}${var.validation_record_suffix}" => {
      name   = dvo.resource_record_name
      record = dvo.resource_record_value
      type   = dvo.resource_record_type
    }
  }

  zone_id         = var.route53_zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "rsa" {
  certificate_arn         = aws_acm_certificate.rsa.arn
  validation_record_fqdns = [for r in aws_route53_record.validation : r.fqdn]
}

###############################################################################
# Public ALB
###############################################################################

resource "aws_lb" "public" {
  name               = "${var.name_prefix}-alb"
  load_balancer_type = "application"
  internal           = false
  subnets            = var.public_subnet_ids
  security_groups    = var.alb_security_group_ids

  idle_timeout                     = var.idle_timeout_seconds
  enable_deletion_protection       = var.deletion_protection
  enable_http2                     = true
  enable_cross_zone_load_balancing = true

  # Reject requests whose headers the ALB cannot parse unambiguously, rather
  # than normalising and forwarding them. This is the ALB-side half of the
  # request-smuggling defence that W4 covers at the WAF.
  drop_invalid_header_fields = true

  # Route 53 health checks and merchants both resolve the same name; without
  # this the ALB answers with an AWS-generated 4xx that carries no request ID.
  desync_mitigation_mode = "strictest"

  access_logs {
    bucket  = var.access_logs_bucket
    prefix  = var.access_logs_prefix
    enabled = true
  }

  connection_logs {
    bucket  = var.access_logs_bucket
    prefix  = "${var.access_logs_prefix}-connection"
    enabled = true
  }

  tags = {
    Name = "${var.name_prefix}-alb"
  }
}

resource "aws_lb_target_group" "api" {
  name        = "${var.name_prefix}-api"
  port        = 8443
  protocol    = "HTTPS"
  target_type = "ip" # Pods have real VPC IPs; targeting instances would add a kube-proxy hop.
  vpc_id      = var.vpc_id

  # 10 s, matching the preStop sleep budget in deployment.md 1.8: the pod keeps
  # serving for 15 s after SIGTERM, so a 10 s deregistration delay drains
  # cleanly with headroom.
  deregistration_delay = 10

  health_check {
    enabled = true
    # The deep composite check, not /readyz: the ALB should stop sending to a
    # target whose dependencies are unhealthy, which is exactly what /healthz
    # reports (deployment.md 1.7).
    path                = "/healthz"
    port                = "traffic-port"
    protocol            = "HTTPS"
    interval            = 10
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
    matcher             = "200"
  }

  stickiness {
    enabled = false # A payment API must never be sticky; every request is independent.
    type    = "lb_cookie"
  }

  tags = {
    Name = "${var.name_prefix}-api"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.public.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = var.ssl_policy
  certificate_arn   = aws_acm_certificate_validation.rsa.certificate_arn

  default_action {
    type = "fixed-response"

    fixed_response {
      content_type = "application/problem+json"
      status_code  = "404"
      # RFC 9457, matching the platform's error model (baseline 20). A default
      # action that returns an ALB-generated HTML page is a different error
      # format on the same hostname, which breaks client SDKs.
      message_body = "{\"type\":\"about:blank\",\"title\":\"Not Found\",\"status\":404}"
    }
  }

  tags = {
    Name = "${var.name_prefix}-https"
  }
}

resource "aws_lb_listener_certificate" "ecdsa" {
  listener_arn    = aws_lb_listener.https.arn
  certificate_arn = aws_acm_certificate.ecdsa.arn
}

resource "aws_lb_listener" "http_redirect" {
  load_balancer_arn = aws_lb.public.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "redirect"

    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }

  tags = {
    Name = "${var.name_prefix}-http-redirect"
  }
}

# Forward to the API target group only when the CloudFront origin-verify header
# is present. Without the header the ALB is a dead end even to somebody who has
# discovered its DNS name, which is what makes "the ALB is not directly
# resolvable" (security.md 2.1) enforceable rather than aspirational.
data "aws_secretsmanager_secret_version" "origin_verify" {
  count = var.cloudfront_origin_secret_arn != null ? 1 : 0

  secret_id = var.cloudfront_origin_secret_arn
}

resource "aws_lb_listener_rule" "api_with_origin_verify" {
  count = var.cloudfront_origin_secret_arn != null ? 1 : 0

  listener_arn = aws_lb_listener.https.arn
  priority     = 100

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }

  condition {
    path_pattern {
      values = ["/v1/*", "/oauth2/*"]
    }
  }

  condition {
    http_header {
      http_header_name = var.cloudfront_origin_secret_header_name
      values           = [data.aws_secretsmanager_secret_version.origin_verify[0].secret_string]
    }
  }

  tags = {
    Name = "${var.name_prefix}-api-origin-verified"
  }
}

resource "aws_lb_listener_rule" "api_direct" {
  count = var.cloudfront_origin_secret_arn == null ? 1 : 0

  listener_arn = aws_lb_listener.https.arn
  priority     = 100

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }

  condition {
    path_pattern {
      values = ["/v1/*", "/oauth2/*"]
    }
  }

  tags = {
    Name = "${var.name_prefix}-api-direct"
  }
}

# /healthz and /readyz are not exposed at the edge at all (security.md 2.1).
# They are reachable only from inside the VPC, which is where the Route 53
# health checker's HTTPS check terminates - against the region's own ALB DNS
# name, not the public API hostname.
resource "aws_lb_listener_rule" "block_health_endpoints" {
  listener_arn = aws_lb_listener.https.arn
  priority     = 10

  action {
    type = "fixed-response"

    fixed_response {
      content_type = "application/problem+json"
      status_code  = "404"
      message_body = "{\"type\":\"about:blank\",\"title\":\"Not Found\",\"status\":404}"
    }
  }

  condition {
    path_pattern {
      values = ["/healthz", "/readyz", "/livez", "/metrics"]
    }
  }

  tags = {
    Name = "${var.name_prefix}-block-health"
  }
}

###############################################################################
# Internal NLB
#
# Layer 4, so a cross-cluster gRPC stream is not re-framed and the client
# connection is preserved. Mostly used by the mesh ingress gateway: in-cluster
# gRPC goes pod-to-pod through the CNI with mTLS and never touches this.
###############################################################################

resource "aws_lb" "internal" {
  count = var.enable_nlb ? 1 : 0

  name               = "${var.name_prefix}-nlb"
  load_balancer_type = "network"
  internal           = true
  subnets            = var.private_subnet_ids

  enable_deletion_protection       = var.deletion_protection
  enable_cross_zone_load_balancing = true

  access_logs {
    bucket  = var.access_logs_bucket
    prefix  = "${var.access_logs_prefix}-nlb"
    enabled = true
  }

  tags = {
    Name = "${var.name_prefix}-nlb"
  }
}

resource "aws_lb_target_group" "grpc" {
  count = var.enable_nlb ? 1 : 0

  name        = "${var.name_prefix}-grpc"
  port        = 9443
  protocol    = "TCP"
  target_type = "ip"
  vpc_id      = var.vpc_id

  deregistration_delay = 10
  preserve_client_ip   = true

  health_check {
    enabled             = true
    protocol            = "TCP"
    port                = "traffic-port"
    interval            = 10
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = {
    Name = "${var.name_prefix}-grpc"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_lb_listener" "grpc" {
  count = var.enable_nlb ? 1 : 0

  load_balancer_arn = aws_lb.internal[0].arn
  port              = 9443
  protocol          = "TCP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.grpc[0].arn
  }

  tags = {
    Name = "${var.name_prefix}-grpc"
  }
}

###############################################################################
# Shield Advanced
#
# The subscription itself is an organization-level 1-year commitment made in the
# management account and is deliberately not managed by this stack: a
# `terraform destroy` here must not be able to cancel it. These resources only
# register the ALB for automatic application-layer mitigation and for DRT
# engagement.
###############################################################################

resource "aws_shield_protection" "alb" {
  count = var.enable_shield_advanced ? 1 : 0

  name         = "${var.name_prefix}-alb"
  resource_arn = aws_lb.public.arn

  tags = {
    Name = "${var.name_prefix}-alb-shield"
  }
}

resource "aws_shield_application_layer_automatic_response" "alb" {
  count = var.enable_shield_advanced ? 1 : 0

  resource_arn = aws_lb.public.arn
  action       = "BLOCK"

  depends_on = [
    aws_shield_protection.alb,
    aws_wafv2_web_acl_association.alb,
  ]
}
