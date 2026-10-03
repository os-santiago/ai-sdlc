#!/usr/bin/env python3
"""Resolve an already-validated .ai-sdlc.yaml (as JSON) into the runtime contract.

Precedence (highest first): workflow input override > repo .ai-sdlc.yaml >
runtime defaults. Org baseline resolution (spec/org-policy.md) is not
applied yet — see docs/runtime.md.

Writes <out-dir>/contract.json and, when GITHUB_OUTPUT is set, flat step
outputs. Stdlib only.
"""
import argparse
import copy
import hashlib
import json
import os
import sys

DEFAULTS = {
    "autonomy": {"level": "suggest", "merge_requires_checks": True, "freeze_env": "AI_SDLC_FREEZE"},
    "triggers": {
        "issue_label": "ready-to-implement",
        "skip_labels": ["epic", "wontfix", "invalid", "duplicate", "question", "dependencies", "needs-human"],
        "authorized_labelers": [],
    },
    "verify": {"commands": [], "required_checks": [], "timeout_seconds": 900},
    "review": {
        "mode": "ai",
        "risk_labels": ["pr:risk-low", "pr:risk-medium", "pr:risk-high", "pr:risk-critical", "pr:risk-accepted"],
        "critical_requires_acceptance": True,
    },
    "merge": {
        "method": "squash",
        "delete_branch": True,
        "max_repairs": 3,
        "pending_max_minutes": 60,
        "grace_minutes": 10,
        "skip_labels": ["needs-human", "hold", "do-not-merge", "wip"],
    },
    "labels": {
        "queued": "scc:queued",
        "in-progress": "scc:in-progress",
        "pr-opened": "scc:pr-opened",
        "failed": "scc:failed",
        "not-ready": "scc:not-ready",
        "no-changes": "scc:no-changes",
    },
    "model": {"primary": "auto/best-coding", "fallback": None, "routing_hints": {}},
    "engine": {"name": "scc", "max_steps": 200, "max_seconds": 900, "flags": []},
    "escalation": {"label": "needs-human", "contacts": [], "notify_channels": []},
}

# Sections resolved field-by-field; maps merge key-wise (spec/org-policy.md).
MAP_FIELDS = {"labels", "model.routing_hints"}
PASSTHROUGH = ("spec", "repo")

# Override surface: dotted path → kind. Unset = null / "" / negative number.
OVERRIDABLE = {
    "triggers.issue_label": "str",
    "model.primary": "str",
    "model.fallback": "str",
    "engine.max_seconds": "int",
    "engine.max_steps": "int",
    "verify.commands": "lines",
    "merge.method": "enum:squash,merge,rebase",
    "merge.max_repairs": "int",
    "merge.pending_max_minutes": "int",
}


class ContractError(Exception):
    pass


def get_path(d, path):
    for part in path.split("."):
        if not isinstance(d, dict) or part not in d:
            return False, None
        d = d[part]
    return True, d


def set_path(d, path, value):
    parts = path.split(".")
    for part in parts[:-1]:
        d = d.setdefault(part, {})
    d[parts[-1]] = value


def leaf_paths(d, prefix=""):
    for k, v in d.items():
        p = f"{prefix}{k}"
        if isinstance(v, dict) and p not in MAP_FIELDS:
            yield from leaf_paths(v, p + ".")
        else:
            yield p


def is_unset(v):
    return v is None or (isinstance(v, str) and v.strip() == "") or (
        isinstance(v, (int, float)) and not isinstance(v, bool) and v < 0)


def coerce(path, kind, v):
    if kind == "str":
        return str(v).strip()
    if kind == "int":
        try:
            f = float(v)
        except (TypeError, ValueError):
            raise ContractError(f"override {path}: expected a number, got {v!r}")
        if f != int(f):
            raise ContractError(f"override {path}: expected an integer, got {v!r}")
        return int(f)
    if kind == "lines":
        items = v if isinstance(v, list) else str(v).splitlines()
        return [str(c).strip() for c in items if str(c).strip()]
    if kind.startswith("enum:"):
        allowed = kind[5:].split(",")
        if v not in allowed:
            raise ContractError(f"override {path}: {v!r} not in {allowed}")
        return v
    raise AssertionError(kind)


