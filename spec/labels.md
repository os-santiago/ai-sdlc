# Label State Machine — intake exclusivity & flow taxonomy (spec)

Normative contract for issue labels across the pipeline: the canonical
states, who may apply or clear each label, which combinations are
forbidden, and what runtimes do when they observe drift. It binds the
human-facing intake labels (`ready-to-implement`, `needs-refinement`,
`needs-human`, the skip labels) and the machine-applied run-state labels
(`scc:*`) into one observable state machine.

Label names are **bindings, not constants**: `triggers.issue_label`,
`triggers.skip_labels`, `escalation.label` and the `labels:` map bind
canonical states to label strings per repo. The canonical state names in
this document are fixed; a repo that renames a label MUST rename it in
its `labels.state_machine` matrix too — janitors match strings, not
intent. (The reference deployment binds `needs-refinement` →
`scc:not-ready` via `labels.not-ready`.)

## States

One primary chain plus an orthogonal human hold:

```
new → triaged ────────────────────────────────▶ armed ──▶ in-progress ──▶ done
        │                                         ▲            │
        └── DoR fail ──▶ needs-refinement ─────────┘            └──▶ pr-opened |
                          (re-check pass re-arms)                    failed |
                                                                     no-changes

needs-human  — orthogonal hold: overlays any pre-dispatch state;
               vetoes dispatch (triggers.skip_labels) and merge
               (merge.skip_labels) while present
skip labels  — epic · wontfix · invalid · duplicate · question ·
               dependencies · umbrella · scc:child → terminal; never dispatch
```

| Canonical state | Default binding | Meaning |
|---|---|---|
| `triaged` | — (e.g. `priority:P*` applied) | scoped + prioritized; awaiting refinement or arming |
| `needs-refinement` | `needs-refinement` (ref impl binds `labels.not-ready` = `scc:not-ready`) | DoR failed; blocked until refined |
| `armed` | `ready-to-implement` (`triggers.issue_label`) | dispatch trigger present — the only dispatchable state |
| `in-progress` | `labels.queued` → `labels.in-progress` → `labels.pr-opened` | a run owns the issue |
| `needs-human` | `needs-human` (`escalation.label`) | orthogonal hold: vetoes dispatch AND merge while present |
| terminal | `labels.failed` / `labels.no-changes`; skip labels | no pipeline action without a new transition |

## Transition table

| # | From | Transition → label delta | Actor | Guard / effect |
|---|---|---|---|---|
| T1 | triaged | arm: `+ready-to-implement` | `authorized-labeler`, `human` | SHOULD satisfy DoR first; MUST strip any hold it displaces (T5/F-rules) |
| T2 | any | DoR fail: `+needs-refinement` | `refiner` | R1–R6 failure (spec/definition-of-ready.md); comment cites the failed rules |
| T3 | needs-refinement | re-check pass: `−needs-refinement +ready-to-implement` | `refiner` | a full DoR re-check MUST precede the arm; a `human` may clear + arm manually |
| T4 | armed | dispatch: `−ready-to-implement +labels.queued` | `dispatch` | the trigger is consumed — never left dangling |
| T5 | any pre-dispatch | hold: `+needs-human` | `admission-gate`, `escalation`, `human` | veto: applying the winner strips `ready-to-implement` / `needs-refinement` in the same operation |
| T6 | needs-human | unblock: `−needs-human` | `human`, `policy` re-evaluation | a policy-fired hold requires `policy:accepted` before clearing |
| T7 | needs-human | re-arm: `+ready-to-implement` | `authorized-labeler`, `human` | only after the hold clears |
| T8 | in-progress | engine exit: `+pr-opened` / `+failed` / `+no-changes` | `dispatch` | exit-code mapping per `docs/runtime.md` |
| T9 | any | escalate: `+needs-human` + cause comment | `escalation` | golden rule — no silent unresolved stall |
| T10 | any | structural: `+duplicate` / `+epic` / `+umbrella` / `+scc:child` | owning component (`dedup`, `triage`, `decomposer`), `human` | vetoes the trigger while present |

## Mutual exclusivity & precedence

Within the intake trio `ready-to-implement` ⊥ `needs-refinement` ⊥
`needs-human`, **at most one label is present**. The trigger additionally
never co-exists with a `triggers.skip_labels` entry or with an active
run-state label (`labels.queued`, `labels.in-progress`,
`labels.pr-opened`).

Precedence on conflict — blockers always outrank the dispatch trigger
(veto first, dispatch second):

1. `needs-human` — the human hold outranks everything, including
   `needs-refinement`
2. `needs-refinement` (the refinement-hold binding, e.g. `scc:not-ready`)
3. active run-state labels — a run already owns the issue
4. terminal skip labels
5. `ready-to-implement` — always the loser

The actor applying a winning label MUST remove the loser in the same
operation where feasible (one `--add-label W --remove-label L` call);
humans SHOULD do the same so drift never persists.

## Forbidden combinations (exhaustive)

Let T = `triggers.issue_label`, R = the refinement-hold binding, H =
`escalation.label`, S = `triggers.skip_labels`, Q/I/P = the active
run-state bindings. An issue's label set is invalid when any of these
holds:

