#!/usr/bin/env bash
#
# scripts/check-rules-documented.sh — every validation rule has a documentation anchor.
#
# WHAT IT ENFORCES
#   D1  every RuleID in the linked validation registry appears in docs/validation-plane.md;
#   D2  every rule-shaped ID mentioned in docs/validation-plane.md is one the platform can
#       actually produce — a registered rule, or an ID emitted as a literal from production
#       source (the "vice versa" — a documented rule that no longer exists sends a support
#       engineer looking for behaviour the platform does not have);
#   D3  every rule ID emitted from non-test Go source as a literal — the values that reach
#       a caller in `apierror.Detail.RuleID` — appears in docs/validation-plane.md;
#   D4  every rule ID is well-formed: ^L[1-7]\.[A-Z][A-Z0-9_]{2,}$ (conventions §Naming);
#   D5  every ERROR-severity registered rule carries a remediation string.
#
# WHY
#   Baseline §21 makes the rule ID a *published* identifier: it travels to the caller in
#   the `ruleId` field of a problem document, and §20's whole argument for a structured
#   error model is that a rejection a merchant's engineer cannot act on is a support
#   ticket the platform chose to receive. An ID with no documentation anchor is exactly
#   that ticket. D2 is the other half: documentation describing a rule that was deleted is
#   worse than no documentation, because it is believed.
#
#   D3 exists because the registry is not the only source of rule IDs. Aggregate methods
#   annotate invariant failures with IDs directly (`apierror.Detail{RuleID: "L7.…"}`)
#   without registering an engine.Rule — a legitimate pattern, since a domain invariant is
#   not a pluggable rule — but the *caller* cannot tell the difference. Both arrive in the
#   same field of the same response, so both need the same anchor.
#
#   The registry is read by LINKING internal/validation/rules (via scripts/specdump), not
#   by grepping, because the registry is populated by blank imports: a grep would check
#   the rules someone remembered to write in a greppable way.
#
# USAGE
#   scripts/check-rules-documented.sh [--doc PATH] [--no-source-scan]
#
#   --no-source-scan disables D3's report only. It exists so that a run can isolate D1/D2
#   while a D3 backlog is being worked through; it is NOT set in CI. The scan itself still
#   runs, because D2 needs its result to know which IDs exist.
#
# EXIT
#   0 clean · 1 undocumented or orphaned rule · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

DOC="docs/validation-plane.md"
SOURCE_SCAN=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --doc)            DOC="$2"; shift 2 ;;
    --no-source-scan) SOURCE_SCAN=0; shift ;;
    -h|--help)        sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)                die "unknown flag: $1" ;;
  esac
done

need go
need python3
cd "$REPO_ROOT"
[[ -f "$DOC" ]] || die "missing $DOC"

hdr "validation rules — registry ↔ ${DOC}"

REG_JSON="$(mktemp)"; REPORT="$(mktemp)"
trap 'rm -f "$REG_JSON" "$REPORT"' EXIT
go run ./scripts/specdump rules > "$REG_JSON" || die "could not dump the validation registry"

set +e
python3 - "$REG_JSON" "$DOC" "$SOURCE_SCAN" > "$REPORT" <<'PY'
import json, os, re, sys

reg_path, doc_path, source_scan = sys.argv[1], sys.argv[2], sys.argv[3] == "1"

RULE_RE = re.compile(r"\bL[1-7]\.[A-Z][A-Z0-9_]{2,}\b")
WELL_FORMED = re.compile(r"^L[1-7]\.[A-Z][A-Z0-9_]{2,}$")

reg = json.load(open(reg_path))
reg_ids = {r["id"] for r in reg}
doc_text = open(doc_path, encoding="utf-8").read()
doc_ids = set(RULE_RE.findall(doc_text))

problems = []
def bad(kind, msg): problems.append((kind, msg))

# D4 — shape.
for r in reg:
    if not WELL_FORMED.match(r["id"]):
        bad("D4", f"{r['id']}: malformed rule ID (want L<1-7>.<SCREAMING_SNAKE>)")
    if r["level"] == 0:
        bad("D4", f"{r['id']}: level does not parse out of the ID")

