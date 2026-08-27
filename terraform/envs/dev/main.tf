###############################################################################
# envs/dev - single region, cheapest shape that still exercises the real code
#
# What is dropped here and why, stated plainly rather than left to be inferred:
#
#   Network Firewall        ~USD 300/mo per endpoint. The in-cluster egress
#                           proxy still enforces the allowlist.
#   Per-AZ NAT              ~USD 70/mo. Voids AZ independence for egress, which
#                           dev does not claim.
#   Provisioned Aurora      Serverless v2 0.5-4 ACU, scaled to floor out of
#                           hours.
#   Provisioned MSK         MSK Serverless. Note the real consequence: a
#                           serverless cluster has no broker configuration, so
#                           min.insync.replicas and
#                           unclean.leader.election.enable cannot be pinned.
#                           Durability behaviour in dev therefore does NOT match
#                           prod, and a durability test result from dev means
#                           nothing. That is why the msk module refuses
#                           serverless in prod.
#   Shield Advanced         Per-resource priced; dev is not a DDoS target.
#   Multi-Region KMS keys   No second region to decrypt in.
#
# What is NOT dropped: encryption with customer-managed keys, the full tag set,
# per-deployable IRSA roles, private subnets for everything stateful, VPC
# endpoints, and the twelve WAF rules. Those are the controls that are
# impossible to retrofit and cheap to keep.
###############################################################################

provider "aws" {
  region = var.region

  default_tags {
    tags = local.default_tags
  }
}

provider "aws" {
  alias  = "us_east_1"
  region = "us-east-1"

  default_tags {
    tags = local.default_tags
  }
}

data "aws_caller_identity" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id

  default_tags = {
    Environment        = var.environment
    Service            = "payment-orchestration-platform"
    Owner              = var.owner
    CostCentre         = var.cost_centre
    DataClassification = var.data_classification
    ComplianceScope    = var.compliance_scope
    ManagedBy          = "terraform"
    Repository         = "payments-platform"
    Stack              = "terraform/envs/${var.environment}"
    RegionRole         = "primary"
  }

  secrets_path_prefix = "/${var.environment}"
  cluster_name        = "pp-${var.environment}-${var.region}"
  name_prefix         = "pp-${var.environment}"

  deployables = {
    payment-api          = { namespace = "pp-data-plane", service_account = "payment-api" }
    payment-orchestrator = { namespace = "pp-data-plane", service_account = "payment-orchestrator" }
    webhook-ingress      = { namespace = "pp-data-plane", service_account = "webhook-ingress" }
    outbox-relay         = { namespace = "pp-data-plane", service_account = "outbox-relay" }
    event-consumer       = { namespace = "pp-data-plane", service_account = "event-consumer" }
    control-plane-api    = { namespace = "pp-control-plane", service_account = "control-plane-api" }
    workflow-worker      = { namespace = "pp-automation", service_account = "workflow-worker" }
  }

  irsa_role_arns = {
    for k, v in local.deployables : k => "arn:aws:iam::${local.account_id}:role/pp-${var.environment}-${k}"
  }

  # The KMS module grants ONLY the principals named here - there is no
  # account-root delegation statement - so a consumer missing from this list
  # surfaces as an AccessDenied on first use rather than being silently
  # permitted. See terraform/README.md, "Known caveats".
  kms_key_users = {
    rds = values(local.irsa_role_arns)
    s3 = [
      local.irsa_role_arns["workflow-worker"],
      local.irsa_role_arns["event-consumer"],
      "arn:aws:iam::${local.account_id}:role/pp-${var.environment}-backup",
    ]
    secrets     = concat(values(local.irsa_role_arns), ["arn:aws:iam::${local.account_id}:role/${local.cluster_name}-external-secrets"])
    elasticache = [local.irsa_role_arns["payment-api"], local.irsa_role_arns["payment-orchestrator"]]
    msk         = values(local.irsa_role_arns)
    dynamodb    = [local.irsa_role_arns["payment-api"], local.irsa_role_arns["payment-orchestrator"]]
    ebs         = ["arn:aws:iam::${local.account_id}:role/${local.cluster_name}-ebs-csi"]
    backup      = ["arn:aws:iam::${local.account_id}:role/pp-${var.environment}-backup"]
  }

  # Services that call KMS with their OWN identity. S3, Secrets Manager and
  # DynamoDB mostly call KMS as the CALLER, which is why the caller's role is in
  # kms_key_users above with a kms:ViaService condition; the three exceptions
  # below use service-created grants.
  kms_service_principals = {
    logs        = ["logs.${var.region}.amazonaws.com"]
    s3          = ["delivery.logs.amazonaws.com"]
    rds         = ["rds.amazonaws.com", "monitoring.rds.amazonaws.com"]
    msk         = ["kafka.amazonaws.com"]
    elasticache = ["elasticache.amazonaws.com"]
    backup      = ["backup.amazonaws.com"]
    ebs         = ["ec2.amazonaws.com"]
    eks         = ["eks.amazonaws.com"]
    dynamodb    = ["dynamodb.amazonaws.com"]
    secrets     = ["secretsmanager.amazonaws.com"]
  }

  # Object Lock is present but short. The point in dev is that the code paths
  # that write to a locked bucket are exercised - a service that cannot write to
  # an Object Lock bucket should fail in dev, not in prod.
  buckets = {
    artifacts = {
      description         = "Certification reports from dev runs."
      object_lock_mode    = "GOVERNANCE"
      object_lock_days    = 1
      expiry_days         = 30
      data_classification = "internal"
    }
    kyc = {
      description         = "Synthetic KYC evidence."
      object_lock_mode    = "GOVERNANCE"
      object_lock_days    = 1
      expiry_days         = 30
      data_classification = "internal"
    }
    audit-archive = {
      description         = "Audit records exported from Aurora."
      object_lock_mode    = "GOVERNANCE"
      object_lock_days    = 1
      expiry_days         = 30
      data_classification = "internal"
    }
    alb-logs = {
      description                = "ALB access and connection logs. SSE-S3 rather than SSE-KMS: ELB log delivery is the one AWS-managed writer that does not support a customer-managed key, so it gets its own bucket instead of every other bucket being weakened to accommodate it."
      sse_algorithm              = "AES256"
      expiry_days                = 14
      transition_to_ia_days      = 30
      data_classification        = "confidential"
      allow_log_delivery_service = true
    }
    logs = {
      description                = "Log archive and VPC flow logs."
      expiry_days                = 14
      data_classification        = "internal"
      allow_log_delivery_service = true
    }
    backups = {
      description         = "Logical exports and config snapshots."
      expiry_days         = 14
      data_classification = "internal"
    }
  }

  platform_secrets = {
    "_platform/jwks/signing" = {
      description = "Platform JWT signing key pair (dev key, never trusted anywhere else)."
      readers     = [local.irsa_role_arns["payment-api"], local.irsa_role_arns["control-plane-api"]]
    }
    "_platform/db/master" = {
      description = "Aurora master credentials."
      readers     = []
    }
    "_platform/redis/auth-token" = {
      description = "ElastiCache AUTH token."
      readers     = [local.irsa_role_arns["payment-api"], local.irsa_role_arns["payment-orchestrator"]]
    }
  }
}

