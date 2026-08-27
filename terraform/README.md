# `terraform/` — the AWS substrate

This tree builds the AWS substrate the nine deployables of
[`docs/spec/00-design-baseline.md`](../docs/spec/00-design-baseline.md) §5 run on:
accounts' networking, EKS, Aurora Global, MSK, ElastiCache, S3, KMS, Secrets
Manager, the edge, DNS, observability and the DR control plane.

It is subordinate to [`docs/deployment.md`](../docs/deployment.md) §2,
[`docs/disaster-recovery.md`](../docs/disaster-recovery.md) and
[`docs/security.md`](../docs/security.md). Where this code and those documents
disagree, the documents win and the code is a defect.

```
terraform/
  versions.tf            provider + core version constraints (canonical copy)
  modules/               12 modules, one per concern
  envs/{dev,staging,prod}  one stack per environment
  policies/              IAM documents applied outside the env stacks
  scripts/               tfcheck.py, tfalign.py
```

Two things referenced below that live outside this directory: the `bootstrap`
stack (§1) and the repository `Makefile` targets (`make sync-versions`,
`make policies`, `make destroy-dev`). Both are named here because the workflow
depends on them; neither is in this tree.

---

## 1. Bootstrap — creating the state bucket itself

The chicken-and-egg problem is real and is solved by admitting it rather than by
a clever trick: **the bootstrap stack's own state is committed to git.**

The bootstrap stack creates, per account:

| Resource | Why |
|---|---|
| `pp-terraform-state-<env>` (S3) | Versioned, SSE-KMS with its own CMK, Object Lock GOVERNANCE 90 d, public access blocked, TLS-only, MFA-delete enabled |
| `pp-terraform-locks` (DynamoDB) | State locking. Two concurrent applies without it interleave writes and produce a state file describing infrastructure that never existed |
| `alias/pp-terraform-state` (KMS) | Separate from every workload key, so "who can read the state" is a different question from "who can read the database" |
| `pp-terraform-<env>` (IAM role) | The deploy role, assumed by CI via GitHub OIDC. Policy in [`policies/terraform-deploy-role.json.tftpl`](policies/terraform-deploy-role.json.tftpl) |
| `pp-terraform-state-<env>` (IAM role) | A *separate* role for state access, so a compromised deploy role cannot rewrite state history |

Procedure, run once per account by a human with break-glass credentials:

```bash
# 1. Local state, deliberately.
cd terraform/bootstrap
terraform init                       # no backend block yet
terraform apply -var-file=../envs/prod/bootstrap.tfvars

# 2. Move the bootstrap stack's state into the bucket it just created.
cat > backend.tf <<'HCL'
terraform {
  backend "s3" {
    bucket         = "pp-terraform-state-prod"
    key            = "bootstrap/bootstrap.tfstate"
    region         = "eu-west-1"
    dynamodb_table = "pp-terraform-locks"
    encrypt        = true
    kms_key_id     = "alias/pp-terraform-state"
  }
}
HCL
terraform init -migrate-state

# 3. Commit the (now empty) local state file's absence, and record the bucket
#    and lock table names in envs/<env>/backend.tf. They are not secrets.
git add backend.tf && git commit -m "bootstrap: migrate state to S3"
```

**If the bootstrap state is ever lost**, the recovery is `terraform import` of
five resources, not a rebuild — the bucket holds every other stack's state and
must not be recreated. The import commands are in
`docs/runbooks/terraform-bootstrap-recovery.md`.

**The bootstrap stack is not in this directory** and is deliberately not part of
the normal plan/apply cycle: it changes perhaps twice a year, and mixing it with
the stacks whose state it holds is how a bad plan takes out the state bucket.

---

## 2. Plan / apply workflow

Nothing in this tree is applied from a laptop. CI holds no long-lived AWS keys;
it federates through GitHub OIDC into `pp-terraform-<env>`.

```
PR opened
  └─ fmt check ──────── terraform fmt -recursive -check
  └─ validate ───────── terraform validate  (each env, -backend=false)
  └─ lint ───────────── tflint --recursive
  └─ policy ─────────── checkov, conftest (OPA) over the plan JSON
  └─ plan ───────────── terraform plan -out=tfplan, posted as a PR comment
                        A plan that DESTROYS a stateful resource fails the
                        build unless the PR carries the `approved-destroy`
                        label (docs/deployment.md §4.1 stage 19).

merge to main
  └─ apply ──────────── dev automatically
                        staging automatically
                        prod: manual approval by someone who is not the author
```

