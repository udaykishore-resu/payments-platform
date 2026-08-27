###############################################################################
# modules/network - VPC, five subnet tiers, NAT, route tables
#
# Tier model (deployment.md 2.2, security.md 2.2):
#
#   public     ALB + NAT gateways only.                     -> IGW
#   pod        EKS nodes and pods.                          -> NAT (per AZ)
#   data       Aurora, ElastiCache.                          NO NAT, NO IGW
#   streaming  MSK brokers.                                  NO NAT, NO IGW
#   egress     The only tier with a general default route.  -> firewall -> NAT
#   firewall   Network Firewall endpoint ENIs (optional).   -> NAT
#
# The data and streaming tiers have no default route at all. An Aurora instance
# has no business reaching the internet, and the *absence of a route* is a
# stronger control than a security-group rule, because there is nothing to
# misconfigure back open.
###############################################################################

terraform {
  required_version = ">= 1.9.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.60.0, < 6.0.0"
    }
  }
}

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

locals {
  name = "pp-${var.environment}"

  az_count = length(var.availability_zones)

  # One NAT per AZ in staging/prod. `single_nat_gateway` is a dev-only cost lever
  # and is rejected for the other environments by the check block below.
  nat_count = var.single_nat_gateway ? 1 : local.az_count

  firewall_enabled = var.enable_network_firewall && length(var.firewall_subnet_cidrs) == local.az_count
}

check "nat_topology_matches_environment" {
  assert {
    condition     = var.environment == "dev" || var.single_nat_gateway == false
    error_message = "single_nat_gateway collapses egress onto one AZ and voids AZ independence. It is permitted in dev only."
  }
}

check "firewall_subnets_present_when_enabled" {
  assert {
    condition     = var.enable_network_firewall == false || length(var.firewall_subnet_cidrs) == length(var.availability_zones)
    error_message = "enable_network_firewall requires one firewall subnet CIDR per availability zone."
  }
}

check "subnet_counts_match_azs" {
  assert {
    condition = alltrue([
      length(var.public_subnet_cidrs) == length(var.availability_zones),
      length(var.pod_subnet_cidrs) == length(var.availability_zones),
      length(var.data_subnet_cidrs) == length(var.availability_zones),
      length(var.streaming_subnet_cidrs) == length(var.availability_zones),
      length(var.egress_subnet_cidrs) == length(var.availability_zones),
    ])
    error_message = "Every subnet tier must declare exactly one CIDR per availability zone."
  }
}

###############################################################################
# VPC
###############################################################################

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true # Required for interface endpoints' private DNS.

  tags = {
    Name       = "${local.name}-vpc"
    RegionRole = var.region_role
  }
}

resource "aws_default_security_group" "this" {
  vpc_id = aws_vpc.this.id

  # No ingress, no egress. The default SG is attached to anything created
  # without an explicit SG; leaving it permissive is a silent bypass of every
  # rule in security-groups.tf.

  tags = {
    Name = "${local.name}-default-DO-NOT-USE"
  }
}

resource "aws_default_network_acl" "this" {
  default_network_acl_id = aws_vpc.this.default_network_acl_id

  # Deny-all default NACL. Every subnet below is explicitly associated with a
  # tier NACL, so nothing should ever land on this one; if something does, it
  # fails loudly rather than inheriting an allow-all.

  tags = {
    Name = "${local.name}-default-deny"
  }

  lifecycle {
    ignore_changes = [subnet_ids]
  }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = {
    Name = "${local.name}-igw"
  }
}

###############################################################################
# Subnets
###############################################################################

resource "aws_subnet" "public" {
  count = local.az_count

  vpc_id                  = aws_vpc.this.id
  cidr_block              = var.public_subnet_cidrs[count.index]
  availability_zone       = var.availability_zones[count.index]
  map_public_ip_on_launch = false # Nothing in a public subnet is launched by us except the ALB/NAT ENIs.

  tags = {
    Name = "${local.name}-public-${var.availability_zones[count.index]}"
    Tier = "public"
    "kubernetes.io/role/elb"                            = "1"
    "kubernetes.io/cluster/${var.eks_cluster_name}"     = "shared"
  }
}

resource "aws_subnet" "pod" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = var.pod_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = {
    Name = "${local.name}-pod-${var.availability_zones[count.index]}"
    Tier = "pod"
    "kubernetes.io/role/internal-elb"               = "1"
    "kubernetes.io/cluster/${var.eks_cluster_name}" = "shared"
    "karpenter.sh/discovery"                        = var.eks_cluster_name
  }
}

resource "aws_subnet" "data" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = var.data_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = {
    Name = "${local.name}-data-${var.availability_zones[count.index]}"
    Tier = "data"
  }
}

