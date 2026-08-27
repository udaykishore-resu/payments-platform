output "account_id" {
  description = "AWS account ID."
  value       = local.account_id
}

output "region" {
  description = "Region."
  value       = var.region
}

output "vpc_id" {
  description = "VPC ID."
  value       = module.network.vpc_id
}

output "subnet_cidrs_for_network_policies" {
  description = "ipBlock CIDRs the NetworkPolicies reference."
  value = {
    data      = module.network.data_subnet_cidrs
    streaming = module.network.streaming_subnet_cidrs
    egress    = module.network.egress_subnet_cidrs
  }
}

output "eks_cluster" {
  description = "Cluster coordinates for the CI pipeline and for the preview-environment ApplicationSet."
  value = {
    name     = module.eks.cluster_name
    endpoint = module.eks.cluster_endpoint
    ca_data  = module.eks.cluster_certificate_authority_data
    version  = module.eks.cluster_version
    region   = var.region
  }
}

output "irsa_role_arns" {
  description = "deployable => IRSA role ARN."
  value       = module.eks.deployable_role_arns
}

output "platform_role_arns" {
  description = "Platform component => IRSA role ARN."
  value       = module.eks.platform_role_arns
}

output "aurora" {
  description = "Aurora Serverless v2 connection targets."
  value = {
    cluster_identifier = module.aurora.cluster_identifier
    writer_endpoint    = module.aurora.writer_endpoint
    reader_endpoint    = module.aurora.reader_endpoint
    port               = module.aurora.port
    database           = module.aurora.database_name
  }
}

output "msk_bootstrap_servers" {
  description = "MSK Serverless SASL/IAM bootstrap servers."
  value       = module.msk.bootstrap_brokers_sasl_iam
}

output "redis_endpoint" {
  description = "Redis primary endpoint (single node)."
  value       = module.elasticache.primary_endpoint_address
}

output "s3_buckets" {
  description = "purpose => bucket name."
  value       = module.s3.bucket_ids
}

output "kms_key_aliases" {
  description = "purpose => alias name."
  value       = module.kms.alias_names
}

output "secret_arns" {
  description = "Platform secret ARNs - names, not values. Dev secrets hold FAKE values (deployment.md 6)."
  value       = module.secrets.secret_arns
}

output "secrets_path_prefix" {
  description = "Secret path prefix."
  value       = local.secrets_path_prefix
}

output "alb" {
  description = "ALB DNS name and target group ARN."
  value = {
    dns_name         = module.edge.alb_dns_name
    target_group_arn = module.edge.api_target_group_arn
  }
}

output "api_fqdn" {
  description = "API hostname."
  value       = module.dns.api_fqdn
}

output "dr_control" {
  description = "Fencing table."
  value = {
    table_name = module.dr.fencing_table_name
    table_arn  = module.dr.fencing_table_arn
  }
}

output "observability" {
  description = "AMP endpoints and the alert topic."
  value = {
    amp_workspace_arn = module.observability.prometheus_workspace_arn
    amp_remote_write  = module.observability.prometheus_remote_write_url
    amp_query         = module.observability.prometheus_query_url
    alerts_topic_arn  = module.observability.alerts_topic_arn
  }
}

output "default_tags" {
  description = "The tag set applied to every resource."
  value       = local.default_tags
}
