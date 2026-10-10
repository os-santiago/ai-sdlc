#!/usr/bin/env python3
"""Resolve an already-validated .ai-sdlc.yaml (as JSON) into the runtime contract.

Precedence (highest first): workflow input override > repo .ai-sdlc.yaml >
org baseline (spec/org-policy.md) > runtime defaults.

Field-level merge between contract layers: scalars replace, maps merge
key-wise, guard-rail lists (triggers.skip_labels, merge.skip_labels,
verify.required_checks) union strictly-additive, and autonomy.level
resolves to the lower of the org ceiling and the repo value — repos can
tighten autonomy, never loosen it. The org baseline may pin floor fields
via policy_floor.pin: pinned scalars/maps take the org value outright,
union fields keep org entries (pin = repos cannot relax, may tighten).
policy_floor.exempt lists repos the baseline skips entirely.

Writes <out-dir>/contract.json and, when GITHUB_OUTPUT is set, flat step
outputs. Stdlib only.
"""
import argparse
import copy
import hashlib
import json
import os
import re
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
        "ai_reviewer": {"model": None, "inline_comments": True, "incremental": True},
        "risk_labels": ["pr:risk-low", "pr:risk-medium", "pr:risk-high", "pr:risk-critical", "pr:risk-accepted"],
        "critical_requires_acceptance": True,
        "max_fix_iterations": 3,
        "wait_max_minutes": 15,
        "external_reviewer": "",
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
# Guard-rail lists: strictly-additive union across layers — a layer can
# add entries but never drop a lower layer's (spec/org-policy.md).
UNION_FIELDS = {"triggers.skip_labels", "merge.skip_labels", "verify.required_checks"}
# Meta sections consumed by resolution itself — never merged into fields.
META_KEYS = {"spec", "repo", "policy_floor"}
PASSTHROUGH = ("spec", "repo")
# Trust-ladder order (schema enum order) for the autonomy.level ceiling.
AUTONOMY_ORDER = ["shadow", "suggest", "auto-pr", "auto-merge-low", "auto-merge-all", "auto-deploy"]

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
    "review.mode": "enum:ai,human,hybrid",
    "review.ai_reviewer.model": "str",
    "review.max_fix_iterations": "int",
    "review.wait_max_minutes": "int",
    "review.external_reviewer": "str",
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


def union_list(base, extra):
    return list(dict.fromkeys(list(base or []) + list(extra or [])))


def min_level(a, b):
    if a not in AUTONOMY_ORDER:
        return b
    if b not in AUTONOMY_ORDER:
        return a
    return a if AUTONOMY_ORDER.index(a) <= AUTONOMY_ORDER.index(b) else b


def merge_layer(result, sources, sections, source):
    for path in leaf_paths(sections):
        _, value = get_path(sections, path)
        if path in UNION_FIELDS:
            set_path(result, path, union_list(get_path(result, path)[1], value))
        elif path in MAP_FIELDS:
            merged = dict(get_path(result, path)[1] or {})
            merged.update(value or {})
            set_path(result, path, merged)
        else:
            set_path(result, path, copy.deepcopy(value))
        sources[path] = source


def resolve(repo_cfg, overrides, org_cfg=None):
    """Layer defaults < org baseline < repo contract, then input overrides.

    Returns (contract, pinned, enforced): `pinned` is the org-declared
    policy_floor.pin list; `enforced` is the fields where an org floor
    actively constrained a repo value (`org-pin` in _sources).
    """
    result = copy.deepcopy(DEFAULTS)
    sources = {p: "default" for p in leaf_paths(DEFAULTS)}

    org_set = set()
    pin_set = set()
    if org_cfg:
        pin_set = set((org_cfg.get("policy_floor") or {}).get("pin") or [])
        org_sections = {k: v for k, v in org_cfg.items() if k not in META_KEYS}
        org_set = set(leaf_paths(org_sections))
        merge_layer(result, sources, org_sections, "org")

    for key in PASSTHROUGH:
        if key in repo_cfg:
            result[key] = copy.deepcopy(repo_cfg[key])

    sections = {k: v for k, v in repo_cfg.items() if k not in META_KEYS}
    for path in leaf_paths(sections):
        _, value = get_path(sections, path)
        pinned = path in pin_set and path in org_set
        if path in UNION_FIELDS:
            # Strictly additive in every layer — union already keeps the
            # org/default guard rails; a pin only records the floor.
            set_path(result, path, union_list(get_path(result, path)[1], value))
            sources[path] = "org-pin" if pinned else "contract"
        elif path == "autonomy.level" and path in org_set:
            level = min_level(get_path(result, path)[1], value)
            set_path(result, path, level)
            sources[path] = "contract" if level == value else ("org-pin" if pinned else "org")
        elif pinned:
            # Non-overridable floor: the org value stands and the repo's
            # own value is recorded as suppressed via the org-pin source.
            sources[path] = "org-pin"
        elif path in MAP_FIELDS:
            merged = dict(get_path(result, path)[1] or {})
            merged.update(value or {})
            set_path(result, path, merged)
            sources[path] = "contract"
        else:
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
    enforced = sorted(p for p, s in sources.items() if s == "org-pin")
    return result, sorted(pin_set), enforced


