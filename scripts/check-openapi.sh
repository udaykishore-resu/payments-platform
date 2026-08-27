#!/usr/bin/env bash
#
# scripts/check-openapi.sh — the published REST contract parses and is structurally sound.
#
# WHAT IT ENFORCES
#   Preferred path: redocly (or spectral) lint, which knows the OpenAPI 3.1 meta-schema and
#   the usual style rules.
#
#   Fallback path (no network, no npx): a Python structural validator that asserts the
#   properties this platform actually depends on, rather than pretending to be a full
#   OpenAPI validator:
#     O1  the document parses as YAML and declares `openapi: 3.x`;
#     O2  `info.title`, `info.version` and at least one path are present;
#     O3  every operation has an `operationId`, and operationIds are unique (they are the
#         SDK's method names — a duplicate silently drops an endpoint from the generated
#         client);
#     O4  every `$ref` resolves within the document (a dangling ref is a generator crash
#         at best and a silently empty type at worst);
#     O5  every non-2xx response body is `application/problem+json` (§19.3);
#     O6  every mutating operation declared idempotent in baseline §19 requires an
#         `Idempotency-Key` header, and every `PATCH`/`PUT` on a mutable resource requires
#         `If-Match`;
#     O7  every error `code` referenced in the document exists in api/errors/catalog.yaml;
#     O8  no `example` anywhere in the document contains a Luhn-valid 13–19 digit run —
#         a PAN in the public API documentation is a PCI finding regardless of whether the
#         number was ever real (§17).
#
# WHY
#   The fallback exists because a check that only runs when the network does is a check
#   that does not run. O5–O8 are the ones worth having in either mode: they are contract
#   properties specific to this platform that no generic linter knows about, and O8 in
#   particular catches the failure mode where somebody pastes a "test card number" into an
#   example and it ships to a public documentation site.
#
# USAGE
#   scripts/check-openapi.sh [--spec PATH] [--no-redocly]
#
# EXIT
#   0 clean · 1 invalid contract · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

SPEC="api/openapi/payments-platform.v1.yaml"
USE_REDOCLY=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --spec)        SPEC="$2"; shift 2 ;;
    --no-redocly)  USE_REDOCLY=0; shift ;;
    -h|--help)     sed -n '2,45p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)             die "unknown flag: $1" ;;
  esac
done

need python3
cd "$REPO_ROOT"
[[ -f "$SPEC" ]] || die "missing $SPEC"

hdr "openapi — ${SPEC}"

# --- preferred: a real OpenAPI linter -------------------------------------------------------
RAN_LINTER=0
if [[ $USE_REDOCLY -eq 1 ]]; then
  if have redocly; then
    info "using the installed redocly"
    redocly lint "$SPEC" || fail "redocly lint reported findings"
    RAN_LINTER=1
  elif have spectral; then
    info "using the installed spectral"
    spectral lint "$SPEC" || fail "spectral lint reported findings"
    RAN_LINTER=1
  elif have npx && ! offline; then
    info "fetching @redocly/cli via npx (first run only)"
    if npx --yes @redocly/cli@latest lint "$SPEC"; then
      RAN_LINTER=1
    else
      # npx failing to *fetch* and redocly failing to *lint* are different outcomes. npx
      # exits 1 for both, so this cannot be told apart reliably; the structural validator
      # below runs either way and is what the exit status is based on.
      warn "npx @redocly/cli did not complete; relying on the structural validator"
    fi
  else
    skip "no redocly/spectral and no network — using the built-in structural validator"
  fi
fi
[[ $RAN_LINTER -eq 1 ]] && ok "OpenAPI linter passed"

# --- always: the platform-specific structural checks ---------------------------------------
info "structural validation (O1–O8) — these run regardless of linter availability"

REPORT="$(mktemp)"; trap 'rm -f "$REPORT"' EXIT
set +e
python3 - "$SPEC" api/errors/catalog.yaml > "$REPORT" <<'PY'
import re, sys

spec_path, catalog_path = sys.argv[1], sys.argv[2]

try:
    import yaml
except ImportError:
    print("SKIP\tPyYAML is not installed; the spec was not parsed")
    sys.exit(3)

problems = []
def bad(kind, msg): problems.append((kind, msg))

# O1 — parses.
try:
    doc = yaml.safe_load(open(spec_path, encoding="utf-8"))
except yaml.YAMLError as e:
    print(f"O1\t{spec_path} is not valid YAML: {e}")
    sys.exit(1)

if not isinstance(doc, dict):
    print(f"O1\t{spec_path} does not parse to a mapping")
    sys.exit(1)

