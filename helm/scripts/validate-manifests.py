#!/usr/bin/env python3
"""
validate-manifests.py — offline validation of the Helm charts and the Kustomize /
Argo CD / Prometheus / Grafana manifests.

This exists because `helm` and `kubeconform` are not always available (an
air-gapped CI runner, a laptop, this repository's own container). Where they ARE
available they are strictly better and should be used instead:

    helm dependency build helm/payments-platform
    helm lint helm/payments-platform -f helm/payments-platform/values-prod.yaml
    helm template ... | kubeconform -strict -schema-location default \
        -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceAPIVersion}}/{{.ResourceKind}}.json'

What this script does instead, in four passes:

  (a) YAML/JSON parse of every non-template file, reporting parse errors.
  (b) Go-template action balance: every {{ }} closes, and every if/range/with/
      define is matched by an end.
  (c) Every `.Values.X` a template dereferences exists in that chart's
      values.yaml (references guarded by `| default` are treated as optional).
  (d) A STRUCTURAL RENDER of each template — helper includes expanded with real
      nindent/indent semantics, `toYaml` fed from the chart's real values, every
      other action replaced by a placeholder scalar, `if` branches taken and
      `range` bodies emitted once — followed by per-kind required-field
      assertions.

Pass (d) is not Helm. It cannot evaluate a conditional, so it validates the
shape of the union of branches rather than the shape of one specific render, and
a template whose correctness depends on a value it cannot see will pass here and
still need `helm template` to be sure. It does catch the errors that actually
happen: broken indentation under nindent, a missing required field, a selector
that does not match the pod labels, an alert without a runbook.

Exit codes: 0 clean, 1 findings.
"""
from __future__ import annotations

import json
import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    print("pyyaml is required: pip install pyyaml", file=sys.stderr)
    raise SystemExit(2)

ROOT = Path(__file__).resolve().parents[2]
HELM = ROOT / "helm"
DEPLOY = ROOT / "deployments"

findings: list[str] = []
checked = {"yaml": 0, "templates": 0, "docs": 0, "values_refs": 0}


def fail(where, msg):
    findings.append(f"{where}: {msg}")


# ===========================================================================
# (b) Go-template action balance
# ===========================================================================
ACTION_RE = re.compile(r"\{\{-?(.*?)-?\}\}", re.S)
COMMENT_RE = re.compile(r"\{\{-?\s*/\*.*?\*/\s*-?\}\}", re.S)
OPENERS = ("if ", "if(", "range ", "range(", "with ", "define ", "block ")


def check_balance(path: Path, text: str) -> None:
    # Delimiter balance first: an unclosed {{ swallows the rest of the file and
    # produces a confusing error much later.
    if text.count("{{") != text.count("}}"):
        fail(path, f"unbalanced delimiters: {text.count('{{')} '{{{{' vs {text.count('}}')} '}}}}'")
        return

    stripped = COMMENT_RE.sub("", text)
    stack = []
    for m in ACTION_RE.finditer(stripped):
        body = m.group(1).strip()
        line = stripped[: m.start()].count("\n") + 1
        if body.startswith(OPENERS):
            stack.append((body.split()[0], line))
        elif body == "end":
            if not stack:
                fail(path, f"line {line}: 'end' with no open if/range/with/define")
            else:
                stack.pop()
        elif body.startswith("else"):
            if not stack:
                fail(path, f"line {line}: 'else' outside a conditional")
    for kind, line in stack:
        fail(path, f"line {line}: '{kind}' is never closed by 'end'")


# ===========================================================================
# (c) .Values.X exists in values.yaml
# ===========================================================================
VALUES_REF_RE = re.compile(r"(?:\$root|\$\.|\$|\.)Values((?:\.[A-Za-z_][A-Za-z0-9_]*)+)")


def values_has(values: dict, path: str) -> bool:
    node = values
    for part in path.strip(".").split("."):
        if not isinstance(node, dict) or part not in node:
            return False
        node = node[part]
    return True


