#!/usr/bin/env python3
"""
tfalign.py - the one transformation `terraform fmt` makes that a human notices.

`terraform fmt` does several things; the only one that produces diff noise in
review is the alignment of `=` across consecutive attribute lines at the same
indentation. This script applies exactly that rule and nothing else, so that a
tree formatted here is byte-identical to a `terraform fmt` tree for this class
of change.

It is deliberately conservative:
  * only lines matching `<indent><identifier> = <value>` participate
  * a run is broken by a blank line, a comment line, a brace, or a change of
    indentation - which is what fmt does
  * heredoc bodies are skipped entirely
  * nothing else about the file is touched

Usage:
  tfalign.py --check    exit 1 and list files that would change
  tfalign.py --write    rewrite the files
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

ATTR = re.compile(r"^(\s*)([A-Za-z_][A-Za-z0-9_-]*)(\s*)=(\s*)(\S.*)$")
HEREDOC_START = re.compile(r"<<[-~]?([A-Za-z_][A-Za-z0-9_]*)\s*$")


def bracket_delta(value: str) -> int:
    """Net bracket depth of an attribute's value, ignoring strings and comments.

    A value that leaves brackets open spans several lines. terraform fmt breaks
    the alignment run at such a line - `type = object({` is not aligned with the
    `description` above it - so this function is what keeps the output identical
    to fmt's.
    """
    depth = 0
    i = 0
    n = len(value)
    while i < n:
        c = value[i]
        if c == "#" or (c == "/" and i + 1 < n and value[i + 1] == "/"):
            break
        if c == '"':
            i += 1
            while i < n:
                if value[i] == "\\":
                    i += 2
                    continue
                if value[i] == '"':
                    break
                i += 1
        elif c in "{[(":
            depth += 1
        elif c in "}])":
            depth -= 1
        i += 1
    return depth


def align(text: str) -> str:
    lines = text.split("\n")
    out: list[str] = []
    run: list[tuple[int, str, str, str]] = []  # (index, indent, name, value)
    heredoc_tag: str | None = None

    def flush() -> None:
        if not run:
            return
        width = max(len(name) for _, _, name, _ in run)
        for idx, indent, name, value in run:
            out[idx] = f"{indent}{name.ljust(width)} = {value}"
        run.clear()

    for line in lines:
        out.append(line)
        i = len(out) - 1

        if heredoc_tag is not None:
            if line.strip() == heredoc_tag:
                heredoc_tag = None
            continue

        m = ATTR.match(line)
        if m:
            indent, name, _, _, value = m.groups()
            hd = HEREDOC_START.search(value)
            if hd:
                flush()
                heredoc_tag = hd.group(1)
                continue
            if bracket_delta(value) != 0:
                # A multi-line value. Break the run and normalise this line to a
                # single space, exactly as terraform fmt does.
                flush()
                out[i] = f"{indent}{name} = {value}"
                continue
            if run and run[-1][1] != indent:
                flush()
            run.append((i, indent, name, value))
        else:
            flush()

    flush()
    return "\n".join(out)


def main() -> int:
    mode = sys.argv[1] if len(sys.argv) > 1 else "--check"
    changed: list[str] = []

    for f in sorted(ROOT.rglob("*.tf")) + sorted(ROOT.rglob("*.tfvars")):
        original = f.read_text()
        formatted = align(original)
        if formatted != original:
            changed.append(str(f.relative_to(ROOT)))
            if mode == "--write":
                f.write_text(formatted)

    if not changed:
        print("tfalign: all files aligned")
        return 0

    if mode == "--write":
        print(f"tfalign: rewrote {len(changed)} file(s)")
        for c in changed:
            print("  " + c)
        return 0

    print(f"tfalign: {len(changed)} file(s) would change")
    for c in changed:
        print("  " + c)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
