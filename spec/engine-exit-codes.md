# Engine Exit-Code Contract — the scc raw taxonomy (spec)

The normative statement of how the AI-SDLC runtime consumes the **raw
process exit codes** emitted by `scc` (`os-santiago/sc-agent-cli`). The
implement stage's one-shot model fallback, the engine-stall classifier,
the ci-repair outcome ledger, and the outcome labeler all branch on `$?` —
this document fixes which codes exist and what the runtime does with each.

The **emission** contract is owned upstream by the engine:
[`docs/exit-codes.md` in `os-santiago/sc-agent-cli`](https://github.com/os-santiago/sc-agent-cli/blob/main/docs/exit-codes.md)
is the normative source for what each code means at exit; its numeric
mirror is `src/utils/exit-codes.ts`, and
`test/e2e/chat-exit-codes.test.ts` asserts every code at the real
process-exit level against a mock provider (engine-side tracking:
[sc-agent-cli#486](https://github.com/os-santiago/sc-agent-cli/issues/486),
taxonomy introduced in
[sc-agent-cli#409](https://github.com/os-santiago/sc-agent-cli/issues/409)).
This spec is the **consumption** contract on the runtime side: the
adapter-registry classification (`config/engine_adapters.json` →
`scc.exitCodeMap` / `scc.stallClassMap`), the fallback gate, and the
outcome routing.

Tracking: [#133](https://github.com/os-santiago/ai-sdlc/issues/133) ·
stall classes: [#158](https://github.com/os-santiago/ai-sdlc/issues/158) ·
registry dispatch: [#228](https://github.com/os-santiago/ai-sdlc/issues/228).

The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are to be
interpreted as described in RFC 2119.

## Terms

| Term | Meaning |
|---|---|
| raw exit taxonomy | The engine's own process exit codes (`0`, `1`, `10`, `11`, `20`–`24`, plus `128+n` signal exits), surfaced verbatim because the scc adapter declares `dispatch.rawExitCodes: true`. |
| `exitCodeMap` label | The registry's per-code classification string (`scc.exitCodeMap`), resolved by `classifyExitCode()`; undeclared codes resolve to the mandatory `default` label. Label space is the engine's error-class vocabulary (`Success`, `ValidationError`, `AgentError`, …) — it predates the raw-taxonomy adoption and MUST NOT be read as engine semantics. |
| stall class | The runtime verdict space — `ok`, `transport`, `tooling`, `spec`, `budget`, `livelock`, plus the `timeout` pseudo-class resolved against run evidence (`scripts/stall-classifier.js`). Each class routes to a distinct terminal state (publish / bounded retry / needs-human / needs-refinement). |
| `stallClassMap` | The registry's per-code stall-class override (`scc.stallClassMap`) — wins over the generic label→class table for codes whose `exitCodeMap` label is ambiguous in stall space (e.g. `20` is `AgentError` by label but a provider/`transport` death). |
| fallback gate | The implement stage's one-shot `model.fallback` retry: raw `primary_exit ∈ {20, 21}` **or** a `transport` stall verdict earns exactly one fresh run. |

## The contract table

The single machine-readable statement of the contract — the conformance
test ([`tests/exit_code_contract.test.js`](../tests/exit_code_contract.test.js))
parses this table and asserts the adapter registry, the workflows, and the
stall classifier agree with it. Column conventions: the `exitCodeMap` cell
names the registry label (`default` → *label* means the code is unmapped
and resolves through the mandatory `default`); the stall cell is the class
the classifier resolves (`stallClassMap` override first, else the label's
base class); the fallback cell is `yes` only for the raw codes that enter
the one-shot `model.fallback` gate.

<!-- contract-table -->
| Code | Engine outcome | `exitCodeMap` label | Stall class | Fallback gate | Runtime handling |
|---|---|---|---|---|---|
| `0` | success — workspace mutations produced | `Success` | `ok` | no | publish path: deterministic patch validate → apply-publish → `scc:pr-opened` |
| `1` | generic/unspecified engine error (usage errors, unclassified failures) | `default` → `AgentError` | `tooling` | no | final — `scc:failed` + `needs-human` (tooling route: postmortem on the engine repo) |
| `10` | clean terminal, zero mutations (`SCC_NO_CHANGES`) | `ValidationError` | `spec` | no | `scc:no-changes` + `scc:not-ready` refinement hold — back to the issue author |
| `11` | clean terminal, not actionable / blocked (`SCC_NOT_ACTIONABLE`) | `not_actionable` | `spec` | no | `scc:failed` + `scc:not-ready` refinement hold — back to the issue author |
| `20` | provider failure outside the failover envelope | `AgentError` | `transport` | **yes** | one `model.fallback` retry on a reset workspace; `scc:failed` + `needs-human` (transport route) when it does not converge |
| `21` | auth failure raised before the network | `AgentFailedError` | `tooling` | **yes** | one `model.fallback` retry (auth can be endpoint-specific); `scc:failed` + `needs-human` on a double provider/auth fault |
| `22` | execution budget exhausted (`SC_BUDGET_EXCEEDED`) | `MaxStepsExceeded` | `budget` | no | `scc:failed` + `scc:not-ready` refinement hold — re-scope the issue or raise `engine.max_seconds`/`engine.max_steps` |
| `23` | agent-loop abort — tool livelock, malformed-args storm (`SC_LIVELOCK`) | `MaxRepairsExceeded` | `livelock` | no | `scc:failed` + `needs-human` (livelock route: iterations burned with nothing written) |
| `24` | provider chain exhausted — every failover candidate failed | `TimeoutError` | `timeout` | no | `scc:failed`; the `timeout` pseudo-class resolves against mutation/signature evidence (transport vs budget vs livelock) |
| `default` | any undeclared code — reserved ranges `2`–`9`, `12`–`19`, `25`+, and signal exits the evidence cascade cannot reclassify | `AgentError` | `tooling` | no | final — `scc:failed` + `needs-human`; an unclassified nonzero exit is an engine defect to postmortem |
<!-- /contract-table -->

Notes on the table:

- **Registry-label vs engine-outcome divergence.** The registry label for
  `24` is `TimeoutError` although the engine emits it for a spent failover
  cascade (`PROVIDER_EXHAUSTED`) — the label space predates that constant.
  The stall pipeline resolves `24` through the `timeout` pseudo-class
  against run evidence rather than trusting the label literally. The
  *Engine outcome* column is authoritative for meaning; the label column is
  authoritative for classification.
- **`21` is `tooling`, yet retryable.** The raw-code fallback gate fires on
  the code (`20`/`21`), not the stall class: an auth failure can be
  model-endpoint-specific, so one retry on `model.fallback` is earned. The
  *final* outcome still classifies `tooling` — a persistent auth defect is
  a config/engine problem to postmortem, not transient infra.
- **Signal exits** (`128+n`, e.g. `130` SIGINT / `143` SIGTERM) carry no
  registry label of their own. The stall classifier resolves them in the
  timeout/signal evidence arm (mutations → `budget`, burned iterations →
  `livelock`, zero evidence → `transport`) before the registry label is
  consulted.

## Invariants (normative)

1. The scc adapter MUST declare `dispatch.rawExitCodes: true` — every
   consumer in this spec branches on the raw taxonomy, not the
   `exitCodeMap`-normalized `0`/`1`.
2. `scc.exitCodeMap` MUST declare a label for every code the runtime acts
   on individually (`0`, `10`, `11`, `20`–`24`) and MUST declare a
   `default` label — no engine exit code ever classifies as `undefined`.
3. The one-shot `model.fallback` retry MUST be gated on raw
   `primary_exit ∈ {20, 21}` or a classifier `transport` verdict, and MUST
   fire at most once per implement run. Every other code is final — a
   non-retryable failure MUST NOT burn the single fallback.
4. Codes `0` and `10` MUST NOT enter the failure path: `0` reaches the
   deterministic publish path, `10` lands on the no-changes refinement
   hold. The outcome labeler MUST key its `scc:pr-opened` / `scc:no-changes`
   arms on `0` / `10`.
5. Every label used by `scc.exitCodeMap` (including `default`) MUST have a
   base stall class in `scripts/stall-classifier.js` (`LABEL_CLASS`), and
   every `scc.stallClassMap` value MUST be a stall class or the `timeout`
   pseudo-class. Every `stallClassMap` key MUST be a code documented in
   this contract.
6. The engine MUST add new codes only inside its upstream reserved ranges
   (`2`–`9` clean terminals, `12`–`19` run outcomes, `25`+ fatal). Adopting
   a new code in this runtime requires the registry entry, this contract
   table, and the conformance test updated in one change — the spec is
   semver-sensitive.
7. A code's documented meaning MUST NOT be remapped to a different runtime
   handling without a spec revision — silent renumbering is exactly the
   failure this contract exists to make loud.

## Where the codes are consumed

| Consumer | Codes acted on |
|---|---|
| `.github/workflows/ai-sdlc-implement.yml` — engine step | fallback gate: `primary_exit ∈ {20, 21}` ∨ `primary_class = transport`; publish gate `exit_code = 0` |
| `.github/workflows/ai-sdlc-implement.yml` — outcome labeler | `case "$CODE" in` `0` → `scc:pr-opened` (apply-gate pass), `10` → `scc:no-changes` + refinement hold, `*` → `scc:failed` + stall-class routing; double `20`/`21` → `needs-human` |
| `.github/workflows/ai-sdlc-ci-repair.yml` — attempt manifest | `code = 0` → push outcome, `10` → `no-changes`, else `engine-exit-N` |
| `scripts/stall-classifier.js` | `LABEL_CLASS` (label→class) + `stallClassMap` (per-code override) → stall class → route |
| `scripts/self-heal.js` | `STALL_CLASS_MAP` label→class for the container/Tekton path (`spec` = no retry) |
| `scripts/gha/stage-engine-resolver.js` | surfaces the raw code under `dispatch.rawExitCodes` (normalized `0`/`1` otherwise) |
| `docs/runtime.md` | operator-facing summary table (advisory; this spec is normative) |

Non-scc engines dispatch through the same resolver but with normalized
`0`/`1` exit codes — their stage outcomes branch on the classifier's
`transport` verdict, not the raw-code gate. This contract governs the scc
taxonomy only.

## Drift checking

[`tests/exit_code_contract.test.js`](../tests/exit_code_contract.test.js)
(picked up by `node --test` in `tools/validate-repo.sh`, hence the contract
job in `.github/workflows/ai-sdlc.yml`) parses the contract table above
and asserts:

- every `scc.exitCodeMap` key is a documented code and carries the
  documented label; `default`-annotated codes stay unmapped;
- every documented stall class equals `stallClassMap[code]` when declared,
  else the label's `LABEL_CLASS` base class; every `stallClassMap` key is a
  documented code;
- every registry label (including `default`) has a `LABEL_CLASS` base
  class in `scripts/stall-classifier.js`;
- the implement workflow's raw-code fallback gate equals the documented
  fallback-eligible set — drift in either direction fails;
- every exit-code literal the implement labeler and the ci-repair ledger
  branch on is a documented code;
- `dispatch.rawExitCodes` stays `true`;
- this document keeps naming the upstream emission contract and its e2e
  coverage (`test/e2e/chat-exit-codes.test.ts`) — the engine-side link the
  issue requires.

## References

- [`docs/exit-codes.md` in `os-santiago/sc-agent-cli`](https://github.com/os-santiago/sc-agent-cli/blob/main/docs/exit-codes.md) — normative emission contract; `test/e2e/chat-exit-codes.test.ts` asserts every code end-to-end
- [`engine-conformance.md`](engine-conformance.md) — the adapter descriptor contract (`exitCodeMap` REQUIRED + `default` mandatory)
- [`docs/engine_contract.md`](../docs/engine_contract.md) — headless engine contract, §Exit Code Semantics
- [`docs/runtime.md`](../docs/runtime.md) — operator-facing fallback + label tables
- [`labels.md`](labels.md) — the `scc:*` label state machine the outcome labeler drives
- `config/engine_adapters.json` — the scc descriptor (`exitCodeMap`, `stallClassMap`, `dispatch.rawExitCodes`)
- `scripts/stall-classifier.js` — the stall-class cascade (`LABEL_CLASS`, `RESOLUTION_CLASS`, evidence arms)
- `scripts/self-heal.js` — `STALL_CLASS_MAP` for the container path
