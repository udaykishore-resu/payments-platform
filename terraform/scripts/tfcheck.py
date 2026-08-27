#!/usr/bin/env python3
"""
tfcheck.py - structural verification of this Terraform tree without Terraform.

`terraform validate` is the right tool. When the binary is unavailable (an
air-gapped runner, a sandbox with no route to releases.hashicorp.com), this
script covers the failure modes that validate would have caught:

  1. HCL2 syntax           - every .tf file is parsed with a real HCL2 parser
                             (python-hcl2 / lark). A brace imbalance, an
                             unterminated string or a malformed block fails here.
  2. Duplicate addresses   - two `resource "type" "name"` blocks with the same
                             address in one module, or two variables/outputs/
                             module calls with the same name.
  3. Undeclared variables  - every `var.X` in a module resolves to a `variable
                             "X"` declared in that same module directory.
  4. Undefined locals      - every `local.X` resolves to a key in a `locals`
                             block in that directory.
  5. Resource wiring       - every `aws_*.name` and `data.aws_*.name` reference
                             resolves to a block declared in the same module.
  6. Module wiring         - every `module.Y` referenced is actually called in
                             that directory; every `module.Y.Z` names an output
                             `Z` that the called module actually declares.
  7. Required inputs       - every module call passes every variable that has
                             no default, and passes no variable the module does
                             not declare.
  8. Provider aliases      - every `provider = aws.X` / providers = { aws = aws.X }
                             names an alias declared in that stack.

Exit status is non-zero if any check fails.
"""

from __future__ import annotations

import json
import os
import re
import sys
from collections import defaultdict
from pathlib import Path

try:
    import hcl2
except ImportError:  # pragma: no cover
    print("python-hcl2 is required: pip install python-hcl2", file=sys.stderr)
    raise SystemExit(2)

ROOT = Path(__file__).resolve().parent.parent

# Identifiers that look like `var.` / `local.` / `module.` references but are
# produced by the language itself rather than declared by us.
BUILTIN_PREFIXES = ("each.", "count.", "self.", "path.", "terraform.")

failures: list[str] = []
notes: list[str] = []


def fail(msg: str) -> None:
    failures.append(msg)


def rel(p: Path) -> str:
    return str(p.relative_to(ROOT))


# ---------------------------------------------------------------------------
# 1. Parse every file
# ---------------------------------------------------------------------------

def tf_files(d: Path) -> list[Path]:
    return sorted(p for p in d.glob("*.tf"))


def all_dirs() -> list[Path]:
    dirs = []
    for base in ("modules", "envs"):
        for p in sorted((ROOT / base).iterdir()):
            if p.is_dir() and tf_files(p):
                dirs.append(p)
    if tf_files(ROOT):
        dirs.append(ROOT)
    return dirs


parsed: dict[Path, dict] = {}
raw: dict[Path, str] = {}


def parse_all() -> None:
    for d in all_dirs():
        for f in tf_files(d):
            text = f.read_text()
            raw[f] = text
            # Brace/bracket/paren balance, ignoring comments, strings and
            # heredocs - a cheap check that localises an imbalance to a file
            # before the parser's error does.
            check_balance(f, text)
            try:
                with f.open() as fh:
                    parsed[f] = hcl2.load(fh)
            except Exception as exc:  # noqa: BLE001
                fail(f"[syntax] {rel(f)}: {type(exc).__name__}: {str(exc)[:400]}")


def strip_noise(text: str) -> str:
    """Remove comments, quoted strings and heredoc bodies."""
    out = []
    i = 0
    n = len(text)
    while i < n:
        c = text[i]
        if c == "#" or (c == "/" and i + 1 < n and text[i + 1] == "/"):
            j = text.find("\n", i)
            i = n if j < 0 else j
            continue
        if c == "/" and i + 1 < n and text[i + 1] == "*":
            j = text.find("*/", i + 2)
            i = n if j < 0 else j + 2
            continue
        if c == "<" and text.startswith("<<", i):
            m = re.match(r"<<[-~]?([A-Za-z_][A-Za-z0-9_]*)", text[i:])
            if m:
                tag = m.group(1)
                end = re.search(rf"^\s*{re.escape(tag)}\s*$", text[i:], re.MULTILINE)
                if end:
                    i = i + end.end()
                    continue
        if c == '"':
            i += 1
            while i < n:
                if text[i] == "\\":
                    i += 2
                    continue
                if text[i] == '"':
                    i += 1
                    break
                i += 1
            continue
        out.append(c)
        i += 1
    return "".join(out)


