# Load tests

k6 scenarios implementing `docs/testing.md` §6.2. Each one carries the SLOs of
`docs/spec/00-design-baseline.md` §18 as k6 **thresholds**, which means a run **fails**
rather than producing a chart someone has to interpret. If you find yourself reading the
graph to decide whether it passed, a threshold is missing.

| File | Shape | Duration | The question it answers |
|---|---|---|---|
| `steady-state.js` | constant 5 000 req/s | 30 min | Does the platform meet its latency and availability SLOs at the sustained target? |
| `ramp.js` | 500 → 15 000 req/s over 20 min, hold, down | ~30 min | Does it scale *smoothly* to peak — or does something bind first? |
| `spike.js` | 1 000 → 15 000 in 30 s, hold 5 min, drop | ~13 min | When it cannot serve the load, does it shed correctly and recover? |
| `soak.js` | constant 3 000 req/s | 4 h | Does anything leak? |
| `lib/payload.js` | — | — | Shared payload generation, custom metrics and the common threshold set. |

---

## Running them

```bash
# The wrapper. Refuses any target that is not a load-test environment, resolves k6 or
# falls back to the container, and writes a summary artifact.
scripts/loadtest.sh steady-state --base https://api.staging.example.com --token "$TOKEN"

scripts/loadtest.sh ramp   --base https://api.staging.example.com --token "$TOKEN"
scripts/loadtest.sh spike  --base https://api.staging.example.com --token "$TOKEN"
scripts/loadtest.sh soak   --base https://api.staging.example.com --token "$TOKEN"   # 4 h

# All four, back to back. This is the pre-release gate (testing.md §10.2) — about five hours.
scripts/loadtest.sh all --base https://api.staging.example.com --token "$TOKEN"

# A smoke run of the whole matrix on a laptop, at 1 % of the rates and 2 minutes each.
# The SLO thresholds still apply: a scaled-down run that relaxes its own pass criteria is
# not a test.
scripts/loadtest.sh all --base http://localhost:8080 --token dev --vus-scale 1 --duration 2m
```

Or k6 directly, if you want to pass k6 flags the wrapper does not expose:

```bash
k6 run tests/load/steady-state.js \
  --env BASE=https://api.staging.example.com \
  --env TOKEN="$TOKEN" \
  --env SCENARIO=steady-state
```

### Never production

`scripts/loadtest.sh` refuses any base URL whose host does not contain `staging`, `stg`,
`perf`, `loadtest` or `preview`, and is not a loopback address. This is an allow-list, not
a deny-list, because a deny-list of production hostnames is a list someone forgets to
update the day a region is added.

The reason is not politeness about shared environments. At the infrastructure layer a load
test is indistinguishable from a denial-of-service attack. At the money layer it is not a
simulation at all: every `POST /v1/payments` is a real authorisation against whatever
gateway account the target is configured with. Against a staging environment that is a
sandbox account and a synthetic merchant. Against production it is a payer's card.

### Environment prerequisites

The target must be seeded with the deterministic dataset (`scripts/seed.sh --profile=load`).
The generator addresses merchants by index across a pool of 200 (`MERCHANT_POOL`), so an
unseeded target produces a run of `MERCHANT_NOT_FOUND` that looks like a 100 % error rate
and is really a missing precondition.

---

## Reading the output

k6 prints a threshold block at the end of every run. That block **is** the result:

```
     ✓ pp_server_side_duration{endpoint:create_payment}
       p(50)=41.2ms  p(99)=187.4ms
     ✓ pp_idempotency_breaches..................: count=0
     ✗ http_req_failed..........................: rate=0.00031  (threshold: rate<0.0001)
```

A `✗` on any line means the run failed and `k6` exited non-zero, which
`scripts/loadtest.sh` passes straight through to CI.

### What each custom metric means

These exist because the built-in `http_*` metrics answer "was it fast" and not "was it
correct". A load test of a payment API that does not assert correctness under load is a
benchmark, and a benchmark cannot fail.

| Metric | Meaning | Why it matters |
|---|---|---|
| `pp_server_side_duration` | The platform's own `Server-Timing: app;dur=…`, excluding gateway time | §18 sets p50 ≤ 60 ms and p99 ≤ 250 ms on **this** number. `http_req_duration` includes the generator's own network and the gateway call, so judging the SLO by it fails a platform that met it. |
| `pp_idempotency_breaches` | Replays that came back as a fresh `201` without `Idempotent-Replay: true` | Each one is a candidate **double charge**. Threshold is `count==0`, not a rate — there is no acceptable frequency of charging a payer twice. |
| `pp_idempotency_honoured` | Rate of replays answered correctly (`201` + header, or `409` in-progress) | The positive form of the same property, so a run with zero replays cannot pass vacuously. |
| `pp_hard_server_errors` | 5xx that is not a `503` with `Retry-After` | §24: under overload the platform *sheds*. A `500` means it broke instead, and on the money route an unhandled path is where a partial write comes from. |
| `pp_unresolved_processing` | Sampled payments still non-terminal after the run | Not an error the client saw — money in an indeterminate state. If this is non-zero the reconciler is not keeping up. |
| `pp_no_eligible_gateway` (ramp) | `503 NO_ELIGIBLE_GATEWAY` responses | Correct behaviour (§24 fails closed) but a **capacity finding**: routing ran out of healthy gateway capacity before the platform ran out of its own. |
| `pp_shed_correctly` (spike) | Rate of 429/503 that carried `Retry-After` and were marked `retryable` | §20.1 makes `retryable` machine-readable so client SDKs back off. A shed response marked non-retryable turns a spike into an outage for well-behaved clients. |
| `pp_hourly_duration` (soak) | p99 bucketed by hour of the run | The soak's whole question is whether anything *trends*. An average over four hours is dominated by the three good ones. |

