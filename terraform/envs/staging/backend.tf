###############################################################################
# Remote state - staging.
#
# Same bucket family, different account and different key. Sharing a state
# bucket across accounts would mean the staging deploy role can read the prod
# state file, which lists every resource ID in production.
# See terraform/README.md section 1.
###############################################################################

terraform {
  backend "s3" {
    bucket = "pp-terraform-state-staging"
    key    = "staging/eu-west-1/platform.tfstate"
    region = "eu-west-1"

    dynamodb_table = "pp-terraform-locks"
    encrypt        = true
    kms_key_id     = "alias/pp-terraform-state"

    # role_arn = "arn:aws:iam::<staging-account>:role/pp-terraform-state-staging"

    workspace_key_prefix = "workspaces"
  }
}