def check_balance(f: Path, text: str) -> None:
    cleaned = strip_noise(text)
    pairs = {"}": "{", "]": "[", ")": "("}
    stack: list[str] = []
    for ch in cleaned:
        if ch in "{[(":
            stack.append(ch)
        elif ch in pairs:
            if not stack or stack[-1] != pairs[ch]:
                fail(f"[balance] {rel(f)}: unbalanced '{ch}'")
                return
            stack.pop()
    if stack:
        fail(f"[balance] {rel(f)}: {len(stack)} unclosed {stack[-1]!r}")


# ---------------------------------------------------------------------------
# Helpers over the parsed structure
# ---------------------------------------------------------------------------

def unq(s: str) -> str:
    """python-hcl2 keeps the quotes on block labels; strip them."""
    return s.strip().strip('"')


def blocks(doc: dict, kind: str) -> list:
    """Yield {label: body} dicts with labels unquoted and marker keys removed."""
    out = []
    for blk in doc.get(kind, []) or []:
        if not isinstance(blk, dict):
            continue
        out.append({unq(k): v for k, v in blk.items() if k != "__is_block__"})
    return out


def body_keys(body) -> set:
    if not isinstance(body, dict):
        return set()
    return {k for k in body if not k.startswith("__")}


def dir_docs(d: Path) -> list[tuple[Path, dict]]:
    return [(f, parsed[f]) for f in tf_files(d) if f in parsed]


def declared_variables(d: Path) -> dict[str, bool]:
    """name -> has_default"""
    out: dict[str, bool] = {}
    for f, doc in dir_docs(d):
        for blk in blocks(doc, "variable"):
            for name, body in blk.items():
                out[name] = "default" in body_keys(body)
    return out


def declared_outputs(d: Path) -> set[str]:
    out: set[str] = set()
    for f, doc in dir_docs(d):
        for blk in blocks(doc, "output"):
            out.update(blk.keys())
    return out


def declared_locals(d: Path) -> set[str]:
    out: set[str] = set()
    for f, doc in dir_docs(d):
        for blk in blocks(doc, "locals"):
            out.update(blk.keys())
    return out


def module_calls(d: Path) -> dict[str, dict]:
    out: dict[str, dict] = {}
    for f, doc in dir_docs(d):
        for blk in blocks(doc, "module"):
            for name, body in blk.items():
                if name in out:
                    fail(f"[duplicate] {rel(d)}: module {name!r} declared twice")
                out[name] = body or {}
    return out


def provider_aliases(d: Path) -> set[str]:
    aliases: set[str] = set()
    for f, doc in dir_docs(d):
        for blk in blocks(doc, "provider"):
            for pname, body in blk.items():
                body = body if isinstance(body, dict) else {}
                alias = body.get("alias") if isinstance(body, dict) else None
                if isinstance(alias, list):
                    alias = alias[0] if alias else None
                if alias:
                    aliases.add(f"{unq(pname)}.{unq(str(alias))}")
    return aliases


# ---------------------------------------------------------------------------
# 2. Duplicate resource / data addresses
# ---------------------------------------------------------------------------

def check_duplicates() -> None:
    for d in all_dirs():
        seen: dict[str, str] = {}
        for kind in ("resource", "data"):
            for f, doc in dir_docs(d):
                for blk in blocks(doc, kind):
                    for rtype, inner in blk.items():
                        for rname in body_keys(inner):
                            addr = f"{kind}.{rtype}.{unq(rname)}"
                            if addr in seen:
                                fail(
                                    f"[duplicate] {rel(d)}: {addr} declared in both "
                                    f"{seen[addr]} and {rel(f)}"
                                )
                            else:
                                seen[addr] = rel(f)

        # variables and outputs
        for kind in ("variable", "output"):
            names: dict[str, str] = {}
            for f, doc in dir_docs(d):
                for blk in blocks(doc, kind):
                    for name in blk:
                        if name in names:
                            fail(
                                f"[duplicate] {rel(d)}: {kind} {name!r} declared in both "
                                f"{names[name]} and {rel(f)}"
                            )
                        else:
                            names[name] = rel(f)


# ---------------------------------------------------------------------------
# 3-5. Reference resolution
# ---------------------------------------------------------------------------

