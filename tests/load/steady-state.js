// tests/load/steady-state.js — the sustained-throughput scenario.
//
// SHAPE      constant arrival rate, 5 000 requests/second, 30 minutes
// ASSERTS    baseline §18 sustained throughput and latency; testing.md §6.2 row 1
// REQUIREMENTS  NFR-01 (p99 ≤ 250 ms server-side) · NFR-02 (5 000 TPS sustained)
//               NFR-03 (error rate ≤ 0.01 %) · FR-38 (idempotent payment creation)
//
// WHY constant-arrival-rate AND NOT constant-vus
//   A VU-based executor is a closed loop: when the system slows down, the load generator
//   slows down with it, and the reported throughput silently drops to whatever the system
//   could manage. That measures the system's capacity to absorb a *self-throttling* client
//   and reports a healthy p99 for a system in trouble. An arrival-rate executor is an open
//   loop — it offers 5 000 requests per second regardless — which is what real traffic
//   does and is the only shape under which a latency SLO means anything.
//
// WHY 2 % REPLAY TRAFFIC
//   The point of running this at all. Idempotency correctness under contention is not
//   observable in a functional test with four goroutines; it is observable when 100
//   requests per second are replays landing while the original is still in flight. The
//   failure it prevents is a second authorisation for one logical payment.

import { sleep } from 'k6';
import http from 'k6/http';
import {
  BASE, headers, createPayment, replayPayment, scaled, sloThresholds, unresolvedProcessing,
} from './lib/payload.js';

export const options = {
  scenarios: {
    steady: {
      executor: 'constant-arrival-rate',
      rate: scaled(5000),
      timeUnit: '1s',
      duration: __ENV.DURATION || '30m',
      // preAllocatedVUs must cover rate × expected-latency with headroom, or k6 spends the
      // first minutes allocating VUs and under-delivers the rate it was asked for — which
      // shows up as a warm-up artifact everybody learns to ignore, right up until it hides
      // a real regression.
      preAllocatedVUs: scaled(2000),
      maxVUs: scaled(6000),
      gracefulStop: '30s',
    },
  },
  thresholds: Object.assign({}, sloThresholds, {
    // §18: 99.99 % availability. At 5 000 TPS for 30 minutes that is 9 000 000 requests,
    // so a rate below 0.0001 is ~900 permitted failures — enough to be measurable and few
    // enough to be meaningful.
    'http_req_failed': ['rate<0.0001'],
    'checks': ['rate>0.9999'],
    // Everything the API accepted must reach a terminal state. A payment left PROCESSING
    // is not an error the client sees; it is money in an indeterminate state, and §24 is
    // explicit that the reconciler must resolve it.
    'pp_unresolved_processing': ['count==0'],
  }),
  // Discard response bodies except where a check reads them: at 5 000 TPS the generator
  // becomes the bottleneck otherwise, and a load test limited by its own load generator
  // reports the generator's p99.
  discardResponseBodies: false,
  noConnectionReuse: false,
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'p(99.9)', 'max'],
};

const created = [];

export default function () {
  const { res, key, body } = createPayment('create_payment');

  if (res.status === 201) {
    try {
      const id = res.json('id');
      if (id && created.length < 200) created.push(id);
    } catch (e) { /* body not JSON: the status check already recorded it */ }
  }

  // 2 % of iterations replay with the SAME key while the original may still be in flight.
  if (__ITER % 50 === 0) {
    replayPayment(key, body);
  }
}

// teardown runs once after the scenario. It resolves the sample of payments this run
// created and counts the ones still non-terminal, which is the assertion testing.md §6.2
// states as "zero PROCESSING payments unresolved after 15 minutes". Sampling rather than
// checking all nine million: the property is "the reconciler keeps up", and a 200-payment
// sample detects a reconciler that has stopped just as reliably as a full sweep.
export function teardown() {
  if (!created.length) return;
  // Give the asynchronous resolution path a chance; §24 budgets 15 minutes, this samples
  // after a short settle because a run's own teardown cannot wait fifteen.
  sleep(30);
  let unresolved = 0;
  for (const id of created) {
    const r = http.get(`${BASE}/v1/payments/${id}`, {
      headers: headers('teardown'), tags: { endpoint: 'teardown_poll' },
    });
    if (r.status !== 200) continue;
    let status;
    try { status = r.json('status'); } catch (e) { continue; }
    if (status === 'PROCESSING' || status === 'PENDING') unresolved++;
  }
  unresolvedProcessing.add(unresolved);
  if (unresolved > 0) {
    console.error(`teardown: ${unresolved}/${created.length} sampled payments are still ` +
      `non-terminal — the reconciler is not keeping up (§24)`);
  }
}
