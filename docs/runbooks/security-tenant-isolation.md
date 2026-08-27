# RB-029: Suspected cross-tenant leak

> **First action, before anything else: freeze the implicated client, verify RLS with the negative
> test, and preserve the request trace.** That ordering is `docs/security.md` §9.3 and it is the
> runbook. Preservation comes before investigation because traces and logs age out on their own
> schedule.

- **Severity:** page (Sev-1)
- **Alert:** no dedicated rule. Reached from `TenantMismatchSpike`
  ([security-events.md](security-events.md)), from a merchant report, from an audit finding, or
  from the daily isolation checks. One of the five incident classes in `docs/security.md` §9.3.
- **Triggered when:** there is reason to believe one tenant's data was returned to another. A
  *suspicion* is enough to start this runbook; confirmation is what it produces.
- **Plane / service:** data and control · platform-wide
- **Related:** `docs/multi-tenancy.md` (the isolation model), `docs/security.md` §1, §4, §9.3,
  `docs/adr/ADR-008-pooled-multi-tenancy-with-rls.md`, [security-events.md](security-events.md),
  [audit-integrity.md](audit-integrity.md)

## What this means

Tenant isolation is defence in depth, and knowing which layer failed is the whole investigation:

1. **The token.** `tenant_id` is the *only* source of tenant identity (baseline §16.2). No header,
   no path parameter, no body field can override it.
2. **The tenant guard** at the transport boundary, which raises `TENANT_MISMATCH` and rejects.
3. **The request-scoped tenant context**, which the repositories refuse to query without.
4. **Postgres row-level security**, `ENABLE` **and** `FORCE`, on every tenant table. `FORCE` matters:
   without it the table owner bypasses the policy, and the application role is often the owner.
5. **Per-tenant encryption keys** for siloed tenants; the environment CMK for pooled.

A leak means at least one layer failed *and* every layer behind it also failed. That is why this is
Sev-1 regardless of how few records were involved: the depth is the design, and a leak means the
depth was not there.

## Impact

- **Confirmed leak:** one tenant's business data was disclosed to another. Depending on content:
  merchant identities, payment amounts, references, configuration. **The platform never handles
  PAN**, so cardholder data is out of scope by construction — but that is the only comfort.
- **Regulatory:** GDPR and contractual notification obligations engage, with clocks.
- **No money moved incorrectly.** This is a confidentiality incident, not an integrity one, unless
  the investigation says otherwise.
- **Suspected but unconfirmed:** the impact is the investigation, and the cost of assuming it away.

## Immediate triage (first 5 minutes)

1. **Freeze the implicated client.** First action. Suspend the API client through the control plane.
   The cost of a frozen integration is small and reversible; the cost of leaving it live while you
   think is not.
2. **Preserve the request trace.** Before anything expires:
   ```logql
   {namespace="pp-data-plane"} | json | trace_id="<trace>"
     | line_format "{{.ts}} {{.service}} {{.route}} {{.status}} {{.tenant_id}} {{.error_code}}"
   ```
   Export it to the incident evidence store, do not rely on retention.
3. **Verify RLS with the negative test.** This is the named first action and it is a command, not a
   review:
   ```bash
   go test -tags=integration ./internal/... -run 'Tenant|RLS|Isolation' -count=1 -v
   ```
   ```sql
   SELECT relname, relrowsecurity AS enabled, relforcerowsecurity AS forced
   FROM   pg_class
   WHERE  relname IN ('payments','payment_attempts','merchants','audit_records','configurations',
                      'workflow_instances','reconciliation_exceptions','outbox_events','workflow_dlq');
   ```
   Every row must be `t`/`t`. Any `f` is the answer and is itself a Sev-1.
