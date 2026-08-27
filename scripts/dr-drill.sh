#!/usr/bin/env bash
#
# scripts/dr-drill.sh — RD-1, the weekly snapshot restore + integrity drill.
#
# WHAT IT DOES
#   Implements the procedure in docs/disaster-recovery.md §5.3 end to end:
#
#     1. pick the most recent AUTOMATED Aurora snapshot — never a pinned known-good one;
#     2. restore it into the isolated dr-verify VPC and wait for the instance;
#     3. verify: row counts, the §5.3 money invariants (scripts/sql/dr-invariants.sql),
#        the audit chain from genesis, and that the migrations apply as a no-op;
#     4. prove the application can run against it (TestAgainstRestoredSnapshot);
#     5. write the evidence bundle, upload it, and tear the drill cluster down.
#
# WHY A BACKUP IS NOT A BACKUP UNTIL IT HAS BEEN RESTORED
#   "We take snapshots" is a statement about a scheduled job, not about recoverability.
#   The three ways this fails silently — a snapshot the KMS key can no longer decrypt, a
#   schema the current binary cannot migrate, and a restore that takes longer than the RTO
#   — are all invisible until someone tries. Step 1 deliberately takes whatever the backup
#   system last produced rather than a snapshot known to work, because the property under
#   test is the backup system, not this script.
#
#   Step 3 is the drill. The restore is only the setup. A restore that completes and
#   contains a broken audit chain is a worse outcome than a restore that fails, because it
#   would be trusted.
#
# TEARDOWN IS UNCONDITIONAL
#   The teardown runs from an EXIT trap, so a failed verification still removes the
#   cluster. A drill that leaves a full copy of the production database running in a VPC
#   until someone notices the bill is a data-exposure incident caused by a data-protection
#   process. `--keep` suppresses it for an investigation and prints, loudly, what must be
#   deleted by hand.
#
# THIS SCRIPT DOES NOT RUN IN THIS ENVIRONMENT
#   It needs AWS credentials for the dr-verify account, the drill VPC's security group and
#   subnet group, and a live Aurora cluster to snapshot. `--dry-run` prints every command
#   it would execute, in order, with the arguments resolved as far as they can be without
#   AWS — which is how it is reviewed and how the runbook is checked against reality.
#
# USAGE
#   scripts/dr-drill.sh [--dry-run] [--keep] [--cluster ID] [--evidence DIR]
#                       [--skip-smoke] [--drill-id ID]
#
#   Required environment (unless --dry-run):
#     AWS_REGION              region of the source cluster
#     PP_DRILL_SG             security group ID for the drill instance
#     PP_DRILL_SUBNET_GROUP   DB subnet group in the dr-verify VPC
#     PP_DRILL_KMS_KEY        KMS key alias/ARN the snapshot is encrypted under
#     PP_DRILL_DSN_TEMPLATE   DSN with %HOST% and %PORT% placeholders
#     PP_EVIDENCE_BUCKET      s3://… destination for the evidence bundle
#
# EXIT
#   0 the drill passed every criterion · 1 a criterion failed · 2 could not run.
#
#   A failed RD-1 pages the on-call at P2 and files a blocking issue; two consecutive
#   failures escalate to P1, because at that point the state of the backup system is
#   unknown, which is operationally identical to having no backups (§5.3).

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

DRY_RUN=0
KEEP=0
SKIP_SMOKE=0
SOURCE_CLUSTER="${PP_SOURCE_CLUSTER:-pp-prod}"
EVIDENCE_ROOT="${PP_EVIDENCE_DIR:-evidence}"
DRILL_ID="rd1-$(date -u +%Y%m%dT%H%M%SZ)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)    DRY_RUN=1; shift ;;
    --keep)       KEEP=1; shift ;;
    --skip-smoke) SKIP_SMOKE=1; shift ;;
    --cluster)    SOURCE_CLUSTER="$2"; shift 2 ;;
    --evidence)   EVIDENCE_ROOT="$2"; shift 2 ;;
    --drill-id)   DRILL_ID="$2"; shift 2 ;;
    -h|--help)    sed -n '2,55p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)            die "unknown flag: $1" ;;
  esac
done

cd "$REPO_ROOT"

DRILL_CLUSTER="pp-drill-${DRILL_ID}"
DRILL_INSTANCE="${DRILL_CLUSTER}-1"
EV="${EVIDENCE_ROOT}/${DRILL_ID}"