# OmniRoute route names (auto/*, devin/*) are Hermes control-plane only —
# unreachable from GHA runners by design (ADR-0001). A contract carrying one
# for the reviewer falls back to model.primary instead of a dead provider.
OMNI_ROUTE = re.compile(r"^(?:auto|devin)/")


def review_model(c):
    m = ((c["review"].get("ai_reviewer") or {}).get("model") or "").strip()
    if OMNI_ROUTE.match(m):
        m = ""
    m = m or (c["model"].get("primary") or "")
    # The fallback can itself be an OmniRoute route (defaults, homedir
    # contracts) — dead on GHA, so emit nothing rather than a dead provider.
    if OMNI_ROUTE.match(m):
        m = ""
    return m


def flat_outputs(c):
    j = lambda v: json.dumps(v, separators=(",", ":"))
    labels = c["labels"]
    return {
        "spec": c["spec"],
        "org_policy": c["_source"]["org_policy"],
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
        "review_mode": c["review"]["mode"],
        "review_model": review_model(c),
        "review_max_fix_iterations": str(c["review"]["max_fix_iterations"]),
        "review_wait_max_minutes": str(c["review"]["wait_max_minutes"]),
        "review_external_reviewer": c["review"].get("external_reviewer") or "",
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
    ap.add_argument("--org", default="", help="validated org baseline as JSON (optional)")
    ap.add_argument("--org-state", default="", help="org layer status when --org is absent: not-applied|unavailable")
    ap.add_argument("--org-repo", default="")
    ap.add_argument("--org-ref", default="")
    ap.add_argument("--org-path", default="")
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
        org_cfg = None
        org_raw = b""
        org_state = a.org_state or "not-applied"
        applied_org = None
        if a.org:
            org_raw = open(a.org, "rb").read()
            org_cfg = json.loads(org_raw)
            if not isinstance(org_cfg, dict):
                raise ContractError("org baseline root must be a mapping")
            org_state = "applied"
            exempt = (org_cfg.get("policy_floor") or {}).get("exempt") or []
            repo_name = (a.source_repo or "").rsplit("/", 1)[-1]
            if a.source_repo in exempt or repo_name in exempt:
                org_state = "exempt"
            else:
                applied_org = org_cfg
        try:
            overrides = json.loads(a.overrides or "{}")
        except json.JSONDecodeError as e:
            raise ContractError(f"overrides is not valid JSON: {e}")
        if not isinstance(overrides, dict):
            raise ContractError("overrides must be a JSON object")
        contract, pinned, enforced = resolve(repo_cfg, overrides, applied_org)
    except ContractError as e:
        print(str(e), file=sys.stderr)
        return 1

    contract["_source"] = {
        "repo": a.source_repo,
        "ref": a.source_ref,
        "path": a.source_path,
        "sha256": hashlib.sha256(raw).hexdigest(),
        "precedence": (["input", "contract", "org", "default"] if org_state == "applied"
                       else ["input", "contract", "default"]),
        "org_policy": org_state,
    }
    if org_cfg is not None:
        contract["_source"]["org"] = {
            "repo": a.org_repo,
            "ref": a.org_ref,
            "path": a.org_path,
            "sha256": hashlib.sha256(org_raw).hexdigest(),
        }
        if org_state == "applied":
            contract["_source"]["pinned"] = pinned
            contract["_source"]["enforced"] = enforced
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
    note = " repo-policy_floor-ignored" if isinstance(repo_cfg.get("policy_floor"), dict) else ""
    print(f"resolved contract spec={contract['spec']} level={contract['autonomy']['level']} "
          f"model={contract['model']['primary']} org={org_state} "
          f"overrides={overridden or 'none'}{note} → {out_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
