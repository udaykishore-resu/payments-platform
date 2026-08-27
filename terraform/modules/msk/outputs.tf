output "cluster_arn" {
  description = "MSK cluster ARN."
  value       = var.serverless ? aws_msk_serverless_cluster.this[0].arn : aws_msk_cluster.this[0].arn
}

output "cluster_name" {
  description = "MSK cluster name. IRSA policies build kafka-cluster:* resource ARNs from this."
  value       = var.cluster_name
}

output "bootstrap_brokers_sasl_iam" {
  description = "Bootstrap servers for SASL/IAM over TLS (port 9098). This is the value the Helm chart injects as KAFKA_BOOTSTRAP_SERVERS."
  value       = var.serverless ? aws_msk_serverless_cluster.this[0].bootstrap_brokers_sasl_iam : aws_msk_cluster.this[0].bootstrap_brokers_sasl_iam
}

output "bootstrap_brokers_sasl_scram" {
  description = "Bootstrap servers for SASL/SCRAM over TLS (port 9096), empty unless SCRAM is enabled."
  value       = var.serverless ? "" : aws_msk_cluster.this[0].bootstrap_brokers_sasl_scram
}

output "zookeeper_connect_string" {
  description = "ZooKeeper/KRaft connect string. Kept as an output for the admin runbooks only; no application uses it."
  value       = var.serverless ? "" : aws_msk_cluster.this[0].zookeeper_connect_string_tls
}

output "configuration_arn" {
  description = "Broker configuration ARN, so a drift check can compare the live revision to the declared one."
  value       = var.serverless ? null : aws_msk_configuration.this[0].arn
}

output "broker_log_group_name" {
  description = "CloudWatch log group receiving broker logs."
  value       = aws_cloudwatch_log_group.broker.name
}
