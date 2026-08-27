#!/usr/bin/env bash
#
# scripts/check-error-catalog.sh — the Go error registry and the published catalogue agree.
#
# WHAT IT ENFORCES
#   pkg/apierror's `registry` map and api/errors/catalog.yaml must describe the same error
#   model:
#     C1  same set of codes — nothing in the catalogue that the platform cannot raise, and
#         nothing raisable that is not published;
#     C2  same category for every code;
#     C3  same retryability for every code (after applying the catalogue's category
#         defaults, which a code inherits unless it overrides them);
#     C4  same HTTP status and gRPC code wherever the catalogue states one explicitly;
#     C5  every code's `go_const` names a constant that exists in pkg/apierror;
#     C6  every code matches ^[A-Z][A-Z0-9_]{2,63}$ and is unique.
#
# WHY
#   §20 makes `retryable` a machine-readable contract, not advice: client SDKs, the
#   workflow engine and the outbox relay branch on it. That means the field has two
#   readers who must never disagree — the Go code that sets it on a response, and the
#   catalogue that generated the SDK reading the response. A drift between them is not a
#   documentation bug; it is a retry-behaviour bug in code we do not own, and it surfaces
#   as either a duplicate charge (client retries something the platform considers final)
#   or a stuck payment (client gives up on something that was retryable).
#
#   The comparison runs against the LINKED Go registry (scripts/specdump errors) rather
#   than a parse of the source, so what is compared is what the binary will actually
#   return.
#
# USAGE
#   scripts/check-error-catalog.sh
#
# EXIT
#   0 clean · 1 divergence · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

need go
need python3
cd "$REPO_ROOT"

CATALOG="api/errors/catalog.yaml"
[[ -f "$CATALOG" ]] || die "missing $CATALOG"

hdr "error catalogue — pkg/apierror ↔ ${CATALOG}"

GO_JSON="$(mktemp)"; trap 'rm -f "$GO_JSON"' EXIT
go run ./scripts/specdump errors > "$GO_JSON" || die "could not dump the Go error registry"

REPORT="$(mktemp)"; trap 'rm -f "$GO_JSON" "$REPORT"' EXIT

set +e
python3 - "$GO_JSON" "$CATALOG" pkg/apierror/apierror.go > "$REPORT" <<'PY'
import json, re, sys

go_path, cat_path, src_path = sys.argv[1], sys.argv[2], sys.argv[3]

try:
    import yaml
except ImportError:
    print("SKIP\tPyYAML is not installed; cannot parse the catalogue")
    sys.exit(3)

go = json.load(open(go_path))
cat = yaml.safe_load(open(cat_path))
src = open(src_path).read()

# Category defaults. A code inherits every field it does not set itself — the catalogue
# says so in its own header, so the comparison must apply the same inheritance or it
# would report every unset field as a divergence.
GRPC_NAME_TO_NUM = {
    "OK": 0, "CANCELLED": 1, "UNKNOWN": 2, "INVALID_ARGUMENT": 3, "DEADLINE_EXCEEDED": 4,
    "NOT_FOUND": 5, "ALREADY_EXISTS": 6, "PERMISSION_DENIED": 7, "RESOURCE_EXHAUSTED": 8,
    "FAILED_PRECONDITION": 9, "ABORTED": 10, "OUT_OF_RANGE": 11, "UNIMPLEMENTED": 12,
    "INTERNAL": 13, "UNAVAILABLE": 14, "DATA_LOSS": 15, "UNAUTHENTICATED": 16,
}
defaults = {c["name"]: c for c in cat.get("categories", [])}

problems = []
def bad(kind, msg): problems.append((kind, msg))

CODE_RE = re.compile(r"^[A-Z][A-Z0-9_]{2,63}$")

