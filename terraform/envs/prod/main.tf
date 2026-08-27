###############################################################################
# envs/prod - the production stack, both regions
#
# Apply order inside this stack is resolved by Terraform from the dependency
# graph. The order between STACKS is not, and matters - see terraform/README.md
# section 3.
#
# Region topology (disaster-recovery.md 2): eu-west-1 active, eu-central-1
# warm-passive at a 10% replica floor. Both regions run the same digest; only
# the write authority differs, and it moves via the DynamoDB fencing token, not
# via anything in this file.
###############################################################################

provider "aws" {
  region = var.primary_region

  default_tags {
    tags = local.default_tags
  }
}

provider "aws" {
  alias  = "dr"
  region = var.dr_region

  default_tags {
    tags = merge(local.default_tags, { RegionRole = "secondary" })
  }
}

# Route 53 health-check metrics and CloudFront certificates are only ever
# published in us-east-1, regardless of where the resources they describe live.
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

  # The tag set. Environment/Service/Owner answer "who do I page"; CostCentre
  # answers "who pays"; DataClassification and ComplianceScope are how the PCI
  # scope boundary is evidenced from the resource inventory alone, without
  # anyone having to be interviewed (docs/compliance.md 1).
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

  # The seven runtime deployables (baseline 5). gateway-simulator is build-tagged
  # out of production images and additionally denied by a Kyverno ClusterPolicy;
  # platformctl runs as a Job under the platformctl-migrator identity.
  deployables = {
    payment-api          = { namespace = "pp-data-plane", service_account = "payment-api" }
    payment-orchestrator = { namespace = "pp-data-plane", service_account = "payment-orchestrator" }
    webhook-ingress      = { namespace = "pp-data-plane", service_account = "webhook-ingress" }
    outbox-relay         = { namespace = "pp-data-plane", service_account = "outbox-relay" }
    event-consumer       = { namespace = "pp-data-plane", service_account = "event-consumer" }
    control-plane-api    = { namespace = "pp-control-plane", service_account = "control-plane-api" }
    workflow-worker      = { namespace = "pp-automation", service_account = "workflow-worker" }
  }

  cluster_name_primary = "pp-${var.environment}-${var.primary_region}"
  cluster_name_dr      = "pp-${var.environment}-${var.dr_region}"

  name_prefix_primary = "pp-${var.environment}-euw1"
  name_prefix_dr      = "pp-${var.environment}-euc1"

  # IRSA role ARNs are constructed from their deterministic names rather than
  # read from module.eks. That breaks what would otherwise be a genuine cycle:
  # the KMS key policies name the roles, the roles' policies name the keys, and
  # the DynamoDB fence policy names the roles while the roles' policies name the
  # table. Deterministic naming is what makes the graph acyclic; the EKS module
  # creates roles with exactly these names, and the check blocks at the bottom
  # of this file assert it.
  #
  # The names are per REGION because IAM is a global namespace: two clusters in
  # one account cannot both own a role called pp-prod-payment-api.
  irsa_role_arns = {
    for k, v in local.deployables : k => "arn:aws:iam::${local.account_id}:role/${local.name_prefix_primary}-${k}"
  }

  irsa_role_arns_dr = {
    for k, v in local.deployables : k => "arn:aws:iam::${local.account_id}:role/${local.name_prefix_dr}-${k}"
  }

  # Service-managed roles this stack creates with deterministic names, which
  # must appear in the key policies of the keys they use. The KMS module grants
  # ONLY the principals named here - there is no account-root delegation
  # statement - so a consumer missing from this list surfaces as an AccessDenied
  # on first use rather than being silently permitted. See
  # terraform/README.md, "Known caveats".
  s3_replication_role_arn = "arn:aws:iam::${local.account_id}:role/pp-${var.environment}-s3-replication"
  backup_role_arn         = "arn:aws:iam::${local.account_id}:role/pp-${var.environment}-backup"

  # Roles permitted to use each CMK. Note what is NOT here: no key grants use to
  # every deployable - payment-api cannot decrypt an S3 object, and
  # event-consumer cannot decrypt a gateway credential.
  #
  # Primary-region roles use the primary-region keys; DR-region roles use the
  # replica keys. A DR pod reads the Secrets Manager REPLICA, which is encrypted
  # under the replica key, so granting it the primary key would be both useless
  # and wrong.
  kms_key_users = {
    rds         = values(local.irsa_role_arns)
    s3          = [local.irsa_role_arns["workflow-worker"], local.irsa_role_arns["event-consumer"], local.s3_replication_role_arn, local.backup_role_arn]
    secrets     = concat(values(local.irsa_role_arns), ["arn:aws:iam::${local.account_id}:role/${local.cluster_name_primary}-external-secrets"])
    elasticache = [local.irsa_role_arns["payment-api"], local.irsa_role_arns["payment-orchestrator"]]
    msk         = values(local.irsa_role_arns)
    dynamodb    = [local.irsa_role_arns["payment-api"], local.irsa_role_arns["payment-orchestrator"]]
    ebs         = ["arn:aws:iam::${local.account_id}:role/${local.cluster_name_primary}-ebs-csi"]
    backup      = [local.backup_role_arn]
  }

  kms_key_users_dr = {
    rds         = values(local.irsa_role_arns_dr)
    s3          = [local.irsa_role_arns_dr["workflow-worker"], local.irsa_role_arns_dr["event-consumer"], local.s3_replication_role_arn]
    secrets     = concat(values(local.irsa_role_arns_dr), ["arn:aws:iam::${local.account_id}:role/${local.cluster_name_dr}-external-secrets"])
    elasticache = [local.irsa_role_arns_dr["payment-api"], local.irsa_role_arns_dr["payment-orchestrator"]]
    msk         = values(local.irsa_role_arns_dr)
    dynamodb    = [local.irsa_role_arns_dr["payment-api"], local.irsa_role_arns_dr["payment-orchestrator"]]
    ebs         = ["arn:aws:iam::${local.account_id}:role/${local.cluster_name_dr}-ebs-csi"]
  }

  # Services that call KMS with their OWN identity rather than the caller's.
  # S3, Secrets Manager and DynamoDB are absent from most of these lists on
  # purpose: they call KMS as the CALLER, which is why the caller's role has to
  # be in kms_key_users above with a kms:ViaService condition.
  #
  # The exceptions that do need a service principal:
  #   secretsmanager  cross-region replica maintenance runs as the service.
  #   dynamodb        table-level SSE uses a service-created grant.
  #   eks             envelope encryption of Kubernetes Secrets uses a grant.
  kms_service_principals = {
    logs        = ["logs.${var.primary_region}.amazonaws.com", "logs.${var.dr_region}.amazonaws.com"]
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

  # disaster-recovery.md 4.4. Object Lock mode is the decision that matters:
  # COMPLIANCE for evidence (nobody, including root, can delete it before its
  # retention date), GOVERNANCE for operational data (a specifically privileged
  # role can override, which is appropriate there and wrong for evidence).
  buckets = {
    artifacts = {
      description                   = "Signed certification reports (baseline 11.4)."
      object_lock_mode              = "COMPLIANCE"
      object_lock_days              = 2555 # 7 years
      transition_to_glacier_ir_days = 90
      replicate                     = true
      data_classification           = "confidential"
    }
    kyc = {
      description                   = "KYC evidence."
      object_lock_mode              = "COMPLIANCE"
      object_lock_days              = 1825 # 5 years (baseline 17.3)
      transition_to_glacier_ir_days = 180
      replicate                     = true
      replication_time_control      = true
      data_classification           = "restricted"
    }
    settlement = {
      description                   = "Gateway settlement files."
      object_lock_mode              = "GOVERNANCE"
      object_lock_days              = 2555
      transition_to_glacier_ir_days = 90
      replicate                     = true
      data_classification           = "restricted"
    }
    audit-archive = {
      description                   = "Audit records exported from Aurora as they are written."
      object_lock_mode              = "COMPLIANCE"
      object_lock_days              = 2555
      transition_to_glacier_ir_days = 365
      replicate                     = true
      replication_time_control      = true
      data_classification           = "restricted"
    }
    alb-logs = {
      description                = "ALB access and connection logs. SSE-S3 rather than SSE-KMS: ELB log delivery is the one AWS-managed writer that does not support a customer-managed key, so it gets its own bucket instead of every other bucket being weakened to accommodate it."
      sse_algorithm              = "AES256"
      expiry_days                = 400
      transition_to_ia_days      = 30
      data_classification        = "confidential"
      allow_log_delivery_service = true
    }
    logs = {
      description                = "Log archive and VPC flow logs."
      expiry_days                = 400
      transition_to_ia_days      = 30
      replicate                  = true
      data_classification        = "confidential"
      allow_log_delivery_service = true
    }
    backups = {
      description         = "Logical pg_dump exports and nightly config snapshots."
      object_lock_mode    = "GOVERNANCE"
      object_lock_days    = 35
      expiry_days         = 400
      replicate           = true
      data_classification = "restricted"
    }
  }

  # Platform-level secrets. Per-tenant secrets under
  # /{env}/{tenant}/{merchant}/{gateway}/... are created at runtime by
  # control-plane-api, which holds CreateSecret and PutSecretValue but not
  # GetSecretValue.
  platform_secrets = {
    "_platform/jwks/signing" = {
      description   = "Platform JWT signing key pair. Rotated every 30 days with a two-key JWKS window (security.md 5.3)."
      rotation_days = 30
      readers = [
        local.irsa_role_arns["payment-api"],
        local.irsa_role_arns["control-plane-api"],
      ]
    }
    "_platform/db/master" = {
      description = "Aurora master credentials. Used for break-glass only; the application authenticates with 15-minute IAM tokens."
      readers     = []
    }
    "_platform/redis/auth-token" = {
      description = "ElastiCache AUTH token."
      readers = [
        local.irsa_role_arns["payment-api"],
        local.irsa_role_arns["payment-orchestrator"],
      ]
    }
    "_platform/edge/origin-verify" = {
      description = "CloudFront-to-ALB origin verification header value."
      readers     = []
    }
    "_platform/vendor/kyc" = {
      description = "KYC vendor API credentials."
      readers     = [local.irsa_role_arns["workflow-worker"]]
    }
    "_platform/vendor/bank-validation" = {
      description = "Bank account validation vendor credentials."
      readers     = [local.irsa_role_arns["workflow-worker"]]
    }
  }
}