Locally, for reading a plan before opening the PR:

```bash
cd terraform/envs/staging
terraform init -backend-config=backend.hcl
terraform plan -var-file=terraform.tfvars
```

Two rules that are not negotiable:

1. **`terraform apply` is never run without a saved plan file.** Applying a
   freshly-computed plan means applying something nobody reviewed — the world
   may have changed between the review and the apply.
2. **`-target` is a debugging tool, never a deployment tool.** A targeted apply
   produces a state file that is correct about the target and stale about
   everything that depends on it.

---

## 3. Order the stacks must be applied in

Terraform resolves the order *inside* a stack from the dependency graph. It
cannot resolve the order *between* stacks, and that order matters:

| # | Stack | Depends on | Why |
|---|---|---|---|
| 0 | `bootstrap` (per account) | — | Everything else stores state in what it creates |
| 1 | `pp-shared-services` (ECR, shared Route 53 zone, CI runners) | 0 | The env stacks write DNS records into the zone and pull images from the ECR it owns. Not in this repository |
| 2 | `envs/dev` | 1 | |
| 3 | `envs/staging` | 1 | |
| 4 | `envs/prod` | 1 | |

Within `envs/prod` the graph is resolved automatically, but the *logical* order
is worth knowing, because it is the order an incident recovery follows:

```
kms → s3 (DR destinations first, then primaries with replication)
    → network (needs the flow-log bucket)
    → secrets (needs subnets for the rotation lambdas)
    → aurora / msk / elasticache
    → dr (fencing table, backup vault)
    → edge → observability → dns
    → eks   (needs the MSK name, the Aurora resource ID, the fence table ARN,
             the AMP workspace and the zone ARN)
```

**Why EKS is last, not first.** The IRSA policies are scoped to the exact
resources the deployables use. That means the cluster cannot be created until
those resources exist and their ARNs are known. It is the direct cost of not
writing `"Resource": "*"`.

**The one cycle that had to be broken by hand.** The KMS key policies name the
IRSA roles; the IRSA policies name the keys. The DynamoDB fence policy names the
IRSA roles; the IRSA policies name the fence table. Both would be genuine cycles
if the role ARNs were read from `module.eks`. They are instead *constructed* from
their deterministic names in `locals`, and a `check` block at the bottom of each
stack asserts that the constructed ARNs match what the EKS module actually
created — because IAM accepts a policy naming a role that does not exist, and
grants nothing, silently.

---

## 4. Adding a new environment

1. Pick a non-overlapping `/16`. The plan so far: prod `10.20.0.0/16`, prod-DR
   `10.21.0.0/16`, staging `10.30.0.0/16`, dev `10.40.0.0/16`. Non-overlap is
   kept even though environment peering is forbidden — it preserves the option
   without ever exercising it.
2. Create the AWS account under the OU, so the SCPs in
   [`policies/scp-payment-platform-ou.json`](policies/scp-payment-platform-ou.json)
   apply from the first API call.
3. Run the bootstrap (§1) in the new account.
4. `cp -r envs/staging envs/<new>` and edit, in this order:
   `backend.tf` (bucket, key), `variables.tf` (the `environment` validation
   pins the name), `terraform.tfvars`, `main.tf` (the `environment` local).
5. Add the environment to `.github/workflows/terraform.yml` and to the drift
   job's matrix.
6. Add it to the GitOps repo's `clusters/` and to the ApplicationSet generators.
7. `make sync-versions` so the stack's `versions.tf` matches the canonical copy.

The env stacks are deliberately **not** a shared module with a `for_each` over
environments. Environments differ in ways that matter (prod has two regions,
dev has serverless data services, staging has cluster mode off), and a module
that absorbed all of that would be a configuration language with worse error
messages. Three stacks that each read top-to-bottom are the cheaper thing to
maintain.

---

## 5. Drift detection

