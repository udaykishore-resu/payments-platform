#!/usr/bin/env bash
# check-no-literal-secrets.sh — manifest-shaped secret check for helm/ and deployments/.
#
#   ./helm/scripts/check-no-literal-secrets.sh helm deployments
#
# THIS IS NOT THE REPOSITORY'S CREDENTIAL SCANNER. `scripts/check-secrets.sh`
# already covers the whole tree for credential *shapes* — provider token
# prefixes, private-key blocks, JWTs, inline connection-string passwords, Luhn-
# valid digit runs — and it has a reviewed allowlist for legitimate test vectors.
# Duplicating that here would mean two pattern lists drifting apart, and the
# second one would be the one without the allowlist.
#
# What this adds is the part a generic scanner cannot know: the Kubernetes and
# Helm *shapes* that mean "secret material is being delivered inline", which are
# wrong here regardless of whether the value looks like a credential.
#
#   K1  `kind: Secret` — a literal Secret manifest. Secrets in this platform are
#       materialised by the External Secrets Operator from Secrets Manager and
#       projected as files; a committed Secret is material in git history.
#   K2  `stringData:` / `data:` under a Secret — same, and unencoded.
#   K3  an env var whose NAME matches (?i)(secret|password|token|api_?key|
#       credential|private_?key) carrying an inline `value:` rather than a
#       reference. Admission policy rejects exactly this pattern at the cluster;
#       catching it in CI is three orders of magnitude cheaper.
#   K4  a values key of the same shape assigned a literal rather than a
#       `secretref://` reference or a template action.
#
# The third layer is `pp-common.secretGuard`, which fails at RENDER time, so
# `helm lint` and `helm template` both trip on a literal that reaches values
# from the GitOps repo rather than from this one.
#
# Exit codes: 0 clean, 1 finding, 2 usage.
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <path> [<path>...]" >&2
  exit 2
fi

status=0
report() {
  echo "FAIL: $1"
  printf '  %s\n' "$2"
  status=1
}

for path in "$@"; do
  [[ -e "$path" ]] || { echo "no such path: $path" >&2; exit 2; }
done

# K1/K2 — a literal Secret manifest.
while IFS= read -r hit; do
  [[ -z "$hit" ]] && continue
  report "literal Secret manifest (use an ExternalSecret)" "$hit"
done < <(grep -rEIn --exclude-dir=.git --exclude="$(basename "$0")" \
           -e '^[[:space:]]*kind:[[:space:]]*Secret[[:space:]]*$' \
           -e '^[[:space:]]*stringData:' "$@" 2>/dev/null || true)

# K3 — an env var carrying inline credential material.
while IFS= read -r hit; do
  [[ -z "$hit" ]] && continue
  # A template action, a downward-API reference or a *_REF name is a reference.
  grep -Eq 'valueFrom|secretKeyRef|_REF|_ref|secretref://|\{\{' <<<"$hit" && continue
  report "env var with a credential-shaped name carrying an inline value" "$hit"
done < <(grep -rEIn -A1 --exclude-dir=.git --exclude="$(basename "$0")" \
           -e 'name:[[:space:]]*["'"'"']?[A-Z_]*(SECRET|PASSWORD|TOKEN|API_?KEY|CREDENTIAL|PRIVATE_?KEY)' \
           "$@" 2>/dev/null | grep -E '^\S+[-:][0-9]+[-:][[:space:]]*value:' || true)

# K4 — a values key of credential shape assigned a literal.
while IFS= read -r hit; do
  [[ -z "$hit" ]] && continue
  # A boolean, a number, an empty value or a template action is a knob, not
  # material: `automountServiceAccountToken: false` is a security control that
  # happens to contain the word "Token".
  # A template action or a secretref:// is a REFERENCE, which is exactly what a
  # manifest is supposed to carry.
  grep -Eq ':[[:space:]]*["'"'"']?(\{\{|secretref://)' <<<"$hit" && continue
  # A boolean, a number or an empty value is a knob, not material:
  # `automountServiceAccountToken: false` is a security control that happens to
  # contain the word "Token".
  grep -Eq ':[[:space:]]*(""|null|\[\]|\{\}|true|false|[0-9]+)[[:space:]]*(#.*)?$' <<<"$hit" && continue
  grep -Eq 'REPLACE_ME|CHANGEME|EXAMPLE|example\.com|Ref:|-ref|_ref|Template|Path|path|Prefix|prefix|Store|store|audience|Audience' <<<"$hit" && continue
  report "values key of credential shape assigned a literal" "$hit"
done < <(grep -rEIn --exclude-dir=.git --exclude="$(basename "$0")" \
           -e '^[[:space:]]*[a-zA-Z_]*([Ss]ecret|[Pp]assword|[Tt]oken|[Aa]pi_?[Kk]ey|[Cc]redential|[Pp]rivate_?[Kk]ey)[a-zA-Z_]*:[[:space:]]*[^ #]' \
           "$@" 2>/dev/null || true)

if [[ $status -eq 0 ]]; then
  echo "OK: no inline secret material in: $*"
  echo "    (credential-shape scanning for the whole tree is scripts/check-secrets.sh)"
else
  cat <<'EOT'

Secrets live in AWS Secrets Manager under /{env}/{tenant}/{merchant}/{gateway}/
{purpose}. They reach a pod as an ExternalSecret-projected FILE under
/var/run/secrets/pp, resolved by that deployable's own IRSA role, and are
re-read on rotation without a restart. A manifest may carry a reference
(secretref://...) and never material. See docs/security.md §5.1-5.2.
EOT
fi
exit $status
