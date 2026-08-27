# EKS managed node group module — spread across >= 3 AZs, sized from the capacity plan in
# docs/07-reliability-slo.md. Uses a managed node group (not self-managed EC2 + ASG hand-rolled)
# so node provisioning/draining/AMI patching is offloaded to AWS, per ADR-005.

variable "cluster_name" {
  type = string
}

variable "subnet_ids" {
  description = "Private subnet IDs, one or more per AZ, across >= 3 AZs."
  type        = list(string)
}

variable "instance_types" {
  type    = list(string)
  default = ["m6i.large"]
}

variable "desired_size" {
  type    = number
  default = 6
}

variable "min_size" {
  type    = number
  default = 3 # never fewer AZs' worth of nodes than the PodDisruptionBudget assumes
}

variable "max_size" {
  type    = number
  default = 30
}

resource "aws_eks_node_group" "this" {
  cluster_name    = var.cluster_name
  node_group_name = "${var.cluster_name}-payments-api"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = var.subnet_ids

  instance_types = var.instance_types
  capacity_type  = "ON_DEMAND" # payments workloads are not spot-eligible: a spot reclamation mid-request-batch is an
                                # avoidable source of the exact failure classes docs/04-failure-recovery-design.md
                                # exists to minimize, not one worth taking on for the marginal cost saving here.

  scaling_config {
    desired_size = var.desired_size
    min_size     = var.min_size
    max_size     = var.max_size
  }

  update_config {
    max_unavailable_percentage = 25 # bounds how many nodes drain simultaneously during a managed node group upgrade
  }

  labels = {
    workload = "payments-api"
  }
}

resource "aws_iam_role" "node" {
  name = "${var.cluster_name}-payments-api-node-role"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "worker" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
}

resource "aws_iam_role_policy_attachment" "cni" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
}

resource "aws_iam_role_policy_attachment" "ecr_readonly" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
}

output "node_group_arn" {
  value = aws_eks_node_group.this.arn
}
