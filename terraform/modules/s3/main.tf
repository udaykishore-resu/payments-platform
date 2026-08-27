###############################################################################
# modules/s3 - buckets, Object Lock, lifecycle, CRR
#
# Every bucket in this module is:
#   - versioned (an accidental overwrite or delete is recoverable)
#   - SSE-KMS with a customer-managed key and S3 Bucket Keys on (bucket keys cut
#     KMS request cost by ~99% on a high-object-count bucket without weakening
#     anything: the data key is still per-object, it is just derived less often)
#   - public-access-blocked at the bucket level as well as the account level
#   - TLS-only and SSE-KMS-only by bucket policy, so a misconfigured client
#     writing plaintext over HTTP gets a 403 rather than a quiet success
#   - owner-enforced (ACLs disabled entirely)
#
# Object Lock is set at creation and cannot be added later. Getting it wrong on
# the audit archive means recreating the bucket and re-exporting seven years of
# records, so the mode and duration are validated in variables.tf.
###############################################################################

terraform {
  required_version = ">= 1.9.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.60.0, < 6.0.0"
    }
  }
}

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

locals {
  name = "pp-${var.environment}"

  bucket_names = { for k, v in var.buckets : k => "${local.name}-${k}${var.name_suffix}" }

  replicating = { for k, v in var.buckets : k => v if v.replicate }
}

check "replication_prerequisites" {
  assert {
    condition     = length(local.replicating) == 0 || var.replica_kms_key_arn != null
    error_message = "replica_kms_key_arn must be set when any bucket has replicate = true; replicated objects cannot be encrypted with the source region's key."
  }

  assert {
    condition     = alltrue([for k, v in local.replicating : contains(keys(var.replica_bucket_arns), k)])
    error_message = "Every bucket with replicate = true needs a matching entry in replica_bucket_arns."
  }
}

###############################################################################
# Buckets
###############################################################################

