output "key_arns" {
  description = "purpose => CMK ARN. Consumed by every module that encrypts something."
  value = local.is_replica ? {
    for k, v in aws_kms_replica_key.this : k => v.arn
    } : {
    for k, v in aws_kms_key.this : k => v.arn
  }
}

output "key_ids" {
  description = "purpose => CMK key ID."
  value = local.is_replica ? {
    for k, v in aws_kms_replica_key.this : k => v.key_id
    } : {
    for k, v in aws_kms_key.this : k => v.key_id
  }
}

output "alias_names" {
  description = "purpose => alias name (alias/pp-<env>-<purpose>). Runbooks reference aliases, not key IDs."
  value = local.is_replica ? {
    for k, v in aws_kms_alias.replica : k => v.name
    } : {
    for k, v in aws_kms_alias.this : k => v.name
  }
}

output "purposes" {
  description = "The purposes managed by this module instance."
  value       = local.all_purposes
}

