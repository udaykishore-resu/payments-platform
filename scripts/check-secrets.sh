#!/usr/bin/env bash
#
# scripts/check-secrets.sh — nothing that looks like a credential, a key or a PAN is committed.
#
# WHAT IT ENFORCES
#   S1  no private key block (RSA/EC/OPENSSH/PGP `-----BEGIN … PRIVATE KEY-----`);
#   S2  no provider credential matching a known high-confidence shape: AWS access key IDs,
#       AWS secret access keys in an assignment, GitHub/GitLab/Slack/Stripe/SendGrid tokens,
#       Google API keys, JWTs with a decodable header, private-looking connection strings
#       with an inline password;
#   S3  no generic assignment of a secret-looking name (password/secret/token/apikey/
#       credential/private_key) to a long literal that is not obviously a placeholder;
#   S4  no Luhn-valid 13–19 digit run anywhere in the tree — a candidate PAN;
#   S5  the allowlist itself is well-formed and every entry carries a reason.
#
# WHY
#   §17 puts the PCI boundary at "no PAN ever reaches this platform". A card number in a
#   fixture is not a compliance technicality: it is a card number in a public git history,
#   and git history is the one place a secret cannot be deleted from. Both halves of that
#   sentence are why this check runs on every push rather than on a schedule — the cost of
#   catching it before the commit lands and after are three orders of magnitude apart.
#
#   S4 uses Luhn rather than a card-prefix pattern because a prefix pattern has both a
#   worse false-negative rate (it misses any issuer not in the list) and no better false-
#   positive rate on the thing that actually generates noise here: long numeric literals.
#   Luhn eliminates ~90 % of random digit runs for free.
#
# THE ALLOWLIST
#   Documented test vectors — the published Visa/Mastercard/Amex test numbers, the RFC
#   example JWTs, the simulator's fixed credentials — are legitimate and are recorded in
#   scripts/secrets-allowlist.txt. An entry is `<sha256-of-the-matched-token>  # reason`
#   or `path:<glob>  # reason`. Hashing rather than listing the literal keeps the
#   allowlist itself from becoming the file where the card numbers live, and forces each
#   exemption to be individually justified rather than a directory being waved through.
#
# USAGE
#   scripts/check-secrets.sh [--allowlist FILE] [--hash TOKEN]
#
#   --hash prints the allowlist hash for a literal, so adding a justified exemption is one
#   command rather than a guess at the digest.
#
# EXIT
#   0 clean · 1 a finding · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

ALLOWLIST="scripts/secrets-allowlist.txt"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --allowlist) ALLOWLIST="$2"; shift 2 ;;
    --hash)      printf '%s' "$2" | sha256sum | cut -d' ' -f1; exit 0 ;;
    -h|--help)   sed -n '2,45p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)           die "unknown flag: $1" ;;
  esac
done

need python3
cd "$REPO_ROOT"

hdr "committed secrets — private keys, credentials and PANs"

# gitleaks, when available, is a strictly better S1/S2 than anything written here; it runs
# in addition, never instead, because it knows nothing about §17's PAN rule.
if have gitleaks; then
  info "gitleaks is installed; running it in addition to the built-in scan"
  gitleaks detect --no-banner --redact --exit-code 1 --source . \
    || fail "gitleaks reported findings (see above)"
elif ! offline && have go; then
  info "gitleaks not installed and network is available; the built-in scan runs alone"
  info "install with: go install github.com/gitleaks/gitleaks/v8@latest"
else
  skip "gitleaks unavailable — running the built-in scan only"
fi

REPORT="$(mktemp)"; trap 'rm -f "$REPORT"' EXIT
set +e
python3 - "$ALLOWLIST" > "$REPORT" <<'PY'
import fnmatch, hashlib, os, re, sys

allow_path = sys.argv[1]

problems = []
def bad(kind, msg): problems.append((kind, msg))

