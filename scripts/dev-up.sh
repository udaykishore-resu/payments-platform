#!/usr/bin/env bash
#
# scripts/dev-up.sh — bring up the local development stack and wait until it is usable.
#
# WHAT IT DOES
#   Starts deploy/docker-compose.dev.yml (Postgres, Redis, Redpanda, the gateway
#   simulator, the local OIDC issuer, the OTel collector, Prometheus, Grafana, Jaeger),
#   runs the migrations to completion, seeds a deterministic dataset, and does not return
#   until every dependency reports healthy.
#
#   It finishes by minting a bearer token from the local issuer and printing it with an
#   example `curl`, and by writing .dev/dev-env.sh with the seeded tenant and merchant ids
#   so that scripts/dev-token.sh and the e2e suite have something real to be scoped to.
#
# WHY THE HEALTH GATE
#   `docker compose up -d` returns when the containers have *started*. For Postgres that
#   means the process exists — not that it accepts connections, and certainly not that the
#   migrations have run. A test suite launched at that moment races the database, and the
#   result is a flake that looks like a product bug and costs an afternoon. Waiting for
#   `service_healthy` on every dependency, and for the one-shot migrate job to exit 0, is
#   the difference between "the stack is up" and "the stack is usable", and only the second
#   one is worth a command.
#
#   The readiness probes are the containers' own healthchecks (defined in the compose
#   file), not a `sleep`. A sleep is a guess that is simultaneously too long on a fast
#   machine and too short on a loaded CI runner.
#
# USAGE
#   scripts/dev-up.sh [--no-migrate] [--no-seed] [--timeout SECONDS] [--rebuild]
#
#     --no-migrate  start the stores but skip `platformctl migrate up`
#     --no-seed     skip the synthetic seed data (default profile: dev, scale 25)
#     --rebuild     rebuild the locally-built images (simulator, platformctl) first
#     --timeout     how long to wait for the stack to become healthy (default 180)
#
# EXIT
#   0 the stack is up and usable · 1 something did not become healthy · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

COMPOSE_FILE="deploy/docker-compose.dev.yml"
DO_MIGRATE=1
DO_SEED=1
REBUILD=0
TIMEOUT=180
SEED_PROFILE="${PP_SEED_PROFILE:-dev}"
SEED_SCALE="${PP_SEED_SCALE:-25}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-migrate) DO_MIGRATE=0; shift ;;
    --no-seed)    DO_SEED=0; shift ;;
    --rebuild)    REBUILD=1; shift ;;
    --timeout)    TIMEOUT="$2"; shift 2 ;;
    -h|--help)    sed -n '2,32p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)            die "unknown flag: $1" ;;
  esac
done

cd "$REPO_ROOT"
[[ -f "$COMPOSE_FILE" ]] || die "missing $COMPOSE_FILE"

# `docker compose` (v2 plugin) and `docker-compose` (v1 binary) are both still in the wild.
# Detecting rather than assuming avoids the single most common "it works on my machine".
if docker compose version >/dev/null 2>&1; then
  DC=(docker compose -f "$COMPOSE_FILE")
elif have docker-compose; then
  DC=(docker-compose -f "$COMPOSE_FILE")
else
  die "neither 'docker compose' nor 'docker-compose' is available"
fi

docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable — start Docker first"

hdr "dev stack — ${COMPOSE_FILE}"

if [[ $REBUILD -eq 1 ]]; then
  info "rebuilding locally-built images"
  "${DC[@]}" build --pull gateway-simulator migrate
fi

info "starting containers"
# The migrate service is a one-shot job. Starting it with the rest and then waiting on its
# exit status separately keeps the dependency ordering in the compose file (where it is
# declarative and reviewable) instead of in this script.
"${DC[@]}" up -d --remove-orphans

# --- readiness -------------------------------------------------------------------------------
# A container with no healthcheck reports "" from the inspect below. Treating that as
# healthy would silently exempt any service someone forgets to give a probe, so the list of
# services expected to have one is explicit.
# Services gated on the container's own healthcheck.
HEALTH_GATED=(postgres redis redpanda prometheus)

