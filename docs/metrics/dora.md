# DORA metrics — os-santiago/ai-sdlc (last 30d)

_Generated 2026-10-17T18:50:53.035Z · base `main` · window `2026-09-17T18:50:53.035Z` → `2026-10-17T18:50:53.035Z`_

> Definitions (epic #88): deployment = merge to the base branch (release tags secondary);
> lead time = `ready-to-implement` label applied → merge (fallback: PR creation);
> change failure = merge followed by red main, revert or non-converging repair;
> MTTR = main red → green. **Agentic** = pipeline-authored PRs; **human** = the rest.

## Comparison — agentic vs human

| Metric | Agentic | Human | All |
|---|---|---|---|
| Deployment frequency | 92 merges · 21.47/wk | 22 merges · 5.13/wk | 114 merges · 26.6/wk |
| Lead Time for Changes | median 17m · p90 8.8h (n=92) | median 9m · p90 19.6h (n=22) | median 16m (n=114) |
| Change Failure Rate | 14.1% (13/92) | 9.1% (2/22) | 13.2% (15/114) |
| MTTR | mean 8m (4 ep) | mean 0m (1 ep) | mean 7m (5 ep) |

## Deployment Frequency

Merges to `main` in the window — agentic 92, human 22, total 114 (3.8/day). Release tags in window (secondary): 0.

## Lead Time for Changes

Hours from `ready-to-implement` (or PR creation when the label is absent — `basis`) to merge.

| PR | Author type | Hours | Basis | Issue |
|---|---|---|---|---|
| #10 | agentic | 1.2h | ready_label | #2 |
| #11 | human | 0m | pr_created | #1 |
| #12 | agentic | 0m | pr_created | #8 |
| #13 | agentic | 1m | pr_created | #9 |
| #14 | human | 0m | pr_created | — |
| #16 | agentic | 25m | pr_created | #15 |
| #17 | human | 2.6h | pr_created | — |
| #31 | human | 25m | pr_created | — |
| #38 | agentic | 39m | pr_created | #23 |
| #39 | human | 56m | pr_created | — |
| #40 | agentic | 4m | pr_created | #29 |
| #41 | agentic | 14m | pr_created | #27 |
| #42 | agentic | 15m | pr_created | #24 |
| #43 | agentic | 2m | pr_created | #22 |
| #45 | agentic | 57m | ready_label | #18 |
| #46 | agentic | 22.1h | ready_label | #44 |
| #47 | agentic | 8.8h | ready_label | #26 |
| #48 | agentic | 10.2h | ready_label | #28 |
| #50 | human | 15m | pr_created | — |
| #51 | agentic | 18m | ready_label | #49 |
| #53 | agentic | 20m | ready_label | #52 |
| #54 | human | 37m | pr_created | — |
| #56 | human | 17m | pr_created | — |
| #57 | agentic | 1.5h | ready_label | #55 |
| #58 | human | 12m | pr_created | — |
| #59 | human | 1m | pr_created | — |
| #60 | human | 1m | pr_created | — |
| #61 | human | 2m | pr_created | — |
| #62 | human | 1m | pr_created | — |
| #63 | agentic | 1.4h | ready_label | #55 |
| #64 | human | 1m | pr_created | — |
| #66 | human | 1m | pr_created | — |
| #70 | human | 1m | pr_created | — |
| #71 | agentic | 34m | ready_label | #69 |
| #73 | agentic | 19m | ready_label | #67 |
| #74 | agentic | 47m | ready_label | #68 |
| #76 | agentic | 3.2h | ready_label | #20 |
| #77 | agentic | 2.9h | ready_label | #19 |
| #83 | agentic | 11m | ready_label | #81 |
| #84 | agentic | 7m | ready_label | #79 |
| #85 | agentic | 20m | ready_label | #80 |
| #232 | agentic | 15m | ready_label | #223 |
| #233 | agentic | 3.1d | ready_label | #231 |
| #234 | agentic | 3.1d | ready_label | #226 |
| #236 | agentic | 10m | ready_label | #216 |
| #237 | agentic | 6m | ready_label | #213 |
| #238 | agentic | 19m | ready_label | #235 |
| #239 | agentic | 47m | ready_label | #214 |
| #240 | agentic | 28m | ready_label | #216 |
| #241 | agentic | 6m | ready_label | #215 |
_… 64 more not shown_

## Change Failure Rate

Failed merges / total merges × 100. Signals: `red_main` (base run red, attributed to the merge), `revert` (merged revert PR), `non_converging_repair` (escalated hold or red streak still open).

| PR | Author type | Signals | Detail |
|---|---|---|---|
| #46 | agentic | non_converging_repair |  |
| #73 | agentic | non_converging_repair |  |
| #84 | agentic | non_converging_repair |  |
| #233 | agentic | non_converging_repair |  |
| #234 | agentic | non_converging_repair |  |
| #250 | agentic | non_converging_repair |  |
| #252 | agentic | non_converging_repair |  |
| #257 | human | red_main |  run 38973129240 |
| #281 | agentic | red_main |  run 39050220159 |
| #283 | agentic | non_converging_repair |  |
| #285 | agentic | non_converging_repair |  |
| #289 | agentic | red_main |  run 39047215646 |
| #298 | agentic | red_main |  run 39047710504 |
| #304 | agentic | red_main |  run 39047495194 |
| #306 | human | non_converging_repair |  |

## MTTR — Time to Restore

Base-branch red → green episodes (gate workflows: all). Episodes: 5 · recovered 5 · ongoing 0 · unattributed 0.

| Started | Recovered | Hours | Attributed to | Workflow |
|---|---|---|---|---|
| 2026-10-16T22:01:29.000Z | 2026-10-16T22:01:32.000Z | 0m | #257 | ai-sdlc-sweep |
| 2026-10-17T15:56:23.000Z | 2026-10-17T16:00:17.000Z | 4m | #289 | ai-sdlc (self) |
| 2026-10-17T16:00:30.000Z | 2026-10-17T16:03:05.000Z | 2m | #304 | ai-sdlc (self) |
| 2026-10-17T16:03:14.000Z | 2026-10-17T16:27:16.000Z | 24m | #298 | ai-sdlc (self) |
| 2026-10-17T16:38:20.000Z | 2026-10-17T16:40:21.000Z | 2m | #281 | ai-sdlc-sweep |

_Scanned 114 merges (92 agentic / 22 human), 200 base runs, 112 timelines._

_Regenerated on schedule by `ai-sdlc-dora.yml` from `scripts/dora-metrics.js`._
