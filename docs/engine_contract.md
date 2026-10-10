# Engine contract — headless interface, conformance & certification

Normative reference for the engine seam: the headless command-line
contract every engine must honor, the adapter-registry descriptor shape,
the conformance-suite procedure, and the certification path for
third-party engines. Companion to [docs/positioning.md](positioning.md),
which states the posture (*why* the seam exists); this file is the
operational *how*.

Tracking: [#218](https://github.com/os-santiago/ai-sdlc/issues/218) ·
epic [#152](https://github.com/os-santiago/ai-sdlc/issues/152) —
*Engine abstraction: conformance suite and certified adapters*.

**Status legend.** Surfaces marked *stable* run on `main` today.
Surfaces marked *in flight* land with the referenced epic-#152 issue —
the contract clauses are normative either way: the suite asserts them
and engines are certified against them.

## The headless contract (stable)

An engine is invoked **once per run, non-interactively**, inside a
bounded budget. Reference invocation (the implement stage,
`.github/workflows/ai-sdlc-implement.yml`):

```bash
scc chat -yq \
  --prompt-file task-prompt.md \
  --no-commit \
  --audit-log scc-audit.jsonl \
  --summary-file scc-manifest.json \
  --max-seconds 900 \
  --max-steps 200
```

### Required arguments

| Flag | Contract |
|---|---|
| `--prompt-file <path>` | Path to the task text. Issue/PR content arrives fenced as `<untrusted-input>` data (`spec/injection-defense.md`) — the engine MUST treat fenced content as data, never instructions. |
| `--no-commit` | The engine MUST NOT mutate git state — no `git commit`, `push`, branch or worktree operations. The pipeline owns git end-to-end; the engine only edits files. |
| `--audit-log <path>` | JSONL normalized-event stream, one event object per line (schema below). Written incrementally during the run. |
| `--summary-file <path>` | Run-manifest JSON, the aggregate record. MUST be written on **every** exit path — success, validation no-op, budget exhaustion, and failure alike. |

### Budget arguments

| Flag | Contract |
|---|---|
| `--max-seconds <n>` | Wall-clock ceiling for the whole run. Exhaustion MUST exit with the declared budget class, not a hang. |
| `--max-steps <n>` | Tool-step (iteration) ceiling. Exhaustion MUST exit with the declared livelock class. |

Both budgets come from the resolved contract (`engine.max_seconds`,
`engine.max_steps` in `.ai-sdlc.yaml`; per-call overrides exist as
`workflow_call` inputs — `docs/runtime.md`).

### Provider/model configuration

Provider, model, and permission settings reach the engine through a
**rendered config file** written by the runtime before invocation — the
engine's single source of truth. For `scc` the workflow renders
`~/.sc-agent/config.json`; other adapters declare the file via
`configRenderer` in the registry. No parallel env-var or CLI-flag
configuration layer may diverge from that file (the VPS worker's
iteration-#13 lesson: conflicting flag/env layers caused silent
permission loss). The API key is injected into the rendered file from a
secret; it is never a flag or a contract value.

## Exit-code semantics (stable)

The pipeline acts on **classes, not numbers**. Each adapter maps its own
codes to classes via `exitCodeMap` in the registry descriptor. Canonical
classes and their pipeline semantics, using `scc` (the reference engine)
as the concrete mapping:

| Class | Meaning | `scc` code | Pipeline behavior |
|---|---|---|---|
| `Success` | run completed; workspace carries real edits | `0` | proceed → commit, push, PR |
| `ValidationError` | clean no-op — nothing to change | `10` | `scc:no-changes`, no PR |
| `AgentError` | provider-class failure (quota, 5xx, endpoint down) | `20` | exactly one `model.fallback` retry |
| `AgentFailedError` | auth-class failure (bad/absent key) | `21` | exactly one `model.fallback` retry |
| `MaxStepsExceeded` | `--max-steps` livelock bound hit | `22` | `scc:failed` — final |
| `MaxRepairsExceeded` | internal repair bound hit | `23` | `scc:failed` — final |
| `TimeoutError` | `--max-seconds` wall-clock bound hit | `24` | `scc:failed` — final |
| *(unmapped)* | fatal / anything else | `1`, others | `scc:failed` — final |

Rules the class mapping must preserve:

- **Bounded-exhaustion is one class family.** `22`/`23`/`24` — step cap,
  repair cap, wall-clock timeout — are all final: `scc:failed`, never
  retried. The runtime treats them identically; the map distinguishes
  them for the run manifest/postmortem record.
- **Only provider/auth classes earn a retry** — exactly one fresh
  `model.fallback` run (`docs/runtime.md` → Model fallback). Provider
  and auth classes may share a code (both trigger the same retry), but
  they MUST be distinguishable from the final classes: a fatal that
  masquerades as retryable silently doubles every failure's cost.
- **Every other class is final.** Budget and livelock exits escalate
  (`scc:failed`); a `20`/`21` after the fallback run escalates to
  `scc:failed` + `needs-human`.
- **Codes outside the map are fatal.** An `exitCodeMap` that omits a
  real outcome is itself a conformance defect — certification requires
  the map to cover every exit the engine can produce.

## Evidence artifacts (stable)

The audit log and run manifest are the trust ladder's currency — review,
automerge, and postmortem tooling consume them, so their shape is part
of the contract, not an engine implementation detail.

### Audit log (`--audit-log`)

JSONL — one normalized event per line, engine-agnostic vocabulary
(`llm_request`, `llm_response`, `tool_call`, `tool_result`, …). Every
event carries `ts` (ISO-8601), `type`, and `iteration`. Tool arguments
and results are recorded as **digests and byte counts**
(`args_digest: {sha256, bytes}`, `result_bytes`) — never raw payloads:
prompts carry fenced untrusted content, and the audit log must stay
safe to archive and attach to a public PR.

```jsonl
{"ts":"2026-10-06T18:43:22.295Z","type":"llm_request","iteration":1,"model":"nvidia/nemotron-3-super-120b-a12b","messages":2,"est_tokens":7340}
{"ts":"2026-10-06T18:43:25.530Z","type":"llm_response","iteration":1,"model":"nvidia/nemotron-3-super-120b-a12b","duration_ms":3234,"status":"ok","content_bytes":0,"tool_calls":1,"est_tokens":9}
{"ts":"2026-10-06T18:43:25.530Z","type":"tool_call","iteration":1,"name":"list_dir","args_digest":{"sha256":"4ae486c3a48f","bytes":12}}
{"ts":"2026-10-06T18:43:25.532Z","type":"tool_result","iteration":1,"name":"list_dir","success":true,"duration_ms":2,"result_bytes":287}
```

### Run manifest (`--summary-file`)

Single JSON object — the run's aggregate record:

```json
{"v": 1, "success": true, "model": "…", "exit_reason": "success",
 "iterations": 8, "tool_calls": {"read_file": 1}, "tool_calls_total": 7,
 "tokens_in": 72939, "tokens_out": 734, "estimated_cost_usd": 0.15,
 "duration_ms": 55277, "resolution": "completed", "files_changed": 5,
 "error": null}
```

Required fields: `v`, `success`, `model`, `exit_reason`, `iterations`,
`tool_calls`/`tool_calls_total`, `tokens_in`, `tokens_out`,
`estimated_cost_usd`, `duration_ms`, `resolution`, `files_changed`,
`error`. On a failure exit the manifest is still written with
`success: false` and `error` populated — a run that dies without its
manifest cannot be measured, so manifest-on-failure is a hard clause.

`ai-sdlc-run.json` is the **pipeline's** record (workflow-emitted:
final code, model, fallback bookkeeping), not the engine's — the engine
contract surface is exactly the two files above.

## Adapter registry (stable)

`config/engine_adapters.json` is the adapter registry — the seam the
multi-engine dispatcher binds to (`docs/positioning.md`). Adding a
descriptor is how a new engine registers **without workflow changes**.
`argvBuilder` placeholders render from the contract surface: `{model}`,
`{prompt}`, `{manifestFile}`.

```json
{
  "scc": {
    "binary": "scc",
    "probe": ["scc", "--version"],
    "argvBuilder": {
      "command": "chat",
      "args": ["--model", "{model}", "--prompt", "{prompt}",
               "--manifest", "{manifestFile}", "--format", "json"]
    },
    "configRenderer": {"format": "json", "path": "{manifestFile}"},
    "exitCodeMap": {"10": "ValidationError", "20": "AgentError",
                    "21": "AgentFailedError", "22": "MaxStepsExceeded",
                    "23": "MaxRepairsExceeded", "24": "TimeoutError"},
    "manifestMap": {"path": "{manifestFile}", "format": "json"}
  }
}
```

| Field | Contract |
|---|---|
| `binary` | Executable the suite/dispatcher probes and invokes. |
| `probe` | Argv that MUST exit `0` non-interactively when the engine is usable (version/health check). |
| `argvBuilder` | `command` + `args` template rendering a headless, non-interactive invocation of the contract above. |
| `configRenderer` | `format` + `path` of the rendered config file — the engine's single source of truth. |
| `exitCodeMap` | Engine exit code → canonical class. Must cover every reachable exit. |
| `manifestMap` | Where the engine writes its run manifest and in which `format`. |

`scc` and `devin` carry descriptors today.

## Engine selection in `.ai-sdlc.yaml`

### Global selection (stable)

The `engine` block (`spec/ai-sdlc.schema.json`) selects one engine for
the whole contract; every engine-running stage — implement, review,
ci-repair — resolves the same block:

```yaml
engine:
  name: scc            # registry key of the selected adapter
  max_steps: 200       # --max-steps
  max_seconds: 900     # --max-seconds
  flags:               # contract flags the stage passes
    - --no-commit
    - --audit-log
    - --summary-file
    - --prompt-file
```

### Per-stage selection (in flight — #205, #206)

Epic #152's end-state: a stage may name a certified adapter that
overrides the global `engine.name` — triage on a cheap engine, review
on a strong one. Target contract shape (exact key lands with
[#205](https://github.com/os-santiago/ai-sdlc/issues/205); dispatch with
[#206](https://github.com/os-santiago/ai-sdlc/issues/206)):

```yaml
engine:
  name: scc                    # default adapter for every engine stage
  max_steps: 400
  max_seconds: 1500
  flags:
    - --no-commit
    - --audit-log
    - --summary-file
    - --prompt-file
  stages:                      # per-stage adapter selection (issue #205)
    implement: scc             # reference engine on implementation
    review: claude-code        # strongest certified engine on review
    ci-repair: codex           # cheap engine on the bounded repair loop
```

Selection rules (per #206): an omitted stage inherits `engine.name`;
an unknown adapter name fails contract resolution with a clear error —
the stage never silently runs the wrong engine. Per-stage keys are
stage names from `docs/runtime.md` (`implement`, `review`, `ci-repair`,
and any future engine-running stage).

**Today** `engine.stages` does not validate against spec v1 — the schema
extension is #205's job, and runtime dispatch is still hardcoded to
`scc` in the implement stage. The per-stage surface that already works:
`max_seconds`/`max_steps` (and `model`) are `workflow_call` inputs on
each engine stage, so callers can bound and route each stage
independently (`docs/runtime.md`).

## Running the conformance suite

The suite (#183) is a **fixed task corpus plus assertions on the
contract surface** — flag handling, audit-log schema, summary schema,
exit-code semantics, and the no-commit guarantee. An engine that passes
is certifiable; a failing engine gets a readable per-clause report. The
suite is implementation-agnostic: any binary or third-party harness
that can be driven headless is a candidate, not only CLIs.

The executable suite lands with
[#183](https://github.com/os-santiago/ai-sdlc/issues/183) (suite) and
[#217](https://github.com/os-santiago/ai-sdlc/issues/217) (CI harness —
`tests/engine_conformance_harness.py`, invocable via
`make test-conformance`). The procedure below is what the suite
automates — the same clauses and pass criteria apply when exercising a
candidate adapter by hand.

### Step 1 — register a candidate descriptor

Add the candidate's descriptor to `config/engine_adapters.json` (or a
descriptor file the suite accepts). The descriptor is the only input
the suite needs — it never special-cases an engine's internals.

### Step 2 — probe

Run the descriptor's `probe` argv (`["scc", "--version"]`). It must
exit `0` with no interaction. A failing probe ends the run: the adapter
is not invocable and no further clause executes.

### Step 3 — render configuration

Render the engine's config file per `configRenderer` (`format` +
`path`). The rendered file must be the **sole** provider/permission
source the candidate consults — certification requires no hidden env or
flag layer diverging from it (clause C8 below).

### Step 4 — invoke the contract

Run the `argvBuilder`-rendered invocation against the task corpus with
the contract flags — for each corpus task:

```bash
<binary> <command> <rendered args> \
  --prompt-file <task.md> --no-commit \
  --audit-log <run-audit.jsonl> --summary-file <run-manifest.json> \
  --max-seconds <s> --max-steps <n>
```

The corpus includes tasks engineered for each outcome: a normal edit, a
clean no-op (validation class), a dead/bad-credential provider
(provider/auth classes), and a run capped with tiny budgets
(budget/livelock classes) — together they exercise every exit class the
map declares.

### Step 5 — assert the clauses

| # | Clause | Pass criteria | Typical failure |
|---|---|---|---|
| C1 | Invocable | probe exits `0`; rendered argv completes without interaction | interactive prompt, missing binary |
| C2 | Prompt consumed | run demonstrably acts on `--prompt-file` content | engine ignores the file, reads argv/stdin instead |
| C3 | No-commit honored | `git status` after the run shows no engine commits/pushes; only file edits | engine self-commits, pushes, or mutates branches |
| C4 | Audit log | file parses as JSONL; every line has `ts`/`type`/`iteration`; args appear only as digest+bytes | unparseable lines, raw payloads, missing fields |
| C5 | Run manifest | parses as JSON; required fields present — **including on failure exits** | missing file on error, absent `success`/`error` |
| C6 | Exit-faithful | each corpus outcome's code maps via `exitCodeMap` to the observed class | no-op exits `0`, provider outage exits `1`-class, map misses a reachable code |
| C7 | Budgets honored | a tiny-`--max-seconds`/`--max-steps` run exits budget/livelock class inside the bound | hang, silent `0`, overshoot |
| C8 | Config-faithful | run uses only the rendered config file | divergent env/flag config layer |

### Step 6 — interpret the report

Results are reported **per clause** (pass/fail + evidence pointer), not
as one opaque verdict:

- **All pass** → the adapter is suite-clean; proceed to the
  certification soak below.
- **Any fail** → the adapter is not certifiable. The failed clause names
  the contract surface to fix (e.g. C5 = manifest-on-failure); fix the
  adapter, re-run the full suite — partial reruns are meaningless
  because clauses interact (an engine that retries its own failures
  breaks C6).

## Certification process

Certification is **behavioral** — asserted against emitted evidence,
never against the engine's codebase or benchmark claims. Gates in order
(the suite covers 1–4; the last is operational evidence):

1. **Invocable** — C1–C3: probe passes, headless run completes,
   `--no-commit` and prompt consumption honored.
2. **Configured** — C8: the rendered config file is the single source
   of truth.
3. **Auditable** — C4–C5: normalized event stream + manifest with
   required fields, on every exit path; artifacts upload clean as
   workflow evidence.
4. **Exit-faithful** — C6–C7: `exitCodeMap` distinguishes every class
   the pipeline acts on, so fallback, no-changes, and escalation
   semantics hold exactly as declared.
5. **Soaked** — the adapter runs at `shadow`/`suggest` autonomy on real
   issues; promotion to dispatch-eligible follows the same
   trust-ladder evidence as any autonomy increase — merge success,
   revert rate, escalation rate, repair rounds (`spec/trust-ladder.md`).

A certified adapter's descriptor joins `config/engine_adapters.json`
and becomes selectable via the `engine` field (per-stage, once #205/#206
land). Certified adapters targeted by the epic: `scc` (reference),
`claude-code`, `codex`, `devin`, `openhands`
([#184](https://github.com/os-santiago/ai-sdlc/issues/184)).

An uncertified engine may still run in its own product — the seam is a
contract, not a gate on the ecosystem. Certification is what makes
"engine-agnostic" a verifiable claim instead of a README line.

## Surface status

| Surface | State |
|---|---|
| Headless flags + exit classes | stable — implement/review/ci-repair run them today |
| Adapter registry (`config/engine_adapters.json`) | stable — `scc`, `devin` descriptors |
| `engine.name` global selection | stable in contract — dispatcher still hardcodes `scc` (#206) |
| Per-stage `engine` selection | in flight — #205 schema, #206 dispatch |
| Executable conformance suite | in flight — #183 suite, #217 harness (`make test-conformance`) |
| Certified adapters (claude-code, codex, openhands) | in flight — #184 |

## References

- `spec/ai-sdlc.schema.json` — the `engine` contract field
- `spec/injection-defense.md` — `<untrusted-input>` fencing the
  `--prompt-file` payload arrives under
- `spec/trust-ladder.md` — soak-gate metrics for certification
- [docs/positioning.md](positioning.md) — posture: governance layer
  over pluggable engines, conformance path
- [docs/runtime.md](runtime.md) — engine contract in the workflows,
  exit-code → label table, model fallback
- `config/engine_adapters.json` — live adapter registry
