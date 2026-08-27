# shellcheck shell=bash
#
# scripts/lib/common.sh — shared plumbing for every check script in scripts/.
#
# Not executable and never run directly: it is sourced. Every consumer sets its own
# `set -euo pipefail`; this file deliberately does not, because a sourced file that
# changes the caller's shell options is a trap for anyone who sources it from an
# interactive shell.
#
# What it provides:
#   * colourised, TTY-aware pass/fail output that degrades to plain text when stdout is
#     not a terminal (CI logs, `tee`, `less -R` off) — colour codes in a log file are
#     noise, and a check whose output is unreadable in CI is a check nobody reads;
#   * a failure counter (`fail`) so a script can report *every* violation in one run
#     rather than stopping at the first, which is what makes a fitness function usable:
#     one CI run should tell you everything that is wrong;
#   * `need`/`have` for optional tooling, so a check can degrade honestly ("skipped:
#     trivy not installed") instead of silently passing.

# --- colour -----------------------------------------------------------------------------
if [[ -t 1 ]] && [[ "${NO_COLOR:-}" == "" ]] && [[ "${TERM:-dumb}" != "dumb" ]]; then
  C_RED=$'\033[31m'; C_GRN=$'\033[32m'; C_YEL=$'\033[33m'
  C_BLU=$'\033[34m'; C_DIM=$'\033[2m';  C_BLD=$'\033[1m'; C_OFF=$'\033[0m'
else
  C_RED=''; C_GRN=''; C_YEL=''; C_BLU=''; C_DIM=''; C_BLD=''; C_OFF=''
fi

# --- repo root --------------------------------------------------------------------------
# Resolved from this file's own location rather than $PWD, so every script works when
# invoked as scripts/x.sh, ./x.sh, or from an arbitrary directory in CI.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export REPO_ROOT

# --- counters ---------------------------------------------------------------------------
FAILURES=0
WARNINGS=0

hdr()  { printf '%s\n' "${C_BLD}${C_BLU}==> $*${C_OFF}"; }
info() { printf '%s\n' "${C_DIM}    $*${C_OFF}"; }
ok()   { printf '%s\n' "    ${C_GRN}PASS${C_OFF}  $*"; }
warn() { WARNINGS=$((WARNINGS + 1)); printf '%s\n' "    ${C_YEL}WARN${C_OFF}  $*" >&2; }
skip() { printf '%s\n' "    ${C_YEL}SKIP${C_OFF}  $*" >&2; }
fail() { FAILURES=$((FAILURES + 1)); printf '%s\n' "    ${C_RED}FAIL${C_OFF}  $*" >&2; }

# die exits immediately: for a broken precondition (a missing input file), not for a
# policy violation. A policy violation is a `fail` so the run can keep finding more.
die() { printf '%s\n' "${C_RED}${C_BLD}error:${C_OFF} $*" >&2; exit 2; }

# summary prints the verdict and returns the exit code the script should use.
# Usage:  summary "check-name"; exit $?
summary() {
  local name="$1"
  if (( FAILURES > 0 )); then
    printf '%s\n' "${C_RED}${C_BLD}✗ ${name}: ${FAILURES} violation(s)${C_OFF}" >&2
    return 1
  fi
  if (( WARNINGS > 0 )); then
    printf '%s\n' "${C_GRN}${C_BLD}✓ ${name}: clean${C_OFF} ${C_YEL}(${WARNINGS} warning(s))${C_OFF}"
  else
    printf '%s\n' "${C_GRN}${C_BLD}✓ ${name}: clean${C_OFF}"
  fi
  return 0
}

# have reports whether a command exists, quietly.
have() { command -v "$1" >/dev/null 2>&1; }

# need requires a command or dies. Use for things without which the check is meaningless
# (go, python3). For optional scanners use `have` + `skip`.
need() { have "$1" || die "required tool not found on PATH: $1"; }

# offline reports whether we appear to have no outbound network. Checks that install a
# tool at runtime consult this so that a laptop on a plane gets an honest SKIP rather
# than a four-minute timeout followed by a confusing failure.
offline() {
  if [[ -n "${PP_ASSUME_OFFLINE:-}" ]]; then return 0; fi
  ! curl --silent --head --max-time 4 https://proxy.golang.org/ >/dev/null 2>&1
}

# go_run_tool installs and runs a Go tool at a pinned version via `go run`, which does
# NOT mutate go.mod/go.sum of this module (module-aware `go run pkg@version` builds in an
# ephemeral module since Go 1.16). That property is why it is used here rather than
# `go install` + PATH juggling: this repository's go.mod is shared and must not drift.
go_run_tool() {
  local pkg="$1"; shift
  if offline; then
    skip "offline: cannot fetch ${pkg}"
    return 100
  fi
  GOFLAGS=-mod=mod go run "$pkg" "$@"
}
