#!/usr/bin/env bash
# Minimal post-deploy smoke test: hits /healthz and /readyz and confirms both are green before
# the CD pipeline (.github/workflows/cd.yml) considers a deploy successful. This is deliberately
# separate from the canary analysis in canary-analysis.sh — a smoke test answers "is it up at
# all," canary analysis answers "is it behaving as well as the previous version."
set -euo pipefail

BASE_URL="${1:?usage: smoke-test.sh <base_url>}"

check() {
  local path="$1"
  local url="${BASE_URL}${path}"
  local status
  status=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "$url") || status="000"
  if [[ "$status" != "200" ]]; then
    echo "SMOKE TEST FAILED: ${url} returned HTTP ${status}" >&2
    exit 1
  fi
  echo "OK: ${url} -> ${status}"
}

check "/healthz"
check "/readyz"

echo "smoke test passed"
