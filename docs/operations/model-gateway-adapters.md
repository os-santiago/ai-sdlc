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
| `claude-cli` | Claude Code in print mode as a completion-only model | Anthropic API key or Google Vertex AI |

Adapters never execute tools. They receive only validated messages and tool
schemas, and their answers are normalized by the shared contract: unknown
tools, non-object arguments, oversized replies, redirects and unknown content
blocks fail closed, and so do truncated answers (`finish_reason: length`,
`stop_reason: max_tokens`). Inline `<think>` blocks are removed. One total
deadline covers each upstream call, including a slowly dripped body. Sampling controls
(`temperature`, `tool_choice`) are typed and bounded before any backend sees
them.

Adding another provider means adding one `Adapter` subclass to
`runtime/k8s/tools/model_adapters.py` (`ADAPTERS`) and the enum value to the
chart schema. The worker, the verifier and the contract do not change.

## Claude Code CLI adapter

`claude-cli` runs the pinned Claude Code binary (worker image, official musl
build verified by SHA-256) once per call:
- print mode, `--tools ""` (no native tools), `--bare` (no hooks, settings,
  CLAUDE.md or keychain), `--strict-mcp-config`, `--no-session-persistence`;
- a JSON-schema structured answer with `content` and proposed `tool_calls`,
  validated by the shared contract;
- a fresh empty directory and a minimal environment per call, holding only
  the installation credential;
- the conversation rendered as a readable transcript with complete tool
  results, plus fidelity instructions: read before editing, prefer exact
  `edit_file` changes, never rewrite files from memory, and express every
  tool use in `tool_calls`.

```yaml
modelGateway:
  adapter: claude-cli
  upstream: {model: claude-sonnet-5-5, seconds: 300, credentialSecret: ai-sdlc-model-claude}
  claude: {provider: vertex, vertexProject: <project>, vertexRegion: global}   # or provider: anthropic
```

The Secret's `credential` key holds an Anthropic API key (`anthropic`) or a
Google service account JSON (`vertex`). OAuth subscription tokens are never
read. No egress rule is added: the gateway uses the existing public HTTPS
egress.

Offline trial on Saferus issue #4, with the real executor and reviewer
profiles, on 2026-11-20:
- generation succeeded in 207 s (6 iterations, 11 tool calls), with exactly
  the three requested files changed and tests passing;
- the independent review approved in 122 s.

The same trial with a JSON-serialized conversation exhausted its 24 steps
and rewrote an unrelated file from memory, which motivated the transcript
rendering.

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
    completionTokens: 4096   # and the largest answer
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

## Capacity

A single local model serves the worker and the verifier. Concurrent calls
share its throughput: two simultaneous benchmarks measured about half the solo
rate (Gemma 4 26B-A4B: derived 357-498 s together against 215 s alone).
Generation and review of one work item run one after the other. Several items
in flight at once need a model server with more capacity, or a ceiling that
covers the contention.

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

The broker still stops the provider call 15 s before the window ends (10 s
for reviews), and never publishes a response after the mailbox window: once
the mailbox has answered SCC with a timeout, a late handoff would never be
received. That call is recorded as uncertain and the run stops. SCC may retry
a mailbox timeout as a new call, which the ledger budgets like any other. When
the profile omits `callSeconds`, workloads, SCC configuration and reservations
are identical to earlier releases.

The worker only adopts the benchmarked gateway timeout `T` when it also fits
the window (`T + 20 s <= callSeconds`). Otherwise it claims no work, because a
slower model would only produce uncertain calls. Choose `upstream.seconds` (the
benchmark ceiling) and `callSeconds` together, for example 216 s measured, a
300 s ceiling and `callSeconds: 330`. In upstream mode the verifier applies the same
gate against the reviewer profile's window: it queues and claims no review
(`gateway-profile-unfit`) while the measured timeout does not fit.
