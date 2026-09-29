# Org-Wide Policy Model — baseline `.ai-sdlc.yaml` inheritance (spec)

Fleet operation needs a baseline every repo inherits and selectively
overrides — the same model as org-level rulesets. This spec defines how an
org baseline `.ai-sdlc.yaml` (hosted in a designated org repo, e.g.
`org/.ai-sdlc`) merges with each repo's own `.ai-sdlc.yaml` (schema:
[`ai-sdlc.schema.json`](ai-sdlc.schema.json)).

## Resolution model

Three layers, in precedence order (lowest → highest):

1. **Runtime defaults** — built into the consuming pipeline.
2. **Org baseline** — the org-wide `.ai-sdlc.yaml`. Applies to every repo
   unless the org config marks the repo exempt.
3. **Repo config** — the repo's own `.ai-sdlc.yaml`.

Resolution is **field-level merge**: for each top-level section
(`autonomy`, `triggers`, `verify`, `review`, `merge`, `model`, `engine`,
`escalation`, `labels`), each leaf field resolves to the value from the
highest layer that sets it. Absent fields inherit downward; a field is
never "deleted" by a lower-precedence absence.

### Collection semantics

- **Scalars**: higher layer replaces.
- **Maps** (`labels`, `routing_hints`): key-wise merge; repo keys win.
- **Lists** (`skip_labels`, `verify.commands`, `required_checks`):
  *union* for guard rails (skip_labels, required_checks — strictly
  additive), *replace* for configuration (verify.commands,
  authorized_labelers) — a repo MAY redefine its own verify commands but
  CANNOT remove org-mandated checks.
- **Ceilings**: `autonomy.level` resolves to the **lower** of org baseline
  and repo value — repos can only tighten autonomy, never loosen it past
  the org ceiling.

## Non-overridable floor fields

Fields the org baseline can pin so repos cannot relax them. A pinned
field is enforced even if the repo attempts to override:

| Field | Rationale |
|---|---|
| `merge.skip_labels` (union-includes `needs-human`, `hold`, `do-not-merge`, `wip`) | Human escape hatches are non-negotiable. |
| `review.critical_requires_acceptance` (pinned `true`) | `pr:risk-critical` always needs human acceptance. |
| `autonomy.level` (ceiling, e.g. org-wide `auto-merge-low`) | Blast-radius control. |
| `triggers.skip_labels` (union-includes `needs-human`) | Intake-side human override. |
| `verify.required_checks` (union) | Org-mandated gates always run. |

The org baseline declares pins explicitly:

```yaml
# org baseline .ai-sdlc.yaml
policy_floor:
  pin:
    - review.critical_requires_acceptance
    - merge.skip_labels
    - autonomy.level
```

## Resolution algorithm

```
resolve(org_cfg, repo_cfg):
  result = deep_copy(defaults)
  for layer in [org_cfg, repo_cfg]:
    for each leaf path p in layer:
      if pinned(p) and layer == repo_cfg: continue        # pin wins
      if isUnionField(p):  result[p] = union(result[p], layer[p])
      elif isMapField(p):  result[p] = mergeMaps(result[p], layer[p])
      elif p == 'autonomy.level': result[p] = min_level(result[p], layer[p])
      else:                result[p] = layer[p]
  validate(result)  # MUST pass ai-sdlc.schema.json
  return result
```

Errors: an invalid repo config fails intake loudly (`scc:config-error`
comment + no dispatch) — a broken contract never silently degrades to
defaults; an unreachable org baseline fails safe per-repo (env/default
behavior) and logs once.

## Example

Org pins `autonomy.level: auto-merge-low` + critical-acceptance; a repo
asks for `auto-merge-all` → effective `auto-merge-low`. A repo asks for
`suggest` under the same org → effective `suggest` (tightening allowed).
