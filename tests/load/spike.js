// tests/load/spike.js — the overload-behaviour scenario.
//
// SHAPE      1 000 → 15 000 requests/second in 30 seconds, hold 5 minutes, drop
// ASSERTS    baseline §24 (retry storm) and §20 (error semantics); testing.md §6.2 row 3
// REQUIREMENTS  NFR-05 (graceful degradation) · NFR-07 (recovery time)
//               NFR-24 (adaptive concurrency limiting)
//
// THIS SCENARIO IS EXPECTED TO PRODUCE ERRORS
//   That is the point, and it is what makes its thresholds different from every other
//   scenario's. A fifteen-fold step in thirty seconds outruns any autoscaler by design —
//   deployment.md §1.4's stabilisation windows are measured in minutes. The system cannot
//   serve this load and is not being asked to.
//
//   What it is being asked is HOW it fails:
//     * every rejection is a 429 or a 503, carries Retry-After, and is marked retryable —
//       §20.1 makes `retryable` machine-readable precisely so a client SDK backs off
//       instead of hammering, which is the difference between a spike and a retry storm;
//     * NOT ONE 500. A 500 means an unhandled path, and under overload an unhandled path
//       is where partial writes and orphaned gateway calls come from;
//     * no payment ends in an indeterminate state. Shedding load is safe; shedding a
//       request that already dispatched to a gateway is not;
//     * p99 returns to baseline within three minutes of the drop. A system that sheds but
//       does not recover has traded an outage for a longer one.
//
// WHY THE ERROR-RATE THRESHOLD IS HIGH AND THE 500 THRESHOLD IS ZERO
//   Setting http_req_failed low here would make the scenario fail for doing the right
//   thing. The pass criterion is not "few errors", it is "the right errors" — so the rate
//   threshold is loose and every assertion about error *kind* is absolute.

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { createPayment, replayPayment, scaled, sloThresholds } from './lib/payload.js';

const shedCorrectly = new Rate('pp_shed_correctly');
const missingRetryAfter = new Counter('pp_shed_without_retry_after');
const nonRetryableOverload = new Counter('pp_overload_marked_non_retryable');
const postSpikeLatency = new Trend('pp_post_spike_duration', true);

export const options = {
  scenarios: {
    spike: {
      executor: 'ramping-arrival-rate',
      startRate: scaled(1000),
      timeUnit: '1s',
      preAllocatedVUs: scaled(4000),
      maxVUs: scaled(20000),
      stages: [
        { target: scaled(1000),  duration: '2m'  },   // establish the baseline p99
        { target: scaled(15000), duration: '30s' },   // the spike
        { target: scaled(15000), duration: '5m'  },   // hold
        { target: scaled(1000),  duration: '30s' },   // drop
        { target: scaled(1000),  duration: '5m'  },   // recovery window
      ],
      gracefulStop: '60s',
    },
  },
  thresholds: {
    // Deliberately loose: this scenario is about the KIND of failure, not the rate.
    'http_req_failed': ['rate<0.60'],

    // Absolute. A 500 under overload is an unhandled path, and an unhandled path on the
    // money route is where a partial write comes from.
    'pp_hard_server_errors': ['count==0'],

    // Every shed response must be shaped so a client backs off.
    'pp_shed_correctly': ['rate==1.0'],
    'pp_shed_without_retry_after': ['count==0'],
    'pp_overload_marked_non_retryable': ['count==0'],

    // Idempotency does not get to degrade under load either.
    'pp_idempotency_breaches': ['count==0'],

    // Recovery: the last five minutes must look like the first two. 400 ms rather than the
    // 250 ms SLO because the recovery window includes the tail of the drop, and demanding
    // full-SLO latency in the same minute the queue drains would fail a system that
    // behaved correctly.
    'pp_post_spike_duration{phase:recovery}': ['p(99)<400'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'p(99.9)', 'max'],
};

// The recovery window begins after 8 minutes (2 baseline + 0.5 spike + 5 hold + 0.5 drop).
const RECOVERY_STARTS_MS = 8 * 60 * 1000;
const startedAt = Date.now();

export default function () {
  const { res, key, body } = createPayment('create_payment');

  const phase = (Date.now() - startedAt) > RECOVERY_STARTS_MS ? 'recovery' : 'load';
  if (res.timings && res.timings.duration != null) {
    postSpikeLatency.add(res.timings.duration, { phase });
  }

  if (res.status === 429 || res.status === 503) {
    const retryAfter = res.headers['Retry-After'];
    let retryable = null;
    try { retryable = res.json('retryable'); } catch (e) { /* not problem+json */ }

    if (!retryAfter) missingRetryAfter.add(1, { status: String(res.status) });
    // §20.1: RATE_LIMIT and INFRASTRUCTURE are retryable. A shed response marked
    // non-retryable tells a well-behaved SDK to give up on a request that would have
    // succeeded a second later.
    if (retryable === false) nonRetryableOverload.add(1, { status: String(res.status) });

    shedCorrectly.add(!!retryAfter && retryable !== false, { status: String(res.status) });

    check(res, {
      'shed response carries Retry-After': () => !!retryAfter,
      'shed response is marked retryable': () => retryable !== false,
    }, { endpoint: 'shed' });
  } else if (res.status >= 500) {
    // Recorded by classify() in lib/payload.js as a hard server error; asserted above.
    shedCorrectly.add(false, { status: String(res.status) });
  }

  if (__ITER % 50 === 0) replayPayment(key, body);
}