# run: the single execution point. In --dry-run it prints the command and returns success,
# which is what makes the whole procedure reviewable without an AWS account. Everything
# that touches AWS or the drill database goes through here — nothing bypasses it, because
# a dry run with one live call in it is worse than no dry run.
run() {
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '%s\n' "    ${C_DIM}[dry-run]${C_OFF} $*"
    return 0
  fi
  "$@"
}

# capture: like run, but the stdout is the value AND is written to the evidence bundle.
capture() {
  local artifact="$1"; shift
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '%s\n' "    ${C_DIM}[dry-run]${C_OFF} $* ${C_DIM}> ${EV}/${artifact}${C_OFF}" >&2
    echo "DRY-RUN-PLACEHOLDER"
    return 0
  fi
  "$@" | tee "${EV}/${artifact}"
}

hdr "RD-1 snapshot restore + integrity drill — ${DRILL_ID}"
if [[ $DRY_RUN -eq 1 ]]; then
  warn "dry run: no AWS call is made and no database is contacted"
else
  need aws
  need psql
  need go
  for v in AWS_REGION PP_DRILL_SG PP_DRILL_SUBNET_GROUP PP_DRILL_KMS_KEY \
           PP_DRILL_DSN_TEMPLATE PP_EVIDENCE_BUCKET; do
    [[ -n "${!v:-}" ]] || die "required environment variable $v is not set"
  done
fi

run mkdir -p "$EV"
info "evidence bundle: ${EV}"

# --- teardown -----------------------------------------------------------------------------------
teardown() {
  local rc=$?
  if [[ $KEEP -eq 1 ]]; then
    warn "--keep: the drill cluster ${DRILL_CLUSTER} is STILL RUNNING and holds a full copy"
    warn "of production data. Delete it by hand when the investigation is finished:"
    warn "  aws rds delete-db-instance --db-instance-identifier ${DRILL_INSTANCE} --skip-final-snapshot"
    warn "  aws rds delete-db-cluster  --db-cluster-identifier ${DRILL_CLUSTER} --skip-final-snapshot"
    return $rc
  fi
  hdr "teardown (runs whether or not the drill passed)"
  run aws rds delete-db-instance \
    --db-instance-identifier "$DRILL_INSTANCE" --skip-final-snapshot >/dev/null 2>&1 || true
  run aws rds delete-db-cluster \
    --db-cluster-identifier "$DRILL_CLUSTER" --skip-final-snapshot >/dev/null 2>&1 || true
  info "drill cluster removed"
  return $rc
}
trap teardown EXIT

T0=$SECONDS

# --- 1. the newest AUTOMATED snapshot -------------------------------------------------------------
hdr "1. select the snapshot"
info "taking whatever the backup system last produced — not a pinned known-good snapshot,"
info "because the backup system is what is under test"

if [[ $DRY_RUN -eq 1 ]]; then
  SNAP="rds:${SOURCE_CLUSTER}-2026-08-26-04-00"
  printf '%s\n' "    ${C_DIM}[dry-run]${C_OFF} aws rds describe-db-cluster-snapshots --db-cluster-identifier ${SOURCE_CLUSTER} --snapshot-type automated …"
else
  SNAP="$(aws rds describe-db-cluster-snapshots \
            --db-cluster-identifier "$SOURCE_CLUSTER" --snapshot-type automated \
            --query 'sort_by(DBClusterSnapshots,&SnapshotCreateTime)[-1].DBClusterSnapshotIdentifier' \
            --output text)"
  [[ -n "$SNAP" && "$SNAP" != "None" ]] || die "no automated snapshot found for ${SOURCE_CLUSTER}"
  printf '%s\n' "$SNAP" > "${EV}/snapshot-id.txt"

  # Pass criterion: the newest automated snapshot must be less than 26 hours old. 26 rather
  # than 24 so that a daily job that drifts by an hour does not page anyone, while a job
  # that stopped entirely still does.
  SNAP_TIME="$(aws rds describe-db-cluster-snapshots \
                 --db-cluster-snapshot-identifier "$SNAP" \
                 --query 'DBClusterSnapshots[0].SnapshotCreateTime' --output text)"
  AGE_H=$(( ( $(date -u +%s) - $(date -u -d "$SNAP_TIME" +%s) ) / 3600 ))
  printf 'snapshot=%s created=%s age_hours=%s\n' "$SNAP" "$SNAP_TIME" "$AGE_H" \
    >> "${EV}/snapshot-id.txt"
  if (( AGE_H >= 26 )); then
    fail "the newest automated snapshot is ${AGE_H}h old (criterion: < 26h) — the backup schedule is not running"
  else
    ok "snapshot ${SNAP} is ${AGE_H}h old"
  fi
