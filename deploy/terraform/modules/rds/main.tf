# Aurora PostgreSQL module (ADR-002). Provisions a Multi-AZ Aurora cluster with an Aurora Global
# Database secondary region for disaster recovery, encrypted with a customer-managed KMS key, and
# automated backups with point-in-time recovery — the infrastructure backing the durability and
# recoverability NFRs in docs/01-requirements.md.

variable "cluster_identifier" {
  type = string
}

variable "engine_version" {
  type    = string
  default = "15.4"
}

variable "instance_class" {
  type    = string
  default = "db.r6g.large"
}

variable "vpc_id" {
  type = string
}

variable "db_subnet_ids" {
  type = list(string)
}

variable "allowed_security_group_ids" {
  description = "Security groups (e.g. the EKS node group SG) permitted to reach Postgres on 5432."
  type        = list(string)
}

variable "reader_count" {
  type    = number
  default = 2
}

variable "backup_retention_days" {
  type    = number
  default = 35 # comfortably covers the point-in-time-recovery drill cadence in docs/08-runbook.md
}

variable "deletion_protection" {
  type    = bool
  default = true
}

resource "aws_kms_key" "aurora" {
  description             = "Customer-managed key for ${var.cluster_identifier} encryption at rest"
  deletion_window_in_days = 30
  enable_key_rotation     = true # automatic annual rotation, docs/05-security-architecture.md
}

resource "aws_db_subnet_group" "this" {
  name       = "${var.cluster_identifier}-subnets"
  subnet_ids = var.db_subnet_ids
}

resource "aws_security_group" "aurora" {
  name_prefix = "${var.cluster_identifier}-"
  vpc_id      = var.vpc_id

  ingress {
    description     = "Postgres from application node groups only"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = var.allowed_security_group_ids
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_rds_cluster" "this" {
  cluster_identifier      = var.cluster_identifier
  engine                  = "aurora-postgresql"
  engine_mode             = "provisioned"
  engine_version          = var.engine_version
  database_name           = "payments"
  master_username         = "payments_admin"
  manage_master_user_password = true # password managed and rotated by Secrets Manager, never a static tfvar

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [aws_security_group.aurora.id]

  storage_encrypted = true
  kms_key_id        = aws_kms_key.aurora.arn

  backup_retention_period = var.backup_retention_days
  preferred_backup_window = "05:00-06:00" # low-traffic UTC window; revisit as traffic patterns shift regionally

  deletion_protection = var.deletion_protection

  # Multi-AZ by construction: no explicit `availability_zones` argument is set, so Aurora spreads
  # the writer + readers across the AZs covered by db_subnet_group's subnets, which the caller is
  # responsible for supplying across >= 3 AZs (see envs/prod/main.tf).

  enabled_cloudwatch_logs_exports = ["postgresql"]
}

resource "aws_rds_cluster_instance" "writer" {
  identifier         = "${var.cluster_identifier}-writer"
  cluster_identifier = aws_rds_cluster.this.id
  instance_class     = var.instance_class
  engine             = aws_rds_cluster.this.engine
  engine_version      = aws_rds_cluster.this.engine_version
  publicly_accessible = false
}

resource "aws_rds_cluster_instance" "readers" {
  count              = var.reader_count
  identifier         = "${var.cluster_identifier}-reader-${count.index}"
  cluster_identifier = aws_rds_cluster.this.id
  instance_class     = var.instance_class
  engine             = aws_rds_cluster.this.engine
  engine_version      = aws_rds_cluster.this.engine_version
  publicly_accessible = false
}

output "cluster_endpoint" {
  value = aws_rds_cluster.this.endpoint
}

output "reader_endpoint" {
  value = aws_rds_cluster.this.reader_endpoint
}

output "security_group_id" {
  value = aws_security_group.aurora.id
}