def check_values_refs(path: Path, text: str, values: dict, chart: str) -> None:
    stripped = COMMENT_RE.sub("", text)
    for m in ACTION_RE.finditer(stripped):
        body = m.group(1)
        line = stripped[: m.start()].count("\n") + 1
        optional = "| default" in body or "|default" in body or body.strip().startswith("with ")
        for ref in VALUES_REF_RE.finditer(body):
            dotted = ref.group(1)
            checked["values_refs"] += 1
            if values_has(values, dotted):
                continue
            # A prefix that exists but bottoms out in a list/scalar is a
            # reference into rendered data, not a missing key.
            parts = dotted.strip(".").split(".")
            node, ok = values, True
            for part in parts:
                if isinstance(node, dict) and part in node:
                    node = node[part]
                else:
                    ok = False
                    break
            if ok or optional:
                continue
            fail(path, f"line {line}: .Values{dotted} is not defined in {chart}/values.yaml")


# ===========================================================================
# (d) structural render
# ===========================================================================
DEFINE_RE = re.compile(r"\{\{-?\s*define\s+\"([^\"]+)\"\s*-?\}\}(.*?)\{\{-?\s*end\s*-?\}\}", re.S)
INCLUDE_RE = re.compile(r'include\s+"([^"]+)"')
NINDENT_RE = re.compile(r"nindent\s+(\d+)")
INDENT_RE = re.compile(r"\bindent\s+(\d+)")
TOYAML_RE = re.compile(r"toYaml\s+(?:\$root|\$|\.)Values((?:\.[A-Za-z_][A-Za-z0-9_]*)+)")


DEFINE_OPEN_RE = re.compile(r"\{\{-?\s*define\s+\"([^\"]+)\"\s*-?\}\}")


def extract_defines(text: str) -> dict[str, str]:
    """Extract {{ define }} bodies, honouring nesting.

    A naive non-greedy regex stops at the FIRST nested {{ end }}, silently
    truncating every helper that contains a conditional — which is most of them,
    and which produces confident-looking but wrong validation output.
    """
    out: dict[str, str] = {}
    for m in DEFINE_OPEN_RE.finditer(text):
        name = m.group(1)
        depth, pos, body_start = 1, m.end(), m.end()
        for a in ACTION_RE.finditer(text, pos):
            act = a.group(1).strip()
            if act.startswith(OPENERS):
                depth += 1
            elif act == "end":
                depth -= 1
                if depth == 0:
                    out[name] = text[body_start:a.start()]
                    break
    return out


def load_helpers(chart_dir: Path) -> dict[str, str]:
    helpers: dict[str, str] = {}
    sources = list((chart_dir / "templates").glob("_*.tpl"))
    common = HELM / "charts" / "pp-common" / "templates"
    if common.exists():
        sources += list(common.glob("_*.tpl"))
    for src in sources:
        text = COMMENT_RE.sub("", src.read_text())
        helpers.update(extract_defines(text))
    return helpers


def indent_block(text: str, n: int) -> str:
    pad = " " * n
    return "\n".join(pad + ln if ln.strip() else ln for ln in text.strip("\n").split("\n"))


SIMPLE_PATH_RE = re.compile(r"^(?:\$root|\$|\.)Values((?:\.[A-Za-z_][A-Za-z0-9_]*)+)$")
SCALAR_ACT_RE = re.compile(
    r"^(?:\$root|\$|\.)Values((?:\.[A-Za-z_][A-Za-z0-9_]*)+)"
    r"(?:\s*\|\s*(quote|toString|int|default\s+\S+))?$")


def resolve_scalar(act: str, values: dict):
    """Resolve `{{ .Values.a.b }}` (optionally | quote) to its literal value.

    Substituting the real value rather than a placeholder is what lets the
    per-kind assertions check what the manifest actually says — that
    readOnlyRootFilesystem is true, that liveness and readiness use different
    paths — instead of merely checking that a field exists.
    """
    m = SCALAR_ACT_RE.match(act.strip())
    if not m:
        return None
    ok, val = _lookup(values, m.group(1))
    if not ok or isinstance(val, (dict, list)):
        return None
    if val is None:
        return None
    if m.group(2) == "quote":
        return f'"{val}"'
    if isinstance(val, bool):
        return "true" if val else "false"
    return str(val)


