###############################################################################
# terraform/versions.tf
#
# The single source of truth for the Terraform core and provider versions used
# by every stack and every module in this tree.
#
# This file is NOT a root module. It is symlinked / copied into each stack under
# envs/<env>/versions.tf by `make sync-versions`, and each module carries its own
# `required_providers` block with the SAME constraints. Terraform requires the
# constraint to be declared in every module that uses a provider; keeping the
# canonical copy here means there is one place to bump.
#
# Pinning policy
# --------------
#   required_version   : a floor plus a hard major ceiling. A Terraform major
#                        release changes state format; it is never picked up
#                        implicitly.
#   provider versions  : `>= x.y.z, < X+1.0.0`. Patch and minor upgrades are
#                        allowed (they carry new AWS APIs we want), majors are
#                        not. The exact resolved versions are committed in
#                        .terraform.lock.hcl per stack, which is what actually
#                        makes a plan reproducible. `terraform init -upgrade`
#                        is a reviewed PR, never an ambient action.
#
# There is deliberately no `latest` anywhere in this repository, including in
# EKS add-on versions, AMIs and engine versions: see modules/eks/variables.tf.
###############################################################################

terraform {
  required_version = ">= 1.9.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.60.0, < 6.0.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.6.0, < 4.0.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = ">= 4.0.5, < 5.0.0"
    }
    null = {
      source  = "hashicorp/null"
      version = ">= 3.2.2, < 4.0.0"
    }
  }
}
