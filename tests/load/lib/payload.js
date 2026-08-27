// tests/load/lib/payload.js — shared payload generation and assertions for every scenario.
//
// Extracted rather than duplicated across the four scenarios for one reason that matters:
// if the request bodies differ between steady-state and spike, the two runs are not
// measuring the same system, and comparing their p99s is meaningless. One generator, four
// load shapes.
//
// Requirements exercised here: NFR-01 (latency), NFR-02 (throughput), FR-38 (idempotent
// payment creation). The IDs are in the source so scripts/traceability.sh can see them.

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

// --- configuration ---------------------------------------------------------------------
export const BASE = __ENV.BASE || 'http://localhost:8080';
export const TOKEN = __ENV.TOKEN || '';

// VUS_SCALE lets a whole scenario be run at 1 % on a laptop without editing the file.
// The thresholds are NOT scaled: a scaled-down run that still has to meet p99 ≤ 250 ms is
// a useful smoke test, and one that relaxes its own pass criteria is not a test at all.
export const SCALE = Math.max(1, Number(__ENV.VUS_SCALE || 100)) / 100;
export const scaled = (n) => Math.max(1, Math.round(n * SCALE));

// --- custom metrics ---------------------------------------------------------------------
//
// These exist because the built-in http_* metrics answer "was it fast" and not "was it
// correct". A load test of a payment API that does not assert correctness under load is a
// benchmark, and a benchmark cannot fail.

// Replays that did not come back marked as replays. Each one is a potential double charge.
export const idempotencyBreaches = new Counter('pp_idempotency_breaches');
// Replay responses that carried the correct header.
export const idempotencyHonoured = new Rate('pp_idempotency_honoured');
// Payments the API accepted but left unresolved at the end of the run.
export const unresolvedProcessing = new Counter('pp_unresolved_processing');
// 5xx that is not a 503-with-Retry-After. §24: under overload the system sheds with 429 or
// 503, and a 500 means it broke instead of shedding.
export const hardServerErrors = new Counter('pp_hard_server_errors');
// Server-side latency as reported by the platform's own Server-Timing header, which
// excludes gateway time — the number baseline §18 actually sets a target for.
export const serverSideDuration = new Trend('pp_server_side_duration', true);

// --- deterministic-but-unique data -------------------------------------------------------

const CURRENCIES = ['USD', 'EUR', 'GBP'];
const METHODS = ['CARD', 'APPLE_PAY', 'GOOGLE_PAY'];
const COUNTRIES = ['US', 'DE', 'GB'];

// A merchant pool rather than one merchant. Driving all traffic through a single merchant
// concentrates every write on one row's lock and one config-cache entry, which measures
// row contention rather than platform throughput — and hides the per-merchant routing and
// risk evaluation entirely.
const MERCHANT_POOL = Number(__ENV.MERCHANT_POOL || 200);

export function merchantId() {
  // The seeded dataset is deterministic (scripts/seed.sh), so a merchant index maps to a
  // real merchant in the target environment. The ULID body is padded from the index.
  const n = ((__VU * 7919 + __ITER) % MERCHANT_POOL) + 1;
  return `mrc_01JB8Z${String(n).padStart(20, '0')}`;
}

// idempotencyKey MUST be unique per logical operation and stable across retries of that
// operation. Date.now() alone collides across VUs inside the same millisecond at 5 000
// TPS; (VU, ITER) alone collides across runs, which turns the second run of the day into
// 5 000 TPS of replays and measures the idempotency cache instead of the payment path.
// Both together are unique in each dimension that matters.
export function idempotencyKey(suffix) {
  return `k6-${__ENV.SCENARIO || 'load'}-${__VU}-${__ITER}-${Date.now()}${suffix ? '-' + suffix : ''}`;
}

// Amounts in MINOR UNITS, always integers. baseline §7: no floats anywhere on the money
// path, and a load generator that emits 10.5 is testing the API's float handling rather
// than its throughput.
export function amountMinor() {
  // A long-tailed distribution, not a uniform one. Real payment volume is dominated by
  // small amounts with a thin high tail, and the high tail is what crosses the merchant's
  // risk thresholds (§23 require3DSAbove, maxTransactionAmount) — so a uniform generator
  // exercises the risk engine's expensive path far more than production does and reports a
  // p99 that is not the platform's.
  const r = Math.random();
  if (r < 0.80) return 500 + Math.floor(Math.random() * 9500);        //     5.00 –    100.00
  if (r < 0.97) return 10000 + Math.floor(Math.random() * 90000);     //   100.00 –  1 000.00
  return 100000 + Math.floor(Math.random() * 400000);                 // 1 000.00 –  5 000.00
}

