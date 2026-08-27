output "vpc_id" {
  description = "VPC ID."
  value       = aws_vpc.this.id
}

output "vpc_cidr" {
  description = "VPC CIDR block."
  value       = aws_vpc.this.cidr_block
}

output "availability_zones" {
  description = "AZs in the order the subnet lists are indexed by."
  value       = var.availability_zones
}

output "public_subnet_ids" {
  description = "Public subnet IDs (ALB, NAT)."
  value       = aws_subnet.public[*].id
}

output "pod_subnet_ids" {
  description = "EKS node/pod subnet IDs."
  value       = aws_subnet.pod[*].id
}

output "data_subnet_ids" {
  description = "Aurora and ElastiCache subnet IDs."
  value       = aws_subnet.data[*].id
}

output "streaming_subnet_ids" {
  description = "MSK broker subnet IDs."
  value       = aws_subnet.streaming[*].id
}

output "egress_subnet_ids" {
  description = "NAT-routed egress subnet IDs."
  value       = aws_subnet.egress[*].id
}

output "data_subnet_cidrs" {
  description = "Data subnet CIDRs, for the NetworkPolicy ipBlocks rendered into Helm values."
  value       = var.data_subnet_cidrs
}

output "streaming_subnet_cidrs" {
  description = "Streaming subnet CIDRs, for NetworkPolicy ipBlocks."
  value       = var.streaming_subnet_cidrs
}

output "egress_subnet_cidrs" {
  description = "Egress subnet CIDRs, for NetworkPolicy ipBlocks."
  value       = var.egress_subnet_cidrs
}

output "alb_security_group_id" {
  description = "ALB security group."
  value       = aws_security_group.alb.id
}

output "app_nodes_security_group_id" {
  description = "EKS node security group. Every datastore SG references this one."
  value       = aws_security_group.app_nodes.id
}

output "eks_control_plane_security_group_id" {
  description = "EKS cluster (control plane ENI) security group."
  value       = aws_security_group.eks_control_plane.id
}

output "aurora_security_group_id" {
  description = "Aurora security group."
  value       = aws_security_group.aurora.id
}

output "redis_security_group_id" {
  description = "ElastiCache security group."
  value       = aws_security_group.redis.id
}

output "msk_security_group_id" {
  description = "MSK security group."
  value       = aws_security_group.msk.id
}

output "vpc_endpoints_security_group_id" {
  description = "Interface endpoint security group."
  value       = aws_security_group.vpc_endpoints.id
}

output "egress_proxy_security_group_id" {
  description = "Egress proxy security group, bound to proxy pods via SecurityGroupPolicy."
  value       = aws_security_group.egress_proxy.id
}

output "nat_gateway_ids" {
  description = "NAT gateway IDs, in AZ order."
  value       = aws_nat_gateway.this[*].id
}

output "nat_public_ips" {
  description = "NAT egress IPs. Gateways and vendors allowlist these on their side, so they are part of the platform's external contract and must not change casually."
  value       = aws_eip.nat[*].public_ip
}

output "network_firewall_arn" {
  description = "Network Firewall ARN, or null when the firewall is disabled."
  value       = local.firewall_enabled ? aws_networkfirewall_firewall.this[0].arn : null
}

output "s3_prefix_list_id" {
  description = "Managed prefix list of the S3 gateway endpoint, for SG rules elsewhere."
  value       = aws_vpc_endpoint.s3.prefix_list_id
}

output "interface_endpoint_dns_names" {
  description = "service => the endpoint's primary private DNS entry. Useful in runbooks when private DNS resolution is in question."
  value       = { for k, v in aws_vpc_endpoint.interface : k => tolist(v.dns_entry)[0].dns_name }
}
