#!/usr/bin/env bash
#
# scripts/seed.sh — wrapper over `platformctl seed`.
#
# WHAT IT DOES
#   Generates a deterministic synthetic dataset — tenants, merchants across every
#   lifecycle state (baseline §8), payments across every state (§9), gateway connections,
#   configurations, ledger entries, reconciliation exceptions — into the target database.
#
# WHY THE DATA IS GENERATED AND NEVER COPIED
#   deployment.md §6.1 states the rule and the reasoning, and it is worth restating at the
#   point of use: anonymising a relational payment dataset is not reliably achievable.
#   Merchant names, bank-account fragments, amounts, timestamps and gateway references
#   re-identify in combination, so a "scrubbed" production dump is a breach that has not
#   been noticed yet. This script therefore has no import path, no --from-dump flag, and
#   refuses to run against production at all — there is no acknowledgement that makes
#   seeding production a reasonable thing to do.
#
# DETERMINISM
#   The same profile, scale and seed produce byte-identical data. That is what lets a test
#   assert on a specific merchant's configuration without first querying for it, and what
#   makes "reproduce it locally" a real instruction rather than an aspiration
#   (testing.md §8.2).
#
# USAGE
#   scripts/seed.sh [--profile NAME] [--scale N] [--seed INT] [--dsn URL] [--reset]
#
#     --profile  dev (default) | integration | load | e2e | minimal
#     --scale    multiplier for the profile's base counts (default 25)
#     --seed     PRNG seed; defaults to a fixed constant so runs are reproducible
#     --reset    truncate the seeded tables first (never in production; see above)
#
# EXIT
#   0 seeded · 1 seeding failed · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

PROFILE="${PP_SEED_PROFILE:-dev}"
SCALE="${PP_SEED_SCALE:-25}"
# A fixed default seed, not $RANDOM and not the clock. A seed that changes per run turns
# "the fixture had merchant mrc_01J…" into an unanswerable question.
SEED="${PP_SEED_VALUE:-1724680000000000000}"
DSN="${PP_DSN:-${DATABASE_URL:-}}"
RESET=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile) PROFILE="$2"; shift 2 ;;
    --scale)   SCALE="$2"; shift 2 ;;
    --seed)    SEED="$2"; shift 2 ;;
    --dsn)     DSN="$2"; shift 2 ;;
    --reset)   RESET=1; shift ;;
    -h|--help) sed -n '2,35p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)         die "unknown flag: $1" ;;
  esac
done

cd "$REPO_ROOT"
[[ -n "$DSN" ]] || die "no DSN: set PP_DSN or DATABASE_URL, or pass --dsn"
[[ "$SCALE" =~ ^[0-9]+$ ]] || die "--scale must be an integer, got: $SCALE"

case "$PROFILE" in
  dev|integration|load|e2e|minimal) : ;;
  *) die "unknown profile: $PROFILE (dev, integration, load, e2e, minimal)" ;;
esac

ENVIRONMENT="${PP_ENVIRONMENT:-development}"
case "$ENVIRONMENT" in
  prod|production)
    die "refusing to seed production, and there is no override.

    Seed data is synthetic data written into merchant, payment and ledger tables. In
    production those tables hold money. deployment.md §6.1: non-prod data is generated,
    production data is never generated. If you need production-shaped data to reproduce
    something, build a synthetic case from the *structure* of the production case —
    states, amounts, currencies, gateway, timing — taken from traces and metrics."
    ;;
esac

target() {
  python3 - "$DSN" <<'PY' 2>/dev/null || echo "<unparseable DSN>"
import sys, urllib.parse as u
p = u.urlparse(sys.argv[1])
print(f"{p.hostname or '?'}:{p.port or 5432}{p.path or ''}")
PY
}

hdr "seed — profile=${PROFILE} scale=${SCALE} seed=${SEED} → $(target)"

if have platformctl; then
  CTL=(platformctl)
elif [[ -x "./bin/platformctl" ]]; then
  CTL=(./bin/platformctl)
elif [[ -d "./cmd/platformctl" ]] && compgen -G "./cmd/platformctl/*.go" >/dev/null; then
  info "platformctl is not installed; running it from source"
  CTL=(go run ./cmd/platformctl)
else
  die "platformctl is not available: no binary on PATH, none in ./bin, and ./cmd/platformctl has no sources yet"
fi

ARGS=(seed "--profile=$PROFILE" "--scale=$SCALE" "--seed=$SEED")
[[ $RESET -eq 1 ]] && ARGS+=(--reset)

if PP_DSN="$DSN" "${CTL[@]}" "${ARGS[@]}"; then
  ok "seeded profile=${PROFILE} scale=${SCALE}"
  info "the dataset is deterministic: the same --profile/--scale/--seed reproduces it exactly"
else
  fail "seeding failed"
  summary "seed"
  exit 1
fi

summary "seed"
