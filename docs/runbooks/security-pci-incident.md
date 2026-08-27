# RB-031: PAN in scope — PCI incident

> **First action: quarantine, scope the exposure window from the log index, notify the QSA and the
> acquirer per contract.** Quarantine before analysis. Every read of a record containing a PAN
> extends the contaminated surface.

- **Severity:** page (Sev-1)
- **Alert:** `PANDetectorHits` covers PAN in a *request*, which is blocked and is a P2
  ([pan-detector.md](pan-detector.md)). **This runbook is for PAN in a log, in storage, or anywhere
  it was accepted** — which `docs/security.md` §9.1 pages as Sev-1 on any occurrence.
  ```promql
  increase(pp_security_events_total{type="SENSITIVE_DATA_IN_REQUEST"}[15m]) > 0
  rate(pp_log_redaction_drops_total[5m]) > 0
  ```
- **Triggered when:** cardholder data is found anywhere the platform persisted, indexed or exported
  it. One of the five incident classes in `docs/security.md` §9.3.
- **Plane / service:** data · platform-wide, and the log and export estate
- **Related:** `docs/adr/ADR-017-pci-scope-minimisation.md`, `docs/security.md` §6 (logging safety),
  §6.3 (the PAN detector), §9.3, `docs/compliance.md`, [pan-detector.md](pan-detector.md)

## What this means

This platform is assessed at SAQ-A / A-EP level because **cardholder data neither traverses nor is
stored by it** — and that property is structural, not a promise:

- The `PaymentMethodReference` schema is a closed `oneOf` over three variants, each with
  `additionalProperties: false`. There is no field named `pan`, `cardNumber`, `cvv`, `cvc`,
  `track1`, `track2` or `expiry` anywhere in the API — not optional, not deprecated, **absent**.
- Every token field must start with a letter (`^[A-Za-z][A-Za-z0-9_.:-]{7,254}$`). A PAN is 13–19
  digits, so a PAN cannot satisfy the pattern. This is why a bare network token (a DPAN, itself
  PAN-formatted) is referenced by vault reference instead of inline.
- The **L1 detector** runs a Luhn-checked PAN scan over every string field in every request. A hit
  is `400 SENSITIVE_DATA_IN_REQUEST`, the value is not logged, and a security event is raised.
- Logs go through an **allowlist serializer**, not a denylist, and `Secret[T]` redacts itself.

So PAN reaching storage means **several structural controls failed at once**. That is why this is
Sev-1 on a single occurrence, and why the scope question ("how far did it spread") is as urgent as
the containment question.

If a tenant genuinely requires vaulting, that capability lives in a physically and administratively
separate card-vault system with its own SAQ-D assessment, reached through a port. It is not part of
this API and it is not something to enable during an incident.

## Impact

- **PCI scope.** If PAN was stored, the platform's assessment level is wrong, and that has
  contractual and audit consequences well beyond this incident.
- **Notification obligations** to the QSA and the acquirer, per contract, with clocks.
- **The cardholders whose data was exposed**, and whatever downstream obligations follow.
- **No payment impact, no money at risk** from the exposure itself.
- **Every system the contaminated record reached** is now in scope: log aggregation, the SIEM,
  backups, exports, anyone's laptop that ran a query.

## Immediate triage (first 5 minutes)

1. **Quarantine.** Block the index write, isolate the affected records, and stop any export or
   backup job that would copy them further. Containment before analysis — every additional read
   spreads it.
2. **Do not copy, paste, screenshot or forward the value.** Reference records by ID. The evidence
   is the field path and the length, never the value — this is the same rule the security event
   schema follows (`evidence` carries allowlisted fields only, and **never** the offending value
   for PAN or credential detections).
3. **Scope the exposure window from the log index.** This is the named first action:
   ```logql
   {namespace="pp-data-plane"} | json | type="security.sensitive_data_in_request.v1"
     | line_format "{{.time}} {{.tenantid}} {{.data.action}} {{.data.detection.rule}} {{.data.evidence}}"
   ```
   ```promql
   increase(pp_security_events_total{type="SENSITIVE_DATA_IN_REQUEST"}[24h])
   increase(pp_log_redaction_drops_total[24h])
   ```
   `pp_log_redaction_drops_total` is the count of log lines the redaction pipeline refused — a
   non-zero value means the last line of defence was doing work, and where it fired tells you which
   path leaked.
