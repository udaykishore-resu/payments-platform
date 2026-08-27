#!/usr/bin/env bash
#
# scripts/verify.sh — everything CI verifies, in the order CI verifies it.
#
# WHAT IT ENFORCES
#   The gate list of docs/spec/00-design-baseline.md §27 item 12 and docs/deployment.md
#   §4.1, in one command:
#
#     1  gofmt -l              formatting drift
#     2  go vet                the compiler's second opinion
#     3  go build ./...        it compiles
#     4  go test -race         correctness and the race detector (non-negotiable on a
#                              concurrent money path — deployment.md §4.1 stage 7)
#     5  golangci-lint run     the linter configuration in .golangci.yml
#     6  check-architecture    baseline §4 dependency rule
#     7  check-error-catalog   pkg/apierror ↔ api/errors/catalog.yaml
#     8  check-rules-documented §21 rule-documentation
#     9  check-metrics-cardinality §22.3
#    10  check-events          event registry ↔ api/events/
#    11  check-openapi         the REST contract
#    12  check-migrations      numbering, pairing, RLS, destructive markers
#    13  check-secrets         credentials, keys, PANs
#    14  check-licences        no copyleft
#    15  check-runbook-links   every runbook_url resolves; every paging alert has one
#    16  check-doc-references  every repo-relative path a document cites exists
#    17  coverage              docs/testing.md §1.1 coverage floors and §1.2 critical paths
#
# WHY THIS ORDER
#   Cheapest and most-likely-to-fail first. gofmt takes 200 ms and catches the most common
#   reason a PR is red; the race detector takes minutes. A developer who has to wait four
#   minutes to be told about a formatting problem stops running the script, and a
#   pre-push check nobody runs is a check that does not exist.
#
# WHY IT KEEPS GOING
#   By default every stage runs even after one fails, and the summary lists all of them.
#   The alternative — stop at the first failure — turns one CI round trip into five. Pass
#   --fail-fast for the opposite behaviour when iterating on a single stage.
#
# USAGE
#   scripts/verify.sh [--fast] [--fail-fast] [--only NAME[,NAME...]] [--skip NAME[,NAME...]]
#
#     --fast       skip the race detector and the integration-tagged tests (the ~4 min
#                  "every push" tier of deployment.md §4.1 rather than the full set)
#     --only       run only the named stages (see the names in the table above)
#     --skip       run everything except the named stages
#     --fail-fast  stop at the first failing stage
#
# EXIT
#   0 every stage passed · 1 at least one stage failed · 2 could not run.

set -uo pipefail   # NOT -e: this script's whole job is to run every stage and report.
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

FAST=0
FAIL_FAST=0
ONLY=""
SKIP=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --fast)      FAST=1; shift ;;
    --fail-fast) FAIL_FAST=1; shift ;;
    --only)      ONLY="$2"; shift 2 ;;
    --skip)      SKIP="$2"; shift 2 ;;
    -h|--help)   sed -n '2,45p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)           die "unknown flag: $1" ;;
  esac
done

need go
# `|| die` because this script runs without `set -e` (it must survive a failing
# stage), so an unchecked cd would silently run every gate in the wrong directory.
cd "$REPO_ROOT" || die "cannot cd to $REPO_ROOT"

STAGES_RUN=()
STAGES_FAILED=()
STAGES_SKIPPED=()

selected() {
  local name="$1"
  if [[ -n "$ONLY" ]] && [[ ",$ONLY," != *",$name,"* ]]; then return 1; fi
  if [[ -n "$SKIP" ]] && [[ ",$SKIP," == *",$name,"* ]]; then return 1; fi
  return 0
}

