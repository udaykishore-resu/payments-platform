# ADR-009: Idempotency is Postgres-authoritative with Redis as a non-authoritative accelerator

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §14 (idempotency contract), §1.3 ambiguity A6, §12 stage 8 of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Refines ADR-004 (idempotency + ledger); related to ADR-012 (attempts)

## Context

Idempotency is the contract that lets a client retry a payment request without risking a second
charge. It is therefore a *correctness* mechanism, not a convenience, and its failure mode is
financial.

The forces:

1. **Retries are the normal case, not the exception.** Clients time out, load balancers retry,
   mobile networks drop responses. A client that receives no response to `POST /v1/payments`
   must be able to retry safely. At 5 000 TPS with even a 0.5 % retry rate, that is 25 duplicate
   claims per second in steady state, and vastly more during an incident.
2. **The store must be linearizable.** "Has this key been claimed?" must have exactly one answer
   at one instant across every replica of `payment-api`. Baseline §15 classifies the idempotency
   claim as **CP (linearizable)**.
3. **Redis is not a linearizable store in any configuration we would deploy.** Redis replication
   is asynchronous; a failover can lose acknowledged writes. Redlock across independent nodes is
   contested in the literature and, more importantly, depends on bounded clock drift and bounded
   GC pauses — neither of which we can guarantee on a Kubernetes node. A lost `SET NX` under
   failover means two processes both believe they hold the claim, and both charge the card.
4. **Latency budget.** §12 stage 8 allocates 8 ms to the idempotency claim. A Postgres
   `INSERT … ON CONFLICT DO NOTHING` against a unique index on a warm connection is ~0.5–2 ms
   in-AZ; comfortably inside budget. A Redis `GET` is ~0.2–0.5 ms. The accelerator earns ~1 ms on
   *replays*, which matters at volume but is not worth a correctness risk.
5. **Concurrent duplicates must not block.** If a second request for an in-flight key waits for
   the first to complete, a retry storm converts into a thread-pool exhaustion event: every
   in-flight request holds a connection waiting on a lease held by a process that may already be
   dead. This is baseline A6, and it is the reason the answer is `409`, not a wait.
6. **A dead claimant must not wedge the key forever.** Processes crash mid-flight. The record
   must be reclaimable.

What breaks if we choose wrong: double charges (Redis-authoritative under failover), or a
platform-wide stall during a retry storm (blocking on the lease), or a client permanently unable
to complete a payment (unreclaimable lease).

## Decision

**PostgreSQL is the authoritative idempotency store. Redis mirrors completed records purely as a
read accelerator. Concurrent duplicates return `409 IDEMPOTENT_REQUEST_IN_PROGRESS` with
`Retry-After: 1` — we never block and never process twice.**

Binding specifics:

1. **Scope:** `(tenant_id, merchant_id, method, path_template, idempotency_key)` — §14.1. The
   scope tuple is part of the unique index, so the same key used by two merchants is two keys.
2. **Claim:** `INSERT … ON CONFLICT DO NOTHING` against that unique index, returning the affected
   row count. One row → we own the claim. Zero rows → a record exists; read it and branch on
   state per §14.3.
3. **Fingerprint:** `SHA-256` over the JCS-canonicalized body plus the scope tuple. Same key,
   different fingerprint → `422 IDEMPOTENCY_KEY_REUSED`. This catches the client bug where one
   key is reused for two different payments — a bug that would otherwise return the *first*
   payment's response for the *second* payment, which is worse than any error.
4. **States and duplicate behaviour** (§14.3): `IN_FLIGHT` → `409` + `Retry-After: 1`;
   `COMPLETED` → replay the stored status and body with `Idempotent-Replay: true`;
   `FAILED_TERMINAL` → replay the stored error; **lease expired** → reclaim atomically with
   `UPDATE … SET lease_expires_at = now() + interval, owner = $me WHERE key = $k AND
   lease_expires_at < now()` and re-execute if exactly one row was updated.
5. **Lease clock is the database's `now()`**, never the pod's. Node clock skew (§24, F-19) must
   not be able to cause double acquisition.