def _lookup(values: dict, dotted: str):
    node = values
    for part in dotted.strip(".").split("."):
        if isinstance(node, dict) and part in node:
            node = node[part]
        else:
            return (False, None)
    return (True, node)


def _tokenize(expr: str):
    """Split a Go-template condition into a prefix-form token tree."""
    toks, buf, depth, quote = [], "", 0, None
    for ch in expr:
        if quote:
            buf += ch
            if ch == quote:
                quote = None
            continue
        if ch in "\"'":
            quote = ch
            buf += ch
            continue
        if ch == "(":
            if depth:
                buf += ch
            depth += 1
            continue
        if ch == ")":
            depth -= 1
            if depth:
                buf += ch
            else:
                toks.append(_tokenize(buf))
                buf = ""
            continue
        if depth:
            buf += ch
            continue
        if ch.isspace():
            if buf:
                toks.append(buf)
                buf = ""
            continue
        buf += ch
    if buf:
        toks.append(buf)
    return toks


def _eval_tokens(tree, values):
    """Best-effort truthiness. None means 'cannot decide'.

    When it cannot decide, the caller takes the branch: validating the union of
    branches is the conservative direction for a structural check.
    """
    if isinstance(tree, str):
        m = SIMPLE_PATH_RE.match(tree.strip())
        if not m:
            return None
        ok, val = _lookup(values, m.group(1))
        if not ok:
            return None
        return bool(val) and val != 0
    if not tree:
        return None
    if len(tree) == 1:
        return _eval_tokens(tree[0], values)
    head = tree[0] if isinstance(tree[0], str) else None
    rest = tree[1:]
    if head in ("and", "or"):
        parts = [_eval_tokens(t, values) for t in rest]
        if head == "and":
            if any(p is False for p in parts):
                return False
            return None if any(p is None for p in parts) else True
        if any(p is True for p in parts):
            return True
        return None if any(p is None for p in parts) else False
    if head == "not" and len(rest) == 1:
        inner = _eval_tokens(rest[0], values)
        return None if inner is None else not inner
    if head in ("eq", "ne") and len(rest) == 2 and isinstance(rest[0], str) and isinstance(rest[1], str):
        m = SIMPLE_PATH_RE.match(rest[0].strip())
        if not m:
            return None
        ok, val = _lookup(values, m.group(1))
        if not ok:
            return None
        want = rest[1].strip().strip("\"'")
        return (str(val) == want) if head == "eq" else (str(val) != want)
    return None


def eval_cond(expr: str, values: dict):
    return _eval_tokens(_tokenize(expr.strip()), values)