module "kms" {
  source = "../../modules/kms"

  environment  = var.environment
  account_id   = local.account_id
  multi_region = false

  key_administrator_arns                = var.key_administrator_arns
  key_users                             = local.kms_key_users
  service_principals                    = local.kms_service_principals
  grant_autoscaling_service_linked_role = true

  # The shortest window AWS allows. Dev keys are recreated often enough that a
  # 30-day pending deletion just accumulates keys nobody can clean up.
  deletion_window_in_days = 7
}

module "s3" {
  source = "../../modules/s3"

  environment       = var.environment
  kms_key_arn       = module.kms.key_arns["s3"]
  source_account_id = local.account_id
  buckets           = local.buckets
}

module "network" {
  source = "../../modules/network"

  environment        = var.environment
  vpc_cidr           = var.vpc_cidr
  availability_zones = var.availability_zones

  public_subnet_cidrs    = var.subnets.public
  pod_subnet_cidrs       = var.subnets.pod
  data_subnet_cidrs      = var.subnets.data
  streaming_subnet_cidrs = var.subnets.streaming
  egress_subnet_cidrs    = var.subnets.egress
  firewall_subnet_cidrs  = var.subnets.firewall

  single_nat_gateway       = var.single_nat_gateway
  enable_network_firewall  = var.enable_network_firewall
  egress_allowlist_domains = var.egress_allowlist_domains

  flow_logs_bucket_arn         = module.s3.bucket_arns["logs"]
  dns_query_log_kms_key_arn    = module.kms.key_arns["logs"]
  dns_query_log_retention_days = 30

  eks_cluster_name    = local.cluster_name
  secrets_path_prefix = local.secrets_path_prefix