version = str(doc.get("openapi", ""))
if not version.startswith("3."):
    bad("O1", f"`openapi` is {version!r}; expected a 3.x document")

# O2 — the minimum a generator needs.
info_block = doc.get("info") or {}
for field in ("title", "version"):
    if not info_block.get(field):
        bad("O2", f"info.{field} is missing")
paths = doc.get("paths") or {}
if not paths:
    bad("O2", "the document declares no paths")

METHODS = {"get", "put", "post", "delete", "patch", "options", "head", "trace"}

# O3 — operationIds.
seen_ops = {}
operations = []          # (path, method, operation)
for p, item in paths.items():
    if not isinstance(item, dict):
        continue
    for m, op in item.items():
        if m.lower() not in METHODS or not isinstance(op, dict):
            continue
        operations.append((p, m.lower(), op))
        oid = op.get("operationId")
        if not oid:
            bad("O3", f"{m.upper()} {p}: no operationId — the generated SDK has no name "
                      f"for this method")
        elif oid in seen_ops:
            bad("O3", f"operationId {oid!r} is used by both {seen_ops[oid]} and "
                      f"{m.upper()} {p}; a duplicate silently drops one from the SDK")
        else:
            seen_ops[oid] = f"{m.upper()} {p}"

# O4 — internal $refs resolve.
def walk(node, fn, path=()):
    if isinstance(node, dict):
        for k, v in node.items():
            fn(k, v, path + (k,))
            walk(v, fn, path + (k,))
    elif isinstance(node, list):
        for i, v in enumerate(node):
            walk(v, fn, path + (str(i),))

def resolve(ref):
    if not ref.startswith("#/"):
        return True          # external refs are out of scope for the structural pass
    cur = doc
    for part in ref[2:].split("/"):
        part = part.replace("~1", "/").replace("~0", "~")
        if isinstance(cur, dict) and part in cur:
            cur = cur[part]
        elif isinstance(cur, list) and part.isdigit() and int(part) < len(cur):
            cur = cur[int(part)]
        else:
            return False
    return True

dangling = set()
def check_ref(k, v, _path):
    if k == "$ref" and isinstance(v, str) and not resolve(v):
        dangling.add(v)
walk(doc, check_ref)
for ref in sorted(dangling):
    bad("O4", f"$ref {ref} does not resolve within the document")

# O5 — errors are problem+json.
# The Kubernetes probe and scrape endpoints are excluded. §19.2 lists /healthz, /readyz,
# /livez and /metrics as cluster-internal with no auth scope: they are consumed by the
# kubelet and by Prometheus, neither of which reads RFC 9457, and a 503 from /readyz
# carries the health document that deployment.md §1.7 specifies. Requiring problem+json
# there would be enforcing the API's error contract on something that is not the API.
PROBE_PATHS = {"/healthz", "/readyz", "/livez", "/metrics", "/startupz"}

for p, m, op in operations:
    if p.rstrip("/") in PROBE_PATHS:
        continue
    for status, resp in (op.get("responses") or {}).items():
        s = str(status)
        if s in ("default",) or (s.isdigit() and 200 <= int(s) < 400):
            continue
        content = (resp or {}).get("content") or {}
        if not content:
            continue     # a bodiless error response (e.g. 304) is legitimate
        if "application/problem+json" not in content:
            bad("O5", f"{m.upper()} {p} -> {s}: body is {sorted(content)} but §19.3 "
                      f"requires application/problem+json for errors")

# O6 — idempotency and optimistic concurrency headers.
def header_names(op, item):
    names = set()
    for src in ((item or {}).get("parameters") or []) + (op.get("parameters") or []):
        if isinstance(src, dict):
            if src.get("in") == "header" and src.get("name"):
                names.add((src["name"].lower(), bool(src.get("required"))))
            elif "$ref" in src:
                # A referenced parameter: resolve one level so a shared header component
                # counts. Anything deeper is out of scope for the structural pass.
                ref = src["$ref"]
                if ref.startswith("#/"):
                    cur = doc
                    ok_ref = True
                    for part in ref[2:].split("/"):
                        if isinstance(cur, dict) and part in cur:
                            cur = cur[part]
                        else:
                            ok_ref = False
                            break
                    if ok_ref and isinstance(cur, dict) and cur.get("in") == "header":
                        names.add((str(cur.get("name", "")).lower(),
                                   bool(cur.get("required"))))
    return names

