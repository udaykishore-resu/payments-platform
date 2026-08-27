# RB-035: Terraform bootstrap state recovery

> **Do not `terraform apply` the bootstrap stack to "recreate" it.** The bootstrap bucket holds
> every other stack's state. Recreating it destroys the state of the entire estate. The recovery is
> `terraform import` of five resources.

- **Severity:** ticket, escalating to page if any other stack needs to apply while state is lost.
- **Alert:** none. Discovered when a bootstrap plan proposes creating resources that already exist,
  or when the local bootstrap state file is missing.
- **Triggered when:** the bootstrap stack's state is lost, corrupted, or was never migrated to S3.
- **Plane / service:** infrastructure · the Terraform state backend
- **Related:** `terraform/README.md` §1 (bootstrap) and §2 (plan/apply workflow),
  `docs/deployment.md`, [region-failover.md](region-failover.md)

## What this means

The bootstrap stack creates the things every other stack depends on to have state at all:

| Resource | Role |
|---|---|
| `aws_s3_bucket.pp-terraform-state-<env>` | Holds every stack's `.tfstate` |
| Bucket versioning / encryption configuration | Recoverability and confidentiality of that state |
| `aws_dynamodb_table.pp-terraform-locks` | State locking; without it two applies race |
| `aws_kms_key` `alias/pp-terraform-state` | Encrypts the state at rest |
| The IAM role CI federates into (`pp-terraform-<env>`, via GitHub OIDC) | The only principal that applies anything |

It is **deliberately not in the normal plan/apply cycle** and not in the `terraform/` tree: it
changes perhaps twice a year, and mixing it with the stacks whose state it holds is how a bad plan
takes out the state bucket.

Losing its state does **not** lose the resources. It loses Terraform's *knowledge* of them. The
resources are still there, still holding everyone else's state, still working. That distinction is
the entire runbook: the fix is to re-teach Terraform, not to rebuild anything.

## Impact

- **No runtime impact at all.** No payment, no merchant, no service is affected. The platform does
  not read Terraform state.
- **The bootstrap stack cannot be planned or applied** until state is restored.
- **Other stacks are unaffected** as long as the bucket, the lock table and the key still exist —
  they read their own state from the bucket, which does not care whether the bootstrap stack knows
  about itself.
- **The danger is entirely self-inflicted**: an `apply` against empty state proposes *creating* the
  bucket, and the failure mode of that is catastrophic and immediate.

## Immediate triage (first 5 minutes)

1. **Stop.** Do not run `terraform apply` on the bootstrap stack. Announce that in the channel
   before doing anything else.
2. Confirm the resources actually still exist — this is almost always the case:
   ```bash
   aws s3api head-bucket --bucket "pp-terraform-state-${ENV}"
   aws s3api get-bucket-versioning --bucket "pp-terraform-state-${ENV}"
   aws dynamodb describe-table --table-name pp-terraform-locks \
     --query 'Table.{Name:TableName,Status:TableStatus}'
   aws kms describe-key --key-id alias/pp-terraform-state --query 'KeyMetadata.{Id:KeyId,State:KeyState}'
   aws iam get-role --role-name "pp-terraform-${ENV}" --query 'Role.{Name:RoleName,Arn:Arn}'
   ```
3. Confirm other stacks' state is intact — this is what you are protecting:
   ```bash
   aws s3 ls "s3://pp-terraform-state-${ENV}/" --recursive | head -40
   ```
4. Check whether the state is merely *unreachable* rather than lost — a wrong backend
   configuration looks identical to lost state:
   ```bash
   cat envs/"${ENV}"/backend.tf
   aws s3 ls "s3://pp-terraform-state-${ENV}/bootstrap/bootstrap.tfstate"
   ```
5. Check for a stale lock, which blocks everyone:
   ```bash
   aws dynamodb scan --table-name pp-terraform-locks \
     --query 'Items[].{LockID:LockID.S,Info:Info.S}'
   ```

## Diagnosis

- **The state object exists in S3 but Terraform cannot read it** → a backend misconfiguration or a
  credentials problem, not lost state. → *M1*.
- **The state object is missing but a prior version exists** → the bucket is versioned; restore the
  previous version. This is by far the cheapest path. → *M2*.
- **The state object is genuinely gone and no version remains** → import. → *M3*.
- **A local `bootstrap.tfstate` exists and was never migrated to S3** → finish the migration:
  `terraform init -migrate-state`. → *M4*.
- **A stale lock is blocking applies** → the lock is not the state. → *M5*.
- **The bucket itself is gone** → this is a different and far worse incident: every stack's state is
  gone. → *M6*.
- **A plan proposes creating the bucket** → your state is empty and you are one keystroke from the
  worst outcome. → *M3*, and do not proceed until the plan shows no creates.

## Mitigation

