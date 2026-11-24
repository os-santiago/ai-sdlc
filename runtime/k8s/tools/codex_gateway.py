#!/usr/bin/env python3
"""Fixed container entrypoint for the loopback completion sidecar.

Only the model authentication file and ephemeral model home are mounted here.
No generated source, database URI, Kubernetes token or forge credential is read.
"""
import json
import os
import threading
from socketserver import ThreadingMixIn
from pathlib import Path
from http.server import HTTPServer

from codex_completion_process import backend_binary
from codex_completion_policy import policy_snapshot
from codexcli_openai_wrapper import Handler, narrow_tools, tool_allowlist


class GatewayHandler(Handler):
    def complete_request(self, request):
        # The installation's tool allowlist (review gateway only); see
        # codexcli_openai_wrapper.narrow_tools.
        return super().complete_request(narrow_tools(request, self.server.tools)[0])

    def do_POST(self):
        if not self.server.model_slot.acquire(blocking=False):
            self.reply(429, {"error": {"message": "completion gateway busy"}})
            return
        try:
            super().do_POST()
        finally:
            self.server.model_slot.release()


class GatewayServer(ThreadingMixIn, HTTPServer):
    daemon_threads = True

    def __init__(self, *args, tools=None):
        self.tools = tools
        self.connections = threading.BoundedSemaphore(4)
        self.model_slot = threading.BoundedSemaphore(1)
        super().__init__(*args)

    def process_request(self, request, client_address):
        if not self.connections.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except Exception:
            self.connections.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.connections.release()


def main():
    if len(os.environ.get("CODEX_WRAPPER_TOKEN", "")) < 32:
        raise RuntimeError("model client identity unavailable")
    Path("/model-home/tmp").mkdir(mode=0o700, parents=True, exist_ok=True)
    root = Path("/model-home/codex")
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    source = Path("/var/run/ai-sdlc/codex/auth.json")
    with source.open("rb") as stream:
        raw = stream.read(16385)
    if not 0 < len(raw) <= 16384 or not isinstance(json.loads(raw), dict):
        raise RuntimeError("model authentication unavailable")
    # Preserve CLI refreshes within this Pod lifetime; mounted configuration is
    # never mutated. A new Pod always starts from the installation-owned Secret.
    target = root / "auth.json"
    if not target.exists():
        descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(raw)
    os.environ["CODEX_HOME"] = str(root)
    os.environ["HOME"] = "/model-home"
    backend_binary()
    policy_snapshot()
    GatewayServer(("127.0.0.1", 8787), GatewayHandler, tools=tool_allowlist()).serve_forever()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        raise SystemExit("model gateway startup failed") from None
