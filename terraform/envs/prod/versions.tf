###############################################################################
# Copied from terraform/versions.tf by `make sync-versions`. Do not edit here.
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
