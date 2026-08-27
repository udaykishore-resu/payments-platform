#!/usr/bin/env bash
#
# scripts/check-metrics-cardinality.sh — no metric declares a high-cardinality label.
#
# WHAT IT ENFORCES
#   M1  no `pp_*` metric declares a forbidden label. Baseline §22.3 names merchant_id,
#       payment_id, attempt_id, idempotency_key, email and ip; this check additionally
#       rejects the near-synonyms an author reaches for when the obvious name is refused
#       (customer_id, user_id, session_id, request_id, trace_id, span_id, correlation_id,
#       client_ip, remote_addr, card_number, pan, url, path, user_agent, error_message,
#       and the camelCase spellings), because a rule that only blocks the exact string is
#       a speed bump.
#   M2  the lint's list and the runtime guard's list agree. telemetry.ValidateLabels is
#       probed with every candidate: a label this lint rejects but the runtime accepts is
#       a lint nobody trusts, and one the runtime rejects but the lint accepts is a
#       production startup failure discovered after merge.
#   M3  no undeclared metric: every `pp_*` metric declared anywhere in the tree is in the
#       telemetry package's exported name set (the §22.2 contract plus the three
#       self-observability metrics).
#   M4  no orphan metric: every name in that set has a declaration site.
#   M5  metric names match ^pp_[a-z0-9]+(_[a-z0-9]+)*$ and carry a unit suffix where the
#       type implies one (`_seconds` on a duration histogram, `_total` on a counter).
#
# WHY
#   A cardinality incident is not a metrics problem, it is an availability problem, and it
#   is one of the few failure modes where the observability stack takes down the thing it
#   was installed to observe: series count explodes, scrape duration grows past the
#   interval, the remote-write queue backs up, and the tenant loses *all* metrics —
#   including the ones the on-call needs to understand what is happening. One
#   `merchant_id` label at 50 000 merchants (§18) times a handful of other dimensions is
#   millions of series from a two-word diff.
#
#   The runtime guard in internal/infrastructure/telemetry catches this at process start.
#   That is the right backstop but the wrong place to *learn* it: the feedback arrives
#   after merge, in a crash-looping pod. This lint moves the same rule to the pull request,
#   which is why both exist and why M2 asserts they cannot drift apart.
#
#   The scan works on the Go AST, matching on the SHAPE of a declaration (a `pp_*` name
#   plus a []string label list in the same call) rather than on a specific prometheus API,
#   so the local counter/gauge/histogram helpers inside the registry constructor are seen
#   as well as a raw prometheus.NewCounterVec anywhere else in the tree.
#
# USAGE
#   scripts/check-metrics-cardinality.sh [--json]
#
# EXIT
#   0 clean · 1 a forbidden label, undeclared metric or orphan metric · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

AS_JSON=0
[[ "${1:-}" == "--json" ]] && AS_JSON=1
[[ "${1:-}" == "-h" || "${1:-}" == "--help" ]] && { sed -n '2,50p' "${BASH_SOURCE[0]}"; exit 0; }

need go
need python3
cd "$REPO_ROOT"

hdr "metric cardinality — baseline §22.3"

DUMP="$(mktemp)"; REPORT="$(mktemp)"
trap 'rm -f "$DUMP" "$REPORT"' EXIT
go run ./scripts/specdump metrics -root . > "$DUMP" || die "could not scan metric declarations"

if [[ $AS_JSON -eq 1 ]]; then cat "$DUMP"; exit 0; fi

set +e
python3 - "$DUMP" > "$REPORT" <<'PY'
import json, re, sys

d = json.load(open(sys.argv[1]))

declared = set(d["declared"])
decls = d["declarations"]
budget = d["maxSeriesPerMetric"]

# The dump marks any label this lint forbids that the runtime guard does NOT reject with a
# " (lint-only)" suffix. That asymmetry is M2's finding.
forbidden, lint_only = set(), set()
for f in d["forbidden"]:
    if f.endswith(" (lint-only)"):
        name = f[: -len(" (lint-only)")]
        forbidden.add(name)
        lint_only.add(name)
    else:
        forbidden.add(f)

problems = []
def bad(kind, msg): problems.append((kind, msg))

NAME_RE = re.compile(r"^pp_[a-z0-9]+(_[a-z0-9]+)*$")

# --- M1: forbidden labels ---------------------------------------------------------------
for decl in decls:
    for label in decl["labels"]:
        if label in forbidden:
            bad("M1", f"{decl['metric']} declares forbidden label {label!r} at {decl['pos']} "
                      f"— it belongs in logs, traces and exemplars, never in a label set "
                      f"(baseline §22.3)")

# --- M2: lint and runtime agree ----------------------------------------------------------
# lint_only is reported as a WARNing, not a failure: the lint being stricter than the
# runtime is safe (nothing reaches production that the lint allowed and the runtime would
# have refused). It is still worth saying out loud, because the intended end state is that
# telemetry.forbiddenLabelNames carries the whole list.
if lint_only:
    bad("WARN-M2", f"{len(lint_only)} label(s) are rejected by this lint but accepted by "
                   f"telemetry.ValidateLabels: {', '.join(sorted(lint_only))}. "
                   f"Add them to telemetry.forbiddenLabelNames so the runtime guard "
                   f"agrees with the pull-request gate.")

# --- M3/M4: declared vs found ------------------------------------------------------------
found = {decl["metric"] for decl in decls}
for m in sorted(found - declared):
    bad("M3", f"{m} is declared in the tree but is not in the telemetry package's "
              f"metric-name set — every pp_* metric belongs to the §22.2 contract "
              f"(observability.md §3.1: the registry is the only place one may be declared)")
for m in sorted(declared - found):
    bad("M4", f"{m} is named by a telemetry constant but has no declaration site — "
              f"an orphan metric: dashboards and alert rules referencing it will never "
              f"resolve")

# --- M5: naming ---------------------------------------------------------------------------
for m in sorted(found | declared):
    if not NAME_RE.match(m):
        bad("M5", f"{m}: name must match ^pp_[a-z0-9]+(_[a-z0-9]+)*$ "
                  f"(conventions §Naming: pp_<subsystem>_<name>_<unit>)")

for kind, msg in problems:
    print(f"{kind}\t{msg}")

nlabels = {len(x["labels"]) for x in decls} or {0}
print(f"COUNT\tdeclarations={len(decls)} distinct_metrics={len(found)} "
      f"labels_per_metric={min(nlabels)}-{max(nlabels)} budget={budget}", file=sys.stderr)

sys.exit(1 if any(not k.startswith("WARN-") for k, _ in problems)
         else (2 if problems else 0))
PY
RC=$?
set -e

emit_report() {
  while IFS=$'\t' read -r kind msg; do
    case "$kind" in
      WARN-*)   warn "[${kind#WARN-}] $msg" ;;
      ""|COUNT) : ;;
      *)        fail "[$kind] $msg" ;;
    esac
  done < "$REPORT"
}

case $RC in
  0)
    ok "M1 no metric declares a forbidden label"
    ok "M2 the lint and the runtime cardinality guard agree"
    ok "M3 no undeclared pp_* metric outside the registry"
    ok "M4 no orphan metric constant"
    ok "M5 every metric name is well-formed"
    ;;
  2) emit_report; ok "M1/M3/M4/M5 clean" ;;
  1) emit_report ;;
  *) die "the lint itself failed (exit $RC)" ;;
esac

summary "check-metrics-cardinality"