  vpc_endpoint_allowed_principal_arns = concat(
    values(local.irsa_role_arns),
    ["arn:aws:iam::${local.account_id}:role/${local.cluster_name}-*"],
  )
}

module "secrets" {
  source = "../../modules/secrets"

  environment = var.environment
  account_id  = local.account_id
  path_prefix = local.secrets_path_prefix
  kms_key_arn = module.kms.key_arns["secrets"]

  secrets = local.platform_secrets

  subnet_ids         = module.network.egress_subnet_ids
  security_group_ids = [module.network.app_nodes_security_group_id]

  cloudwatch_log_kms_key_arn = module.kms.key_arns["logs"]
  log_retention_days         = 30
}

module "aurora" {
  source = "../../modules/aurora"

  environment        = var.environment
  cluster_identifier = "pp-${var.environment}"
  engine_version     = var.aurora_engine_version

  # Serverless v2. The floor is what makes the 20:00-07:00 scale-down worth
  # anything; the ceiling is the cost cap.
  instance_class          = "db.serverless"
  serverless_min_capacity = var.aurora_serverless_min_acu
  serverless_max_capacity = var.aurora_serverless_max_acu
  reader_count            = 0

  subnet_ids         = module.network.data_subnet_ids
  security_group_ids = [module.network.aurora_security_group_id]
  kms_key_arn        = module.kms.key_arns["rds"]

  master_password_secret_arn = module.secrets.secret_arns["_platform/db/master"]

  backup_retention_period = 7
  deletion_protection     = false # Dev clusters are rebuilt from `platformctl seed`; protecting them just makes teardown a ticket.
  apply_immediately       = true

  performance_insights_enabled = true
  monitoring_interval          = 0 # Enhanced monitoring is per-instance-priced and dev has no latency SLO.

  cloudwatch_log_kms_key_arn    = module.kms.key_arns["logs"]
  cloudwatch_log_retention_days = 30
}

module "msk" {
  source = "../../modules/msk"

  environment  = var.environment
  cluster_name = "pp-${var.environment}"
  serverless   = true

  kafka_version = var.msk_kafka_version

  subnet_ids         = module.network.streaming_subnet_ids
  security_group_ids = [module.network.msk_security_group_id]

  kms_key_arn                = module.kms.key_arns["msk"]
  cloudwatch_log_kms_key_arn = module.kms.key_arns["logs"]
  log_retention_days         = 14
}

module "elasticache" {
  source = "../../modules/elasticache"

  environment          = var.environment
  replication_group_id = "pp-${var.environment}"

  node_type               = var.redis_node_type
  cluster_mode_enabled    = false
  replicas_per_node_group = 0 # Single node. Failover is not exercised in dev.

  subnet_ids         = module.network.data_subnet_ids
  security_group_ids = [module.network.redis_security_group_id]

  kms_key_arn                = module.kms.key_arns["elasticache"]
  cloudwatch_log_kms_key_arn = module.kms.key_arns["logs"]

  auth_token_secret_arn = module.secrets.secret_arns["_platform/redis/auth-token"]

  snapshot_retention_limit = 0 # Nothing here is worth a snapshot.
  apply_immediately        = true
}

module "dr" {
  source = "../../modules/dr"

  environment        = var.environment
  fencing_table_name = "pp-${var.environment}-dr-control"

  kms_key_arn     = module.kms.key_arns["dynamodb"]
  replica_regions = []

  fence_reader_role_arns = [
    local.irsa_role_arns["payment-api"],
    local.irsa_role_arns["payment-orchestrator"],
  ]

  fence_writer_role_arns = [
    "arn:aws:iam::${local.account_id}:role/pp-break-glass-dr-promotion",
  ]

  # No backup vault in dev: everything here is regenerable from
  # `platformctl seed`, and a backup of synthetic data is a bill, not a control.
  create_backup_vault      = false
  backup_vault_kms_key_arn = module.kms.key_arns["backup"]
}

resource "aws_cloudwatch_log_group" "waf" {
  name              = "aws-waf-logs-${local.name_prefix}"
  retention_in_days = 30
  kms_key_id        = module.kms.key_arns["logs"]

  tags = {
    Name = "aws-waf-logs-${local.name_prefix}"
  }
}

module "observability" {
  source = "../../modules/observability"

  environment = var.environment
  name_prefix = local.name_prefix

  kms_key_arn_logs = module.kms.key_arns["logs"]
  sns_kms_key_arn  = module.kms.key_arns["logs"]