6. **Redis is advisory.** It caches `COMPLETED` and `FAILED_TERMINAL` response snapshots keyed
   by `pp:{tenant_id}:idem:{hash}`. A Redis hit short-circuits the replay path. A Redis miss,
   stale value, or total outage costs latency only: the code path always falls through to
   Postgres, and Postgres always decides. Redis is **never** consulted to decide whether a claim
   is free.
7. **Completion is written in the same transaction as the business effect** (§12 stage 16–17), so
   "payment created" and "idempotency completed" cannot diverge.
8. **Retention:** 7 days (configurable, must exceed the longest client retry window), then
   archived to S3 with the audit trail.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Postgres-authoritative + Redis accelerator (chosen)** | Linearizable claim via a unique index on the same primary that holds the payment; claim and business effect commit in one transaction, so they cannot diverge; Redis outage is a latency event, not a correctness event; reclaim is a plain conditional `UPDATE`; the durable record doubles as the audit artifact | Adds ~0.5–2 ms to every mutating request; idempotency rows are write amplification on the primary (at 5 000 TPS, ~5 000 rows/s, ~3 billion over the 7-day retention if unpartitioned — so the table must be partitioned and pruned); two stores to reason about, with a discipline that Redis must never be believed | **Accepted** |
| **Redis-authoritative (`SET NX PX`), Postgres for history only** | Fastest possible claim (~0.3 ms), keeps the write off the primary; the pattern everyone recognises; scales horizontally without touching the database | Redis replication is asynchronous: a failover can lose an acknowledged `SET NX`, after which a second process claims the same key and **charges the card twice**. Redlock does not fix this — it trades one set of assumptions (durable replication) for another (bounded clock drift and bounded process pauses), and a Go GC pause or a Kubernetes node freeze violates the latter. The claim would also no longer be in the same transaction as the payment write, reintroducing a dual-write window. This is the option a performance-minded engineer pushes for, and the latency argument is real — it loses because the failure mode is a double charge, and we are explicitly trading latency for that (A4, A6) | Rejected |
| **Blocking on the in-flight lease (second caller waits)** | Best client ergonomics: the retry gets the real answer instead of a `409`, so naive clients "just work"; no `Retry-After` handling needed in SDKs | Under a retry storm, every duplicate holds a request goroutine, a connection-pool slot and an upstream socket while waiting on a lease that may be held by a *dead* process for the full lease duration. The queue grows faster than it drains, and the ingress tier collapses — the exact mechanism in §24 F-18. It also makes the p99 latency of a duplicate unbounded by our own budget. Explicitly rejected in baseline A6 | Rejected |
| **Advisory locks (`pg_advisory_xact_lock`) on a hash of the key** | Linearizable, no extra table, no unique-index maintenance; automatically released at transaction end | Blocking by construction (same failure as above), or non-blocking `try_lock` which then needs a durable record anyway to answer "what was the result?"; hash collisions across a 64-bit space at billions of keys are non-trivial and produce false conflicts between unrelated requests; leaves no artifact to replay a stored response from | Rejected |
| **No idempotency store: rely on natural keys / client-supplied payment IDs** | Simplest; no extra write; the payment table's own unique constraint does the work | Only covers `POST /v1/payments`, not `/capture`, `/refund`, `/void`, where the natural key is the payment plus an amount and two legitimate identical refunds are indistinguishable from one retried refund; provides no way to replay the original *response*; pushes the burden onto every client SDK | Rejected |

## Consequences

### Positive

- A double charge cannot originate from the idempotency layer. The unique index is the proof.
- Claim, business effect and outbox event commit atomically — there is no window where a payment
  exists but its idempotency record does not, or vice versa.
- A total Redis outage degrades p99 by ~1 ms on replays and changes nothing else (§24 lists this
  explicitly as a latency-only degradation).
- The stored response snapshot means a replay returns *byte-identical* results, including the
  original `payment_id` — clients cannot observe a difference between the first and Nth call.
- Reclaim after a crash is a single conditional `UPDATE`; no lock manager, no distributed
  coordination.

### Negative

- Clients must handle `409 IDEMPOTENT_REQUEST_IN_PROGRESS` with backoff. This is a real
  integration burden and must be prominent in the SDKs and documentation; a client that treats
  `409` as fatal will report spurious failures.
