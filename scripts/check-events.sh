#!/usr/bin/env bash
#
# scripts/check-events.sh — the event registry and the published JSON Schemas agree.
#
# WHAT IT ENFORCES
#   E1  every type in the Go registry (internal/events) has a schema file in api/events/;
#   E2  every schema file in api/events/ has a registry entry (no orphan contract);
#   E3  every schema is itself a valid JSON Schema (2020-12 unless it says otherwise);
#   E4  every schema's `examples` validate against that schema;
#   E5  the schema's `x-topic` and `x-partition-key` match the registry's Topic and
#       PartitionKeyField;
#   E6  the schema `$id` and `title` carry the same versioned type name as the registry;
#   E7  every schema sets `additionalProperties: false` and a non-empty `required` list.
#
# WHY
#   §13.1 makes the envelope's `dataschema` a resolvable URI and the platform's
#   compatibility promise additive-within-a-major. Both promises are only worth something
#   if the schema a consumer fetches is the schema the producer actually satisfies. Three
#   ways that breaks, all caught here: a type registered with no schema (the URI 404s at
#   the consumer), a schema with no producer (a consumer builds against a contract nobody
#   emits), and a schema whose own examples do not validate — which means the schema and
#   the intended payload disagree and one of them is wrong.
#
#   E4 is the one that catches real bugs. An example is the only executable statement of
#   intent a schema carries. A `required` entry added without updating the examples, a
#   pattern tightened by one character, a `money` $ref that no longer resolves: all of
#   them show up as an example that stops validating, and none of them show up any other
#   way until a consumer's deserializer throws in production.
#
#   E7 is a compatibility gate rather than a style rule. Without
#   `additionalProperties: false` a schema cannot detect a producer that started sending
#   an undeclared field, which is precisely the change §13.1 needs to see in order to
#   classify it as additive.
#
# USAGE
#   scripts/check-events.sh
#
# EXIT
#   0 clean · 1 divergence or an example that does not validate · 2 could not run.

set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

need go
need python3
cd "$REPO_ROOT"

SCHEMA_DIR="api/events"
[[ -d "$SCHEMA_DIR" ]] || die "missing $SCHEMA_DIR"

hdr "event contracts — internal/events registry ↔ ${SCHEMA_DIR}/"

REG="$(mktemp)"; REPORT="$(mktemp)"
trap 'rm -f "$REG" "$REPORT"' EXIT
go run ./scripts/specdump events > "$REG" || die "could not dump the event registry"

set +e
python3 - "$REG" "$SCHEMA_DIR" > "$REPORT" <<'PY'
import glob, json, os, re, sys

reg_path, schema_dir = sys.argv[1], sys.argv[2]

try:
    import jsonschema
    from jsonschema import Draft202012Validator, validators
except ImportError:
    print("SKIP\tpython3 jsonschema is not installed; schema validation was not run")
    sys.exit(3)

reg = json.load(open(reg_path))
by_type = {r["type"]: r for r in reg}

problems = []
def bad(kind, msg): problems.append((kind, msg))

# The envelope is a shared definition, not an event payload; it has no registry entry by
# design and is excluded from the orphan check rather than silently tolerated.
NON_EVENT_SCHEMAS = {"envelope.schema.json"}

files = {os.path.basename(p) for p in glob.glob(os.path.join(schema_dir, "*.schema.json"))}
event_files = files - NON_EVENT_SCHEMAS

# --- E1: registry -> schema --------------------------------------------------------------
for t, r in sorted(by_type.items()):
    want = r["schemaFile"] or f"{t}.schema.json"
    if want not in files:
        bad("E1", f"{t}: registry names schema {want} which does not exist in {schema_dir}/ "
                  f"— the envelope's dataschema URI will not resolve for a consumer")

# --- E2: schema -> registry --------------------------------------------------------------
expected = {(r["schemaFile"] or f"{r['type']}.schema.json") for r in reg}
for f in sorted(event_files - expected):
    bad("E2", f"{f}: no registry entry — an orphan contract that no producer emits")

