# Evaluation batteries 2 and 3, and the first OpenShift delivery, 2026-11-22

**Scope:** `os-santiago/ai-sdlc-sample`, a small Go module, delivered by
`v0.1.0-alpha.1` plus the fixes merged during this window. Each battery issue
was delivered from its trigger label only, with up to two attempts and a
15-minute limit per attempt. No stage was advanced by hand. The runner is
[`tools/evaluation/run_battery.py`](../../tools/evaluation/README.md). Battery 1
is recorded in [evaluation battery 1](2026-11-22-evaluation-battery-1.md).

**Model service:** the local OpenAI-compatible service in front of an agent
CLI, as in the earlier runs.

## Summary

| Run | Platform | Images | Issues | First attempt | Within two attempts |
|---|---|---|---|---|---|
| Battery 1 | kind | `44a506c` | 6 | 3 (50%) | 4 (67%) |
| Battery 2 | kind | `612feaf` | 10 | 6 (60%) | 7 (70%) |
| Battery 3 | kind | `9caf83b` | 8 | 4 (50%) | 6 (75%) |
| Battery 4 | kind | `b2033ca` | 6 | 2 (33%) | 4 (67%) |
| One issue | OpenShift Local | `b2033ca` | 1 | 1 | 1 |

- **Successful deliveries:** in batteries 2 and 3, 5.4 to 7.5 minutes from
  label to merge, using 5 to 12 model calls. Battery 4 ranged from 4.7 to 12.5
  minutes, using up to 16 calls. Every merged change passed verification with the
  repository's own tests and an independent review.
- **OpenShift Local (OpenShift 5.0, CRI-O):** issue #29 went from label to
  merged pull request #41 in 6.5 minutes. The steps were:
  - generation captured;
  - repository tests passed in three pods;
  - independent review approved;
  - publication;
  - merge after the required check.

  The sandbox RuntimeClass there used `crun`, not gVisor (see the limits
  below).

The runner's reports are
[battery 2](2026-11-22-evaluation-battery-2.json) and
[battery 3](2026-11-22-evaluation-battery-3.json).

## Battery 2 (images `612feaf`)

| Issue | Attempt 1 | Attempt 2 |
|---|---|---|
| #7 `kv.Parse` trailing separator | review `uncertain` | review `uncertain` |
| #9 `textfmt.Title` multibyte | merged, 7.5 min | |
| #10 `kv.Format` | review `uncertain` | verification `comparison-failed` (candidate tests fail) |
| #11 `textutil.IsPalindrome` | merged, 6.4 min | |
| #16 `mathutil.Mode` | review `uncertain` | merged, 7.5 min |
| #17 `mathutil.StdDev` | merged, 6.8 min | |
| #18 `textutil.Initials` | merged, 5.7 min | |
| #19 `kv.ParseInts` | merged, 5.4 min | |
| #20 `textfmt.PadLeft` | merged, 5.7 min | |
| #21 `kv.Parse` trims spaces | verification stop at `config` | execution stop at `model-authorization-dispatch` |

