# AI-SDLC Operations Runbook

Operator-facing reference for a repo consuming this runtime: what the
`scc:*` labels mean, where each failure mode surfaces, and the recovery
action for each. Normative label semantics: `spec/labels.md`; stage
reference: `docs/runtime.md`.

Reading order when something looks wrong:

1. **Issue labels** — the state machine below says where the flow stopped.
2. **Cause comments** — every escalation stamps a marker comment naming the
   machine cause (`<!-- ai-sdlc:… -->`); read the last one first.
3. **Run artifacts** — `gh run view <id>` / `gh run download` — the ledger,
   `contract.json`, engine audit log and manifests per stage (see
   [Artifacts](#artifacts)).
4. **Ledger bundle** — `scripts/audit-query.js` stitches the sealed
   stage-by-stage trail (`why escalated`, `labels`, `trace`).

## Label state machine

Canonical states and default bindings (rename via `labels.*` in
`.ai-sdlc.yaml`; names here are the reference deployment's):

```
issue ──▶ triaged ──▶ ready-to-implement ──▶ scc:queued ──▶ scc:in-progress ──▶ scc:pr-opened
              │              ▲                    │                                 │
              └─▶ scc:not-ready (re-check re-arms)│            scc:failed · scc:no-changes (terminal)

needs-human — orthogonal hold: vetoes dispatch AND merge while present
skip labels — epic · umbrella · scc:child · duplicate · wontfix · invalid ·
              question · dependencies: terminal, never dispatched
```

| Label | Meaning | Where it surfaces | Recovery |
|---|---|---|---|
| `ready-to-implement` | dispatch trigger present — the only dispatchable state | issue labels | Intake consumes it on dispatch; if it stays put, intake never ran or vetoed — check for a coexisting blocker label (forbidden pair → janitor strips the trigger, comment `<!-- ai-sdlc:label-veto -->`) |
| `scc:not-ready` | refinement hold — spec failed DoR (R1–R6) or the conversational refiner judged it under-specified | issue labels + upserted `<!-- hermes:refinement -->` comment listing questions/ACs | Answer the refiner comment or edit the body; the fingerprint re-triggers the check and a pass clears the hold + restores the trigger. Manual: remove `scc:not-ready`, add `ready-to-implement` |
| `scc:queued` | DoR passed, dispatch accepted | issue labels + intake run outputs (`dispatch=true`) | Implement should follow in the same caller run; if stuck, check the `ai-sdlc-implement` job of that run |
| `scc:in-progress` | an engine run owns the issue right now | issue labels | Transient — should not linger past the run. If stale, the run died before labeling: check the implement run's conclusion |
| `scc:pr-opened` | engine exited 0 — branch pushed, PR opened (`Closes #N`) | issue labels + `ai-sdlc-run-<issue>` artifact | Normal terminal. Follow the PR — review/automerge take over there |
| `scc:failed` | engine exited 20/21/22/23/1 after fallback handling | issue labels + `ai-sdlc-run.json` (`exit_code`, `primary_exit`, `fallback_used`) | Read the exit code → failure-mode table below. Re-arm: remove `scc:failed`, add `ready-to-implement` (sanctioned retry path — trigger + terminal run-state is legal) |
| `scc:no-changes` | engine exited 10 — clean no-op, no workspace mutations | issue labels + engine manifest | Spec likely already satisfied or unimplementable as written. Refine the issue, then re-arm (`−scc:no-changes +ready-to-implement`) |
| `scc:child` | epic-decomposition child — terminal until a human arms it | child issue labels + `<!-- hermes:epic-child -->` marker | Human reviews the child spec, then applies `ready-to-implement` |
| `needs-human` | orthogonal human hold — vetoes dispatch and merge | issue/PR labels + cause comment | Read the cause comment. Fix the underlying condition, remove the label, then re-arm (`+ready-to-implement`) — T7. A policy-fired hold needs `policy:accepted` before clearing |

`pr:risk-*` labels are PR-side risk tiers (spec/risk-taxonomy.md), not run
states: `pr:risk-critical` holds merge until a human applies
`pr:risk-accepted`.

## Failure modes

Each entry names the machine cause (marker comment / gate reason), the
artifact to inspect, and the recovery. Engine exit codes per
`docs/runtime.md` → Engine contract.

| Failure | Surfaces as | Where to look | Recovery |
|---|---|---|---|
| Provider error — dead/quota endpoint | engine exit 20 → one `model.fallback` run; both dead → `scc:failed` + `needs-human` | `ai-sdlc-run.json` (`fallback_used`, `primary_exit`), `scc-audit.primary.jsonl` | Fix `vars.AI_SDLC_PROVIDER_URL` / `MODEL_API_KEY` or rotate the model. Since GitHub Models retired (2026-07-30), unset provider config is the usual cause. Clear `needs-human`, re-arm |
| Auth failure | engine exit 21 — same path as 20 | same artifacts | Missing/expired `MODEL_API_KEY`, or `AI_SDLC_APP_ID`/`AI_SDLC_APP_PRIVATE_KEY` unset (stage fails fast with `ai-sdlc auth` — no PAT fallback). Restore the secret, clear `needs-human`, re-arm |
| Engine no-op | exit 10 → `scc:no-changes` | `scc-manifest.json` | Spec satisfied or unimplementable — refine and re-arm |
| Budget cap | exit 22 (`max_seconds`) / 23 (livelock) → `scc:failed` | `ai-sdlc-run.json` | Raise `engine.max_seconds`/`max_steps` in `.ai-sdlc.yaml` or shrink the issue's scope, then re-arm |
| Contract invalid on base | stage fails fast, `::error … scc:config-error::`; intake/implement label `scc:not-ready` | `error.txt` in `ai-sdlc-contract-<job>-<attempt>` | Run `tools/validate-ai-sdlc-config.sh` locally; fix `.ai-sdlc.yaml` on `main`. In ci-repair the same signature on the BASE ref escalates immediately (`base-contract-error`) — no push can green it |
| Checks red on PR | gate action `repair-signaled` → ci-repair attempt comment `<!-- ai-sdlc:ci-repair attempt=N sha=… sig=… -->` | `ai-sdlc-ci-repair-<pr>-attempt-<n>` artifact (`ci-repair-manifest.json`: `failure.signature`, `rootCause`, `runbookHit`) | Bounded — up to `merge.max_repairs` (default 3) heads signaled. Let the loop run; it escalates on its own |
| Repair loop exhausted | `needs-human` on the PR, gate reason `repairs-exhausted:<names>` | last ci-repair artifact + gate run | Fix the check manually on the head branch, or close the PR and re-spec the issue. Remove `needs-human` to hand control back |
| Same failure signature repeating | `needs-human`, gate reason `repair-stall:<sig>` / `repair-stalled:<names>` | `sig` = sha256-16 of the sorted failing check names, in the attempt markers | The repair pushed but left the identical checks red — deterministic failure. Diagnose the check from `ci-repair-manifest.json`, fix manually, clear `needs-human` |
| Checks pending too long | `needs-human`, gate reason `pending-timeout` (`merge.pending_max_minutes`, default 60) | `gh pr checks N` | Workflow stuck/queued — re-run or cancel in Actions; clear `needs-human` when checks resolve |
| Merge conflict | `needs-human`, gate reason `conflict` | `gh pr view N` mergeable=CONFLICTING | Update the branch (`gh pr update-branch N`) or rebase manually; clear `needs-human` |
| `pr:risk-critical` unaccepted | gate `held` — `risk-critical-unaccepted` | PR labels | Human reviews and applies `pr:risk-accepted` — merge proceeds on next gate pass |
| Review blocked | `needs-human`, gate reason `review-fix-stalled` / `review-fix-not-dispatched` / `external-review-blocking` | `<!-- ai-sdlc:review sha=… verdict=blocked -->` marker + `ai-sdlc-review-<pr>-<attempt>` artifact | Address findings on the head branch, or dismiss in the review comment; the new head re-triggers review |
| Review infra down | merge proceeds anyway past `review.wait_max_minutes` (15) with `review_timeout` in the merge reason | gate run eval | No action — fail-open by design; fix review infra separately |
| Injection-defense block | `needs-human` on the issue/PR; the hostile field never reached the engine | `injection-audit.jsonl` in run artifacts (pattern names + counts only) | Quarantine review: edit the hostile content out of the issue/PR text if legitimate, or close. Clear `needs-human` to re-arm |
| Main-guard sentinel | gate `held` — `main-guard:*`; deduped postmortem issue `fix(ci): main red — sentinel halt on <repo>` (open issue = the latch) | `merge.main_guard.workflows` newest completed run on `main` | Fix `main` — green next gate run closes the latch and merges resume. Disable: `merge.main_guard.enabled: false` |
| Label contradiction | `<!-- ai-sdlc:label-veto -->` comment; loser label stripped | issue labels + sweep `label-repair` job | A forbidden pair (e.g. trigger + skip label) is a pipeline defect — the blocker always wins. Resolve the vetoing label first, then re-apply the trigger. Reappearing pairs escalate `needs-human` with cause `label-drift-loop` |
| Kill-switch engaged | every gated stage fails its Autonomy gate → run red (hard stages), `::error` naming the engaged channel | `gh run view` gate step; `ai-sdlc:off` issue in control/target repo; `vars.AI_SDLC_KILL`/`AI_SDLC_OFF` | See [Kill-switch](#kill-switch). An *unreadable* channel denies too — check `vars.AI_SDLC_CONTROL_REPO` reachability |
| Post-merge verify red | canonical postmortem issue (signature-deduped), or revert when `revert_on_failure` | verify run + postmortem issue | See [Postmortem lifecycle](#postmortem-lifecycle) |
| Stage red for no visible reason | look at the gate step order: kill-switch → contract → real work | `contract.json` artifact (`ai-sdlc-contract-<job>-<attempt>`) — resolved values + per-field source | Missing/invalid `.ai-sdlc.yaml`, unreadable org baseline (`org_policy: unavailable` — warning only, never bricks), or missing App secrets |

## Escalation and re-arm

`needs-human` is the universal escape valve: it overlays any pre-dispatch
state, vetoes new dispatch (`triggers.skip_labels`) and PR merge
(`merge.skip_labels`), but does **not** abort an in-flight run.

Recovery pattern for an issue:

```bash
gh issue edit N --remove-label needs-human --remove-label scc:failed \
                --add-label ready-to-implement   # re-arms dispatch (T7)
```

For a PR: remove `needs-human` after the manual fix lands on the head
branch — the new head re-triggers checks, and the sweep feeds the PR back
to the merge gate within ~30 min (or dispatch `ai-sdlc-sweep`).

The trigger is consumed on dispatch (`−ready-to-implement +scc:queued`),
so re-arming always means re-applying it — never assume it survived.

## Artifacts

Canonical names, locations and retentions are normative in
`spec/run-evidence.md` (enforced by `tools/check-run-evidence.py`).
Per-run uploads (14-day retention unless noted there):

| Artifact | Content |
|---|---|
| `ai-sdlc-ledger-<stage>-<id>-<attempt>` | sealed hash-chained run ledger — per-stage `{stage, data_sha256, prev, hash}` entries + seal record; verify offline with `run-ledger.js verify`, query with `scripts/audit-query.js` |
| `ai-sdlc-contract-<job>-<attempt>` | `contract.json` — resolved `.ai-sdlc.yaml` + per-field source (`input`/`contract`/`org`/`org-pin`/`default`) + `org_policy` status; `error.txt` when validation failed |
| `ai-sdlc-run-<issue>` | implement run bundle — `ai-sdlc-run.json` (exit/model/fallback summary), `scc-audit.jsonl` (engine audit log), `scc-manifest.json` (engine manifest), prompt files |
| `scc-audit.primary.jsonl` / `scc-manifest.primary.json` | the failed primary-model run's evidence when the fallback fired |
| `ai-sdlc-review-<pr>-<attempt>` | review bundle — `ai-sdlc-review-verdict.json`, bounded diff, external signal, audit log |
| `ai-sdlc-review-fix-<pr>-attempt-<n>` | review-fix engine run |
| `ai-sdlc-ci-repair-<pr>-attempt-<n>` | repair bundle — `ci-repair-manifest.json` (`failure.signature`, `rootCause`, `runbookHit`), prompt, audit log, `runbook.json` |
| `injection-audit.jsonl` | sanitizer telemetry — pattern names + counts, never matched text |

Marker comments are the durable cross-run state: `<!-- ai-sdlc:ledger-head
<64-hex> -->` (chain recovery on the PR), `<!-- ai-sdlc:review sha=… -->`
(per-head verdict), `<!-- ai-sdlc:ci-repair attempt=N sha=… sig=… -->`
(repair budget + stall signature), `<!-- ai-sdlc:repair-signal sha=… -->`,
`<!-- ai-sdlc:label-veto -->` (janitor repair), `<!-- hermes:refinement
fp:"…" -->` (DoR fingerprint).

## Kill-switch

One action revokes autonomy fleet-wide; any engaged channel denies
(spec/kill-switch.md). Channels, evaluated in order:

1. `vars.AI_SDLC_KILL` / `vars.AI_SDLC_OFF` — repo or org Actions variable.
   Truthy engages, falsy clears, **any other non-empty value engages**
   (fail-closed on typos).
2. Open `ai-sdlc:off` issue in the control repo
   (`vars.AI_SDLC_CONTROL_REPO`, default = the repo itself) — halts every
   enrolled repo pointing there.
3. Open `ai-sdlc:off` issue in the target repo — per-repo kill.

Canonical engage/release (audited — actor + timestamp on the issue
timeline + run-ledger entry):

```bash
gh workflow run ai-sdlc-kill.yml -f action=engage  -f reason="incident-42"   # control repo
gh workflow run ai-sdlc-kill.yml -f action=release -f reason="resolved"
```

Equivalent manual signals: open/close the `ai-sdlc:off` issue by hand, or
`gh variable set AI_SDLC_KILL=true`.

Semantics operators need:

- **Fail-closed** — a channel that can't be read (API error, missing
  control repo) denies: verdict `unreadable`, autonomy OFF.
- **Per-run check, no teardown** — in-flight engine runs finish on their
  budget and halt at the next gate; a PR opened during a kill is never
  merged or repaired while engaged.
- **Gated stages**: intake, implement, automerge, ci-repair, sweep,
  verify → hard-fail; review → soft-skip (`decision=skip`, never red-marks
  human PRs); classify is deliberately ungated (advisory labels only).
- Releases resume on the next event — nothing needs manual re-dispatch;
  the sweep picks PRs back up within ~30 min.

## Postmortem lifecycle

Post-merge verify failure files a **canonical postmortem issue**
(`scripts/postmortem.js`; spec/postmortem-lifecycle.md), deduped by
signature hash over `component + class + location` carried in a hidden
body marker:

- **Dedup** — an open canonical issue always absorbs recurrences (comment
  `<!-- ai-sdlc-postmortem-recurrence -->`); a closed one absorbs them for
  `postmortem.dedup_window_days` (90) from `closedAt`.
- **Reopen** — post-close recurrences ≥ `postmortem.reopen_min` (default 2)
  reopen the canonical issue.
- **Runbook ingest** — closing a postmortem feeds the failure mode into
  `docs/runbooks/failure_modes.md`, which ci-repair consults
  (`scripts/failure-runbook.js`) — a matching signature prepends the
  recorded root cause + fix to the repair brief.
- The same postmortem machinery latches the main-guard sentinel halt
  (above).

## Contract drift

`ai-sdlc-drift` (`scripts/contract-drift.js`, scheduled daily) compares
declared `.ai-sdlc.yaml` with observed runs/labels/merges and files one
deduped issue per repo when they diverge — treat it as the contract-audit
alarm: either fix the contract or the behavior, then close.

Sweep-side drift repair is label-only (label contradictions, above); it
never dispatches and never picks winners outside the precedence order.
