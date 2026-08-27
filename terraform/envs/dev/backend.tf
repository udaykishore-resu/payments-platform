###############################################################################
# Remote state - dev.
#
# Dev's state is still versioned, encrypted and locked. The temptation to keep
# a dev state file locally is what produces the "it works on my laptop and
# nowhere else" class of infrastructure incident, and it also means the state -
# which lists every resource ID in the account - is sitting in a working
# directory somewhere.
# See terraform/README.md section 1.
###############################################################################

terraform {
  backend "s3" {
    bucket = "pp-terraform-state-dev"
    key    = "dev/eu-west-1/platform.tfstate"
    region = "eu-west-1"

    dynamodb_table = "pp-terraform-locks"
    encrypt        = true
    kms_key_id     = "alias/pp-terraform-state"

    # role_arn = "arn:aws:iam::<dev-account>:role/pp-terraform-state-dev"

    workspace_key_prefix = "workspaces"
  }
}
