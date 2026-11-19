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

Adding another provider means adding one `Adapter` subclass to
`runtime/k8s/tools/model_adapters.py` (`ADAPTERS`) and the enum value to the
chart schema. The worker, the verifier and the contract do not change.

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
    promptTokens: 16384      # planning envelope: the largest prompt one SCC step may need
    completionTokens: 4096   # and the largest answer
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
proposed, the benchmark fails. After three attempts the gateway exits, and the
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

## Known limit: the per-call window

Each executor model call is still bounded by a fixed 60 s window: the mailbox
request wait and the `model_calls` reservation expiry. The broker stops 15 s
before that (10 s for reviews), and review runs are capped at 180 s in total.
Until that window becomes installation-configurable, a derived timeout above
about 45 s cannot take effect. Making it configurable is the next required
change before slow local models can run full SCC steps.
