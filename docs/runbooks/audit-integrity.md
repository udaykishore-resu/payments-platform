# RB-026: Audit chain verification failure

> **Freeze first, investigate second, and repair never.** Do not touch `pp.audit_records` — not to
> fix it, not to test a theory. The chain is the evidence, and a chain repaired in place is
> indistinguishable from a chain successfully tampered with.

- **Severity:** page (P1)
- **Alert:** `AuditChainBroken`
  ```promql
  increase(pp_audit_chain_verification_failures_total[1h]) > 0
  ```
- **Triggered when:** any verification failure in the last hour, `for: 0m`.
- **Plane / service:** control · `control-plane-api`
- **Related:** `docs/compliance.md` §6 and §7.5–7.6, `docs/security.md` §9.1 and §9.3,
  [audit-tamper.md](audit-tamper.md) (the response procedure for a confirmed divergence)

## What this means

The audit log is hash-chained: each record's `prev_digest` links to its predecessor's
`entry_digest`, so that tampering or corruption is **detectable** rather than merely discouraged. A
verification failure is either data corruption or someone editing history. Both are Sev-1 and both
need the chain frozen and preserved before anything else is touched.

Verification runs at several cadences (`docs/compliance.md` §7.5): continuous on insert, rolling
over the last 24 h hourly, anchor verification hourly, a weekly full historical pass, and a daily
cross-account anchor comparison. Which one fired narrows the cause considerably — a
cross-account divergence means one of two independent copies was altered, which is a very different
finding from a single bad digest.

Audit **writes continue** during all of this, buffering to the local WAL. Losing new evidence while
investigating old evidence is the wrong trade. What is frozen is **export**.

This runbook is the detection response. Once a divergence is *confirmed*,
[audit-tamper.md](audit-tamper.md) is the procedure.

## Impact

- **No merchant impact. No payment impact. No money at risk** in the immediate sense.
- **Compliance impact is severe.** The audit log is the evidence for PCI, for the regulator and for
  every dispute. An unverifiable chain means the platform cannot prove what it did.
- Audit **exports are frozen** for the affected tenant, which blocks auditor deliverables.
- If tampering is confirmed, this is a reportable security incident with notification obligations
  per contract.

## Immediate triage (first 5 minutes)

1. **Freeze exports for the affected tenant.** First action, before diagnosis. `audit:export` is
   dual-controlled; suspending it is a control-plane action.
2. Verify the chain yourself and get the first break:
   ```bash
   ./bin/platformctl verify-audit-chain ten_…
   ./bin/platformctl verify-audit-chain ten_… --from=1 --to=0     # 0 means the tail
   ```
   Exit `0` means intact and prints the record count verified. Exit `1` means the chain reported a
   failure and names the first break. Exit `2` means it could not run.
3. **Snapshot the affected range and the current chain head before anything else touches them.**
   Preserve, do not analyse in place:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT max(sequence) AS chain_head, count(*) AS records FROM pp.audit_records;
   -- Preserve the window around the reported break.
   CREATE TABLE audit_incident_snapshot AS
   SELECT * FROM pp.audit_records
   WHERE  sequence BETWEEN <break-500> AND <break+500>;
   ```
4. Sequence gaps, which distinguish a failed transaction from a deletion:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT sequence + 1 AS gap_start, next_seq - 1 AS gap_end
   FROM  (SELECT sequence, lead(sequence) OVER (ORDER BY sequence) AS next_seq
         FROM pp.audit_records) s
   WHERE next_seq - sequence > 1;
   ```
5. Scope — one tenant or many?
   ```promql
   increase(pp_audit_chain_verification_failures_total[24h])
   sum by (tenant) (increase(pp_audit_chain_verification_failures_total[24h]))
   ```
6. **Page security.** This is not a decision to make alone.

## Diagnosis

- **A single digest mismatch, isolated, with no gap** → likely storage corruption or a bug in digest
  computation. Still Sev-1 until disproven. → *M2*.
- **A sequence gap correlating with an error in the logs at that timestamp** → a failed transaction.
  Benign, and the correlation is the proof. → *M3*.
- **A sequence gap with no correlating error** → **not benign.** Records were deleted. →
  [audit-tamper.md](audit-tamper.md).
- **Cross-account anchor divergence** → two independent copies disagree; one of them was altered.
  → [audit-tamper.md](audit-tamper.md), immediately.
