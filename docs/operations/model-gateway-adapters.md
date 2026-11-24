# Model gateway adapters and startup benchmark

The worker (which runs SCC) and the verifier never talk to a model provider
directly. Each pod runs a `model-gateway` sidecar on `127.0.0.1:8787` that
serves one authenticated, bounded, OpenAI-style chat-completions contract
(`runtime/k8s/tools/codexcli_openai_wrapper.py`: request validation, tool-name
allowlist, bounded assistant message). The backend behind it is one
installation-selected **adapter**, so AI-SDLC stays decoupled from any single
AI service.

| `modelGateway.adapter` | Backend | Typical services |
|---|---|---|
| `agent-cli` (default) | A local agent CLI acting only as a completion model | Codex CLI (`codex_gateway.py`) |
| `openai` | `POST /v1/chat/completions` | llama.cpp, vLLM, Ollama, LM Studio, LiteLLM, hosted OpenAI-compatible APIs |
| `anthropic` | `POST /v1/messages` (Messages API) | Anthropic API or a compatible proxy |

Adapters never execute tools. They receive only validated messages and tool
schemas, and their answers are normalized by the shared contract: unknown
tools, non-object arguments, oversized replies, redirects and unknown content
blocks fail closed, and so do truncated answers (`finish_reason: length`,
`stop_reason: max_tokens`). Inline `<think>` blocks are removed. One total
deadline covers each upstream call, including a slowly dripped body. Sampling controls
(`temperature`, `tool_choice`) are typed and bounded before any backend sees
them.

Proposed `edit_file` patches also get their hunk counts recomputed from the
hunk bodies (see [Edit-patch hunk counts](#edit-patch-hunk-counts)).

Adding another provider means adding one `Adapter` subclass to
`runtime/k8s/tools/model_adapters.py` (`ADAPTERS`) and the enum value to the
chart schema. The worker, the verifier and the contract do not change.

## Local development without a model service

AI-SDLC only consumes a model-as-a-service. To simulate one on a developer host
with a local agent CLI, see `tools/local_model_service/README.md`. The
installation keeps using the `openai` adapter against that endpoint, and no CLI
enters the cluster.

## Choosing the model service at installation

A local candidate or verification installation can select an OpenAI-compatible
model service directly:

```bash
./install.sh local ... --model-upstream path/to/upstream.json
```

The document is the same reviewed upstream document as the
[model-upstream runtime upgrade](model-upstream-upgrade.md) and is validated
the same way: one private HTTP completions endpoint whose URL, `/32` CIDR and
port agree, bounded `seconds`, and the per-call windows `callSeconds` (executor)
and `reviewCallSeconds` (reviewer). An installation-time `credentialSecret` is
not supported yet; the service's client allowlist controls access. With an upstream, the generation
budget allows one model call per SCC step (24 calls and 6 MiB of reserved output
by default, 32 calls and 8 MiB with `--generation-steps 32`; 4 MiB of input). The flag is
chosen once: reconciling the installation with a different (or no) upstream is
refused, and a change goes through a reviewed runtime transition. Until the
installer stops requiring it, a Codex credential is still requested.

## Configuration

```yaml
modelGateway:
  adapter: openai
  upstream:
    url: http://10.89.0.1:18080/v1/chat/completions  # fixed; https, or http to a literal RFC 1918/ULA address
    model: gemma-4-26b-a4b                           # fixed alias sent upstream
    cidr: 10.89.0.1/32                               # the only added egress (worker and verifier)
    port: 18080
    seconds: 300                                     # ceiling for one upstream call (30-570)
    credentialSecret: ""                             # optional Secret with key "credential"
    directAnswers: true                              # llama.cpp/vLLM: chat_template_kwargs.enable_thinking=false
  benchmark:
    promptTokens: 16384      # worker planning envelope: the largest prompt one SCC step may need
    completionTokens: 4096   # and the largest answer: the generator profile's maxTokens
  reviewBenchmark:           # verifier envelope: large candidates, short verdicts
    promptTokens: 16384
    completionTokens: 1024
```

`anthropic` requires `credentialSecret`. The credential is mounted read-only
into the gateway only, and is sent only as the provider's authentication header
(`Authorization: Bearer` for `openai`, `x-api-key` for `anthropic`). The
default (`agent-cli`) renders byte-identically to earlier charts.

## Startup benchmark

Upstream adapters benchmark the model before the gateway reports ready:

1. **Tool call**: the model must propose a valid `read_file` call through the
   contract (functional readiness, not only reachability).
2. **Prefill**: a ~1,800-token prompt with a 1-token answer gives prompt
   tokens/second. Each run starts with a fresh nonce so prompt caches cannot
   inflate it, and the slower of two runs is used.
3. **Generation**: a bounded answer gives completion tokens/second, net of its
   own prompt time.

The per-call timeout is derived for the planning envelope:

```text
timeout = ceil(1.5 * (promptTokens/prefillRate + completionTokens/generationRate)) + 5   (minimum 30 s)
```

With `--model-upstream`, the installer sets the worker envelope's
`completionTokens` to the generator's answer-token bound
(`--generation-answer-tokens`, 4096 by default and at most 8192), so the derived
timeout covers the longest answer the profile admits. Runtime transitions carry
the recorded bound and its envelope unchanged. The verifier envelope stays at
1,024.

If the derived timeout exceeds `upstream.seconds`, or no valid tool call is
proposed, the benchmark fails. Attempts are retried with randomized backoff,
so gateways sharing one upstream stop measuring each other's load. After
five attempts the gateway exits, and the
restarting container is the signal. Until it passes, `/readyz` returns 503 and
completions return 503. The worker checks `/readyz` before every claim, so no
work is claimed against an unmeasured model. Adapters that do not report token
usage are bounded by their measured tool-call latency instead.

The gateway applies the derived timeout to every upstream call and serves it
at the authenticated `GET /v1/gateway/profile` (`{"timeoutSeconds": N}`). With
`AI_SDLC_MODEL_GATEWAY_PROFILE=benchmark` (rendered for the worker in upstream
mode), the worker bounds each model call by `N + 30` seconds. It refuses to
claim work if the profile is missing or above its own ceiling
(`upstream.seconds + 30`, at most 600). The verifier keeps that ceiling, which
is never below its gateway's derived timeout.

The full measured profile is logged once as a `model-benchmark` JSON event:

```json
{"adapter": "openai", "model": "gemma-4-26b-a4b", "toolCall": true, "toolCallSeconds": 0.961,
 "promptTokensPerSecond": 454.9, "completionTokensPerSecond": 37.1,
 "derivedSeconds": 225, "timeoutSeconds": 225, "ceilingSeconds": 300}
```

The example above is the measurement against a local llama.cpp server running
Gemma 4 26B-A4B (64k context) on 2026-11-19.

## Edit-patch hunk counts

SCC's `edit_file` takes a unified diff and parses it strictly (jsdiff
`parsePatch` plus an exact `@@ -start,count +start,count @@` check). The two
counts in each hunk header are redundant: they are fully determined by the hunk
body, yet local models often get them wrong. In evaluation batteries 20 and 21
a local Qwen3-Coder declared `@@ -390,3 +390,8 @@` for a hunk of 9 context and
9 added lines; SCC refused the call, and the model's fallback (rewriting the
whole file) exceeded the 4,096-token answer bound and stopped the attempt.

