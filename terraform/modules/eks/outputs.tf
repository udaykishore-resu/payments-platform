output "cluster_name" {
  description = "EKS cluster name."
  value       = aws_eks_cluster.this.name
}

output "cluster_arn" {
  description = "EKS cluster ARN."
  value       = aws_eks_cluster.this.arn
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint. Private unless the public allowlist is non-empty."
  value       = aws_eks_cluster.this.endpoint
}

output "cluster_certificate_authority_data" {
  description = "Base64 cluster CA. Public information - it authenticates the server to the client, not the other way round."
  value       = aws_eks_cluster.this.certificate_authority[0].data
}

output "cluster_version" {
  description = "Running Kubernetes minor version."
  value       = aws_eks_cluster.this.version
}

output "oidc_provider_arn" {
  description = "IAM OIDC provider ARN, for IRSA trust policies created outside this module."
  value       = aws_iam_openid_connect_provider.this.arn
}

output "oidc_provider_url" {
  description = "OIDC issuer URL without the scheme."
  value       = local.oidc_provider_url
}

output "node_role_arn" {
  description = "Shared node instance role ARN. Karpenter passes this to EC2."
  value       = aws_iam_role.node.arn
}

output "node_group_names" {
  description = "Managed node group names."
  value       = [for k, v in aws_eks_node_group.this : v.node_group_name]
}

output "deployable_role_arns" {
  description = "deployable => IRSA role ARN. This is what the Helm values render into each ServiceAccount's eks.amazonaws.com/role-arn annotation."
  value       = { for k, v in aws_iam_role.deployable : k => v.arn }
}

output "platform_role_arns" {
  description = "platform component => IRSA role ARN."
  value       = { for k, v in aws_iam_role.platform : k => v.arn }
}

output "karpenter_interruption_queue_name" {
  description = "SQS queue Karpenter polls for Spot interruption notices."
  value       = aws_sqs_queue.karpenter_interruption.name
}

output "cluster_log_group_name" {
  description = "CloudWatch log group carrying the control-plane audit log."
  value       = aws_cloudwatch_log_group.cluster.name
}

output "addon_versions" {
  description = "The add-on versions actually running, so a drift job can compare them to the declared ones."
  value = {
    vpc-cni    = aws_eks_addon.vpc_cni.addon_version
    coredns    = aws_eks_addon.coredns.addon_version
    kube-proxy = aws_eks_addon.kube_proxy.addon_version
    ebs-csi    = aws_eks_addon.ebs_csi.addon_version
  }
}
