output "secret_arns" {
  description = "key => secret ARN. Not the values - this module never reads a value."
  value       = { for k, v in aws_secretsmanager_secret.this : k => v.arn }
}

output "secret_names" {
  description = "key => full secret path."
  value       = { for k, v in aws_secretsmanager_secret.this : k => v.name }
}

output "path_prefix" {
  description = "The environment's secret path prefix, for the IRSA policies that scope by it."
  value       = var.path_prefix
}

output "rotation_lambda_arns" {
  description = "key => rotation lambda ARN."
  value       = { for k, v in aws_lambda_function.rotation : k => v.arn }
}

output "rotation_role_arns" {
  description = "key => rotation lambda role ARN, for the KMS key policy's user list."
  value       = { for k, v in aws_iam_role.rotation : k => v.arn }
}

output "replica_regions" {
  description = "Regions carrying replica secrets."
  value       = [for r in var.replica_regions : r.region]
}
