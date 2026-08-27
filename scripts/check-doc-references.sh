#!/usr/bin/env bash
#
# scripts/check-doc-references.sh — every repo-relative path a document cites must exist.
#
# WHAT IT ENFORCES
#   D1  Every markdown link target `[text](path)` in a scanned document that is a
#       repo-relative path (not a URL, not a bare `#anchor`, not a `mailto:`) resolves to a
#       file or directory that exists. Link targets are resolved relative to the document's
#       own directory, then — if that misses — relative to the repository root, because both
#       spellings appear in this tree and both render correctly on GitHub.
#   D2  Every inline-code span `` `path` `` that is unambiguously a repository path resolves.
#       "Unambiguously" is defined narrowly on purpose (see WHAT IT DELIBERATELY IGNORES):
#       the span must start with one of the tracked top-level directories, or carry a
#       recognised source extension and a `/`.
#   D3  A path carrying a `::TestName`, `#anchor`, `:line` or `§n` suffix is checked without
#       the suffix — the file must exist even when the anchor within it is not checkable.
#   D4  A glob (`docs/runbooks/security-*.md`, `api/events/*.schema.json`) must match at
#       least one file.
#
# WHY
#   This is the check that would have caught the whole class of drift where a document
#   describes a script, a registry file or a test that was renamed or never written. A
#   citation that 404s is worse than no citation: the reader spends time looking for
#   something the author implied exists, and the document's other claims lose credit.
#
#   The rule is deliberately about *existence*, not about content. Asserting that
#   `docs/testing.md` describes what `scripts/coverage.sh` actually does is a job for a
#   human; asserting that `scripts/coverage.sh` is a file is a job for a machine, and the
#   machine should do it on every push.
#
# WHAT IT SCANS
#   docs/**/*.md and the repository-root *.md files (README, CONTRIBUTING, SECURITY).
#
# WHAT IT DELIBERATELY IGNORES
#   * URLs, `mailto:`, protocol-relative and bare-anchor links.
#   * Fenced code blocks. They are examples, and an example may legitimately name
#     `tests/load/steady-state.js --env BASE=...` or a path in someone else's repository.
#     Prose and tables are where a citation makes a promise; a code fence is where it
#     demonstrates a shape.
#   * Inline code that is prose, a command, an environment variable, a glob of state names,
#     a package path with a `...` wildcard, or a URL-ish string.
#   * Package-qualified Go identifiers — `internal/domain/payment.FailoverPolicy` names a
#     type in a package, not a file, and the package it names is checked separately by the
#     architecture fitness function and by the compiler.
#   * `<placeholder>` and `…` segments — `docs/runbooks/<name>.md` names a shape, not a file.
#
# NAMING SOMETHING THAT DELIBERATELY DOES NOT EXIST
#   A limitations section, a gap table or a "this is not implemented" paragraph has to be able
#   to name the artifact it says is absent — that is the whole point of writing it down. Two
#   escape hatches, both HTML comments so they render as nothing:
#
#     `scripts/mutation-probe.sh` does not exist. <!-- doc-refs: allow-missing -->
#
#         skips every reference on that one line.
#
#     <!-- doc-refs: allow-missing begin -->
#     | Referenced by | Missing artifact |
#     …
#     <!-- doc-refs: allow-missing end -->
#
#         skips every reference between the markers.
#
#   Use them for absence that is being *reported*, never to quiet a citation that is merely
#   wrong. A reference inside a marked region is one the author is asserting is missing; if it
#   turns out to exist, the marker is the lie, not the check.
#
# USAGE
#   scripts/check-doc-references.sh [--json] [--list]
#
#     --json   machine-readable findings
#     --list   also print every reference that was checked and passed
#
# EXIT
#   0 every reference resolves · 1 one or more dangling · 2 could not run.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

AS_JSON=0
LIST=0
for arg in "$@"; do
  case "$arg" in
    --json) AS_JSON=1 ;;
    --list) LIST=1 ;;
    -h|--help) sed -n '2,60p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }

AS_JSON="$AS_JSON" LIST="$LIST" python3 - <<'PY'
import glob, json, os, re, sys

as_json = os.environ["AS_JSON"] == "1"
listing = os.environ["LIST"] == "1"

# --- what counts as a repository path -------------------------------------------------------
# Anchored on the tree's actual top-level directories so that prose like `application/json`
# or `text/plain` cannot be mistaken for a path.
TOP = ("api/", "cmd/", "config/", "deploy/", "deployments/", "docs/", "helm/", "internal/",
       "migrations/", "pkg/", "scripts/", "terraform/", "tests/", ".github/")
ROOT_FILES = {"Makefile", "Dockerfile", "go.mod", "go.sum", "README.md", "CONTRIBUTING.md",
              "SECURITY.md", "LICENSE", ".env.dev", ".golangci.yml", ".dockerignore",
              ".gitignore", "tools.go"}
SRC_EXT = (".go", ".sh", ".py", ".sql", ".yaml", ".yml", ".json", ".md", ".tf", ".proto",
           ".js", ".tpl", ".txt")