###############################################################################
# 1. KMS - primary Multi-Region Keys and their DR replicas
###############################################################################

module "kms" {
  source = "../../modules/kms"

  environment  = var.environment
  account_id   = local.account_id
  multi_region = true # Required: the Aurora Global secondary, the Secrets Manager replicas and the S3 CRR destination all decrypt primary-region ciphertext.

  key_administrator_arns                = var.key_administrator_arns
  key_users                             = local.kms_key_users
  service_principals                    = local.kms_service_principals
  grant_autoscaling_service_linked_role = true
  enable_key_rotation                   = true
  deletion_window_in_days               = 30
}

module "kms_dr" {
  source = "../../modules/kms"

  providers = {
    aws = aws.dr
  }

  environment = var.environment
  account_id  = local.account_id

  # Replica keys: same key material, same key-id suffix. A ciphertext produced
  # in eu-west-1 decrypts in eu-central-1 with no re-encryption, which is what
  # keeps KMS entirely off the failover critical path.
  replica_source_key_arns = module.kms.key_arns

  key_administrator_arns  = var.key_administrator_arns
  key_users               = local.kms_key_users_dr
  service_principals      = local.kms_service_principals
  deletion_window_in_days = 30
}

###############################################################################
# 2. S3 - DR destinations first, then the primary buckets that replicate to them
###############################################################################

