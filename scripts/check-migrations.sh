#!/usr/bin/env bash
#
# scripts/check-migrations.sh — the migration set is well-formed, reversible and tenant-safe.
#
# WHAT IT ENFORCES
#   G1  numbering is contiguous from 0001 with no gaps and no duplicates;
#   G2  every `NNNN_slug.up.sql` has a matching `NNNN_slug.down.sql` with the identical slug;
#   G3  every table carrying a `tenant_id` column has ROW LEVEL SECURITY enabled AND at
#       least one policy — enabling RLS without a policy denies everything, and adding a
#       policy without enabling RLS enforces nothing, so both halves are required;
#   G4  no unmarked destructive statement. `DROP TABLE`, `DROP COLUMN`, `DROP INDEX`,
#       `DROP CONSTRAINT`, `TRUNCATE`, `ALTER COLUMN … TYPE` and `SET NOT NULL` in an
#       `.up.sql` must carry a `-- pp:destructive <reason>` marker on the preceding line;
#   G5  no `CREATE INDEX` without `CONCURRENTLY` on a table that already exists in an
#       earlier migration (a plain CREATE INDEX takes an ACCESS EXCLUSIVE-adjacent lock
#       and blocks writes for the duration);
#   G6  no `BEGIN`/`COMMIT` inside a migration file — the runner owns the transaction, and
#       a nested one either fails or silently splits the migration into two units of work;
#   G7  filenames match ^[0-9]{4}_[a-z0-9_]+\.(up|down)\.sql$.
#
# WHY
#   G1/G2 are about being able to *reason* about the schema. A gap means a merge dropped a
#   file; a missing down means the expand/contract sequence in deployment.md §5.1 cannot be
#   rehearsed, and a migration that has never been reversed in a test is a migration whose
#   reversal is a hypothesis.
#
#   G3 is the tenant-isolation control from baseline §16.2 expressed where it actually
#   lives. Application-level tenant scoping is a filter someone can forget; RLS is a
#   property of the table. The check exists because the failure is silent: a table added
#   without a policy reads perfectly well in every test that uses one tenant.
#
#   G4 does not forbid destructive statements — expand/contract requires them. It forbids
#   *unannounced* ones. The marker is what makes a reviewer look at the deploy-ordering
#   question ("is every replica of the old binary gone?") instead of at the SQL.
#
#   G5 and G6 are the two operational footguns that turn a routine deploy into an incident
#   on a table with 10⁸ rows.
#
# USAGE
#   scripts/check-migrations.sh [--dir migrations]
#
# EXIT
#   0 clean · 1 a violation · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

DIR="migrations"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)     DIR="$2"; shift 2 ;;
    -h|--help) sed -n '2,45p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)         die "unknown flag: $1" ;;
  esac
done

need python3
cd "$REPO_ROOT"
[[ -d "$DIR" ]] || die "missing directory $DIR"

hdr "migrations — ${DIR}/"

REPORT="$(mktemp)"; trap 'rm -f "$REPORT"' EXIT
set +e
python3 - "$DIR" > "$REPORT" <<'PY'
import glob, os, re, sys

d = sys.argv[1]
problems = []
def bad(kind, msg): problems.append((kind, msg))

NAME_RE = re.compile(r"^(\d{4})_([a-z0-9_]+)\.(up|down)\.sql$")

files = sorted(os.path.basename(p) for p in glob.glob(os.path.join(d, "*.sql")))
ups, downs = {}, {}

# --- G7: filenames -------------------------------------------------------------------------
for f in files:
    m = NAME_RE.match(f)
    if not m:
        bad("G7", f"{f}: filename must match ^[0-9]{{4}}_[a-z0-9_]+\\.(up|down)\\.sql$")
        continue
    num, slug, direction = int(m.group(1)), m.group(2), m.group(3)
    target = ups if direction == "up" else downs
    if num in target:
        bad("G1", f"{f}: number {num:04d} is already taken by "
                  f"{num:04d}_{target[num]}.{direction}.sql — a merge collision")
    target[num] = slug