def render(text: str, helpers: dict[str, str], values: dict, depth: int = 0,
           drop_solo: bool = True) -> str:
    """Conservative structural render. Not Helm; see the module docstring."""
    if depth > 8:
        return ""
    text = COMMENT_RE.sub("", text)
    # Collapse multi-line actions so line-oriented control-flow handling works.
    text = ACTION_RE.sub(lambda m: "{{" + " ".join(m.group(1).split()) + "}}", text)

    out: list[str] = []
    # A stack of conditional frames. `emit` is whether this frame's current
    # branch is being kept; `satisfied` records that an earlier branch of the
    # same if/else chain was already taken, so a later `else` does not reopen it.
    # Getting this wrong (treating `else` as a simple toggle) silently emits both
    # branches of every helper that has one, which is exactly the bug this
    # structure exists to avoid.
    frames: list[dict] = []

    def emitting() -> bool:
        return all(f["emit"] for f in frames)

    for raw in text.split("\n"):
        solo = re.fullmatch(r"\s*\{\{(.*?)\}\}\s*", raw)
        body = solo.group(1).strip() if solo else None

        if body is not None and body.startswith(OPENERS):
            kw = body.split()[0]
            arg = body.split(" ", 1)[1] if " " in body else ""
            cond = eval_cond(arg, values) if kw in ("if", "with", "range") else None
            take = emitting() and cond is not False
            dot = None
            if kw == "range":
                mm = SIMPLE_PATH_RE.match(arg.strip())
                if mm:
                    ok, val = _lookup(values, mm.group(1))
                    # A range over a list of scalars is emitted once, bound to
                    # the first element, so `- {{ . }}` yields the real value.
                    if ok and isinstance(val, list) and val and not isinstance(val[0], (dict, list)):
                        dot = val[0]
            frames.append({"emit": take, "satisfied": take, "kw": kw, "dot": dot})
            continue
        if body == "end":
            if frames:
                frames.pop()
            continue
        if body is not None and body.startswith("else"):
            if frames:
                f = frames[-1]
                if f["satisfied"]:
                    f["emit"] = False
                else:
                    rest = body[len("else"):].strip()
                    cond = None
                    if rest.startswith("if"):
                        cond = eval_cond(rest[2:].strip(), values)
                    parent = all(x["emit"] for x in frames[:-1])
                    f["emit"] = parent and cond is not False
                    f["satisfied"] = f["emit"]
            continue
        if not emitting():
            continue
        # A variable assignment or an unresolvable standalone action contributes
        # no YAML of its own; dropping the line is closer to Helm than emitting a
        # bare scalar at column zero.
        if body is not None and drop_solo and (":=" in body or body.startswith(("$", "/*"))):
            continue

        line = raw
        while True:
            m = re.search(r"\{\{(.*?)\}\}", line)
            if not m:
                break
            act = m.group(1).strip()
            pre, post = line[: m.start()], line[m.end():]

            ninc = NINDENT_RE.search(act)
            # Only an action that *is* an include/toYaml is expanded; one that
            # merely mentions include(...) inside e.g. a `fail` message is not.
            inc = INCLUDE_RE.search(act) if act.startswith("include") else None
            # toYaml is expanded only when it carries an explicit indent: without
            # one (e.g. `toYaml .Values.config | sha256sum`) the result is a
            # scalar, not a block.
            ty = (TOYAML_RE.search(act)
                  if act.startswith("toYaml") and (ninc or INDENT_RE.search(act)) else None)

            if inc and inc.group(1) in helpers:
                if ninc or INDENT_RE.search(act):
                    body_txt = render(helpers[inc.group(1)], helpers, values, depth + 1)
                    n = int(ninc.group(1)) if ninc else int(INDENT_RE.search(act).group(1))
                    line = pre.rstrip() + "\n" + indent_block(body_txt, n) + post
                elif solo:
                    # A block-producing include on a line of its own: expand at
                    # the line's own indentation, and drop the line if the helper
                    # produced nothing (a guard such as pp-common.secretGuard).
                    body_txt = render(helpers[inc.group(1)], helpers, values, depth + 1)
                    if not body_txt.strip():
                        line = None
                        break
                    n = len(raw) - len(raw.lstrip())
                    line = indent_block(body_txt, n)
                    break
                else:
                    # Inline include: a scalar-valued helper such as
                    # pp-common.fullname or pp-common.image.
                    body_txt = render(helpers[inc.group(1)], helpers, values,
                                      depth + 1, drop_solo=False).strip()
                    one = " ".join(body_txt.split()) or "PLACEHOLDER"
                    line = pre + one + post
                continue
            if ty:
                node, ok = values, True
                for part in ty.group(1).strip(".").split("."):
                    if isinstance(node, dict) and part in node:
                        node = node[part]
                    else:
                        ok = False
                        break
                dumped = yaml.safe_dump(node, default_flow_style=False, sort_keys=False).rstrip() if ok and node not in (None, {}, []) else "{}"
                n = int(ninc.group(1)) if ninc else (int(INDENT_RE.search(act).group(1)) if INDENT_RE.search(act) else 0)
                line = pre.rstrip() + "\n" + indent_block(dumped, n) + post
                continue
            if inc:
                # An include of a helper we could not resolve.
                line = pre + "PLACEHOLDER" + post
                continue
            # A standalone action we could not resolve (toYaml of a non-.Values
            # expression, an include of an unknown helper) contributes a block,
            # not a scalar — emitting PLACEHOLDER at column zero would corrupt
            # the document. Drop the line instead.
            if solo and drop_solo:
                line = None
                break
            # Plain value substitution: the real value where it is knowable.
            repl = resolve_scalar(act, values)
            if repl is None and act == ".":
                dots = [f["dot"] for f in frames if f.get("dot") is not None]
                if dots:
                    repl = str(dots[-1])
            if repl is None:
                repl = "PLACEHOLDER"
            if re.search(r"(quote|toJson)\s*\}?$", act) or "quote" in act:
                repl = '"PLACEHOLDER"'
            line = pre + repl + post

        if line is not None:
            out.append(line)
    return "\n".join(out)