### Interpreting a failure

| Symptom | Most likely cause | Where to look next |
|---|---|---|
| `pp_server_side_duration` p99 over 250 ms, `http_req_duration` normal | The platform itself is slow: a lock, a missing index, a synchronous call added to the pipeline | Traces for `POST /v1/payments`, `pp_http_request_duration_seconds` by route |
| `http_req_duration` p99 high, `pp_server_side_duration` fine | Gateway latency or the generator's network | `pp_gateway_request_duration_seconds`, and check the generator is in the same region |
| `pp_idempotency_breaches` > 0 | **Stop.** The idempotency claim is not atomic under contention | `pp_idempotency_outcomes_total`, the `idempotency_records` table, and `internal/platform/idempotency` |
| `pp_hard_server_errors` > 0 in `spike` | An unhandled path under overload | The 500s' traces; this is a bug, not a capacity result |
| `pp_no_eligible_gateway` > 0 in `ramp` | Gateway concurrency limits bind before platform capacity | `pp_circuit_breaker_state`, `pp_routing_decisions_total{reason="no_eligible"}` |
| `ramp` p99.9 cliff but p99 fine | The HPA is adding pods *after* latency degrades, not before | HPA events, `pp_gateway_inflight_calls`, the stabilisation window in `deployment.md` §1.4 |
| `soak` hour-4 p99 more than 10 % above hour 1 | A leak — goroutines, connections, file descriptors, or an unbounded cache | The Prometheus half of the assertion (below) |

### The soak's other half

`soak.js` sees the client side only. It catches every leak that manifests as degradation,
which is most of them, but it cannot see the heap. The complete assertion is:

```promql
# run over the same 4-hour window; a positive slope fails the drill
deriv(go_goroutines{job="pp-services"}[4h])                  > 0
deriv(go_memstats_heap_inuse_bytes{job="pp-services"}[4h])   > 0
deriv(process_open_fds{job="pp-services"}[4h])               > 0
```

The nightly workflow (`.github/workflows/nightly.yml`) runs both halves. Neither is
sufficient on its own: a leak that has not yet reached a limit shows no degradation, and a
degradation with a flat heap is a different bug.

---

## Design decisions worth knowing about

**Arrival-rate executors, never VU-based ones.** A VU-based executor is a closed loop: when
the system slows down, the generator slows down with it and reports a healthy p99 for a
system in trouble. `constant-arrival-rate` and `ramping-arrival-rate` are open loops — they
offer the configured rate regardless — which is what real traffic does and the only shape
under which a latency SLO means anything.

**2 % replay traffic in every scenario.** This is the reason these tests are worth running.
Idempotency correctness under contention is not observable in a functional test with four
goroutines; it is observable when replays land while the original request is still in
flight. See `docs/testing.md` §6.2.

**Unique idempotency keys built from `(scenario, VU, ITER, timestamp)`.** `Date.now()` alone
collides across VUs within one millisecond at 5 000 TPS. `(VU, ITER)` alone collides across
runs, which turns the second run of the day into 5 000 TPS of replays and measures the
idempotency cache rather than the payment path. Both together are unique in each dimension
that matters.

**A merchant pool, not one merchant.** Driving all traffic through one merchant concentrates
every write on one row's lock and one config-cache entry — that measures row contention, not
platform throughput, and skips per-merchant routing and risk evaluation entirely.

**Long-tailed amounts.** Real volume is mostly small payments with a thin high tail, and the
high tail is what crosses `require3DSAbove` and `maxTransactionAmount` (§23). A uniform
generator exercises the risk engine's expensive path far more than production does and
reports a p99 that is not the platform's.

**Integer minor units, always.** §7: no floats on the money path. A generator that emits
`10.5` is testing the API's float handling.

**Gateway tokens, never card data.** §17: this API refuses a PAN on every endpoint. A
generator that sends one is testing the PAN detector, at 5 000 requests per second.

---

## Cross-references

- `docs/testing.md` §6.2 — the scenario table these implement
- `docs/spec/00-design-baseline.md` §18 — the SLO targets in the thresholds
- `docs/spec/00-design-baseline.md` §24 — the degradation behaviour `spike.js` asserts
- `docs/deployment.md` §1.4 — the HPA behaviour `ramp.js` is shaped around
- `scripts/loadtest.sh` — the wrapper, and the production guard
