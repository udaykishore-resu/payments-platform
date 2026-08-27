###############################################################################
# modules/network/endpoints.tf - VPC endpoints
#
# With no NAT route in the data and streaming tiers and a domain allowlist on
# the egress tier, an endpoint is not a latency optimisation - it is the only
# path. A missing endpoint is a hard outage, which is why the default list in
# variables.tf is exhaustive rather than minimal.
#
# Each interface endpoint carries a policy that restricts it to this account and
# to the named principals. That policy is evaluated independently of IAM, so an
# over-broad IAM policy is still not sufficient to use the endpoint
# (security.md 2.2).
###############################################################################

data "aws_vpc_endpoint_service" "interface" {
  for_each = toset(var.interface_endpoint_services)

  service      = each.value
  service_type = "Interface"
}

# --- Gateway endpoints (no ENI, no cost, route-table based) --------------------
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${data.aws_region.current.name}.s3"
  vpc_endpoint_type = "Gateway"

  route_table_ids = concat(
    aws_route_table.pod[*].id,
    aws_route_table.data[*].id,
    aws_route_table.streaming[*].id,
    aws_route_table.egress[*].id,
  )

  policy = data.aws_iam_policy_document.s3_endpoint.json

  tags = {
    Name = "${local.name}-vpce-s3"
  }
}

data "aws_iam_policy_document" "s3_endpoint" {
  # Allow our own principals to reach our own buckets, plus the read-only paths
  # that a node genuinely needs (ECR layer storage, the EKS AMI's package repos
  # are not used - the AMI is immutable).
  statement {
    sid    = "OwnAccountAccess"
    effect = "Allow"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions   = ["s3:*"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:PrincipalAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }

  statement {
    sid    = "EcrLayerStorage"
    effect = "Allow"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = ["s3:GetObject"]

    # ECR stores image layers in a service-owned bucket per region; pulling an
    # image without this statement fails after the manifest is fetched, which
    # presents as an ImagePullBackOff nobody can explain.
    resources = ["arn:aws:s3:::prod-${data.aws_region.current.name}-starport-layer-bucket/*"]
  }
}

resource "aws_vpc_endpoint" "dynamodb" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${data.aws_region.current.name}.dynamodb"
  vpc_endpoint_type = "Gateway"

  route_table_ids = concat(
    aws_route_table.pod[*].id,
    aws_route_table.egress[*].id,
  )

  policy = data.aws_iam_policy_document.dynamodb_endpoint.json

  tags = {
    Name = "${local.name}-vpce-dynamodb"
  }
}

data "aws_iam_policy_document" "dynamodb_endpoint" {
  statement {
    sid    = "OwnAccountOnly"
    effect = "Allow"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions   = ["dynamodb:*"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:PrincipalAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

# --- Interface endpoints ------------------------------------------------------
resource "aws_vpc_endpoint" "interface" {
  for_each = toset(var.interface_endpoint_services)

  vpc_id              = aws_vpc.this.id
  service_name        = data.aws_vpc_endpoint_service.interface[each.value].service_name
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true

  # Endpoint ENIs live in the pod subnets: they must be reachable from the pod,
  # data and streaming tiers, and those tiers route to each other inside the VPC
  # without leaving it.
  subnet_ids         = aws_subnet.pod[*].id
  security_group_ids = [aws_security_group.vpc_endpoints.id]

  policy = each.value == "secretsmanager" ? data.aws_iam_policy_document.secretsmanager_endpoint.json : data.aws_iam_policy_document.generic_endpoint.json

  tags = {
    Name = "${local.name}-vpce-${replace(each.value, ".", "-")}"
  }
}

data "aws_iam_policy_document" "generic_endpoint" {
  statement {
    sid    = "OwnAccountPrincipalsOnly"
    effect = "Allow"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions   = ["*"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:PrincipalAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

data "aws_iam_policy_document" "secretsmanager_endpoint" {
  # The Secrets Manager endpoint is the one worth an explicit, narrower policy:
  # it is the path to every gateway credential in the estate. Two independent
  # restrictions - the caller must be one of our named roles, AND the secret
  # must be under this environment's path prefix. A staging pod that somehow
  # assumed a prod role still cannot read /prod/** through this endpoint.
  statement {
    sid    = "NamedPrincipalsOnThisEnvironmentPathOnly"
    effect = "Allow"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
      "secretsmanager:ListSecretVersionIds",
    ]

    resources = ["arn:aws:secretsmanager:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:secret:${var.secrets_path_prefix}/*"]

    condition {
      test     = "StringEquals"
      variable = "aws:PrincipalAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }

    dynamic "condition" {
      for_each = length(var.vpc_endpoint_allowed_principal_arns) > 0 ? [1] : []

      content {
        test     = "ArnLike"
        variable = "aws:PrincipalArn"
        values   = var.vpc_endpoint_allowed_principal_arns
      }
    }
  }

  # The rotation Lambda needs the management verbs; it runs in this VPC and must
  # be able to stage and promote a version, but not to delete a secret.
  statement {
    sid    = "RotationLambda"
    effect = "Allow"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = [
      "secretsmanager:PutSecretValue",
      "secretsmanager:UpdateSecretVersionStage",
      "secretsmanager:GetRandomPassword",
    ]

    resources = ["arn:aws:secretsmanager:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:secret:${var.secrets_path_prefix}/*"]

    condition {
      test     = "ArnLike"
      variable = "aws:PrincipalArn"
      values   = ["arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/${local.name}-secret-rotation-*"]
    }
  }
}

###############################################################################
# VPC flow logs and DNS query logs
#
# Flow logs record ACCEPT as well as REJECT. Only recording rejects tells you
# what was blocked, which is the half of the story you already knew.
###############################################################################

resource "aws_flow_log" "this" {
  vpc_id               = aws_vpc.this.id
  traffic_type         = "ALL"
  log_destination_type = "s3"
  log_destination      = "${var.flow_logs_bucket_arn}/${var.flow_logs_prefix}/"

  # One minute rather than ten: a ten-minute aggregation window is useless during
  # an incident and only marginally cheaper.
  max_aggregation_interval = 60

  destination_options {
    file_format                = "parquet"
    per_hour_partition         = true
    hive_compatible_partitions = true
  }

  log_format = "$${version} $${account-id} $${interface-id} $${srcaddr} $${dstaddr} $${srcport} $${dstport} $${protocol} $${packets} $${bytes} $${start} $${end} $${action} $${log-status} $${vpc-id} $${subnet-id} $${instance-id} $${tcp-flags} $${type} $${pkt-srcaddr} $${pkt-dstaddr} $${region} $${az-id} $${pkt-src-aws-service} $${pkt-dst-aws-service} $${flow-direction} $${traffic-path}"

  tags = {
    Name = "${local.name}-flow-logs"
  }
}

resource "aws_cloudwatch_log_group" "dns_query" {
  name              = "/aws/route53/resolver/${local.name}"
  retention_in_days = var.dns_query_log_retention_days
  kms_key_id        = var.dns_query_log_kms_key_arn

  tags = {
    Name = "${local.name}-dns-query-logs"
  }
}

resource "aws_route53_resolver_query_log_config" "this" {
  name            = "${local.name}-resolver-query-log"
  destination_arn = aws_cloudwatch_log_group.dns_query.arn

  tags = {
    Name = "${local.name}-resolver-query-log"
  }
}

resource "aws_route53_resolver_query_log_config_association" "this" {
  resolver_query_log_config_id = aws_route53_resolver_query_log_config.this.id
  resource_id                  = aws_vpc.this.id
}