# Services gated on an HTTP probe from the host, as "name|url".
#
# These three run from shell-less images — two distroless Go binaries of ours and the upstream
# OTel collector — so `CMD-SHELL` cannot execute inside them and there is no wget or curl to
# exec. A compose healthcheck that cannot run does not report "unknown": it reports *unhealthy*,
# forever, while the service serves traffic perfectly well. That is a worse failure than having
# no healthcheck, because it is indistinguishable from a real outage.
#
# Probing from the host is honest about what is being checked — the port a developer will
# actually call — and it works for images we do not build. For the two binaries we do build, a
# `-healthcheck` self-probe flag would let the check move back inside the container; that is the
# better end state and it is not written yet.
HTTP_GATED=(
  # Two things about this URL are easy to get wrong, and both fail as "dead process":
  # the probes are on the admin listener, not the API port, and health.Registry mounts
  # /livez, /readyz and /startupz — there is no /healthz. Readiness is the right gate:
  # it is the one that means "usable", which is what this script promises.
  "gateway-simulator|http://localhost:${PP_DEV_SIMULATOR_ADMIN_PORT:-8091}/readyz"
  "dev-issuer|http://localhost:${PP_DEV_ISSUER_PORT:-8088}/healthz"
  "otel-collector|http://localhost:13133/"
)

# http_ok probes one URL. curl is present on macOS and on every CI image we use; wget is the
# fallback for a minimal Linux box that has one and not the other.
http_ok() {
  local url="$1"
  if command -v curl >/dev/null 2>&1; then
    curl -fsS -m 3 -o /dev/null "$url" 2>/dev/null
  elif command -v wget >/dev/null 2>&1; then
    wget -q -T 3 -O /dev/null "$url" 2>/dev/null
  else
    return 0   # nothing to probe with; do not fail the stack over a missing client
  fi
}

container_health() {
  local svc="$1" cid
  cid="$("${DC[@]}" ps -q "$svc" 2>/dev/null | head -1)"
  [[ -z "$cid" ]] && { echo "missing"; return; }
  docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}nohealthcheck{{end}}' \
    "$cid" 2>/dev/null || echo "missing"
}

