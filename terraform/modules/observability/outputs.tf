output "prometheus_workspace_arn" {
  description = "AMP workspace ARN, granted to the Prometheus agent's IRSA role."
  value       = var.create_prometheus_workspace ? aws_prometheus_workspace.this[0].arn : null
}

output "prometheus_workspace_id" {
  description = "AMP workspace ID."
  value       = var.create_prometheus_workspace ? aws_prometheus_workspace.this[0].id : null
}

output "prometheus_remote_write_url" {
  description = "remote_write endpoint for the in-cluster Prometheus agent."
  value       = var.create_prometheus_workspace ? "${aws_prometheus_workspace.this[0].prometheus_endpoint}api/v1/remote_write" : null
}

output "prometheus_query_url" {
  description = "Query endpoint, used by the Argo Rollouts AnalysisTemplates."
  value       = var.create_prometheus_workspace ? "${aws_prometheus_workspace.this[0].prometheus_endpoint}api/v1/query" : null
}

output "grafana_endpoint" {
  description = "Managed Grafana workspace URL."
  value       = var.create_grafana_workspace ? aws_grafana_workspace.this[0].endpoint : null
}

output "alerts_topic_arn" {
  description = "SNS topic every alarm publishes to."
  value       = aws_sns_topic.alerts.arn
}

output "log_group_names" {
  description = "Names of the log groups this module created."
  value       = [for k, v in aws_cloudwatch_log_group.this : v.name]
}