VAR_RE = re.compile(r"\bvar\.([A-Za-z_][A-Za-z0-9_]*)")
LOCAL_RE = re.compile(r"\blocal\.([A-Za-z_][A-Za-z0-9_]*)")
MOD_RE = re.compile(r"\bmodule\.([A-Za-z_][A-Za-z0-9_-]*)\.([A-Za-z_][A-Za-z0-9_]*)")
MOD_BARE_RE = re.compile(r"\bmodule\.([A-Za-z_][A-Za-z0-9_-]*)\b")


def reference_text(d: Path) -> str:
    """Concatenate every .tf in the directory with comments stripped."""
    chunks = []
    for f in tf_files(d):
        text = raw.get(f, "")
        # Strip line comments only; keep strings, because references live inside
        # interpolations within strings.
        text = re.sub(r"(?m)^\s*#.*$", "", text)
        text = re.sub(r"(?m)^\s*//.*$", "", text)
        text = re.sub(r"(?m)\s#[^\n\"]*$", "", text)
        chunks.append(text)
    return "\n".join(chunks)


def check_references() -> None:
    for d in all_dirs():
        text = reference_text(d)
        vars_declared = declared_variables(d)
        locals_declared = declared_locals(d)
        mods = module_calls(d)

        for name in sorted(set(VAR_RE.findall(text))):
            if name not in vars_declared:
                fail(f"[undeclared-var] {rel(d)}: var.{name} is referenced but not declared")

        for name in sorted(set(LOCAL_RE.findall(text))):
            if name not in locals_declared:
                fail(f"[undefined-local] {rel(d)}: local.{name} is referenced but not defined")

        for mname in sorted(set(MOD_BARE_RE.findall(text))):
            if mname not in mods:
                fail(f"[unknown-module] {rel(d)}: module.{mname} is referenced but not called")

        for mname, oname in sorted(set(MOD_RE.findall(text))):
            if mname not in mods:
                continue  # already reported
            src = mods[mname].get("source")
            if isinstance(src, list):
                src = src[0] if src else None
            if not src or not str(src).startswith("."):
                continue
            target = (d / str(src)).resolve()
            if not target.exists():
                fail(f"[bad-source] {rel(d)}: module {mname!r} source {src} does not exist")
                continue
            outs = declared_outputs(target)
            if oname not in outs:
                fail(
                    f"[missing-output] {rel(d)}: module.{mname}.{oname} - "
                    f"{rel(target)} declares no output named {oname!r}"
                )


# ---------------------------------------------------------------------------
# 5b. Resource and data-source reference resolution
#
# The failure this catches is the one `terraform validate` catches loudest and
# a human reviewer catches least often: a reference to `aws_iam_role.foo` in one
# file when the role is actually declared as `aws_iam_role.platform["foo"]` in
# another.
# ---------------------------------------------------------------------------

DATA_REF_RE = re.compile(r"\bdata\.([a-z][a-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)")
RES_REF_RE = re.compile(r"(?<![\w.])((?:aws|tls|random|null)_[a-z0-9_]+)\.([A-Za-z_][A-Za-z0-9_]*)")


def declared_addresses(d: Path, kind: str) -> set[str]:
    out: set[str] = set()
    for f, doc in dir_docs(d):
        for blk in blocks(doc, kind):
            for rtype, inner in blk.items():
                for rname in body_keys(inner):
                    out.add(f"{rtype}.{unq(rname)}")
    return out


def check_resource_references() -> None:
    for d in all_dirs():
        text = reference_text(d)
        res = declared_addresses(d, "resource")
        dat = declared_addresses(d, "data")

        for rtype, rname in sorted(set(DATA_REF_RE.findall(text))):
            addr = f"{rtype}.{rname}"
            if addr not in dat:
                fail(f"[unknown-data] {rel(d)}: data.{addr} is referenced but not declared")

        # Strip `data.` prefixed matches before checking managed resources.
        stripped = DATA_REF_RE.sub(" ", text)
        for rtype, rname in sorted(set(RES_REF_RE.findall(stripped))):
            addr = f"{rtype}.{rname}"
            if addr in res:
                continue
            # A reference inside a resource block header, e.g. the label of the
            # block itself, never appears unquoted, so anything left is a real
            # reference to something that does not exist.
            fail(f"[unknown-resource] {rel(d)}: {addr} is referenced but not declared")


# ---------------------------------------------------------------------------
# 6. Module call completeness
# ---------------------------------------------------------------------------

META_ARGS = {"source", "version", "providers", "depends_on", "count", "for_each", "lifecycle"}


