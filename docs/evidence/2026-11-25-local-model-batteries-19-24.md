# Local model batteries 19–24: from corrupted output to autonomous deliveries, 2026-11-24/25

**Scope:**
- **Repository:** `os-santiago/ai-sdlc-sample`, a small Go repository.
- **Trigger:** the label only, with two attempts per issue.
- **Model path:** a local model through `--model-upstream`. Qwen3-Coder-30B-A3B Q4_K_M ran on llama.cpp on the reference laptop's integrated GPU (AMD Radeon 780M).
- **Cluster:** a local kind cluster.

This complements the
[2026-11-24 GPU instability evidence](2026-11-24-local-model-gpu-instability.md).

## Summary

| Battery | Installation | Result | Dominant stop |
|---|---|---|---|
| 19 | HIP backend, 300 s call ceiling | stopped early | calls over the ceiling |
| 20 | plus a 570 s call ceiling | partial | whole-file rewrites; publication helper exit after 300 s calls |
| 21 | plus the manifest toolchain (#773) | partial | refused patches, whole-file rewrites |
| 22 | plus hunk-count repair (#777) and the answer bound (#778) | partial | publication after calls over 300 s |
| 23 | plus the streaming guard (#779) and the mailbox keepalive (#781) | partial: first candidates reach verification and review | steps run out; reviewer offered forbidden tools |
| 24 | plus step guidance (#782), review tool narrowing (#784), signed images (#783) | **3/10 delivered within two attempts**, 2/10 on the first | steps run out; long rewrites |

Battery 24's report is [2026-11-25-evaluation-battery-24.json](2026-11-25-evaluation-battery-24.json).
The evaluation harness (#786) summarises it as follows. The interval is a Wilson 95% interval; installation revision fields did not exist yet, so the Beta gate cannot count it.

| Measure | Value |
|---|---|
| Delivered within two attempts | 3/10 = 30.0% [10.8%, 60.3%] |
| First attempt | 2/10 = 20.0% [5.7%, 51.0%] |
| Median seconds to delivery | 629 |
| Model calls | 388 over 18 attempts |

Batteries 19–23 were stopped as soon as their dominant stop was understood, so they are not rate evidence.

## Findings

1. **GPU backend.** Vulkan and HIP with a 2,048-token micro-batch both corrupted
   output intermittently: a run of one character until `max_tokens`. HIP with a
   1,024-token micro-batch stayed clean in repeated replays of captured
   requests, including 0 corrupted out of 64 replays, while Vulkan failed in the
   same time window. On HIP, restarting the server clears a corrupted process.
   An evaluation-only guard proxy on the host restarts it when it sees
   degenerate output. The gateway's streaming guard (#779) aborts such an
   answer early and retries once.
2. **Refused patches drove whole-file rewrites.** The model wrote wrong hunk
   counts, so SCC refused the `edit_file` patches and the model rewrote whole
   files. The gateway now recomputes the redundant counts (#777). It was
   validated against SCC's own `edit_file` with 293 corrupted patches, all of
   which applied after the recount.
3. **A hidden 300-second client default.** Node's `fetch` (undici) in the
   worker image aborts a request whose headers take more than 300 s. The
   mailbox sent headers only when the answer arrived, so calls over 300 s
   failed at publication. The mailbox now commits headers before 300 s and
   keeps the connection alive (#781).
4. **The reviewer was offered tools it must not use.** SCC advertises every
   tool, so the reviewer called `run_shell` and the strict policy refused the
   review. The review gateway now offers only the four read-only tools the
   policy accepts (#784).
5. **The model runs the tests but does not stop.** In a captured result, the
   model saw `go test` pass, then kept re-running variants until SCC's step
   budget ended. Prompt guidance (#782) did not change this. A candidate left
   by a step-budget exit is now exported for the independent gates to decide
   (#788).
6. **Remaining model-capability limits.**
   - Whole rewrites of large test files at about 13 tokens per second reach the
     call ceiling.
   - Some candidates fail their own tests.
   - Some reviewer final answers miss the strict verdict format.
7. **The first signed publication verified end to end.** Main c36bcef signed
   all 13 image digests keyless. The resolver verified the 9 locked images
   with no legacy exception, bound to the main push workflow and the exact
   revision (#783).
8. **Transient transport stops.** `readiness` and `candidate` (tracked in
   #790), and `cleanup-after-baseline`. Some coincided with brief RuntimeClass
   read failures.

## Evaluation-only infrastructure

The capture and guard proxies, the tool-call log and the init-state recorder
ran on the evaluation host only. They are not part of the product. Captured
requests stayed on the host; this document reports only bounded facts derived
from them.
