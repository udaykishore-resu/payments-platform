# RB-028: Tenant mismatch spike — security events

- **Severity:** page (P1)
- **Alert:** `TenantMismatchSpike`
  ```promql
  sum(rate(pp_security_events_total{type="TENANT_MISMATCH"}[5m])) > 0.1
  ```
- **Triggered when:** tenant-mismatch security events exceed 0.1/s, sustained for 5 minutes.
  `docs/security.md` §9.1 treats **any** occurrence as a Sev-1 candidate; the alert threshold is
  where it becomes a page.
- **Plane / service:** data · `payment-api`
- **Related:** `docs/security.md` §9.1, §9.2 (event schema), §9.3,
  `docs/multi-tenancy.md` §3.3, `docs/adr/ADR-008-pooled-multi-tenancy-with-rls.md`,
  [security-tenant-isolation.md](security-tenant-isolation.md)

## What this means

A request presented a tenant scope inconsistent with its authenticated identity. **The tenant guard
rejected it** — the alert is about *intent*, not about a breach.

Tenant identity comes from the token's `tenant_id` claim and from nothing else (baseline §16.2).
A mismatch means a request tried to act on a resource belonging to a different tenant than its token
names. Behind the guard sit two more layers: the request-scoped tenant context, and Postgres
row-level security, which is `FORCE`d on every tenant table.

So there are two readings and they need different responses:

- **A client bug** — an SDK caching a tenant ID across contexts, a test harness pointed at the wrong
  environment, a misconfigured multi-tenant integration.
- **An attempt at cross-tenant access** — probing, or a compromised credential being used to reach
  data it should not.

"The guard rejected it" is why this is an alert rather than an incident report. It is a page because
telling the two readings apart is urgent, and because `docs/security.md` §9.1's manual step is
explicit: *investigate the client; assume compromise until disproven.*

## Impact

- **No data was disclosed.** The request was blocked before any tenant-scoped query ran.
- **No merchant impact, no money at risk** from the blocked requests themselves.
- The affected client's requests fail. If it is a client bug, that merchant's integration is broken.
- If it is a compromised credential, the real impact is elsewhere and this is the symptom that found
  it.

## Immediate triage (first 5 minutes)

1. Rate, and whether it is one source:
   ```promql
   sum(rate(pp_security_events_total{type="TENANT_MISMATCH"}[5m]))
   sum by (service) (rate(pp_security_events_total{type="TENANT_MISMATCH"}[15m]))
   sum by (type) (rate(pp_security_events_total[15m]))
   ```
   Other event types rising at the same time changes the picture from "one client's bug" to
   "someone is probing".
2. Identify the principal. The security event carries everything needed and deliberately excludes
   the token itself (`data.principal.tokenId` is the `jti`, not the token — a token in a security
   event is a credential in a log):
   ```logql
   {namespace="pp-data-plane"} | json | type="security.tenant_mismatch.v1"
     | line_format "{{.data.principal.id}} {{.data.principal.tenantId}} {{.data.evidence.claimedTenantId}} {{.data.source.ip}} {{.data.source.asn}} {{.data.action}}"
   ```
   The `evidence` block gives `claimedTenantId` and `tokenTenantId` — the two values that disagree.
3. Source characteristics — one IP/ASN or many:
   ```logql
   {namespace="pp-data-plane"} | json | type="security.tenant_mismatch.v1"
     | line_format "{{.data.source.ip}} {{.data.source.asn}} {{.data.source.country}} {{.data.source.userAgent}}"
   ```
   A single ASN with a consistent user agent is a client. Many ASNs is not.
4. **Check the rest of the isolation stack held.** This is the question that decides severity:
   ```promql
   sum by (type) (rate(pp_security_events_total{type=~"AUTH_.*|ISOLATION.*"}[15m]))
   ```
   ```sql
   -- RLS is FORCEd on the tenant tables; verify it, do not assume it.
   SELECT relname, relrowsecurity, relforcerowsecurity
   FROM   pg_class WHERE relname IN
          ('payments','payment_attempts','merchants','audit_records','configurations','workflow_instances');
   ```
   Every row must show `t` for both columns.
5. Is the client's own traffic otherwise normal? A client that suddenly changed behaviour entirely
   is a different story from one with a 1 % mismatch rate.
6. **Page security.** §9.1's response for this signal is "page (Sev-1 candidate)".

## Diagnosis