module "s3_dr" {
  source = "../../modules/s3"

  providers = {
    aws = aws.dr
  }

  environment       = var.environment
  name_suffix       = "-dr"
  kms_key_arn       = module.kms_dr.key_arns["s3"]
  source_account_id = local.account_id

  # The destination buckets carry the same Object Lock settings as the sources.
  # A replicated audit record that is deletable in the DR region is not an
  # audit record.
  buckets = { for k, v in local.buckets : k => merge(v, { replicate = false, replication_time_control = false }) }

  enable_access_logging = true
}

module "s3" {
  source = "../../modules/s3"

  environment       = var.environment
  kms_key_arn       = module.kms.key_arns["s3"]
  source_account_id = local.account_id
  buckets           = local.buckets

  replica_bucket_arns = module.s3_dr.bucket_arns
  replica_kms_key_arn = module.kms_dr.key_arns["s3"]

  enable_access_logging = true
}

###############################################################################
# 3. Network
###############################################################################

module "network" {
  source = "../../modules/network"

  environment        = var.environment
  region_role        = "primary"
  vpc_cidr           = var.primary_vpc_cidr
  availability_zones = var.primary_availability_zones

  public_subnet_cidrs    = var.primary_subnets.public
  pod_subnet_cidrs       = var.primary_subnets.pod
  data_subnet_cidrs      = var.primary_subnets.data
  streaming_subnet_cidrs = var.primary_subnets.streaming
  egress_subnet_cidrs    = var.primary_subnets.egress
  firewall_subnet_cidrs  = var.primary_subnets.firewall

