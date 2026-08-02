#!/usr/bin/env bash
# Automated canary analysis: queries Prometheus for the deployed Deployment's error rate and P99
# latency over a bake window and fails (non-zero exit) if either breaches the supplied threshold.
# This is what .github/workflows/cd.yml's "automated canary analysis" step runs before promoting
# a prod deploy from canary to fully rolled out — see docs/09-production-checklist.md and
# docs/07-reliability-slo.md for the thresholds this should be reconciled against.
#
# Reference implementation: a real deployment likely replaces this hand-rolled polling script
# with Argo Rollouts' AnalysisTemplate (querying the same Prometheus metrics declaratively) once
# progressive-delivery tooling is installed — see the comment in cd.yml's canary step.
set -euo pipefail

PROMETHEUS_URL=""
DEPLOYMENT=""
NAMESPACE=""
BAKE_SECONDS=300
MAX_ERROR_RATE="0.01"
MAX_P99_MS="600"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --prometheus-url) PROMETHEUS_URL="$2"; shift 2 ;;
    --deployment) DEPLOYMENT="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --bake-seconds) BAKE_SECONDS="$2"; shift 2 ;;
    --max-error-rate) MAX_ERROR_RATE="$2"; shift 2 ;;
    --max-p99-ms) MAX_P99_MS="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$PROMETHEUS_URL" || -z "$DEPLOYMENT" || -z "$NAMESPACE" ]]; then
  echo "missing required arguments" >&2
  exit 2
fi

echo "baking for ${BAKE_SECONDS}s before evaluating canary metrics for ${DEPLOYMENT} in ${NAMESPACE}..."
sleep "$BAKE_SECONDS"

query() {
  local promql="$1"
  curl -sG --max-time 10 "${PROMETHEUS_URL}/api/v1/query" \
    --data-urlencode "query=${promql}" \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); r=d.get("data",{}).get("result",[]); print(r[0]["value"][1] if r else "0")'
}

error_rate=$(query "sum(rate(http_requests_total{namespace=\"${NAMESPACE}\",status=~\"5..\"}[5m])) / sum(rate(http_requests_total{namespace=\"${NAMESPACE}\"}[5m]))")
p99_seconds=$(query "histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{namespace=\"${NAMESPACE}\"}[5m])) by (le))")
p99_ms=$(python3 -c "print(float('${p99_seconds:-0}') * 1000)")

echo "observed error_rate=${error_rate} p99_ms=${p99_ms}"

fail=0
if python3 -c "exit(0 if float('${error_rate:-0}') <= ${MAX_ERROR_RATE} else 1)"; then
  echo "error rate OK (<= ${MAX_ERROR_RATE})"
else
  echo "error rate BREACHED threshold ${MAX_ERROR_RATE}" >&2
  fail=1
fi

if python3 -c "exit(0 if float('${p99_ms}') <= ${MAX_P99_MS} else 1)"; then
  echo "p99 latency OK (<= ${MAX_P99_MS}ms)"
else
  echo "p99 latency BREACHED threshold ${MAX_P99_MS}ms" >&2
  fail=1
fi

exit "$fail"
