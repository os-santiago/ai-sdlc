# Model Routing — task-class → model selection in the contract (spec)

Normative semantics for how `.ai-sdlc.yaml` routes work to models
(issue #171, epic #148). Every stage in the pipeline is a **task class**;
the contract declares which model each class runs on so cost and quality
are spent where they matter — triage does not need frontier intelligence,
security review does.

Schema: [`ai-sdlc.schema.json`](ai-sdlc.schema.json) (`model`,
`review.ai_reviewer.model`, `engines.<stage>.model`). Reference resolver:
`.github/actions/load-config/resolve.py` (`stage_model`, `review_model`).

## Task classes

The canonical task classes are the pipeline stage keys already declared
by the `engines` map:

`intake` · `implement` · `review` · `automerge` · `verify` ·
`ci-repair` · `sweep`

A stage key is the unit of routing: the contract speaks in task classes,
never in model internals. Finer-grained classes inside a single engine
run (a docs pass inside implement, a refactor subtask) are engine-local
and route through `model.routing_hints` — an opaque map the engine reads,
not the pipeline.

## Contract surface

| Field | Meaning |
|---|---|
| `model.primary` | Global default model for every stage. |
| `model.fallback` | One-shot retry model on provider/quota failure. Empty = no fallback run. |
| `model.routing_hints` | Engine-internal task-class → model map; opaque to the pipeline, key-wise merged across layers. |
| `review.ai_reviewer.model` | Dedicated model for the AI reviewer — the most specific field on the `review` task class. |
| `engines.<stage>.model` | Per-task-class model: routes this stage to a model without pinning its adapter. |

`engines.<stage>` remains the single place a stage is defined — the
stage-definition object carries `engine` and/or `model`, at least one
required:

```yaml
engines:
  intake:
    model: auto/best-fast            # cheap class, default adapter (engine.name)
  implement:
    engine: scc
    model: auto/best-coding          # coding-optimized class
  review:
    model: auto/best-reasoning       # frontier class — review is where quality pays
  ci-repair:
    model: auto/best-reasoning
```

`engines.<stage>: <id>` (bare-id shorthand) still selects only the
adapter; routing a model requires the object form. Declaring `model`
alone does **not** change the stage's adapter — `stage_engine` still
resolves `engine.name`.

## Resolution chain

For a stage `S`, the effective model resolves top-down; first hit wins:

1. **Workflow input override** — `model` input on the called workflow
   (per-run escape hatch).
2. **Dedicated stage field** — `review.ai_reviewer.model` for `review`
   (none exists for other stages today).
3. **`engines.S.model`** — the declared task-class model.
4. **`model.primary`** — the global default; itself defaults to the
   runtime's built-in primary.
5. **Runtime failure fallback** — `model.fallback`, once, only on a
   provider/auth-class engine failure (below).

Engine selection is an independent, parallel chain — `engines.S.engine`
→ `engine.name` → `scc`. A stage definition may touch either axis
without the other.

Resolved maps are emitted for dispatch: `contract.json` keeps
`engines.<stage>` entries (object form iff a `model` is declared) and
the `stage_models` step output carries the filtered stage → model map.

## Fallback rules

- **No stage entry** → `model.primary`. No `model.primary` → the
  runtime's built-in default. The chain never yields "no model" from a
  valid contract — it yields the least specific applicable value.
- **Unreachable routes degrade, never dispatch.** OmniRoute route names
  (`auto/*`, `devin/*`) are Hermes control-plane only and unreachable
  from GitHub Actions runners (ADR-0001). A hop naming an OmniRoute
  route is skipped — resolution continues down the chain; if
  `model.primary` itself is an OmniRoute route the resolved value is
  empty rather than a dead provider. Deployments that can reach
  OmniRoute (homedir-style workers) honor the route as written.
- **`model.fallback` is global and one-shot.** When the resolved stage
  model fails with a provider/quota-class exit (engine `exitCodeMap`
  class 20/auth per `docs/engine_contract.md`), the stage retries once
  on `model.fallback`. It applies regardless of which hop produced the
  model — a stage-model run and a primary run share the same fallback.
- **Invalid references fail at contract check.** Unknown stage keys,
  non-certified `engine` ids, empty stage definitions, and empty or
  non-string `model` values are schema errors — a bad contract fails
  intake loudly (`scc:config-error`) rather than degrading silently to
  defaults.

## Benchmark-informed defaults

The contract is deliberately conservative at the default layer: unset,
every task class runs on `model.primary`. Repos then spend capability
per class. The recommended allocation mirrors benchmark tiers:

| Task class | Profile | Rationale |
|---|---|---|
| `intake`, `sweep`, `verify` | cheapest adequate (`auto/best-fast`-class) | Mechanical classification and orchestration; volume work where cost dominates. |
| `implement` | coding-optimized (`auto/best-coding`-class — the built-in default) | Diff quality drives merge rate; the primary benchmark axis for agentic coding. |
| `review`, `ci-repair` | frontier reasoning (`auto/best-reasoning`-class) | Adversarial reading of diffs and failure-log diagnosis are where reasoning depth pays for itself. |
| `automerge` | deterministic machinery | The merge gate is a decision table, not an agent run — it consumes no model unless a repair is dispatched. |

These are recommendations, not enforcement: the conformance suite
(`docs/engine_contract.md`) certifies that any registered adapter can
fill a stage slot, and the run ledger records the model id per run so
per-class cost/quality trade-offs are measurable against evidence
(epic #148) rather than benchmark hearsay.

## Org policy

`engines.<stage>.engine` and `engines.<stage>.model` are ordinary leaf
fields under `spec/org-policy.md`: the org baseline may set or pin them —
e.g. pin `engines.review.model` to a vetted reasoning model fleet-wide —
and a repo leaf wins unless pinned.