# --- C6: shape and uniqueness -----------------------------------------------------------
seen = set()
entries = {}
for e in cat.get("codes", []):
    code = e.get("code")
    if not code:
        bad("C6", "a catalogue entry has no `code` field"); continue
    if not CODE_RE.match(code):
        bad("C6", f"{code}: does not match ^[A-Z][A-Z0-9_]{{2,63}}$")
    if code in seen:
        bad("C6", f"{code}: duplicated in the catalogue")
    seen.add(code)
    entries[code] = e
    if e.get("category") not in defaults:
        bad("C6", f"{code}: category {e.get('category')!r} is not declared in `categories`")

# --- C1: same code set ------------------------------------------------------------------
go_codes, cat_codes = set(go), set(entries)
for code in sorted(cat_codes - go_codes):
    bad("C1", f"{code}: in {cat_path} but NOT registered in pkg/apierror "
              f"(published to clients, unraisable by the platform)")
for code in sorted(go_codes - cat_codes):
    bad("C1", f"{code}: registered in pkg/apierror but NOT in {cat_path} "
              f"(raisable by the platform, undocumented for clients)")

# --- C2/C3/C4: field agreement ----------------------------------------------------------
for code in sorted(go_codes & cat_codes):
    g, e = go[code], entries[code]
    cat_cat = e.get("category")
    d = defaults.get(cat_cat, {})

    if g["category"] != cat_cat:
        bad("C2", f"{code}: category go={g['category']} catalog={cat_cat}")

    want_retry = e.get("retryable", d.get("retryable"))
    if want_retry is None:
        bad("C3", f"{code}: retryable is unset in the catalogue and its category "
                  f"{cat_cat} declares no default")
    elif bool(want_retry) != bool(g["retryable"]):
        note = "" if "retryable" in e else " (inherited from the category default)"
        bad("C3", f"{code}: retryable go={g['retryable']} catalog={bool(want_retry)}{note} "
                  f"— clients and the workflow engine branch on this")

    if "http_status" in e and int(e["http_status"]) != int(g["httpStatus"]):
        bad("C4", f"{code}: http_status go={g['httpStatus']} catalog={e['http_status']}")

    if "grpc_code" in e:
        want = e["grpc_code"]
        want_num = GRPC_NAME_TO_NUM.get(want) if isinstance(want, str) else int(want)
        if want_num is None:
            bad("C4", f"{code}: grpc_code {want!r} is not a canonical gRPC code name")
        elif want_num != int(g["grpcCode"]):
            bad("C4", f"{code}: grpc_code go={g['grpcCode']} catalog={want}({want_num})")

# --- C5: go_const resolves --------------------------------------------------------------
consts = set(re.findall(r"^\s*(Code[A-Za-z0-9_]*)\s+Code\s*=", src, re.M))
for code in sorted(cat_codes):
    gc = entries[code].get("go_const")
    if not gc:
        bad("C5", f"{code}: no `go_const` — the code generator has nothing to emit")
    elif gc not in consts:
        bad("C5", f"{code}: go_const {gc} does not exist in pkg/apierror "
                  f"(generator target is dangling)")

for kind, msg in problems:
    print(f"{kind}\t{msg}")

print(f"COUNT\tgo={len(go_codes)} catalog={len(cat_codes)} shared={len(go_codes & cat_codes)}",
      file=sys.stderr)
sys.exit(1 if problems else 0)
PY
RC=$?
set -e

case $RC in
  0)
    ok "C1 same code set"
    ok "C2 same category for every code"
    ok "C3 same retryability for every code"
    ok "C4 HTTP status and gRPC code agree wherever the catalogue states them"
    ok "C5 every go_const resolves in pkg/apierror"
    ok "C6 every code is unique and well-formed"
    ;;
  3)
    skip "PyYAML unavailable — catalogue not parsed"
    ;;
  1)
    while IFS=$'\t' read -r kind msg; do
      case "$kind" in
        SKIP) skip "$msg" ;;
        ""|COUNT) : ;;
        *) fail "[$kind] $msg" ;;
      esac
    done < "$REPORT"
    ;;
  *)
    die "the comparison itself failed (exit $RC)"
    ;;
esac

summary "check-error-catalog"
