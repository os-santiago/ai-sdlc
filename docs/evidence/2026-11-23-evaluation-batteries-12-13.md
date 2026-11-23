# Evaluation batteries 12 and 13: the intake freshness window, 2026-11-23

**Scope:** as in [batteries 8 to 11](2026-11-23-evaluation-batteries-8-11.md):
`os-santiago/ai-sdlc-sample`, trigger label only, two attempts, a 15-minute
limit per attempt, and the same local agent-CLI model service.

## Summary

| Run | Images | Issues | First attempt | Within two attempts |
|---|---|---|---|---|
| Battery 12 | `55739d6`, an installation already tracking 19 issues | 11 | 0 | 2 (18%) |
| Battery 13 | `814aeb5`: plus #753, fresh installation | 10 | 5 (50%) | 7 (70%) |

The reports are [battery 12](2026-11-23-evaluation-battery-12.json) and
[battery 13](2026-11-23-evaluation-battery-13.json). Successful deliveries took
6.5 to 9.8 minutes.

## Battery 12: the degradation, named

Battery 12 ran on the installation of battery 11, starting two hours after
installation. 11 of its 22 attempts stopped at
`model-authorization-publication-snapshot`. That name comes from #751: the
broker's re-authorization before publishing a model response refused because
the issue snapshot evidence was not fresh. This is the same degradation that
stopped battery 10 about three hours after its installation.

**Cause, measured on the installation:**

- **The window.** Execution, verification and publication evidence requires
  the intake watermark and the issue's observation to be younger than two
  poll intervals: 60 s at the default 30 s.
- **The scan.** Each intake scan re-read every issue it had ever tracked,
  with two issue reads, the label timeline, and the base branch when
  eligible. Delivered issues stay open with the trigger label removed, so a
  scan grew with every delivery.
- **The measurement.** With 19 tracked issues, a scan took about 25 s.
  Sampling every 5 s, the oldest observation reached 55–58 s just before
  each refresh, so any extra latency pushed active work out of the window.

**Fix (#753):** the intake no longer re-reads a tracked issue that the listing
shows closed or without the trigger label when the issue is all of these:
- recorded as ineligible;
- recorded under the current configuration and authority revision;
- unchanged in its request content.

An open, labeled issue is always read. Two audit rounds narrowed the rule
from comparing `updated_at` to comparing request content.

## Battery 13: the fix measured

A sampler recorded the intake every 30 s during the whole battery (230
samples):

| Measurement | Battery 12 installation (19 tracked) | Battery 13 with #753 (up to 10 tracked) |
|---|---|---|
| Issue observations refreshed per minute | All tracked issues | At most 2 |
| Oldest observation of an eligible issue | 55–58 s | At most 35 s (median 18 s) |
| Watermark age | Up to about 58 s | At most 37 s |
| `...-snapshot` stops | 11 of 22 attempts | 0 of 15 |

| Issue | Attempt 1 | Attempt 2 |
|---|---|---|
| #149 | merged, 8.0 min | |
| #150 | merged, 7.6 min | |
| #151 | execution `execution-exit-race` | merged, 9.8 min |
| #152 | execution `worker-exit` code 22 (step budget) | review `model-mailbox-read` |
| #153 | merged, 8.0 min | |
| #154 | review `model-drive-observe` | execution `execution-read` |
| #155 | execution `execution-read` | verification `refused-candidate-failed`, after `cleanup-config-observe-request` |
| #156 | merged, 9.8 min | |
| #157 | review `model-drive-observe` | merged, 6.5 min |
| #158 | merged, 9.8 min | |

## What the remaining stops point to

With the freshness window fixed, most remaining stops in battery 13 are
transport steps between the controllers and the sandbox pods:
- reading the mailbox (`execution-read`, `model-mailbox-read`);
- observing the worker (`model-drive-observe`);
- an exit racing a read (`execution-exit-race`);
- the confirmation GET of a cleanup (`cleanup-config-observe-request`).

Each is now named, which earlier batteries could not do. They are the next
area to investigate on this single-node evaluation cluster.

## Limits

- One small repository, one model path, and a single-node kind cluster
  sharing its host with the model service.
- Battery 13 ran on a fresh installation. It shows the window holding with
  up to 10 tracked issues, not over many hours of growth.