  single_nat_gateway       = false # One NAT per AZ. AZ independence is not negotiable in prod.
  enable_network_firewall  = true
  egress_allowlist_domains = var.egress_allowlist_domains

  flow_logs_bucket_arn         = module.s3.bucket_arns["logs"]
  dns_query_log_kms_key_arn    = module.kms.key_arns["logs"]
  dns_query_log_retention_days = 365

  eks_cluster_name    = local.cluster_name_primary
  secrets_path_prefix = local.secrets_path_prefix

  vpc_endpoint_allowed_principal_arns = concat(
    values(local.irsa_role_arns),
    ["arn:aws:iam::${local.account_id}:role/${local.cluster_name_primary}-*"],
  )
}

module "network_dr" {
  source = "../../modules/network"

  providers = {
    aws = aws.dr
  }

  environment        = var.environment
  region_role        = "secondary"
  vpc_cidr           = var.dr_vpc_cidr
  availability_zones = var.dr_availability_zones

  public_subnet_cidrs    = var.dr_subnets.public
  pod_subnet_cidrs       = var.dr_subnets.pod
  data_subnet_cidrs      = var.dr_subnets.data
  streaming_subnet_cidrs = var.dr_subnets.streaming
  egress_subnet_cidrs    = var.dr_subnets.egress
  firewall_subnet_cidrs  = var.dr_subnets.firewall

  single_nat_gateway       = false
  enable_network_firewall  = true
  egress_allowlist_domains = var.egress_allowlist_domains

  flow_logs_bucket_arn         = module.s3_dr.bucket_arns["logs"]
  dns_query_log_kms_key_arn    = module.kms_dr.key_arns["logs"]
  dns_query_log_retention_days = 365

  eks_cluster_name    = local.cluster_name_dr
  secrets_path_prefix = local.secrets_path_prefix

  vpc_endpoint_allowed_principal_arns = concat(
    values(local.irsa_role_arns),
    ["arn:aws:iam::${local.account_id}:role/${local.cluster_name_dr}-*"],
  )
}

###############################################################################
# 4. Secrets
#
# Containers and policies only. Values are seeded out of band - see
# terraform/README.md, "Not managed here".
###############################################################################

module "secrets" {
  source = "../../modules/secrets"

  environment = var.environment
  account_id  = local.account_id
  path_prefix = local.secrets_path_prefix
  kms_key_arn = module.kms.key_arns["secrets"]

  replica_regions = [
    {
      region      = var.dr_region
      kms_key_arn = module.kms_dr.key_arns["secrets"]
    },
  ]

  secrets = local.platform_secrets

  rotation_lambdas = {
    for k, v in var.rotation_lambda_artifacts : k => {
      description       = v.description
      s3_bucket         = var.rotation_lambda_artifact_bucket
      s3_key            = v.s3_key
      s3_object_version = v.s3_object_version
    }
  }

  subnet_ids         = module.network.egress_subnet_ids
  security_group_ids = [module.network.app_nodes_security_group_id]

  cloudwatch_log_kms_key_arn = module.kms.key_arns["logs"]
  log_retention_days         = 365
}

###############################################################################
# 5. Aurora Global
###############################################################################

resource "aws_rds_global_cluster" "this" {
  global_cluster_identifier = "pp-${var.environment}-global"
  engine                    = "aurora-postgresql"
  engine_version            = var.aurora_engine_version
  database_name             = "payments"
  storage_encrypted         = true
  deletion_protection       = true

  lifecycle {
    prevent_destroy = true

    ignore_changes = [
      # The primary cluster attaches itself; recording it here would fight the
      # member clusters on every plan.
      database_name,
    ]
  }
}

module "aurora" {
  source = "../../modules/aurora"

  environment        = var.environment
  cluster_identifier = "pp-${var.environment}"
  engine_version     = var.aurora_engine_version
  instance_class     = var.aurora_instance_class
  reader_count       = 2 # One per remaining AZ: an AZ loss leaves a reader to promote and a reader to serve.

  subnet_ids         = module.network.data_subnet_ids
  security_group_ids = [module.network.aurora_security_group_id]
  kms_key_arn        = module.kms.key_arns["rds"]

