#!/usr/bin/env bash
#
# scripts/dev-token.sh — mint a local bearer token and print it on stdout.
#
# WHAT IT DOES
#   Asks the local development OIDC issuer (scripts/devissuer, started by dev-up.sh or by
#   `make run-dev-issuer`) for an access token and prints the raw JWT — nothing else — so
#   that the documented idiom works:
#
#       export PP_TEST_AUTH_TOKEN="$(scripts/dev-token.sh)"
#
#   Everything human-readable goes to stderr, so a token captured in a command substitution
#   is never polluted by a banner.
#
# WHY A REAL ISSUER RATHER THAN A FIXTURE
#   internal/platform/authn has no "skip authentication in development" switch, and should not:
#   a switch like that is one environment variable away from being on in production. The
#   validator wants a real JWKS endpoint, an RS256 signature, an exact `iss` and `aud`, a
#   `jti`, a well-formed `tenant_id` and an `env` claim equal to the deployment's environment.
#   The issuer under scripts/devissuer satisfies all of them, and its own test drives the
#   platform's validator to prove it.
#
# USAGE
#   scripts/dev-token.sh [--tenant ten_…] [--scope "a b c"] [--ttl 15m]
#                        [--audience AUD] [--url http://localhost:8088] [--json] [--curl]
#
#     --tenant    tenant_id to scope the token to (default: the issuer's, or $PP_TEST_TENANT_ID)
#     --merchant  merchant id(s) for the merchant_scope claim (default: $PP_TEST_MERCHANT_ID)
#     --scope     space-delimited scopes (default: the issuer's set)
#     --ttl       token lifetime as a Go duration (default: the issuer's, 15m)
#     --audience  the `aud` claim; must equal PP_AUTH_AUDIENCE
#     --url       issuer base URL (default: $PP_DEV_ISSUER_URL or http://localhost:8088)
#     --json      print the whole token response instead of just the JWT
#     --curl      also print an example authenticated request, on stderr
#
# EXIT
#   0 a token was printed · 1 the issuer answered but refused · 2 the issuer is not reachable.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

ISSUER_URL="${PP_DEV_ISSUER_URL:-http://localhost:8088}"
TENANT="${PP_TEST_TENANT_ID:-}"
SCOPE=""
# NOT defaulted from PP_TEST_MERCHANT_ID: setting merchant_scope narrows the credential, and a
# narrowed credential is denied every tenant-wide operation — `GET /v1/payments` with no
# merchantId returns 403, because the policy engine will not widen a scoped grant. That is
# correct behaviour and the least obvious 403 in the platform, so it is opt-in via --merchant.
MERCHANT=""
TTL=""
AUDIENCE=""
AS_JSON=0
SHOW_CURL=0
BASE_URL="${PP_TEST_BASE_URL:-http://localhost:8080}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tenant)   TENANT="$2"; shift 2 ;;
    --merchant) MERCHANT="$2"; shift 2 ;;
    --scope)    SCOPE="$2"; shift 2 ;;
    --ttl)      TTL="$2"; shift 2 ;;
    --audience) AUDIENCE="$2"; shift 2 ;;
    --url)      ISSUER_URL="${2%/}"; shift 2 ;;
    --json)     AS_JSON=1; shift ;;
    --curl)     SHOW_CURL=1; shift ;;
    -h|--help)  sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)          die "unknown flag: $1" ;;
  esac
done

need curl

# `.dev/dev-env.sh` is written by dev-up.sh and carries the seeded tenant and merchant ids.
# Sourcing it means `scripts/dev-token.sh` with no arguments produces a token scoped to the
# tenant that actually exists in the local database, which is the difference between a token
# that authenticates and a token that also authorizes.
if [[ -z "$TENANT" && -f "$REPO_ROOT/.dev/dev-env.sh" ]]; then
  # shellcheck disable=SC1091
  source "$REPO_ROOT/.dev/dev-env.sh"
  TENANT="${PP_TEST_TENANT_ID:-}"
fi

QUERY=""
# Note the explicit `return 0`: with `set -e`, a function whose last command is a failed test
# aborts the script, and an empty optional parameter is not an error.
add_param() {
  [[ -n "$2" ]] || return 0
  QUERY="${QUERY}${QUERY:+&}$1=$(printf '%s' "$2" | tr ' ' '+')"
}
add_param tenant_id "$TENANT"
add_param scope          "$SCOPE"
add_param merchant_scope "$MERCHANT"
add_param ttl       "$TTL"
add_param audience  "$AUDIENCE"

URL="${ISSUER_URL%/}/token${QUERY:+?$QUERY}"

if ! RESPONSE="$(curl -fsS --max-time 5 "$URL" 2>/dev/null)"; then
  printf '%s\n' "${C_RED}error:${C_OFF} the local OIDC issuer at ${ISSUER_URL} is not reachable." >&2
  cat >&2 <<'EOF'

    The issuer is what config/dev.yaml points PP_AUTH_JWKS_URL at. Start it with either:

        make dev-up            # brings up the whole local stack, issuer included
        make run-dev-issuer    # just the issuer, in the foreground

    Then re-run this script.
EOF
  exit 2
fi

# jq is not required: the response is a flat, single-line JSON object this issuer produces, and
# requiring jq for a token would make the quick start depend on a tool that is not in go.mod.
extract() { printf '%s' "$RESPONSE" | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p"; }

TOKEN="$(extract access_token)"
if [[ -z "$TOKEN" ]]; then
  printf '%s\n' "${C_RED}error:${C_OFF} the issuer did not return an access_token:" >&2
  printf '%s\n' "$RESPONSE" >&2
  exit 1
fi

TOKEN_TENANT="$(extract tenant_id)"
TOKEN_SCOPE="$(extract scope)"

if [[ $AS_JSON -eq 1 ]]; then
  printf '%s\n' "$RESPONSE"
else
  printf '%s\n' "$TOKEN"
fi

{
  printf '\n'
  printf '%s\n' "${C_DIM}    tenant  ${TOKEN_TENANT}${C_OFF}"
  printf '%s\n' "${C_DIM}    scopes  ${TOKEN_SCOPE}${C_OFF}"
  printf '%s\n' "${C_DIM}    issuer  ${ISSUER_URL}${C_OFF}"
} >&2

if [[ $SHOW_CURL -eq 1 ]]; then
  cat >&2 <<EOF

    Example authenticated request:

      curl -sS ${BASE_URL}/v1/payments \\
        -H "Authorization: Bearer \$(scripts/dev-token.sh)" \\
        -H 'Accept: application/json'

    And a payment (note: amount is {amount, currency}, and the payment method reference is a
    discriminated union with a \`type\`):

      curl -sS -X POST ${BASE_URL}/v1/payments \\
        -H "Authorization: Bearer \$(scripts/dev-token.sh)" \\
        -H 'Content-Type: application/json' \\
        -H "Idempotency-Key: \$(uuidgen 2>/dev/null || date +%s%N)" \\
        -d '{
              "merchantId": "'"\${PP_TEST_MERCHANT_ID:-mrc_…}"'",
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
EOF
fi
