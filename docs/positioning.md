# Positioning — ai-sdlc as the governance layer over agent engines

Posture statement, not a normative spec. It answers the recurring
question — *how does ai-sdlc relate to fullsend, Copilot-style agents,
and vendor runtimes?* — and the answer is the strategy: they are
candidate engines; this repo is the layer that governs them.

Tracking: [#216](https://github.com/os-santiago/ai-sdlc/issues/216) ·
epic [#153](https://github.com/os-santiago/ai-sdlc/issues/153).

## Thesis — the contract is the product surface

ai-sdlc is not another agent product competing on execution. The product
surface is the **normative contract**: `.ai-sdlc.yaml` plus the `spec/`
vocabulary plus the evidence every governed run must emit. Engines and
harnesses plug in **below** that surface — `sc-agent-cli` is the
reference engine today; `devin` is already declared in the adapter
registry; third-party harnesses are the intended next arrivals.

The winning end-state is **integration without fusion**: a certified
third-party harness running under an ai-sdlc contract — governed by the
same trust ladder, the same merge gate, the same evidence trail — while
the engine remains somebody else's product, improved on somebody else's
roadmap.

The pipeline does not care which harness produced a diff. It cares that
the run honored the contract: bounded, attributable, reviewable, and
backed by emitted evidence.

## What each layer owns

| Governance layer (this repo) | Engine / harness (pluggable below) |
|---|---|
| `.ai-sdlc.yaml` — per-repo policy: verify commands, autonomy ceiling, review/merge rules, escalation | prompt execution, tool loop, edit strategy |
| Trust ladder L0–L5, promotion/demotion gates | model interaction within the routed model |
| DoR intake (R1–R6), label state machine, risk taxonomy + merge gate | diff quality, step efficiency |
| Evidence contract: `contract.json`, run manifest, audit event stream | emitting that evidence faithfully |
| Budgets as enforced ceilings (`max_seconds`, `max_steps`, `max_repairs`) | staying inside them |

## The interoperability seam

The seam is exactly two things: **normalized events** and the
**headless engine contract**. Everything else is engine internals, and
the layer deliberately does not reach into them.

### Headless engine contract

A conforming engine is invocable non-interactively, once per run:

- reads the task from `--prompt-file` — issue text arrives fenced as
  `<untrusted-input>` data (`spec/injection-defense.md`);
- takes provider/model configuration from a **rendered config file**
  written by the runtime — the engine's single source of truth, no
  env/flag override layers;
- honors `--no-commit` — the pipeline owns git state;
- honors `--max-seconds` / `--max-steps` and exits with the declared
  class on exhaustion;
- writes `--audit-log` (normalized events, below) and `--summary-file`
  (run manifest).

Exit semantics are declared per engine via `exitCodeMap`: `0` success;
distinct classes for validation/no-changes, provider failure, auth
failure, and budget/livelock exhaustion. The pipeline acts on **classes,
not numbers** — only provider/auth earns the single `model.fallback`
retry; validation maps to `scc:no-changes`; exhaustion escalates.

### Normalized events

The audit log is JSONL — one normalized event per line in an
engine-agnostic vocabulary (`llm_request`, `llm_response`, `tool_call`,
`tool_result`, …), each carrying `ts` and `iteration` with argument
*digests and byte counts*, never raw payloads. The run manifest is the
aggregate record (`v`, `success`, `model`, `exit_reason`, `iterations`,
`tool_calls*`, `tokens_*`, `estimated_cost_usd`, `duration_ms`,
`resolution`, `files_changed`, `error`). Review, automerge, and
postmortem tooling consume these artifacts — they are the evidence the
trust ladder measures.

### Registration surface

`config/engine_adapters.json` is the adapter registry: per engine it
declares `binary`, `probe`, `argvBuilder`, `configRenderer`,
`exitCodeMap`, and `manifestMap` (`scc` and `devin` are declared).
Runtime dispatch is still hardcoded to `scc` in the implement stage —
the registry is the seam a multi-engine dispatcher binds to, and adding
a descriptor is how a new engine registers without workflow changes.

## Conformance path — how a third-party engine certifies

Certification is **behavioral**: asserted against emitted evidence, not
against the engine's codebase or benchmark claims. A conforming
harness demonstrates, in order:

1. **Invocable** — `probe` passes; `argvBuilder` renders a headless,
   non-interactive invocation; the run honors `--no-commit` and both
   budgets.
2. **Configured** — accepts the rendered config file as its sole
   provider/permission source; no hidden env or flag layer diverges
   from it.
3. **Auditable** — emits the normalized event stream and a run manifest
   with the required fields; artifacts upload clean as workflow
   evidence.
4. **Exit-faithful** — its `exitCodeMap` distinguishes the classes the
   pipeline acts on, so fallback, no-changes, and escalation semantics
   hold exactly as declared.
5. **Soaked** — runs at `shadow`/`suggest` autonomy on real issues;
   promotion to dispatch-eligible follows the same trust-ladder
   evidence as any autonomy increase (merge success, revert rate,
   escalation rate, repair rounds — `spec/trust-ladder.md`).

A certified harness is a first-class engine under the contract. An
uncertified one may still run in its own product — the seam is a
contract, not a gate on the ecosystem.

## Governance vocabulary we contribute

| Term | Anchored in | Meaning |
|---|---|---|
| `.ai-sdlc.yaml` contract | `spec/ai-sdlc.schema.json` | per-repo declarative policy — verify, autonomy, review, merge, escalation |
| trust ladder L0–L5 | `spec/trust-ladder.md` | earned autonomy `shadow` → `auto-deploy`; promotion gated on metrics |
| DoR R1–R6 | `spec/definition-of-ready.md` | machine-checkable issue readiness; vague work is refused, not attempted |
| `pr:risk-*` + evidence matrix | `spec/risk-taxonomy.md` | PR risk tiers and the review evidence each tier requires before merge |
| label state machine | `spec/labels.md` | `scc:*` run-states, veto pairs, `needs-human` orthogonal hold |
| trust boundaries | `spec/injection-defense.md` | T0–T3 content trust, `<untrusted-input>` fencing, forbidden actions |
| bounded everything | `docs/runtime.md` | `max_seconds` / `max_steps` / `max_repairs` / `max_fix_iterations` — no unbounded loops |
| evidence artifacts | `docs/runtime.md` | `contract.json`, run manifest, audit JSONL, `ai-sdlc-run.json` |

An engine that speaks this vocabulary can be governed by any conforming
runtime; a repo carrying the contract can swap engines without changing
policy. That portability is the contribution to the ecosystem.

## Non-goals

- **Competing on agent execution quality.** Whether scc, Devin, or a
  vendor harness writes better diffs is their race. The layer judges
  outcomes — merge success, revert rate, escalation rate — not how the
  diff was produced.
- **Engine internals.** Tool-loop design, context management, and
  routing heuristics live upstream (e.g. `os-santiago/sc-agent-cli` for
  the reference engine).
- **Chat/UX surfaces, hosting, model brokerage.** Deployment concerns —
  the Hermes control plane per
  [ADR-0001](architecture/adr/0001-homedir-ai-sdlc-sunset-and-cutover.md).
- **Certifying model quality.** The contract governs runs, not
  leaderboard claims.

## Posture summary

Engines compete on execution; ai-sdlc competes on governance. The
contract is the product, evidence is the currency, and the seam —
normalized events plus the headless engine contract — is where the two
worlds meet without merging.
