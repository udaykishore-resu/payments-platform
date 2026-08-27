###############################################################################
# Remote state
#
# The state bucket and the lock table are created by the bootstrap stack, NOT by
# this stack - a stack cannot store its own state in a bucket it is in the
# middle of creating. See terraform/README.md section 1 for the bootstrap
# procedure and for why the bootstrap stack's own state is committed to git.
#
# Every field here matters:
#
#   bucket          Versioned, SSE-KMS, Object Lock in GOVERNANCE mode, public
#                   access blocked, and MFA-delete on. The state file contains
#                   every resource ID in the estate and, despite our best
#                   efforts, occasionally a value that should not be there;
#                   treat it as a restricted-classification object.
#   key             Per environment AND per region-role. Two stacks sharing a
#                   key is how one team's apply silently reverts another's.
#   dynamodb_table  State locking. Without it, two concurrent applies interleave
#                   writes and produce a state file describing infrastructure
#                   that never existed.
#   encrypt         Belt and braces: the bucket enforces SSE-KMS by policy, and
#                   this makes the client send it explicitly, so a policy
#                   regression fails the write rather than storing plaintext.
#   kms_key_id      The state CMK, separate from every workload key so that
#                   "who can read the state" is a distinct question from "who
#                   can read the database".
#
# Values are supplied by `terraform init -backend-config=backend.hcl` in CI so
# the account ID is not committed; the block below carries the shape and the
# non-secret parts.
###############################################################################

terraform {
  backend "s3" {
    bucket = "pp-terraform-state-prod"
    key    = "prod/eu-west-1/platform.tfstate"
    region = "eu-west-1"

    dynamodb_table = "pp-terraform-locks"
    encrypt        = true
    kms_key_id     = "alias/pp-terraform-state"

    # The role CI assumes to touch state. Separate from the role that creates
    # infrastructure, so a compromised deploy role cannot rewrite history.
    # role_arn = "arn:aws:iam::<prod-account>:role/pp-terraform-state-prod"

    # Refuse to write a state file whose lineage does not match. Catches the
    # case where two stacks were pointed at one key.
    workspace_key_prefix = "workspaces"
  }
}