```yaml
# .github/workflows/terraform-drift.yml (summary)
on:
  schedule: [{ cron: "0 */6 * * *" }]
jobs:
  drift:
    strategy: { matrix: { env: [dev, staging, prod] } }
    steps:
      - terraform init -backend-config=backend.hcl
      - terraform plan -detailed-exitcode -lock=false -var-file=terraform.tfvars
      # exit 0 = no drift, 2 = drift, 1 = error
      - if exit 2: open (or update) a `drift/<env>` issue with the plan, and
                   post to #pp-platform. Do NOT auto-apply.
```

Four decisions in that job:

- **Every six hours, not every hour.** A `plan` against the prod stack makes
  several thousand read API calls; hourly is enough volume to matter against
  the account's API rate limits during an incident.
- **`-lock=false`.** A read-only plan must never block a real apply.
- **It never auto-applies.** Drift is either an emergency change somebody made
  under break-glass — in which case the fix is a PR that codifies it — or it is
  an AWS-side change we need to understand before reverting.
- **Some drift is expected and is filtered.** `aws_eks_node_group.desired_size`
  moves with the HPA and Karpenter; MSK broker storage moves with autoscaling.
  Both carry `ignore_changes`, which is why the job's output is signal.

A separate weekly job asserts things a plan cannot see: that the live EKS add-on
versions match `var.eks_addon_versions`, that the live AMP retention matches
`var.prometheus_retention_days`, and that every resource carries the six
mandatory tags.

---

## 6. What is deliberately NOT managed by Terraform

This is the section to read before adding a resource here.

| Not managed | Managed by | Why |
|---|---|---|
| **Secret values** | An operator, once, via `platformctl secrets seed`; thereafter the rotation lambdas | A value written by Terraform is a value stored in the state file **in plaintext**. The state file is then the most valuable object in the estate: readable by everyone who can plan, versioned forever, and copied into every CI job's working directory. This module creates secret *containers* and never a `aws_secretsmanager_secret_version` — the omission is structural, not an oversight |
| **Kubernetes workloads** | ArgoCD, from `payments-platform-gitops` | Terraform reconciles on apply; ArgoCD reconciles continuously with `selfHeal`. Two reconcilers over one namespace fight, and the loser is whichever ran least recently. Terraform stops at the cluster boundary: it creates the cluster, the node groups, the add-ons and the IAM identities, and nothing that runs *inside* |
| **Helm releases** | ArgoCD | Same reason. `helm_release` in Terraform also puts the whole release state into the tfstate, so a `terraform destroy` becomes a cluster-wide uninstall |
| **Karpenter NodePools / EC2NodeClasses** | ArgoCD | They are CRDs. Terraform owns the SQS interruption queue and the IAM role Karpenter needs, which is the AWS-side half |
| **Kafka topics, partitions, retention** | `platformctl topics apply`, from `docs/events.md` §5.2 | Partition counts can be increased and never decreased, and increasing them re-hashes keys. That is a data-model change with a migration, not an infrastructure change, and it should not be reachable by editing a `.tf` file |
| **Database schema** | `platformctl migrate up`, as an ArgoCD PreSync hook | `docs/deployment.md` §5 |
| **Database roles and grants** | `migrations/` | The IRSA policies name database users (`pp_app`, `pp_app_ro`, `pp_outbox`…); the users themselves and their in-database grants are created by migrations, because that is where the RLS policies they interact with live |
| **AMP recording and alerting rules, Grafana dashboards** | ArgoCD, from `deployments/observability/` | They change weekly; the workspace changes yearly |
| **Shield Advanced subscription** | `pp-org-management`, manually | A one-year organization-level commitment. A `terraform destroy` must not be able to cancel it |
| **SCPs and the OU structure** | `pp-org-management` | An account inside an OU cannot attach its own SCP. Reproduced in `policies/` for review |
| **ECR repositories and images** | `pp-shared-services` + CI | Cross-account pull; no production data |
| **The public hosted zone** | `pp-shared-services` | One zone, many environments writing records into it |
| **ACM certificate renewal** | ACM | Automatic; Terraform owns the request and the validation records |

---

## 7. Cost

Monthly, US dollars, **at public list price in `eu-west-1`/`eu-central-1`, at
the sustained load in baseline §18 (5 000 TPS sustained, 15 000 peak)**. These
are estimates built from instance-hours and published unit prices, not from a
bill: treat them as accurate to about ±20 % and as a model to argue with rather
than a forecast.

### prod — two regions

