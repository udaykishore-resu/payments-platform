#!/usr/bin/env bash
#
# scripts/traceability.sh — regenerate docs/spec/09-traceability.md and fail on an orphan.
#
# WHAT IT ENFORCES
#   Baseline §26: every requirement is traced to design, code and test, and CI fails on
#   an orphan requirement (no test) or an orphan test (no requirement).
#
#     T1  every BR-/FR-/NFR- defined by a heading in docs/spec/01,02,03 is referenced by
#         at least one test — a test function name, a `// Requirement: FR-nn` comment, or
#         a `requirements:` key in tests/critical_paths.yaml;
#     T2  every requirement is referenced by at least one non-test source file or design
#         document section, so "which code implements this" has an answer;
#     T3  every requirement reference found anywhere resolves to a defined requirement —
#         a `FR-92` in a test comment when the spec stops at FR-91 is a typo that silently
#         un-traces a requirement;
#     T4  the generated matrix is byte-identical to the committed one (drift check).
#
# WHY
#   Traceability is usually theatre: a matrix maintained by hand, generated once, and
#   wrong within a month. The only version worth having is one that is derived from the
#   artefacts, regenerated on every run, and *blocking* — which is why T4 exists. A matrix
#   that CI regenerates but does not diff is a matrix nobody has read since it was written.
#
#   T3 is the one that earns its keep in practice. An orphan requirement is visible (the
#   matrix has a blank cell); a reference to a requirement ID that does not exist is
#   invisible, and it means the requirement everyone believes is covered has no test.
#
# WHY IT IS TOLERANT ABOUT *WHERE* A REFERENCE LIVES
#   A requirement can be discharged by a test name (`TestFR12_IdempotentReplay`), by a
#   comment in the test, or by an entry in tests/critical_paths.yaml. Insisting on one
#   convention would be enforcing a naming style rather than the property; accepting all
#   three keeps the mechanism about coverage.
#
# USAGE
#   scripts/traceability.sh [--check] [--out PATH]
#
#     --check  do not write; fail if the committed matrix differs from the generated one.
#              This is what CI runs. Without it, the file is rewritten in place.
#
# EXIT
#   0 every requirement traced and the matrix is current · 1 an orphan or drift · 2 could
#   not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

OUT="docs/spec/09-traceability.md"
CHECK_ONLY=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --check)   CHECK_ONLY=1; shift ;;
    --out)     OUT="$2"; shift 2 ;;
    -h|--help) sed -n '2,42p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)         die "unknown flag: $1" ;;
  esac
done

need python3
cd "$REPO_ROOT"

hdr "traceability — baseline §26"

GENERATED="$(mktemp)"; REPORT="$(mktemp)"
trap 'rm -f "$GENERATED" "$REPORT"' EXIT

set +e
python3 - "$GENERATED" "$OUT" > "$REPORT" <<'PY'
import glob, os, re, sys, datetime

out_tmp, committed_path = sys.argv[1], sys.argv[2]

problems = []
def bad(kind, msg): problems.append((kind, msg))

REQ = re.compile(r"\b(BR|FR|NFR)-(\d{1,3})\b")
HEADING = re.compile(r"^#{2,4}\s+((?:BR|FR|NFR)-\d{1,3})\s*[—:-]\s*(.+?)\s*$", re.M)

SPEC_FILES = {
    "BR": "docs/spec/01-business-requirements.md",
    "FR": "docs/spec/02-functional-requirements.md",
    "NFR": "docs/spec/03-non-functional-requirements.md",
}

# --- 1. the definitions -----------------------------------------------------------------
defined = {}          # id -> (title, source file)
for kind, path in SPEC_FILES.items():
    if not os.path.exists(path):
        bad("T0", f"missing specification document {path}")
        continue
    text = open(path, encoding="utf-8").read()
    for rid, title in HEADING.findall(text):
        if rid in defined:
            bad("T0", f"{rid} is defined twice ({defined[rid][1]} and {path})")
        defined[rid] = (title.strip(), path)

if not defined:
    print("T0\tno requirement headings found; the matrix would be empty")
    sys.exit(1)

