###############################################################################
# modules/network/security-groups.tf
#
# Rule: intra-VPC rules reference a security group ID, never a CIDR
# (security.md 2.2). SG-to-SG references survive subnet growth and IP churn; a
# CIDR rule silently widens the moment somebody adds a subnet to the tier.
#
# The only CIDR-based rules in this file are the ones that genuinely have no SG
# on the other end: the ALB's ingress from the internet, and the pod tier's
# egress to the AWS service prefix lists.
#
# Every rule is a separate aws_vpc_security_group_*_rule resource rather than an
# inline block, so that a plan shows exactly which rule changed rather than
# "security group replaced".
###############################################################################

# --- Application Load Balancer -------------------------------------------------
resource "aws_security_group" "alb" {
  name        = "${local.name}-alb"
  description = "Public ALB. The only security group in this VPC with ingress from the internet."
  vpc_id      = aws_vpc.this.id

  tags = {
    Name = "${local.name}-alb"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "alb_https" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from the internet. In prod the ALB sits behind CloudFront and is additionally gated by the origin secret header and the CloudFront managed prefix list (security.md 2.1)."
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

resource "aws_vpc_security_group_ingress_rule" "alb_http_redirect" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTP, answered only with a 301 to HTTPS. Kept open so a plaintext request is redirected rather than timing out, which is a materially better client experience and leaks nothing."
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 80
  to_port           = 80
}

resource "aws_vpc_security_group_egress_rule" "alb_to_nodes" {
  security_group_id            = aws_security_group.alb.id
  description                  = "ALB to the ingress controller on the nodes."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 1025
  to_port                      = 65535
}

# --- EKS nodes ----------------------------------------------------------------
resource "aws_security_group" "app_nodes" {
  name        = "${local.name}-app-nodes"
  description = "EKS worker nodes and the pods that share their ENIs."
  vpc_id      = aws_vpc.this.id

  tags = {
    Name = "${local.name}-app-nodes"
    "kubernetes.io/cluster/${var.eks_cluster_name}" = "owned"
    "karpenter.sh/discovery"                        = var.eks_cluster_name
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "nodes_from_alb" {
  security_group_id            = aws_security_group.app_nodes.id
  description                  = "Ingress controller traffic from the ALB."
  referenced_security_group_id = aws_security_group.alb.id
  ip_protocol                  = "tcp"
  from_port                    = 1025
  to_port                      = 65535
}

resource "aws_vpc_security_group_ingress_rule" "nodes_from_nodes" {
  security_group_id            = aws_security_group.app_nodes.id
  description                  = "Pod-to-pod across nodes: the CNI gives every pod a VPC IP, so intra-cluster traffic is real VPC traffic. NetworkPolicy, not this SG, is what restricts it."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "-1"
}

resource "aws_vpc_security_group_ingress_rule" "nodes_from_control_plane" {
  security_group_id            = aws_security_group.app_nodes.id
  description                  = "EKS control plane to kubelet and to webhook/extension API servers running as pods."
  referenced_security_group_id = aws_security_group.eks_control_plane.id
  ip_protocol                  = "tcp"
  from_port                    = 1025
  to_port                      = 65535
}

resource "aws_vpc_security_group_egress_rule" "nodes_to_nodes" {
  security_group_id            = aws_security_group.app_nodes.id
  referenced_security_group_id = aws_security_group.app_nodes.id
  description                  = "Pod-to-pod."
  ip_protocol                  = "-1"
}

resource "aws_vpc_security_group_egress_rule" "nodes_to_control_plane" {
  security_group_id            = aws_security_group.app_nodes.id
  referenced_security_group_id = aws_security_group.eks_control_plane.id
  description                  = "kubelet and pods to the Kubernetes API."
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_security_group_egress_rule" "nodes_to_aurora" {
  security_group_id            = aws_security_group.app_nodes.id
  referenced_security_group_id = aws_security_group.aurora.id
  description                  = "PostgreSQL."
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

resource "aws_vpc_security_group_egress_rule" "nodes_to_redis" {
  security_group_id            = aws_security_group.app_nodes.id
  referenced_security_group_id = aws_security_group.redis.id
  description                  = "Redis over TLS."
  ip_protocol                  = "tcp"
  from_port                    = 6379
  to_port                      = 6379
}

resource "aws_vpc_security_group_egress_rule" "nodes_to_msk_tls" {
  security_group_id            = aws_security_group.app_nodes.id
  referenced_security_group_id = aws_security_group.msk.id
  description                  = "Kafka, SASL/IAM over TLS."
  ip_protocol                  = "tcp"
  from_port                    = 9098
  to_port                      = 9098
}

resource "aws_vpc_security_group_egress_rule" "nodes_to_msk_sasl_scram" {
  security_group_id            = aws_security_group.app_nodes.id
  referenced_security_group_id = aws_security_group.msk.id
  description                  = "Kafka, SASL/SCRAM over TLS. Present for the migration window only; IAM auth is the target state."
  ip_protocol                  = "tcp"
  from_port                    = 9096
  to_port                      = 9096
}

resource "aws_vpc_security_group_egress_rule" "nodes_to_vpce" {
  security_group_id            = aws_security_group.app_nodes.id
  referenced_security_group_id = aws_security_group.vpc_endpoints.id
  description                  = "AWS APIs via PrivateLink. This is the path Secrets Manager, KMS, STS, ECR and CloudWatch take; none of it leaves the VPC."
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_security_group_egress_rule" "nodes_to_s3_gateway" {
  security_group_id = aws_security_group.app_nodes.id
  description       = "S3 via the gateway endpoint. Gateway endpoints have no ENI, so this must be expressed against the managed prefix list rather than an SG."
  prefix_list_id    = aws_vpc_endpoint.s3.prefix_list_id
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

resource "aws_vpc_security_group_egress_rule" "nodes_to_dynamodb_gateway" {
  security_group_id = aws_security_group.app_nodes.id
  description       = "DynamoDB via the gateway endpoint - the DR fencing table (disaster-recovery.md 3)."
  prefix_list_id    = aws_vpc_endpoint.dynamodb.prefix_list_id
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

resource "aws_vpc_security_group_egress_rule" "nodes_dns_udp" {
  security_group_id = aws_security_group.app_nodes.id
  description       = "DNS to the VPC resolver (.2 address of the VPC CIDR)."
  cidr_ipv4         = "${cidrhost(var.vpc_cidr, 2)}/32"
  ip_protocol       = "udp"
  from_port         = 53
  to_port           = 53
}

resource "aws_vpc_security_group_egress_rule" "nodes_dns_tcp" {
  security_group_id = aws_security_group.app_nodes.id
  description       = "DNS over TCP for responses above 512 bytes."
  cidr_ipv4         = "${cidrhost(var.vpc_cidr, 2)}/32"
  ip_protocol       = "tcp"
  from_port         = 53
  to_port           = 53
}

# There is deliberately NO 0.0.0.0/0 egress rule on app_nodes. Outbound calls to
# gateways leave through the egress-tier ENIs of the egress proxy pods, whose SG
# is below. A node that could reach the internet directly would make the whole
# allowlist decorative.

# --- Egress proxy (the only workload with a route to the internet) -------------
resource "aws_security_group" "egress_proxy" {
  name        = "${local.name}-egress-proxy"
  description = "Egress proxy pods' dedicated ENIs, placed in the egress subnets by an ENIConfig/security-group-for-pods binding."
  vpc_id      = aws_vpc.this.id

  tags = {
    Name = "${local.name}-egress-proxy"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "egress_proxy_from_nodes" {
  security_group_id            = aws_security_group.egress_proxy.id
  description                  = "payment-orchestrator and workflow-worker to the forward proxy."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 3128
  to_port                      = 3128
}

resource "aws_vpc_security_group_ingress_rule" "egress_proxy_ssrf_guard_from_nodes" {
  security_group_id            = aws_security_group.egress_proxy.id
  description                  = "webhook-sender to the SSRF-guarding proxy. Separate port because merchant-supplied URLs are attacker-controlled input and must not share a policy with gateway calls (security.md 2.2)."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 3129
  to_port                      = 3129
}

resource "aws_vpc_security_group_egress_rule" "egress_proxy_https" {
  security_group_id = aws_security_group.egress_proxy.id
  description       = "Outbound HTTPS. The destination set is constrained by the Network Firewall domain allowlist and, above it, by the proxy's own per-service-account allowlist."
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

resource "aws_vpc_security_group_egress_rule" "egress_proxy_dns_udp" {
  security_group_id = aws_security_group.egress_proxy.id
  description       = "DNS to the VPC resolver."
  cidr_ipv4         = "${cidrhost(var.vpc_cidr, 2)}/32"
  ip_protocol       = "udp"
  from_port         = 53
  to_port           = 53
}

# --- EKS control plane --------------------------------------------------------
resource "aws_security_group" "eks_control_plane" {
  name        = "${local.name}-eks-control-plane"
  description = "EKS cluster (control plane) ENIs in the pod subnets."
  vpc_id      = aws_vpc.this.id

  tags = {
    Name = "${local.name}-eks-control-plane"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "control_plane_from_nodes" {
  security_group_id            = aws_security_group.eks_control_plane.id
  description                  = "kubelet and pods to the API server."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_security_group_egress_rule" "control_plane_to_nodes" {
  security_group_id            = aws_security_group.eks_control_plane.id
  description                  = "API server to kubelet and to admission webhooks (Kyverno) running as pods."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 1025
  to_port                      = 65535
}

# --- Aurora -------------------------------------------------------------------
resource "aws_security_group" "aurora" {
  name        = "${local.name}-aurora"
  description = "Aurora PostgreSQL cluster. Ingress from the node SG only; no egress at all."
  vpc_id      = aws_vpc.this.id

  tags = {
    Name = "${local.name}-aurora"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "aurora_from_nodes" {
  security_group_id            = aws_security_group.aurora.id
  description                  = "PostgreSQL from EKS pods (through PgBouncer)."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

# No egress rule on the Aurora SG. Aurora initiates nothing: enhanced monitoring
# and Performance Insights are delivered by the RDS service, not by the
# instance's ENI. An empty egress set is the correct configuration and any
# addition to it should be treated as a finding.

# --- ElastiCache --------------------------------------------------------------
resource "aws_security_group" "redis" {
  name        = "${local.name}-redis"
  description = "ElastiCache Redis replication group."
  vpc_id      = aws_vpc.this.id

  tags = {
    Name = "${local.name}-redis"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "redis_from_nodes" {
  security_group_id            = aws_security_group.redis.id
  description                  = "Redis (TLS) from payment-api and payment-orchestrator."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 6379
  to_port                      = 6379
}

# --- MSK ----------------------------------------------------------------------
resource "aws_security_group" "msk" {
  name        = "${local.name}-msk"
  description = "MSK brokers."
  vpc_id      = aws_vpc.this.id

  tags = {
    Name = "${local.name}-msk"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "msk_from_nodes_iam" {
  security_group_id            = aws_security_group.msk.id
  description                  = "Kafka SASL/IAM over TLS from outbox-relay, event-consumer, control-plane-api and payment-orchestrator."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 9098
  to_port                      = 9098
}

resource "aws_vpc_security_group_ingress_rule" "msk_from_nodes_scram" {
  security_group_id            = aws_security_group.msk.id
  description                  = "Kafka SASL/SCRAM over TLS (migration window)."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 9096
  to_port                      = 9096
}

resource "aws_vpc_security_group_ingress_rule" "msk_interbroker" {
  security_group_id            = aws_security_group.msk.id
  description                  = "Inter-broker replication."
  referenced_security_group_id = aws_security_group.msk.id
  ip_protocol                  = "tcp"
  from_port                    = 9093
  to_port                      = 9093
}

resource "aws_vpc_security_group_ingress_rule" "msk_zookeeper_tls" {
  security_group_id            = aws_security_group.msk.id
  description                  = "KRaft controller / ZooKeeper TLS between brokers."
  referenced_security_group_id = aws_security_group.msk.id
  ip_protocol                  = "tcp"
  from_port                    = 2182
  to_port                      = 2182
}

resource "aws_vpc_security_group_egress_rule" "msk_interbroker_out" {
  security_group_id            = aws_security_group.msk.id
  description                  = "Inter-broker replication, outbound side."
  referenced_security_group_id = aws_security_group.msk.id
  ip_protocol                  = "-1"
}

resource "aws_vpc_security_group_egress_rule" "msk_to_vpce" {
  security_group_id            = aws_security_group.msk.id
  description                  = "Broker log delivery and tiered storage go via the AWS APIs on PrivateLink."
  referenced_security_group_id = aws_security_group.vpc_endpoints.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_security_group_egress_rule" "msk_to_s3" {
  security_group_id = aws_security_group.msk.id
  description       = "Tiered storage for pp.audit.v1 (400 d retention on broker storage would be absurd)."
  prefix_list_id    = aws_vpc_endpoint.s3.prefix_list_id
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

# --- VPC interface endpoints --------------------------------------------------
resource "aws_security_group" "vpc_endpoints" {
  name        = "${local.name}-vpc-endpoints"
  description = "Interface endpoint ENIs. Ingress on 443 from the workloads that call AWS APIs."
  vpc_id      = aws_vpc.this.id

  tags = {
    Name = "${local.name}-vpc-endpoints"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "vpce_from_nodes" {
  security_group_id            = aws_security_group.vpc_endpoints.id
  description                  = "AWS API calls from pods."
  referenced_security_group_id = aws_security_group.app_nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_security_group_ingress_rule" "vpce_from_msk" {
  security_group_id            = aws_security_group.vpc_endpoints.id
  description                  = "MSK broker log and metric delivery."
  referenced_security_group_id = aws_security_group.msk.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_security_group_ingress_rule" "vpce_from_egress_proxy" {
  security_group_id            = aws_security_group.vpc_endpoints.id
  description                  = "The proxy fetches its own configuration and writes its logs through the endpoints."
  referenced_security_group_id = aws_security_group.egress_proxy.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}