| Line | Detail | USD/mo |
|---|---|---:|
| EKS compute — `eu-west-1` managed floor | 12× `c7g.4xlarge` (data, on-demand), 6× `m7g.2xlarge` (control), 4× `m7g.xlarge` (general, Spot) | 7,000 |
| EKS compute — `eu-west-1` Karpenter burst | peak-following; ≈22 `c7g.4xlarge`-equivalents averaged over the month | 9,900 |
| EKS compute — `eu-central-1` warm floor | the 10 % passive floor: 3× `c7g.4xlarge`, 2× `m7g.2xlarge`, 2× `m7g.xlarge` | 2,000 |
| EKS control planes | 2 clusters × $73 | 150 |
| EBS for nodes | ≈45 × 200 GB gp3 with provisioned throughput | 800 |
| Aurora instances — primary | writer + 2 readers `db.r6g.4xlarge`, I/O-Optimized | 3,250 |
| Aurora instances — secondary | 2 readers `db.r6g.2xlarge` (half class, deliberately) | 1,080 |
| Aurora storage, backups, cross-region replication | 3 TB, 35 d PITR, replicated write I/O | 1,450 |
| MSK brokers | 3× `kafka.m5.2xlarge` per region, both regions | 2,020 |
| MSK storage | 2 TB per broker with provisioned throughput, both regions | 1,700 |
| MSK tiered storage | `pp.audit.v1`, 400 d | 180 |
| ElastiCache | 3 shards × 2 nodes `cache.r7g.large`, both regions | 1,060 |
| NAT gateways | 6 × hourly + ≈4 TB processed | 780 |
| Network Firewall | 6 endpoints × $0.395/h + per-GB inspection | 2,050 |
| ALB × 2, NLB × 2 | plus LCU at this request rate | 340 |
| WAF | 2 web ACLs, 12 rules, ≈13 bn requests/mo | 1,150 |
| S3 | ≈25 TB across Standard/IA/Glacier IR, requests, CRR, RTC on two buckets | 1,350 |
| Data transfer | cross-AZ on the money path (not avoided — HA beats the transfer bill) + cross-region Aurora | 3,100 |
| CloudWatch Logs | control-plane, VPC flow, RDS, WAF only — application logs go to Loki | 900 |
| Amazon Managed Prometheus | ingestion + storage + query at the post-sampling cardinality | 1,450 |
| Amazon Managed Grafana | 25 users | 200 |
| KMS | 20 CMKs + ≈400 M requests | 420 |
| Secrets Manager | ≈1 500 secrets including multi-region replicas | 620 |
| Route 53 | zone + 2 fast health checks from 3 checker regions | 35 |
| AWS Backup | vault + cross-account copies | 260 |
| DynamoDB | the fencing table: one item, Global Table | 10 |
| Shield Advanced | per-resource protections only; the **subscription is org-level** and billed once for the whole organization ($3,000/mo) | 0 |
| **Total, list price** | | **≈ 43,300** |
| **Total, with 3-year Compute Savings Plan on the managed floor and 1-year Aurora RIs** | | **≈ 33,000** |

### staging — one region

| Line | Detail | USD/mo |
|---|---|---:|
| EKS compute | 3× `c7g.xlarge`, 2× `m7g.large`, 3× `m7g.large` Spot | 520 |
| EKS control plane + EBS | | 165 |
| Aurora | writer + 1 reader `db.r6g.large`, 300 GB, 7 d backups | 490 |
| MSK | 3× `kafka.m5.large`, 300 GB per broker | 560 |
| ElastiCache | 2× `cache.r7g.large` | 350 |
| NAT gateways | 3 × hourly + processing | 145 |
| Network Firewall | 3 endpoints — **25 % of the staging bill**, kept so the rule set is exercised before prod | 970 |
| ALB, NLB, WAF | | 100 |
| S3, KMS, Secrets Manager, Backup | | 185 |
| CloudWatch Logs + AMP | | 400 |
| **Total** | | **≈ 3,900** |

### dev — one region