# --- E3..E7 --------------------------------------------------------------------------------
for f in sorted(files):
    path = os.path.join(schema_dir, f)
    try:
        schema = json.load(open(path, encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        bad("E3", f"{f}: not parseable as JSON: {e}")
        continue

    # E3 — is this a legal schema at all?
    try:
        cls = validators.validator_for(schema, default=Draft202012Validator)
        cls.check_schema(schema)
    except jsonschema.exceptions.SchemaError as e:
        bad("E3", f"{f}: not a valid JSON Schema: {e.message}")
        continue

    if f in NON_EVENT_SCHEMAS:
        continue

    etype = f[: -len(".schema.json")]
    reg_entry = by_type.get(etype)

    # E6 — identity.
    title = schema.get("title", "")
    if title != etype:
        bad("E6", f"{f}: title is {title!r}, expected {etype!r} — the title is what a "
                  f"reader matches against the type in the envelope")
    sid = schema.get("$id", "")
    if sid and not sid.rstrip("/").endswith(f"{etype}.json"):
        bad("E6", f"{f}: $id {sid!r} does not end in {etype}.json")
    if reg_entry and reg_entry["schemaUri"] and sid and reg_entry["schemaUri"] != sid:
        bad("E6", f"{f}: $id {sid!r} != registry SchemaURI {reg_entry['schemaUri']!r} — "
                  f"a consumer resolving dataschema would fetch a different document")

    # E5 — routing metadata.
    if reg_entry:
        xt, xk = schema.get("x-topic"), schema.get("x-partition-key")
        if xt and xt != reg_entry["topic"]:
            bad("E5", f"{f}: x-topic {xt!r} != registry topic {reg_entry['topic']!r}")
        if xk and xk != reg_entry["partitionKey"]:
            bad("E5", f"{f}: x-partition-key {xk!r} != registry PartitionKeyField "
                      f"{reg_entry['partitionKey']!r} — changing the partition key "
                      f"silently destroys per-aggregate ordering (§13.3)")

    # E7 — compatibility posture.
    if schema.get("additionalProperties", None) is not False:
        bad("E7", f"{f}: additionalProperties is not false — an undeclared field cannot "
                  f"be detected, so the additive-only compatibility rule (§13.1) has "
                  f"nothing to check")
    if not schema.get("required"):
        bad("E7", f"{f}: no `required` list — every field optional means the schema "
                  f"asserts nothing")

    # E4 — the examples must validate.
    examples = schema.get("examples")
    if examples is None:
        bad("E4", f"{f}: no `examples` — an example is the only executable statement of "
                  f"intent a schema carries")
        continue
    if not isinstance(examples, list) or not examples:
        bad("E4", f"{f}: `examples` must be a non-empty array")
        continue
    validator = cls(schema)
    for i, ex in enumerate(examples):
        errs = sorted(validator.iter_errors(ex), key=lambda e: list(e.path))
        for e in errs[:5]:
            loc = "/".join(str(p) for p in e.absolute_path) or "(root)"
            bad("E4", f"{f}: examples[{i}] at {loc}: {e.message}")
        if len(errs) > 5:
            bad("E4", f"{f}: examples[{i}]: … and {len(errs) - 5} further error(s)")

for kind, msg in problems:
    print(f"{kind}\t{msg}")

print(f"COUNT\tregistered={len(by_type)} schemas={len(event_files)}", file=sys.stderr)
sys.exit(1 if problems else 0)
PY
RC=$?
set -e

case $RC in
  0)
    ok "E1 every registered type has a schema"
    ok "E2 every schema has a producer"
    ok "E3 every schema is a valid JSON Schema"
    ok "E4 every example validates against its schema"
    ok "E5 topic and partition key agree with the registry"
    ok "E6 \$id and title carry the versioned type name"
    ok "E7 every schema is closed and has required fields"
    ;;
  3)
    skip "python3 jsonschema unavailable — schemas were not validated"
    warn "install with: pip3 install --user jsonschema  (CI installs it in the validation job)"
    ;;
  1)
    while IFS=$'\t' read -r kind msg; do
      case "$kind" in
        SKIP)     skip "$msg" ;;
        ""|COUNT) : ;;
        *)        fail "[$kind] $msg" ;;
      esac
    done < "$REPORT"
    ;;
  *)
    die "the comparison itself failed (exit $RC)"
    ;;
esac

summary "check-events"
