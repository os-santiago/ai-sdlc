# Evaluation batteries 8 to 11, 2026-11-23

**Scope:** `os-santiago/ai-sdlc-sample`, a small Go module. Each issue was
delivered by `main` from its trigger label only, with up to two attempts and a
15-minute limit per attempt. No stage was advanced by hand. The runner is
[`tools/evaluation/run_battery.py`](../../tools/evaluation/README.md).
Earlier batteries are in
[batteries 2 to 7](2026-11-22-evaluation-batteries-2-3-openshift.md).

**Model service:** the same local OpenAI-compatible service in front of an
agent CLI as in batteries 5 to 7, with extended thinking on (#737).

## Summary

| Run | Images | Issues | First attempt | Within two attempts |
|---|---|---|---|---|
| Battery 7 | `6bb0a94` | 8 | 1 (13%) | 4 (50%) |
| Battery 8 | `c4a4522`: per-call ceiling (#739) | 10 | 5 (50%) | 8 (80%) |
| Battery 9 | `4f613eb`: plus #740, #742–#744 | 10 | 6 (60%) | 9 (90%) |
| Battery 10 | `4f613eb`, stopped early (see below) | 5 of 11 | 1 | 2 |
| Battery 11 | `55739d6`: plus #746–#749, #751, fresh installation | 9 | 6 (67%) | 8 (89%) |

The reports are [battery 8](2026-11-23-evaluation-battery-8.json),
[battery 9](2026-11-23-evaluation-battery-9.json),
[battery 10](2026-11-23-evaluation-battery-10.json) (partial) and
[battery 11](2026-11-23-evaluation-battery-11.json).

Successful deliveries took 6.5 to 12.8 minutes from label to merge.

## What changed the rate

- **One `model-provider` stop since #739** (battery 11), against 7 and 9 in
  batteries 6 and 7, where calls ran past the gateway's benchmarked timeout. Each call may now run up
  to the upstream ceiling, bounded by the broker's remaining wait.
- **Earlier failures were delivered.**
  - #81 (string escaping) failed twice in battery 8 and was delivered in
    battery 9.
  - #87 (line splitting) failed six times across batteries 8 to 10 and was
    delivered in battery 11.
  - #119 failed twice during battery 10's authorization episode and was
    delivered on its first attempt in battery 11.

## Stops and their causes

| Stop as logged | Count | Cause found |
|---|---|---|
| verification `unknown`, now `refused-candidate-failed` | 8 | The candidate's own new tests fail. Two refused candidates were extracted and run offline: one test file did not compile, and the other implementation disagreed with its own test on an edge case. The generator has `run_shell` but no toolchain (#745). |
| execution `model-publication-before-observation ... workerExitCode=0` | 3 | The generation hit its 600 s `maxSeconds`. A pod-status recorder showed the worker killed with exit code 124 at exactly 600 s, with a model call in flight. `workerExitCode=0` only meant "not a worker-exit stop". It is now named `model-publication-worker-exit-124` (#749). The budget is fixed in the installer (#750). |
| execution `worker-exit ... workerExitCode=22` | 2 | SCC `BUDGET_EXCEEDED` at the 24-step budget, after 24 and 16 model calls (#750). |
| execution `model-authorization-publication` | 4 | All in battery 10, from 03:29 UTC on, about three hours after that installation. From then on, four of the five attempts stopped at the broker's re-authorization before publishing a response; the fifth reached verification and stopped at `baseline`. A fresh installation delivered the first of the same issues (battery 11). The failing check is now named (#751). |
| execution or review `cleanup-pod` | 3 | Once the pod deletion failed 3–6 s after the pod had completed normally. The failing step is now named (#748); in battery 11 it was `observe-request`, a transport failure of the confirmation GET. The battery runner then removed the label while the runtime was still handing the candidate to verification (#747). |
| execution `model-provider` | 1 | One call over its window (battery 11). |
| review `model-response-policy` | 1 | The reviewer's final answer had no `VERDICT` line (74 characters). |
| review `model` | 1 | The review drive loop did not name its stop. Now named `model-drive-*` (#746). |
| verification `baseline` | 1 | Battery 10, during the authorization episode. |

## Battery 11 (images `55739d6`, fresh installation)

| Issue | Attempt 1 | Attempt 2 |
|---|---|---|
| #119 | merged, 10.1 min | |
| #120 | execution `worker-exit` code 22 (step budget) | merged, 9.8 min |
| #121 | merged, 8.7 min | |
| #122 | merged, 7.9 min | |
| #123 | merged, 6.5 min | |
| #124 | timeout: review asked for changes, repair handoff stopped; earlier `cleanup-pod-observe-request` | verification `refused-candidate-failed` |
| #125 | merged, 10.8 min | |
| #126 | merged, 7.0 min | |
| #87 | execution `model-provider` | merged, 9.8 min |

- The newly named cleanup step identified one cause:
  `cleanup-pod-observe-request` means the confirmation GET of a pod deletion
  failed at the transport level, not that the pod stayed.
- No authorization stop occurred in the first two hours after installation.

## The runner ended attempts the runtime was still completing

When an execution captures its candidate but a cleanup step fails, the runtime
records the execution as stopped and then hands the candidate to verification
once cleanup completes. The runner counted that stop as the attempt's failure
and removed the trigger label, which revoked the in-flight verification. #747
now waits for the handoff, unless it can no longer happen. Four audit rounds
shaped the rule. The two "execution cleanup after a captured candidate" stops of
battery 6 were most likely the same artifact.

## Source reads and the anonymous forge quota

Every production source read is anonymous (#741): worker preflight,
verification, review and publication. Each read cost three API calls, so a
delivery attempt cost about 12, against the forge's 60 anonymous calls per
hour per address. During battery 8 the host showed 24 of 60 calls used 40
minutes into the window. #743 lowers this to about 8 calls per attempt.
Credentialed reads remain a design decision.

## Sandbox pod deletion

During battery 8, executor and reviewer pods were removed 0.6 to 2.3 seconds
after deletion, well inside the 10-second confirmation budget.

## Fixes merged during these batteries

| Change | Effect |
|---|---|
| #740 | The repair composition fixture scopes CI's read token. It was the most frequent `review-kind` CI flake (#685), most likely unauthenticated fetches hitting the rate limit. |
| #742 | Verification and review name a forge rate limit. |
| #743 | An anonymous source read downloads the pinned archive directly: two API calls instead of three. |
| #744 | A verification refusal reads `refused-<reason>`. |
| #746 | The review drive loop names its stops, and a reviewer exit 0 before its verdict is named. |
| #747 | The battery runner follows a handed-off candidate instead of ending the attempt. |
| #748 | Execution and review cleanup stops name the failed removal step. |
| #749 | A response that finds the worker gone names the worker's exit code. |
| #751 | Model call authorization names the check that refused. |

## Open items

- #745: give the generator the manifest's toolchain. A spike ran the pinned
  Go toolchain in the worker's base image offline in 3.5 s; the design needs
  owner decisions.
- #750: a configurable generation budget for slow model endpoints.
- #741: credentialed source reads.
- The authorization episode in battery 10: battery 12 runs through the third
  hour after installation, now with named steps.

## Limits

- **Sample size:** one small repository and one model path. The rates do not
  predict other repositories or models.
- **Model service:** a local agent-CLI simulator with a latency tail, not a
  hosted API.
- **Issue difficulty varies:** each battery adds new issues and repeats
  earlier failures.