  global_cluster_identifier  = aws_rds_global_cluster.this.id
  master_password_secret_arn = module.secrets.secret_arns["_platform/db/master"]

  backup_retention_period      = var.aurora_backup_retention_days
  preferred_backup_window      = "02:00-03:00"
  preferred_maintenance_window = "sun:04:00-sun:05:00"

  deletion_protection = true

  # False in prod. A parameter change that triggers a reboot or a failover must
  # land in the maintenance window, when the on-call engineer is expecting it -
  # not at 15:00 on the Tuesday the plan happened to be approved.
  apply_immediately = false

  performance_insights_enabled          = true
  performance_insights_retention_period = 731 # Two years: a capacity or regression question is often asked long after the fact.
  monitoring_interval                   = 1

  cloudwatch_log_kms_key_arn    = module.kms.key_arns["logs"]
  cloudwatch_log_retention_days = 365
}

module "aurora_dr" {
  source = "../../modules/aurora"

  providers = {
    aws = aws.dr
  }

  environment         = var.environment
  cluster_identifier  = "pp-${var.environment}-secondary"
  engine_version      = var.aurora_engine_version
  instance_class      = var.aurora_dr_instance_class
  reader_count        = 2
  is_global_secondary = true

  subnet_ids         = module.network_dr.data_subnet_ids
  security_group_ids = [module.network_dr.aurora_security_group_id]
  kms_key_arn        = module.kms_dr.key_arns["rds"]

  global_cluster_identifier = aws_rds_global_cluster.this.id

  preferred_maintenance_window = "sun:06:00-sun:07:00" # Offset from the primary's, so a maintenance event never hits both.

  deletion_protection = true
  apply_immediately   = false

  performance_insights_enabled          = true
  performance_insights_retention_period = 731
  monitoring_interval                   = 1

  cloudwatch_log_kms_key_arn    = module.kms_dr.key_arns["logs"]
  cloudwatch_log_retention_days = 365

  depends_on = [module.aurora]
}

###############################################################################
# 6. MSK
#
# Two independent clusters, no replication between them. See
# disaster-recovery.md 4.2 - the DR region's topics start empty and are refilled
# from the outbox, which is already the source of truth for events.
###############################################################################

module "msk" {
  source = "../../modules/msk"

  environment  = var.environment
  cluster_name = "pp-${var.environment}"

  kafka_version             = var.msk_kafka_version
  broker_instance_type      = var.msk_broker_instance_type
  broker_count              = 3
  broker_ebs_volume_size_gb = var.msk_broker_ebs_gb

  subnet_ids         = module.network.streaming_subnet_ids
  security_group_ids = [module.network.msk_security_group_id]

  kms_key_arn                = module.kms.key_arns["msk"]
  cloudwatch_log_kms_key_arn = module.kms.key_arns["logs"]
  log_retention_days         = 90

  enable_iam_auth   = true
  enable_scram_auth = false
}

module "msk_dr" {
  source = "../../modules/msk"

  providers = {
    aws = aws.dr
  }

  environment  = var.environment
  cluster_name = "pp-${var.environment}-dr"

  kafka_version             = var.msk_kafka_version
  broker_instance_type      = var.msk_broker_instance_type
  broker_count              = 3
  broker_ebs_volume_size_gb = var.msk_broker_ebs_gb

  subnet_ids         = module.network_dr.streaming_subnet_ids
  security_group_ids = [module.network_dr.msk_security_group_id]

  kms_key_arn                = module.kms_dr.key_arns["msk"]
  cloudwatch_log_kms_key_arn = module.kms_dr.key_arns["logs"]
  log_retention_days         = 90

  enable_iam_auth   = true
  enable_scram_auth = false
}

###############################################################################
# 7. ElastiCache
###############################################################################

module "elasticache" {
  source = "../../modules/elasticache"

  environment          = var.environment
  replication_group_id = "pp-${var.environment}"

  node_type               = var.redis_node_type
  cluster_mode_enabled    = true
  num_node_groups         = var.redis_shards
  replicas_per_node_group = 1

  subnet_ids         = module.network.data_subnet_ids
  security_group_ids = [module.network.redis_security_group_id]

  kms_key_arn                = module.kms.key_arns["elasticache"]
  cloudwatch_log_kms_key_arn = module.kms.key_arns["logs"]

  auth_token_secret_arn = module.secrets.secret_arns["_platform/redis/auth-token"]