- **Failure started at a deploy** → a digest-computation change. → *M1*.
- **Failures across many tenants at once** → systemic: a code defect or a database-level event, more
  likely than coordinated tampering. → *M1*, *M2*.
- **Break-glass session or unusual `rds:*` CloudTrail activity in the window** → treat as tampering
  until disproven. → [audit-tamper.md](audit-tamper.md).

## Mitigation

**M1 — roll back the deploy** if digest computation changed:
```bash
kubectl -n pp-control-plane rollout undo deployment/control-plane-api
kubectl -n pp-control-plane rollout status deployment/control-plane-api --timeout=5m
```
Expected: new records verify. **Records written under the defect still do not**, and the range must
be documented rather than quietly re-verified against a changed algorithm.

**M2 — verify against the independent copies.** Two exist and were written by different paths, which
is the whole point:
- The **WORM S3 export**: `s3://pp-{env}-audit-worm/{tenant_id}/{yyyy}/{mm}/{dd}/`, Object Lock in
  compliance mode with 7-year retention. Compliance mode cannot be shortened or bypassed by any
  principal including root — which is what makes it evidence.
- The **Kafka `pp.audit.v1` stream**, 400-day retention.
```bash
aws s3 ls "s3://pp-${ENV}-audit-worm/${TENANT}/$(date -u +%Y/%m/%d)/"
aws s3 cp "s3://pp-${ENV}-audit-worm/${TENANT}/…/bundle.ndjson" - | head -5
```
If the database diverges from both, the database was altered. If all three diverge in the same way,
suspect the writer.

**M3 — document a benign gap.** Correlate the gap with the failed transaction's error and record
the finding. A gap that is explained is closed; a gap that is assumed benign is a finding deferred.

**M4 — do not repair the chain in place.** Ever. The procedure (`docs/compliance.md` §7.5 step 6) is
to write an `audit.chain_divergence_detected` record documenting the range, the finding and the
reconstruction, and to continue the chain forward from a **new genesis linked to the last verified
anchor**. A silently repaired chain is indistinguishable from a successfully tampered one, which
means the repair destroys the property the chain exists to provide.

## Rollback / escalation

- **Sev-1 from minute zero.** Security is paged, the incident commander is engaged, and the
  compliance owner is notified.
- **Preserve everything before touching anything.** Snapshot the range, the chain head, the
  CloudTrail window, and any break-glass session recordings. Evidence destroyed in the first ten
  minutes is not recoverable later.
- **Confirmed tampering** → [audit-tamper.md](audit-tamper.md) and regulator/QSA notification per
  contract. That notification has a clock on it; start it in parallel with the technical work.
- **Do not stop audit writes.** They buffer to the local WAL and continue by design.
- **Do not delete the snapshot table** when the incident closes. It is evidence; it is retained per
  the incident-evidence policy.

## Verification

```bash
./bin/platformctl verify-audit-chain ten_…     # exit 0: "intact: N record(s) verified"
```
```promql
increase(pp_audit_chain_verification_failures_total[1h]) == 0
```
```sql
SET LOCAL app.tenant_id = 'ten_…';
-- No unexplained gaps remain.
SELECT sequence + 1 AS gap_start, next_seq - 1 AS gap_end
FROM  (SELECT sequence, lead(sequence) OVER (ORDER BY sequence) AS next_seq
      FROM pp.audit_records) s
WHERE next_seq - sequence > 1;
-- The divergence, if any, is documented rather than erased.
SELECT audit_id, sequence, action, occurred_at FROM pp.audit_records
WHERE  action = 'audit.chain_divergence_detected' ORDER BY sequence DESC LIMIT 5;
```
Verification is not complete until the chain verifies **and** the divergence range is documented
**and** exports are unfrozen deliberately, by a named person, after the compliance owner agrees.

## Follow-up

- Full timeline for the postmortem, with the CloudTrail correlation, and it is shared with
  compliance rather than staying in engineering.
- If it was corruption, the finding is the storage or the code path; add the test.
- If it was tampering, the finding is the access path that allowed it, and the fix is that path —
  not more monitoring of it.
- Verify the other cadences still work: if the weekly full-historical pass would have caught this
  months earlier, its cadence is the finding.
- Confirm exports were frozen promptly. If they were not, the freeze mechanism is the second bug.
