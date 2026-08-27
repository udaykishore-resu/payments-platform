output "replication_group_id" {
  description = "Replication group ID."
  value       = aws_elasticache_replication_group.this.id
}

output "arn" {
  description = "Replication group ARN."
  value       = aws_elasticache_replication_group.this.arn
}

output "primary_endpoint_address" {
  description = "Primary endpoint (cluster mode disabled). Empty when cluster mode is enabled."
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "reader_endpoint_address" {
  description = "Reader endpoint (cluster mode disabled)."
  value       = aws_elasticache_replication_group.this.reader_endpoint_address
}

output "configuration_endpoint_address" {
  description = "Configuration endpoint (cluster mode enabled). This is what the client library needs for slot discovery."
  value       = aws_elasticache_replication_group.this.configuration_endpoint_address
}

output "port" {
  description = "Redis port."
  value       = aws_elasticache_replication_group.this.port
}

output "tls_enabled" {
  description = "Always true. Exposed so the Helm values can assert it rather than assume it."
  value       = aws_elasticache_replication_group.this.transit_encryption_enabled
}
