"""Model backend adapters behind one loopback completion contract.

The worker and verifier always speak the same authenticated, bounded
OpenAI-style chat-completions contract to their loopback gateway. The gateway
validates every request with the shared contract, then hands it to exactly one
adapter selected by the installation:

  agent-cli  a local agent CLI used as a completion model (Codex CLI today)
  openai     any OpenAI-compatible Chat Completions endpoint (llama.cpp,
             vLLM, Ollama, LM Studio, LiteLLM, hosted OpenAI-compatible APIs)
  anthropic  the Anthropic Messages API (or a compatible proxy)

Adapters never execute tools: proposed calls are normalized and bounded by
``assistant_message`` and executed only by SCC. An adapter receives only the
validated messages and tool schemas, plus the single upstream URL, model alias
and optional credential the installation configured. Adding a provider means
adding one class to ``ADAPTERS``; the gateway, worker and chart are unchanged.
"""
import http.client
import ipaddress
import json
import os
import re
import socket
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from urllib.parse import urlsplit

from codexcli_openai_wrapper import MAX_RESPONSE_BYTES, assistant_message, validate_request

# The worker/verifier broker allows the gateway timeout + 30 s (bounded at 600).
UPSTREAM_SECONDS_MAX = 570
# Explicit RFC 1918 / ULA ranges; ipaddress.is_private also admits
# documentation and other special ranges.
PRIVATE_NETWORKS = tuple(ipaddress.ip_network(n) for n in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"))
MODEL_ALIAS = re.compile(r"[A-Za-z0-9._:/-]{1,128}\Z")
# Some models emit an inline reasoning block even with thinking disabled.
THINK = re.compile(r"\A\s*<think>.*?</think>\s*", re.S)
MAX_CREDENTIAL_BYTES = 4096


@dataclass
class Completion:
    """One bounded assistant message plus what the backend reported about it."""
    message: dict
    prompt_tokens: int = 0
    completion_tokens: int = 0
    estimated: bool = True
    usage: dict = field(default_factory=dict)


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def validate_parameters(request, names):
    """Optional sampling controls are typed and bounded before any backend sees them."""
    temperature = request.get("temperature")
    if temperature is not None and (type(temperature) not in (int, float) or not 0 <= temperature <= 2):
        raise ValueError("invalid temperature")
    choice = request.get("tool_choice")
    if choice is None or choice in ("auto", "none", "required"):
        return
    if (isinstance(choice, dict) and set(choice) == {"type", "function"} and choice["type"] == "function"
            and isinstance(choice["function"], dict) and set(choice["function"]) == {"name"}
            and choice["function"]["name"] in names):
        return
    raise ValueError("invalid tool choice")


def fixed_url(url, path):
    """One installation URL: https anywhere, or plain http only to a literal
    private address (a local bridge), never a hostname DNS could redirect."""
    parts = urlsplit(url or "")
    if (not url or len(url) > 512 or parts.path != path or parts.query or parts.fragment
            or parts.username or parts.password or parts.scheme not in ("http", "https") or not parts.hostname):
        raise RuntimeError("model upstream unavailable")
    if parts.scheme == "http":
        try:
            address = ipaddress.ip_address(parts.hostname)
        except ValueError:
            raise RuntimeError("model upstream unavailable") from None
        if not any(address in network for network in PRIVATE_NETWORKS):
            raise RuntimeError("model upstream unavailable")
    return url


def read_credential(path):
    if not path:
        return ""
    try:
        with open(path, "rb") as handle:
            raw = handle.read(MAX_CREDENTIAL_BYTES + 1)
    except OSError:
        raise RuntimeError("model upstream credential unavailable") from None
    value = raw.decode("ascii", errors="strict").strip() if len(raw) <= MAX_CREDENTIAL_BYTES else ""
    if not value or any(c.isspace() or not c.isprintable() for c in value):
        raise RuntimeError("model upstream credential unavailable")
    return value


class _Deadline:
    """Shuts down the upstream socket at one total deadline, covering connect,
    TLS, headers and a slowly dripped body."""
    def __init__(self, seconds):
        self.expired = threading.Event()
        self.sockets = []
        self.lock = threading.Lock()
        self.timer = threading.Timer(seconds, self.fire)
        self.timer.daemon = True

    def track(self, sock):
        with self.lock:
            self.sockets.append(sock)
            if self.expired.is_set():
                self._close(sock)

    @staticmethod
    def _close(sock):
        try:
            sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass

    def fire(self):
        with self.lock:
            self.expired.set()
            for sock in self.sockets:
                self._close(sock)


def _tracked(base, deadline):
    class Connection(base):
        def connect(self):
            super().connect()
            deadline.track(self.sock)
    return Connection


class _HTTP(urllib.request.HTTPHandler):
    def __init__(self, deadline):
        super().__init__()
        self.deadline = deadline

    def http_open(self, req):
        return self.do_open(_tracked(http.client.HTTPConnection, self.deadline), req)


class _HTTPS(urllib.request.HTTPSHandler):
    def __init__(self, deadline):
        super().__init__()
        self.deadline = deadline

    def https_open(self, req):
        return self.do_open(_tracked(http.client.HTTPSConnection, self.deadline), req,
                            context=self._context)


def post_json(url, payload, headers, seconds):
    """POST once with no proxy and no redirects. One total deadline covers the
    whole call: a watchdog armed before connecting shuts the socket down."""
    raw = json.dumps(payload).encode()
    call = urllib.request.Request(url, data=raw, method="POST", headers=dict(headers, **{"Content-Type": "application/json"}))
    deadline = _Deadline(seconds)
    opener = urllib.request.build_opener(_NoRedirect(), urllib.request.ProxyHandler({}), _HTTP(deadline), _HTTPS(deadline))
    body = b""
    deadline.timer.start()
    try:
        with opener.open(call, timeout=seconds) as response:
            if response.status != 200:
                raise RuntimeError("upstream status")
            while len(body) <= MAX_RESPONSE_BYTES and not deadline.expired.is_set():
                chunk = response.read1(16384)
                if not chunk:
                    break
                body += chunk
    except (urllib.error.URLError, OSError, TimeoutError, ValueError, http.client.HTTPException, AttributeError):
        if len(body) > MAX_RESPONSE_BYTES:
            raise ValueError("completion response exceeded bound") from None
        raise RuntimeError("completion backend failed") from None
    finally:
        deadline.timer.cancel()
    if deadline.expired.is_set():
        raise RuntimeError("completion deadline exceeded")
    if len(body) > MAX_RESPONSE_BYTES:
        raise ValueError("completion response exceeded bound")
    return json.loads(body)


def _count(value):
    return value if type(value) is int and 0 <= value <= 10_000_000 else None


def _estimate(text):
    return max(1, len(text) // 4)


class Adapter:
    """Base adapter: ``complete`` takes a raw request, validates it with the
    shared contract and returns a bounded ``Completion``."""
    name = ""

    def __init__(self, url="", model="", seconds=300, credential="", direct_answers=False):
        self.url, self.model, self.seconds, self.credential = url, model, seconds, credential
        self.direct_answers = direct_answers

    def complete(self, request, seconds=None, probe=False):
        """``probe`` is used only by the startup benchmark, whose prefill and
        generation probes are truncated on purpose; SCC traffic never is."""
        messages, tools, names, tokens = validate_request(request)
        validate_parameters(request, names)
        self.truncation_ok = probe
        try:
            return self.backend(request, messages, tools, names, tokens, seconds or self.seconds)
        finally:
            self.truncation_ok = False

    truncation_ok = False

    def backend(self, request, messages, tools, names, tokens, seconds):
        raise NotImplementedError


class AgentCLIAdapter(Adapter):
    """A local agent CLI acting only as a completion model (no tool execution)."""
    name = "agent-cli"
    reports_usage = False

    def backend(self, request, messages, tools, names, tokens, seconds):
        from codexcli_openai_wrapper import complete
        message = complete(request)
        text = json.dumps(messages)
        return Completion(message, _estimate(text), _estimate(message.get("content") or ""), True)


class OpenAIAdapter(Adapter):
    """OpenAI-compatible Chat Completions (``/v1/chat/completions``)."""
    name = "openai"
    path = "/v1/chat/completions"
    reports_usage = True

    def backend(self, request, messages, tools, names, tokens, seconds):
        payload = {"model": self.model, "messages": messages, "max_tokens": tokens, "stream": False}
        if self.direct_answers:
            # Direct answers: reasoning traces are not part of the contract.
            # Only for servers that template thinking (llama.cpp, vLLM).
            payload["chat_template_kwargs"] = {"enable_thinking": False}
        if tools:
            payload["tools"] = tools
        for key in ("temperature", "tool_choice"):
            if key in request:
                payload[key] = request[key]
        headers = {"Authorization": "Bearer " + self.credential} if self.credential else {}
        answer = post_json(self.url, payload, headers, seconds)
        choices = answer.get("choices") if isinstance(answer, dict) else None
        if not isinstance(choices, list) or len(choices) != 1 or not isinstance(choices[0], dict):
            raise ValueError("invalid completion")
        if choices[0].get("finish_reason") == "length" and not self.truncation_ok:
            raise ValueError("truncated completion")
        message = choices[0].get("message")
        if not isinstance(message, dict) or message.get("role", "assistant") != "assistant":
            raise ValueError("invalid completion")
        content = message.get("content") or ""
        if not isinstance(content, str):
            raise ValueError("invalid completion")
        proposed = []
        for item in message.get("tool_calls") or []:
            function = item.get("function") if isinstance(item, dict) else None
            if not isinstance(function, dict) or item.get("type", "function") != "function":
                raise ValueError("invalid proposed tool")
            proposed.append({"name": function.get("name"), "arguments": function.get("arguments")})
        result = assistant_message(THINK.sub("", content), proposed, names)
        usage = answer.get("usage") if isinstance(answer.get("usage"), dict) else {}
        prompt, completion = _count(usage.get("prompt_tokens")), _count(usage.get("completion_tokens"))
        if prompt is None or completion is None:
            return Completion(result, _estimate(json.dumps(messages)), _estimate(content), True)
        return Completion(result, prompt, completion, False)


class AnthropicAdapter(Adapter):
    """Anthropic Messages API (``/v1/messages``)."""
    name = "anthropic"
    path = "/v1/messages"
    version = "2023-06-01"
    reports_usage = True

    @staticmethod
    def translate(messages, tools, request):
        system, turns = [], []

        def add(role, blocks):
            if turns and turns[-1]["role"] == role:
                turns[-1]["content"].extend(blocks)
            else:
                turns.append({"role": role, "content": list(blocks)})

        for message in messages:
            role, content = message["role"], message.get("content") or ""
            if role in ("system", "developer"):
                system.append(content)
            elif role == "user":
                add("user", [{"type": "text", "text": content}] if content else [])
            elif role == "assistant":
                blocks = [{"type": "text", "text": content}] if content else []
                calls = message.get("tool_calls") or []
                if not isinstance(calls, list) or len(calls) > 8:
                    raise ValueError("invalid assistant tool calls")
                for call in calls:
                    function = call.get("function") if isinstance(call, dict) else None
                    if (not isinstance(function, dict) or not isinstance(call.get("id"), str)
                            or not isinstance(function.get("name"), str) or not isinstance(function.get("arguments"), str)):
                        raise ValueError("invalid assistant tool call")
                    arguments = json.loads(function["arguments"])
                    if not isinstance(arguments, dict):
                        raise ValueError("invalid assistant tool call")
                    blocks.append({"type": "tool_use", "id": call["id"], "name": function["name"], "input": arguments})
                add("assistant", blocks)
            else:  # tool result
                if not isinstance(message.get("tool_call_id"), str):
                    raise ValueError("tool result without call id")
                add("user", [{"type": "tool_result", "tool_use_id": message["tool_call_id"], "content": content}])
        turns = [turn for turn in turns if turn["content"]]
        if not turns or turns[0]["role"] != "user":
            turns.insert(0, {"role": "user", "content": [{"type": "text", "text": "Continue."}]})
        payload = {"messages": turns}
        if system:
            payload["system"] = "\n\n".join(system)
        if tools:
            payload["tools"] = [{"name": t["function"]["name"], "description": t["function"].get("description", ""),
                                 "input_schema": t["function"].get("parameters") or {"type": "object"}} for t in tools]
        choice = request.get("tool_choice")
        if choice == "none" and tools:
            # Keep tools: history may hold tool_use/tool_result blocks.
            payload["tool_choice"] = {"type": "none"}
        elif choice == "required":
            payload["tool_choice"] = {"type": "any"}
        elif isinstance(choice, dict):
            payload["tool_choice"] = {"type": "tool", "name": choice["function"]["name"]}
        if "temperature" in request:
            payload["temperature"] = min(float(request["temperature"]), 1.0)
        return payload

    def backend(self, request, messages, tools, names, tokens, seconds):
        payload = dict(self.translate(messages, tools, request), model=self.model, max_tokens=tokens)
        answer = post_json(self.url, payload, {"x-api-key": self.credential, "anthropic-version": self.version}, seconds)
        if not isinstance(answer, dict) or answer.get("role") != "assistant" or not isinstance(answer.get("content"), list):
            raise ValueError("invalid completion")
        if answer.get("stop_reason") == "max_tokens" and not self.truncation_ok:
            raise ValueError("truncated completion")
        text, proposed = [], []
        for block in answer["content"]:
            kind = block.get("type") if isinstance(block, dict) else None
            if kind == "text" and isinstance(block.get("text"), str):
                text.append(block["text"])
            elif kind == "tool_use" and isinstance(block.get("input"), dict):
                proposed.append({"name": block.get("name"), "arguments": json.dumps(block["input"])})
            elif kind not in ("thinking", "redacted_thinking"):
                raise ValueError("invalid completion block")
        content = "".join(text)
        result = assistant_message(THINK.sub("", content), proposed, names)
        usage = answer.get("usage") if isinstance(answer.get("usage"), dict) else {}
        prompt, completion = _count(usage.get("input_tokens")), _count(usage.get("output_tokens"))
        if prompt is None or completion is None:
            return Completion(result, _estimate(json.dumps(messages)), _estimate(content), True)
        return Completion(result, prompt, completion, False)


ADAPTERS = {adapter.name: adapter for adapter in (AgentCLIAdapter, OpenAIAdapter, AnthropicAdapter)}


def adapter_from_environ(environ=os.environ):
    """Builds the single installation-selected adapter or refuses to start."""
    name = environ.get("AI_SDLC_MODEL_ADAPTER", "openai")
    cls = ADAPTERS.get(name)
    if cls is None:
        raise RuntimeError("model adapter unavailable")
    seconds = environ.get("AI_SDLC_MODEL_UPSTREAM_SECONDS", "300")
    if not seconds.isdigit() or not 1 <= int(seconds) <= UPSTREAM_SECONDS_MAX:
        raise RuntimeError("model upstream unavailable")
    if cls is AgentCLIAdapter:
        return cls(seconds=int(seconds))
    url = fixed_url(environ.get("AI_SDLC_MODEL_UPSTREAM_URL", ""), cls.path)
    model = environ.get("AI_SDLC_MODEL_UPSTREAM_MODEL", "")
    if not MODEL_ALIAS.fullmatch(model):
        raise RuntimeError("model upstream unavailable")
    credential = read_credential(environ.get("AI_SDLC_MODEL_UPSTREAM_CREDENTIAL_FILE", ""))
    if cls is AnthropicAdapter and not credential:
        raise RuntimeError("model upstream credential unavailable")
    direct = environ.get("AI_SDLC_MODEL_UPSTREAM_DIRECT_ANSWERS", "false")
    if direct not in ("true", "false"):
        raise RuntimeError("model upstream unavailable")
    return cls(url, model, int(seconds), credential, direct == "true")