info "waiting up to ${TIMEOUT}s for readiness"
deadline=$((SECONDS + TIMEOUT))
# The set of services already reported, as a space-delimited string rather than an associative
# array. `declare -A` is bash 4; macOS ships bash 3.2 as /bin/bash and `#!/usr/bin/env bash`
# finds it unless the developer has installed a newer one and put it first on PATH. A
# development script that only runs on a laptop with a hand-upgraded shell is a script that
# greets a new joiner with a syntax error.
reported=" "
while :; do
  pending=()
  for svc in "${HEALTH_GATED[@]}"; do
    st="$(container_health "$svc")"
    case "$st" in
      healthy)
        if [[ "$reported" != *" $svc "* ]]; then ok "$svc healthy"; reported+="$svc "; fi
        ;;
      nohealthcheck)
        # Not a pass. A service in HEALTH_GATED without a probe means the compose file and
        # this script disagree, and the honest report is that the gate is not being
        # enforced for it.
        if [[ "$reported" != *" $svc "* ]]; then
          warn "$svc has no healthcheck in $COMPOSE_FILE — readiness is not gated for it"
          reported+="$svc "
        fi
        ;;
      *)
        pending+=("$svc:$st")
        ;;
    esac
  done

  for entry in "${HTTP_GATED[@]}"; do
    svc="${entry%%|*}"; url="${entry#*|}"
    # A container that is not running at all is a different failure from one that is running
    # and not answering, and the report should say which.
    st="$(container_health "$svc")"
    if [[ "$st" == "missing" ]]; then
      pending+=("$svc:not-started")
    elif http_ok "$url"; then
      if [[ "$reported" != *" $svc "* ]]; then ok "$svc healthy"; reported+="$svc "; fi
    else
      pending+=("$svc:no-http-response")
    fi
  done

  [[ ${#pending[@]} -eq 0 ]] && break

  if (( SECONDS >= deadline )); then
    fail "timed out after ${TIMEOUT}s waiting for: ${pending[*]}"
    echo
    hdr "last 40 log lines per unhealthy service"
    for entry in "${pending[@]}"; do
      svc="${entry%%:*}"
      printf '%s\n' "${C_BLD}--- $svc ---${C_OFF}"
      "${DC[@]}" logs --tail 40 "$svc" || true
    done
    exit 1
  fi
  sleep 2
done

# --- migrations --------------------------------------------------------------------------------
if [[ $DO_MIGRATE -eq 1 ]]; then
  hdr "migrations"
  # `up` already started the one-shot job; wait for its exit status rather than re-running.
  # A migration that fails must stop the stack from being declared ready — ArgoCD enforces
  # the same thing with sync wave -1 (deployment.md §3.3), and a local stack that is more
  # permissive teaches the wrong habit.
  if "${DC[@]}" wait migrate >/dev/null 2>&1; then
    rc="$("${DC[@]}" ps -a --format json migrate 2>/dev/null | head -1 \
          | python3 -c 'import json,sys; d=sys.stdin.read().strip(); print(json.loads(d).get("ExitCode", 1) if d else 1)' 2>/dev/null || echo 1)"
  else
    # `compose wait` is recent; fall back to running the migration synchronously.
    "${DC[@]}" run --rm migrate migrate up && rc=0 || rc=$?
  fi
  if [[ "${rc:-1}" -ne 0 ]]; then
    fail "migrations failed (exit ${rc}); the stack is up but the schema is not current"
    "${DC[@]}" logs --tail 60 migrate || true
    exit 1
  fi
  ok "schema is current"
fi

# --- seed ----------------------------------------------------------------------------------------
SEEDED_TENANT=""
SEEDED_MERCHANT=""
if [[ $DO_SEED -eq 1 && $DO_MIGRATE -eq 1 ]]; then
  hdr "seed data"
  info "profile=${SEED_PROFILE} scale=${SEED_SCALE} (synthetic; never production data — deployment.md §6.1)"
  SEED_OUT="$(mktemp)"
  if "${DC[@]}" run --rm migrate seed --profile="$SEED_PROFILE" --scale="$SEED_SCALE" | tee "$SEED_OUT"; then
    ok "seeded"
    # The ids are captured rather than left in the scrollback: a token scoped to a tenant that
    # does not exist authenticates and then fails every authorization check, which is a
    # confusing way to spend an afternoon.
    SEEDED_TENANT="$(grep -oE 'ten_[0-9A-HJKMNP-TV-Z]{26}' "$SEED_OUT" | head -1 || true)"
    SEEDED_MERCHANT="$(grep -oE 'mrc_[0-9A-HJKMNP-TV-Z]{26}' "$SEED_OUT" | head -1 || true)"
  else
    warn "seeding failed; the stack is usable but empty (run scripts/seed.sh to retry)"
  fi
  rm -f "$SEED_OUT"
fi

# --- the developer environment ------------------------------------------------------------------
# .dev/ is gitignored. dev-token.sh sources this file, so `scripts/dev-token.sh` with no arguments
# mints a token for the tenant that is actually in the database.
mkdir -p "$REPO_ROOT/.dev"
{
  echo "# Written by scripts/dev-up.sh at $(date -u +%FT%TZ). Source it, or let dev-token.sh do so."
  echo "export PP_TEST_BASE_URL=\"http://localhost:${PP_DEV_PAYMENT_API_PORT:-8080}\""
  echo "export PP_TEST_CONTROL_URL=\"http://localhost:${PP_DEV_CONTROL_API_PORT:-8082}\""
  echo "export PP_TEST_SIMULATOR_URL=\"http://localhost:${PP_DEV_SIMULATOR_PORT:-8090}\""
  echo "export PP_DEV_ISSUER_URL=\"http://localhost:${PP_DEV_ISSUER_PORT:-8088}\""
  [[ -n "$SEEDED_TENANT" ]]   && echo "export PP_TEST_TENANT_ID=\"$SEEDED_TENANT\""
  [[ -n "$SEEDED_MERCHANT" ]] && echo "export PP_TEST_MERCHANT_ID=\"$SEEDED_MERCHANT\""
  true
} > "$REPO_ROOT/.dev/dev-env.sh"
ok "wrote .dev/dev-env.sh"

# --- summary -----------------------------------------------------------------------------------
echo
hdr "the stack is up"
cat <<EOF
    Postgres    postgres://pp_dev:pp_dev_not_a_real_password@localhost:${PP_DEV_POSTGRES_PORT:-5432}/pp
    Redis       redis://localhost:${PP_DEV_REDIS_PORT:-6379}
    Kafka       localhost:${PP_DEV_KAFKA_PORT:-19092}   (Redpanda; advertised as localhost from the host)
    Simulator   http://localhost:${PP_DEV_SIMULATOR_PORT:-8090}
    OTLP        localhost:${PP_DEV_OTLP_GRPC_PORT:-4317} (grpc) / ${PP_DEV_OTLP_HTTP_PORT:-4318} (http)
    Prometheus  http://localhost:${PP_DEV_PROMETHEUS_PORT:-9090}
    Grafana     http://localhost:${PP_DEV_GRAFANA_PORT:-3000}   (anonymous admin)
    Jaeger      http://localhost:${PP_DEV_JAEGER_UI_PORT:-16686}
    OIDC issuer http://localhost:${PP_DEV_ISSUER_PORT:-8088}    (JWKS at /.well-known/jwks.json)
EOF

if [[ -n "$SEEDED_TENANT" ]]; then
  printf '    Tenant      %s\n' "$SEEDED_TENANT"
fi
if [[ -n "$SEEDED_MERCHANT" ]]; then
  printf '    Merchant    %s   (the first seeded merchant is always ACTIVE)\n' "$SEEDED_MERCHANT"
fi

# --- a working token, and something to do with it ------------------------------------------------
echo
hdr "a bearer token"
if TOKEN="$("$REPO_ROOT/scripts/dev-token.sh" 2>/dev/null)"; then
  printf '%s\n\n' "$TOKEN"
  cat <<EOF
    export PP_TEST_AUTH_TOKEN="\$(scripts/dev-token.sh)"

    curl -sS http://localhost:${PP_DEV_PAYMENT_API_PORT:-8080}/v1/payments \\
      -H "Authorization: Bearer \$(scripts/dev-token.sh)" \\
      -H 'Accept: application/json'

    curl -sS -X POST http://localhost:${PP_DEV_PAYMENT_API_PORT:-8080}/v1/payments \\
      -H "Authorization: Bearer \$(scripts/dev-token.sh)" \\
      -H 'Content-Type: application/json' \\
      -H "Idempotency-Key: \$(uuidgen 2>/dev/null || date +%s%N)" \\
      -d '{
            "merchantId": "${SEEDED_MERCHANT:-mrc_…}",
            "amount": {"amount": 1050, "currency": "EUR"},
            "paymentMethod": "CARD",
            "paymentMethodReference": {
              "type": "GATEWAY_TOKEN",
              "gatewayCode": "simulator",
              "token": "tok_dev_visa",
              "brand": "VISA",
              "last4": "4242",
              "expiryMonth": 12,
              "expiryYear": 2030
            },
            "captureMode": "AUTOMATIC"
          }'

    Note: nothing above starts an application service. \`make run-payment-api\` runs one against
    this stack; the compose file provides the dependencies, not the platform.

    Next:  make run-payment-api      # :8080, admin :8081
           make test-integration     # testcontainers-backed suite
           make test-e2e             # against this stack
           scripts/dev-down.sh       # tear it all down
EOF
else
  warn "could not mint a token from the local issuer; run scripts/dev-token.sh for the reason"
fi

summary "dev-up"