def resolve(repo_cfg, overrides):
    result = copy.deepcopy(DEFAULTS)
    sources = {p: "default" for p in leaf_paths(DEFAULTS)}

    for key in PASSTHROUGH:
        if key in repo_cfg:
            result[key] = copy.deepcopy(repo_cfg[key])

    sections = {k: v for k, v in repo_cfg.items() if k not in PASSTHROUGH}
    for path in leaf_paths(sections):
        _, value = get_path(sections, path)
        if path in MAP_FIELDS:
            _, base = get_path(result, path)
            merged = dict(base or {})
            merged.update(value or {})
            value = merged
        set_path(result, path, copy.deepcopy(value))
        sources[path] = "contract"

    for path, raw in (overrides or {}).items():
        if path not in OVERRIDABLE:
            raise ContractError(f"unknown override path {path!r} (allowed: {', '.join(sorted(OVERRIDABLE))})")
        if is_unset(raw):
            continue
        value = coerce(path, OVERRIDABLE[path], raw)
        if OVERRIDABLE[path] == "lines" and not value:
            continue
        set_path(result, path, value)
        sources[path] = "input"

    if not result.get("spec"):
        raise ContractError("resolved contract has no spec version")
    if result["engine"]["max_seconds"] < 60 or result["engine"]["max_steps"] < 10:
        raise ContractError("engine budgets below schema minimum (max_seconds ≥ 60, max_steps ≥ 10)")
    result["_sources"] = dict(sorted(sources.items()))
    return result


def flat_outputs(c):
    j = lambda v: json.dumps(v, separators=(",", ":"))
    labels = c["labels"]
    return {
        "spec": c["spec"],
        "autonomy_level": c["autonomy"]["level"],
        "trigger_label": c["triggers"]["issue_label"],
        "skip_labels": j(c["triggers"]["skip_labels"]),
        "authorized_labelers": j(c["triggers"]["authorized_labelers"]),
        "model_primary": c["model"]["primary"] or "",
        "model_fallback": c["model"]["fallback"] or "",
        "max_seconds": str(c["engine"]["max_seconds"]),
        "max_steps": str(c["engine"]["max_steps"]),
        "verify_commands": "\n".join(c["verify"]["commands"]),
        "verify_timeout_seconds": str(c["verify"]["timeout_seconds"]),
        "required_checks": j(c["verify"]["required_checks"]),
        "merge_method": c["merge"]["method"],
        "merge_delete_branch": "true" if c["merge"]["delete_branch"] else "false",
        "max_repairs": str(c["merge"]["max_repairs"]),
        "pending_max_minutes": str(c["merge"]["pending_max_minutes"]),
        "merge_skip_labels": j(c["merge"]["skip_labels"]),
        "escalation_label": c["escalation"]["label"],
        "labels": j(labels),
        "label_queued": labels.get("queued", ""),
        "label_pr_opened": labels.get("pr-opened", ""),
        "label_failed": labels.get("failed", ""),
        "label_not_ready": labels.get("not-ready", ""),
        "label_no_changes": labels.get("no-changes", ""),
        "json": j(c),
    }


def write_github_output(outputs, path):
    with open(path, "a", encoding="utf-8") as fh:
        for k, v in outputs.items():
            if "\n" in v:
                delim = "AI_SDLC_EOF_" + hashlib.sha256(v.encode()).hexdigest()[:16]
                fh.write(f"{k}<<{delim}\n{v}\n{delim}\n")
            else:
                fh.write(f"{k}={v}\n")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--input", required=True, help="validated contract as JSON")
    ap.add_argument("--overrides", default="{}", help="JSON object: dotted path → value")
    ap.add_argument("--out-dir", required=True)
    ap.add_argument("--source-repo", default="")
    ap.add_argument("--source-ref", default="")
    ap.add_argument("--source-path", default=".ai-sdlc.yaml")
    a = ap.parse_args()

    try:
        raw = open(a.input, "rb").read()
        repo_cfg = json.loads(raw)
        if not isinstance(repo_cfg, dict):
            raise ContractError("contract root must be a mapping")
        try:
            overrides = json.loads(a.overrides or "{}")
        except json.JSONDecodeError as e:
            raise ContractError(f"overrides is not valid JSON: {e}")
        if not isinstance(overrides, dict):
            raise ContractError("overrides must be a JSON object")
        contract = resolve(repo_cfg, overrides)
    except ContractError as e:
        print(str(e), file=sys.stderr)
        return 1

    contract["_source"] = {
        "repo": a.source_repo,
        "ref": a.source_ref,
        "path": a.source_path,
        "sha256": hashlib.sha256(raw).hexdigest(),
        "precedence": ["input", "contract", "default"],
        "org_policy": "not-applied",
    }
    os.makedirs(a.out_dir, exist_ok=True)
    out_path = os.path.join(a.out_dir, "contract.json")
    with open(out_path, "w", encoding="utf-8") as fh:
        json.dump(contract, fh, indent=2, sort_keys=True)
        fh.write("\n")

    outputs = flat_outputs(contract)
    outputs["path"] = out_path
    if os.environ.get("GITHUB_OUTPUT"):
        write_github_output(outputs, os.environ["GITHUB_OUTPUT"])
    overridden = [p for p, s in contract["_sources"].items() if s == "input"]
    print(f"resolved contract spec={contract['spec']} level={contract['autonomy']['level']} "
          f"model={contract['model']['primary']} overrides={overridden or 'none'} → {out_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