fi
info "snapshot: ${SNAP}"

# --- 2. restore ------------------------------------------------------------------------------------
hdr "2. restore into the dr-verify VPC"
run aws rds restore-db-cluster-from-snapshot \
  --db-cluster-identifier "$DRILL_CLUSTER" \
  --snapshot-identifier "$SNAP" \
  --engine aurora-postgresql \
  --vpc-security-group-ids "${PP_DRILL_SG:-sg-DRILL}" \
  --db-subnet-group-name "${PP_DRILL_SUBNET_GROUP:-pp-drill-db}" \
  --kms-key-id "${PP_DRILL_KMS_KEY:-alias/pp-prod-rds}" \
  --no-publicly-accessible \
  --deletion-protection=false

run aws rds create-db-instance \
  --db-instance-identifier "$DRILL_INSTANCE" \
  --db-cluster-identifier "$DRILL_CLUSTER" \
  --db-instance-class db.r6g.2xlarge \
  --engine aurora-postgresql \
  --no-publicly-accessible

run aws rds wait db-instance-available --db-instance-identifier "$DRILL_INSTANCE"

T1=$SECONDS
RESTORE_SECONDS=$((T1 - T0))
run bash -c "printf 'restore_seconds=%s\n' '$RESTORE_SECONDS' >> '${EV}/timings.txt'"
info "restore took ${RESTORE_SECONDS}s"

# Pass criterion: ≤ 30 minutes for the current data volume. This tracks growth; a trend
# breaching 30 min triggers a capacity review long before it breaches the 15 min RTO,
# because a restore is only one part of a region failover.
if [[ $DRY_RUN -eq 0 ]] && (( RESTORE_SECONDS > 1800 )); then
  fail "restore took ${RESTORE_SECONDS}s (criterion: ≤ 1800s); the RTO budget of §18 is at risk"
else
  ok "restore duration within budget"
fi

# --- resolve the drill DSN --------------------------------------------------------------------------
if [[ $DRY_RUN -eq 1 ]]; then
  DRILL_DSN="postgres://pp_drill:REDACTED@${DRILL_CLUSTER}.cluster-XXXX.rds.amazonaws.com:5432/pp"
else
  HOST="$(aws rds describe-db-clusters --db-cluster-identifier "$DRILL_CLUSTER" \
            --query 'DBClusters[0].Endpoint' --output text)"
  PORT="$(aws rds describe-db-clusters --db-cluster-identifier "$DRILL_CLUSTER" \
            --query 'DBClusters[0].Port' --output text)"
  DRILL_DSN="${PP_DRILL_DSN_TEMPLATE//%HOST%/$HOST}"
  DRILL_DSN="${DRILL_DSN//%PORT%/$PORT}"
fi
export PP_DSN="$DRILL_DSN"

# --- 3. verify -----------------------------------------------------------------------------------
hdr "3. verify — this is the drill; the restore was the setup"

info "3a. row counts and store-level checks"
capture verify.json \
  platformctl dr verify --dsn "$DRILL_DSN" --checks all --format json >/dev/null || \
  fail "platformctl dr verify reported a problem"

info "3b. audit chain from genesis"
capture audit-chain.txt \
  platformctl audit verify-chain --dsn "$DRILL_DSN" --from-genesis >/dev/null || \
  fail "the audit chain is broken — a record written before the snapshot is missing or altered"

info "3c. money invariants (scripts/sql/dr-invariants.sql)"
if [[ $DRY_RUN -eq 1 ]]; then
  printf '%s\n' "    ${C_DIM}[dry-run]${C_OFF} psql \"\$DRILL_DSN\" -f scripts/sql/dr-invariants.sql > ${EV}/invariants.txt"
