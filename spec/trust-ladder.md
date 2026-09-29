# Trust Ladder — progressive autonomy levels (spec)

Autonomy is earned incrementally, not granted. This document is the
normative ladder every AI-SDLC deployment shares: six levels, the
observable signals that gate promotion, and the conditions that force
demotion. The per-repo ceiling lives in `.ai-sdlc.yaml` (`autonomy.level`)
— see [`ai-sdlc.schema.json`](ai-sdlc.schema.json).

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

## Transition criteria

Promotion and demotion are driven by metrics over a rolling window of the
last N completed pipeline runs (default N = 20, minimum N = 10 before any
promotion is considered):

| Metric | Definition | Promotion gate (to next level) | Demotion trigger (one level down) |
|---|---|---|---|
| Merge success rate | merged PRs / opened PRs | ≥ 90% | < 70% |
| Revert rate | PRs reverted or closed-unmerged after merge / merged PRs | ≤ 5% | > 15% |
| Escalation rate | runs ending in `needs-human` / total runs | ≤ 10% | > 30% |
| Repair rounds per PR | mean CI-repair rounds before merge | ≤ 1.5 | > 3 |
| `pr:risk-critical` frequency | critical-labeled PRs / merged PRs | ≤ 10% | — (informational; criticals never auto-merge anyway) |

All metrics are derivable from the GitHub API (PR state, labels, check
runs) plus the pipeline's own run ledger. The runtime SHOULD emit them as
a periodic report; consumers may compute them per repo.

## Demotion and freeze

- **Automatic demotion**: breaching any demotion trigger demotes one level
  immediately and posts a notice explaining which bound was crossed.
- **Freeze**: setting the env named by `autonomy.freeze_env`
  (default `AI_SDLC_FREEZE`) pins the effective level to `shadow` until
  cleared. Freeze is orthogonal to the configured level.
- **Manual override**: a human may always lower the level; raising above
  the measured evidence requires the promotion gate to pass.

## Per-repo configuration

```yaml
autonomy:
  level: auto-merge-low        # ceiling for this repo
  merge_requires_checks: true
  freeze_env: AI_SDLC_FREEZE
```

Newly adopted repos SHOULD start at `shadow` or `suggest` and let evidence
drive promotion.
