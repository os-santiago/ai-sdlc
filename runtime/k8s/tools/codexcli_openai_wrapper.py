#!/usr/bin/env python3
"""Minimal local OpenAI-compatible adapter for Codex CLI development trials.

It is intentionally localhost-only and for local, supervised use. It bridges a
single non-streaming chat completion to `codex exec`; it is not a production
model gateway, credential store, or multi-tenant service.
"""
import json
import os
import subprocess
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

WORKSPACE = os.environ.get("CODEX_WRAPPER_WORKSPACE", os.getcwd())
PORT = int(os.environ.get("CODEX_WRAPPER_PORT", "8787"))


class Handler(BaseHTTPRequestHandler):
    def log_message(self, _format, *_args):
        pass

    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200); self.end_headers(); self.wfile.write(b"ok\n"); return
        self.send_error(404)

    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_error(404); return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if size < 1 or size > 128 * 1024:
                raise ValueError("request body must be between 1 and 131072 bytes")
            request = json.loads(self.rfile.read(size))
            if request.get("stream"):
                raise ValueError("streaming is not supported by this local adapter")
            messages = request.get("messages", [])
            prompt = "\n\n".join(str(item.get("content", "")) for item in messages if isinstance(item, dict))
            if not prompt:
                raise ValueError("messages must contain prompt content")
            with tempfile.NamedTemporaryFile() as output:
                result = subprocess.run(
                    ["codex", "exec", "--ephemeral", "--sandbox", "workspace-write", "-C", WORKSPACE,
                     "--output-last-message", output.name, prompt],
                    capture_output=True, text=True, timeout=900, check=False,
                )
                output.seek(0)
                content = output.read().decode().strip()
            if result.returncode != 0:
                raise RuntimeError("codex exec failed: " + result.stderr[-1000:])
            response = {"id": "codex-local", "object": "chat.completion", "created": 0,
                        "model": request.get("model", "codex-cli"),
                        "choices": [{"index": 0, "message": {"role": "assistant", "content": content}, "finish_reason": "stop"}]}
            body = json.dumps(response).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
        except Exception as error:
            body = json.dumps({"error": {"message": str(error), "type": "local_wrapper_error"}}).encode()
            self.send_response(400); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
