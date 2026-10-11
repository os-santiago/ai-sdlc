# Interop Posture: AI-SDLC as Governance Layer over Third-Party Engines

AI-SDLC is not an agent product competing on execution quality. Instead, it is a **normative governance layer** that defines the contract (`ai-sdlc.yaml`), trust ladder, and evidence requirements under which any third-party engine or harness can operate. The winning end-state is a certified third-party harness running under an AI-SDLC contract — integration without fusion.

## Core Thesis

The AI-SDLC contract (`.ai-sdlc.yaml` + trust ladder + evidence) is the product surface. Engines and harnesses are pluggable components that operate *below* this layer. AI-SDLC provides the governance, policy, and compliance framework; engines provide the execution. This separation allows organizations to choose best-of-breed engines while maintaining a unified standard for safety, reliability, and autonomy.

## What Each Layer Owns

| Governance layer (this repo) | Engine / harness (pluggable below) |
|---|---|
| `.ai-sdlc.yaml` — per-repo policy: verify commands, autonomy ceiling, review/merge rules, escalation | prompt execution, tool loop, edit strategy |
| Trust ladder L0–L5, promotion/demotion gates | model interaction within the routed model |
| DoR intake (R1–R6), label state machine, risk taxonomy + merge gate | diff quality, step efficiency |
| Evidence contract: `contract.json`, run manifest, audit event stream | emitting that evidence faithfully |
| Budgets as enforced ceilings (`max_seconds`, `max_steps`, `max_repairs`) | staying inside them |

## Non-Goals

- **Do not compete on agent execution quality**: AI-SDLC does not aim to outperform or replace the execution capabilities of specialized engines (e.g., coding agents, test generators, deployers). Execution quality remains the domain of the harness.
- **Do not mandate a specific engine**: AI-SDLC is engine-agnostic. Any harness that conforms to the contract can be used.
- **Do not fuse governance with execution**: The governance layer (contract, trust ladder, evidence) is separate from the execution layer. This separation enables independent evolution and substitution.

## Certification Path for External Engines/Harnesses

A third-party harness can be certified to run under AI-SDLC by meeting the following requirements:

1. **Contract Adherence**: The harness must accept and operate under the constraints defined in `.ai-sdlc.yaml` (e.g., autonomy levels, allowed actions, resource limits).
2. **Trust Ladder Compliance**: The harness must report its actions in a way that allows the AI-SDLC runtime to assess trust metrics (merge success rate, revert rate, etc.) and adjust the autonomy level accordingly.
3. **Evidence Generation**: The harness must produce auditable evidence (logs, artifacts, test results) that conforms to the AI-SDLC evidence schema, enabling runtime verification and post-mortem analysis.
4. **Headless Engine Contract**: The harness must expose a normalized interface for receiving tasks (as structured events) and reporting progress/results, without requiring human interaction during execution.
5. **Conformance Testing**: The harness must pass the AI-SDLC conformance test suite, which validates that it respects the contract boundaries and correctly interacts with the runtime.

Upon successful certification, the harness is listed in the AI-SDLC registry of approved engines and can be referenced in `.ai-sdlc.yaml` via the `engine` field.

## Interoperability Seam

The seam between AI-SDLC governance and engine execution consists of two normalized components:

### 1. Normalized Events
All communication between the AI-SDLC runtime and the harness occurs via a defined set of JSON events:
- **Task Invocation**: `ai-sdlc/task/start` — contains the issue description, context, and contract constraints.
- **Progress Report**: `ai-sdlc/task/progress` — periodic updates on execution state.
- **Artifact Publication**: `ai-sdlc/task/artifact` — signals the production of a verified artifact (e.g., code diff, test report).
- **Completion Signal**: `ai-sdlc/task/end` — indicates success or failure, with attached evidence.

These events are exchanged over a durable transport (e.g., message queue, HTTP callbacks) and are versioned to allow independent evolution.

### 2. Headless Engine Contract
A certified harness must implement a headless (non-interactive) API that:
- Accepts a task invocation event and begins execution without requiring user input.
- Reports progress and artifacts via the normalized event stream.
- Respects the autonomy level and constraints from the contract (e.g., if at level `suggest`, it must not open a PR; if at `auto-merge-low`, it may open a PR but only merge under certain conditions).
- Can be paused, resumed, or terminated by the runtime via control events.

This contract ensures that the runtime retains governance control (e.g., ability to demote autonomy, freeze execution, or require human approval) while the harness focuses on execution.

### Registration Surface
`config/engine_adapters.json` is the adapter registry: per engine it declares `binary`, `probe`, `argvBuilder`, `configRenderer`, `exitCodeMap`, and `manifestMap` (`scc` and `devin` are declared). Runtime dispatch is still hardcoded to `scc` in the implement stage — the registry is the seam a multi-engine dispatcher binds to, and adding a descriptor is how a new engine registers without workflow changes.

## Governance Vocabulary

AI-SDLC contributes the following terms to the ecosystem to enable clear interoperability discussions:

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

An engine that speaks this vocabulary can be governed by any conforming runtime; a repo carrying the contract can swap engines without changing policy. That portability is the contribution to the ecosystem.

### Conformance Path — How a Third-Party Engine Certifies

Certification is **behavioral**: asserted against emitted evidence, not against the engine's codebase or benchmark claims. A conforming harness demonstrates, in order:

1. **Invocable** — `probe` passes; `argvBuilder` renders a headless, non-interactive invocation; the run honors `--no-commit` and both budgets.
2. **Configured** — accepts the rendered config file as its sole provider/permission source; no hidden env or flag layer diverges from it.
3. **Auditable** — emits the normalized event stream and a run manifest with the required fields; artifacts upload clean as workflow evidence.
4. **Exit-faithful** — its `exitCodeMap` distinguishes the classes the pipeline acts on, so fallback, no-changes, and escalation semantics hold exactly as declared.
5. **Soaked** — runs at `shadow`/`suggest` autonomy on real issues; promotion to dispatch-eligible follows the same trust-ladder evidence as any autonomy increase (merge success, revert rate, escalation rate, repair rounds — `spec/trust-ladder.md`).

A certified harness is a first-class engine under the contract. An uncertified one may still run in its own product — the seam is a contract, not a gate on the ecosystem.

## Conclusion

AI-SDLC’s role is to establish a trusted, verifiable framework under which diverse engines can operate safely and effectively. By focusing on governance rather than execution, AI-SDLC enables organizations to harness the innovation of third-party agents while maintaining consistent standards for risk management, compliance, and operational excellence.

Engines compete on execution; ai-sdlc competes on governance. The contract is the product, evidence is the currency, and the seam — normalized events plus the headless engine contract — is where the two worlds meet without merging.