The gateway therefore applies one deterministic normalization to every
adapter's answer (`recount_edit_patches` in `model_adapters.py`), before it
returns the answer and so before the broker digests, stores or hands it off:
for each tool call named exactly `edit_file` whose JSON arguments hold a string
`patch`, each hunk header's two counts are recomputed from its body. Context
lines (a space prefix, or a bare empty line, a common omission) count on both
sides, `-` lines on the old side, `+` lines on the new side, and
`\ No newline at end of file` on neither. Blank lines that end a hunk are
counted only when the declared counts already include them, because the
parser otherwise skips them. Only the counts change: start values, the
optional section text after `@@`, every body and file-header line, and every
other argument key and value stay the same. A repaired call's arguments are
re-serialized as JSON with the same keys in the same order.

It does nothing, leaving the arguments byte for byte unchanged, when the
declared counts are already consistent, or when anything is ambiguous: no
hunk, a line starting with `@@` that is not a well-formed header (SCC's own
header form, starts and counts of at most nine digits), a body line with an
unknown prefix or an unknown backslash line, a hunk with no counted line, more
than one file, a carriage return, more than 512 hunks, a patch or argument over
32 KiB, a recount that would turn a side empty or non-empty (which would change
the meaning of its start), or a non-zero count at start 0. SCC then refuses or
applies the patch exactly as before. The work is one linear pass per call.

The normalization never alters what the edit does: SCC still matches every
context and removed line against the file and refuses a patch that does not
match. It only lets a correct edit with wrong bookkeeping reach that check.
Each repair is visible in the `model-call` event as `editPatchesRecounted`;
patch content is never logged.

## Per-call events

After the benchmark, the gateway logs one bounded `model-call` event per
completion:

