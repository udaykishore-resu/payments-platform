#!/usr/bin/env bash
#
# scripts/loadtest.sh — wrapper over the k6 scenarios in tests/load/.
#
# WHAT IT DOES
#   Runs one of the four scenarios of docs/testing.md §6.2 (steady-state, ramp, spike,
#   soak) against a target, with the SLO thresholds from baseline §18 already encoded in
#   the scripts, and writes a JSON summary for the CI artifact.
#
# WHY A WRAPPER
#   Four things that must be true every time:
#
#   1. **Never production.** The script refuses any target that is not explicitly marked
#      as a load-test environment. A load test is indistinguishable from an attack at the
#      infrastructure layer and identical to one at the money layer: 5 000 TPS of synthetic
#      payments against live gateway accounts is real authorisations on real cards.
#      testing.md §6.2 says "point at staging, never at prod" — this makes it mechanical.
#
#   2. **A token must be supplied, never defaulted.** A wrapper with a baked-in token is
#      a wrapper that eventually runs with the wrong one.
#
#   3. **The thresholds are the pass criteria.** k6 exits non-zero when a threshold is
#      crossed; that exit status is this script's exit status, unmodified. A load test
#      that produces a chart for someone to interpret is a load test whose result depends
#      on who is looking.
#
#   4. **The summary is an artifact.** `--summary-export` writes the threshold results in
#      a form the nightly workflow can attach and a human can diff against last week's.
#
# USAGE
#   scripts/loadtest.sh <steady-state|ramp|spike|soak|all> [options]
#
#     --base URL        target base URL (or PP_LOADTEST_BASE)
#     --token TOKEN     bearer token   (or PP_LOADTEST_TOKEN)
#     --out DIR         where to write summaries (default: .loadtest/)
#     --vus-scale N     multiply the scenario's VU/rate targets by N/100 (default 100).
#                       For a smoke run of the whole matrix on a laptop: --vus-scale 1
#     --duration D      override the scenario duration (k6 duration syntax)
#     --dry-run         print the k6 invocation and exit
#
# EXIT
#   0 every threshold held · 1 a threshold was crossed or k6 failed · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

SCENARIOS=(steady-state ramp spike soak)
WANT=""
BASE="${PP_LOADTEST_BASE:-}"
TOKEN="${PP_LOADTEST_TOKEN:-}"
OUTDIR=".loadtest"
VUS_SCALE=100
DURATION=""
DRY_RUN=0

[[ $# -gt 0 ]] || { sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 2; }
WANT="$1"; shift

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base)      BASE="$2"; shift 2 ;;
    --token)     TOKEN="$2"; shift 2 ;;
    --out)       OUTDIR="$2"; shift 2 ;;
    --vus-scale) VUS_SCALE="$2"; shift 2 ;;
    --duration)  DURATION="$2"; shift 2 ;;
    --dry-run)   DRY_RUN=1; shift ;;
    -h|--help)   sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)           die "unknown flag: $1" ;;
  esac
done

cd "$REPO_ROOT"

case "$WANT" in
  all) RUN=("${SCENARIOS[@]}") ;;
  steady-state|ramp|spike|soak) RUN=("$WANT") ;;
  *) die "unknown scenario: $WANT (steady-state, ramp, spike, soak, all)" ;;
esac

[[ -n "$BASE" ]]  || die "no target: pass --base or set PP_LOADTEST_BASE"
[[ -n "$TOKEN" ]] || die "no token: pass --token or set PP_LOADTEST_TOKEN"

# --- the production guard --------------------------------------------------------------------
#
# Allow-list rather than deny-list. A deny-list of production hostnames is a list someone
# forgets to update the day a new region is added; an allow-list fails closed, which is the
# correct direction for a control whose failure mode is "5 000 TPS of card authorisations
# against live merchant accounts".
if [[ ! "$BASE" =~ (staging|stg|perf|loadtest|preview|localhost|127\.0\.0\.1|host\.docker\.internal) ]]; then
  die "refusing to load-test ${BASE}.

    The target must be a load-test environment: its host must contain 'staging', 'stg',
    'perf', 'loadtest', 'preview', or be a loopback address. testing.md §6.2: point at
    staging, never at prod.

    At the infrastructure layer a load test is indistinguishable from a denial-of-service
    attack, and at the money layer it is not a simulation — every POST /v1/payments is a
    real authorisation against whatever gateway account the target is configured with.

    If a new environment name needs to be accepted, add it to the pattern in this script
    in a reviewed change. Do not bypass this by editing it locally."
fi

# --- k6 ----------------------------------------------------------------------------------------
if have k6; then
  K6=(k6)
elif have docker; then
  info "k6 is not installed; using the grafana/k6 container"
  K6=(docker run --rm -i
      -e "BASE=$BASE" -e "TOKEN=$TOKEN" -e "VUS_SCALE=$VUS_SCALE"
      -v "$PWD:/work" -w /work
      --network host
      grafana/k6:0.55.0)
else
  die "k6 is not installed and Docker is unavailable.
    Install k6:  https://grafana.com/docs/k6/latest/set-up/install-k6/
    or: go install go.k6.io/k6@latest"
fi

mkdir -p "$OUTDIR"

hdr "load test — ${RUN[*]} → ${BASE}"
info "thresholds are the pass criteria: k6's exit status is this script's exit status"
[[ "$VUS_SCALE" != "100" ]] && warn "--vus-scale ${VUS_SCALE}: the SLO thresholds still \
apply, but a scaled-down run does not exercise the capacity the §18 targets are about"

for scenario in "${RUN[@]}"; do
  script="tests/load/${scenario}.js"
  [[ -f "$script" ]] || die "missing $script"

  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  summary="${OUTDIR}/${scenario}-${stamp}.summary.json"

  ARGS=(run
        --env "BASE=${BASE}"
        --env "TOKEN=${TOKEN}"
        --env "VUS_SCALE=${VUS_SCALE}"
        --env "SCENARIO=${scenario}"
        --summary-export "$summary"
        --out "json=${OUTDIR}/${scenario}-${stamp}.metrics.json")
  [[ -n "$DURATION" ]] && ARGS+=(--duration "$DURATION")
  ARGS+=("$script")

  hdr "${scenario}"
  if [[ $DRY_RUN -eq 1 ]]; then
    info "${K6[*]} ${ARGS[*]}"
    continue
  fi

  # The token is passed through --env, which k6 reads from this process's environment when
  # the value is omitted. It is written here explicitly for the container case; either way
  # it never lands in the summary file, which is uploaded as a CI artifact.
  if "${K6[@]}" "${ARGS[@]}"; then
    ok "${scenario}: every threshold held"
    info "summary: ${summary}"
  else
    rc=$?
    fail "${scenario}: k6 exited ${rc} — a threshold was crossed (see the summary above)"
    info "summary: ${summary}"
    info "read it with: jq '.metrics | to_entries[] | select(.value.thresholds)' ${summary}"
  fi
done

summary "loadtest"
