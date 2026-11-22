#!/usr/bin/env python3
"""Loopback completion gateway with an installation-selected model adapter.

The worker/verifier contract is unchanged: the same authenticated loopback
endpoint, request validation and bounded assistant message. The backend is one
adapter from ``model_adapters.ADAPTERS`` (agent CLI, OpenAI-compatible or
Anthropic Messages), configured only by the installation, never by a request.

At startup the gateway benchmarks the model (``model_benchmark``) before it
reports ready. ``/readyz`` stays 503 until then, and the worker checks it
before claiming any work. The measured profile, including the derived per-call
timeout, is served at ``/v1/gateway/profile`` to the authenticated caller, which
uses it to admit the model. Each upstream call may run up to the configured
ceiling, bounded by the caller's remaining wait. No generated source,
database URI, Kubernetes token or forge credential is read here.
"""
import hmac
import json
import os
import random
import sys
import threading
import time
from http.server import HTTPServer
from socketserver import ThreadingMixIn

from codexcli_openai_wrapper import Handler
from model_adapters import adapter_from_environ
from model_benchmark import BenchmarkFailed, benchmark

ATTEMPTS = 5
# Short randomized backoff: contention between gateways sharing one upstream
# lasts one benchmark (~15-30 s, plus two minimal latency calls). A failed
# attempt usually fails fast; the rollout convergence window (300 s) bounds a
# gateway that keeps failing, which then stays unready (fail closed).
RETRY_SECONDS = (5, 25)
# The broker's remaining wait for this call (modelbroker.CallSecondsHeader).
CALL_SECONDS_HEADER = "X-AI-SDLC-Call-Seconds"


def _authorized(handler):
    token = os.environ.get("CODEX_WRAPPER_TOKEN", "")
    supplied = handler.headers.get_all("Authorization", [])
    return (len(token) >= 32 and len(supplied) == 1
            and hmac.compare_digest(supplied[0].encode(), ("Bearer " + token).encode()))


class GatewayHandler(Handler):
    def complete_request(self, request):
        profile = self.server.profile
        if profile is None:
            raise RuntimeError("model gateway not benchmarked")
        return self.server.adapter.complete(request, self.call_seconds(profile)).message

    def call_seconds(self, profile):
        """The benchmarked timeout admits the model; each real call may run up
        to the installation's ceiling, because real calls have a latency tail
        the startup probes do not show, but never longer than the broker that
        sent it will wait (its call-seconds header). Without that header (an
        older broker) the benchmarked timeout applies, as before."""
        values = self.headers.get_all(CALL_SECONDS_HEADER) or []
        if len(values) == 1 and values[0].isascii() and values[0].isdigit() and 1 <= int(values[0]) <= 600:
            return min(int(values[0]), self.server.adapter.seconds)
        return profile["timeoutSeconds"]

    def do_GET(self):
        self.read_deadline.cancel()
        if self.path not in ("/readyz", "/v1/gateway/profile"):
            super().do_GET()
            return
        if not _authorized(self):
            self.reply(401, {"ready": False})
            return
        profile = self.server.profile
        if profile is None:
            self.reply(503, {"ready": False})
        elif self.path == "/readyz":
            self.reply(200, {"ready": True})
        else:
            self.reply(200, {"timeoutSeconds": profile["timeoutSeconds"]})

    def do_POST(self):
        # Authenticate before revealing benchmark state or taking the slot.
        if not _authorized(self):
            self.reply(401, {"error": {"message": "authentication required"}})
            return
        if self.server.profile is None:
            self.reply(503, {"error": {"message": "model gateway benchmarking"}})
            return
        if not self.server.model_slot.acquire(blocking=False):
            self.reply(429, {"error": {"message": "completion gateway busy"}})
            return
        try:
            super().do_POST()
        finally:
            self.server.model_slot.release()


class GatewayServer(ThreadingMixIn, HTTPServer):
    daemon_threads = True

    def __init__(self, address, handler, adapter):
        self.adapter = adapter
        self.profile = None
        self.model_slot = threading.BoundedSemaphore(1)
        super().__init__(address, handler)


def envelope(environ=os.environ):
    values = []
    for key, default, low, high in (("AI_SDLC_MODEL_BENCHMARK_PROMPT_TOKENS", "16384", 256, 262144),
                                    ("AI_SDLC_MODEL_BENCHMARK_COMPLETION_TOKENS", "4096", 64, 32768)):
        raw = environ.get(key, default)
        if not raw.isdigit() or not low <= int(raw) <= high:
            raise RuntimeError("model benchmark envelope invalid")
        values.append(int(raw))
    return values


def run_benchmark(server, prompt_tokens, completion_tokens, attempts=ATTEMPTS, pause=RETRY_SECONDS):
    """Benchmarks with a bounded number of attempts (a model may still be
    loading). Returns the profile, or None after the last failure."""
    for attempt in range(attempts):
        try:
            profile = benchmark(server.adapter, server.adapter.seconds, prompt_tokens, completion_tokens)
        except BenchmarkFailed as failure:
            print(json.dumps({"event": "model-benchmark-failed", "attempt": attempt + 1, "reason": str(failure)}), flush=True)
            if attempt + 1 < attempts:
                # Randomized backoff: gateways sharing one upstream start
                # together and would otherwise measure each other's load.
                time.sleep(random.uniform(*pause) if isinstance(pause, (tuple, list)) else pause)
            continue
        print(json.dumps({"event": "model-benchmark", "profile": profile}), flush=True)
        server.profile = profile
        return profile
    return None


def main():
    if len(os.environ.get("CODEX_WRAPPER_TOKEN", "")) < 32:
        raise RuntimeError("model client identity unavailable")
    adapter = adapter_from_environ()
    prompt_tokens, completion_tokens = envelope()
    server = GatewayServer(("127.0.0.1", 8787), GatewayHandler, adapter)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    if run_benchmark(server, prompt_tokens, completion_tokens) is None:
        # Never ready with an unmeasured model; the restart is the signal.
        server.shutdown()
        sys.exit(1)
    threading.Event().wait()


if __name__ == "__main__":
    try:
        main()
    except SystemExit:
        raise
    except Exception:
        raise SystemExit("model gateway startup failed") from None