export function paymentBody() {
  const currency = CURRENCIES[(__VU + __ITER) % CURRENCIES.length];
  return {
    merchantId: merchantId(),
    amount: { value: amountMinor(), currency },
    paymentMethod: {
      type: METHODS[(__VU * 3 + __ITER) % METHODS.length],
      // A gateway token reference, never card data. §17: this API refuses a PAN on every
      // endpoint, so a load generator that sends one is testing the PAN detector.
      token: `tok_k6_${__VU}_${__ITER % 1000}`,
    },
    customer: {
      reference: `cus_k6_${__VU}`,
      country: COUNTRIES[(__VU + __ITER) % COUNTRIES.length],
    },
    capture: true,
    reference: `k6-${__ENV.SCENARIO || 'load'}-${__VU}-${__ITER}`,
    statementDescriptor: 'K6 LOAD TEST',
    metadata: { source: 'k6', scenario: __ENV.SCENARIO || 'load' },
  };
}

export function headers(key, extra) {
  return Object.assign({
    'Idempotency-Key': key,
    'Authorization': `Bearer ${TOKEN}`,
    'Content-Type': 'application/json',
    'Accept': 'application/json',
    'X-Correlation-Id': `k6-${__VU}-${__ITER}`,
  }, extra || {});
}

// --- the request ---------------------------------------------------------------------------

export function createPayment(tag) {
  const key = idempotencyKey();
  const body = JSON.stringify(paymentBody());
  const res = http.post(`${BASE}/v1/payments`, body, {
    headers: headers(key),
    tags: { endpoint: tag || 'create_payment' },
  });

  recordServerTiming(res);
  classify(res);

  check(res, {
    'status is 201': (r) => r.status === 201,
    // §7: amounts cross the wire as integer minor units. A float here means somebody
    // introduced a serialization path that will eventually lose a cent.
    'amount is an integer in minor units': (r) => {
      if (r.status !== 201) return true;   // not this check's business
      try { return Number.isInteger(r.json('amount.value')); } catch (e) { return false; }
    },
    'correlation is echoed': (r) =>
      r.status >= 500 || !!r.headers['X-Request-Id'] || !!r.headers['Traceparent'],
  }, { endpoint: tag || 'create_payment' });

  return { res, key, body };
}

// replayPayment re-sends an identical request with the SAME idempotency key. This is the
// reason these tests are worth running at all: idempotency correctness under contention is
// not observable in a functional test with four goroutines, and the failure it prevents —
// a second authorisation for one logical payment — is the most expensive bug this platform
// can have (baseline §14).
export function replayPayment(key, body) {
  const res = http.post(`${BASE}/v1/payments`, body, {
    headers: headers(key),
    tags: { endpoint: 'replay' },
  });

  recordServerTiming(res);

  const honoured =
    (res.status === 201 && String(res.headers['Idempotent-Replay']).toLowerCase() === 'true') ||
    // 409 IDEMPOTENT_REQUEST_IN_PROGRESS is a correct answer too: the first request has
    // not finished. What is NOT correct is a second 201 without the replay header.
    res.status === 409;

  idempotencyHonoured.add(honoured, { endpoint: 'replay' });
  if (!honoured) {
    idempotencyBreaches.add(1);
  }

  check(res, {
    'replay is idempotent': () => honoured,
  }, { endpoint: 'replay' });

  return res;
}

// --- classification -------------------------------------------------------------------------

function recordServerTiming(res) {
  // Server-Timing: app;dur=12.3 — the platform's own measurement of the time it spent,
  // excluding the gateway call. baseline §18 sets p50 ≤ 60 ms and p99 ≤ 250 ms on exactly
  // this number, so measuring the client-observed duration instead would be measuring the
  // load generator's network as well and failing a threshold the platform met.
  const st = res.headers['Server-Timing'];
  if (!st) return;
  const m = /app;dur=([0-9.]+)/.exec(st);
  if (m) serverSideDuration.add(Number(m[1]), { endpoint: 'create_payment' });
}

function classify(res) {
  if (res.status < 500) return;
  // §24, retry storm: under overload the platform sheds load with 429 or 503 and a
  // Retry-After. A 500 or a 502 without one means it fell over instead of shedding, which
  // is the distinction the spike scenario exists to assert.
  const sheds = res.status === 503 && !!res.headers['Retry-After'];
  if (!sheds) hardServerErrors.add(1, { status: String(res.status) });
}

// --- shared threshold fragments ----------------------------------------------------------------
//
// Exported so that each scenario states only what is different about it. A scenario that
// restates the common SLOs would eventually restate one of them slightly differently, and
// then two runs would be judged by two standards.

export const sloThresholds = {
  // baseline §18: p50 ≤ 60 ms, p99 ≤ 250 ms, server-side, excluding gateway time.
  'pp_server_side_duration{endpoint:create_payment}': ['p(50)<60', 'p(99)<250'],
  // The end-to-end number including the gateway: §18 p99 ≤ 1.5 s.
  'http_req_duration{endpoint:create_payment}': ['p(99)<1500'],
  // Every replay must be honoured. Not 99.99 % — a single unhonoured replay is a candidate
  // double charge, and there is no acceptable rate of those.
  'pp_idempotency_breaches': ['count==0'],
  'pp_idempotency_honoured': ['rate==1.0'],
  // The system sheds; it does not break.
  'pp_hard_server_errors': ['count==0'],
};