resource "aws_subnet" "streaming" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = var.streaming_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = {
    Name = "${local.name}-streaming-${var.availability_zones[count.index]}"
    Tier = "streaming"
  }
}

resource "aws_subnet" "egress" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = var.egress_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = {
    Name = "${local.name}-egress-${var.availability_zones[count.index]}"
    Tier = "egress"
  }
}

resource "aws_subnet" "firewall" {
  count = local.firewall_enabled ? local.az_count : 0

  vpc_id            = aws_vpc.this.id
  cidr_block        = var.firewall_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = {
    Name = "${local.name}-firewall-${var.availability_zones[count.index]}"
    Tier = "firewall"
  }
}

###############################################################################
# NAT gateways
###############################################################################

resource "aws_eip" "nat" {
  count = local.nat_count

  domain = "vpc"

  tags = {
    Name = "${local.name}-nat-${var.availability_zones[count.index]}"
  }

  depends_on = [aws_internet_gateway.this]
}

resource "aws_nat_gateway" "this" {
  count = local.nat_count

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = {
    Name = "${local.name}-nat-${var.availability_zones[count.index]}"
  }

  depends_on = [aws_internet_gateway.this]
}

###############################################################################
# Route tables
###############################################################################

# --- public -------------------------------------------------------------------
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  tags = {
    Name = "${local.name}-rt-public"
  }
}

resource "aws_route" "public_default" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}

# Return path for firewall-inspected flows: traffic coming back from the NAT
# gateway to an egress-subnet address must re-enter the firewall endpoint,
# otherwise the stateful engine sees a one-sided flow and drops it.
resource "aws_route" "public_return_to_firewall" {
  count = local.firewall_enabled ? local.az_count : 0

  route_table_id         = aws_route_table.public.id
  destination_cidr_block = var.egress_subnet_cidrs[count.index]
  vpc_endpoint_id        = local.firewall_endpoint_ids[count.index]
}

resource "aws_route_table_association" "public" {
  count = local.az_count

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# --- pod (one table per AZ so a NAT failure is contained to its own AZ) --------
resource "aws_route_table" "pod" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  tags = {
    Name = "${local.name}-rt-pod-${var.availability_zones[count.index]}"
  }
}

resource "aws_route" "pod_default" {
  count = local.az_count

  route_table_id         = aws_route_table.pod[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[var.single_nat_gateway ? 0 : count.index].id
}

resource "aws_route_table_association" "pod" {
  count = local.az_count

  subnet_id      = aws_subnet.pod[count.index].id
  route_table_id = aws_route_table.pod[count.index].id
}

# --- data (no default route at all) -------------------------------------------
resource "aws_route_table" "data" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  tags = {
    Name = "${local.name}-rt-data-${var.availability_zones[count.index]}"
    Note = "No default route. Intentional and load-bearing."
  }
}

resource "aws_route_table_association" "data" {
  count = local.az_count

  subnet_id      = aws_subnet.data[count.index].id
  route_table_id = aws_route_table.data[count.index].id
}

# --- streaming (no default route) ---------------------------------------------
resource "aws_route_table" "streaming" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  tags = {
    Name = "${local.name}-rt-streaming-${var.availability_zones[count.index]}"
    Note = "No default route. Intentional and load-bearing."
  }
}

resource "aws_route_table_association" "streaming" {
  count = local.az_count

  subnet_id      = aws_subnet.streaming[count.index].id
  route_table_id = aws_route_table.streaming[count.index].id
}

# --- egress -------------------------------------------------------------------
# With the firewall enabled the default route points at the AZ-local firewall
# endpoint; without it, straight at the AZ-local NAT gateway.
resource "aws_route_table" "egress" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  tags = {
    Name = "${local.name}-rt-egress-${var.availability_zones[count.index]}"
  }
}

