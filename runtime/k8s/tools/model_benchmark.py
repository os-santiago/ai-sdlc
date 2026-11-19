"""Startup benchmark: measure the configured model, then derive its timeout.

Runs once in the gateway before it reports ready, so the worker (which runs
SCC) never claims work against a model whose behavior and speed are unknown.

Probes, all through the installation-selected adapter and the shared contract:
  1. tool call   the model proposes a valid call to a supplied tool (function)
  2. prefill     a long prompt with a 1-token answer (prompt tokens/second)
  3. generation  a short prompt with a bounded answer (completion tokens/second)

The per-call timeout is derived for the planning envelope (the largest prompt
and answer one SCC step may need) with a safety factor:

  timeout = ceil(safety * (promptTokens/prefillRate + completionTokens/generationRate)) + overhead

clamped below at 30 s. A model that cannot meet the installation ceiling, or
cannot propose a valid tool call, fails the benchmark: the gateway exits and
never reports ready (fail closed, visible as a restarting container).
Adapters that do not report token usage (an agent CLI) are bounded by their
measured tool-call latency instead.
"""
import math
import time

MIN_SECONDS = 30
TOOLS = [{"type": "function", "function": {
    "name": "read_file", "description": "Read one repository file.",
    "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}}]
TOOL_PROBE = {"messages": [
    {"role": "system", "content": "You are a coding agent. Use the provided tools."},
    {"role": "user", "content": "Read the file README.md using the read_file tool."}],
    "tools": TOOLS, "max_tokens": 128, "temperature": 0}
# About 1,800 tokens of varied text, so caches and repetition do not inflate the rate.
PREFILL_TEXT = " ".join("Line %d: the service records event %d with checksum %x and state %s." %
                        (i, i * 7, i * 2654435761 % 2**32, ("ready", "queued", "done")[i % 3]) for i in range(120))
PREFILL_PROBE = {"messages": [{"role": "user", "content": PREFILL_TEXT + "\nReply with the single word OK."}],
                 "max_tokens": 1, "temperature": 0}
GENERATION_PROBE = {"messages": [{"role": "user", "content": "Write the numbers from 1 to 400 separated by spaces."}],
                    "max_tokens": 192, "temperature": 0}


class BenchmarkFailed(RuntimeError):
    pass


def _timed(adapter, request, seconds):
    started = time.monotonic()
    completion = adapter.complete(request, seconds)
    return completion, max(time.monotonic() - started, 1e-3)


def benchmark(adapter, ceiling, prompt_tokens=16384, completion_tokens=4096, safety=1.5, overhead=5):
    """Returns the published profile or raises BenchmarkFailed."""
    if not MIN_SECONDS <= ceiling <= 570 or prompt_tokens < 1 or completion_tokens < 1 or safety < 1:
        raise BenchmarkFailed("invalid benchmark envelope")
    try:
        tool, tool_seconds = _timed(adapter, TOOL_PROBE, ceiling)
    except Exception:
        raise BenchmarkFailed("model unavailable") from None
    calls = tool.message.get("tool_calls") or []
    if not calls or calls[0]["function"]["name"] != "read_file":
        raise BenchmarkFailed("model did not propose a valid tool call")
    profile = {"adapter": adapter.name, "model": adapter.model or adapter.name, "toolCall": True,
               "toolCallSeconds": round(tool_seconds, 3), "ceilingSeconds": ceiling,
               "planning": {"promptTokens": prompt_tokens, "completionTokens": completion_tokens,
                            "safety": safety, "overheadSeconds": overhead}}
    if not getattr(adapter, "reports_usage", False):
        timeout = math.ceil(safety * 4 * tool_seconds) + overhead
        profile["basis"] = "tool-call-latency"
    else:
        try:
            prefill, prefill_seconds = _timed(adapter, PREFILL_PROBE, ceiling)
            generation, generation_seconds = _timed(adapter, GENERATION_PROBE, ceiling)
        except Exception:
            raise BenchmarkFailed("model unavailable") from None
        prefill_rate = prefill.prompt_tokens / prefill_seconds
        # Remove the (short) prompt's share so the rate reflects decoding only.
        decode_seconds = max(generation_seconds - generation.prompt_tokens / prefill_rate, 1e-3)
        generation_rate = generation.completion_tokens / decode_seconds
        if prefill_rate <= 0 or generation_rate <= 0 or generation.completion_tokens < 8:
            raise BenchmarkFailed("model throughput unmeasurable")
        timeout = math.ceil(safety * (prompt_tokens / prefill_rate + completion_tokens / generation_rate)) + overhead
        profile.update({"basis": "throughput", "estimatedUsage": prefill.estimated or generation.estimated,
                        "promptTokensPerSecond": round(prefill_rate, 1),
                        "completionTokensPerSecond": round(generation_rate, 1)})
    timeout = max(MIN_SECONDS, timeout)
    profile["derivedSeconds"] = timeout
    if timeout > ceiling:
        raise BenchmarkFailed("model too slow for the installation ceiling (%ds > %ds)" % (timeout, ceiling))
    profile["timeoutSeconds"] = timeout
    profile["measuredAt"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    return profile