# --- S5: load and validate the allowlist -----------------------------------------------------
allow_hashes, allow_paths = {}, {}
if os.path.exists(allow_path):
    for lineno, raw in enumerate(open(allow_path, encoding="utf-8"), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "#" not in line:
            bad("S5", f"{allow_path}:{lineno}: no `# reason` — an exemption without a "
                      f"stated reason is indistinguishable from an accident")
            continue
        value, reason = line.split("#", 1)
        value, reason = value.strip(), reason.strip()
        if not reason:
            bad("S5", f"{allow_path}:{lineno}: empty reason")
            continue
        if value.startswith("path:"):
            allow_paths[value[len("path:"):].strip()] = reason
        elif re.fullmatch(r"[0-9a-f]{64}", value):
            allow_hashes[value] = reason
        else:
            bad("S5", f"{allow_path}:{lineno}: entry must be a sha256 hex digest or "
                      f"`path:<glob>`, got {value!r} "
                      f"(use scripts/check-secrets.sh --hash '<literal>')")

def allowed(token, path):
    h = hashlib.sha256(token.encode()).hexdigest()
    if h in allow_hashes:
        return True
    for glob, _ in allow_paths.items():
        if fnmatch.fnmatch(path, glob):
            return True
    return False

# --- what to scan ------------------------------------------------------------------------------
SKIP_DIRS = {".git", "vendor", "node_modules", ".terraform", "dist", "build",
             ".idea", ".vscode", "__pycache__"}
# The detector's own source contains every pattern it looks for, by construction, and
# the allowlist holds the digests of the values it exempts. Scanning either reports the
# tool on itself.
SKIP_FILES = {"scripts/check-secrets.sh", "scripts/secrets-allowlist.txt"}
# Binary-ish and lockfile extensions produce nothing but false positives.
SKIP_EXT = {".png", ".jpg", ".jpeg", ".gif", ".ico", ".pdf", ".zip", ".gz", ".tar",
            ".woff", ".woff2", ".ttf", ".so", ".dylib", ".dll", ".exe", ".bin",
            ".sum", ".lock", ".wasm", ".jar", ".class", ".pyc"}
MAX_BYTES = 4 * 1024 * 1024

# --- detectors -----------------------------------------------------------------------------------
# A bare `-----BEGIN PRIVATE KEY-----` string with nothing after it is a label, not a key
# — it shows up in redaction tests and in documentation about redaction. The block must
# carry at least 40 characters of base64 payload before its END marker to count, which is
# the difference between "the words appear" and "the key is here".
PRIVATE_KEY_BLOCK = re.compile(
    r"-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY(?: BLOCK)?-----"
    r"[\s]*[A-Za-z0-9+/=\s:.,\-]{40,}?"
    r"-----END (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY(?: BLOCK)?-----",
    re.S)

PROVIDER = [
    ("aws-access-key-id",   re.compile(r"\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b")),
    ("aws-secret-key",      re.compile(r"(?i)aws.{0,20}?(?:secret|private).{0,20}?['\"]"
                                       r"([A-Za-z0-9/+=]{40})['\"]")),
    ("github-token",        re.compile(r"\bgh[pousr]_[A-Za-z0-9]{36,255}\b")),
    ("gitlab-token",        re.compile(r"\bglpat-[A-Za-z0-9_\-]{20,}\b")),
    ("slack-token",         re.compile(r"\bxox[abposr]-[A-Za-z0-9-]{10,}\b")),
    ("stripe-key",          re.compile(r"\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b")),
    ("sendgrid-key",        re.compile(r"\bSG\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}\b")),
    ("google-api-key",      re.compile(r"\bAIza[0-9A-Za-z_\-]{35}\b")),
    ("npm-token",           re.compile(r"\bnpm_[A-Za-z0-9]{36}\b")),
    ("pgp-block",           re.compile(r"-----BEGIN PGP PRIVATE KEY BLOCK-----")),
    ("jwt",                 re.compile(r"\beyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}"
                                       r"\.[A-Za-z0-9_\-]{10,}\b")),
    ("dsn-inline-password", re.compile(r"\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|amqp)"
                                       r"://[^\s:/@]+:([^\s:/@]{6,})@")),
]

# S3: a secret-looking name assigned a long literal. Placeholders are excluded by value,
# not by filename, because a fixture is exactly where a real credential gets pasted.
GENERIC = re.compile(
    r"(?i)\b(pass(?:wd|word)?|secret|token|api[_-]?key|access[_-]?key|client[_-]?secret|"
    r"credential|private[_-]?key)\b\s*[:=]\s*['\"]([^'\"\s]{12,})['\"]")

# A value that announces itself as fake. The list is matched against the VALUE, never
# against the filename: a fixture file is exactly where a real credential gets pasted, so
# the exemption has to be earned by the string rather than by where it lives.
#
# `not_a_real_password`, `REDACTED` and `${...}` are included because the local development
# stack, the CI service containers and this repository's own scripts all carry a DSN with a
# deliberately-obvious password in it, and reporting those trains everyone to skip S2's
# output — which is where the real provider tokens are reported.
PLACEHOLDER = re.compile(
    r"(?i)^(?:\$\{|\{\{|<|xxx|placeholder|example|changeme|redacted|dummy|fake|sample|"
    r"your[_-]|test[_-]?only|replace[_-]?me|todo|n/?a$|\*+$)"
    r"|(?:^[Xx]+$)|(?:^0+$)|(?:^(?:secret|password|token|value)$)"
    r"|env:|vault:|aws-secrets:|sops:|file://|arn:aws:"
    r"|not[_-]?a[_-]?real|not[_-]?real|dev[_-]?only|ci[_-]?only|scratch"
    r"|^REDACTED$|^hunter2$|^changeit$")

# The run must not be embedded in a word. `ten_01JB8Z0000000000000000000` contains
# nineteen consecutive zeros that satisfy Luhn, but they are the tail of a ULID, not a
# card number — and a scanner that reports every identifier is a scanner whose output is
# ignored. Requiring a non-word boundary keeps `"4111111111111111"` (quoted, comma- or
# whitespace-delimited) while dropping digit runs glued to letters or underscores.
PAN = re.compile(r"(?<![\w\-])\d(?:[ -]?\d){12,18}(?![\w\-])")


def luhn(digits: str) -> bool:
    total, alt = 0, False
    for ch in reversed(digits):
        d = ord(ch) - 48
        if alt:
            d *= 2
            if d > 9:
                d -= 9
        total += d
        alt = not alt
    return total % 10 == 0


def mask(tok: str) -> str:
    """Never print a candidate secret in full: the CI log is one more place it would live."""
    t = re.sub(r"\s", "", tok)
    if len(t) <= 8:
        return "*" * len(t)
    return t[:4] + "…" + t[-4:] + f" (len {len(t)})"


scanned = 0
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
    for fn in files:
        path = os.path.join(root, fn)
        if path.startswith("./"):
            path = path[2:]
        if path in SKIP_FILES:
            continue
        if os.path.splitext(fn)[1].lower() in SKIP_EXT:
            continue
        try:
            if os.path.getsize(path) > MAX_BYTES:
                continue
            with open(path, "rb") as fh:
                raw = fh.read()
        except OSError:
            continue
        if b"\x00" in raw[:8192]:
            continue                       # binary
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError:
            continue
        scanned += 1

        # S1 spans lines, so it is matched against the whole file rather than line by line.
        for m in PRIVATE_KEY_BLOCK.finditer(text):
            if allowed(m.group(0), path):
                continue
            lineno = text.count("\n", 0, m.start()) + 1
            bad("S1", f"{path}:{lineno}: a private key block with key material is committed")

        # A file's test-ness decides the severity of S3 ONLY. Test fixtures are synthetic
        # by convention and by review habit, so a secret-shaped assignment there is a weak
        # signal and a large share of this scan's noise; the same assignment in config, a
        # deployment manifest or production code is a strong one. S1, S2 and S4 are
        # unaffected: a real key, a real provider token or a real card number is exactly as
        # leaked from a _test.go file as from anywhere else.
        is_test = (fn.endswith("_test.go") or "/testdata/" in "/" + path
                   or path.startswith("tests/") or fn.endswith("_test.py"))

        for lineno, line in enumerate(text.splitlines(), 1):
            # S2
            for name, pat in PROVIDER:
                for m in pat.finditer(line):
                    tok = m.group(1) if m.groups() else m.group(0)
                    if allowed(tok, path):
                        continue
                    # The provider-token detectors match a vendor-specific SHAPE
                    # (`AKIA…`, `ghp_…`, `sk_live_…`) and are reported unconditionally: a
                    # string in that shape is indistinguishable from a leak to GitHub's own
                    # secret scanning, to the vendor's, and to an attacker, so "it's fake"
                    # is not a property anything downstream can verify.
                    #
                    # `dsn-inline-password` is different. Its capture group is a free-form
                    # password, so the placeholder test applies to it exactly as it does to
                    # S3 — otherwise every local-stack connection string in the repository
                    # is a finding and S2's output stops being read.
                    if name == "dsn-inline-password" and PLACEHOLDER.search(tok):
                        continue
                    bad("S2", f"{path}:{lineno}: {name} — {mask(tok)}")

            # S3
            for m in GENERIC.finditer(line):
                key, val = m.group(1), m.group(2)
                if PLACEHOLDER.search(val) or allowed(val, path):
                    continue
                # A value with no entropy at all (one repeated character, a pure word) is
                # a placeholder someone did not spell in the obvious way.
                if len(set(val)) < 5:
                    continue
                kind = "WARN-S3" if is_test else "S3"
                bad(kind, f"{path}:{lineno}: {key} assigned a literal — {mask(val)}"
                          + ("  [test fixture: warning only]" if is_test else ""))

            # S4
            for m in PAN.finditer(line):
                digits = re.sub(r"\D", "", m.group(0))
                if not (13 <= len(digits) <= 19) or not luhn(digits):
                    continue
                if allowed(digits, path) or allowed(m.group(0), path):
                    continue
                bad("S4", f"{path}:{lineno}: Luhn-valid {len(digits)}-digit run — "
                          f"{mask(digits)} — a candidate PAN. §17 puts the PCI boundary at "
                          f"'no card number reaches this platform'; git history is the one "
                          f"place a leaked one cannot be deleted from. If it is a published "
                          f"test vector, add its hash to the allowlist with a reason "
                          f"(scripts/check-secrets.sh --hash <value>)")

for kind, msg in problems:
    print(f"{kind}\t{msg}")

print(f"COUNT\tfiles_scanned={scanned} allowlist_hashes={len(allow_hashes)} "
      f"allowlist_paths={len(allow_paths)}", file=sys.stderr)
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
    ok "S1 no private key material"
    ok "S2 no provider credential"
    ok "S3 no secret-looking assignment to a live-looking literal"
    ok "S4 no Luhn-valid digit run outside the allowlist"
    ok "S5 the allowlist is well-formed"
    ;;
  2) emit_report; ok "S1/S2/S4 clean; S3 raised warnings in test fixtures only" ;;
  1) emit_report ;;
  *) die "the scan itself failed (exit $RC)" ;;
esac

summary "check-secrets"
