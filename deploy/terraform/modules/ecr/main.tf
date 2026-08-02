# ECR repository module — image scanning on push and lifecycle policy to bound storage cost while
# retaining enough history for rollback (docs/08-runbook.md section 8).

variable "repository_name" {
  type = string
}

resource "aws_ecr_repository" "this" {
  name                 = var.repository_name
  image_tag_mutability = "IMMUTABLE" # a tag, once pushed, can never point at a different image — required for reliable rollback

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "KMS"
  }
}

resource "aws_ecr_lifecycle_policy" "this" {
  repository = aws_ecr_repository.this.name
  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Keep the most recent 30 tagged images (covers several weeks of rollback history)"
        selection = {
          tagStatus   = "tagged"
          tagPrefixList = ["prod-", "staging-", "dev-"]
          countType   = "imageCountMoreThan"
          countNumber = 30
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "Expire untagged images after 7 days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 7
        }
        action = { type = "expire" }
      }
    ]
  })
}

output "repository_url" {
  value = aws_ecr_repository.this.repository_url
}