| Line | Detail | USD/mo |
|---|---|---:|
| EKS compute | Spot everywhere, scaled to floor 20:00–07:00 | 120 |
| EKS control plane + EBS | | 145 |
| Aurora Serverless v2 | 0.5–4 ACU, ≈1.2 ACU average | 115 |
| **MSK Serverless** | **$0.75/h cluster charge alone — the single largest dev line, and larger than the compute it serves** | 550 |
| ElastiCache | 1× `cache.t4g.micro` | 12 |
| NAT gateway | one, shared across AZs | 55 |
| ALB, WAF, S3, KMS, Secrets | | 115 |
| CloudWatch Logs + AMP | | 150 |
| Preview environments | 72 h TTL, hard budget action at 100 % | 300 |
| **Total** | | **≈ 1,560** |

### The three biggest levers

1. **Commit the compute floor, and keep Graviton.** EKS compute is ≈45 % of the
   prod bill. A 3-year Compute Savings Plan covering the managed floor (not the
   burst — burst is what the plan is deliberately *not* sized for) takes roughly
   45 % off ≈$19 900/mo, or about **$8 500/mo**. Graviton is already assumed in
   every instance class here and is worth a further ≈20 % against the x86
   equivalents; the allowlists in `modules/*/variables.tf` reject Intel classes
   precisely so this cannot be undone by accident.

2. **Re-examine what the passive region actually needs to be warm.** The DR
   region costs ≈$8 000/mo: the 10 % compute floor, a full second MSK cluster, a
   full second Redis, and three Network Firewall endpoints. The MSK cluster is
   the interesting one — its topics are *empty* until a promotion, because
   nothing is replicated into it (`disaster-recovery.md` §4.2). Building it
   during promotion instead would recover **≈$1 900/mo** at the cost of adding
   MSK cluster creation (10–15 min) to a 15-minute RTO, which does not fit. A
   smaller broker class in the passive region, resized at promotion, recovers
   about half of that and does fit — and is the change worth costing properly.

3. **Keep AWS-service traffic off NAT and off the firewall.** Data transfer,
   NAT processing and Network Firewall inspection together are ≈$5 900/mo, and
   every byte of S3, ECR, Secrets Manager, KMS and CloudWatch traffic that goes
   through a VPC endpoint instead is billed at neither. The endpoint list in
   `modules/network/variables.tf` is exhaustive for exactly this reason; the
   thing to watch is a new AWS service being adopted without an endpoint, which
   shows up as a NAT-processing step change before it shows up anywhere else.
   The same line is why observability cardinality limits and tail sampling
   (`observability.md` §2.6) are not optional: without them the AMP line roughly
   doubles.

**Not a lever, and worth saying:** cross-AZ data transfer on the money path.
It is ≈$1 900/mo of the transfer line and it buys AZ-loss survival. Topology-
aware routing is applied where it is safe (the otel-agent is node-local); it is
not applied to the payment path.

Cost is an SLI: `pp_observability_cost_usd_estimate` plus AWS Budgets alarms at
80 % / 100 % / 120 % of forecast, routed to the platform team rather than to
finance, because the team that can change the spend is the team that should
hear about it.

---

## 8. Guardrails: `prevent_destroy` and how to legitimately remove a resource

Every stateful resource whose loss is not recoverable from anything else carries
`lifecycle { prevent_destroy = true }`:

| Resource | Module |
|---|---|
| Aurora cluster | `aurora` |
| Aurora Global cluster | `envs/prod` |
| MSK cluster | `msk` |
| S3 buckets (all) | `s3` |
| KMS keys and replica keys | `kms` |
| Secrets Manager secrets | `secrets` |
| DynamoDB fencing table | `dr` |
| AWS Backup vault | `dr` |
| EKS clusters | `eks` |
| Route 53 hosted zone | `dns` |

`prevent_destroy` cannot be driven by a variable — it is evaluated before
variables are known. That is a Terraform limitation, and this tree accepts it
rather than working around it with duplicated resource blocks: the flag is on in
every environment, including dev.

**The consequence for dev**, stated plainly: `terraform destroy` on the dev
stack will fail. Tearing dev down is `make destroy-dev`, which runs
`terraform state rm` for the protected addresses first, then destroys, then
deletes the orphaned resources with the AWS CLI. That is more friction than a
dev environment deserves — and it is less friction than the day somebody runs
`terraform destroy` in the wrong terminal.

**To legitimately remove a protected resource**, in every case:

1. Open a change record naming the resource and the retention obligation it is
   under. For Aurora and the audit buckets, `docs/compliance.md` §4.5 applies:
   financial records are the carve-out to crypto-shredding and cannot simply be
   deleted.
