# Local model batteries 25–30: from 0 of 6 to 9 of 10 delivered, 2026-11-25

**Scope:**
- **Repository:** `os-santiago/ai-sdlc-sample`, a small Go repository.
- **Trigger:** the label only, with two attempts per issue.
- **Model path:** a local model through `--model-upstream`. Qwen3-Coder-30B-A3B Q4_K_M ran on llama.cpp (ROCm/HIP) on the reference laptop's integrated GPU, at about 11–13 generated tokens per second.
- **Cluster:** a fresh local kind cluster for each installation.
- **Generation budget:** 900 s, 32 steps, 8,192 answer tokens.

This continues the [batteries 19–24 evidence](2026-11-25-local-model-batteries-19-24.md).

## Summary

| Battery | Installation | Corpus | Delivered within two attempts | First attempt |
|---|---|---|---|---|
| 27 | 436583d | issues 185–190 (stopped early) | 0/6 | 0/6 |
| 28 | 94436fa, plus the rewrite guard (#797) | issues 185–186 (stopped early) | 0/2 | 0/2 |
| 29 | b7c12ae, plus transient-read retries (#799, #800) and the verifier source cache (#802) | issues 185–194 | 5/10 = 50% [23.7%, 76.3%] | 5/10 |
| 30 | 7f50d07, plus the copy guard (#806), timed-out-read retries with reserves (#805) and named intake scan steps (#807) | issues 195–196, 206–213 | **9/10 = 90% [59.6%, 98.2%]** | 5/10 |

Intervals are Wilson 95% intervals from the evaluation harness (#786).

The reports are attached:
- [battery 29](2026-11-25-evaluation-battery-29.json): the runner stopped on a GitHub API outage after 10 of its 12 issues;
- [battery 30](2026-11-25-evaluation-battery-30.json).

Batteries 27 and 28 were stopped as soon as their dominant stop was understood, so they are not rate evidence.

Battery 30 meets the Beta gate's 80% rate for one cell. It does not yet meet the 70% lower bound or the 60 distinct issues per cell. The second repository and the second model path are not started.

## Findings

1. **Refused candidates came from the model damaging existing files.** Applying one refused candidate to its source and running the tests named the cause in minutes; the stop name alone ("candidate failed its tests") did not. Three forms were seen:
   - **A whole-file rewrite** of an existing, roughly 800-line test file that dropped most existing tests and a helper (battery 27).
   - **A copy of every existing test** into a new test file, after the prompt asked for new tests in a new file. The call ran into the 570 s ceiling and would not have compiled (battery 29).
   - **Several `edit_file` patches** that together deleted most of an existing source file (battery 30).

   The prompt already said not to rewrite existing files. Guidance alone did not change the behaviour; mechanical limits at the tool-call boundary did. The model gateway now refuses, once, a whole rewrite of a file the model read (#797) and a new file that copies a read file (#806). It then retries the call with a short correction, and fails open on a repeat. A guard for destructive edits is in review (#809).
2. **A single failed API read lost whole attempts.** On the loaded single-node cluster, one dropped GET, one 5xx answer or one GET past its 5 s bound stopped an attempt as `uncertain`. This happened in readiness, verification observation and resource cleanup. Reads and removals now retry transient failures within bounds (#799, #800, #805). Reserves keep a retry from starving a side-effecting step that follows, such as a pod POST, a start signal or a source import.
3. **The verifier exhausted the forge's anonymous quota.** A review waiting on its gateway re-downloaded the same pinned source on every 5 s tick. The verifier now shares one cached source per commit and backs off until the forge's reset (#802). An optional read-only credential removes the anonymous limit for verification and review (#796); creating it needs a maintainer.
4. **One incomplete intake scan aged the authority evidence past its window.** The intake watermark advances only after a complete scan, and a failed scan retries after 60 s. Twice in battery 29, an attempt then stopped at `model-authorization-publication-snapshot` and lost its model work. The authorization now waits, within a bound and every deadline, for fresh evidence. Stale or changed evidence never authorizes (#804). The intake now logs where a scan stopped (#807).
5. **Unnamed stops.** A failed candidate export now names its step (#810).
6. **Evaluation infrastructure.**
   - The host's guard proxy restarted llama-server on degenerate output only after the stream ended. The gateway's immediate retry therefore reached the corrupted process that the restart then killed. The proxy now holds new requests from the moment it detects corruption, at a threshold below the gateway's.
   - The battery runner now retries a label edit through a forge outage (#808).

   These proxy changes are evaluation-only and run only on the host.

## Remaining limits

- The generation budget (900 s and 32 steps at about 12 tokens per second) ended several attempts with exit 22, or with a step-budget exit that changed nothing.
- Some candidates still fail their own tests.
- The Beta gate needs 60 distinct issues per cell, a second repository in another language, and a second model path.

## Evaluation-only infrastructure

The capture and guard proxies, the tool-call log, the bounded slow-stream tails and the init-state recorder ran on the evaluation host only. They are not part of the product. Captured requests stayed on the host. This document reports only bounded facts derived from them.
