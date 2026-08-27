# RB-015: Control-plane availability burn

- **Severity:** ticket (P2, `page: "false"`)
- **Alert:** `ControlPlaneAvailabilityBurn`
  ```promql
  pp:control_plane_error:ratio_rate1h > 14.4 * 0.001
    and pp:control_plane_error:ratio_rate5m > 14.4 * 0.001
  ```
- **Triggered when:** the control plane is burning its 99.9 % availability budget at 14.4× over
  both a 1 h and a 5 m window, for 5 minutes.
- **Plane / service:** control · `control-plane-api`
- **Related:** `docs/adr/ADR-007-control-plane-data-plane-independence.md`,
  `docs/control-plane.md`, `docs/spec/00-design-baseline.md` §15,
  [config-staleness.md](config-staleness.md), [onboarding-stuck.md](onboarding-stuck.md)

## What this means

The control plane owns merchants, configuration, gateway registration and credentials. It is
**not** in the payment path — that independence is ADR-007, and it is why this is a ticket and not
a page even though a whole plane is degraded.

The data plane is unaffected **by design**: it runs fail-static on its last-known-good
configuration snapshot until `max_config_staleness` (15 minutes in production), after which new
merchants are rejected while existing ones continue.

**That 15-minute window is the clock on this ticket.** It is a P2 that becomes a P1 if it is not
resolved inside the window, and the mechanism that turns it into one is
[config-staleness.md](config-staleness.md).

The control plane's SLO is 99.9 %, one nine lower than the payment API's 99.99 %, because it is
explicitly allowed to be less available. Its unavailability is designed to be survivable.

## Impact

- **Payments: unaffected**, for 15 minutes. This is the single most important sentence to put in
  the incident channel, because the instinct on seeing "control plane down" is to assume the
  platform is down.
- **Merchant onboarding stops.** Workflows that need a control-plane call park and retry; the
  automated portion's 30-minute SLO starts burning ([onboarding-stuck.md](onboarding-stuck.md)).
- **Configuration cannot be published or rolled back.** If a bad configuration is live, the
  rollback path is also unavailable — which is why this can become urgent for reasons unrelated to
  its own availability.
- **Gateway credential rotation is unavailable.** A credential expiring during this window becomes
  a data-plane incident.
- **After 15 minutes: newly-onboarded merchants cannot transact.**

## Immediate triage (first 5 minutes)

1. Size it, and check the clock:
   ```promql
   pp:control_plane_error:ratio_rate5m
   pp:control_plane_error:ratio_rate1h
   max(pp_config_snapshot_age_seconds)      # this is the countdown
   ```
2. Is the API up at all?
   ```bash
   kubectl -n pp-control-plane get pods -l app=control-plane-api -o wide
   kubectl -n pp-control-plane logs deploy/control-plane-api --since=10m | tail -60
   kubectl -n pp-control-plane rollout history deployment/control-plane-api
   ```
3. Which routes are failing?
   ```promql
   sum by (route, status) (rate(pp_http_requests_total{service="control-plane-api",status="5xx"}[5m]))
   ```
4. Its dependencies — Postgres and Redis:
   ```promql
   pp_db_pool_in_use / pp_db_pool_max
   redis_up
   changes(pg_writer_instance_changed_total[10m])
   ```
5. **Confirm the data plane really is fine**, so you can say so:
   ```promql
   sum by (status) (rate(pp_http_requests_total{route="/v1/payments"}[5m]))
   pp:payments:tps5m
   ```

## Diagnosis

- **A deploy is inside the window** → *M1*.
- **`CrashLoopBackOff`** → configuration. `control-plane-api` requires `PP_ENVIRONMENT`,
  `PP_REGION`, `PP_DATABASE_URL`, `PP_HTTP_ADDR`, `PP_PUBLIC_BASE_URL`, the three
  `PP_AUTH_*` variables, and — uniquely to this binary — `PP_L2_SUPPORTED_COUNTRIES` and
  `PP_L2_SANCTIONED_COUNTRIES`, both required with no default. The startup failure names every
  missing one at once. → *M2*.
