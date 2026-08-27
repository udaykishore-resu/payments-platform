# RB-033: PAN detector hits — card data in a request

- **Severity:** ticket (P2, `page: "false"`)
- **Alert:** `PANDetectorHits`
  ```promql
  increase(pp_security_events_total{type="SENSITIVE_DATA_IN_REQUEST"}[15m]) > 0
  ```
- **Triggered when:** any request in the last 15 minutes contained what the L1 detector recognises
  as a primary account number, `for: 0m`. `docs/security.md` §9.1 escalates to paging security at
  ≥ 1 per 5 min per merchant.
- **Plane / service:** data · `payment-api`
- **Related:** `docs/security.md` §6.3 (the detector), §9.1,
  `docs/adr/ADR-017-pci-scope-minimisation.md`, [security-pci-incident.md](security-pci-incident.md)

## What this means

The L1 validator runs a Luhn-checked PAN scan over **every string field in every request**. A hit is
rejected with `400 SENSITIVE_DATA_IN_REQUEST`, the value is **not logged**, and a security event is
raised carrying the field path and the length — never the value.

**The detector rejected them, so no card data was stored.** This platform never handles PAN. What
the alert means is that a **merchant is sending card data to the wrong endpoint**, which is their
compliance problem — and becomes ours the moment we accept it, which is exactly what the detector
prevents.

Schema and detector are belt and braces, deliberately: a schema can be bypassed by a future field,
a detector cannot. The schema makes PAN inexpressible (closed `oneOf`, `additionalProperties: false`,
token pattern requiring a leading letter so 13–19 digits can never match); the detector catches
anything that finds a string field anyway.

This is a P2 because nothing leaked. It becomes [security-pci-incident.md](security-pci-incident.md)
— Sev-1 — the moment PAN is found anywhere it was *accepted*: a log, a store, an export.

## Impact

- **No card data stored. No PCI scope change. No money at risk.**
- The merchant's requests are being rejected with 400, so their integration is failing — from their
  side this looks like a platform problem, and they will report it as one.
- The reputational and contractual risk is that the merchant is handling raw card data in a
  checkout that should be using hosted fields, which is their own PCI exposure.

## Immediate triage (first 5 minutes)

P2 pace. Same business day, unless the rate is high.

1. Rate and trend:
   ```promql
   increase(pp_security_events_total{type="SENSITIVE_DATA_IN_REQUEST"}[1h])
   sum(rate(pp_security_events_total{type="SENSITIVE_DATA_IN_REQUEST"}[5m]))
   ```
   Above 1 per 5 min for one merchant, §9.1 pages security.
2. **Which merchant, which field, which endpoint** — the event carries exactly this and no more:
   ```logql
   {namespace="pp-data-plane"} | json | type="security.sensitive_data_in_request.v1"
     | line_format "{{.merchantid}} {{.data.action}} {{.data.detection.rule}} {{.data.evidence}} {{.data.outcome}}"
   ```
   `evidence` carries structured, allowlisted fields only — for PAN detections, the field path and
   the length. **Never** the offending value.
3. Confirm `outcome` is `BLOCKED` on every event. Anything else changes this from a P2 to a Sev-1:
   ```logql
   {namespace="pp-data-plane"} | json | type="security.sensitive_data_in_request.v1"
     | line_format "{{.data.outcome}}"
   ```
