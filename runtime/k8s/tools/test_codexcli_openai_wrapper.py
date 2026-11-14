import json
import os
import threading
import unittest
import urllib.error
import urllib.request
from pathlib import Path
from unittest.mock import patch

import codexcli_openai_wrapper as wrapper


class AdapterTests(unittest.TestCase):
    def test_completion_proposes_tools_instead_of_editing_checkout(self):
        def invoke(args, **kwargs):
            self.assertEqual(args[args.index("--sandbox") + 1], "read-only")
            self.assertIn("--ignore-user-config", args)
            self.assertEqual(args[-1], "-")
            self.assertIn('"role": "system"', kwargs["input"])
            output = Path(args[args.index("--output-last-message") + 1])
            output.write_text(json.dumps({"content": "", "tool_calls": [{"name": "read_file", "arguments": '{"path":"README.md"}'}]}))
            return type("Result", (), {"returncode": 0})()
        with patch.object(wrapper.subprocess, "run", side_effect=invoke):
            result = wrapper.complete({"messages": [{"role": "system", "content": "policy"}], "tools": [{"function": {"name": "read_file"}}]})
            later = wrapper.complete({"messages": [{"role": "system", "content": "policy"}], "tools": [{"function": {"name": "read_file"}}]})
        self.assertEqual(result["tool_calls"][0]["function"]["name"], "read_file")
        self.assertNotEqual(result["tool_calls"][0]["id"], later["tool_calls"][0]["id"])

    def test_http_auth_streaming_and_sanitized_failure(self):
        server = wrapper.HTTPServer(("127.0.0.1", 0), wrapper.Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            url = f"http://127.0.0.1:{server.server_port}/v1/chat/completions"
            request = urllib.request.Request(url, data=json.dumps({"stream": True, "messages": [{"role": "user", "content": "task"}]}).encode())
            with patch.dict(os.environ, {"CODEX_WRAPPER_TOKEN": "test-token"}):
                with self.assertRaises(urllib.error.HTTPError) as failure:
                    urllib.request.urlopen(request)
                self.assertEqual(failure.exception.code, 401)
                failure.exception.close()
                request.add_header("Authorization", "Bearer test-token")
                message = {"role": "assistant", "content": "", "tool_calls": [{"id": "call_0", "type": "function", "function": {"name": "read_file", "arguments": "{}"}}]}
                with patch.object(wrapper, "complete", return_value=message):
                    with urllib.request.urlopen(request) as response:
                        chunks = response.read().decode()
                        self.assertEqual(response.headers["Content-Type"], "text/event-stream")
                        self.assertIn('"finish_reason": "tool_calls"', chunks)
                        self.assertIn('"index": 0', chunks)
                        self.assertTrue(chunks.endswith("data: [DONE]\n\n"))
                with patch.object(wrapper, "complete", side_effect=RuntimeError("credential must not escape")):
                    with self.assertRaises(urllib.error.HTTPError) as failure:
                        urllib.request.urlopen(request)
                    self.assertEqual(failure.exception.code, 502)
                    self.assertNotIn("credential", failure.exception.read().decode())
                    failure.exception.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