resource "aws_route" "egress_default_via_firewall" {
  count = local.firewall_enabled ? local.az_count : 0

  route_table_id         = aws_route_table.egress[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  vpc_endpoint_id        = local.firewall_endpoint_ids[count.index]
}

resource "aws_route" "egress_default_via_nat" {
  count = local.firewall_enabled ? 0 : local.az_count

  route_table_id         = aws_route_table.egress[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[var.single_nat_gateway ? 0 : count.index].id
}

resource "aws_route_table_association" "egress" {
  count = local.az_count

  subnet_id      = aws_subnet.egress[count.index].id
  route_table_id = aws_route_table.egress[count.index].id
}

# --- firewall -----------------------------------------------------------------
resource "aws_route_table" "firewall" {
  count = local.firewall_enabled ? local.az_count : 0

  vpc_id = aws_vpc.this.id

  tags = {
    Name = "${local.name}-rt-firewall-${var.availability_zones[count.index]}"
  }
}

resource "aws_route" "firewall_default" {
  count = local.firewall_enabled ? local.az_count : 0

  route_table_id         = aws_route_table.firewall[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[var.single_nat_gateway ? 0 : count.index].id
}

resource "aws_route_table_association" "firewall" {
  count = local.firewall_enabled ? local.az_count : 0

  subnet_id      = aws_subnet.firewall[count.index].id
  route_table_id = aws_route_table.firewall[count.index].id
}

###############################################################################
# NACLs - a stateless second opinion (security.md 2.2)
#
# NACLs cannot express identity, so they are deliberately coarse. Their value is
# that they fail differently from security groups: a bug in an SG rule and a bug
# in a NACL rule are not the same bug.
###############################################################################

resource "aws_network_acl" "data" {
  vpc_id     = aws_vpc.this.id
  subnet_ids = aws_subnet.data[*].id

  tags = {
    Name = "${local.name}-nacl-data"
  }
}

resource "aws_network_acl_rule" "data_ingress_pod_postgres" {
  count = local.az_count

  network_acl_id = aws_network_acl.data.id
  rule_number    = 100 + count.index
  egress         = false
  protocol       = "tcp"
  rule_action    = "allow"
  cidr_block     = var.pod_subnet_cidrs[count.index]
  from_port      = 5432
  to_port        = 5432
}

resource "aws_network_acl_rule" "data_ingress_pod_redis" {
  count = local.az_count

  network_acl_id = aws_network_acl.data.id
  rule_number    = 110 + count.index
  egress         = false
  protocol       = "tcp"
  rule_action    = "allow"
  cidr_block     = var.pod_subnet_cidrs[count.index]
  from_port      = 6379
  to_port        = 6379
}

resource "aws_network_acl_rule" "data_ingress_vpc_ephemeral" {
  network_acl_id = aws_network_acl.data.id
  rule_number    = 200
  egress         = false
  protocol       = "tcp"
  rule_action    = "allow"
  cidr_block     = var.vpc_cidr
  from_port      = 1024
  to_port        = 65535
}

resource "aws_network_acl_rule" "data_egress_vpc" {
  network_acl_id = aws_network_acl.data.id
  rule_number    = 100
  egress         = true
  protocol       = "tcp"
  rule_action    = "allow"
  cidr_block     = var.vpc_cidr
  from_port      = 0
  to_port        = 65535
}

resource "aws_network_acl" "public" {
  vpc_id     = aws_vpc.this.id
  subnet_ids = aws_subnet.public[*].id

  tags = {
    Name = "${local.name}-nacl-public"
  }
}

# Explicit deny for remote-administration ports, numbered ahead of the allows.
resource "aws_network_acl_rule" "public_deny_ssh" {
  network_acl_id = aws_network_acl.public.id
  rule_number    = 10
  egress         = false
  protocol       = "tcp"
  rule_action    = "deny"
  cidr_block     = "0.0.0.0/0"
  from_port      = 22
  to_port        = 22
}

resource "aws_network_acl_rule" "public_deny_rdp" {
  network_acl_id = aws_network_acl.public.id
  rule_number    = 11
  egress         = false
  protocol       = "tcp"
  rule_action    = "deny"
  cidr_block     = "0.0.0.0/0"
  from_port      = 3389
  to_port        = 3389
}

resource "aws_network_acl_rule" "public_allow_https" {
  network_acl_id = aws_network_acl.public.id
  rule_number    = 100
  egress         = false
  protocol       = "tcp"
  rule_action    = "allow"
  cidr_block     = "0.0.0.0/0"
  from_port      = 443
  to_port        = 443
}

resource "aws_network_acl_rule" "public_allow_http_redirect" {
  network_acl_id = aws_network_acl.public.id
  rule_number    = 110
  egress         = false
  protocol       = "tcp"
  rule_action    = "allow"
  cidr_block     = "0.0.0.0/0"
  from_port      = 80
  to_port        = 80
}

resource "aws_network_acl_rule" "public_allow_ephemeral" {
  network_acl_id = aws_network_acl.public.id
  rule_number    = 120
  egress         = false
  protocol       = "tcp"
  rule_action    = "allow"
  cidr_block     = "0.0.0.0/0"
  from_port      = 1024
  to_port        = 65535
}

resource "aws_network_acl_rule" "public_egress_all" {
  network_acl_id = aws_network_acl.public.id
  rule_number    = 100
  egress         = true
  protocol       = "-1"
  rule_action    = "allow"
  cidr_block     = "0.0.0.0/0"
  from_port      = 0
  to_port        = 0
}