# Suffixes a citation may carry that are not part of the filename.
SUFFIX = re.compile(r"(::.*|#.*|:\d+.*|\s+§.*)$")

# `internal/domain/payment.FailoverPolicy` — a package path plus an exported identifier.
GO_IDENT = re.compile(r"/[a-z0-9_]+\.[A-Z][A-Za-z0-9_]*$")

def scanned_files():
    out = []
    for root, dirs, files in os.walk("docs"):
        dirs[:] = sorted(d for d in dirs if not d.startswith("."))
        for f in sorted(files):
            if f.endswith(".md"):
                out.append(os.path.join(root, f))
    out += sorted(f for f in os.listdir(".") if f.endswith(".md") and os.path.isfile(f))
    return out

ALLOW_LINE  = "<!-- doc-refs: allow-missing -->"
ALLOW_BEGIN = "<!-- doc-refs: allow-missing begin -->"
ALLOW_END   = "<!-- doc-refs: allow-missing end -->"

def strip_code_fences(text):
    """Blank out fenced blocks and allow-missing regions, keeping line numbering intact."""
    lines = text.split("\n")
    fence, allowing = None, False
    for i, line in enumerate(lines):
        if ALLOW_BEGIN in line:
            allowing = True
            lines[i] = ""
            continue
        if ALLOW_END in line:
            allowing = False
            lines[i] = ""
            continue
        if allowing or ALLOW_LINE in line:
            lines[i] = ""
            continue
        m = re.match(r"^\s*(```+|~~~+)", line)
        if fence is None and m:
            fence = m.group(1)[0] * 3
            lines[i] = ""
            continue
        if fence is not None:
            closing = re.match(r"^\s*(```+|~~~+)\s*$", line)
            lines[i] = ""
            if closing:
                fence = None
    return "\n".join(lines)

def looks_like_path(s):
    if not s or s.startswith(("http://", "https://", "mailto:", "#", "//", "$", "-")):
        return False
    if any(c in s for c in " \t\"'|`()[]{}<>,;!?=") or "\\" in s:
        return False
    if "..." in s or "…" in s or ("*" in s and "/" not in s):
        return False
    if GO_IDENT.search(s):
        return False
    if s in ROOT_FILES:
        return True
    if s.startswith(TOP):
        return True
    if "/" in s and s.endswith(SRC_EXT):
        return True
    return False

def resolve(ref, doc):
    """Return True when ref exists, relative to doc's directory or to the repo root."""
    if any(ch in ref for ch in "*?["):
        base = os.path.dirname(doc)
        return bool(glob.glob(os.path.join(base, ref))) or bool(glob.glob(ref))
    cands = [os.path.normpath(os.path.join(os.path.dirname(doc), ref)), os.path.normpath(ref)]
    return any(os.path.exists(c) for c in cands)

LINK    = re.compile(r"\[[^\]]*\]\(\s*<?([^)\s>]+)>?\s*(?:\"[^\"]*\")?\)")
CODESPN = re.compile(r"`([^`\n]+)`")

findings = []
checked = 0
passed  = []

for doc in scanned_files():
    with open(doc, encoding="utf-8") as fh:
        raw = fh.read()
    prose = strip_code_fences(raw)

    refs = []   # (reference, kind)
    for m in LINK.finditer(prose):
        refs.append((m.group(1), "link"))
    for m in CODESPN.finditer(prose):
        refs.append((m.group(1), "code"))

    for ref, kind in refs:
        ref = ref.strip()
        if kind == "link":
            if ref.startswith(("http://", "https://", "mailto:", "#", "//")):
                continue
            ref = ref.split("#", 1)[0]
            if not ref:
                continue
        else:
            ref = SUFFIX.sub("", ref).strip()
            if not looks_like_path(ref):
                continue
        if "<" in ref or ">" in ref:            # `docs/runbooks/<name>.md` is a shape
            continue
        ref = ref.rstrip("/") or "/"
        checked += 1
        if resolve(ref, doc):
            passed.append((doc, ref))
            continue
        findings.append({"file": doc, "reference": ref, "kind": kind,
                         "message": "referenced path does not exist"})

if as_json:
    print(json.dumps({"checked": checked, "dangling": len(findings),
                      "findings": findings}, indent=2))
    sys.exit(1 if findings else 0)

if listing:
    for doc, ref in passed:
        print("    ok    %s -> %s" % (doc, ref))

for f in findings:
    print("    \033[31mFAIL\033[0m  %s cites %s (does not exist)" % (f["file"], f["reference"]),
          file=sys.stderr)

print("    checked %d repo-relative reference(s) across %d document(s)"
      % (checked, len(scanned_files())))
if findings:
    print("\033[31m\033[1m✗ check-doc-references: %d dangling reference(s)\033[0m" % len(findings),
          file=sys.stderr)
    sys.exit(1)
print("\033[32m\033[1m✓ check-doc-references: clean\033[0m")
PY