2. Take and *verify* a backup — restore it somewhere, do not assume it.
3. Turn off the service-level protection (`deletion_protection`,
   `object_lock`, `deletion_protection_enabled`) in one reviewed commit and
   apply. For RDS an SCP requires this to have happened in a *separate, earlier*
   API call than the delete.
4. Delete the `lifecycle` block in a **second** reviewed commit, with the ticket
   in the message. CI's stage-19 gate then requires the `approved-destroy` label
   and two approvals.

The two-commit rule is the whole point: it makes destruction require two
separate acts of intent, on two separate days, with two separate reviews.

---

## 9. Verification, and what was actually run

`terraform` is not installed in the environment this tree was authored in, and
`releases.hashicorp.com` is not reachable from it. So:

| Check | Status |
|---|---|
| `terraform fmt -recursive -check` | **Not run.** `scripts/tfalign.py` applies the one `fmt` transformation that produces review noise — `=` alignment across consecutive same-indent attribute runs, broken at multi-line values exactly as `fmt` breaks it — and reports the tree clean |
| `terraform validate` | **Not run.** `scripts/tfcheck.py` covers the structural subset: see below |
| `tflint`, `checkov`, `conftest` | **Not run** |
| `terraform plan` | **Not run** — it needs credentials |

`scripts/tfcheck.py` parses every `.tf` file with a real HCL2 parser
(`python-hcl2`, lark-based) and then checks:

1. HCL2 syntax, plus an independent brace/bracket/paren balance check that
   localises an imbalance to a file
2. no duplicate `resource`/`data` addresses, `variable` names or `output` names
   within a module
3. every `var.X` resolves to a `variable "X"` declared in that directory
4. every `local.X` resolves to a `locals` key in that directory
5. every `aws_*.name` and `data.aws_*.name` reference resolves to a block
   declared in that module
6. every `module.Y` referenced is called, and every `module.Y.Z` names a
   declared output of the module at `Y`'s `source`
7. every module call passes every input without a default, and passes nothing
   the module does not declare
8. every `aws.<alias>` provider reference names an alias the stack declares

```
$ python3 scripts/tfcheck.py
TOTAL  files=57  resource blocks=257  data sources=59  module calls=45
       variables=319  outputs=170
OK
```

The script was validated by injecting a deliberate reference to a non-existent
resource, data source, variable and local, and confirming all four were caught.

**What this does not catch, and a reviewer should assume is unverified:**
provider schema conformance (a misspelled argument, a block that this provider
version does not accept, an argument that moved between majors), IAM policy
semantics, AWS-side quota and naming constraints, and anything that only a real
`plan` against a real account would surface.

---

## 10. Known caveats

Three things in this tree that a reviewer should see stated rather than
discover.

**KMS key policies have no account-root delegation.** Each key policy names the
administrator ARNs, the workload role ARNs and the service principals, and
nothing else. This is what the least-privilege requirement asks for, and it has
a real operational cost: adding a consumer of a key means adding it to
`kms_key_users` in the env stack, and forgetting to surfaces as an `AccessDenied`
on first use rather than being silently permitted. That trade is deliberate —
the alternative ("allow the account root, and rely on IAM") makes the key policy
decorative and the audit question unanswerable.

**Two regional certificates for one hostname.** Both regional edge stacks
request an ACM certificate covering `api.example.com` and both write the
validation CNAME into the same hosted zone. ACM normally returns the same
validation record for the same domain in the same account, in which case both
writes are identical and idempotent. If it returns distinct tokens, the second
apply overwrites the first and one certificate stays `PENDING_VALIDATION`. The
resolution, if that happens, is to give the DR certificate its own regional
hostname and put the shared hostname on CloudFront — which is the topology
`security.md` §2.1 describes anyway. `modules/edge/variables.tf`
(`validation_record_suffix`) carries the same note.

**Network Firewall is not the whole egress control.** SNI is client-asserted; a
compromised process that speaks TLS to an allowlisted host is not stopped by it.
The firewall answers "which destination"; the in-cluster egress proxy answers
"which workload", per service account. They are complementary, they fail
differently, and dev runs with only the proxy — a priced trade, not an
oversight. `modules/network/firewall.tf` says the same thing at the top of the
file.
