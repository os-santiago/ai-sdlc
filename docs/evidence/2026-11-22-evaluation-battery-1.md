# Evaluation battery 1, 2026-11-21/22

**Scope:** the first M4 battery on the local kind installation of
`v0.1.0-alpha.1` for `os-santiago/ai-sdlc-sample`. This is the same
installation and model service as the
[generic inner loop run](2026-11-21-generic-inner-loop-e2e.md).

The sample repository was seeded with eight issues: six bugs and two small
features in three Go packages. Each issue was delivered by its trigger label
only, with up to two attempts and a 15-minute limit per attempt. The runner is
[`tools/evaluation/run_battery.py`](../../tools/evaluation/README.md).

## Results

The battery was stopped after six issues, when a defect blocked every later
verification (see below).

| Issue | Attempt 1 | Attempt 2 | Delivered |
|---|---|---|---|
| #4 `mathutil.Clamp` wrong bound | execution stop, 0 model calls, 46 s | merged, 5.4 min, 7 calls | yes |
| #5 `mathutil.Median` even count | merged, 6.8 min, 11 calls | | yes |
| #6 `mathutil.Mean` empty slice | merged, 5.8 min, 8 calls | | yes |
| #7 `kv.Parse` trailing separator | SCC livelock (exit 23), 12 calls | SCC livelock (exit 23), 19 calls | no |
| #8 `textfmt.Truncate` multibyte | merged, 6.1 min, 8 calls | | yes |
| #9 `textfmt.Title` multibyte | verification stopped, then verifier blocked | SCC livelock (exit 23), 12 calls | no |

- **First-attempt success:** 3 of 6 (50%).
- **Success within two attempts:** 4 of 6 (67%).
- **Successful deliveries:** 5 to 7 minutes from label to merge, using 7 to 11
  model calls. Every merged change passed independent verification with the
  repository's own tests and an independent review.

The raw report is [2026-11-22-evaluation-battery-1.json](2026-11-22-evaluation-battery-1.json).

## Failure causes and fixes

| Cause | Attempts | Fix |
|---|---|---|
| The final summary of an unattended run triggered SCC's self-heal re-prompts. Words like "error", "unable to" or "recommend" read as an unfixed failure, and three prose answers in a row aborted the run (exit 23). This happened after the change was written, every time an issue was about error handling. | 3 | os-santiago/sc-agent-cli#636; the worker pin is updated in #700 |
| A verification pod's create response was read, but a later check failed. Its UID was discarded, so cleanup could never remove the pod and the verifier stayed blocked for all later work. | 1, and it blocked the battery | #699 keeps and acknowledges the observed UID |
| An execution stopped 14 s after it started, with no model call. Every exit of the model drive loop was logged as `execution`, so the cause is unknown. | 1 | #697 names each exit (`execution-authority`, `-read`, `-sequence`, ...) for the next occurrence |

The verification failure caused one more defect. In that case the verifier
logs only `cleanup` and loses the stage that failed first.

## Limits

- **Sample size:** six issues on one small repository, with one model path (the
  local OpenAI-compatible service in front of an agent CLI). These rates do
  not predict other repositories or models.
- **Weak decisions:** the sample has no JUnit report, so every decision was weak.
- **Lost create responses:** a create response that never arrives still blocks
  the verifier until an operator intervenes. This is by design
  ([independent verification](../operations/independent-verification.md)).
- **Next battery:** the next run uses images that contain the fixes above.
