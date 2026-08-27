output "alb_arn" {
  description = "Public ALB ARN."
  value       = aws_lb.public.arn
}

output "alb_dns_name" {
  description = "ALB DNS name. Route 53 aliases to this; merchants never resolve it directly."
  value       = aws_lb.public.dns_name
}

output "alb_zone_id" {
  description = "ALB hosted-zone ID, required for an alias record."
  value       = aws_lb.public.zone_id
}

output "alb_arn_suffix" {
  description = "ALB ARN suffix, the dimension CloudWatch uses for ELB metrics."
  value       = aws_lb.public.arn_suffix
}

output "api_target_group_arn" {
  description = "Target group the AWS Load Balancer Controller binds pods into via TargetGroupBinding."
  value       = aws_lb_target_group.api.arn
}

output "nlb_arn" {
  description = "Internal NLB ARN, or null when disabled."
  value       = var.enable_nlb ? aws_lb.internal[0].arn : null
}

output "nlb_dns_name" {
  description = "Internal NLB DNS name."
  value       = var.enable_nlb ? aws_lb.internal[0].dns_name : null
}

output "grpc_target_group_arn" {
  description = "gRPC target group ARN."
  value       = var.enable_nlb ? aws_lb_target_group.grpc[0].arn : null
}

output "certificate_arn" {
  description = "Validated RSA certificate ARN."
  value       = aws_acm_certificate_validation.rsa.certificate_arn
}

output "web_acl_arn" {
  description = "WAF web ACL ARN."
  value       = aws_wafv2_web_acl.this.arn
}

output "web_acl_name" {
  description = "WAF web ACL name, the CloudWatch dimension for its rule metrics."
  value       = aws_wafv2_web_acl.this.name
}

output "shield_protected" {
  description = "Whether Shield Advanced per-resource protection is registered for the ALB."
  value       = var.enable_shield_advanced
}
