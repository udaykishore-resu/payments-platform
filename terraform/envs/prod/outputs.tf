###############################################################################
# envs/prod/outputs.tf
#
# Two consumers: the Helm values renderer (which turns these into the GitOps
# repo's environments/prod/config.yaml) and the CI pipeline.
#
# Nothing secret is output. Specifically absent, and absent on purpose:
#   - the Aurora master password (it is never read by Terraform at all)
#   - the Redis AUTH token
#   - the CloudFront origin-verify header value
#   - any Secrets Manager secret VALUE
# Secret ARNs are published, because an ARN is a name, not a credential: reading
# it requires an IAM grant that these outputs do not confer.
###############################################################################

output "account_id" {
  description = "AWS account ID."
  value       = local.account_id
}

output "regions" {
  description = "Active and passive regions."
  value = {
    primary = var.primary_region
    dr      = var.dr_region
  }
}

# --- Networking ---------------------------------------------------------------

output "vpc_ids" {
  description = "region role => VPC ID."
  value = {
    primary = module.network.vpc_id
    dr      = module.network_dr.vpc_id
  }
}

output "subnet_cidrs_for_network_policies" {
  description = "The ipBlock CIDRs the NetworkPolicies in deployments/k8s reference. Rendered into Helm values so a subnet change cannot silently desynchronise from the policies."
  value = {
    primary = {
      data      = module.network.data_subnet_cidrs
      streaming = module.network.streaming_subnet_cidrs
      egress    = module.network.egress_subnet_cidrs
    }
    dr = {
      data      = module.network_dr.data_subnet_cidrs
      streaming = module.network_dr.streaming_subnet_cidrs
      egress    = module.network_dr.egress_subnet_cidrs
    }
  }
}

output "nat_egress_ips" {
  description = "NAT public IPs. Gateways and vendors allowlist these on their side, so they are part of the platform's external contract - changing them is a coordinated change, not an infrastructure detail."
  value = {
    primary = module.network.nat_public_ips
    dr      = module.network_dr.nat_public_ips
  }
}

# --- EKS ----------------------------------------------------------------------

output "eks_clusters" {
  description = "Everything the CI pipeline needs to authenticate to each cluster. The CA data is public - it authenticates the server to the client, not the reverse."
  value = {
    primary = {
      name     = module.eks.cluster_name
      endpoint = module.eks.cluster_endpoint
      ca_data  = module.eks.cluster_certificate_authority_data
      version  = module.eks.cluster_version
      region   = var.primary_region
    }
    dr = {
      name     = module.eks_dr.cluster_name
      endpoint = module.eks_dr.cluster_endpoint
      ca_data  = module.eks_dr.cluster_certificate_authority_data
      version  = module.eks_dr.cluster_version
      region   = var.dr_region
    }
  }
}

output "irsa_role_arns" {
  description = "deployable => IRSA role ARN per region. Rendered into each ServiceAccount's eks.amazonaws.com/role-arn annotation."
  value = {
    primary = module.eks.deployable_role_arns
    dr      = module.eks_dr.deployable_role_arns
  }
}

output "platform_role_arns" {
  description = "Platform component => IRSA role ARN per region, for the ArgoCD-managed add-ons' values."
  value = {
    primary = module.eks.platform_role_arns
    dr      = module.eks_dr.platform_role_arns
  }
}

output "karpenter_interruption_queues" {
  description = "SQS queue names Karpenter polls in each region."
  value = {
    primary = module.eks.karpenter_interruption_queue_name
    dr      = module.eks_dr.karpenter_interruption_queue_name
  }
}

# --- Data services ------------------------------------------------------------

output "aurora" {
  description = "Connection targets. The application authenticates with IAM tokens, so there is no credential here to leak."
  value = {
    global_cluster_id = aws_rds_global_cluster.this.id
    primary = {
      cluster_identifier = module.aurora.cluster_identifier
      writer_endpoint    = module.aurora.writer_endpoint
      reader_endpoint    = module.aurora.reader_endpoint
      port               = module.aurora.port
      database           = module.aurora.database_name
    }
    dr = {
      cluster_identifier = module.aurora_dr.cluster_identifier
      reader_endpoint    = module.aurora_dr.reader_endpoint
      port               = module.aurora_dr.port
    }
  }
}

