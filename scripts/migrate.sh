#!/usr/bin/env bash
#
# scripts/migrate.sh — wrapper over `platformctl migrate`.
#
# WHAT IT DOES
#   Resolves the DSN from the environment, refuses to run against production without an
#   explicit acknowledgement, runs `scripts/check-migrations.sh` first, and then invokes
#   `platformctl migrate <up|status|plan>`.
#
# WHY A WRAPPER RATHER THAN CALLING platformctl DIRECTLY
#   Three things that must happen every time and that nobody remembers every time:
#
#   1. The static checks run first. A migration with a gap in its numbering or an unmarked
#      DROP is going to fail in CI anyway; failing here costs two seconds instead of a
#      round trip, and — more importantly — the destructive-statement marker is checked
#      *before* the statement runs rather than after.
#
#   2. Production is refused unless PP_CONFIRM_ENVIRONMENT names it. Migrations against
#      production run from a PreSync hook Job in the cluster (deployment.md §3.3, §5.3),
#      never from a laptop. The guard is here because "just this once, from my machine"
#      is how the incident starts, and a prompt is a control that costs nothing.
#
#   3. The DSN never reaches the log. It is passed through the environment, and every
#      message prints host/database only.
#
# FORWARD-ONLY
#   There is deliberately no `down` subcommand. deployment.md §5.4 is explicit: a
#   migration is reverted by a new, numbered, compensating migration, not by running the
#   down script against a live database. The down scripts exist so the reversal is written,
#   reviewed and testable — `platformctl migrate up` in a throwaway database is where they
#   run. Making that inconvenient here is intentional.
#
# USAGE
#   scripts/migrate.sh [up|status|plan] [--dsn URL] [--dry-run] [--no-check]
#
#   Environment: PP_DSN (or DATABASE_URL), PP_ENVIRONMENT, PP_CONFIRM_ENVIRONMENT
#
# EXIT
#   0 success · 1 a check or the migration failed · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

SUBCMD="up"
DSN="${PP_DSN:-${DATABASE_URL:-}}"
DRY_RUN=0
RUN_CHECKS=1

while [[ $# -gt 0 ]]; do
  case "$1" in
    up|status|plan) SUBCMD="$1"; shift ;;
    down)
      die "there is no 'down'. deployment.md §5.4: a migration is reverted by a new
      numbered compensating migration, never by running a down script against a live
      database. To rehearse a reversal, apply it to a throwaway database."
      ;;
    --dsn)      DSN="$2"; shift 2 ;;
    --dry-run)  DRY_RUN=1; shift ;;
    --no-check) RUN_CHECKS=0; shift ;;
    -h|--help)  sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)          die "unknown argument: $1" ;;
  esac
done

cd "$REPO_ROOT"
[[ -n "$DSN" ]] || die "no DSN: set PP_DSN or DATABASE_URL, or pass --dsn"

# Redacted description of the target, for every message this script prints.
target() {
  python3 - "$DSN" <<'PY' 2>/dev/null || echo "<unparseable DSN>"
import sys, urllib.parse as u
p = u.urlparse(sys.argv[1])
print(f"{p.hostname or '?'}:{p.port or 5432}{p.path or ''}")
PY
}
TARGET="$(target)"

ENVIRONMENT="${PP_ENVIRONMENT:-development}"
hdr "migrate ${SUBCMD} — ${TARGET} (${ENVIRONMENT})"

# --- the production guard ---------------------------------------------------------------------
case "$ENVIRONMENT" in
  prod|production)
    if [[ "${PP_CONFIRM_ENVIRONMENT:-}" != "$ENVIRONMENT" ]]; then
      die "refusing to migrate production from this script.

    Production migrations run as the ArgoCD PreSync hook Job in sync wave -1
    (deployment.md §3.3), under the advisory lock, with the change record and the
    rollout gate attached. Running one from a shell bypasses all of that and leaves no
    trace anyone can find during the incident review.

    If this really is the break-glass path, it is:
        PP_CONFIRM_ENVIRONMENT=$ENVIRONMENT scripts/migrate.sh $SUBCMD
    and it needs a ticket number in the audit record."
    fi
    warn "running against PRODUCTION with an explicit acknowledgement"
    ;;
esac

# --- static checks first --------------------------------------------------------------------------
if [[ $RUN_CHECKS -eq 1 && "$SUBCMD" != "status" ]]; then
  info "running scripts/check-migrations.sh before touching the database"
  if ! ./scripts/check-migrations.sh; then
    die "the migration set does not pass its own static checks; not applying anything"
  fi
fi

# --- resolve platformctl ------------------------------------------------------------------------
# Prefer an installed binary; fall back to `go run`, which is what a developer working on
# the tool itself wants and what a clean checkout needs.
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

ARGS=(migrate "$SUBCMD")
[[ $DRY_RUN -eq 1 ]] && ARGS+=(--dry-run)

info "invoking: ${CTL[*]} ${ARGS[*]}   (DSN passed via PP_DSN, never on the command line)"
if PP_DSN="$DSN" "${CTL[@]}" "${ARGS[@]}"; then
  ok "migrate ${SUBCMD} completed against ${TARGET}"
else
  rc=$?
  fail "migrate ${SUBCMD} exited ${rc}"
  info "the schema is in whatever state the last successful migration left it; run"
  info "  scripts/migrate.sh status   to see applied vs pending before retrying"
  summary "migrate"
  exit 1
fi

summary "migrate"