# --- G1: contiguity ------------------------------------------------------------------------
if ups:
    lo, hi = min(ups), max(ups)
    if lo != 1:
        bad("G1", f"numbering starts at {lo:04d}, not 0001")
    missing = [n for n in range(lo, hi + 1) if n not in ups]
    for n in missing:
        bad("G1", f"no migration numbered {n:04d} — a gap is a merge accident, and it "
                  f"makes 'which migrations has this database seen' unanswerable")
else:
    bad("G1", f"no *.up.sql files found in {d}/")

# --- G2: pairing ---------------------------------------------------------------------------
for n, slug in sorted(ups.items()):
    if n not in downs:
        bad("G2", f"{n:04d}_{slug}.up.sql has no matching down script — its reversal is "
                  f"an untested hypothesis (deployment.md §5.4)")
    elif downs[n] != slug:
        bad("G2", f"{n:04d}: up slug {slug!r} != down slug {downs[n]!r}")
for n, slug in sorted(downs.items()):
    if n not in ups:
        bad("G2", f"{n:04d}_{slug}.down.sql has no matching up script")

# --- read the up scripts once --------------------------------------------------------------
up_src = {}
for n, slug in sorted(ups.items()):
    p = os.path.join(d, f"{n:04d}_{slug}.up.sql")
    try:
        up_src[n] = open(p, encoding="utf-8").read()
    except OSError as e:
        bad("G7", f"{p}: unreadable: {e}")
all_up = "\n".join(up_src[n] for n in sorted(up_src))


def strip_comments(sql):
    """Remove -- line comments and /* */ blocks so a keyword in prose is not a finding."""
    sql = re.sub(r"/\*.*?\*/", " ", sql, flags=re.S)
    return re.sub(r"--[^\n]*", "", sql)


def create_tables(sql):
    """Yield (name, column-body) for each CREATE TABLE, matching parentheses rather than
    guessing at a terminator, because a column list contains nested parens (CHECK, numeric
    precision, GENERATED expressions) that a lazy regex truncates."""
    for m in re.finditer(r"CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][\w.%\"]*)\s*\(",
                         sql, re.I):
        i, depth = m.end() - 1, 0
        while i < len(sql):
            if sql[i] == "(":
                depth += 1
            elif sql[i] == ")":
                depth -= 1
                if depth == 0:
                    break
            i += 1
        yield m.group(1), sql[m.end():i]


# --- G3: RLS on every tenant-scoped table ---------------------------------------------------
clean_all = strip_comments(all_up)
tenant_tables = []
for n in sorted(up_src):
    for name, body in create_tables(strip_comments(up_src[n])):
        # A partition template written with a format placeholder (pp.%I) is generated at
        # runtime by a partition-management function; it inherits the parent's RLS and has
        # no static declaration to check.
        if "%" in name:
            continue
        if re.search(r"(^|,)\s*\"?tenant_id\"?\s", body, re.M | re.I):
            tenant_tables.append((n, name))

for n, name in tenant_tables:
    esc = re.escape(name)
    rls = re.search(rf"ALTER\s+TABLE\s+(?:ONLY\s+)?{esc}\b[^;]*ENABLE\s+ROW\s+LEVEL\s+SECURITY",
                    clean_all, re.I | re.S)
    pol = re.search(rf"CREATE\s+POLICY\b[^;]*?\bON\s+(?:TABLE\s+)?{esc}\b",
                    clean_all, re.I | re.S)
    if not rls:
        bad("G3", f"{name} (migration {n:04d}) has a tenant_id column but never has ROW "
                  f"LEVEL SECURITY enabled — cross-tenant reads are prevented only by "
                  f"application code that a single forgotten WHERE clause defeats "
                  f"(baseline §16.2)")
    if not pol:
        bad("G3", f"{name} (migration {n:04d}) has a tenant_id column but no CREATE POLICY "
                  f"— RLS without a policy denies every row, including to the service")

