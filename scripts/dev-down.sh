#!/usr/bin/env bash
#
# scripts/dev-down.sh — tear the local development stack down completely.
#
# WHAT IT DOES
#   Stops and removes every container, network and volume created by
#   deploy/docker-compose.dev.yml, and by default removes the images built from this
#   repository so that the next `dev-up.sh --rebuild` starts from the current source.
#
# WHY IT REMOVES VOLUMES BY DEFAULT
#   A local stack is disposable by definition, and a surviving Postgres volume is the most
#   reliable way to produce a bug that reproduces on exactly one machine: the schema is
#   from three weeks ago, the migrations "already ran", and the failure is blamed on the
#   code. `--keep-volumes` exists for the rare case where a manual investigation is in
#   progress, and it prints a warning saying what the next `dev-up.sh` will and will not
#   do with that state.
#
#   If state needs to survive, it belongs in a migration or a seed profile
#   (`platformctl seed --profile`), which are reviewed, versioned and reproducible. A
#   volume is none of those things.
#
# USAGE
#   scripts/dev-down.sh [--keep-volumes] [--keep-images] [--quiet]
#
# EXIT
#   0 always, unless Docker itself is unreachable. Tearing down something that is already
#   down is a success, not an error — a teardown that fails when there is nothing to tear
#   down cannot be put in a trap or a CI `always` step, which is exactly where it belongs.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

COMPOSE_FILE="deploy/docker-compose.dev.yml"
KEEP_VOLUMES=0
KEEP_IMAGES=1     # locally-built images are kept unless asked otherwise: rebuilding the
                  # simulator on every dev-up is a minute nobody chose to spend
QUIET=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep-volumes) KEEP_VOLUMES=1; shift ;;
    --keep-images)  KEEP_IMAGES=1; shift ;;
    --purge-images) KEEP_IMAGES=0; shift ;;
    --quiet)        QUIET=1; shift ;;
    -h|--help)      sed -n '2,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)              die "unknown flag: $1" ;;
  esac
done

cd "$REPO_ROOT"
[[ -f "$COMPOSE_FILE" ]] || die "missing $COMPOSE_FILE"

if docker compose version >/dev/null 2>&1; then
  DC=(docker compose -f "$COMPOSE_FILE")
elif have docker-compose; then
  DC=(docker-compose -f "$COMPOSE_FILE")
else
  die "neither 'docker compose' nor 'docker-compose' is available"
fi

docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"

[[ $QUIET -eq 0 ]] && hdr "tearing down the dev stack"

ARGS=(down --remove-orphans)
if [[ $KEEP_VOLUMES -eq 0 ]]; then
  ARGS+=(--volumes)
else
  [[ $QUIET -eq 0 ]] && warn "keeping volumes: the next dev-up.sh will run migrations against \
whatever schema version is already there, and will NOT re-seed"
fi
if [[ $KEEP_IMAGES -eq 0 ]]; then
  ARGS+=(--rmi local)
fi

# `down` on an already-stopped stack exits 0 and prints nothing interesting, which is why
# this script is safe in a trap.
if [[ $QUIET -eq 1 ]]; then
  "${DC[@]}" "${ARGS[@]}" >/dev/null 2>&1 || true
else
  "${DC[@]}" "${ARGS[@]}"
fi

# Belt and braces: compose only removes what its project labels claim. A container left
# behind by an interrupted `docker run` during debugging carries the same stack label and
# would otherwise hold the ports.
STRAYS="$(docker ps -aq --filter 'label=com.payments-platform.stack=dev' 2>/dev/null || true)"
if [[ -n "$STRAYS" ]]; then
  [[ $QUIET -eq 0 ]] && info "removing $(wc -w <<< "$STRAYS") stray labelled container(s)"
  # shellcheck disable=SC2086 # deliberate word splitting: docker rm takes a list of IDs
  docker rm -f $STRAYS >/dev/null 2>&1 || true
fi

if [[ $QUIET -eq 0 ]]; then
  ok "stack removed"
  summary "dev-down"
fi
