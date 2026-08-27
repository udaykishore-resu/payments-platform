# RB-027: Audit tamper — confirmed chain divergence

> **This is a Sev-1 security incident from the first minute.** Freeze exports, preserve everything,
> and do not repair the chain. Reaching for a fix before preserving the evidence is the single most
> damaging thing anyone can do here.

- **Severity:** page (Sev-1)
- **Alert:** reached from `AuditChainBroken`
  ([audit-integrity.md](audit-integrity.md)) once a divergence is **confirmed**, or from the daily
  cross-account anchor comparison, or from the weekly full-historical verification.
  ```promql
  increase(pp_audit_chain_verification_failures_total[1h]) > 0
  ```
- **Triggered when:** verification confirms that the stored chain diverges from an independent copy
  or from a verified anchor. One of the five incident classes in `docs/security.md` §9.3.
- **Plane / service:** control · `control-plane-api`, and the audit estate
- **Related:** `docs/compliance.md` §6.4, §7.5 (the procedure, authoritative) and §7.6 (WORM
  export), `docs/security.md` §9.1 and §9.3, [audit-integrity.md](audit-integrity.md)

## What this means

The hash chain exists so that tampering is detectable. Detection has now happened. Either records
were altered or deleted, or two independently written copies of the same history disagree.

The **first action** in `docs/security.md` §9.3 for this class is: *freeze exports, recompute the
chain from the last anchor, preserve the divergence range.* That ordering is the runbook.

Three properties make the investigation tractable:

- **Anchors.** The chain is anchored periodically and each anchor is KMS-signed. The last verified
  anchor is a trusted floor; without it a binary search has nowhere to start.
- **Independent copies.** The WORM S3 export (Object Lock, **compliance mode**, 7-year retention —
  compliance mode cannot be shortened or bypassed by any principal including root, which is why
  governance mode would be useless as evidence) and the Kafka `pp.audit.v1` stream (400 d) are
  written by different paths.
- **Cross-account anchors.** Compared daily. A divergence between accounts means one of them was
  altered, and it tells you which side to look at.

## Impact

- **No merchant or payment impact.** Nothing about this stops money moving.
- **The platform's ability to prove what it did is in question**, for the affected range. That is
  the impact, and it is not smaller than an outage — it is slower.
- Audit exports frozen; auditor deliverables blocked.
- Regulator/QSA notification obligations engage per contract.
- If tampering is confirmed, an insider or an intrusion is in scope, which changes who is in the
  room.

## Immediate triage (first 5 minutes)

Follow `docs/compliance.md` §7.5 exactly. Summarised, with the commands:

1. **Freeze audit exports for the affected tenant.** Do **not** stop audit *writes* — they buffer
   to the local WAL and continue.
