output "fencing_table_name" {
  description = "DR control table name. The promotion runbook's aws dynamodb update-item uses this."
  value       = aws_dynamodb_table.fence.name
}

output "fencing_table_arn" {
  description = "DR control table ARN, granted read-only to payment-api and payment-orchestrator."
  value       = aws_dynamodb_table.fence.arn
}

output "fencing_table_stream_arn" {
  description = "Stream ARN, subscribed by the SIEM so an epoch change is an audited event."
  value       = aws_dynamodb_table.fence.stream_arn
}

output "backup_vault_arn" {
  description = "Local backup vault ARN."
  value       = var.create_backup_vault ? aws_backup_vault.this[0].arn : null
}

output "backup_plan_id" {
  description = "Backup plan ID."
  value       = var.create_backup_vault ? aws_backup_plan.this[0].id : null
}

output "backup_role_arn" {
  description = "The role AWS Backup assumes."
  value       = var.create_backup_vault ? aws_iam_role.backup[0].arn : null
}
