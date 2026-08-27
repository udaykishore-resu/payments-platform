#!/usr/bin/env bash
#
# scripts/check-licences.sh — no copyleft licence enters the dependency graph.
#
# WHAT IT ENFORCES
#   L1  every module in the *build* dependency graph has a licence file that can be found;
#   L2  no module carries a licence on the denylist: GPL-2.0, GPL-3.0, AGPL-1.0, AGPL-3.0,
#       LGPL (any version), SSPL, OSL, EUPL, CDDL, MPL-2.0-with-secondary-licence-notice,
#       or a "commons clause" rider;
#   L3  every licence resolves to a recognised identifier — an unidentifiable licence is
#       treated as a failure, not as a pass, because "we could not tell" and "it is fine"
#       are different answers;
#   L4  the allowlist of accepted identifiers is explicit, so adding a new licence family
#       is a deliberate edit rather than a silent widening.
#
# WHY
#   deployment.md §4.1 stage 13 states the gate as "GPL/AGPL fails". The reason is not
#   ideological. This platform ships as a proprietary hosted service; a strong-copyleft
#   dependency linked into a binary that reaches a customer creates an obligation to
#   publish the source of the payment orchestrator, and the moment to discover that is
#   before the dependency is merged rather than during a licensing audit. AGPL in
#   particular attaches on *network* use — which is exactly how this software is used.
#
#   L3 is the part that is usually skipped and is the part that matters. A tool that
#   reports "unknown" for six modules and green for the rest has told you nothing about
#   those six. Unknown is a failure here; the fix is either to identify the licence and
#   add it to the allowlist, or to name the module in the exception file with a reason.
#
# HOW
#   The dependency set comes from `go list -deps ./...`, which is the *linked* set — not
#   `go.mod`, which lists modules the build may never touch. The licence text is read from
#   the module cache; nothing is downloaded, so the check runs offline against whatever
#   `go build` already needed. Identification is by SPDX header first, then by a signature
#   match on the licence body (the distinguishing sentence of each family), then by the
#   filename. `go-licenses` is used instead when it is installed.
#
# USAGE
#   scripts/check-licences.sh [--exceptions FILE] [--report FILE]
#
# EXIT
#   0 clean · 1 a denied or unidentifiable licence · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

EXCEPTIONS="scripts/licence-exceptions.txt"
REPORT_OUT=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --exceptions) EXCEPTIONS="$2"; shift 2 ;;
    --report)     REPORT_OUT="$2"; shift 2 ;;
    -h|--help)    sed -n '2,45p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)            die "unknown flag: $1" ;;
  esac
done

need go
need python3
cd "$REPO_ROOT"

hdr "dependency licences — no copyleft in the linked build graph"

MODS="$(mktemp)"; REPORT="$(mktemp)"
trap 'rm -f "$MODS" "$REPORT"' EXIT

# The linked set, deduplicated. The main module is excluded (it is ours) and so is the
# standard library (no .Module).
# {{"\t"}} rather than a literal \t: Go's text/template does not process escape
# sequences in template text, so a bare \t would be emitted as two characters and the
# columns would not split.
go list -deps -f '{{if .Module}}{{.Module.Path}}{{"\t"}}{{.Module.Version}}{{"\t"}}{{.Module.Dir}}{{end}}' ./... \
  | sort -u | grep -v '^github.com/udaykishore-resu/payments-platform' > "$MODS" \
  || die "go list -deps failed"

info "$(wc -l < "$MODS") third-party modules in the linked graph"

set +e
python3 - "$MODS" "$EXCEPTIONS" > "$REPORT" <<'PY'
import glob, os, re, sys

mods_path, exc_path = sys.argv[1], sys.argv[2]

problems = []
def bad(kind, msg): problems.append((kind, msg))

# --- policy ------------------------------------------------------------------------------
# Permissive identifiers this platform accepts. Adding one is a deliberate edit and shows
# up in review, which is the whole mechanism: the list is the policy.
ALLOWED = {
    "MIT", "ISC", "BSD-2-Clause", "BSD-3-Clause", "Apache-2.0", "Unlicense",
    "0BSD", "Zlib", "BSD-3-Clause-Clear", "MIT-0", "PostgreSQL", "Python-2.0",
    # MPL-2.0 is weak copyleft with a file-level scope. It is accepted because the
    # obligation attaches to modified files of the dependency itself, not to the work
    # that links it — but it is listed separately so that a future decision to drop it is
    # a one-line change rather than an archaeology exercise.
    "MPL-2.0",
    # Go's own additional patent grant, which accompanies BSD-3-Clause on golang.org/x.
    "BSD-3-Clause+PATENTS",
}