2. **Snapshot the affected table range and the current chain head before anything else touches
   them:**
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT max(sequence) AS chain_head, count(*) AS records FROM pp.audit_records;
   CREATE TABLE audit_tamper_snapshot_<incident> AS
   SELECT * FROM pp.audit_records;      -- the whole tenant range; storage is cheaper than evidence
   ```
3. **Locate the first divergent sequence** by binary search from the last **verified anchor**:
   ```bash
   ./bin/platformctl verify-audit-chain ten_… --from=<anchor_seq> --to=<midpoint>
   ./bin/platformctl verify-audit-chain ten_… --from=<midpoint>   --to=0
   ```
   Exit `0` intact, exit `1` the chain reported a break and names it, exit `2` could not run. Halve
   until the first divergent sequence is isolated.
4. **Correlate that timestamp window** with:
   ```bash
   aws cloudtrail lookup-events --start-time "$WINDOW_START" --end-time "$WINDOW_END" \
     --query 'Events[?contains(EventName,`Rds`)||contains(EventName,`Kms`)||contains(EventName,`S3`)].{t:EventTime,n:EventName,u:Username}'
   kubectl -n pp-control-plane rollout history deployment/control-plane-api
   ```
   plus the Postgres audit-extension DDL logs and any break-glass session recordings in the window
   (break-glass sessions are recorded and reviewed within 24 h, mandatory).
5. **Page security and the compliance owner.** Not after triage — now.

## Diagnosis

- **Cross-account anchor divergence** → one of two independent copies was altered. Compare both
  against the WORM export to establish which. Strongest evidence of deliberate action.
- **A sequence gap with no correlating failed transaction** → deletion.
- **A digest mismatch with intact sequence numbers** → in-place modification of a record's content.
- **CloudTrail shows `rds:*` activity by an unexpected principal in the window** → the access path
  is identified; treat as intrusion until disproven.
- **A break-glass session overlaps the window** → the session recording is the primary evidence. It
  may be entirely legitimate; establish that from the recording, not from the person.
- **Divergence starts exactly at a deploy** → more likely a defect than an attack, but it is
  investigated as an attack until the code proves otherwise.
- **All three copies agree with each other and disagree with an anchor** → the anchor or its KMS
  signature is the problem. Verify the signature before concluding the records are wrong.

## Mitigation

**M1 — reconstruct the authoritative range** from the two independent copies:
```bash
aws s3 ls "s3://pp-${ENV}-audit-worm/${TENANT}/" --recursive | tail -20
aws s3 cp "s3://pp-${ENV}-audit-worm/${TENANT}/…/bundle.ndjson" ./evidence/
# each bundle carries a manifest: sequence range, record count, start and end digests, anchor ref
```
plus the Kafka `pp.audit.v1` stream for the same range. Two copies written by different paths
agreeing is strong evidence of what the truth was.

**M2 — do NOT repair the chain in place.** The procedure is:
1. Write an `audit.chain_divergence_detected` record documenting the range, the finding and the
   reconstruction.
2. Continue the chain forward from a **new genesis linked to the last verified anchor**.

A silently repaired chain is indistinguishable from a successfully tampered one. Repairing it
destroys exactly the property that made this detectable.

**M3 — close the access path.** If a principal, credential or role was used, revoke it now:
[security-credential-rotation.md](security-credential-rotation.md). If it was a human session,
that is an HR and legal matter as well as a technical one, and it stops being the on-call's
decision.

**M4 — verify the other tenants.** A tamper that touched one chain may have touched others:
```bash
for t in $(cat tenants.txt); do ./bin/platformctl verify-audit-chain "$t" || echo "BREAK: $t"; done
```

**M5 — preserve for the regulator.** The snapshot table, the S3 bundles, the CloudTrail extract, the
session recordings and the verification output form the evidence package. It is retained per the
incident-evidence policy, not deleted when the incident closes.

## Rollback / escalation

- **Sev-1, security-led, from the first minute.** Engineering supports the investigation; it does
  not run it.
- **Regulator/QSA notification per contract.** There is a clock; start it in parallel.
- **Do not delete, alter or "clean up" any audit record**, including the snapshot tables. If a
  cleanup job would touch them, disable it now.
- **Do not un-freeze exports** until the compliance owner agrees, in writing, and the reconstruction
  is documented.
- **Do not repair.** Stated three times in this runbook because the instinct to make the check pass
  is very strong and it is the one thing that cannot be undone.
- **If the tamper is confirmed as insider action**, follow the organisation's insider-threat
  process. Access revocation happens before notification of the individual.

## Verification

```bash
./bin/platformctl verify-audit-chain ten_…      # exit 0 forward from the new genesis
```
```promql
increase(pp_audit_chain_verification_failures_total[1h]) == 0
```
```sql
SET LOCAL app.tenant_id = 'ten_…';
-- The divergence is documented, not erased.
SELECT audit_id, sequence, action, reason, occurred_at
FROM   pp.audit_records WHERE action = 'audit.chain_divergence_detected'
ORDER  BY sequence DESC LIMIT 5;
-- The snapshot is preserved.
SELECT count(*) FROM audit_tamper_snapshot_<incident>;
```
Verification is complete only when: the chain verifies forward from the new genesis; the divergence
record exists and names the range; the reconstruction is documented; the access path is closed; the
evidence package is stored; and the compliance owner has approved un-freezing exports.

## Follow-up

- The blameless postmortem is shared with compliance and, where contractually required, with the
  regulator. It states what was altered, over what range, by what path, and what is now different.
- The durable fix is the **access path**, not more verification. Verification worked — that is why
  you are reading this.
- Review who can reach `pp.audit_records` at all. Direct database access to the audit table by any
  application principal is a finding in itself.
- Verify that anchoring cadence and cross-account comparison would have caught this sooner. If the
  weekly pass found what the hourly pass missed, the hourly pass has a scope gap.
- Confirm break-glass review actually happens within 24 h. This incident is the argument.