# --- G4: unmarked destructive statements -----------------------------------------------------
DESTRUCTIVE = [
    (r"\bDROP\s+TABLE\b", "DROP TABLE"),
    (r"\bDROP\s+COLUMN\b", "DROP COLUMN"),
    (r"\bDROP\s+INDEX\b", "DROP INDEX"),
    (r"\bDROP\s+CONSTRAINT\b", "DROP CONSTRAINT"),
    (r"\bDROP\s+SCHEMA\b", "DROP SCHEMA"),
    (r"\bTRUNCATE\b", "TRUNCATE"),
    (r"\bALTER\s+COLUMN\s+\w+\s+TYPE\b", "ALTER COLUMN … TYPE"),
    (r"\bSET\s+NOT\s+NULL\b", "SET NOT NULL"),
]
MARKER = re.compile(r"--\s*pp:destructive\s+\S", re.I)

for n in sorted(up_src):
    lines = up_src[n].splitlines()
    for idx, line in enumerate(lines):
        code = re.sub(r"--[^\n]*", "", line)
        for pat, label in DESTRUCTIVE:
            if not re.search(pat, code, re.I):
                continue
            # The marker may sit on the same line or on any of the three preceding lines,
            # so a multi-line statement can be annotated once at its head.
            window = lines[max(0, idx - 3): idx + 1]
            if any(MARKER.search(w) for w in window):
                continue
            bad("G4", f"{n:04d} line {idx + 1}: unmarked {label} — annotate with "
                      f"`-- pp:destructive <why it is safe now>`; the marker is what makes "
                      f"a reviewer check that every replica of the old binary is gone "
                      f"(deployment.md §5.1 contract phase)")

# --- G5: blocking index creation --------------------------------------------------------------
for n in sorted(up_src):
    src = strip_comments(up_src[n])
    earlier = "\n".join(strip_comments(up_src[m]) for m in sorted(up_src) if m < n)
    earlier_tables = {name for name, _ in create_tables(earlier)}
    for m in re.finditer(r"CREATE\s+(UNIQUE\s+)?INDEX\s+(CONCURRENTLY\s+)?"
                         r"(?:IF\s+NOT\s+EXISTS\s+)?([\w.\"]+)\s+ON\s+(?:ONLY\s+)?([\w.\"]+)",
                         src, re.I):
        concurrently, table = m.group(2), m.group(4)
        if concurrently:
            continue
        if table in earlier_tables:
            bad("G5", f"{n:04d}: CREATE INDEX on the pre-existing table {table} without "
                      f"CONCURRENTLY — it takes a lock that blocks writes for the whole "
                      f"build. An index created in the same migration as its table is "
                      f"fine; this one is not")

# --- G6: explicit transaction control ----------------------------------------------------------
for n in sorted(up_src):
    src = strip_comments(up_src[n])
    for kw in ("BEGIN", "COMMIT", "ROLLBACK"):
        # `BEGIN` also opens a PL/pgSQL block body, which is legitimate and common here.
        # Only a bare statement-level BEGIN/COMMIT is a finding, so require a terminating
        # semicolon on the same line and no preceding `AS $$`-style block marker nearby.
        for m in re.finditer(rf"^\s*{kw}\s*;", src, re.I | re.M):
            head = src[:m.start()]
            if head.count("$$") % 2 == 1:
                continue      # inside a dollar-quoted function body
            bad("G6", f"{n:04d}: bare {kw}; — the migration runner owns the transaction "
                      f"(deployment.md §5.3); a nested one either errors or splits the "
                      f"migration into two units of work with a window between them")

for kind, msg in problems:
    print(f"{kind}\t{msg}")

print(f"COUNT\tup={len(ups)} down={len(downs)} tenant_scoped_tables={len(tenant_tables)}",
      file=sys.stderr)
sys.exit(1 if problems else 0)
PY
RC=$?
set -e

case $RC in
  0)
    ok "G1 numbering is contiguous from 0001"
    ok "G2 every up has a matching down"
    ok "G3 every tenant-scoped table has RLS enabled and a policy"
    ok "G4 no unmarked destructive statement"
    ok "G5 no blocking CREATE INDEX on a pre-existing table"
    ok "G6 no migration manages its own transaction"
    ok "G7 every filename is well-formed"
    ;;
  1)
    while IFS=$'\t' read -r kind msg; do
      case "$kind" in
        ""|COUNT) : ;;
        *)        fail "[$kind] $msg" ;;
      esac
    done < "$REPORT"
    ;;
  *) die "the check itself failed (exit $RC)" ;;
esac

summary "check-migrations"