```json
{"event": "model-call", "outcome": "ok", "seconds": 4.2, "budgetSeconds": 176, "promptTokens": 9180, "completionTokens": 104, "estimated": false}
{"event": "model-call", "outcome": "failed", "seconds": 176.0, "budgetSeconds": 176, "reason": "deadline"}
```

A successful call that repaired the hunk counts of proposed `edit_file`
patches also carries `"editPatchesRecounted": N` (1 to 8, the number of repaired
calls); the field is absent otherwise.

`reason` is a fixed class: `truncated` (the answer reached its token bound),
`deadline`, `upstream-status`, `oversized`, `backend`, `unavailable`, `invalid`
or `other`. `estimated` is true when the upstream reported no usage and the counts are estimates. Events never contain request or response content. They separate
a slow model from an answer cut off at its bound, which both surface as
`model-provider` stops.

## Capacity

A single local model serves the worker and the verifier. Concurrent calls
share its throughput: two simultaneous benchmarks measured about half the solo
rate (Gemma 4 26B-A4B: derived 357-498 s together against 215 s alone).
Generation and review of one work item run one after the other. Several items
in flight at once need a model server with more capacity, or a ceiling that
covers the contention.

The worker and verifier gateways start together, so their startup benchmarks
would otherwise overlap and the second would adopt an inflated profile, which
can make every review `gateway-profile-unfit`. Two bounded measures prevent
this:

- Before its first attempt, each gateway waits a random 0 to N seconds.
  `AI_SDLC_MODEL_BENCHMARK_STAGGER_SECONDS` sets N: default 30, range 0-120,
  and 0 disables the delay. An invalid value stops the gateway at startup.
  Readiness is delayed by at most N seconds.
- When the first profile's timeout exceeds 80% of the installation ceiling,
  the gateway waits a random 5-25 s, benchmarks once more and keeps the faster
  profile. A failed second measurement keeps the first profile. The recheck
  is logged as `model-benchmark-recheck`; `model-benchmark` logs the profile
  kept.

## Per-call window

Each model call crosses SCC, the executor's model mailbox, the call reservation
and the broker. By default each call has a fixed 60 s window, enough for the
Codex CLI adapter. Slower installed models select it in the reviewed executor
or reviewer profile:

```json
{"version": "ai-sdlc.executor-profile/v1", "maxSeconds": 900, "callSeconds": 300, ...}
```

`callSeconds` (60-600, at most `maxSeconds`; reviewers stay within their
180 s run cap) sets:
- SCC's provider wait (`model.timeout` and `SC_PROVIDER_CONNECT_TIMEOUT_MS`,
  window + 15 s, so SCC's own timer never fires before the mailbox answers);
- the mailbox request wait (`--wait-seconds`);
- the `model_calls` reservation expiry.

Node's `fetch` inside SCC also abandons a response after 300 s without headers
or body bytes. For windows above 240 s the mailbox therefore commits a `200` at
240 s and sends whitespace until the answer arrives; see
[calls longer than 300 seconds](../security/offline-model-mailbox.md#calls-longer-than-300-seconds).

The broker still stops the provider call 15 s before the window ends (10 s
for reviews), and never publishes a response after the mailbox window: once
the mailbox has answered SCC with a timeout, a late handoff would never be
received. That call is recorded as uncertain and the run stops. SCC may retry
a mailbox timeout as a new call, which the ledger budgets like any other. When
the profile omits `callSeconds`, workloads, SCC configuration and reservations
are identical to earlier releases.

The worker only admits the model when the benchmarked gateway timeout `T` also
fits the window (`T + 20 s <= callSeconds`). Otherwise it claims no work, because a
slower model would only produce uncertain calls. Choose `upstream.seconds` (the
benchmark ceiling) and `callSeconds` together, for example 216 s measured, a
300 s ceiling and `callSeconds: 330`. In upstream mode the verifier applies the same
gate against the reviewer profile's window: it queues and claims no review
(`gateway-profile-unfit`) while the measured timeout does not fit.

`T` admits the model; it does not shorten each call. Every upstream call may
take up to `upstream.seconds`, bounded by the broker's remaining wait for that
call, which the provider sends as `X-AI-SDLC-Call-Seconds`. The gateway
therefore never keeps working, or holding its single model slot, after the
broker gives up (window minus 15 s; minus 10 s for reviews). Without the
header, from an older broker, the gateway applies `T` as before. The startup
probes show the best case; real calls have a latency tail, from queueing,
provider retries or long answers. In the evaluation
batteries, calls of 92–123 s failed against a benchmarked `T` of about 93 s,
inside a 330 s window.