  apply_immediately = false
}

module "elasticache_dr" {
  source = "../../modules/elasticache"

  providers = {
    aws = aws.dr
  }

  environment          = var.environment
  replication_group_id = "pp-${var.environment}-dr"

  node_type               = var.redis_node_type
  cluster_mode_enabled    = true
  num_node_groups         = var.redis_shards
  replicas_per_node_group = 1

  subnet_ids         = module.network_dr.data_subnet_ids
  security_group_ids = [module.network_dr.redis_security_group_id]

  kms_key_arn                = module.kms_dr.key_arns["elasticache"]
  cloudwatch_log_kms_key_arn = module.kms_dr.key_arns["logs"]

  # The DR cache is a separate, empty cluster. Nothing in it is authoritative,
  # so there is nothing to replicate; it is warmed by a PostSync hook on
  # promotion (disaster-recovery.md 4.3).
  auth_token_secret_arn = module.secrets.secret_arns["_platform/redis/auth-token"]

  apply_immediately = false
}

###############################################################################
# 8. DR control plane - the fencing token and the backup vault
###############################################################################

module "dr" {
  source = "../../modules/dr"

  environment        = var.environment
  fencing_table_name = "pp-dr-control"

  kms_key_arn     = module.kms.key_arns["dynamodb"]
  replica_regions = [var.dr_region]

  replica_kms_key_arns = {
    (var.dr_region) = module.kms_dr.key_arns["dynamodb"]
  }

  # Readers are the two money-path deployables that poll the fence every 10 s.
  # Note that the constructed ARNs are used here rather than module.eks outputs:
  # the EKS module needs the table ARN, so reading its outputs here would be a
  # cycle.
  fence_reader_role_arns = [
    local.irsa_role_arns["payment-api"],
    local.irsa_role_arns["payment-orchestrator"],
    local.irsa_role_arns_dr["payment-api"],
    local.irsa_role_arns_dr["payment-orchestrator"],
  ]

  # Exactly one writer: the promotion role the DR runbook uses. No application
  # workload can move the fence, so no bug can promote a region.
  fence_writer_role_arns = [
    "arn:aws:iam::${local.account_id}:role/pp-break-glass-dr-promotion",
  ]

  create_backup_vault            = true
  backup_vault_kms_key_arn       = module.kms.key_arns["backup"]
  backup_cross_account_vault_arn = var.backup_vault_account_arn
  backup_retention_days          = 35
  vault_lock_enabled             = true
  vault_lock_changeable_for_days = 3

  # Explicit ARNs, not a tag selector: a tag selector stops protecting a
  # resource the moment somebody edits a tag, and it does so silently. Aurora is
  # the only stateful resource whose loss is unrecoverable from anything else -
  # S3 has versioning and CRR, MSK is rebuilt from the outbox, Redis is
  # disposable.
  backup_resource_arns = [
    module.aurora.cluster_arn,
  ]

  backup_notification_sns_topic_arn = module.observability.alerts_topic_arn
}

###############################################################################
# 9. Observability
###############################################################################

resource "aws_cloudwatch_log_group" "waf_primary" {
  # WAF requires the destination log group name to start with aws-waf-logs-.
  name              = "aws-waf-logs-${local.name_prefix_primary}"
  retention_in_days = 365
  kms_key_id        = module.kms.key_arns["logs"]

  tags = {
    Name = "aws-waf-logs-${local.name_prefix_primary}"
  }
}

resource "aws_cloudwatch_log_group" "waf_dr" {
  provider = aws.dr

  name              = "aws-waf-logs-${local.name_prefix_dr}"
  retention_in_days = 365
  kms_key_id        = module.kms_dr.key_arns["logs"]

  tags = {
    Name = "aws-waf-logs-${local.name_prefix_dr}"
  }
}

module "observability" {
  source = "../../modules/observability"

  environment = var.environment
  name_prefix = local.name_prefix_primary

  kms_key_arn_logs = module.kms.key_arns["logs"]
  sns_kms_key_arn  = module.kms.key_arns["logs"]

  create_prometheus_workspace = true
  prometheus_retention_days   = 150
  create_grafana_workspace    = true
  grafana_sso_admin_group_ids = var.grafana_sso_admin_group_ids

  alert_topic_subscriptions = var.alert_topic_subscriptions
  alb_arn_suffix            = module.edge.alb_arn_suffix

