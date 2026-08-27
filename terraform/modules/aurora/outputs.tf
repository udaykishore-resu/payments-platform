output "cluster_identifier" {
  description = "Cluster identifier."
  value       = aws_rds_cluster.this.cluster_identifier
}

output "cluster_arn" {
  description = "Cluster ARN. IAM database-auth policies scope to the cluster resource ID, not this ARN - see cluster_resource_id."
  value       = aws_rds_cluster.this.arn
}

output "cluster_resource_id" {
  description = "The immutable cluster resource ID (cluster-XXXX). This, not the identifier, is what an rds-db:connect policy must reference: renaming the cluster must not silently widen or void the grant."
  value       = aws_rds_cluster.this.cluster_resource_id
}

output "writer_endpoint" {
  description = "Cluster writer endpoint. Follows the writer through an AZ failover."
  value       = aws_rds_cluster.this.endpoint
}

output "reader_endpoint" {
  description = "Cluster reader endpoint, round-robin across healthy readers."
  value       = aws_rds_cluster.this.reader_endpoint
}

output "port" {
  description = "Database port."
  value       = aws_rds_cluster.this.port
}

output "database_name" {
  description = "Initial database name."
  value       = aws_rds_cluster.this.database_name
}

output "security_group_ids" {
  description = "Security groups attached to the cluster."
  value       = aws_rds_cluster.this.vpc_security_group_ids
}

output "cluster_parameter_group_name" {
  description = "Cluster parameter group name, so a runbook can diff the running parameters against the declared ones."
  value       = aws_rds_cluster_parameter_group.this.name
}

output "instance_identifiers" {
  description = "All instance identifiers, writer first."
  value       = concat(aws_rds_cluster_instance.writer[*].identifier, aws_rds_cluster_instance.reader[*].identifier)
}