else
  if psql "$DRILL_DSN" -v ON_ERROR_STOP=1 -f scripts/sql/dr-invariants.sql \
       > "${EV}/invariants.txt" 2>&1; then
    if grep -q 'FAIL' "${EV}/invariants.txt"; then
      fail "one or more money invariants FAILED — see ${EV}/invariants.txt"
      grep -B1 'FAIL' "${EV}/invariants.txt" | head -40 >&2
    else
      ok "I1–I8 all PASS"
    fi
  else
    fail "the invariant script did not complete — an inconclusive integrity check is a failed one"
  fi
fi

info "3d. migrations apply as a no-op"
capture migration-status.txt \
  platformctl migrate status --dsn "$DRILL_DSN" >/dev/null || \
  fail "migrate status failed against the restored schema"
run platformctl migrate up --dsn "$DRILL_DSN" --dry-run || \
  fail "a no-op 'migrate up' against the restored schema did not succeed — the restore is \
not one the current binary can run against, which is a restore that cannot be used in an \
actual failover"

# --- 4. the application actually runs against it -------------------------------------------------------
if [[ $SKIP_SMOKE -eq 0 ]]; then
  hdr "4. smoke — prove the application can run against the restore"
  info "reads a payment, replays an idempotent request, appends a ledger entry, and asserts"
  info "RLS blocks a cross-tenant read (§5.3 pass criteria)"
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '%s\n' "    ${C_DIM}[dry-run]${C_OFF} go test ./tests/integration/... -tags=integration -run TestAgainstRestoredSnapshot"
  else
    if go test ./tests/integration/... -tags=integration -count=1 \
         -run TestAgainstRestoredSnapshot 2>&1 | tee "${EV}/smoke.txt"; then
      ok "TestAgainstRestoredSnapshot passed"
    else
      fail "TestAgainstRestoredSnapshot failed — see ${EV}/smoke.txt"
    fi
  fi
fi

T2=$SECONDS
run bash -c "printf 'verify_seconds=%s\ntotal_seconds=%s\n' '$((T2 - T1))' '$((T2 - T0))' >> '${EV}/timings.txt'"

# --- 5. report and publish -------------------------------------------------------------------------
hdr "5. evidence"
VERDICT="PASS"
(( FAILURES > 0 )) && VERDICT="FAIL"

REPORT="${EV}/report.md"
if [[ $DRY_RUN -eq 1 ]]; then
  printf '%s\n' "    ${C_DIM}[dry-run]${C_OFF} write ${REPORT}"
else
  cat > "$REPORT" <<EOF
# RD-1 restore drill — ${DRILL_ID}

| Field | Value |
|---|---|
| Verdict | **${VERDICT}** |
| Source cluster | \`${SOURCE_CLUSTER}\` |
| Snapshot | \`${SNAP}\` |
| Drill cluster | \`${DRILL_CLUSTER}\` (deleted at teardown) |
| Restore duration | ${RESTORE_SECONDS}s (criterion ≤ 1800s) |
| Verify duration | $((T2 - T1))s |
| Total | $((T2 - T0))s |
| Failed criteria | ${FAILURES} |

## Pass criteria (disaster-recovery.md §5.3)

| Check | Evidence |
|---|---|
| Snapshot age < 26 h | \`snapshot-id.txt\` |
| Restore completes, cluster available | \`timings.txt\` |
| Restore ≤ 30 min | \`timings.txt\` |
| Audit chain unbroken from genesis | \`audit-chain.txt\` |
| I1 refunds ≤ captured | \`invariants.txt\` |
| I2 captures ≤ authorized | \`invariants.txt\` |
| I3 ≤ 1 successful attempt per payment | \`invariants.txt\` |
| I4 ledger balances per account | \`invariants.txt\` |
| I5 audit chain links resolve | \`invariants.txt\` |
| I6 no payment stranded > 24 h | \`invariants.txt\` |
| I7 RLS intact on tenant tables | \`invariants.txt\` |
| I8 outbox partition keys present | \`invariants.txt\` |
| Migrations no-op | \`migration-status.txt\` |
| Smoke suite passes | \`smoke.txt\` |

## Escalation

A failed RD-1 pages the on-call at **P2** and files a blocking issue. Two consecutive
failures escalate to **P1**: at that point the state of the backup system is unknown,
which is operationally identical to having no backups.
EOF
  ok "report written to ${REPORT}"
fi

run aws s3 cp "$EV" "${PP_EVIDENCE_BUCKET:-s3://pp-prod-backups/dr-evidence}/${DRILL_ID}/" --recursive

echo
summary "dr-drill (${DRILL_ID})"
