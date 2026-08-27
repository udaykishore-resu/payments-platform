// tests/load/ramp.js — the scale-out scenario.
//
// SHAPE      0 → 15 000 requests/second over 20 minutes, then hold, then down
// ASSERTS    baseline §18 peak throughput; testing.md §6.2 row 2
// REQUIREMENTS  NFR-02 (15 000 TPS peak) · NFR-04 (3× headroom) · NFR-06 (HPA behaviour)
//
// WHAT THIS SCENARIO IS ACTUALLY FOR
//   Not "can it do 15 000 TPS" — the steady-state run at 5 000 already says the per-request
//   path is fast enough, and capacity is a multiplication. This asks whether the system
//   scales *smoothly*: whether the HPA adds pods before latency degrades rather than after,
//   whether the PgBouncer pool ceiling is reached before the pod count is, and whether the
//   routing layer starts returning NO_ELIGIBLE_GATEWAY because gateway concurrency limits
//   bind before platform capacity does.
//
//   All three failures look identical on a throughput chart (the line stops going up) and
//   completely different in the metrics, which is why the thresholds below assert on the
//   *shape* of the degradation and not only on the peak number.
//
// WHY THE RAMP IS LINEAR AND SLOW
//   deployment.md §1.4: the orchestrator's HPA scales on in-flight gateway calls with a
//   stabilisation window. A step change outruns any autoscaler and would measure the
//   scale-up latency of Kubernetes rather than the platform's capacity curve. Twenty
//   minutes to peak is roughly four HPA decision cycles per doubling, which is the regime
//   production traffic actually presents.

import { Counter } from 'k6/metrics';
import { createPayment, replayPayment, scaled, sloThresholds } from './lib/payload.js';

// Counted separately from other 503s so that "we shed load safely" and "we ran out of
// gateway capacity" are distinguishable results rather than one number.
const noEligibleGateway = new Counter('pp_no_eligible_gateway');

export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-arrival-rate',
      startRate: scaled(500),
      timeUnit: '1s',
      preAllocatedVUs: scaled(3000),
      maxVUs: scaled(18000),
      stages: [
        { target: scaled(2500),  duration: '4m'  },
        { target: scaled(5000),  duration: '4m'  },   // the §18 sustained target
        { target: scaled(10000), duration: '6m'  },
        { target: scaled(15000), duration: '6m'  },   // the §18 peak target
        { target: scaled(15000), duration: '5m'  },   // hold at peak
        { target: scaled(500),   duration: '5m'  },   // and back down: scale-IN matters too
      ],
      gracefulStop: '60s',
    },
  },
  thresholds: Object.assign({}, sloThresholds, {
    // The SLOs must hold all the way to peak, not on average across the ramp. Tagging by
    // scenario stage is not available in k6, so the assertion is on the whole run and the
    // hold-at-peak segment dominates the tail.
    'http_req_failed': ['rate<0.0005'],
    'checks': ['rate>0.999'],

    // NO_ELIGIBLE_GATEWAY under load means the routing layer ran out of healthy capacity
    // before the platform did — a 503 that is correct behaviour (§24 fails closed) but a
    // capacity finding, not a passing run. It is counted separately from other 503s
    // precisely so that "we shed load safely" and "we ran out of gateways" are different
    // results.
    'pp_no_eligible_gateway': ['count==0'],

    // A latency cliff rather than a latency curve is the HPA failing to keep up. p99 at the
    // 99.9th percentile of the whole run catches a cliff that a p99 average would smooth
    // away.
    'pp_server_side_duration{endpoint:create_payment}': ['p(99)<250', 'p(99.9)<800'],
  }),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'p(99.9)', 'max'],
};

export default function () {
  const { res, key, body } = createPayment('create_payment');

  if (res.status === 503) {
    let code = '';
    try { code = res.json('code') || ''; } catch (e) { /* problem+json not returned */ }
    if (code === 'NO_ELIGIBLE_GATEWAY') noEligibleGateway.add(1);
  }

  if (__ITER % 50 === 0) replayPayment(key, body);
}