**M1 — fix the backend configuration.** The bucket, key and lock table names are **not secrets** and
belong in `envs/<env>/backend.tf`:
```hcl
backend "s3" {
  bucket         = "pp-terraform-state-prod"
  key            = "bootstrap/bootstrap.tfstate"
  region         = "eu-west-1"
  dynamodb_table = "pp-terraform-locks"
  encrypt        = true
  kms_key_id     = "alias/pp-terraform-state"
}
```
```bash
terraform init -reconfigure
terraform plan       # must show NO creates and NO destroys
```

**M2 — restore the previous state version.** The bucket is versioned precisely for this:
```bash
aws s3api list-object-versions --bucket "pp-terraform-state-${ENV}" \
  --prefix bootstrap/bootstrap.tfstate \
  --query 'Versions[].{V:VersionId,T:LastModified,Latest:IsLatest}'
aws s3api get-object --bucket "pp-terraform-state-${ENV}" \
  --key bootstrap/bootstrap.tfstate --version-id "<version>" ./bootstrap.tfstate
aws s3api put-object --bucket "pp-terraform-state-${ENV}" \
  --key bootstrap/bootstrap.tfstate --body ./bootstrap.tfstate
terraform plan       # must show no changes
```

**M3 — import the five resources.** This is the documented recovery. Import into a fresh state, then
plan and confirm it shows nothing to do:
```bash
terraform import aws_s3_bucket.tfstate            "pp-terraform-state-${ENV}"
terraform import aws_s3_bucket_versioning.tfstate "pp-terraform-state-${ENV}"
terraform import aws_dynamodb_table.tflocks       pp-terraform-locks
terraform import aws_kms_key.tfstate              "$(aws kms describe-key --key-id alias/pp-terraform-state --query 'KeyMetadata.KeyId' --output text)"
terraform import aws_iam_role.terraform           "pp-terraform-${ENV}"
terraform plan
```
Resource *addresses* depend on the bootstrap module's own naming — read it before importing; the
addresses above are the shape, not necessarily the literals. **A plan showing any create or destroy
after import means the import was incomplete or an address is wrong. Do not apply. Fix the import.**

**M4 — finish the migration to S3.**
```bash
terraform init -migrate-state
git add backend.tf && git commit -m "bootstrap: migrate state to S3"
```

**M5 — release a stale lock**, only after confirming no apply is genuinely running:
```bash
terraform force-unlock <LOCK_ID>
```
Forcing a lock while an apply is in flight produces two concurrent applies against one state, which
is how state gets corrupted — the thing you are recovering from.

**M6 — the bucket itself is gone.** Escalate immediately: this is every stack's state, not just the
bootstrap's. Recovery is per-stack import across the estate, and it is a project rather than a task.
Check for an S3 replica or a backup of the bucket before assuming the worst.

## Rollback / escalation

- **Never apply a bootstrap plan that proposes creating the state bucket.** It is the single
  destructive action in this runbook, and it takes out every stack's state.
- **Never delete or recreate the DynamoDB lock table** while any apply might be running.
- **Nothing here is applied from a laptop.** CI holds no long-lived AWS keys and federates through
  GitHub OIDC into `pp-terraform-<env>`. If you are recovering by hand, you are outside the normal
  path and a second person should be watching.
- **A destructive plan against a stateful resource fails the pipeline** by policy. If your recovery
  requires overriding that, stop and escalate instead.
- **If another stack needs to apply urgently while bootstrap state is lost**, note that it probably
  can: it reads its own state from the bucket, which does not depend on the bootstrap stack knowing
  about itself. Verify rather than assume.

## Verification

```bash
terraform plan       # "No changes. Your infrastructure matches the configuration."
aws s3api head-object --bucket "pp-terraform-state-${ENV}" --key bootstrap/bootstrap.tfstate
aws s3 ls "s3://pp-terraform-state-${ENV}/" --recursive | wc -l    # every stack's state still present
aws dynamodb describe-table --table-name pp-terraform-locks --query 'Table.TableStatus'
```
Then prove another stack can still plan — that is the real test that nothing was harmed:
```bash
terraform -chdir=envs/"${ENV}" init -reconfigure
terraform -chdir=envs/"${ENV}" plan      # no unexpected creates or destroys
```
And confirm the pipeline's own checks still pass on the bootstrap tree:
```bash
terraform fmt -recursive -check
terraform validate
```

## Follow-up

- **How was the state lost?** A local state never migrated, a manual delete, a wrong backend key.
  The answer determines the fix.
- Verify bucket versioning and MFA-delete (or equivalent protection) are enabled on the state
  bucket. Versioning is what made *M2* possible; if it was off, turning it on is the finding.
- Record the imported resource addresses in the bootstrap module's README so the next recovery is a
  copy-paste rather than an investigation.
- Consider a scheduled state backup outside the bucket. State versioning protects against overwrite,
  not against bucket deletion.
- If the recovery was done from a laptop, note it. That is outside the documented workflow, and
  either the workflow needs a break-glass path or the recovery needs to move into CI.
