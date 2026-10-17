# Trust Ladder — progressive autonomy levels (spec)

Autonomy is earned incrementally, not granted. This document is the
normative ladder every AI-SDLC deployment shares: six levels, the
evidence that gates promotion, the conditions that force demotion, and
the contract-PR mechanics that move a repo between rungs. The per-repo
ceiling lives in `.ai-sdlc.yaml` (`autonomy.level`); the thresholds its
evidence is measured against live in the contract's `promotion` and
`demotion` blocks — see [`ai-sdlc.schema.json`](ai-sdlc.schema.json).
(Earlier drafts named this surface `autonomy.evidence`; the landed
contract fields are the top-level `promotion.*` / `demotion.*` blocks,
issue #211.)

## Levels

| Level | Name | What the pipeline may do |
|---|---|---|
| L0 | `shadow` | Observe only. Reports what it *would* do; never mutates (no branches, PRs, labels, comments beyond dry-run logs). |
| L1 | `suggest` | Comment-only. Proposes diffs/plans as issue comments; humans implement. |
| L2 | `auto-pr` | Implements issues and opens PRs. Never merges. |
| L3 | `auto-merge-low` | Opens PRs; merges them only when risk label is `pr:risk-low` and all required checks are green. |
| L4 | `auto-merge-all` | Opens and merges PRs on green checks at any risk level below `pr:risk-critical`. Critical still requires `pr:risk-accepted`. |
| L5 | `auto-deploy` | L4 plus post-merge deploy/verify stage. Requires a verified rollback path configured. |

The configured level is a **ceiling**: a pipeline may operate below it
(freezes, incidents, model outages), never above it.

## Evidence model

Promotion and demotion are decided over a rolling **window** of the last
N *completed pipeline runs* (`promotion.window`, `demotion.window` —
units are runs, not days; 10 is the minimum evidence base before any
promotion is considered). A run is completed when it reaches a terminal
run-state (`labels.pr-opened`, `labels.failed`, `labels.no-changes` per
[labels.md](labels.md)); in-flight runs are not evidence either way.

### Defined terms

| Term | Definition |
|---|---|
| Clean run | A completed run that reached its normal terminal state, required no `escalation.label` application attributable to it, and left no unaddressed failure signal. Pre-merge rungs (`shadow`, `suggest`) produce no merges — the clean run is their evidence unit. |
| Clean merge | A pipeline-opened PR merged with all `verify.required_checks` green on the merged head SHA, never subsequently reverted, and carrying no escalation attributable to its run. The evidence unit for merge rungs (`auto-merge-low` and up). |
| Escalation event | An application of `escalation.label` (default `needs-human`) attributable to a run inside the window — see §Escalation semantics. |
| Sentinel halt | A post-merge gate firing on the default branch: the `verify` stage's commands failing, or a required-check regression attributable to a pipeline-merged change — evidence that a "clean" merge was not clean. |
| Postmortem recurrence | A postmortem `signatureHash` (spec/postmortem-lifecycle.md) recurring at or above its reopen threshold. |

### The four signal types

A promotion verdict consumes four composable evidence signals (the
taxonomy of issue #179). Merge count alone is the weakest signal;
`promotion.required_signals` declares which signals the contract
requires — the default is all four, `track-record` is mandatory, and it
may never be the only required signal:

| Signal id | Source | What it attests |
|---|---|---|
| `track-record` | GitHub API (PR state, labels, check runs) + the runtime's own run ledger | Historical behavior over the window: clean merges/runs, escalation rate, repair rounds, reverts. Necessary; never sufficient alone. |
| `static-config` | Lint-grade analysis of the contract, engine config, and agent instructions | The declared configuration is well-formed and free of known-dangerous patterns — schema-valid contract, no forbidden flags, injection-defense surfaces present (spec/injection-defense.md). |
| `behavioral-eval` | Bounded eval suite over golden tasks | The engine still performs the work the repo expects — quality evidence independent of production history. |
| `audit-trail` | Run-ledger integrity check | The evidence itself is complete and untampered — a hash-chained ledger with no gaps. |

Each signal reports `pass`, `fail`, or `unknown` (not instrumented).
`unknown` **fails closed** for pipeline-initiated promotion: a runtime
without evals or a chained ledger cannot self-accredit — it reports the
gap instead of skipping the signal silently. Track record by itself
never suffices; the schema enforces it (`required_signals` requires
`track-record` plus at least one other signal).

### Metric gates

All track-record metrics are derivable from the GitHub API (PR state,
labels, check runs) plus the pipeline's own run ledger. The runtime
SHOULD emit them as a periodic report; consumers may compute them per
repo.

| Metric | Definition | Promotion gate (to next level) | Demotion trigger (one level down) |
|---|---|---|---|
| Merge success rate | merged PRs / opened PRs | ≥ 90% | < 70% |
| Revert rate | PRs reverted or closed-unmerged after merge / merged PRs | ≤ 5% | > 15% |
| Escalation rate | runs ending in `needs-human` / total runs | ≤ 10% | > 30% |
| Repair rounds per PR | mean CI-repair rounds before merge | ≤ 1.5 | > 3 |
| `pr:risk-critical` frequency | critical-labeled PRs / merged PRs | ≤ 10% | — (informational; criticals never auto-merge anyway) |

### Thresholds per rung

The scalar contract fields (`promotion.clean_merges_threshold`,
`promotion.window`) are the defaults for every target level;
`promotion.overrides.<level>` tightens them per rung. The table below is
the normative **floor** — a contract MAY tighten thresholds but MUST NOT
set a value below its rung's floor:

| Target rung | Evidence unit | Floor | Additional gates |
|---|---|---|---|
| `suggest` | clean runs | 3 | zero escalations in window |
| `auto-pr` | clean runs | 5 | zero escalations in window |
| `auto-merge-low` | clean merges | `clean_merges_threshold` (floor 3) | zero escalations; every promotion gate in the metric table passes |
| `auto-merge-all` | clean merges | 10 | zero escalations; every metric gate passes |
| `auto-deploy` | clean merges | 20 | zero escalations; zero postmortem recurrences in window; a verified rollback path configured |

Promotion is single-step: the gate proposes the *next* rung only —
evidence never skips levels.

## Promotion and demotion proposals — the contract PR

Autonomy moves through the same governance surface as everything else.
When a gate trips, the runtime files a normal, reviewed PR against its
own `.ai-sdlc.yaml` — the pipeline proposes, a human disposes.

- **Promotion PR**: title `feat(promotion): bump autonomy level to
  <level>`; the diff MUST change only `autonomy.level` — never
  thresholds, labels, or other contract fields in the same proposal.
- **Demotion PR**: title `fix(demotion): lower autonomy level to
  <level>`; the same single-field diff rule applies. A demotion triggered
  by a sentinel halt additionally carries `demotion.priority_label`
  (default `priority:P0`).
- **Evidence summary in the body (MUST)**: the resolved window and
  thresholds (contract defaults plus any `promotion.overrides`), the
  per-signal verdict table for all four signals (including `unknown`),
  the raw counts (clean merges/runs, escalations, repair rounds,
  reverts), and links to the underlying runs, PRs, and ledger entries.
  A reviewer must be able to audit the claim without recomputing it.
- **Risk classification**: `autonomy.*` changes classify
  `pr:risk-critical` regardless of diff size ([risk-taxonomy.md](risk-taxonomy.md)
  — policy-file surface) and therefore require human `pr:risk-accepted`
  even at `auto-merge-all`. The initiative is algorithmic; the authority
  stays human.
- **Advisory until merged**: an unmerged proposal changes nothing — the
  effective level is the merged `autonomy.level`, floored by the org
  baseline. Manual contract edits remain valid: a human may lower the
  level at any time, and may merge a proposal or raise the level by
  direct edit — but a raise, by hand or by proposal, SHOULD be backed by
  evidence that passes the gate. The gate is the standard every raise is
  measured against.
- **Dedup and staleness**: at most one open promotion proposal and one
  open demotion proposal per repo — re-evidence updates the open
  proposal rather than stacking new ones. If the sliding window drops
  the evidence below the gate before merge, the runtime SHOULD close the
  stale proposal with a note; a demotion proposal whose trigger resolves
  (revert completed, freeze cleared) SHOULD close likewise.
- **Org ceiling**: the runtime MUST NOT file a promotion above the
  resolved org-baseline ceiling ([org-policy.md](org-policy.md)) — a
  merged proposal would clamp to the floor anyway; propose up to the
  ceiling, never past it.
- **Frozen repos**: while the variable named by `autonomy.freeze_env` is
  set, evidence keeps accruing but no promotion proposals are filed.
  Demotion proposals still file — a freeze is containment, not amnesty.

## Escalation semantics — what counts against the record

- An escalation event is an application of `escalation.label`
  (`needs-human` by default) to a pipeline-owned issue or a
  pipeline-opened PR, attributable to a run inside the evidence window.
- **Attribution is by run, not by actor**: a `needs-human` a human
  applies to a pipeline PR counts — the automation needed a human. The
  same label on an issue the pipeline never touched is not evidence.
- A policy-fired hold that a human later clears under
  `clear_guard: policy:accepted` ([labels.md](labels.md) T6) was a false
  positive — it SHOULD NOT count against the record. Every other
  escalation counts even when resolved quickly: the signal is that
  intervention was needed at all.
- Escalations are shared evidence: the same event stream feeds
  `demotion.escalation_threshold` and the promotion gate's
  zero-escalation clauses — an escalation inside the window resets the
  rung floor's "zero escalations" requirement.

## Demotion and freeze

- **Demotion proposals**: breaching any demotion trigger — a metric
  bound, `demotion.escalation_threshold` escalations inside
  `demotion.window`, a sentinel halt, or a postmortem recurrence — files
  a demotion PR lowering `autonomy.level` one rung, labeled
  `demotion.priority_label` (default `priority:P0`) when the trigger was
  a sentinel halt.
- **Immediate containment**: a trigger indicating active harm (sentinel
  halt, postmortem recurrence) SHOULD additionally set the
  `autonomy.freeze_env` variable (default `AI_SDLC_FREEZE`) — the freeze
  pins the effective level to `shadow` *now*; the demotion PR makes the
  drop durable once a human reviews it.
- **Freeze**: setting the env named by `autonomy.freeze_env` pins the
  effective level to `shadow` until cleared. Freeze is orthogonal to the
  configured level.
- **Manual override**: a human may always lower the level; raising above
  the measured evidence requires the promotion gate to pass.

## Per-repo configuration

```yaml
autonomy:
  level: auto-merge-low        # ceiling for this repo
  merge_requires_checks: true
  freeze_env: AI_SDLC_FREEZE

promotion:                     # evidence gates for promotion proposals
  clean_merges_threshold: 3    # clean merges in window before proposing
  window: 30                   # rolling window in completed runs (min 10)
  required_signals:            # default: all four; track-record is
    - track-record             # mandatory and never sufficient alone
    - static-config
    - behavioral-eval
    - audit-trail
  overrides:                   # per-target-rung tightening (floors above)
    auto-merge-all:
      clean_merges_threshold: 10

demotion:                      # demotion triggers
  escalation_threshold: 1      # escalations in window before proposing
  window: 30
  priority_label: priority:P0  # applied to sentinel-halt demotion PRs
```

Newly adopted repos SHOULD start at `shadow` or `suggest` and let
evidence drive promotion.

## Notes for implementers

- Reference implementation surface: `src/trust/promotion.py` computes
  the track-record gate (issue #201); `src/trust/promotion_pr.py`
  generates the promotion contract PR (issue #202); the demotion
  counterpart follows the same contract (issue #209).
- The evidence engine is deterministic: identical inputs MUST produce
  identical verdicts. Evidence computation never routes through the
  model — signals are queries and counts, not judgments.
