#!/usr/bin/env bash
#
# scripts/check-runbook-links.sh — every runbook reference resolves to a file.
#
# WHAT IT ENFORCES
#   R1  Every `runbook_url` annotation in deployments/** and deploy/** points at a file that
#       exists under docs/runbooks/.
#   R2  Every `docs/runbooks/<name>` reference anywhere in the tree points at a file that
#       exists. Glob references (`security-*.md`) are resolved as globs and must match at
#       least one file.
#   R3  Every alert rule with `page: "true"` carries a `runbook_url` annotation.
#   R4  Every runbook under docs/runbooks/ is referenced by something, or is listed in
#       docs/runbooks/README.md. An unreferenced runbook is one nobody will find.
#   R5  Every relative link between runbooks resolves.
#
# WHY
#   An alert whose runbook 404s has spent the cost of a page and bought nothing. The rule in
#   docs/runbooks/README.md is that an alert without a runbook is not allowed to page; this
#   script is what makes that a property rather than an intention.
#
#   Extension-less URLs (https://docs.example.com/runbooks/payment-api-latency) resolve to
#   `<name>.md`, because a published docs site strips the extension. Both forms are accepted
#   so the check does not force a cosmetic rewrite of every annotation.
#
# USAGE
#   scripts/check-runbook-links.sh [--json]
#
# EXIT
#   0 every reference resolves · 1 one or more dangling · 2 could not run.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

AS_JSON=0
[[ "${1:-}" == "--json" ]] && AS_JSON=1

RB_DIR="docs/runbooks"
[[ -d "$RB_DIR" ]] || { echo "missing $RB_DIR" >&2; exit 2; }

command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }

AS_JSON="$AS_JSON" RB_DIR="$RB_DIR" python3 - <<'PY'
import glob, json, os, re, sys

rb_dir   = os.environ["RB_DIR"]
as_json  = os.environ["AS_JSON"] == "1"
findings = []
checked  = 0

def add(rule, where, ref, msg):
    findings.append({"rule": rule, "file": where, "reference": ref, "message": msg})

# ---- collect the files to scan -------------------------------------------------------------
SKIP_DIRS = {".git", "bin", "vendor", "node_modules", ".terraform", "sbom", "evidence"}
SKIP_EXT  = {".png", ".jpg", ".jpeg", ".gif", ".pdf", ".zip", ".gz", ".tar", ".ico", ".woff",
             ".woff2", ".ttf", ".sum", ".out", ".html"}

def scannable():
    for root, dirs, files in os.walk("."):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        for f in files:
            p = os.path.join(root, f)
            if os.path.splitext(f)[1].lower() in SKIP_EXT:
                continue
            try:
                if os.path.getsize(p) > 4 * 1024 * 1024:      # skip compiled binaries
                    continue
                with open(p, "rb") as fh:
                    head = fh.read(1024)
                if b"\0" in head:
                    continue
            except OSError:
                continue
            yield p.lstrip("./")

FILES = sorted(set(scannable()))

# `runbooks/<name>` in any form: a URL, a docs/ path, or a bare relative reference.
REF = re.compile(r"(?:docs/)?runbooks/([A-Za-z0-9][A-Za-z0-9._*-]*)")

def resolve(name):
    """Return the list of files a reference resolves to, or [] if it dangles."""
    if "*" in name:
        return sorted(glob.glob(os.path.join(rb_dir, name)))
    cands = [name] if name.endswith(".md") else [name + ".md", name]
    return [os.path.join(rb_dir, c) for c in cands if os.path.isfile(os.path.join(rb_dir, c))]

existing = {os.path.basename(p) for p in glob.glob(os.path.join(rb_dir, "*.md"))}
referenced = set()

# ---- R1 / R2 / R5: every reference resolves -------------------------------------------------
for path in FILES:
    try:
        text = open(path, encoding="utf-8", errors="replace").read()
    except OSError:
        continue
    for m in REF.finditer(text):
        name = m.group(1).rstrip(".,;:)\"'`")
        if not name or name in ("md",):
            continue
        checked += 1
        hits = resolve(name)
        if not hits:
            rule = "R1" if "runbook_url" in text[max(0, m.start() - 40):m.start()] else "R2"
            add(rule, path, name, "no file under %s/ matches this reference" % rb_dir)
        else:
            referenced.update(os.path.basename(h) for h in hits)

# Relative links between runbooks: [text](other.md)
LINK = re.compile(r"\]\(([A-Za-z0-9][A-Za-z0-9._-]*\.md)(?:#[^)]*)?\)")
for path in sorted(glob.glob(os.path.join(rb_dir, "*.md"))):
    text = open(path, encoding="utf-8", errors="replace").read()
    for m in LINK.finditer(text):
        target = m.group(1)
        checked += 1
        if not os.path.isfile(os.path.join(rb_dir, target)):
            add("R5", path, target, "relative link between runbooks does not resolve")
        else:
            referenced.add(target)

# ---- R3: a rule that pages carries a runbook_url --------------------------------------------
ALERT = re.compile(r"^\s*-\s*alert:\s*(\S+)\s*$")
for path in sorted(glob.glob("deployments/prometheus/*.yaml")):
    lines = open(path, encoding="utf-8").read().splitlines()
    idx = [i for i, l in enumerate(lines) if ALERT.match(l)]
    for n, start in enumerate(idx):
        end = idx[n + 1] if n + 1 < len(idx) else len(lines)
        block = "\n".join(lines[start:end])
        name = ALERT.match(lines[start]).group(1)
        pages = re.search(r'page:\s*"true"', block) is not None
        has_rb = "runbook_url:" in block
        checked += 1
        if pages and not has_rb:
            add("R3", path, name, "alert pages but carries no runbook_url annotation")

# ---- R4: no orphan runbooks -----------------------------------------------------------------
readme = os.path.join(rb_dir, "README.md")
readme_text = open(readme, encoding="utf-8").read() if os.path.isfile(readme) else ""
for f in sorted(existing):
    if f == "README.md":
        continue
    checked += 1
    if f not in referenced and f not in readme_text:
        add("R4", os.path.join(rb_dir, f), f,
            "runbook is referenced by nothing and is not in the index")

# ---- report ----------------------------------------------------------------------------------
if as_json:
    print(json.dumps({"checked": checked,
                      "runbooks": len(existing) - (1 if "README.md" in existing else 0),
                      "findings": findings}, indent=2))
else:
    total = len(existing) - (1 if "README.md" in existing else 0)
    print("runbook link check — %d runbooks, %d references checked" % (total, checked))
    if findings:
        print()
        for f in findings:
            print("  \033[31m✗\033[0m [%s] %s: %s — %s"
                  % (f["rule"], f["file"], f["reference"], f["message"]))
        print()
        print("\033[31m✗\033[0m %d dangling reference(s)" % len(findings))
    else:
        print("\033[32m✓\033[0m every runbook reference resolves; every paging alert has a runbook")

sys.exit(1 if findings else 0)
PY
