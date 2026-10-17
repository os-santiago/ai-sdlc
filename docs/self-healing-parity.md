# Self-healing parity — Hermes control plane → AI-SDLC runtimes (issue #187)

Part of epic #115 (self-hosted AI-SDLC). This is the parity map: every
self-healing capability that keeps the Hermes control plane alive
unattended, its GHA-native equivalent in this repo, and the contract a
non-GHA orchestrator (Tekton / Dagger / other forges) consumes.

**Canonical orchestrator: `scripts/self-heal.js`** — one sequencing module
(`plan` / `classify` / `postmortem` / `sentinel`) so no orchestrator
re-implements the order. Everything below is a `require()` or a CLI call.

## The sequence (normative)

```
extract  failure signature → canonical failure artifact
         {signature, signatureParts, rootCause, checks, baseFault}
consult  signature → docs/runbooks/failure_modes.md → bounded advisory
decide   stall guard (per-signature burn) + flat budget →
         repair | escalate | quarantine
escalate stall class (transport|tooling|spec) → retry policy →
         canonical postmortem (dedup + reopen) → needs-human
sentinel base-branch health poll → latch issue (halt / clear)
```

## Parity map

| Hermes control-plane feature | GHA-native equivalent (this repo) | Self-hosted / forge-agnostic delta |
|---|---|---|
| Crash classes auto-file as deduplicated postmortems (`signatureHash`, open+closed canonical) | `scripts/postmortem.js` — `<!-- ai-sdlc-failure-signature: H -->` marker, recurrence comments, `postmortem.reopen_min` reopen (spec/postmortem-lifecycle.md) | `handleOccurrence(opts, client)` takes the forge client — `{listIssues, listComments, addComment, reopenIssue, createIssue, ensureLabel}`. GHA injects `ghClient()`; other forges implement the same surface. |
| Failure signature artifact per repair attempt | `failure.json` written by `ai-sdlc-ci-repair.yml` (sha256[:16] over sorted normalized `check:error-line` parts — `failure-runbook.failureSignature`) | `self-heal.js extractFailure()` emits the identical artifact — signature formula single-sourced in `failure-runbook.js`, lockstep-tested. |
| Runbook memory — known signatures inject the previous fix into the next worker brief | `scripts/failure-runbook.js` + `docs/runbooks/failure_modes.md` (entry sections, `<!-- ai-sdlc:runbook-entry -->`, `Signatures:`/`Match:`/`Fix:` bullets; bounded `--max-bytes` advisory) | Store is a repo doc — any forge carries it. `self-heal.js consultRunbook()` returns the advisory + injected brief; a miss is byte-identical (no-noise contract). |
| Engine stalls classified transport / tooling / spec and routed | **`scripts/self-heal.js classifyStall()`** — adapter `exitCodeMap` class name → `transport` / `tooling` / `spec`; pipeline outcomes (`no-changes`, `push-rejected`, `base-contract-error`…) classify without an exit code | `{retries, backoff}` per class is plain data — a Tekton Task maps `retries` onto its retry field: `transport` → 3/exponential, `tooling` → 1, `spec` → 0 (escalate). Unmapped classes fall back to `spec` (fail-closed). |
| Repair-stall guard — same failing-check signature re-burns budget → escalate | `scripts/repair-stall-guard.js` — per-signature `sig=` on `<!-- ai-sdlc:ci-repair attempt=N … -->` markers; `stall_max` in `ai-sdlc-ci-repair.yml` | `self-heal.js planRepair()` — markers are any forge's attempt ledger entries `{attempt, sha, sig}` filtered to trusted authors; identical signature ≥ `stall_max` → escalate, never re-burn. |
| Flat repair budget (`PR_MAX_REPAIRS`) → `needs-human` | `max_repairs` input → `needs-human` label + cause comment | `planRepair` returns `repairs-exhausted:n/m`; `escalate()` applies the label via injected `exec`. |
| Main-guard sentinel — red base halts automerge | `scripts/main-guard.js` — sentinel issue latch (`fix(ci): main red — sentinel halt`), `ai-sdlc-automerge.yml` gate + `ai-sdlc-sweep.yml` cron | `self-heal.js sentinel()` — the poll *is* the Task for forges without webhooks; latch lives on the forge's issue tracker. `--apply` performs mutations; dry-run computes the verdict. |
| Failure-class alert routing (`repair-stalled` → right maintainers) | `scripts/alert-router.js` — reason → canonical class (`spec/risk-taxonomy.md`) → `escalation.routes` | `sequence()` normalizes the decision reason to the canonical class before routing (`repair-stall:*` → `repair-stalled`). |
| Pre-prompt sanitization fence | `ai-sdlc-ci-repair.yml` sanitize stage (`spec/injection-defense.md`) | `planRepair` honors `sanitizeBlocked` → `quarantine` before any other decision. |
| Trust boundary — repair machinery never runs from PR head | `runtime_ref` pin fetch (`gh api …?ref=$RUNTIME_REF`) for every trusted script | Same pin-fetch posture; `self-heal.js` and its dependencies are fetched from the runtime repo ref, never the consumer/PR head. |

## Where each piece is invoked

| Phase | GHA caller | Non-GHA caller |
|---|---|---|
| extract + consult + decide | `ai-sdlc-ci-repair.yml` guard/prompt steps (inline node, same primitives) | `self-heal.js plan` |
| engine run + outcome record | `ai-sdlc-ci-repair.yml` engine/push/record steps | orchestrator's own engine task |
| stall classification | — (recorded as `engine-exit-N` outcome) | `self-heal.js classify` → retry policy |
| postmortem | `ai-sdlc-verify.yml` (verify failures) | `self-heal.js postmortem` |
| sentinel | `ai-sdlc-sweep.yml` `sentinel` job | `self-heal.js sentinel` on a schedule |

## Remaining deltas (tracked, not solved here)

- **Forge-adapter package**: `handleOccurrence`'s client surface is the
  de-facto forge-adapter contract; a first-class `forge/` adapter module
  (GitLab/Gitea implementations) is future work under epic #115.
- **In-workflow reuse**: `ai-sdlc-ci-repair.yml` sequences these phases
  inline today; `self-heal.js` exists so non-GHA orchestrators don't fork
  that logic. Converging the workflow to call `self-heal.js plan` is a
  follow-up (keeps the diff auditable).
- **Tekton retry contract**: `classifyStall().retry` is the vocabulary the
  Tekton Results API consumes; the Tekton-side mapping lands with the
  self-hosted orchestrator (epic #115), not in this repo.
- **Cross-repo runbook entries**: the store seeds only entries observed in
  this runtime; Hermes' production runbook entries are ported as they
  recur (signature-keyed, so porting is additive).