# --- 2. where each one is referenced ------------------------------------------------------
tests = {}      # id -> set of "file::TestName" or "file:line"
code = {}       # id -> set of files
design = {}     # id -> set of "doc §section"

def note(store, rid, where):
    store.setdefault(rid, set()).add(where)

SKIP_DIRS = {".git", "vendor", "node_modules", "testdata", ".terraform"}

# Go sources. A test file's references count as test coverage; a non-test file's count as
# implementation. The distinction is what separates "we wrote code for it" from "we proved
# it works", and §26 requires both.
TESTFUNC = re.compile(r"^func\s+(Test[A-Za-z0-9_]*)\s*\(", re.M)

for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
    for fn in files:
        path = os.path.join(root, fn)
        if path.startswith("./"):
            path = path[2:]
        if not fn.endswith(".go"):
            continue
        try:
            src = open(path, encoding="utf-8").read()
        except OSError:
            continue

        is_test = fn.endswith("_test.go")
        if is_test:
            # Attribute a reference to the enclosing test function where one can be found,
            # so the matrix names the test rather than the file.
            spans = [(m.start(), m.group(1)) for m in TESTFUNC.finditer(src)]
            for m in REQ.finditer(src):
                rid = m.group(0)
                owner = None
                for start, name in spans:
                    if start <= m.start():
                        owner = name
                    else:
                        break
                note(tests, rid, f"{path}::{owner}" if owner else path)
        else:
            for m in REQ.finditer(src):
                note(code, m.group(0), path)

# The design documents. A requirement traces to ≥ 1 design section; the plane docs and the
# baseline are where those live.
for path in sorted(glob.glob("docs/*.md")) + sorted(glob.glob("docs/spec/0[04-9]*.md")) \
        + sorted(glob.glob("docs/adr/*.md")):
    if os.path.basename(path) == os.path.basename(committed_path):
        continue     # the matrix references every requirement by construction
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        continue
    for m in REQ.finditer(text):
        note(design, m.group(0), os.path.basename(path))

# tests/critical_paths.yaml, where a critical path may declare the requirements it covers.
CP = "tests/critical_paths.yaml"
if os.path.exists(CP):
    try:
        import yaml
        cps = yaml.safe_load(open(CP, encoding="utf-8")) or []
        for entry in cps if isinstance(cps, list) else []:
            ids = entry.get("requirements") or []
            for rid in ids:
                for t in entry.get("tests") or []:
                    note(tests, str(rid), f"{CP}:{entry.get('id','?')} → {t}")
    except Exception as e:                                    # noqa: BLE001
        bad("T0", f"{CP} could not be parsed: {e}")

# k6 scenarios carry NFR references for the §18 targets they assert.
for path in sorted(glob.glob("tests/load/*.js")):
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        continue
    for m in REQ.finditer(text):
        note(tests, m.group(0), path)

# --- 3. the checks ----------------------------------------------------------------------
seen_anywhere = set(tests) | set(code) | set(design)

# T3 — a reference to something that does not exist.
for rid in sorted(seen_anywhere - set(defined)):
    where = sorted(tests.get(rid, set()) | code.get(rid, set()) | design.get(rid, set()))[:3]
    bad("T3", f"{rid} is referenced ({', '.join(where)}) but no such requirement is "
              f"defined — a typo here silently un-traces the requirement it meant")

# T1 — orphan requirement (no test).
for rid in sorted(defined):
    if not tests.get(rid):
        bad("T1", f"{rid} ({defined[rid][0]}) has no test — §26 requires ≥ 1 test per "
                  f"requirement. Name a test after it, add a `// {rid}` comment to the "
                  f"test that covers it, or list it under `requirements:` in "
                  f"tests/critical_paths.yaml")

# T2 — no implementing package.
for rid in sorted(defined):
    if not code.get(rid) and not design.get(rid):
        bad("T2", f"{rid} ({defined[rid][0]}) is referenced by no source file and no "
                  f"design document — 'which code implements this' has no answer")

# --- 4. render ----------------------------------------------------------------------------
def order_key(rid):
    kind, num = rid.split("-")
    return ({"BR": 0, "FR": 1, "NFR": 2}[kind], int(num))