# stage NAME DESCRIPTION -- COMMAND...
stage() {
  local name="$1" desc="$2"; shift 2
  [[ "$1" == "--" ]] && shift

  if ! selected "$name"; then
    STAGES_SKIPPED+=("$name")
    return 0
  fi
  if (( FAIL_FAST && ${#STAGES_FAILED[@]} > 0 )); then
    STAGES_SKIPPED+=("$name")
    return 0
  fi

  hdr "$name — $desc"
  local start elapsed rc
  start=$SECONDS
  "$@"
  rc=$?
  elapsed=$((SECONDS - start))
  STAGES_RUN+=("$name")
  if (( rc == 0 )); then
    ok "$name (${elapsed}s)"
  else
    STAGES_FAILED+=("$name")
    fail "$name exited $rc (${elapsed}s)"
  fi
  return 0
}

# --- 1. formatting -------------------------------------------------------------------------
gofmt_check() {
  local out
  out="$(gofmt -l . 2>&1 | grep -v '^$')"
  if [[ -n "$out" ]]; then
    printf '%s\n' "$out" | while IFS= read -r f; do
      printf '    %s needs gofmt\n' "$f" >&2
    done
    return 1
  fi
  return 0
}
stage fmt "gofmt -l ." -- gofmt_check

# --- 2. vet --------------------------------------------------------------------------------
stage vet "go vet ./..." -- go vet ./...

# --- 3. build ------------------------------------------------------------------------------
stage build "go build ./..." -- go build ./...

# --- 4. tests ------------------------------------------------------------------------------
if (( FAST )); then
  stage test "go test ./... -short" -- go test ./... -short
else
  # -count=1 defeats the test cache. A cached PASS from before the change under test is
  # the single most misleading thing a verification script can print.
  stage test-race "go test ./... -race -count=1" -- go test ./... -race -count=1
fi

# --- 5. lint -------------------------------------------------------------------------------
lint() {
  if have golangci-lint; then
    golangci-lint run ./...
    return $?
  fi
  if offline; then
    skip "golangci-lint is not installed and there is no network"
    return 0
  fi
  info "golangci-lint not installed; running it via go run at the pinned version"
  go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0 run ./...
}
stage lint "golangci-lint run" -- lint

# --- 6..14. the repository's own checks ------------------------------------------------------
stage architecture         "baseline §4 dependency rule"      -- ./scripts/check-architecture.sh
stage error-catalog        "pkg/apierror ↔ catalog.yaml"      -- ./scripts/check-error-catalog.sh
stage rules-documented     "§21 rule documentation"           -- ./scripts/check-rules-documented.sh
stage metrics-cardinality  "§22.3 metric labels"              -- ./scripts/check-metrics-cardinality.sh
stage events               "event registry ↔ api/events"      -- ./scripts/check-events.sh
stage openapi              "REST contract"                    -- ./scripts/check-openapi.sh
stage migrations           "numbering, pairing, RLS"          -- ./scripts/check-migrations.sh
stage secrets              "credentials, keys, PANs"          -- ./scripts/check-secrets.sh
stage licences             "no copyleft in the graph"         -- ./scripts/check-licences.sh
stage runbook-links        "alert runbook_url ↔ docs/runbooks" -- ./scripts/check-runbook-links.sh
stage doc-references       "every path a document cites exists" -- ./scripts/check-doc-references.sh

# Coverage runs last because it re-runs the -short suite to produce a profile. With --fast
# only the critical-path registry is checked, which is instantaneous and is the half that
# catches a renamed test.
coverage_gate() {
  if (( FAST )); then
    ./scripts/coverage.sh --only-paths
    return $?
  fi
  ./scripts/coverage.sh
}
stage coverage             "§1.1 coverage floors, §1.2 critical paths" -- coverage_gate

# --- summary ---------------------------------------------------------------------------------
echo
hdr "verify summary"
info "ran ${#STAGES_RUN[@]} stage(s); skipped ${#STAGES_SKIPPED[@]}"
if (( ${#STAGES_SKIPPED[@]} > 0 )); then
  info "skipped: ${STAGES_SKIPPED[*]}"
fi

if (( ${#STAGES_FAILED[@]} == 0 )); then
  printf '%s\n' "${C_GRN}${C_BLD}✓ verify: all ${#STAGES_RUN[@]} stage(s) passed${C_OFF}"
  if (( FAST )); then
    warn "--fast was used: the race detector did not run. CI will run it."
  fi
  exit 0
fi

printf '%s\n' "${C_RED}${C_BLD}✗ verify: ${#STAGES_FAILED[@]} stage(s) failed${C_OFF}" >&2
for s in "${STAGES_FAILED[@]}"; do
  printf '%s\n' "    ${C_RED}✗${C_OFF} $s   ${C_DIM}(rerun alone: scripts/verify.sh --only $s)${C_OFF}" >&2
done
exit 1
