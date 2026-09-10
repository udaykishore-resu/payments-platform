#!/usr/bin/env bash
#
# deploy/localstack/init/10-platform.sh — LocalStack ready hook for the local dev stack.
#
# Runs inside the pp-dev-localstack container once the emulator reports healthy (mounted at
# /etc/localstack/init/ready.d by deploy/docker-compose.dev.yml). It creates the *static* AWS
# resources the platform expects to exist: the DR-drill evidence bucket and the KMS key the drill
# encrypts with. It does not create secrets — their names come from `platformctl seed`, which has
# not run yet at this point, so scripts/dev-up.sh writes those after seeding.
#
# Everything here is idempotent: LocalStack restarts re-run ready hooks, and a hook that fails on
# "already exists" would mark the container unhealthy on the second start.
#
# Nothing here is a credential. `test`/`test` are the well-known LocalStack placeholder keys and
# are rejected by every real AWS endpoint.

set -euo pipefail

export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"

EVIDENCE_BUCKET="${PP_EVIDENCE_BUCKET:-pp-dev-dr-evidence}"
KMS_ALIAS="alias/pp-dev-drill"

log() { printf '[pp-init] %s\n' "$*"; }

# --- S3: the DR-drill evidence bucket -----------------------------------------------------------
if awslocal s3api head-bucket --bucket "$EVIDENCE_BUCKET" >/dev/null 2>&1; then
  log "bucket s3://${EVIDENCE_BUCKET} already exists"
else
  awslocal s3api create-bucket --bucket "$EVIDENCE_BUCKET" >/dev/null
  # Versioning on, as the production evidence bucket has it: a drill report that can be
  # overwritten is not evidence.
  awslocal s3api put-bucket-versioning --bucket "$EVIDENCE_BUCKET" \
    --versioning-configuration Status=Enabled >/dev/null
  log "created s3://${EVIDENCE_BUCKET} (versioned)"
fi

# --- KMS: the drill key -------------------------------------------------------------------------
if awslocal kms describe-key --key-id "$KMS_ALIAS" >/dev/null 2>&1; then
  log "kms key ${KMS_ALIAS} already exists"
else
  KEY_ID="$(awslocal kms create-key \
    --description 'payments-platform local dev — DR drill encryption key (not a real key)' \
    --query 'KeyMetadata.KeyId' --output text)"
  awslocal kms create-alias --alias-name "$KMS_ALIAS" --target-key-id "$KEY_ID"
  log "created kms key ${KEY_ID} as ${KMS_ALIAS}"
fi

log "ready"
