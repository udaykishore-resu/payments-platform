# RB-030: Credential compromise — emergency rotation

> **First action: rotate via the dual-run workflow, denylist the token family, and — critically —
> do NOT delete the old credential before the audit snapshot.** The old credential is how you find
> out what was done with it. Deleting it first destroys the investigation to close the hole a few
> minutes sooner.

- **Severity:** page (Sev-1)
- **Alert:** no dedicated rule for compromise itself. Reached from §9.1 detections: a Secrets
  Manager read by an unexpected principal (**page**), `jti` reuse from two ASNs within 60 s
  (**page**), an auth-failure rate above 95 % for a client (**page**), credential age past 90 days
  (page), or an external report. One of the five incident classes in `docs/security.md` §9.3.
- **Triggered when:** a credential is believed to be in the hands of someone who should not have it.
  Belief is enough; confirmation is what this produces.
- **Plane / service:** control and data · platform-wide
- **Related:** `docs/security.md` §3.4 (token lifetime, refresh, revocation), §5 (secrets
  management), §5.3 (rotation), §9.1, §9.3, `cmd/platformctl/audit.go`'s evidence-preservation
  note, [security-events.md](security-events.md), [audit-tamper.md](audit-tamper.md)

## What this means

"Credential" covers several things with different blast radii, and the first job is to say which:

| Kind | Where it lives | Blast radius |
|---|---|---|
| Tenant OAuth2 client secret | Secrets Manager, referenced as `secret://…` | That tenant's API access |
| Gateway API credential | Secrets Manager, per merchant × gateway | Money movement at that gateway |
| Webhook signing secret | Secrets Manager, per merchant | Ability to forge state transitions |
| Database / broker credential | Secrets Manager | The store |
| A bearer token (not a credential, but often the thing leaked) | Nowhere — it is minted | Its remaining lifetime, bounded by `exp`, `MaxTokenAge` and revocation |

Two design facts shape the response:

- **No credential material is ever in an environment variable.** `docs/security.md` §5.2 forbids it,
  an admission policy rejects any pod spec whose variable name matches the credential pattern, and
  CI runs the same check over `deployments/k8s/**` and `helm/**`. Every `PP_SECRETS_*` variable is a
  reference or a knob, never material. So "which pods have the secret" is answered by
  "none of them, they resolve it at point of use".
- **Rotation is dual-run**: the new credential is published and accepted alongside the old one, then
  the old one is retired. The overlap is what makes rotation not an outage.

## Impact

- **Depends entirely on the kind.** A tenant client secret is that tenant's API surface. A gateway
  credential is the ability to move that merchant's money. A webhook signing secret is the ability
  to forge payment outcomes.
- **During rotation: nothing.** The dual-run overlap means both credentials are accepted, so
  correctly-implemented rotation has no merchant impact.
- **The real impact is whatever was done with the credential before you noticed**, which is why the
  audit snapshot comes before the deletion.

## Immediate triage (first 5 minutes)

1. **Identify the credential and its scope.** Which reference, which tenant, which merchant, which
   gateway. Everything below depends on this.
