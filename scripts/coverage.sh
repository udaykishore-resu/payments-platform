#!/usr/bin/env bash
#
# scripts/coverage.sh — the two gates of docs/testing.md §1.1 and §1.2.
#
# STAGE 1: per-scope statement coverage (§1.1)
#   Measures coverage for each scope in the table below and compares it against two numbers:
#
#     floor    the coverage this tree already achieves, minus a point of slack. Dropping
#              below it FAILS. This is the ratchet: a change that removes coverage is a
#              build failure, which is the last row of the §1.1 table ("coverage may not
#              decrease") made into a property rather than an intention.
#     target   the gate docs/testing.md §1.1 states. Being below it WARNS and prints the
#              distance. Several scopes are a long way below, and the warning is the point:
#              the number is visible on every run instead of living only in a document.
#
#   Raising a floor after improving coverage is a deliberate, reviewable edit to this file.
#   Lowering one requires saying so in a pull request, which is exactly the conversation a
#   coverage regression should cause.
#
# STAGE 2: the critical-path registry (§1.2)
#   Coverage measures which lines executed; it says nothing about whether anything was
#   asserted. tests/critical_paths.yaml is the list of money-safety properties and the tests
#   that prove them. This stage asserts every `tests:` entry resolves — the file exists and
#   declares a top-level `func <Name>(`. A renamed or deleted test therefore fails the build
#   naming the critical-path ID that lost its evidence, rather than silently leaving a
#   property unasserted.
#
#   It does NOT prove the test still asserts the property. That needs mutation probing, which
#   this repository does not implement; docs/testing.md §1.2 says so and says what it would
#   take.
#
# WHAT IS MEASURED
#   `go test ./... -short`, the same set `make cover` and the `test` stage of verify --fast
#   run: no build tags, no containers. Integration-, chaos- and e2e-tagged tests therefore do
#   not contribute, which is why the floors are where they are — a package whose behaviour is
#   covered only by an integration test reads as uncovered here. That is the honest number
#   for "what a developer's laptop proves", and it is the number CI's unit stage produces.
#
# USAGE
#   scripts/coverage.sh [--profile FILE] [--enforce-targets] [--only-paths] [--quiet]
#
#     --profile FILE      use an existing coverage profile instead of running the tests
#                         (make cover passes the one it just produced, so the suite runs once)
#     --enforce-targets   treat the §1.1 targets, not the floors, as the failure threshold.
#                         Currently fails. It is the switch that turns the aspiration into a
#                         gate on the day the tree earns it.
#     --only-paths        skip the coverage stage; run only the critical-path registry check
#     --quiet             print only failures and the verdict
#
# EXIT
#   0 every gate passed · 1 a gate failed · 2 could not run.

set -uo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

need go
need python3
cd "$REPO_ROOT" || die "cannot cd to $REPO_ROOT"

PROFILE=""
ENFORCE_TARGETS=0
ONLY_PATHS=0
QUIET=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile)          PROFILE="${2:-}"; shift 2 ;;
    --enforce-targets)  ENFORCE_TARGETS=1; shift ;;
    --only-paths)       ONLY_PATHS=1; shift ;;
    --quiet)            QUIET=1; shift ;;
    -h|--help)          sed -n '2,55p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)                  die "unknown flag: $1" ;;
  esac
done

# scope<TAB>floor<TAB>target — the gate table of docs/testing.md §1.1.
# "." is the whole repository.
read -r -d '' SCOPES <<'TSV' || true
internal/domain	79.0	95
internal/application	51.0	90
internal/validation	79.0	95
internal/platform/idempotency	86.0	100
internal/platform/tenantctx	96.0	100
internal/workflows	56.0	85
internal/adapters/gateway	52.0	80
internal/infrastructure	52.0	70
.	56.0	80
TSV

RC=0

# --- stage 1: coverage --------------------------------------------------------------------
if (( ! ONLY_PATHS )); then
  hdr "coverage — per-scope statement coverage (docs/testing.md §1.1)"

  CLEANUP=""
  if [[ -z "$PROFILE" ]]; then
    PROFILE="$(mktemp)"
    CLEANUP="$PROFILE"
    # shellcheck disable=SC2064  # expand PROFILE now: the trap must survive the variable
    trap "rm -f '$CLEANUP'" EXIT
    info "running go test ./... -short -covermode=atomic"
    if ! go test ./... -short -covermode=atomic -coverprofile="$PROFILE" >/dev/null 2>&1; then
      fail "the test suite did not pass; coverage is not meaningful until it does"
      RC=1
    fi
  fi
  [[ -s "$PROFILE" ]] || die "coverage profile $PROFILE is empty"

  set +e
  SCOPES="$SCOPES" ENFORCE_TARGETS="$ENFORCE_TARGETS" QUIET="$QUIET" \
    python3 - "$PROFILE" <<'PY'
import os, sys

profile = sys.argv[1]
enforce_targets = os.environ["ENFORCE_TARGETS"] == "1"
quiet = os.environ["QUIET"] == "1"
module = "github.com/udaykishore-resu/payments-platform/"

covered, total = {}, {}
with open(profile) as fh:
    for line in fh:
        line = line.strip()
        if not line or line.startswith("mode:"):
            continue
        block, stmts, count = line.rsplit(" ", 2)
        path = block.split(":")[0]
        if not path.startswith(module):
            continue
        pkg = path[len(module):].rsplit("/", 1)[0]
        n = int(stmts)
        total[pkg] = total.get(pkg, 0) + n
        if int(count) > 0:
            covered[pkg] = covered.get(pkg, 0) + n