def check_module_inputs() -> None:
    for d in all_dirs():
        for mname, body in module_calls(d).items():
            src = body.get("source")
            if isinstance(src, list):
                src = src[0] if src else None
            if not src or not str(src).startswith("."):
                continue
            target = (d / str(src)).resolve()
            if not target.exists():
                continue
            declared = declared_variables(target)
            passed = {k for k in body_keys(body) if k not in META_ARGS}

            missing = [n for n, has_default in declared.items() if not has_default and n not in passed]
            for n in sorted(missing):
                fail(
                    f"[missing-input] {rel(d)}: module {mname!r} ({rel(target)}) "
                    f"requires variable {n!r} which has no default and is not passed"
                )

            extra = sorted(passed - set(declared))
            for n in extra:
                fail(
                    f"[unknown-input] {rel(d)}: module {mname!r} passes {n!r}, "
                    f"which {rel(target)} does not declare"
                )


# ---------------------------------------------------------------------------
# 7. Provider aliases
# ---------------------------------------------------------------------------

ALIAS_RE = re.compile(r"\baws\s*=\s*(aws\.[A-Za-z_][A-Za-z0-9_]*)")
PROVIDER_ATTR_RE = re.compile(r"^\s*provider\s*=\s*(aws\.[A-Za-z_][A-Za-z0-9_]*)", re.MULTILINE)


def check_provider_aliases() -> None:
    for d in all_dirs():
        if d.parent.name != "envs":
            continue  # modules inherit providers; aliases are declared by the stack
        aliases = provider_aliases(d)
        text = reference_text(d)
        used = set(ALIAS_RE.findall(text)) | set(PROVIDER_ATTR_RE.findall(text))
        for a in sorted(used):
            if a not in aliases:
                fail(f"[unknown-provider-alias] {rel(d)}: {a} is used but not declared")


# ---------------------------------------------------------------------------
# Inventory
# ---------------------------------------------------------------------------

def inventory() -> dict:
    counts = defaultdict(int)
    per_dir = {}
    for d in all_dirs():
        n_res = n_data = n_mod = n_var = n_out = 0
        for f, doc in dir_docs(d):
            for blk in blocks(doc, "resource"):
                for rtype, inner in blk.items():
                    k = len(body_keys(inner))
                    n_res += k
                    counts[rtype] += k
            for blk in blocks(doc, "data"):
                for rtype, inner in blk.items():
                    n_data += len(body_keys(inner))
            n_mod += sum(len(b) for b in blocks(doc, "module"))
            n_var += sum(len(b) for b in blocks(doc, "variable"))
            n_out += sum(len(b) for b in blocks(doc, "output"))
        per_dir[rel(d)] = {
            "resources": n_res,
            "data_sources": n_data,
            "module_calls": n_mod,
            "variables": n_var,
            "outputs": n_out,
            "files": len(tf_files(d)),
        }
    return {"per_dir": per_dir, "resource_types": dict(sorted(counts.items()))}


def main() -> int:
    parse_all()
    check_duplicates()
    check_references()
    check_resource_references()
    check_module_inputs()
    check_provider_aliases()

    inv = inventory()
    total_res = sum(v["resources"] for v in inv["per_dir"].values())
    total_data = sum(v["data_sources"] for v in inv["per_dir"].values())
    total_files = sum(v["files"] for v in inv["per_dir"].values())
    total_vars = sum(v["variables"] for v in inv["per_dir"].values())
    total_outs = sum(v["outputs"] for v in inv["per_dir"].values())
    total_mods = sum(v["module_calls"] for v in inv["per_dir"].values())

    if os.environ.get("TFCHECK_JSON"):
        print(json.dumps({"inventory": inv, "failures": failures}, indent=2))
    else:
        print("=" * 72)
        print("tfcheck - structural verification")
        print("=" * 72)
        for name, v in inv["per_dir"].items():
            print(
                f"{name:<34} files={v['files']:<3} res={v['resources']:<4} "
                f"data={v['data_sources']:<3} mods={v['module_calls']:<3} "
                f"vars={v['variables']:<3} outs={v['outputs']}"
            )
        print("-" * 72)
        print(
            f"TOTAL  files={total_files}  resource blocks={total_res}  "
            f"data sources={total_data}  module calls={total_mods}  "
            f"variables={total_vars}  outputs={total_outs}"
        )
        print("-" * 72)

    if failures:
        print(f"\nFAILED: {len(failures)} problem(s)\n")
        for f in failures:
            print("  " + f)
        return 1

    print("\nOK: HCL2 parses, no duplicate addresses, every var/local/module "
          "reference resolves,\n    every module call passes its required "
          "inputs and no unknown ones,\n    every provider alias is declared.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
