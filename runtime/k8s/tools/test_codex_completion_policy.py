"""Optional actual pinned-CLI conformance against a local fake model endpoint."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

from codex_completion_policy import SUPPORTED_VERSION, completion_args, stage_policy
from codex_completion_process import child_environment


@unittest.skipUnless(os.environ.get("AI_SDLC_TEST_CODEX_BINARY"), "actual pinned Codex CLI required")
class ActualPolicyTests(unittest.TestCase):
    def test_actual_model_request_has_no_action_tools(self):
        self.check_actual_request("bad_request")

    def test_server_error_has_no_upstream_retry(self):
        self.check_actual_request("server_error")

    def test_truncated_stream_has_no_upstream_retry(self):
        self.check_actual_request("truncated_stream")

    def check_actual_request(self, mode):
        binary = os.environ["AI_SDLC_TEST_CODEX_BINARY"]
        version = subprocess.run([binary, "--version"], capture_output=True, text=True, timeout=3)
        self.assertEqual(version.stdout.strip(), SUPPORTED_VERSION)
        requests = []

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_POST(self):
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 1024 * 1024:
                    self.send_error(400)
                    return
                payload = json.loads(self.rfile.read(size))
                def names(tools):
                    result = []
                    for tool in tools:
                        if "tools" in tool:
                            result.extend(names(tool["tools"]))
                        else:
                            result.append(tool.get("name", tool.get("type", "unknown")))
                    return result
                # Responses Lite carries tools in additional_tools input items;
                # the ordinary Responses API uses the top-level tools field.
                requests.append({"direct": names(payload.get("tools", [])),
                                 "additional": [names(item.get("tools", [])) for item in payload.get("input", [])
                                                if item.get("type") == "additional_tools"]})
                # Intentionally fail the fake backend; no external model call.
                body = b'{"error":{"message":"fixture complete","type":"invalid_request_error"}}'
                if mode == "truncated_stream":
                    body = b'event: response.created\ndata: {"type":"response.created","response":{"id":"fixture","status":"in_progress"}}\n\n'
                self.send_response({"bad_request": 400, "server_error": 503, "truncated_stream": 200}[mode])
                self.send_header("Content-Type", "text/event-stream" if mode == "truncated_stream" else "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="ai-sdlc-policy-fixture-") as directory:
                stage_policy(directory)
                schema = Path(directory) / "schema.json"
                schema.write_text(json.dumps({"type": "object", "properties": {"content": {"type": "string"}},
                                              "required": ["content"], "additionalProperties": False}))
                args = completion_args(binary, directory, schema, Path(directory) / "output.json", directory)
                args[-1:-1] = ["-c", 'model_providers.ai_sdlc_completion.base_url="http://127.0.0.1:'
                               + str(server.server_port) + '/v1"', "-c",
                               'model_providers.ai_sdlc_completion.requires_openai_auth=false']
                result = subprocess.run(args, input="Return one completion without actions.",
                                        capture_output=True, text=True, timeout=20,
                                        env=child_environment(), cwd=directory)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(requests, [{"direct": [], "additional": []}],
                                 "actual CLI must advertise exactly zero tools")
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