2. **Take the audit snapshot first.** This is the step `cmd/platformctl/audit.go` points at this
   runbook for:
   ```bash
   ./bin/platformctl verify-audit-chain ten_…
   ```
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   -- Everything this principal did, preserved before anything is revoked.
   CREATE TABLE cred_incident_snapshot_<id> AS
   SELECT * FROM pp.audit_records
   WHERE  actor_id = '<principal>' AND occurred_at > '<suspected compromise>';

   SELECT action, resource_type, outcome, count(*), min(occurred_at), max(occurred_at)
   FROM   pp.audit_records
   WHERE  actor_id = '<principal>' AND occurred_at > '<suspected compromise>'
   GROUP  BY action, resource_type, outcome ORDER BY 4 DESC;
   ```
3. Who read the secret, and from where:
   ```bash
   aws cloudtrail lookup-events --start-time "$WINDOW_START" --end-time "$WINDOW_END" \
     --lookup-attributes AttributeKey=EventName,AttributeValue=GetSecretValue \
     --query 'Events[].{t:EventTime,u:Username,src:CloudTrailEvent}'
   ```
   A read by an unexpected principal is the §9.1 detection; it is detect-only, because blocking is
   IAM's job.
4. Token-level evidence:
   ```logql
   {namespace="pp-data-plane"} | json | data_principal_tokenId != ""
     | line_format "{{.data.principal.id}} {{.data.principal.tokenId}} {{.data.source.ip}} {{.data.source.asn}} {{.data.action}}"
   ```
   The `jti`, never the token. A token in a log is a credential in a log.
5. Credential age across the estate, since a compromise often coincides with a rotation that never
   completed:
   ```promql
   (time() - pp_gateway_credential_created_timestamp_seconds) / 86400
   sum by (type) (rate(pp_security_events_total[1h]))
   ```
6. **Page security.**

## Diagnosis

- **Secrets Manager read by an unexpected principal** → the credential is out. Assume full
  compromise of its scope. → *M1*, *M2*.
- **`jti` reuse from two ASNs within 60 s** → token theft, not credential theft. The token is
  denylisted automatically and the client is forced to re-authenticate. → *M3*.
- **Auth-failure rate above 95 % for a client** → usually a rotation the tenant did not complete,
  not an attack. Check the rotation state before escalating. → *M4*.
- **Credential age above 90 days** → the automatic rotation at 75 days did not run. Not a
  compromise, but the control failed. → *M4*.
- **A credential found in a repository, a log, a ticket or a chat** → treat as compromised from the
  moment it was written there, not from when it was found. → *M1*.
- **Audit shows actions by the principal that the tenant does not recognise** → confirmed misuse.
  → *M1*, *M2*, and the scope of those actions becomes the incident.
- **The credential is a gateway credential and payments were made** → money moved under a
  compromised credential. [reconciliation.md](reconciliation.md) runs in parallel.
- **The credential is a webhook signing secret** → forged webhooks could have driven state
  transitions. Check `pp.inbound_webhooks` for the window against the gateway's own delivery log.

## Mitigation

**M1 — rotate via the dual-run workflow.** For a gateway credential:
```
POST /v1/gateways/{gatewayId}/credentials:rotate
```
The workflow publishes the new credential, accepts both during the overlap, then retires the old
one. **Do not delete the old credential before the audit snapshot is taken** (step 2 above). This
is stated in `docs/security.md` §9.3 and again in `platformctl`'s own output, because it is the step
people skip under pressure.

**M2 — denylist the token family.** Every token minted from the compromised credential must stop
working, not merely expire. Token lifetime is short and `MaxTokenAge` bounds it further, but "it
will expire soon" is not a control. Revocation is checked on every request against a ≤30 s-stale
cache with priority invalidation.

**M3 — token theft path.** The `jti` is denylisted and the client re-authenticates automatically.
The manual work is establishing how the token was obtained. Note that emergency **key** revocation
is a deploy-time issuer-list change, not something to wait for a JWKS refresh to deliver.

**M4 — complete the rotation.** If a half-finished rotation is the cause, finish it rather than
rolling back: the tenant has one credential that works and one that does not, and rolling back
leaves them where they started. Verify both sides accept before retiring the old one.

**M5 — verify no credential material is anywhere it should not be:**
```bash
./scripts/check-secrets.sh
./scripts/check-licences.sh
```
`check-secrets.sh` is the same scanner the admission policy and the config redactor share, so a
value it flags is a value that would also be rejected in a pod spec.

**M6 — if the compromise reached the audit path** → [audit-tamper.md](audit-tamper.md). A
credential with write access to `pp.audit_records` is the worst version of this incident.

## Rollback / escalation

- **Never delete the old credential before the audit snapshot.** Repeated because it is the step
  that gets skipped and it cannot be undone.
- **Never put a credential in an environment variable, a ConfigMap, a ticket or a chat message**,
  including during the incident. The rotation workflow exists so nobody has to handle material.
- **Never disable signature verification or authentication** to restore service during a rotation.
  If the overlap failed, fix the overlap.
- **Money moved under a compromised gateway credential** → Sev-1 with the payments product owner
  and finance. [reconciliation.md](reconciliation.md) for the payment-level work.
- **A compromised platform-level credential (database, broker)** → assume the store is compromised
  until disproven, and involve the infrastructure owner immediately.
- **If the credential came from a compromised dependency or image** →
  [security-supply-chain.md](security-supply-chain.md) in parallel.

## Verification

```promql
(time() - pp_gateway_credential_created_timestamp_seconds) / 86400 < 1
sum(rate(pp_security_events_total{type=~"AUTH_.*"}[15m])) == 0
sum by (status) (rate(pp_http_requests_total{status="401"}[5m]))
```
```bash
./scripts/check-secrets.sh                    # exits 0
./bin/platformctl verify-audit-chain ten_…    # exits 0
```
```sql
SET LOCAL app.tenant_id = 'ten_…';
-- No further activity by the compromised principal after revocation.
SELECT count(*) FROM pp.audit_records
WHERE  actor_id = '<principal>' AND occurred_at > '<revocation time>';
-- The snapshot is preserved.
SELECT count(*) FROM cred_incident_snapshot_<id>;
```
Zero for the first, non-zero for the second. Then confirm the tenant's legitimate traffic recovered:
401 rates back to baseline and their payments flowing.

## Follow-up

- The scope list: every action taken with the credential during the exposure window, from the audit
  snapshot. This is what notification is based on.
- **How the credential leaked is the finding.** A credential in a repository is a scanner gap; in a
  log, a redaction gap; read by an unexpected principal, an IAM gap. Fix the class, not the
  instance.
- If automatic rotation at 75 days did not run, that control is broken and it is the more valuable
  fix — a rotated credential has a much smaller exposure window when this happens again.
- Verify the dual-run overlap length is adequate. A rotation that causes an outage is a rotation
  people avoid, and avoided rotations are how credentials reach 90 days.
- Retain the evidence package per the incident-evidence policy.