  create_prometheus_workspace = true
  prometheus_retention_days   = 31
  create_grafana_workspace    = false

  alert_topic_subscriptions = var.alert_topic_subscriptions
  alb_arn_suffix            = module.edge.alb_arn_suffix

  budget_monthly_usd         = var.budget_monthly_usd
  budget_notification_emails = var.budget_notification_emails
}

module "edge" {
  source = "../../modules/edge"

  environment = var.environment
  name_prefix = local.name_prefix

  vpc_id                 = module.network.vpc_id
  public_subnet_ids      = module.network.public_subnet_ids
  private_subnet_ids     = module.network.pod_subnet_ids
  alb_security_group_ids = [module.network.alb_security_group_id]

  domain_name               = var.api_hostname
  subject_alternative_names = ["*.preview.${var.api_hostname}"] # Ephemeral preview environments (deployment.md 6.2).
  route53_zone_id           = var.route53_zone_id

  access_logs_bucket = module.s3.bucket_ids["alb-logs"]

  # Same twelve rules, same thresholds. A dev WAF tuned looser would let a rule
  # regression through to staging.
  waf_log_destination_arn = aws_cloudwatch_log_group.waf.arn

  enable_shield_advanced = false
  enable_nlb             = false # No cross-cluster gRPC in dev.
  deletion_protection    = false

  cloudfront_origin_secret_arn = null
}

module "dns" {
  source = "../../modules/dns"

  providers = {
    aws = aws.us_east_1
  }

  environment      = var.environment
  zone_name        = var.zone_name
  create_zone      = false
  existing_zone_id = var.route53_zone_id
  api_hostname     = var.api_hostname

  endpoints = {
    (var.region) = {
      role              = "PRIMARY"
      alb_dns_name      = module.edge.alb_dns_name
      alb_zone_id       = module.edge.alb_zone_id
      health_check_fqdn = module.edge.alb_dns_name
    }
  }
}

resource "aws_iam_policy" "irsa_boundary" {
  name        = "pp-${var.environment}-irsa-boundary"
  description = "Permissions boundary for every IRSA role and the node role."

  policy = templatefile("${path.module}/../../policies/irsa-permissions-boundary.json.tftpl", {
    account_id          = local.account_id
    secrets_path_prefix = local.secrets_path_prefix
    approved_regions    = [var.region, "us-east-1"]
  })

  tags = {
    Name = "pp-${var.environment}-irsa-boundary"
  }
}

module "eks" {
  source = "../../modules/eks"

  environment        = var.environment
  cluster_name       = local.cluster_name
  kubernetes_version = var.kubernetes_version
  account_id         = local.account_id

  vpc_id                    = module.network.vpc_id
  subnet_ids                = module.network.pod_subnet_ids
  cluster_security_group_id = module.network.eks_control_plane_security_group_id
  node_security_group_id    = module.network.app_nodes_security_group_id
  public_access_cidrs       = var.eks_public_access_cidrs

  kms_key_arn_secrets = module.kms.key_arns["eks"]
  kms_key_arn_ebs     = module.kms.key_arns["ebs"]
  kms_key_arn_logs    = module.kms.key_arns["logs"]

  cluster_log_retention_days = 30

  node_groups    = var.node_groups
  addon_versions = var.eks_addon_versions

  secrets_path_prefix         = local.secrets_path_prefix
  kms_key_arn_secrets_manager = module.kms.key_arns["secrets"]
  kms_key_arn_s3              = module.kms.key_arns["s3"]

  msk_cluster_name           = module.msk.cluster_name
  aurora_cluster_resource_id = module.aurora.cluster_resource_id
  s3_bucket_arns             = module.s3.bucket_arns
  dr_control_table_arn       = module.dr.fencing_table_arn
  amp_workspace_arn          = module.observability.prometheus_workspace_arn
  route53_zone_arns          = [module.dns.zone_arn]

  # The boundary is kept in dev too. It costs nothing and it means a policy
  # written in dev cannot be more permissive than one written in prod - which is
  # how an over-broad policy usually reaches prod in the first place.
  permissions_boundary_arn = aws_iam_policy.irsa_boundary.arn
  irsa_service_accounts    = local.deployables
  access_entries           = var.eks_access_entries
}

check "irsa_role_names_match_the_constructed_arns" {
  assert {
    condition = alltrue([
      for k, arn in local.irsa_role_arns : contains(values(module.eks.deployable_role_arns), arn)
    ])
    error_message = "An IRSA role ARN constructed in locals does not match the role the EKS module created."
  }
}