# ===========================================================================
# per-kind required fields
# ===========================================================================
def dig(d, *path):
    for p in path:
        if not isinstance(d, dict) or p not in d:
            return None
        d = d[p]
    return d


def check_doc(where, doc):
    if doc is None:
        return
    if not isinstance(doc, dict):
        fail(where, f"top-level document is {type(doc).__name__}, expected a mapping")
        return
    checked["docs"] += 1
    kind = doc.get("kind")
    for req in ("apiVersion", "kind"):
        if not doc.get(req):
            fail(where, f"missing {req}")
    # Kustomization and Component are build-time objects, not API objects: they
    # legitimately carry no metadata.name.
    if kind not in ("Kustomization", "Component") and not dig(doc, "metadata", "name"):
        fail(where, f"{kind}: missing metadata.name")

    if kind in ("Deployment", "Rollout", "DaemonSet", "StatefulSet"):
        sel = dig(doc, "spec", "selector", "matchLabels")
        lbl = dig(doc, "spec", "template", "metadata", "labels")
        if not sel:
            fail(where, f"{kind}: missing spec.selector.matchLabels")
        if not lbl:
            fail(where, f"{kind}: missing spec.template.metadata.labels")
        if sel and lbl:
            missing = set(sel) - set(lbl)
            if missing:
                fail(where, f"{kind}: selector keys {sorted(missing)} are absent from the pod template labels "
                            "— the workload would select zero pods")
        containers = dig(doc, "spec", "template", "spec", "containers") or []
        if not containers:
            fail(where, f"{kind}: no containers")
        for c in containers:
            n = c.get("name", "?")
            if not c.get("image"):
                fail(where, f"{kind}/{n}: no image")
            elif "@sha256:" not in str(c["image"]) and "PLACEHOLDER" not in str(c["image"]):
                fail(where, f"{kind}/{n}: image is not digest-pinned ({c['image']})")
            if not c.get("resources"):
                fail(where, f"{kind}/{n}: no resources block")
            else:
                if not dig(c, "resources", "requests", "cpu"):
                    fail(where, f"{kind}/{n}: no CPU request")
                if not dig(c, "resources", "requests", "memory"):
                    fail(where, f"{kind}/{n}: no memory request")
                if not dig(c, "resources", "limits", "memory"):
                    fail(where, f"{kind}/{n}: no memory LIMIT — memory is not compressible; "
                                "a leak without a limit takes down the node")
            sc = c.get("securityContext")
            if not sc:
                fail(where, f"{kind}/{n}: no container securityContext")
            else:
                if sc.get("allowPrivilegeEscalation") is not False:
                    fail(where, f"{kind}/{n}: allowPrivilegeEscalation must be false")
                if sc.get("readOnlyRootFilesystem") is not True:
                    fail(where, f"{kind}/{n}: readOnlyRootFilesystem must be true")
                if dig(sc, "capabilities", "drop") != ["ALL"]:
                    fail(where, f"{kind}/{n}: capabilities.drop must be [ALL]")
            if kind in ("Deployment", "Rollout"):
                for probe in ("startupProbe", "readinessProbe", "livenessProbe"):
                    if probe not in c:
                        fail(where, f"{kind}/{n}: missing {probe}")
                lp, rp = c.get("livenessProbe"), c.get("readinessProbe")
                if lp and rp:
                    lpath = dig(lp, "httpGet", "path")
                    rpath = dig(rp, "httpGet", "path")
                    if lpath and rpath and lpath == rpath:
                        fail(where, f"{kind}/{n}: liveness and readiness share the path {lpath}. "
                                    "Liveness must not depend on any downstream; if they are the same "
                                    "endpoint, a database failover kills the whole fleet")
        psc = dig(doc, "spec", "template", "spec", "securityContext")
        if not psc:
            fail(where, f"{kind}: no pod securityContext")
        else:
            if psc.get("runAsNonRoot") is not True:
                fail(where, f"{kind}: pod securityContext.runAsNonRoot must be true")
            if dig(psc, "seccompProfile", "type") != "RuntimeDefault":
                fail(where, f"{kind}: seccompProfile.type must be RuntimeDefault")
        if "automountServiceAccountToken" not in (dig(doc, "spec", "template", "spec") or {}):
            fail(where, f"{kind}: automountServiceAccountToken not set explicitly")

    if kind == "Service":
        if not dig(doc, "spec", "ports"):
            fail(where, "Service: no ports")
        if not dig(doc, "spec", "selector"):
            fail(where, "Service: no selector")

    if kind == "HorizontalPodAutoscaler":
        for f in ("scaleTargetRef", "minReplicas", "maxReplicas", "metrics"):
            if dig(doc, "spec", f) in (None, [], {}):
                fail(where, f"HPA: missing spec.{f}")

    if kind == "ScaledObject":
        if not dig(doc, "spec", "triggers"):
            fail(where, "ScaledObject: no triggers")
        if dig(doc, "spec", "minReplicaCount") == 0:
            fail(where, "ScaledObject: minReplicaCount 0 — nothing on the money path scales to zero")

    if kind == "PodDisruptionBudget":
        mn, mx = dig(doc, "spec", "minAvailable"), dig(doc, "spec", "maxUnavailable")
        if (mn is None) == (mx is None):
            fail(where, "PDB: set exactly one of minAvailable / maxUnavailable")
        if not dig(doc, "spec", "selector"):
            fail(where, "PDB: no selector — a PDB matching nothing silently permits everything")

    if kind == "NetworkPolicy":
        if dig(doc, "spec", "podSelector") is None:
            fail(where, "NetworkPolicy: no podSelector")
        if not dig(doc, "spec", "policyTypes"):
            fail(where, "NetworkPolicy: no policyTypes")

    if kind == "ServiceMonitor":
        if not dig(doc, "spec", "endpoints"):
            fail(where, "ServiceMonitor: no endpoints")

    if kind == "ExternalSecret":
        for f in ("secretStoreRef", "target"):
            if not dig(doc, "spec", f):
                fail(where, f"ExternalSecret: missing spec.{f}")
        if not (dig(doc, "spec", "data") or dig(doc, "spec", "dataFrom")):
            fail(where, "ExternalSecret: neither data nor dataFrom")

    if kind in ("Job", "CronJob"):
        spec = dig(doc, "spec", "template", "spec") if kind == "Job" else \
            dig(doc, "spec", "jobTemplate", "spec", "template", "spec")
        if not spec:
            fail(where, f"{kind}: no pod template spec")
        else:
            if spec.get("restartPolicy") not in ("Never", "OnFailure"):
                fail(where, f"{kind}: restartPolicy must be Never or OnFailure")
            if not spec.get("containers"):
                fail(where, f"{kind}: no containers")

    if kind == "PrometheusRule":
        for g in dig(doc, "spec", "groups") or []:
            for r in g.get("rules", []):
                if "alert" in r:
                    lbl, ann = r.get("labels", {}), r.get("annotations", {})
                    a = r["alert"]
                    if not lbl.get("severity"):
                        fail(where, f"alert {a}: no severity label")
                    for k in ("runbook_url", "summary", "description"):
                        if not ann.get(k):
                            fail(where, f"alert {a}: no {k} annotation")
                    if ann.get("runbook_url") and not str(ann["runbook_url"]).startswith("http"):
                        fail(where, f"alert {a}: runbook_url is not a URL")
                elif "record" not in r:
                    fail(where, f"rule group {g.get('name')}: entry is neither alert nor record")
                if not r.get("expr"):
                    fail(where, f"rule {r.get('alert') or r.get('record')}: no expr")

    if kind == "PriorityClass":
        if "value" not in doc:
            fail(where, "PriorityClass: no value")
        if doc.get("globalDefault"):
            fail(where, "PriorityClass: globalDefault must be false — a cluster-wide default "
                        "priority silently reclassifies every workload that omits one")