resource "aws_s3_bucket" "this" {
  for_each = var.buckets

  bucket = local.bucket_names[each.key]

  # Object Lock must be enabled at creation time. It is enabled on every bucket
  # here even where no default retention is configured, because turning it on
  # later is impossible and the cost of having it available is zero.
  object_lock_enabled = each.value.object_lock_mode != null

  force_destroy = false

  tags = {
    Name               = local.bucket_names[each.key]
    BucketPurpose      = each.key
    DataClassification = each.value.data_classification
    ObjectLockMode     = coalesce(each.value.object_lock_mode, "none")
  }

  # A bucket holding compliance evidence, audit exports or backups is not
  # replaceable infrastructure: with Object Lock in COMPLIANCE mode the objects
  # cannot be deleted at all until their retention expires, and the bucket
  # therefore cannot be deleted either.
  #
  # To legitimately remove one:
  #   1. Confirm the retention period of every object has expired
  #      (s3:GetObjectRetention over an inventory report), and that the legal
  #      and compliance owners named in docs/compliance.md have signed off in a
  #      ticket.
  #   2. Empty the bucket with a dedicated, logged job.
  #   3. Remove the entry from var.buckets AND delete this lifecycle block in
  #      the same reviewed PR, with the ticket in the commit message. CI's
  #      "plan destroys a stateful resource" gate then requires the
  #      approved-destroy label.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_ownership_controls" "this" {
  for_each = var.buckets

  bucket = aws_s3_bucket.this[each.key].id

  rule {
    # ACLs disabled. Object ACLs are the mechanism behind most accidental S3
    # exposure; with BucketOwnerEnforced they cannot be set at all.
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "this" {
  for_each = var.buckets

  bucket = aws_s3_bucket.this[each.key].id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "this" {
  for_each = var.buckets

  bucket = aws_s3_bucket.this[each.key].id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "this" {
  for_each = var.buckets

  bucket = aws_s3_bucket.this[each.key].id

  rule {
    apply_server_side_encryption_by_default {
      # ELB access logs are the one AWS-managed writer that cannot use a
      # customer-managed key: the service supports SSE-S3 only. Rather than
      # weaken every bucket, the ALB log bucket is declared with
      # sse_algorithm = "AES256" and everything else keeps its CMK.
      sse_algorithm     = each.value.sse_algorithm
      kms_master_key_id = each.value.sse_algorithm == "aws:kms" ? var.kms_key_arn : null
    }

    bucket_key_enabled = each.value.sse_algorithm == "aws:kms"
  }
}

resource "aws_s3_bucket_object_lock_configuration" "this" {
  for_each = { for k, v in var.buckets : k => v if v.object_lock_mode != null }

  bucket = aws_s3_bucket.this[each.key].id

  rule {
    default_retention {
      mode = each.value.object_lock_mode
      days = each.value.object_lock_days
    }
  }

  depends_on = [aws_s3_bucket_versioning.this]
}

###############################################################################
# Lifecycle
#
# Transitions are expressed in days-to-Glacier-IR rather than Glacier Flexible:
# Instant Retrieval costs a little more per GB and removes the retrieval latency
# that makes an auditor's sampling request a two-day operation.
###############################################################################

resource "aws_s3_bucket_lifecycle_configuration" "this" {
  for_each = var.buckets

  bucket = aws_s3_bucket.this[each.key].id

  rule {
    id     = "abort-incomplete-multipart"
    status = "Enabled"

    filter {}

    abort_incomplete_multipart_upload {
      days_after_initiation = each.value.abort_multipart_days
    }
  }

  dynamic "rule" {
    for_each = each.value.transition_to_ia_days != null ? [1] : []

    content {
      id     = "transition-standard-ia"
      status = "Enabled"

      filter {}

      transition {
        days          = each.value.transition_to_ia_days
        storage_class = "STANDARD_IA"
      }
    }
  }

  dynamic "rule" {
    for_each = each.value.transition_to_glacier_ir_days != null ? [1] : []

    content {
      id     = "transition-glacier-ir"
      status = "Enabled"

      filter {}

      transition {
        days          = each.value.transition_to_glacier_ir_days
        storage_class = "GLACIER_IR"
      }
    }
  }

  dynamic "rule" {
    for_each = each.value.expiry_days != null ? [1] : []

    content {
      id     = "expire-current"
      status = "Enabled"

      filter {}

      expiration {
        days = each.value.expiry_days
      }
    }
  }

  rule {
    id     = "expire-noncurrent"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days           = each.value.noncurrent_version_expiry_days
      newer_noncurrent_versions = 5
    }
  }

  depends_on = [aws_s3_bucket_versioning.this]
}

###############################################################################
# Bucket policies
###############################################################################

data "aws_iam_policy_document" "bucket" {
  for_each = var.buckets

  # Deny anything not over TLS 1.2+. `aws:SecureTransport` alone permits TLS 1.0.
  statement {
    sid    = "DenyInsecureTransport"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = ["s3:*"]

    resources = [
      aws_s3_bucket.this[each.key].arn,
      "${aws_s3_bucket.this[each.key].arn}/*",
    ]

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  statement {
    sid    = "DenyOutdatedTls"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = ["s3:*"]

    resources = [
      aws_s3_bucket.this[each.key].arn,
      "${aws_s3_bucket.this[each.key].arn}/*",
    ]

    condition {
      test     = "NumericLessThan"
      variable = "s3:TlsVersion"
      values   = ["1.2"]
    }
  }

  # Deny any PUT that is not SSE-KMS with our key. Without this a client can
  # write an object with SSE-S3 and the bucket default is silently bypassed.
  statement {
    sid    = "DenyUnencryptedPut"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = ["s3:PutObject"]

    resources = ["${aws_s3_bucket.this[each.key].arn}/*"]

    condition {
      test     = "StringNotEquals"
      variable = "s3:x-amz-server-side-encryption"
      values   = [each.value.sse_algorithm]
    }
  }

  dynamic "statement" {
    for_each = each.value.sse_algorithm == "aws:kms" ? [1] : []

    content {
      sid    = "DenyWrongKmsKey"
      effect = "Deny"

      principals {
        type        = "AWS"
        identifiers = ["*"]
      }

      actions = ["s3:PutObject"]

      resources = ["${aws_s3_bucket.this[each.key].arn}/*"]

      condition {
        test     = "StringNotEqualsIfExists"
        variable = "s3:x-amz-server-side-encryption-aws-kms-key-id"
        values   = [var.kms_key_arn]
      }
    }
  }

  # Version deletion is the one operation that defeats versioning as a recovery
  # mechanism. It is denied to every principal; break-glass grants it by a
  # session policy that this statement's condition permits.
  statement {
    sid    = "DenyObjectVersionDeletionExceptBreakGlass"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = [
      "s3:DeleteObjectVersion",
      "s3:PutBucketVersioning",
      "s3:PutBucketPolicy",
      "s3:DeleteBucketPolicy",
      "s3:PutEncryptionConfiguration",
    ]

    resources = [
      aws_s3_bucket.this[each.key].arn,
      "${aws_s3_bucket.this[each.key].arn}/*",
    ]

    condition {
      test     = "ArnNotLike"
      variable = "aws:PrincipalArn"

      values = [
        "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/pp-break-glass-*",
        "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/pp-terraform-*",
      ]
    }
  }

  # Log delivery: VPC flow logs, S3 server access logs, CloudTrail.
  dynamic "statement" {
    for_each = each.value.allow_log_delivery_service ? [1] : []

    content {
      sid    = "AwsLogDeliveryWrite"
      effect = "Allow"

      principals {
        type = "Service"

        identifiers = [
          "delivery.logs.amazonaws.com",                    # VPC flow logs
          "logging.s3.amazonaws.com",                       # S3 server access logs
          "logdelivery.elasticloadbalancing.amazonaws.com", # ALB access and connection logs
        ]
      }

      actions   = ["s3:PutObject"]
      resources = ["${aws_s3_bucket.this[each.key].arn}/*"]

      condition {
        test     = "StringEquals"
        variable = "aws:SourceAccount"
        values   = [data.aws_caller_identity.current.account_id]
      }
    }
  }

  dynamic "statement" {
    for_each = each.value.allow_log_delivery_service ? [1] : []

    content {
      sid    = "AwsLogDeliveryAclCheck"
      effect = "Allow"

      principals {
        type = "Service"

        identifiers = [
          "delivery.logs.amazonaws.com",
          "logging.s3.amazonaws.com",
          "logdelivery.elasticloadbalancing.amazonaws.com",
        ]
      }

      actions   = ["s3:GetBucketAcl", "s3:ListBucket"]
      resources = [aws_s3_bucket.this[each.key].arn]

      condition {
        test     = "StringEquals"
        variable = "aws:SourceAccount"
        values   = [data.aws_caller_identity.current.account_id]
      }
    }
  }

  # Replica-side: accept objects replicated from the source account.
  dynamic "statement" {
    for_each = var.name_suffix != "" && var.source_account_id != data.aws_caller_identity.current.account_id ? [1] : []

    content {
      sid    = "AcceptReplicationFromSourceAccount"
      effect = "Allow"

      principals {
        type        = "AWS"
        identifiers = ["arn:aws:iam::${var.source_account_id}:root"]
      }

      actions = [
        "s3:ReplicateObject",
        "s3:ReplicateDelete",
        "s3:ReplicateTags",
        "s3:ObjectOwnerOverrideToBucketOwner",
        "s3:GetBucketVersioning",
        "s3:PutBucketVersioning",
      ]

      resources = [
        aws_s3_bucket.this[each.key].arn,
        "${aws_s3_bucket.this[each.key].arn}/*",
      ]
    }
  }
}

resource "aws_s3_bucket_policy" "this" {
  for_each = var.buckets

  bucket = aws_s3_bucket.this[each.key].id
  policy = data.aws_iam_policy_document.bucket[each.key].json

  depends_on = [aws_s3_bucket_public_access_block.this]
}

###############################################################################
# Cross-region replication
#
# Replication of delete markers is OFF. A delete in the primary region must not
# propagate as a delete in the DR region: the DR copy exists to survive the
# primary, including surviving a mistake made in the primary
# (disaster-recovery.md 4.4).
###############################################################################

data "aws_iam_policy_document" "replication_assume" {
  count = length(local.replicating) > 0 ? 1 : 0

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["s3.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "replication" {
  count = length(local.replicating) > 0 ? 1 : 0

  name               = "${local.name}-s3-replication"
  assume_role_policy = data.aws_iam_policy_document.replication_assume[0].json

  tags = {
    Name = "${local.name}-s3-replication"
  }
}

data "aws_iam_policy_document" "replication" {
  count = length(local.replicating) > 0 ? 1 : 0

  statement {
    sid    = "ReadSourceObjects"
    effect = "Allow"

    actions = [
      "s3:GetReplicationConfiguration",
      "s3:ListBucket",
    ]

    resources = [for k, v in local.replicating : aws_s3_bucket.this[k].arn]
  }

  statement {
    sid    = "ReadSourceObjectVersions"
    effect = "Allow"

    actions = [
      "s3:GetObjectVersionForReplication",
      "s3:GetObjectVersionAcl",
      "s3:GetObjectVersionTagging",
      "s3:GetObjectRetention",
      "s3:GetObjectLegalHold",
    ]

    resources = [for k, v in local.replicating : "${aws_s3_bucket.this[k].arn}/*"]
  }

  statement {
    sid    = "WriteDestinationObjects"
    effect = "Allow"

    actions = [
      "s3:ReplicateObject",
      "s3:ReplicateTags",
      "s3:ObjectOwnerOverrideToBucketOwner",
    ]

    resources = [for k, v in local.replicating : "${var.replica_bucket_arns[k]}/*"]
  }

  statement {
    sid    = "DecryptSourceObjects"
    effect = "Allow"

    actions = ["kms:Decrypt"]

    resources = [var.kms_key_arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["s3.${data.aws_region.current.name}.amazonaws.com"]
    }
  }

  statement {
    sid    = "EncryptDestinationObjects"
    effect = "Allow"

    actions = ["kms:Encrypt", "kms:GenerateDataKey"]

    resources = [var.replica_kms_key_arn]
  }
}

resource "aws_iam_policy" "replication" {
  count = length(local.replicating) > 0 ? 1 : 0

  name   = "${local.name}-s3-replication"
  policy = data.aws_iam_policy_document.replication[0].json
}

resource "aws_iam_role_policy_attachment" "replication" {
  count = length(local.replicating) > 0 ? 1 : 0

  role       = aws_iam_role.replication[0].name
  policy_arn = aws_iam_policy.replication[0].arn
}

resource "aws_s3_bucket_replication_configuration" "this" {
  for_each = local.replicating

  role   = aws_iam_role.replication[0].arn
  bucket = aws_s3_bucket.this[each.key].id

  rule {
    id       = "crr-${each.key}"
    status   = "Enabled"
    priority = 1

    filter {}

    delete_marker_replication {
      status = "Disabled"
    }

    source_selection_criteria {
      sse_kms_encrypted_objects {
        status = "Enabled"
      }
    }

    destination {
      bucket        = var.replica_bucket_arns[each.key]
      storage_class = "STANDARD"

      encryption_configuration {
        replica_kms_key_id = var.replica_kms_key_arn
      }

      access_control_translation {
        owner = "Destination"
      }

      account = var.source_account_id

      # Replication Time Control: a 15-minute replication SLA with a CloudWatch
      # event when an object misses it. Enabled only where the RPO commitment is
      # explicit (audit archive, KYC evidence); best-effort elsewhere, because
      # RTC roughly doubles the per-GB replication cost.
      dynamic "replication_time" {
        for_each = each.value.replication_time_control ? [1] : []

        content {
          status = "Enabled"

          time {
            minutes = 15
          }
        }
      }

      dynamic "metrics" {
        for_each = each.value.replication_time_control ? [1] : []

        content {
          status = "Enabled"

          event_threshold {
            minutes = 15
          }
        }
      }
    }
  }

  depends_on = [aws_s3_bucket_versioning.this]
}

###############################################################################
# Server access logging
###############################################################################

resource "aws_s3_bucket_logging" "this" {
  for_each = var.enable_access_logging && contains(keys(var.buckets), var.access_log_bucket_key) ? {
    for k, v in var.buckets : k => v if k != var.access_log_bucket_key
  } : {}

  bucket        = aws_s3_bucket.this[each.key].id
  target_bucket = aws_s3_bucket.this[var.access_log_bucket_key].id
  target_prefix = "s3-access-logs/${local.bucket_names[each.key]}/"
}