output "msk_bootstrap_servers" {
  description = "SASL/IAM bootstrap servers per region."
  value = {
    primary = module.msk.bootstrap_brokers_sasl_iam
    dr      = module.msk_dr.bootstrap_brokers_sasl_iam
  }
}

output "redis_endpoints" {
  description = "Configuration endpoints (cluster mode) per region."
  value = {
    primary = module.elasticache.configuration_endpoint_address
    dr      = module.elasticache_dr.configuration_endpoint_address
  }
}

output "s3_buckets" {
  description = "purpose => bucket name per region."
  value = {
    primary = module.s3.bucket_ids
    dr      = module.s3_dr.bucket_ids
  }
}

# --- Security -----------------------------------------------------------------

output "kms_key_aliases" {
  description = "purpose => alias name per region. Runbooks reference aliases, not key IDs."
  value = {
    primary = module.kms.alias_names
    dr      = module.kms_dr.alias_names
  }
}

output "secret_arns" {
  description = "Platform secret ARNs. Names, not values - reading one requires an IAM grant these outputs do not confer."
  value       = module.secrets.secret_arns
}

output "secrets_path_prefix" {
  description = "Environment secret path prefix, consumed by the External Secrets ClusterSecretStore configuration."
  value       = local.secrets_path_prefix
}

output "waf_web_acl_arns" {
  description = "WAF web ACL ARNs per region."
  value = {
    primary = module.edge.web_acl_arn
    dr      = module.edge_dr.web_acl_arn
  }
}

# --- Edge and DNS -------------------------------------------------------------

output "alb" {
  description = "ALB DNS names and target group ARNs, for the TargetGroupBinding objects."
  value = {
    primary = {
      dns_name         = module.edge.alb_dns_name
      target_group_arn = module.edge.api_target_group_arn
    }
    dr = {
      dns_name         = module.edge_dr.alb_dns_name
      target_group_arn = module.edge_dr.api_target_group_arn
    }
  }
}

output "api_fqdn" {
  description = "Public API hostname."
  value       = module.dns.api_fqdn
}

output "route53_health_check_ids" {
  description = "Health check IDs the DR runbook and the game-day scripts reference."
  value       = module.dns.health_check_ids
}

# --- DR -----------------------------------------------------------------------

output "dr_control" {
  description = "The fencing table. The promotion runbook's conditional write targets this table by name."
  value = {
    table_name = module.dr.fencing_table_name
    table_arn  = module.dr.fencing_table_arn
    stream_arn = module.dr.fencing_table_stream_arn
  }
}

output "backup_vault_arn" {
  description = "Local backup vault ARN."
  value       = module.dr.backup_vault_arn
}

# --- Observability ------------------------------------------------------------

output "observability" {
  description = "AMP and Grafana endpoints. The remote_write URL goes into the Prometheus agent's values; the query URL goes into the Argo Rollouts AnalysisTemplates."
  value = {
    primary = {
      amp_workspace_arn = module.observability.prometheus_workspace_arn
      amp_remote_write  = module.observability.prometheus_remote_write_url
      amp_query         = module.observability.prometheus_query_url
      grafana_endpoint  = module.observability.grafana_endpoint
      alerts_topic_arn  = module.observability.alerts_topic_arn
    }
    dr = {
      amp_workspace_arn = module.observability_dr.prometheus_workspace_arn
      amp_remote_write  = module.observability_dr.prometheus_remote_write_url
      amp_query         = module.observability_dr.prometheus_query_url
      alerts_topic_arn  = module.observability_dr.alerts_topic_arn
    }
  }
}

# --- Tagging ------------------------------------------------------------------

output "default_tags" {
  description = "The tag set applied to every resource. Emitted so the compliance evidence job can assert that the live inventory carries exactly these keys."
  value       = local.default_tags
}
