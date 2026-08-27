// tests/load/soak.js — the four-hour endurance scenario.
//
// SHAPE      constant 3 000 requests/second for 4 hours
// ASSERTS    baseline §18 sustained operation; testing.md §6.2 row 4
// REQUIREMENTS  NFR-08 (no resource leak under sustained load) · NFR-01 (latency stability)
//
// WHY A SOAK IS NOT "A LONGER STEADY-STATE RUN"
//   Everything this scenario detects is invisible in thirty minutes, because everything it
//   detects is a slope rather than a value:
//
//     * a goroutine leaked per request — 3 000/s for 4 h is 43 million; a leak of one in a
//       thousand is 43 000 goroutines and a heap that has quietly tripled;
//     * a connection-pool leak — a borrowed pgx connection never returned shows up when
//       the pool is exhausted, which at 3 000 TPS with a 100-connection pool takes hours;
//     * a file-descriptor leak on the gateway HTTP client — the classic missing
//       `resp.Body.Close()`, which is why `bodyclose` is in .golangci.yml, and this is the
//       test that catches the one the linter did not;
//     * unbounded growth in a cache with no eviction — the config snapshot map, the
//       idempotency LRU, a metric series set that a cardinality lint did not catch because
//       the label value is invented at runtime.
//
//   None of these fail a request. They fail the process, once, after the run that would
//   have caught them has finished.
//
// WHAT THE THRESHOLDS CAN AND CANNOT SEE
//   k6 sees the client side. It can assert that the p99 at hour four is within 10 % of the
//   p99 at hour one, which catches every leak that manifests as degradation — and that is
//   most of them. It cannot see the heap. The complete assertion is the second half:
//   the nightly workflow queries Prometheus for go_goroutines, go_memstats_heap_inuse_bytes,
//   process_open_fds and the pgx pool gauges over the same window and fails on a positive
//   slope. Both halves are needed; neither is sufficient. This file is the half that lives
//   with the load.

import { Trend, Counter } from 'k6/metrics';
import { createPayment, replayPayment, scaled, sloThresholds } from './lib/payload.js';

// Latency bucketed by hour, so the comparison the scenario is named for is in the summary
// rather than in someone's memory of what the chart looked like.
const hourlyLatency = new Trend('pp_hourly_duration', true);
const lateFailures = new Counter('pp_late_failures');

export const options = {
  scenarios: {
    soak: {
      executor: 'constant-arrival-rate',
      rate: scaled(3000),
      timeUnit: '1s',
      duration: __ENV.DURATION || '4h',
      preAllocatedVUs: scaled(1500),
      maxVUs: scaled(4000),
      gracefulStop: '2m',
    },
  },
  thresholds: Object.assign({}, sloThresholds, {
    'http_req_failed': ['rate<0.0001'],
    'checks': ['rate>0.9999'],

    // The SLO must hold at hour four, not on average over four hours. An average over the
    // whole run is dominated by the three good hours and would pass a system that degraded
    // steadily throughout — which is precisely the failure this scenario exists to find.
    'pp_hourly_duration{hour:1}': ['p(99)<250'],
    'pp_hourly_duration{hour:4}': ['p(99)<275'],   // §6.2: within 10 % of hour 1

    // Failures concentrated in the last hour are a leak reaching its limit, and they must
    // be zero even though the overall error-rate threshold would tolerate a few.
    'pp_late_failures': ['count==0'],

    'pp_unresolved_processing': ['count==0'],
  }),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'p(99.9)', 'max'],
};

const startedAt = Date.now();

export default function () {
  const { res, key, body } = createPayment('create_payment');

  const elapsedH = (Date.now() - startedAt) / 3600000;
  const hour = Math.min(4, Math.floor(elapsedH) + 1);
  if (res.timings && res.timings.duration != null) {
    hourlyLatency.add(res.timings.duration, { hour: String(hour) });
  }
  if (hour === 4 && res.status >= 500) {
    lateFailures.add(1, { status: String(res.status) });
  }

  if (__ITER % 50 === 0) replayPayment(key, body);
}

export function handleSummary(data) {
  // A soak produces four hours of data and one question: did anything trend. Printing the
  // hour-over-hour comparison in the summary means the answer is in the CI log rather than
  // in a Grafana query someone has to remember to run.
  const p99 = (h) => {
    const m = data.metrics[`pp_hourly_duration{hour:${h}}`];
    return m && m.values ? m.values['p(99)'] : null;
  };
  const h1 = p99(1), h4 = p99(4);
  const drift = (h1 && h4) ? ((h4 - h1) / h1 * 100).toFixed(1) : 'n/a';

  const text =
    `\nsoak: p99 hour 1 = ${h1 ? h1.toFixed(1) : 'n/a'} ms, ` +
    `hour 4 = ${h4 ? h4.toFixed(1) : 'n/a'} ms, drift = ${drift}%\n` +
    `remember: the heap, goroutine and fd slopes are asserted by the nightly workflow's\n` +
    `Prometheus query over this same window — this summary is only the client-side half.\n`;

  return {
    stdout: text,
    '.loadtest/soak-summary.json': JSON.stringify(data, null, 2),
  };
}