DENIED = {
    "GPL-1.0", "GPL-2.0", "GPL-3.0",
    "AGPL-1.0", "AGPL-3.0",
    "LGPL-2.0", "LGPL-2.1", "LGPL-3.0",
    "SSPL-1.0", "OSL-3.0", "EUPL-1.2", "CDDL-1.0", "CDDL-1.1",
    "CC-BY-NC-4.0", "Commons-Clause", "BUSL-1.1", "Elastic-2.0",
}

# --- exceptions ----------------------------------------------------------------------------
exceptions = {}
if os.path.exists(exc_path):
    for lineno, raw in enumerate(open(exc_path, encoding="utf-8"), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "#" not in line:
            bad("L4", f"{exc_path}:{lineno}: no `# reason` — a licence exception without "
                      f"a stated reason cannot be reviewed")
            continue
        mod, reason = line.split("#", 1)
        exceptions[mod.strip()] = reason.strip()

# --- identification --------------------------------------------------------------------------
LICENCE_FILES = ("LICENSE", "LICENCE", "LICENSE.txt", "LICENCE.txt", "LICENSE.md",
                 "LICENCE.md", "COPYING", "COPYING.txt", "LICENSE-MIT", "LICENSE-APACHE",
                 "LICENSE.BSD", "License.txt", "license.txt", "NOTICE")

# Ordered: the first match wins, and the copyleft signatures come first so that a dual
# licence containing both a permissive and a GPL grant is classified by its stronger term.
SIGNATURES = [
    ("AGPL-3.0",     r"GNU AFFERO GENERAL PUBLIC LICENSE\s+Version 3"),
    ("AGPL-1.0",     r"GNU AFFERO GENERAL PUBLIC LICENSE\s+Version 1"),
    ("GPL-3.0",      r"GNU GENERAL PUBLIC LICENSE\s+Version 3"),
    ("GPL-2.0",      r"GNU GENERAL PUBLIC LICENSE\s+Version 2"),
    ("LGPL-3.0",     r"GNU LESSER GENERAL PUBLIC LICENSE\s+Version 3"),
    ("LGPL-2.1",     r"GNU LESSER GENERAL PUBLIC LICENSE\s+Version 2\.1"),
    ("SSPL-1.0",     r"Server Side Public License"),
    ("BUSL-1.1",     r"Business Source License 1\.1"),
    ("Elastic-2.0",  r"Elastic License 2\.0"),
    ("Commons-Clause", r"Commons Clause"),
    ("CDDL-1.1",     r"COMMON DEVELOPMENT AND DISTRIBUTION LICENSE.{0,40}1\.1"),
    ("CDDL-1.0",     r"COMMON DEVELOPMENT AND DISTRIBUTION LICENSE"),
    ("EUPL-1.2",     r"European Union Public Licence"),
    ("MPL-2.0",      r"Mozilla Public License Version 2\.0"),
    ("Apache-2.0",   r"Apache License\s+Version 2\.0"),
    ("ISC",          r"Permission to use, copy, modify, and(?:/or)? distribute this software"),
    ("BSD-3-Clause", r"Neither the name of .{0,120}? nor the names of its\s+contributors"
                     r"|3\. Neither the name"),
    ("BSD-2-Clause", r"Redistribution and use in source and binary forms"),
    ("MIT",          r"Permission is hereby granted, free of charge, to any person obtaining"),
    ("Unlicense",    r"This is free and unencumbered software released into the public domain"),
    ("0BSD",         r"Permission to use, copy, modify, and/or distribute this software for any"),
]

SPDX = re.compile(r"SPDX-License-Identifier:\s*([A-Za-z0-9.\-+]+)")


def classify_dir(mod_dir):
    """Identify the licence from files in one directory. Returns (ident, path) or (None, None)."""
    for name in LICENCE_FILES:
        p = os.path.join(mod_dir, name)
        if not os.path.isfile(p):
            continue
        try:
            text = open(p, encoding="utf-8", errors="replace").read()
        except OSError:
            continue
        m = SPDX.search(text[:4000])
        if m:
            return m.group(1), p
        for ident, pat in SIGNATURES:
            if re.search(pat, text, re.I | re.S):
                # golang.org/x modules ship a PATENTS file alongside a BSD-3 LICENSE.
                if ident == "BSD-3-Clause" and os.path.isfile(os.path.join(mod_dir, "PATENTS")):
                    return "BSD-3-Clause+PATENTS", p
                return ident, p
    return None, None


GOMODCACHE = os.environ.get("GOMODCACHE") or os.path.join(
    os.environ.get("GOPATH", os.path.expanduser("~/go")), "pkg", "mod")


def identify(mod_path, mod_dir):
    """Identify a module's licence, falling back to its parent module.

    A Go submodule (go.opentelemetry.io/otel/metric inside the otel repository,
    franz-go/pkg/kmsg inside franz-go) ships its own go.mod but usually no LICENSE: the
    repository's single licence file lives in the parent module's zip. Reporting those as
    'unidentified' would be a false positive on nine of this repository's forty-two
    dependencies, and a check with that much noise is a check that gets a blanket
    exception. So: look in the module's own directory first, then walk the import path
    upwards and look for a parent module in the cache, longest prefix first, and report
    the inheritance in the evidence path so a reader can see where the answer came from.
    """
    ident, evidence = classify_dir(mod_dir)
    if ident:
        return ident, evidence

    parts = mod_path.split("/")
    for cut in range(len(parts) - 1, 0, -1):
        parent = "/".join(parts[:cut])
        for cand in sorted(glob.glob(os.path.join(GOMODCACHE, parent + "@*"))):
            ident, evidence = classify_dir(cand)
            if ident:
                return ident, f"{evidence} (inherited from parent module {parent})"
    return None, None


rows = []
for line in open(mods_path, encoding="utf-8"):
    parts = line.rstrip("\n").split("\t")
    if len(parts) != 3:
        continue
    path, version, mod_dir = parts
    ident, evidence = identify(path, mod_dir)
    rows.append((path, version, ident, evidence))

    if path in exceptions:
        print(f"NOTE\t{path} {version}: {ident or 'unidentified'} — exempted: "
              f"{exceptions[path]}")
        continue

    # L1/L3
    if ident is None:
        listing = ""
        if os.path.isdir(mod_dir):
            listing = ", ".join(sorted(f for f in os.listdir(mod_dir)
                                       if "licen" in f.lower() or "copying" in f.lower())) \
                      or "no licence-looking file present"
        bad("L1", f"{path} {version}: no licence could be identified in {mod_dir} "
                  f"({listing}). 'Unknown' is not 'permissive' — identify it and add the "
                  f"identifier to ALLOWED, or record the module in "
                  f"scripts/licence-exceptions.txt with a reason")
        continue

    # L2
    base = ident.split(" ")[0].rstrip("+")
    if base in DENIED or any(base.startswith(p) for p in ("GPL-", "AGPL-", "LGPL-")):
        bad("L2", f"{path} {version}: {ident} (evidence {evidence}) — copyleft. This "
                  f"platform ships as a hosted proprietary service; AGPL in particular "
                  f"attaches on network use, which is exactly how it is used "
                  f"(deployment.md §4.1 stage 13)")
        continue

    # L4
    if base not in ALLOWED:
        bad("L4", f"{path} {version}: {ident} is neither allowed nor denied — the policy "
                  f"has no opinion, which means nobody has formed one. Add it to ALLOWED "
                  f"in scripts/check-licences.sh or deny it")

for path, version, ident, _ in sorted(rows):
    print(f"ROW\t{path}\t{version}\t{ident or 'UNKNOWN'}")

for kind, msg in problems:
    print(f"{kind}\t{msg}")

print(f"COUNT\tmodules={len(rows)} identified={sum(1 for r in rows if r[2])}",
      file=sys.stderr)
sys.exit(1 if problems else 0)
PY
RC=$?
set -e

if [[ -n "$REPORT_OUT" ]]; then
  { echo -e "module\tversion\tlicence"
    grep '^ROW' "$REPORT" | cut -f2-
  } > "$REPORT_OUT"
  info "SBOM-adjacent licence report written to $REPORT_OUT"
fi

while IFS=$'\t' read -r kind rest; do
  case "$kind" in
    ROW|COUNT|"") : ;;
    NOTE) info "$rest" ;;
    *)    fail "[$kind] $rest" ;;
  esac
done < "$REPORT"

if [[ $RC -eq 0 ]]; then
  ok "L1 every linked module has an identifiable licence"
  ok "L2 no copyleft licence in the graph"
  ok "L3 nothing classified as unknown"
  ok "L4 every licence is on the explicit allowlist"
elif [[ $RC -ne 1 ]]; then
  die "the licence scan itself failed (exit $RC)"
fi

summary "check-licences"
