#!/usr/bin/env python3
"""Loopback completion sidecar backed by a fixed OpenAI-compatible upstream.

The worker/verifier contract is unchanged: the same authenticated loopback
endpoint, request validation and bounded assistant message as the Codex CLI
backend. Only the backend differs: one fixed upstream URL and model alias from
the installation, never from a request. No generated source, database URI,
Kubernetes token or forge credential is read; the upstream receives only the
validated messages and tool schemas.
"""
import ipaddress
import json
import os
import threading
import urllib.error
import urllib.request
from http.server import HTTPServer
from socketserver import ThreadingMixIn
from urllib.parse import urlsplit

from codexcli_openai_wrapper import Handler, MAX_RESPONSE_BYTES, assistant_message, validate_request

# The worker/verifier broker allows seconds + 30 (bounded at 600).
UPSTREAM_SECONDS_MAX = 570
# Explicit RFC 1918 / ULA ranges; ipaddress.is_private also admits
# documentation and other special ranges.
PRIVATE_NETWORKS = tuple(ipaddress.ip_network(n) for n in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"))
PARAMETERS = {"max_tokens", "temperature", "tool_choice"}


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def upstream_config(environ=os.environ):
    url = environ.get("AI_SDLC_MODEL_UPSTREAM_URL", "")
    model = environ.get("AI_SDLC_MODEL_UPSTREAM_MODEL", "")
    seconds = environ.get("AI_SDLC_MODEL_UPSTREAM_SECONDS", "300")
    parts = urlsplit(url)
    if (not url or len(url) > 512 or parts.path != "/v1/chat/completions" or parts.query or parts.fragment
            or parts.username or parts.password or parts.scheme not in ("http", "https") or not parts.hostname):
        raise RuntimeError("model upstream unavailable")
    if parts.scheme == "http":
        # Plain HTTP only to a literal private address (e.g. a local bridge),
        # never a hostname that DNS could redirect.
        try:
            address = ipaddress.ip_address(parts.hostname)
        except ValueError:
            raise RuntimeError("model upstream unavailable") from None
        if not any(address in network for network in PRIVATE_NETWORKS):
            raise RuntimeError("model upstream unavailable")
    if not model or len(model) > 128 or not all(c.isalnum() or c in "._:/-" for c in model):
        raise RuntimeError("model upstream unavailable")
    if not seconds.isdigit() or not 1 <= int(seconds) <= UPSTREAM_SECONDS_MAX:
        raise RuntimeError("model upstream unavailable")
    return url, model, int(seconds)


def upstream_complete(request, url, model, seconds, opener=None):
    messages, tools, names, tokens = validate_request(request)
    payload = {"model": model, "messages": messages, "max_tokens": tokens, "stream": False,
               # Direct answers: reasoning traces are not part of the contract.
               "chat_template_kwargs": {"enable_thinking": False}}
    if tools:
        payload["tools"] = tools
    for key in PARAMETERS - {"max_tokens"}:
        if key in request:
            payload[key] = request[key]
    raw = json.dumps(payload).encode()
    call = urllib.request.Request(url, data=raw, method="POST", headers={"Content-Type": "application/json"})
    opener = opener or urllib.request.build_opener(_NoRedirect(), urllib.request.ProxyHandler({}))
    try:
        with opener.open(call, timeout=seconds) as response:
            if response.status != 200:
                raise ValueError("upstream status")
            body = response.read(MAX_RESPONSE_BYTES + 1)
    except (urllib.error.URLError, OSError, TimeoutError):
        raise RuntimeError("completion backend failed") from None
    if len(body) > MAX_RESPONSE_BYTES:
        raise ValueError("completion response exceeded bound")
    answer = json.loads(body)
    choices = answer.get("choices") if isinstance(answer, dict) else None
    if not isinstance(choices, list) or len(choices) != 1 or not isinstance(choices[0], dict):
        raise ValueError("invalid completion")
    message = choices[0].get("message")
    if not isinstance(message, dict) or message.get("role", "assistant") != "assistant":
        raise ValueError("invalid completion")
    content = message.get("content") or ""
    proposed = []
    for call_item in message.get("tool_calls") or []:
        function = call_item.get("function") if isinstance(call_item, dict) else None
        if not isinstance(function, dict) or call_item.get("type", "function") != "function":
            raise ValueError("invalid proposed tool")
        proposed.append({"name": function.get("name"), "arguments": function.get("arguments")})
    return assistant_message(content, proposed, names)


class UpstreamHandler(Handler):
    def complete_request(self, request):
        url, model, seconds = self.server.upstream
        return upstream_complete(request, url, model, seconds)

    def do_POST(self):
        if not self.server.model_slot.acquire(blocking=False):
            self.reply(429, {"error": {"message": "completion gateway busy"}})
            return
        try:
            super().do_POST()
        finally:
            self.server.model_slot.release()


class UpstreamServer(ThreadingMixIn, HTTPServer):
    daemon_threads = True

    def __init__(self, address, handler, upstream):
        self.upstream = upstream
        self.model_slot = threading.BoundedSemaphore(1)
        super().__init__(address, handler)


def main():
    if len(os.environ.get("CODEX_WRAPPER_TOKEN", "")) < 32:
        raise RuntimeError("model client identity unavailable")
    UpstreamServer(("127.0.0.1", 8787), UpstreamHandler, upstream_config()).serve_forever()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        raise SystemExit("model gateway startup failed") from None
