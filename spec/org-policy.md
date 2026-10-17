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
`engines`, `escalation`, `labels`), each leaf field resolves to the value
from the
highest layer that sets it. Absent fields inherit downward; a field is
never "deleted" by a lower-precedence absence.

### Collection semantics

- **Scalars**: higher layer replaces.
- **Maps** (`labels`, `routing_hints`): key-wise merge; repo keys win.
- **Stage definitions** (`engines.<stage>`): merged per leaf —
  `engines.<stage>.engine` and `engines.<stage>.model` are independent
  scalars (spec/model-routing.md), so an org-pinned adapter coexists
  with a repo-chosen model.
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
| `security.*` gate floors (`enabled`, `min_severity`, `block_on_secret`, `severity_matrix.*`, scan modes `secrets_scan`/`dependency_audit`/`license_check`, `sbom.enabled`, `sbom.attach_to`) | The deterministic security gate cannot be relaxed — see [Security Gate](#security-gate). |

The org baseline declares pins explicitly:

```yaml
# org baseline .ai-sdlc.yaml
policy_floor:
  pin:
    - review.critical_requires_acceptance
    - merge.skip_labels
    - autonomy.level
```

## Security Gate

Epic #87 mandates a deterministic supply-chain security gate — secret and
dependency findings evaluated by code, never by generative judgment. The
gate runs as the required check `ai-sdlc-security`, carried by the
`verify.required_checks` union (strictly additive — a repo can add checks
but can never drop an org-mandated one). The contract surface is the
`security` section of [`ai-sdlc.schema.json`](ai-sdlc.schema.json); this
spec owns the contract and the merge semantics, the stage that emits the
check is specified separately.

### Floor fields

Unlike generic pins (org value outright), the security floors are
*clamps* — they apply whenever the org baseline declares the field, and
they still allow repo-side tightening. Listing them in
`policy_floor.pin` records the floor in the resolved contract's
`pinned`/`enforced` audit fields (the same double-declaration
`autonomy.level` uses):

| Field | Floor semantics |
|---|---|
| `security.enabled` | Boolean floor — an org `true` cannot be relaxed to `false` (repo opt-out of the gate is a non-goal); a repo `true` under an org `false` only tightens. |
| `security.min_severity` | Severity floor — resolves to the *stricter* (lower-rank) of the org and repo values: `low` < `medium` < `high` < `critical`. Findings at/above the resolved threshold are blocking-class; below it they are always report-only. |
| `security.block_on_secret` | Boolean floor — org `true` stands; secret/credential-material findings always block, regardless of severity or `pr:risk-*` tier. |
| `security.secrets_scan` | Scan-mode floor (issue #90) — resolves strictest-wins over `off` < `report` < `enforce`: a repo cannot silence or downgrade an org-mandated secrets scan. |
| `security.dependency_audit` | Scan-mode floor — same ranked clamp; a repo may audit harder, never weaker. |
| `security.license_check` | Scan-mode floor — same ranked clamp. Normative default is `report` (license observations stay advisory); an org may raise it to `enforce` fleet-wide. |
| `security.sbom.enabled` | Boolean floor — an org `true` cannot be relaxed to `false` (issue #92: the baseline can pin the SBOM artifact on). |
| `security.sbom.format` | Not a strictness dimension — no clamp. An org pins a format outright via `policy_floor.pin` (`cyclonedx` is the normative default). |
| `security.sbom.attach_to` | Guard-rail list — union strictly-additive like `verify.required_checks`: a repo may attach the SBOM to more surfaces (`pr`, `release`), never drop an org-declared one. |
| `security.severity_matrix.<pr:risk-*>` | Strictest-outcome floor per leaf the org declares — outcomes rank `report-only` < `blocking-if-fix-available` < `blocking`; a repo may harden a tier, never soften it. |
| `security.sbom` | Boolean floor — org `true` pins CycloneDX SBOM emission on org-wide (PR artifact + attested release asset; issue #92); a repo `true` under an org `false` only tightens. Evidence-only — never gates a check. |

### Severity matrix

`security.severity_matrix` maps a `pr:risk-*` tier (spec/risk-taxonomy.md)
to the gate outcome for blocking-class findings on that PR:

| pr:risk-* tier | Normative default | Meaning |
|---|---|---|
| `pr:risk-low` | `report-only` | Findings recorded on the check, never fail it. |
| `pr:risk-medium` | `report-only` | Findings recorded on the check, never fail it. |
| `pr:risk-high` | `blocking-if-fix-available` | Fails `ai-sdlc-security` when a finding ≥ `min_severity` ships with an upstream fix; unfixable findings degrade to report-only. |
| `pr:risk-critical` | `blocking-if-fix-available` | Same as high — blocking when a fix exists, report-only otherwise. |

`blocking` (opt-in, stricter than any default) fails the check
unconditionally. `blocking-if-fix-available` exists so the loop is never
paralyzed by findings nobody can act on (CVEs without an upstream fix).
Findings below `min_severity` stay report-only under every tier; license
observations are not severity-classed and follow `security.license_check`
(report-only at the normative `report` default). The full finding-class →
verdict calibration lives in
[docs/security/gate-calibration.md](../docs/security/gate-calibration.md).

## Resolution algorithm

```
resolve(org_cfg, repo_cfg):
  result = deep_copy(defaults)
  for layer in [org_cfg, repo_cfg]:
    for each leaf path p in layer:
      if isUnionField(p):    result[p] = union(result[p], layer[p])
      elif isFloorClamp(p):  result[p] = clamp(result[p], layer[p])   # autonomy.level ceiling + security.* floors — tightening wins even under a pin
      elif pinned(p) and layer == repo_cfg: continue                  # pin wins outright
      elif isMapField(p):    result[p] = mergeMaps(result[p], layer[p])
      else:                  result[p] = layer[p]
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