# D5 — an ERROR rule without remediation.
for r in reg:
    if r["severity"] == "ERROR" and not r["remediation"].strip():
        bad("D5", f"{r['id']}: ERROR severity with no remediation — the caller is told "
                  f"'no' with no way to act")

# The source scan. It is run unconditionally, because BOTH D2 and D3 need it: D3 reports
# what it finds, and D2 needs it to know which IDs exist. --no-source-scan silences D3's
# report only, which is what its documented contract says it does.
LIT = re.compile(r'"(L[1-7]\.[A-Z][A-Z0-9_]{2,})"')
used = {}
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs
               if d not in (".git", "vendor", "node_modules", "scripts", "testdata")]
    for f in files:
        if not f.endswith(".go") or f.endswith("_test.go"):
            continue
        p = os.path.join(root, f)
        try:
            src = open(p, encoding="utf-8").read()
        except OSError:
            continue
        for m in LIT.finditer(src):
            used.setdefault(m.group(1), p)

# An ID that production source emits exists just as surely as a registered one does — it is
# the same field of the same response to the same caller, which is the whole premise of D3.
# So "exists" for D2's purposes is the registry PLUS the emitted set, minus the composed
# prefixes that are not IDs at all.
emitted = {rid for rid in used if not rid.endswith("_")}
exists = reg_ids | emitted

# D1 — registered but undocumented.
for rid in sorted(reg_ids - doc_ids):
    bad("D1", f"{rid}: registered but absent from {doc_path}")

# D2 — documented but neither registered nor emitted.
#
# The scope is unchanged in the only sense that matters: an ID documented here that nothing
# in the platform can produce still fails, which is the failure D2 was written to catch ("a
# rule deleted from code but left documented as live" — docs/validation-plane.md §4.2). What
# it no longer does is fail on the domain-emitted IDs that D3 *requires* be documented; the
# two rules previously demanded opposite things of the same identifier, and the file could
# not be made to satisfy both. §4.2 scopes this assertion to the §3 rule catalog, and §6 —
# where the domain-emitted IDs live — is deliberately not that catalog.
for rid in sorted(doc_ids - exists):
    bad("D2", f"{rid}: documented in {doc_path} but neither registered nor emitted from "
              f"production source (describes behaviour the platform does not have)")

# D3 — literals emitted from production source.
if source_scan:
    for rid in sorted(used):
        # A literal ending in "_" is a prefix that the code concatenates a suffix onto
        # (e.g. "L4.TENANT_QUOTA_" + quotaName). It is not itself an ID, so reporting it
        # would be a false positive; it is noted so the reader knows it was considered.
        if rid.endswith("_"):
            print(f"NOTE\t{rid} ({used[rid]}) looks like a runtime-composed ID prefix; skipped")
            continue
        if rid in doc_ids:
            continue
        where = "also unregistered" if rid not in reg_ids else "registered"
        bad("D3", f"{rid}: emitted to callers from {used[rid]} but absent from "
                  f"{doc_path} ({where}) — the `ruleId` in the response resolves to nothing")

for kind, msg in problems:
    print(f"{kind}\t{msg}")

print(f"COUNT\tregistry={len(reg_ids)} documented={len(doc_ids)} "
      f"overlap={len(reg_ids & doc_ids)}", file=sys.stderr)
sys.exit(1 if problems else 0)
PY
RC=$?
set -e

case $RC in
  0)
    ok "D1 every registered rule is documented"
    ok "D2 every documented rule ID is one the platform can raise"
    [[ $SOURCE_SCAN -eq 1 ]] && ok "D3 every rule ID emitted to callers has a doc anchor"
    ok "D4 every rule ID is well-formed"
    ok "D5 every ERROR rule carries a remediation"
    ;;
  1)
    while IFS=$'\t' read -r kind msg; do
      case "$kind" in
        NOTE)     info "$msg" ;;
        ""|COUNT) : ;;
        *)        fail "[$kind] $msg" ;;
      esac
    done < "$REPORT"
    ;;
  *)
    die "the comparison itself failed (exit $RC)"
    ;;
esac

summary "check-rules-documented"
