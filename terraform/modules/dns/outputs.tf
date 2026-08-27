output "zone_id" {
  description = "Hosted zone ID."
  value       = local.zone_id
}

output "zone_arn" {
  description = "Hosted zone ARN, for the ExternalDNS write grant."
  value       = "arn:aws:route53:::hostedzone/${local.zone_id}"
}

output "name_servers" {
  description = "Delegation set. These go to the registrar; a mismatch here is a total outage."
  value       = var.create_zone ? aws_route53_zone.this[0].name_servers : []
}

output "api_fqdn" {
  description = "The API hostname the failover record answers for."
  value       = var.api_hostname
}

output "health_check_ids" {
  description = "region key => Route 53 health check ID, referenced by the DR runbook."
  value       = { for k, v in aws_route53_health_check.region : k => v.id }
}