- Every mutating request writes to the primary before doing anything useful, adding write
  amplification and ~0.5–2 ms of latency to the hot path.
- The `idempotency_records` table is high-churn: it needs monthly partitioning with partition
  drop (not `DELETE`) for pruning, or autovacuum will not keep up at 5 000 rows/s.
- Two representations of the same fact (Postgres row, Redis entry) can disagree; the rule that
  Redis is never authoritative must be enforced by review, since it is not enforceable by types.

### Neutral / accepted costs

- Lease duration is a tuning parameter with a real trade-off: too short and a slow-but-alive
  request gets its claim stolen (mitigated by fencing — the reclaiming process bumps an owner
  epoch and the original's completion write is rejected); too long and a genuine crash blocks the
  key. Default 30 s, aligned with the 8 s gateway timeout plus pipeline overhead and one retry.
- `Retry-After: 1` is a hint; clients that ignore it get rate-limited by stage 6 instead.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| Redis is treated as authoritative by a future change | Medium | **Critical** — double charge | The Redis path is read-only for replay snapshots and lives behind an interface with no "claim" method at all; there is no API to misuse | Code review; absence of any write path in `IdempotencyStore`'s Redis implementation is asserted by an interface-conformance test |
| Lease stolen from a live request, both complete | Low | High | Owner epoch fencing: completion writes include the epoch and are rejected if it has advanced; the loser returns `409` and the client retries into the replay | `pp_idempotency_outcomes_total{outcome="fenced"}` |
| Client reuses one key for different payloads | Medium | Medium — payments rejected, client confusion | Fingerprint check → `422 IDEMPOTENCY_KEY_REUSED` with a clear message; documented prominently | `pp_idempotency_outcomes_total{outcome="conflict"}` by tenant |
| Table bloat / autovacuum falls behind at 5 000 rows/s | High if unmanaged | High — primary degradation | Range partition by day; prune with `DROP PARTITION`; monitor dead-tuple ratio | `pg_stat_user_tables` dead tuples; partition count; write latency p99 |
| Retry storm of `409`s becomes its own load problem | Medium | Medium | `Retry-After: 1` plus full jitter guidance in SDKs; stage 6 rate limiting; adaptive concurrency limiter (§24 F-18) | `pp_idempotency_outcomes_total{outcome="in_progress"}` rate; request rate rising without unique-key rate rising |
| Client never retries after a `409` and the payment silently completes | Medium | Medium — client state diverges from ours | The payment completes regardless; `GET /v1/payments/{id}` and webhooks reconcile the client; SDK guidance requires a terminal `GET` after exhausting retries | Support signal; ratio of payments whose result was never fetched |

## Validation

- **Concurrency test:** 100 goroutines issue the same key simultaneously. Assert exactly one
  `2xx`, ninety-nine `409`s, exactly one payment row, and zero gateway calls beyond the first.
- **Crash test:** kill the process holding a claim mid-flight; assert the key is reclaimable after
  lease expiry, that the reclaimed execution produces one payment, and that a late completion
  write from the original process is fenced out.
- **Redis-outage test:** `tests/chaos` removes Redis entirely under load. Assert zero correctness
  errors and a p99 regression of ≤ 5 ms.
- **Metric:** `pp_idempotency_outcomes_total{outcome}` — the `in_progress` share in steady state
  should be < 0.1 % of mutating requests. A sustained rise means clients are retrying too
  aggressively or our latency has regressed.
- **The financial check:** zero payments per month attributable to duplicate processing of a
  single idempotency key. This is the number that decides whether this ADR was right.

## Revisit criteria

Reopen if:

1. Idempotency writes become a measurable bottleneck on the primary (> 15 % of write IOPS or a
   demonstrable contribution to write p99) — the fix is likely partitioning or a dedicated
   cluster, not moving authority to Redis.
2. We adopt a datastore that is both linearizable and materially faster for this access pattern
   *and* can participate in the same transaction as the payment write. The transactional
   requirement is the binding one.
3. The `409` contract proves untenable for a large fraction of integrators, in which case the
   change to consider is a short bounded server-side wait (e.g. ≤ 200 ms) before returning `409`
   — never an unbounded block.