4. Establish the scope from the audit log — every read is audited:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT audit_id, sequence, actor_type, actor_id, action, resource_type, resource_id,
          outcome, occurred_at, trace_id
   FROM   pp.audit_records
   WHERE  occurred_at BETWEEN '<start>' AND '<end>'
     AND  actor_id = '<implicated principal>'
   ORDER  BY sequence;
   ```
5. Correlate with security events:
   ```promql
   sum by (type) (rate(pp_security_events_total[1h]))
   sum(rate(pp_security_events_total{type="TENANT_MISMATCH"}[5m]))
   ```
6. **Page security and the data-protection owner.** The notification clock starts on suspicion, not
   on confirmation.

## Diagnosis

- **RLS is off or not forced on a table** → the last layer was absent. Every request through a code
  path that missed the tenant context would have leaked. → *M1*, and scope by that code path.
- **RLS intact, but a query ran without a tenant context** → the repository guard was bypassed,
  probably by a new code path that does not use the unit of work. → *M2*.
- **RLS intact, tenant context set correctly, data still crossed** → the tenant context itself was
  wrong: built from a header, a parameter, or a cached value rather than from the token claim.
  → *M2*. This is the most serious variant, because the identity itself was wrong.
- **Only `TENANT_MISMATCH` rejections, no successful cross-tenant reads** → **no leak.** The guard
  worked. → [security-events.md](security-events.md), and downgrade with the evidence.
- **A siloed tenant's data reached a pooled query** → the connection factory or the cache namespace
  routed wrongly. Check the tier and the routing record.
- **Cache keys without a tenant prefix** → a cross-tenant cache hit, which leaks without ever
  touching the database. Check the key construction.
- **The leak was via a metric label, a log line or an export** → not the query path at all. Metric
  labels never carry `tenant_id` by design ([cardinality.md](cardinality.md)); if one did, that is
  the vector.

## Mitigation

**M1 — restore RLS.** Re-apply the migration that establishes `ENABLE`/`FORCE` on the affected
table:
```bash
./bin/platformctl migrate status
./scripts/migrate.sh status --dsn "$PP_DSN"
```
Then treat the *removal* as its own incident: a migration or a manual change turned off the last
line of defence, and how that shipped is the finding.

**M2 — roll back the code path**, and block the route if rollback is not immediate:
```bash
kubectl -n pp-data-plane rollout undo deployment/payment-api
kubectl -n pp-data-plane rollout status deployment/payment-api --timeout=5m
```
If a single route is implicated and rollback is slow, removing that route from the ingress is a
legitimate emergency measure. Merchants losing one endpoint is better than tenants losing
isolation.

**M3 — scope the disclosure precisely.** From the audit log, which records every authorized business
action, and from the request traces. The output is a list: which tenant's records, how many, to
whom, when. That list is what notification is based on, and "we think it was small" is not a list.

**M4 — rotate credentials for the implicated client**
([security-credential-rotation.md](security-credential-rotation.md)) if a credential is in scope.

**M5 — verify no other tenant is affected.** Run the negative test across the estate and re-verify
the audit chains:
```bash
for t in $(cat tenants.txt); do ./bin/platformctl verify-audit-chain "$t" || echo "BREAK: $t"; done
```

## Rollback / escalation

- **Sev-1, security-led, data-protection owner engaged from the start.** Notification clocks are
  legal, not internal.
- **Do not un-freeze the client** until the scope is established and the path is closed.
- **Do not "test the theory" against production data.** Reproduce in an isolated environment; the
  test suite has the negative case for exactly this reason.
- **Do not disable RLS to investigate.** It will be suggested, because RLS makes ad-hoc querying
  awkward. Use a properly scoped session instead.
- **Notify affected tenants** per contract and regulation. That decision is the data-protection
  owner's; the engineering job is to make the scope list accurate.
- **If the disclosure included anything the platform should never have held** (a PAN in a metadata
  field, for instance) → [security-pci-incident.md](security-pci-incident.md) runs in parallel.

## Verification

```bash
go test -tags=integration ./internal/... -run 'Tenant|RLS|Isolation' -count=1
```
```sql
SELECT relname, relrowsecurity, relforcerowsecurity FROM pg_class
WHERE  relname IN ('payments','payment_attempts','merchants','audit_records','configurations',
                   'workflow_instances','reconciliation_exceptions','outbox_events','workflow_dlq');
```
All `t`/`t`.
```promql
sum(rate(pp_security_events_total{type="TENANT_MISMATCH"}[5m])) == 0
```
```bash
./bin/platformctl verify-audit-chain ten_…
```
Verification is complete when: the negative test passes; RLS is forced on every tenant table; the
code path is fixed or rolled back; the scope list is final; the audit chains verify; and the
data-protection owner has signed off on the notification decision.

## Follow-up

- The blameless postmortem states which layers failed and why the ones behind them did not catch it.
  A single-layer failure with the rest holding is a near miss and should be written up as one.
- **The durable fix is a test, not a review.** The negative RLS test must be in the required CI set,
  and it must cover the shape that leaked.
- If a new code path bypassed the unit of work, add the architecture check that makes that
  impossible: `./scripts/check-architecture.sh` enforces the dependency rule mechanically, and the
  same approach applies here.
- Review the layer that failed for other instances of the same shape. A missing tenant prefix on one
  cache key usually means there are others.
- Notification and evidence retention follow the incident-evidence policy; the package is not
  deleted when the incident closes.
