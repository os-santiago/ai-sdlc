# Local model on an integrated GPU: intermittent corrupted output, 2026-11-24

**Scope:**
- **Model:** a local model served by an OpenAI-compatible llama.cpp server.
- **Hardware:** a laptop integrated GPU (AMD Radeon 780M, Vulkan, Mesa RADV 26.2).
- **Install:** the model is exposed to the kind installation as its model upstream (`--model-upstream`).
- **Battery 18:** the sample repository, trigger label only, two attempts per issue.

## Model choice

`llama-bench` numbers are prefill / generation in tokens per second, measured
first at empty context and then at 16k context.

| Model (GGUF) | Empty context | At 16k context | Notes |
|---|---|---|---|
| Gemma 4 26B-A4B QAT | 585 / 29.9 | 350 / 23.6 | |
| Qwen3-Coder-30B-A3B Q4_K_M | 526 / 33.8 | 199 / 19.6 | Selected: correct tool calls on a captured worker request |
| gpt-oss-20b MXFP4 | 635 / 27.4 | 452 / 23.5 | The server's harmony tool-call parser returned HTTP 500 at about 17k context |

Server settings:
- f16 KV cache: a q8_0 cache produced corrupted tokens.
- 32k context: every model degraded above about 33–45k.
- One slot, flash attention on.

## Battery 18: not measurable

Every attempt stopped at `model-provider`, which the gateway's `model-call`
event classified as `truncated`. The investigation went in five steps:

1. **Capture.** A bounded, evaluation-only capture proxy recorded one failing
   request. It had about 9.8k prompt tokens and a clean conversation history.
   The completion was one sentence followed by `?` until the 4,096-token
   request cap.
2. **Reference backend.** Replaying the same request on the CPU backend gave a
   correct tool call (`list_dir`). On the GPU it gave the same corrupted
   output, with and without the prompt cache. This places the fault in the GPU
   path, not in the model or the request.
3. **Backend settings.** Each llama.cpp Vulkan switch was tested with 6–12
   replays in one server process:
   - disabling sparse flash attention, stream-K, async submission, fusion and
     graph reordering;
   - disabling flash attention;
   - a smaller micro-batch;
   - zero-initialised VRAM.

   Every setting looked clean in one run and failed in a later one. The
   failure rate drifted over time between 0% and 40% for identical input.
4. **Fixed clocks.** A run with the GPU forced to its highest fixed clock level
   froze the host during prefill. Fixed clocks are not a safe mitigation on
   this hardware.
5. **No kernel errors.** No GPU kernel errors were logged before the freeze.

## Consequences

- The runtime behaved correctly. The gateway refused truncated completions,
  each stop was named, and nothing corrupted was published.
- The local GPU path cannot measure a delivery rate until it is stable.
  Candidate next paths:
  - a different GPU backend (ROCm/HIP) on the same hardware;
  - a smaller model on the CPU;
  - a discrete GPU or a hosted model, once an API key is available (B6b).
- A gateway check that refuses a degenerate completion early (a long run of
  one repeated token) would save up to the full call window per corrupted
  call. That would be a cost improvement only: the existing `truncated`
  refusal already fails closed.