4. **Confirm nothing was stored.** This is the check that keeps you in this runbook rather than the
   PCI one — count only, never select the values:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT count(*) FROM pp.payments
   WHERE  created_at > now() - interval '24 hours'
     AND  (method_token ~ '^[0-9]{13,19}$' OR metadata::text ~ '[0-9]{13,19}');
   ```
   Zero. If it is not zero → [security-pci-incident.md](security-pci-incident.md), immediately.
5. Confirm the redaction pipeline did not have to intervene:
   ```promql
   increase(pp_log_redaction_drops_total[1h])
   ```
6. Identify the merchant's integration contact.

## Diagnosis

- **One merchant, one field, steady rate** → their checkout is posting card data instead of a token.
  → *M1*.
- **One merchant, started abruptly** → they deployed a change to their checkout. → *M1*, with the
  timestamp.
- **Hits in `metadata`** → they are putting card data in a free-text field, probably as a "reference".
  The detector inspects metadata values like every other string, which is why it caught it. → *M1*.
- **Hits in `paymentMethodReference.token`** → they are sending a raw PAN as a token. The schema's
  leading-letter pattern already rejects it; the detector is the second catch. → *M1*.
- **Many merchants at once** → suspect a shared SDK or integration partner. → *M2*.
- **Hits with `outcome` other than `BLOCKED`** → the detector did not block. **Sev-1.**
  → [security-pci-incident.md](security-pci-incident.md).
- **A false positive** — a 13–19 digit Luhn-valid number that is not a PAN (some order references
  and account numbers pass Luhn by coincidence) → *M3*. Verify carefully; the cost of a wrong
  "false positive" call is a PCI incident.
- **Hits from our own test traffic** → a test fixture with a real-format card number. Fix the
  fixture; do not exempt the path.

## Mitigation

**M1 — contact the merchant.** This is the mitigation, and it is not a soft one. Tell them:
- which endpoint and which field,
- that the requests are being rejected and why,
- that they must use the gateway's **hosted fields or SDK**, so the card goes from the customer's
  browser to the gateway and never touches either of us,
- that they should treat their own logs and stores for the same window as potentially contaminated —
  if they sent it to us, they probably logged it too.

§9.1's automatic mitigation already flags their integration; the conversation is the manual step.

**M2 — escalate to the integration partner** if several merchants share a cause. Their SDK is the
fix, and one conversation there beats twenty here.

**M3 — investigate a suspected false positive properly.** Compare the field path and length against
what the merchant says they send. **Do not exempt a field from the detector to reduce noise.** The
detector's value is that it covers every string field with no exceptions; the first exception is the
one a real PAN arrives in. If the false-positive rate is genuinely a problem, the fix is a better
rule, reviewed, with a test.

**M4 — if the rate is high enough to matter operationally**, suspend the merchant's integration
until they fix it. That is a commercial conversation and it belongs with the payments product owner,
but the technical position is simple: we cannot accept card data, and they are sending it.

## Rollback / escalation

- **Never disable or narrow the detector to reduce alert noise.** It is one of the two structural
  controls behind the platform's SAQ-A/A-EP assessment.
- **Never log the offending value**, not in an incident channel, not in a ticket, not to "check
  whether it's really a PAN". Field path and length only.
- **Any evidence PAN was accepted rather than blocked** → Sev-1,
  [security-pci-incident.md](security-pci-incident.md), and the QSA/acquirer notification clocks
  start.
- **≥ 1 hit per 5 min for one merchant** → page security per §9.1.
- **Never accept a merchant's request to "just allow it this once".** There is no version of that
  which does not change the platform's PCI scope.

## Verification

```promql
increase(pp_security_events_total{type="SENSITIVE_DATA_IN_REQUEST"}[1h]) == 0
increase(pp_log_redaction_drops_total[1h]) == 0
```
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT count(*) FROM pp.payments
WHERE  created_at > now() - interval '7 days'
  AND  (method_token ~ '^[0-9]{13,19}$' OR metadata::text ~ '[0-9]{13,19}');
```
Zero, over a window wider than the incident. Then confirm the structural controls are still in
place:
```bash
./scripts/check-openapi.sh                                  # schema still forbids the fields
go test ./internal/... -run 'PAN|Redact|Sensitive' -count=1  # detector and serializer tests
```
And confirm the merchant's traffic recovered — their payments succeeding again is the proof they
fixed it, not their assurance that they did.

## Follow-up

- Record the merchant, the field, the count, and the resolution. If it recurs after they say it is
  fixed, that is a commercial escalation.
- Ask whether our API made the mistake easy. A merchant sending a PAN in `metadata` may have been
  looking for a field we do not offer; the answer is usually "use `reference`", and if the
  documentation does not say so plainly, that is our finding.
- If the detector's coverage had a gap, extend it and add the unit test over that field.
- Feed the pattern into merchant onboarding: a merchant whose integration sends card data on day one
  is a merchant who was not shown hosted fields during certification. `platformctl certify` runs the
  certification suite against a merchant's connections — check whether it should be asserting this.
