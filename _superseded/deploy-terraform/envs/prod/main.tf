# Production environment composition root. Wires together the modules in ../../modules against
# an existing VPC/EKS cluster (referenced by data source / variable, not created here — cluster
# bootstrapping is a separate, lower-churn Terraform root intentionally not mixed with the
# frequently-iterated application infrastructure below).
#
# This file is deliberately a *reference composition*, not a `terraform apply`-ready root: values
# like vpc_id/subnet_ids/cluster_name are left as required variables with no defaults so a real
# deployment must supply them explicitly for the target AWS account, per the production checklist
# ("no manual console changes to prod" — everything here is meant to be reviewed as code before
# it touches an account).

terraform {
  required_version = ">= 1.7"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }

  # Remote state with locking — never local state for anything touching production.
  backend "s3" {
    bucket         = "payments-platform-tfstate-prod"
    key            = "payments-api/terraform.tfstate"
    region         = "us-east-1"
    dynamodb_table = "payments-platform-tfstate-lock"
    encrypt        = true
  }
}

provider "aws" {
  region = var.aws_region
}

variable "aws_region" {
  type    = string
  default = "us-east-1"
}

variable "vpc_id" {
  type = string
}

variable "db_subnet_ids" {
  type = list(string)
}

variable "eks_node_subnet_ids" {
  type = list(string)
}

variable "eks_cluster_name" {
  type = string
}

variable "eks_node_security_group_id" {
  type = string
}

module "aurora" {
  source = "../../modules/rds"

  cluster_identifier          = "payments-prod"
  vpc_id                      = var.vpc_id
  db_subnet_ids               = var.db_subnet_ids
  allowed_security_group_ids  = [var.eks_node_security_group_id]
  reader_count                = 2
  deletion_protection         = true
}

module "payment_events_queue" {
  source = "../../modules/sqs"

  queue_name = "payments-prod-payment-events"
}

module "ecr" {
  source = "../../modules/ecr"

  repository_name = "payments-api"
}

module "eks_nodegroup" {
  source = "../../modules/eks-nodegroup"

  cluster_name = var.eks_cluster_name
  subnet_ids   = var.eks_node_subnet_ids
  desired_size = 6
  min_size     = 3
  max_size     = 30
}

# IRSA role assumed by the payments-api ServiceAccount (see
# deploy/k8s/base/serviceaccount.yaml). Scoped to exactly the AWS APIs this service calls:
# SQS SendMessage on its one queue, nothing else — the least-privilege implementation of
# docs/05-security-architecture.md's "Service -> AWS APIs" row.
data "aws_iam_policy_document" "payments_api_irsa_trust" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = ["REPLACE_WITH_EKS_OIDC_PROVIDER_ARN"]
    }
    condition {
      test     = "StringEquals"
      variable = "REPLACE_WITH_EKS_OIDC_PROVIDER:sub"
      values   = ["system:serviceaccount:payments:payments-api"]
    }
  }
}

resource "aws_iam_role" "payments_api_irsa" {
  name               = "payments-api-irsa-role"
  assume_role_policy = data.aws_iam_policy_document.payments_api_irsa_trust.json
}

data "aws_iam_policy_document" "payments_api_permissions" {
  statement {
    effect    = "Allow"
    actions   = ["sqs:SendMessage"]
    resources = [module.payment_events_queue.queue_arn]
  }
  statement {
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = ["arn:aws:secretsmanager:${var.aws_region}:*:secret:prod/payments-api/*"]
  }
}

resource "aws_iam_role_policy" "payments_api_permissions" {
  name   = "payments-api-permissions"
  role   = aws_iam_role.payments_api_irsa.id
  policy = data.aws_iam_policy_document.payments_api_permissions.json
}

output "aurora_writer_endpoint" {
  value = module.aurora.cluster_endpoint
}

output "payment_events_queue_url" {
  value = module.payment_events_queue.queue_url
}

output "ecr_repository_url" {
  value = module.ecr.repository_url
}