def cell(items, limit=4):
    if not items:
        return "**—**"
    xs = sorted(items)
    shown = ", ".join(f"`{x}`" for x in xs[:limit])
    if len(xs) > limit:
        shown += f", +{len(xs) - limit} more"
    return shown

lines = []
lines.append("# 09 — Traceability matrix")
lines.append("")
lines.append("> **Generated file — do not edit.** Regenerate with `scripts/traceability.sh`.")
lines.append("> CI runs `scripts/traceability.sh --check` and fails on drift or on an orphan")
lines.append("> requirement (baseline §26).")
lines.append("")
lines.append("Every requirement defined by a heading in `01-business-requirements.md`,")
lines.append("`02-functional-requirements.md` and `03-non-functional-requirements.md` appears")
lines.append("below with the design documents that describe it, the packages that implement it")
lines.append("and the tests that prove it. A `—` in the **Tests** column fails the build.")
lines.append("")
lines.append("| Requirement | Title | Design | Code | Tests |")
lines.append("|---|---|---|---|---|")
for rid in sorted(defined, key=order_key):
    title = defined[rid][0].replace("|", "\\|")
    lines.append(f"| `{rid}` | {title} | {cell(design.get(rid, set()))} "
                 f"| {cell(code.get(rid, set()))} | {cell(tests.get(rid, set()))} |")

traced = sum(1 for r in defined if tests.get(r))
lines.append("")
lines.append("## Coverage summary")
lines.append("")
lines.append("| Class | Defined | With a test | With code or design | Orphans |")
lines.append("|---|---|---|---|---|")
for kind in ("BR", "FR", "NFR"):
    ids = [r for r in defined if r.startswith(kind + "-")]
    with_test = sum(1 for r in ids if tests.get(r))
    with_impl = sum(1 for r in ids if code.get(r) or design.get(r))
    lines.append(f"| {kind} | {len(ids)} | {with_test} | {with_impl} | {len(ids) - with_test} |")
lines.append(f"| **Total** | **{len(defined)}** | **{traced}** | "
             f"**{sum(1 for r in defined if code.get(r) or design.get(r))}** | "
             f"**{len(defined) - traced}**  |")
lines.append("")
lines.append("<!-- generated by scripts/traceability.sh; the date is deliberately omitted so")
lines.append("     that a regeneration with no substantive change produces no diff -->")
lines.append("")

open(out_tmp, "w", encoding="utf-8").write("\n".join(lines))

for kind, msg in problems:
    print(f"{kind}\t{msg}")

print(f"COUNT\tdefined={len(defined)} tested={traced} orphans={len(defined) - traced}",
      file=sys.stderr)
sys.exit(1 if problems else 0)
PY
RC=$?
set -e
[[ $RC -gt 1 ]] && die "the traceability generator failed (exit $RC)"

# --- write or diff ----------------------------------------------------------------------------
if [[ $CHECK_ONLY -eq 1 ]]; then
  if [[ ! -f "$OUT" ]]; then
    fail "[T4] $OUT does not exist; run scripts/traceability.sh to generate it"
  elif ! diff -q "$OUT" "$GENERATED" >/dev/null 2>&1; then
    fail "[T4] $OUT is out of date — regenerate it with scripts/traceability.sh"
    diff -u "$OUT" "$GENERATED" | head -40 >&2 || true
  else
    ok "T4 the committed matrix matches the generated one"
  fi
else
  mkdir -p "$(dirname "$OUT")"
  if [[ -f "$OUT" ]] && diff -q "$OUT" "$GENERATED" >/dev/null 2>&1; then
    ok "$OUT is already current"
  else
    cp "$GENERATED" "$OUT"
    ok "wrote $OUT"
  fi
fi

while IFS=$'\t' read -r kind msg; do
  case "$kind" in
    ""|COUNT) : ;;
    *)        fail "[$kind] $msg" ;;
  esac
done < "$REPORT"

if (( FAILURES == 0 )); then
  ok "T1 every requirement has a test"
  ok "T2 every requirement has code or a design section"
  ok "T3 every reference resolves to a defined requirement"
fi

summary "traceability"