def measure(scope):
    c = t = 0
    for pkg in total:
        if scope == "." or pkg == scope or pkg.startswith(scope + "/"):
            c += covered.get(pkg, 0)
            t += total[pkg]
    return (100.0 * c / t if t else 0.0), c, t

rows, failed, warned = [], 0, 0
for line in os.environ["SCOPES"].strip().split("\n"):
    scope, floor, target = line.split("\t")
    floor, target = float(floor), float(target)
    pct, c, t = measure(scope)
    if t == 0:
        print("    \033[33mWARN\033[0m  %s matched no statements" % scope, file=sys.stderr)
        warned += 1
        continue
    threshold = target if enforce_targets else floor
    ok = pct + 1e-9 >= threshold
    if not ok:
        failed += 1
    elif pct + 1e-9 < target:
        warned += 1
    rows.append((scope, pct, c, t, floor, target, ok))

label = "target" if enforce_targets else "floor"
if not quiet:
    print("    %-32s %8s %8s %8s   %s" % ("scope", "measured", "floor", "target", "statements"))
for scope, pct, c, t, floor, target, ok in rows:
    mark = "\033[32mPASS\033[0m" if ok else "\033[31mFAIL\033[0m"
    if pct + 1e-9 < target and ok:
        mark = "\033[33mBELOW\033[0m"
    if not quiet or not ok:
        stream = sys.stdout if ok else sys.stderr
        print("    %s  %-30s %7.2f%% %7.1f%% %7.1f%%   %d/%d"
              % (mark, scope, pct, floor, target, c, t), file=stream)

if failed:
    print("\033[31m\033[1m✗ coverage: %d scope(s) below their %s\033[0m" % (failed, label),
          file=sys.stderr)
    sys.exit(1)
if warned and not quiet:
    print("    \033[33m%d scope(s) are above their floor but below the docs/testing.md §1.1 "
          "target\033[0m" % warned)
print("\033[32m\033[1m✓ coverage: every scope is at or above its %s\033[0m" % label)
PY
  cov_rc=$?
  set -e
  (( cov_rc == 0 )) || RC=1
fi

# --- stage 2: the critical-path registry ---------------------------------------------------
hdr "critical paths — every registered property still has its test (docs/testing.md §1.2)"

REGISTRY="tests/critical_paths.yaml"
[[ -f "$REGISTRY" ]] || die "missing $REGISTRY"

set +e
python3 - "$REGISTRY" <<'PY'
import os, re, sys

registry = sys.argv[1]

# A deliberately small parser rather than a PyYAML dependency: this file's shape is fixed by
# its own header, and a check that cannot run because a library is missing is a check nobody
# runs. It reads `- id:` blocks and the `tests:` list items inside them.
entries, cur = [], None
for raw in open(registry, encoding="utf-8"):
    line = raw.rstrip("\n")
    m = re.match(r"^\s*-\s+id:\s*(\S+)\s*$", line)
    if m:
        cur = {"id": m.group(1), "tests": [], "property": ""}
        entries.append(cur)
        continue
    if cur is None:
        continue
    m = re.match(r"^\s*property:\s*\"(.*)\"\s*$", line)
    if m:
        cur["property"] = m.group(1)
        continue
    m = re.match(r"^\s*-\s+\"([^\"]+::[A-Za-z0-9_]+)\"\s*$", line)
    if m:
        cur["tests"].append(m.group(1))

if not entries:
    print("    \033[31mFAIL\033[0m  %s parsed to zero entries" % registry, file=sys.stderr)
    sys.exit(1)

seen, bad, checked = set(), 0, 0
for e in entries:
    if e["id"] in seen:
        print("    \033[31mFAIL\033[0m  duplicate id %s" % e["id"], file=sys.stderr)
        bad += 1
    seen.add(e["id"])
    if not e["property"]:
        print("    \033[31mFAIL\033[0m  %s has no property statement" % e["id"], file=sys.stderr)
        bad += 1
    if not e["tests"]:
        print("    \033[31mFAIL\033[0m  %s names no test" % e["id"], file=sys.stderr)
        bad += 1
    for ref in e["tests"]:
        checked += 1
        path, name = ref.split("::", 1)
        if not os.path.isfile(path):
            print("    \033[31mFAIL\033[0m  %s cites %s — no such file" % (e["id"], path),
                  file=sys.stderr)
            bad += 1
            continue
        with open(path, encoding="utf-8") as fh:
            src = fh.read()
        if not re.search(r"^func %s\(" % re.escape(name), src, re.M):
            print("    \033[31mFAIL\033[0m  %s cites %s::%s — %s declares no such test"
                  % (e["id"], path, name, path), file=sys.stderr)
            bad += 1

print("    %d critical path(s), %d test reference(s) checked" % (len(entries), checked))
if bad:
    print("\033[31m\033[1m✗ critical paths: %d unresolved reference(s)\033[0m" % bad,
          file=sys.stderr)
    sys.exit(1)
print("\033[32m\033[1m✓ critical paths: every registered property still names a test that exists\033[0m")
PY
cp_rc=$?
set -e
(( cp_rc == 0 )) || RC=1

exit "$RC"