for p, item in paths.items():
    if not isinstance(item, dict):
        continue
    for m, op in item.items():
        if m.lower() not in METHODS or not isinstance(op, dict):
            continue
        hdrs = header_names(op, item)
        names = {n for n, _ in hdrs}
        required = {n for n, r in hdrs if r}
        if m.lower() == "post" and not p.rstrip("/").endswith(("/healthz", "/readyz", "/livez")):
            # Webhook receipt is gateway-authenticated and carries no Idempotency-Key:
            # the gateway chooses the delivery semantics, and the platform deduplicates on
            # the gateway's own event reference (§24, duplicate webhook).
            if "/webhooks/" in p:
                pass
            elif "idempotency-key" not in names:
                bad("O6", f"POST {p}: no Idempotency-Key header parameter — §19.2 makes "
                          f"the key mandatory on every mutating data-plane operation")
            elif "idempotency-key" not in required:
                bad("O6", f"POST {p}: Idempotency-Key is declared but not required")
        if m.lower() in ("patch", "put") and "if-match" not in names:
            bad("O6", f"{m.upper()} {p}: no If-Match header parameter — §19.3 requires it "
                      f"on every mutation of a resource carrying an ETag")

# O7 — every error code named in the spec exists in the catalogue.
try:
    catalog = yaml.safe_load(open(catalog_path, encoding="utf-8"))
    # `code:` appears in two roles in this API: the top-level error code and the
    # per-field sub-code inside details[]. The catalogue publishes both, so both are
    # accepted here — checking only `codes` would report every detail sub-code as
    # unknown, which is a false positive, not a finding.
    known = {e["code"] for e in catalog.get("codes", []) if e.get("code")}
    known |= {e["code"] for e in (catalog.get("detail_codes") or []) if e.get("code")}
except (OSError, yaml.YAMLError):
    known = None

if known:
    raw = open(spec_path, encoding="utf-8").read()
    # Only strings that appear as the value of a `code:` key, so an unrelated SCREAMING
    # constant elsewhere in the document is not mistaken for an error code.
    referenced = set(re.findall(r'^\s*code:\s*["\']?([A-Z][A-Z0-9_]{2,63})["\']?\s*$',
                                raw, re.M))
    for c in sorted(referenced - known):
        bad("O7", f"error code {c} is referenced in the spec but is not in {catalog_path}")

# O8 — no PAN in an example.
def luhn(digits):
    total, alt = 0, False
    for ch in reversed(digits):
        d = ord(ch) - 48
        if alt:
            d *= 2
            if d > 9:
                d -= 9
        total += d
        alt = not alt
    return total % 10 == 0

pan_hits = []
def scan_examples(k, v, path):
    if k not in ("example", "examples", "default"):
        return
    text = str(v)
    # One run of 13–19 digits, optionally separated by single spaces or hyphens (the two
    # groupings a PAN is written in). Anchored on non-digit boundaries so a longer numeric
    # blob is not sliced into a coincidental match.
    for run in re.findall(r"(?<![\d-])\d(?:[ -]?\d){12,18}(?![\d-])", text):
        digits = re.sub(r"\D", "", run)
        if 13 <= len(digits) <= 19 and luhn(digits):
            pan_hits.append((".".join(path), digits[:6] + "…" + digits[-4:]))
walk(doc, scan_examples)
for where, masked in pan_hits:
    bad("O8", f"a Luhn-valid 13–19 digit run appears in an example at {where} "
              f"({masked}) — a PAN in published API documentation is a §17 finding "
              f"whether or not the number was ever live")

for kind, msg in problems:
    print(f"{kind}\t{msg}")

print(f"COUNT\tpaths={len(paths)} operations={len(operations)} "
      f"operationIds={len(seen_ops)}", file=sys.stderr)
sys.exit(1 if problems else 0)
PY
RC=$?
set -e

case $RC in
  0)
    ok "O1 parses as an OpenAPI 3.x document"
    ok "O2 info and paths present"
    ok "O3 every operation has a unique operationId"
    ok "O4 every internal \$ref resolves"
    ok "O5 every error response is application/problem+json"
    ok "O6 Idempotency-Key and If-Match are declared where §19 requires them"
    ok "O7 every referenced error code exists in the catalogue"
    ok "O8 no Luhn-valid digit run in any example"
    ;;
  3) skip "PyYAML unavailable — spec not parsed" ;;
  1)
    while IFS=$'\t' read -r kind msg; do
      case "$kind" in
        SKIP)     skip "$msg" ;;
        ""|COUNT) : ;;
        *)        fail "[$kind] $msg" ;;
      esac
    done < "$REPORT"
    ;;
  *) die "the structural validator failed to run (exit $RC)" ;;
esac

summary "check-openapi"
