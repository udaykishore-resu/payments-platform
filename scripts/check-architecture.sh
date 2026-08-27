#!/usr/bin/env bash
#
# scripts/check-architecture.sh — the architecture fitness function.
#
# WHAT IT ENFORCES
#   The dependency rule of docs/spec/00-design-baseline.md §4, mechanically, on the package
#   graph the Go toolchain reports:
#
#     R1  pkg/**                        imports stdlib only.
#     R2  internal/domain/**            imports stdlib, other domain packages and pkg/** only.
#     R3  internal/application/**       imports no internal/infrastructure/** and no
#                                       internal/adapters/** except internal/adapters/gateway/spi.
#     R4  internal/adapters/gateway/spi imports stdlib, domain and pkg/** only.
#     R5  internal/validation/**,
#         internal/workflows/engine     import no internal/infrastructure/**.
#     R6  no package imports an external `_test` package.
#     H1  no non-test file under cmd/** exceeds N lines (default 300).
#     H2  no loop under cmd/** operates on a type from internal/domain/**.
#
# WHY
#   Baseline §4 is a design decision whose whole value is that it holds. A layering rule
#   defended only by review is a rule that decays: the first `database/sql` import in a
#   domain package is a two-line diff nobody objects to, and by the time it is obvious the
#   domain is untestable without a container and the "wide, fast base of the test pyramid"
#   the strategy depends on (testing.md §1) no longer exists. This check is what converts
#   the rule from an intention into a property.
#
#   R4 is the guard rail on R3's single exception. internal/adapters/gateway/spi is
#   importable from the application layer because it is a port declaration that happens to
#   live next to its implementations. That argument holds only while spi imports nothing a
#   port could not import — so R4 asserts exactly that. Without R4 the exception is a hole
#   through which the whole adapters tree eventually arrives.
#
#   H1 and H2 are HEURISTICS, and are documented as such. "Business logic" is not
#   mechanically decidable. The two proxies were chosen because they are cheap, produce a
#   specific message, and have a low false-positive rate on wiring code:
#     H1  a composition root is a flat list of constructor calls; length is where logic
#         hides. 300 lines is deliberately generous — a nine-service wiring file with
#         config, telemetry, pools, servers and shutdown fits comfortably under it.
#     H2  wiring iterates over config entries and service descriptors, never over payments
#         or merchants. A `for … range` whose body touches an internal/domain/** type is a
#         decision being made in the wrong layer. Detected on the AST, so a domain name in
#         a comment or a log string does not trip it.
#   Both are suppressible in the source they apply to with
#       //archcheck:allow H2-cmd-domain-iteration <reason>
#   where the reason is mandatory: an unexplained suppression is indistinguishable from an
#   accident. Suppressions are visible in review, which is the point.
#
# USAGE
#   scripts/check-architecture.sh [--max-cmd-lines N] [--json]
#
# EXIT
#   0 clean · 1 one or more violations · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

MAX_CMD_LINES=300
AS_JSON=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --max-cmd-lines) MAX_CMD_LINES="$2"; shift 2 ;;
    --json)          AS_JSON=1; shift ;;
    -h|--help)       sed -n '2,50p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)               die "unknown flag: $1" ;;
  esac
done

need go
cd "$REPO_ROOT"

hdr "architecture — baseline §4 dependency rule"
info "module graph via 'go list -json ./...'; cmd/** heuristics on the AST"
info "cmd/** file-length limit: ${MAX_CMD_LINES} lines"

if [[ $AS_JSON -eq 1 ]]; then
  exec go run ./scripts/archcheck -root . -cmd-max-lines "$MAX_CMD_LINES" -json
fi

# archcheck prints one violation per line: RULE<TAB>PACKAGE<TAB>DETAIL. It exits 1 when it
# found violations and 2 when it could not run; those are different outcomes and are
# reported differently, because "the check is broken" must never read as "the check passed".
set +e
OUT="$(go run ./scripts/archcheck -root . -cmd-max-lines "$MAX_CMD_LINES" 2>/tmp/archcheck.err)"
RC=$?
set -e

if [[ $RC -eq 2 ]]; then
  cat /tmp/archcheck.err >&2
  die "archcheck could not run (see above); this is NOT a passing result"
fi

if [[ -s /tmp/archcheck.err ]]; then
  while IFS= read -r line; do warn "$line"; done < /tmp/archcheck.err
fi

if [[ -n "$OUT" ]]; then
  while IFS=$'\t' read -r rule pkg detail; do
    [[ -z "$rule" ]] && continue
    fail "[$rule] $pkg: $detail"
  done <<< "$OUT"
else
  ok "R1 pkg/** is stdlib-only"
  ok "R2 internal/domain/** imports only stdlib, domain and pkg"
  ok "R3 internal/application/** reaches no infrastructure and no adapter but spi"
  ok "R4 internal/adapters/gateway/spi is a port declaration, not an adapter"
  ok "R5 validation and the workflow engine port are infrastructure-free"
  ok "R6 no package imports an external _test package"
  ok "H1/H2 cmd/** is composition only"
fi

summary "check-architecture"