- **One `principal.id`, one ASN, consistent user agent, steady rate** → a client bug. → *M1*.
- **One principal, rate started abruptly at their deploy time** → a client regression. → *M1*.
- **One principal, many `claimedTenantId` values, enumerating** → probing. Assume compromise.
  → *M2*, then [security-tenant-isolation.md](security-tenant-isolation.md).
- **Multiple principals from one source** → credential stuffing or a compromised integration
  partner. → *M2*.
- **`jti` reuse from two ASNs within 60 s** → token theft; §9.1 pages on this independently and
  the token is denylisted automatically. → *M3*.
- **Mismatch events accompanied by successful cross-tenant reads** → **this is a breach, not an
  attempt.** → [security-tenant-isolation.md](security-tenant-isolation.md), immediately, and
  escalate to Sev-1 without waiting.
- **Any table shows `relforcerowsecurity = f`** → the last line of defence is off. Sev-1 in its own
  right, regardless of this alert. → *M4*.
- **Started at one of our deploys** → we may be constructing the tenant context wrongly. → *M5*.

## Mitigation

**M1 — freeze the implicated client and contact them.** Freezing first is the §9.3 first action for
the isolation class, and it is correct even when the likely cause is a bug: the cost of a frozen
integration for an hour is small, and the cost of assuming good faith wrongly is not. The client key
is already flagged automatically; suspending the client is a control-plane action. Tell them what
was claimed, what their token carries, and when it started.

**M2 — treat as compromise.** Denylist the token family, rotate the client credential
([security-credential-rotation.md](security-credential-rotation.md)), and **preserve the request
traces** before anything expires. Do not delete the old credential before the audit snapshot.

**M3 — token theft path.** The token is denylisted and the client is forced to re-authenticate
automatically. The manual work is establishing how the token was obtained, and that is a security
investigation, not an engineering one.

**M4 — restore RLS.** If `FORCE ROW LEVEL SECURITY` is off on any tenant table, that is a migration
or a manual change that removed the last line of defence:
```bash
./bin/platformctl migrate status
```
Re-apply the migration that establishes it, and treat the removal itself as an incident.

**M5 — roll back our deploy** if tenant-context construction changed:
```bash
kubectl -n pp-data-plane rollout undo deployment/payment-api
```

## Rollback / escalation

- **Assume compromise until disproven.** That is the documented manual step, and it is the right
  default when the alternative is discovering a breach later.
- **Preserve the request trace before anything else.** Traces are sampled and logs are retained on a
  tier schedule; the evidence has a shorter life than the investigation.
- **Never widen the tenant guard, disable it, or add an exception for a client.** A client that
  needs to act across tenants needs a correctly-scoped credential, issued through the normal path.
  An exception in the guard is a permanent hole opened under time pressure.
- **Confirmed cross-tenant *access*, as opposed to attempts** → Sev-1,
  [security-tenant-isolation.md](security-tenant-isolation.md), and the notification obligations in
  §9.3 engage.
- **If the source is an integration partner**, their security team is a party to this, and the
  conversation happens through the established contact, not through a support ticket.

## Verification

```promql
sum(rate(pp_security_events_total{type="TENANT_MISMATCH"}[5m])) == 0
sum by (type) (rate(pp_security_events_total[15m]))
```
```sql
-- RLS intact on every tenant table.
SELECT relname, relrowsecurity, relforcerowsecurity FROM pg_class
WHERE  relname IN ('payments','payment_attempts','merchants','audit_records',
                   'configurations','workflow_instances','reconciliation_exceptions');
```
All `t`. Then confirm **no cross-tenant data was actually returned** during the window — the
negative test is the proof, and running it is not optional:
```bash
go test -tags=integration ./internal/... -run 'Tenant|RLS|Isolation' -count=1
```
And confirm the audit chain covering the window is intact:
```bash
./bin/platformctl verify-audit-chain ten_…
```

## Follow-up

- Record: principal, source characteristics, claimed vs token tenant, event count, and the
  conclusion — bug or attempt. State the conclusion explicitly; "probably a bug" is not a
  conclusion.
- If a client bug: the ticket against their integration, and a check on whether our SDK makes the
  mistake easy. A mistake many clients make is our API's problem.
- If an attempt: the full security investigation, and the question is how the credential was
  obtained.
- Verify the negative RLS test is in the required CI set. It is the control that turns "the guard
  rejected it" from a claim into a tested property.
- Review whether 0.1/s is the right page threshold when §9.1 says *any* occurrence is a Sev-1
  candidate. The gap between the document and the rule is worth closing in one direction or the
  other.