The SCC livelocks of battery 1 were gone. The generator no longer looped on
its own final summary (sc-agent-cli#636, #700).

## Battery 3 (images `9caf83b`, with the review stop diagnostic)

| Issue | Attempt 1 | Attempt 2 |
|---|---|---|
| #7 `kv.Parse` trailing separator | review `uncertain` (the runner recorded no line; the verifier log reads `model-response-policy`) | merged, 7.5 min |
| #10 `kv.Format` | merged, 6.8 min | |
| #21 `kv.Parse` trims spaces | merged, 6.4 min | |
| #30 `textutil.CountVowels` | review `model-mailbox-read` | review `model-evidence` |
| #31 `mathutil.MinMax` | merged, 6.5 min | |
| #32 `textfmt.Repeat` | review `model-response-policy` | review `model-evidence` |
| #33 `kv.Keys` | merged, 5.4 min | |
| #34 `textutil.WordCount` punctuation | review `cleanup` | merged, 7.5 min |

## Failure causes

| Cause | Where | Status |
|---|---|---|
| The reviewer approved with one exact final `VERDICT: approve` line but quoted code in a fenced block, which the verdict parser refused. | Both `model-response-policy` stops in battery 3, #7 and #32, measured with the model service's answer shape (#705). The source is the local service's log, which holds format facts only, never text, and is not kept in this repository | Fixed in #711, which accepts balanced fences; two adversarial audits ran. It ran in the OpenShift delivery, but has not been measured in a battery yet. |
| The model-evidence binding did not match the reviewer summary. | 2 stops | Cause not confirmed. Non-ASCII text was ruled out: it also appears in passing reviews. |
| A mailbox read failed after the reviewer had exited cleanly. | 1 stop | The transport refuses to infer a clean exit after a protocol error, and the tests fix that rule. Left as an operator decision. |
| Review or verification resource steps failed (`cleanup`, `config`), and one model authorization check failed. | 3 stops | Likely control-plane latency: the host also ran a 16 GiB OpenShift Local VM. For verification resource steps, #706 now names the exact step (for example `config-fence`); review `cleanup` and model authorization stops are not refined yet. |
| The candidate's own tests failed. | 1 stop | Correctly refused. The generator cannot run the tests itself yet. |
| An executor, verification or review pod failed its post-create check, its UID was lost, and cleanup blocked the component. | battery 1, OpenShift | Fixed in #699 and #708, which keep the observed UID. |

## OpenShift findings fixed on the way

Each one stopped the first OpenShift attempts fail-closed:

- **DNS egress:** it allowed only `kube-system:53`. OpenShift DNS listens on
  5353 in `openshift-dns` (#704).
- **Memory-pressure toleration:** admission added one, and the exact pod
  comparison refused it (#707).
- **Image identity:** CRI-O reports the resolved platform manifest in `imageID`
  for images pinned by OCI index, where containerd reports the index digest
  (#710).
- **Installer:** there was no path for an existing cluster. Added
  `install.sh local --platform openshift` (#704).

## Battery 4 (images `b2033ca`, with balanced fences; OpenShift VM stopped)

The battery re-ran the two issues that failed in battery 3 (#30, #32) and four
new ones. The report is
[battery 4](2026-11-22-evaluation-battery-4.json).

| Issue | Attempt 1 | Attempt 2 |
|---|---|---|
| #30 `textutil.CountVowels` | review `model` | review `model-evidence` |
| #32 `textfmt.Repeat` | review `model-mailbox-read` | execution `execution-observe` |
| #42 `textutil.Capitalize` | timeout: merge blocked (see below) | merged, 6.1 min |
| #43 `mathutil.Abs` | review `model-evidence` | merged, 4.7 min (fenced approval, per the service log) |
| #44 `kv.Merge` | merged, 12.5 min (16 model calls) | |
| #45 `textfmt.Center` | merged, 6.8 min | |

- **#711 measured:** a fenced approval that the old parser would have refused
  was accepted, and the issue was delivered (#43, attempt 2). The fence comes
  from the local model service's answer-shape log, which is not kept in the
  repository.
- **`model-evidence`, likely secret redaction:** a pending change (#713),
  deployed only on the local service from 06:37 UTC, flags answers that the
  agent CLI's secret redaction would rewrite. After that time, the service's
  log shows the only `model-evidence` stop (#43, attempt 1) flagged and no
  unflagged approval failing. #30's `model-evidence` stop (attempt 2, 06:26)
  came before the flag existed. The suspected mechanism is that the agent CLI
  redacts `name: value` text such as `Tests pass: yes` in its saved final
  message, so the summary no longer matches the provider's digest. This is
  likely but not yet confirmed.
- **Merge blocked by a third-party bot:** the sample repository's native
  ruleset requires resolved review threads. A review bot installed on the
  repository left a thread open on the first pull request for #42. The merge
  controller waited for checks until the attempt timed out. The retry
  produced a second pull request, which merged; the first was closed as
  superseded. The runtime does not answer review comments, so a target
  repository needs no thread-resolution rule, or no other reviewer bots.
- **Transport stops (`model-mailbox-read`, `execution-observe`):** they
  persisted with the OpenShift VM stopped, so host memory pressure does not
  fully explain them. They cluster around the worker's exit and are not yet
  explained.

## Limits

- **Sample size:** one small repository and one model path. The rates do not
  predict other repositories or models.
- **Weak decisions:** the sample has no JUnit report.
- **#711 measured once in battery 4,** where it turned a refused fenced approval into a delivery.
- **OpenShift sandbox:** gVisor does not run under CRI-O. The OpenShift
  delivery used `crun` with SCC `nonroot-v2`, Pod Security admission and a
  deny-all NetworkPolicy, without a kernel sandbox. That is acceptable only on
  a disposable cluster. The supported path to evaluate next is OpenShift
  sandboxed containers (Kata). See [OpenShift](../operations/openshift.md).
- **Shared repository:** two installations watched the same repository.
  Their intake was paused in turn, so only one acted on labels at a time.