  budget_monthly_usd         = var.budget_monthly_usd
  budget_notification_emails = var.budget_notification_emails
}

module "observability_dr" {
  source = "../../modules/observability"

  providers = {
    aws = aws.dr
  }

  environment = var.environment
  name_prefix = local.name_prefix_dr

  kms_key_arn_logs = module.kms_dr.key_arns["logs"]
  sns_kms_key_arn  = module.kms_dr.key_arns["logs"]

  create_prometheus_workspace = true
  prometheus_retention_days   = 150

  # No second Grafana. DR of the observability stack is deliberately not a
  # prerequisite for DR of the platform (disaster-recovery.md 1.1): every step
  # of the failover runbook reads the authoritative store directly, not a
  # dashboard.
  create_grafana_workspace = false

  alert_topic_subscriptions = var.alert_topic_subscriptions
  alb_arn_suffix            = module.edge_dr.alb_arn_suffix

  budget_monthly_usd         = var.budget_monthly_usd
  budget_notification_emails = var.budget_notification_emails
}

###############################################################################
# 10. Edge
###############################################################################

module "edge" {
  source = "../../modules/edge"

  environment = var.environment
  name_prefix = local.name_prefix_primary

  vpc_id                 = module.network.vpc_id
  public_subnet_ids      = module.network.public_subnet_ids
  private_subnet_ids     = module.network.pod_subnet_ids
  alb_security_group_ids = [module.network.alb_security_group_id]

  domain_name               = var.api_hostname
  subject_alternative_names = ["${var.primary_region}.${var.api_hostname}"]
  route53_zone_id           = var.route53_zone_id
  validation_record_suffix  = "-primary"

  access_logs_bucket = module.s3.bucket_ids["alb-logs"]
  access_logs_prefix = "alb/${var.primary_region}"

  waf_log_destination_arn = aws_cloudwatch_log_group.waf_primary.arn
  blocked_country_codes   = var.blocked_country_codes

  enable_shield_advanced = var.enable_shield_advanced
  deletion_protection    = true

  cloudfront_origin_secret_arn = module.secrets.secret_arns["_platform/edge/origin-verify"]
}

module "edge_dr" {
  source = "../../modules/edge"

  providers = {
    aws = aws.dr
  }

  environment = var.environment
  name_prefix = local.name_prefix_dr

  vpc_id                 = module.network_dr.vpc_id
  public_subnet_ids      = module.network_dr.public_subnet_ids
  private_subnet_ids     = module.network_dr.pod_subnet_ids
  alb_security_group_ids = [module.network_dr.alb_security_group_id]

  domain_name               = var.api_hostname
  subject_alternative_names = ["${var.dr_region}.${var.api_hostname}"]
  route53_zone_id           = var.route53_zone_id
  validation_record_suffix  = "-dr"

  access_logs_bucket = module.s3_dr.bucket_ids["alb-logs"]
  access_logs_prefix = "alb/${var.dr_region}"

  waf_log_destination_arn = aws_cloudwatch_log_group.waf_dr.arn
  blocked_country_codes   = var.blocked_country_codes

  enable_shield_advanced = var.enable_shield_advanced
  deletion_protection    = true

  cloudfront_origin_secret_arn = module.secrets.secret_arns["_platform/edge/origin-verify"]
}

###############################################################################
# 11. DNS - failover between the two regional ALBs
###############################################################################

module "dns" {
  source = "../../modules/dns"

  providers = {
    # Route 53 is global but its health-check alarms live in us-east-1.
    aws = aws.us_east_1
  }

  environment      = var.environment
  zone_name        = var.zone_name
  create_zone      = false
  existing_zone_id = var.route53_zone_id
  api_hostname     = var.api_hostname

  endpoints = {
    (var.primary_region) = {
      role              = "PRIMARY"
      alb_dns_name      = module.edge.alb_dns_name
      alb_zone_id       = module.edge.alb_zone_id
      health_check_fqdn = module.edge.alb_dns_name
    }
    (var.dr_region) = {
      role              = "SECONDARY"
      alb_dns_name      = module.edge_dr.alb_dns_name
      alb_zone_id       = module.edge_dr.alb_zone_id
      health_check_fqdn = module.edge_dr.alb_dns_name
    }
  }
}

###############################################################################
# 12. EKS - both regions
###############################################################################