# ===========================================================================
# passes
# ===========================================================================
def parse_plain_yaml(path: Path):
    text = path.read_text()
    if "{{" in text and path.suffix in (".yaml", ".yml") and "templates/" in str(path):
        return None
    try:
        docs = list(yaml.safe_load_all(text))
    except yaml.YAMLError as e:
        fail(path, f"YAML parse error: {str(e).splitlines()[0]}")
        return None
    checked["yaml"] += 1
    return docs


def main() -> int:
    print("=" * 78)
    print("(a) YAML / JSON parse")
    print("=" * 78)
    manifests = sorted(DEPLOY.rglob("*.yaml"))
    chart_files = [p for p in sorted(HELM.rglob("*.yaml")) if "templates/" not in str(p)]
    for p in manifests:
        docs = parse_plain_yaml(p)
        if docs:
            for i, d in enumerate(docs):
                if d is not None:
                    check_doc(f"{p.relative_to(ROOT)}[{i}]", d)
    # values.yaml / Chart.yaml are parsed but are not Kubernetes objects.
    for p in chart_files:
        parse_plain_yaml(p)
    for p in sorted(DEPLOY.rglob("*.json")):
        try:
            obj = json.loads(p.read_text())
            checked["yaml"] += 1
            if "panels" in obj:
                ids = [pl["id"] for pl in obj["panels"]]
                if len(ids) != len(set(ids)):
                    fail(p, "duplicate panel ids")
                for pl in obj["panels"]:
                    if not pl.get("targets"):
                        fail(p, f"panel {pl.get('id')} '{pl.get('title')}' has no targets")
                    for t in pl.get("targets", []):
                        if not t.get("expr"):
                            fail(p, f"panel {pl.get('id')} target {t.get('refId')} has no expr")
                if not obj.get("uid") or not obj.get("title"):
                    fail(p, "dashboard has no uid/title")
        except json.JSONDecodeError as e:
            fail(p, f"JSON parse error: {e}")
    print(f"  parsed {checked['yaml']} files, {checked['docs']} kubernetes documents")

    print()
    print("=" * 78)
    print("(b) Go-template action balance  /  (c) .Values keys  /  (d) structural render")
    print("=" * 78)
    for chart_dir in sorted((HELM / "charts").iterdir()):
        if not chart_dir.is_dir():
            continue
        vfile = chart_dir / "values.yaml"
        values = yaml.safe_load(vfile.read_text()) if vfile.exists() else {}
        values = values if isinstance(values, dict) else {}
        helpers = load_helpers(chart_dir)
        if chart_dir.name != "pp-common":
            # Helper bodies are checked in each CONSUMING chart's context: a
            # library chart has no values of its own, and `.Values` inside a
            # helper is always the caller's.
            for hname, hbody in helpers.items():
                check_values_refs(Path(f"pp-common::{hname} (as used by {chart_dir.name})"),
                                  hbody, values, chart_dir.name)
        tdir = chart_dir / "templates"
        if not tdir.exists():
            continue
        n_tpl = 0
        for tpl in sorted(tdir.iterdir()):
            if tpl.suffix not in (".yaml", ".tpl", ".txt"):
                continue
            text = tpl.read_text()
            rel = tpl.relative_to(ROOT)
            check_balance(rel, text)
            if chart_dir.name != "pp-common":
                check_values_refs(rel, text, values, chart_dir.name)
            checked["templates"] += 1
            n_tpl += 1
            if tpl.suffix != ".yaml":
                continue
            rendered = render(text, helpers, values)
            try:
                docs = list(yaml.safe_load_all(rendered))
            except yaml.YAMLError as e:
                fail(rel, f"structural render does not parse as YAML: {str(e).splitlines()[0]}")
                continue
            for i, d in enumerate(docs):
                if d is not None:
                    check_doc(f"{rel}[{i}] (structural render)", d)
        print(f"  {chart_dir.name:<24} {n_tpl:>2} templates, {len(helpers)} helpers in scope")

    print()
    print("=" * 78)
    print(f"checked: {checked['yaml']} yaml/json files, {checked['templates']} templates, "
          f"{checked['docs']} k8s documents, {checked['values_refs']} .Values references")
    if findings:
        print(f"FINDINGS: {len(findings)}")
        for f in findings:
            print(f"  - {f}")
        return 1
    print("RESULT: clean")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