- **`AuroraFailoverDetected` also firing** → the 5xx are the failover window.
  → [aurora-failover.md](aurora-failover.md); expect self-recovery.
- **Pool exhaustion on the control-plane pool** → long reporting queries (its statement timeout is
  30 s, off the hot path) holding connections. → [db-pool-exhaustion.md](db-pool-exhaustion.md),
  then *M3*.
- **Errors only on write routes (`PUT`/`POST` configuration, merchant creation)** → a validation or
  migration problem, not availability. Check `./bin/platformctl migrate status`. → *M4*.
- **Errors only on onboarding routes** → the workflow engine, not the API.
  → [onboarding-stuck.md](onboarding-stuck.md).
- **`redis_up == 0`** → degraded, not broken; the control plane's Redis use is caching.
  → [redis-loss.md](redis-loss.md).

## Mitigation

**M1 — roll back.**
```bash
kubectl -n pp-control-plane rollout undo deployment/control-plane-api
kubectl -n pp-control-plane rollout status deployment/control-plane-api --timeout=5m
```
Expected: error ratio to baseline within a rollout.

**M2 — fix configuration and roll.** Read the startup failure; it enumerates what to set:
```bash
kubectl -n pp-control-plane logs deploy/control-plane-api --tail=40
```
The two L2 policy variables have no defaults on purpose: an empty supported-countries list means
"nowhere", and defaulting it to "anywhere" would let a merchant be onboarded in a jurisdiction the
platform holds no licence for.

**M3 — relieve pool pressure.** Kill the long-running reporting queries:
```sql
SELECT pid, now() - query_start AS runtime, left(query, 120)
FROM   pg_stat_activity
WHERE  state = 'active' AND now() - query_start > interval '30 seconds'
ORDER  BY runtime DESC;
-- then, per pid, with the query identified:
SELECT pg_cancel_backend(<pid>);
```
Prefer `pg_cancel_backend` to `pg_terminate_backend`: cancel ends the query, terminate ends the
connection and the transaction with it.

**M4 — verify the schema is current.**
```bash
./bin/platformctl migrate status
```
If a migration is pending, applying it is a deliberate action:
```bash
./scripts/migrate.sh up --dsn "$PP_DSN"
```

**M5 — restart, if nothing else fits.**
```bash
kubectl -n pp-control-plane rollout restart deployment/control-plane-api
```
Cheap here in a way it is not in the data plane: nothing money-carrying is in flight.

## Rollback / escalation

- **`max(pp_config_snapshot_age_seconds)` crossing 300 s** → this is no longer contained. Move to
  [config-staleness.md](config-staleness.md), which is where the merchant impact lives.
- **Unresolved at 15 minutes** → escalate to Sev-2 with the incident commander, because the
  fail-static window is expiring and the failure mode changes from "invisible" to "new merchants
  cannot transact".
- **A bad configuration is live and rollback is unavailable** → escalate immediately regardless of
  the burn rate. The merchant impact of the bad configuration is the incident; the control plane's
  availability is merely what is blocking the fix.
- **Do not put the control plane in the payment path to "help".** Every version of that idea
  converts a survivable control-plane outage into a payment outage, which is precisely what
  ADR-007 was written to prevent.

## Verification

```promql
pp:control_plane_error:ratio_rate5m < 0.001
pp:control_plane_error:ratio_rate1h < 0.001
max(pp_config_snapshot_age_seconds) < 30
```
Then prove the plane actually works, rather than merely returning 200 on `/healthz`:
```
GET  /v1/merchants
GET  /v1/gateways
GET  /v1/merchants/{merchantId}/configuration/versions
```
and confirm the data plane's snapshots refreshed — a control plane that is up but not being read
from is not recovered.

## Follow-up

- Record the outage duration against the 15-minute fail-static window. How much margin was left is
  the number that matters, not whether it was breached.
- If margin was thin, that is an argument for widening the window or for making the control plane
  more available — a design conversation, held with the numbers.
- Count onboardings delayed and configurations that could not be published, and follow up with
  those merchants.
- If a required environment variable was missing, the deploy pipeline let a pod ship without it.
  That is a chart or overlay defect and it is more durable to fix than the incident.