resource "aws_iam_policy" "irsa_boundary" {
  name        = "pp-${var.environment}-irsa-boundary"
  description = "Permissions boundary for every IRSA role and the node role. Caps what any of them can ever do, independently of the policy attached to them."

  policy = templatefile("${path.module}/../../policies/irsa-permissions-boundary.json.tftpl", {
    account_id          = local.account_id
    secrets_path_prefix = local.secrets_path_prefix
    approved_regions    = [var.primary_region, var.dr_region, "us-east-1"]
  })

  tags = {
    Name = "pp-${var.environment}-irsa-boundary"
  }
}

module "eks" {
  source = "../../modules/eks"

  environment        = var.environment
  cluster_name       = local.cluster_name_primary
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

  cluster_log_retention_days = 365

  node_groups    = var.node_groups["primary"]
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

  permissions_boundary_arn = aws_iam_policy.irsa_boundary.arn
  irsa_role_name_prefix    = local.name_prefix_primary
  irsa_service_accounts    = local.deployables
  access_entries           = var.eks_access_entries
}

module "eks_dr" {
  source = "../../modules/eks"

  providers = {
    aws = aws.dr
  }

  environment        = var.environment
  cluster_name       = local.cluster_name_dr
  kubernetes_version = var.kubernetes_version
  account_id         = local.account_id

  vpc_id                    = module.network_dr.vpc_id
  subnet_ids                = module.network_dr.pod_subnet_ids
  cluster_security_group_id = module.network_dr.eks_control_plane_security_group_id
  node_security_group_id    = module.network_dr.app_nodes_security_group_id
  public_access_cidrs       = var.eks_public_access_cidrs

  kms_key_arn_secrets = module.kms_dr.key_arns["eks"]
  kms_key_arn_ebs     = module.kms_dr.key_arns["ebs"]
  kms_key_arn_logs    = module.kms_dr.key_arns["logs"]

  cluster_log_retention_days = 365

  # The DR node groups run the 10% warm-passive floor. Cold (zero-replica)
  # passive was measured at 4-6 minutes of image pull, JIT warm-up, pool
  # establishment and JWKS fetch added to the RTO; 10% costs about 9% of one
  # region's compute and removes that (disaster-recovery.md 2.2).
  node_groups    = var.node_groups["dr"]
  addon_versions = var.eks_addon_versions

  secrets_path_prefix         = local.secrets_path_prefix
  kms_key_arn_secrets_manager = module.kms_dr.key_arns["secrets"]
  kms_key_arn_s3              = module.kms_dr.key_arns["s3"]

  msk_cluster_name           = module.msk_dr.cluster_name
  aurora_cluster_resource_id = module.aurora_dr.cluster_resource_id
  s3_bucket_arns             = module.s3_dr.bucket_arns
  dr_control_table_arn       = module.dr.fencing_table_arn
  amp_workspace_arn          = module.observability_dr.prometheus_workspace_arn
  route53_zone_arns          = [module.dns.zone_arn]

  permissions_boundary_arn = aws_iam_policy.irsa_boundary.arn
  irsa_role_name_prefix    = local.name_prefix_dr
  irsa_service_accounts    = local.deployables
  access_entries           = var.eks_access_entries
}

###############################################################################
# Cross-checks that only make sense once everything is in one graph
###############################################################################

check "irsa_role_names_match_the_constructed_arns" {
  # The KMS key policies, the Secrets Manager resource policies and the DynamoDB
  # fence policy all name roles by a constructed ARN. If the EKS module ever
  # changed its naming convention those policies would silently grant nothing -
  # IAM accepts a policy naming a role that does not exist. This check turns
  # that silent failure into a failed apply.
  assert {
    condition = alltrue([
      for k, arn in local.irsa_role_arns : contains(values(module.eks.deployable_role_arns), arn)
    ])
    error_message = "A primary-region IRSA role ARN constructed in locals does not match the role the EKS module created. Every key policy and secret policy that names it now grants nothing."
  }

  assert {
    condition = alltrue([
      for k, arn in local.irsa_role_arns_dr : contains(values(module.eks_dr.deployable_role_arns), arn)
    ])
    error_message = "A DR-region IRSA role ARN constructed in locals does not match the role the EKS module created."
  }
}

check "both_regions_run_the_same_kubernetes_version" {
  assert {
    condition     = module.eks.cluster_version == module.eks_dr.cluster_version
    error_message = "The DR cluster is on a different Kubernetes version than the primary. A DR region running a different build is a DR plan that has not been tested (deployment.md 3.5)."
  }
}