| # | Combination | Winner (kept) | Loser (stripped) |
|---|---|---|---|
| F1 | T + H | H | T |
| F2 | T + R | R | T |
| F3 | H + R | H | R |
| F4 | T + s, for any s ∈ S | s | T |
| F5 | T + Q / I / P | the run-state | T |

Deliberately legal co-occurrences:

- `needs-human` + run-state: the hold vetoes *future* dispatch and PR
  merge; it does not abort an in-flight run.
- `ready-to-implement` + terminal run-state (`scc:failed`,
  `scc:no-changes`): the sanctioned re-arm path for retry.
- Within the run-state family the lifecycle order `queued → in-progress →
  pr-opened` resolves internal drift — the later state wins.

Machine-checkable form: `labels.state_machine.veto_pairs` in
`.ai-sdlc.yaml` enumerates `{winner, loser}` pairs; the effective
forbidden set is the declared pairs **∪** `{(s, T)}` for every s in
`triggers.skip_labels`.

## Drift rule — veto + repair, never silent skip

A forbidden combination is a **pipeline defect**, not a legitimate state.
On observing one, runtimes and janitors MUST:

1. **Veto** — never dispatch the issue; the blocker wins first.
2. **Repair** — strip the loser label; the winner survives.
3. **Log** — comment on the issue naming the observed pair and the
   repair, and emit a defect signal so the bug that produced the pair
   stays visible.

"Trigger + blocker" MUST NOT be treated as merely "skip this issue" —
vetoed-but-armed issues are exactly the silently-frozen queue this
contract exists to prevent (ref: Axel-DaMage/hermes#376).

## Actor vocabulary

| Token | Meaning |
|---|---|
| `human` | maintainer via UI/CLI — may apply/remove any issue label; ultimate arbiter |
| `authorized-labeler` | login ∈ `triggers.authorized_labelers` — may apply the trigger |
| `refiner` | DoR intake component — owns the refinement-hold label; re-checks on edit events |
| `admission-gate` | intake policy evaluation — fires `needs-human` when policy requires human judgment |
| `escalation` | any runtime stage applying `escalation.label` (implement, ci-repair, automerge) |
| `dispatch` | worker lifecycle — consumes the trigger; owns run-state transitions |
| `janitor` | drift sweeper (e.g. Hermes `issue_watcher`) — repairs forbidden combos |
| `dedup` / `triage` / `decomposer` | owning components of `duplicate` / `epic`, `umbrella` / `scc:child` |
| `policy` | org/policy layer — `policy:accepted` is the human acceptance label for policy-fired holds |

## Machine-checkable matrix

The contract carries the same rules in `labels.state_machine`
(`state_machine` is a reserved `labels:` key — not a renamable flow
state):

```yaml
labels:
  queued: scc:queued
  in-progress: scc:in-progress
  pr-opened: scc:pr-opened
  failed: scc:failed
  not-ready: scc:not-ready       # refinement-hold binding (canonical: needs-refinement)
  no-changes: scc:no-changes
  state_machine:
    veto_pairs:
      - {winner: needs-human, loser: ready-to-implement}
      - {winner: scc:not-ready, loser: ready-to-implement}
      - {winner: needs-human, loser: scc:not-ready}
      - {winner: scc:queued, loser: ready-to-implement}
      - {winner: scc:in-progress, loser: ready-to-implement}
      - {winner: scc:pr-opened, loser: ready-to-implement}
    owners:
      ready-to-implement: {set_by: [authorized-labeler, refiner], cleared_by: [dispatch, human]}
      scc:not-ready: {set_by: [refiner], cleared_by: [refiner, human]}
      needs-human: {set_by: [admission-gate, escalation, human], cleared_by: [human], clear_guard: policy:accepted}
      scc:in-progress: {set_by: [dispatch], cleared_by: [dispatch]}
      epic: {set_by: [triage, human]}
      duplicate: {set_by: [dedup, human]}
      umbrella: {set_by: [triage, human]}
      scc:child: {set_by: [decomposer]}
```

- `owners` is the write-authority map; `janitor` and a veto pair's
  winning actor are always implicit `cleared_by`. `clear_guard` names a
  label that must be present for a non-human clear — `policy:accepted`
  applies only when the hold was policy-fired.
- Runtimes SHOULD flag out-of-owner label writes they perform or
  observe; `human` is always authorized.

## Reference implementation — `issue_watcher` / janitor

- **skip = veto + repair**, not skip alone. A watcher that sees
  `ready-to-implement` + a blocker does not dispatch, strips the loser,
  comments, and logs the defect.
- Repair is label-only: the janitor never dispatches and never picks a
  winner outside the precedence order.
- Evaluation order: `needs-human` first, then the refinement hold, then
  run-state labels, then the trigger.
- Repairs are idempotent and bounded. If the same pair reappears after a
  repair (a human or a buggy loop re-adds the loser), the janitor repairs
  once more; on the next repeat it applies `needs-human` with cause
  `label-drift-loop` instead of fighting silently.
- Divergences between a runtime's label writes and this contract are
  bugs — report them, don't accommodate them.
