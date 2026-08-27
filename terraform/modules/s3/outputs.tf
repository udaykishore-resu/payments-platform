output "bucket_ids" {
  description = "purpose => bucket name."
  value       = { for k, v in aws_s3_bucket.this : k => v.id }
}

output "bucket_arns" {
  description = "purpose => bucket ARN. IRSA policies scope to these plus a prefix, never to s3:::*."
  value       = { for k, v in aws_s3_bucket.this : k => v.arn }
}

output "bucket_regional_domain_names" {
  description = "purpose => regional domain name, for presigned-URL construction that must not depend on the global endpoint."
  value       = { for k, v in aws_s3_bucket.this : k => v.bucket_regional_domain_name }
}

output "replication_role_arn" {
  description = "The S3 replication role, or null when nothing replicates from this instance."
  value       = length(local.replicating) > 0 ? aws_iam_role.replication[0].arn : null
}