4. Where could it have been persisted? Reference by ID:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT payment_id, created_at FROM pp.payments
   WHERE  created_at BETWEEN '<start>' AND '<end>'
     AND  (method_token ~ '^[0-9]{13,19}$' OR metadata::text ~ '[0-9]{13,19}');
   ```
   Do **not** select the matching values into a terminal or a ticket.
5. **Notify the QSA and the acquirer per contract**, and page security and compliance. The clock is
   contractual and it starts now.
6. Identify the source merchant and the endpoint they used.

## Diagnosis

- **The L1 detector fired and the request was blocked** → **no PAN was stored.** This is
  [pan-detector.md](pan-detector.md), a P2, and the response is a merchant conversation. Confirm
  this before escalating further — it is the common case by a wide margin.
- **PAN found in a log line** → the allowlist serializer was bypassed, probably by a new log
  statement that formatted a struct wholesale. → *M2*.
- **PAN found in `metadata`** → a merchant put card data in a free-text field and the detector did
  not catch it. Check whether the detector covers that field. → *M3*.
- **PAN found in `method_token`** → the token pattern was not enforced on that path. → *M3*, and
  this is a validation defect, because the pattern exists precisely to make it impossible.
- **PAN found in an export or a backup** → the contaminated surface includes systems outside the
  platform. → *M4*, and the scope grows.
- **PAN found in a trace or a span attribute** → span attributes bypassed the serializer. → *M2*.
- **PAN reached the SIEM** → it is now in a 400-day hot store and a 7-year S3 archive. → *M4*,
  urgently, and the archive's Object Lock may make deletion impossible, which is a conversation with
  compliance rather than an engineering task.

## Mitigation

**M1 — stop the source.** Block the merchant's ability to send it: suspend the integration, or
reject at the edge. Contact them and confirm their checkout uses the gateway's hosted fields. §9.1's
automatic mitigation flags the merchant integration; the manual step is the conversation.

**M2 — fix the logging path and purge.** Roll back the change that introduced the unredacted log
statement:
```bash
kubectl -n pp-data-plane rollout undo deployment/<service>
```
Then purge the affected log index entries. Coordinate with compliance: some stores are WORM and
cannot be purged, and knowing which is part of the scope.
```bash
./scripts/check-secrets.sh
```

**M3 — fix the validation gap.** The schema and the detector are belt and braces: a schema can be
bypassed by a future field, a detector cannot. If a field escaped the detector, extend the detector
first (it covers every string field, so a miss is a bug), then tighten the schema.
```bash
./scripts/check-openapi.sh
```

**M4 — scope and purge downstream.** Log aggregation, the SIEM, backups, exports, any local copies.
Produce the list before purging, because the list is the notification basis. Note that the WORM
audit export uses Object Lock in **compliance mode** with 7-year retention, which cannot be
shortened or bypassed by any principal including root — if PAN reached it, deletion is not
available and compliance must be told immediately.

**M5 — do not attempt to "encrypt it in place" or "tokenize it now".** Both are handling cardholder
data in a system not assessed to handle it, which enlarges the problem. Quarantine, scope, notify.

## Rollback / escalation

- **Sev-1, security- and compliance-led, from the first minute.**
- **QSA and acquirer notification per contract**, with clocks. Start them in parallel with the
  technical work; do not wait for a complete scope.
- **Never move the value.** Not into a ticket, not into a chat message, not into a spreadsheet for
  analysis. Reference by ID and field path.
- **Never widen the API to accept card data**, under any commercial pressure. ADR-017 is the reason
  the platform's assessment level is what it is, and reversing it is a strategic decision, not an
  incident action.
- **If the exposure reached a WORM store**, escalate to compliance immediately — the remediation
  options are legal, not technical.
- **If the source is a merchant's integration**, their PCI obligations are engaged too, and that is
  a conversation their compliance function needs to be in.

## Verification

```promql
increase(pp_security_events_total{type="SENSITIVE_DATA_IN_REQUEST"}[1h]) == 0
rate(pp_log_redaction_drops_total[5m]) == 0
```
```sql
SET LOCAL app.tenant_id = 'ten_…';
-- No PAN-shaped values remain in the fields that could hold one. Count only.
SELECT count(*) FROM pp.payments
WHERE  method_token ~ '^[0-9]{13,19}$' OR metadata::text ~ '[0-9]{13,19}';
```
Zero. Then confirm the structural controls are intact:
```bash
./scripts/check-openapi.sh      # the schema still forbids the fields
./scripts/check-secrets.sh      # no material anywhere it should not be
go test ./internal/... -run 'PAN|Redact|Sensitive' -count=1
```
Verification is complete when: the source is stopped; every contaminated store is purged or
documented as un-purgeable; the detector and schema gaps are closed with tests; the scope list is
final; and the QSA and acquirer have been notified per contract.

## Follow-up

- The scope report for the QSA: what was exposed, where it reached, for how long, and what was
  purged. Accuracy matters more than speed here, and the two are in tension — say which you traded.
- **The finding is which structural control failed.** ADR-017's claim is that PAN cannot arrive.
  If it did, the claim has an exception, and the exception must be closed rather than documented.
- Add the regression test at the layer that failed: schema (`./scripts/check-openapi.sh`), detector
  (unit test over the field), or serializer (`internal/platform/…/leak_test.go` is the existing
  pattern).
- Review the assessment level with compliance. If PAN was stored, the SAQ-A/A-EP basis was not true
  for that window, and that is a disclosure question.
- Re-examine why the merchant sent it. A merchant sending card data to the wrong endpoint is their
  compliance problem and becomes ours the moment we accept it — so the durable fix is making
  acceptance impossible, not asking them again.
