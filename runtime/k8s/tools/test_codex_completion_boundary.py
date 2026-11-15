import json
import os
import socket
import threading
import time
import unittest
from unittest.mock import patch

import codexcli_openai_wrapper as wrapper

TOKEN = "fixture-token-012345678901234567890123456789"


class BoundaryTests(unittest.TestCase):
    def setUp(self):
        self.server = wrapper.HTTPServer(("127.0.0.1", 0), wrapper.Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.environment = patch.dict(os.environ, {"CODEX_WRAPPER_TOKEN": TOKEN})
        self.environment.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.environment.stop()

    def exchange(self, data):
        with socket.create_connection(self.server.server_address, timeout=2) as client:
            client.sendall(data)
            client.shutdown(socket.SHUT_WR)
            chunks = []
            while chunk := client.recv(8192):
                chunks.append(chunk)
            return b"".join(chunks)

    def test_ambiguous_framing_is_rejected_before_backend(self):
        cases = [b"Content-Length: 2\r\nContent-Length: 2", b"Content-Length: 2\r\nTransfer-Encoding: chunked",
                 b"Content-Length: 131073", b"Content-Length: -1", b"Content-Length: 0"]
        with patch.object(wrapper, "complete") as backend:
            for framing in cases:
                with self.subTest(framing=framing):
                    response = self.exchange(b"POST /v1/chat/completions HTTP/1.1\r\nAuthorization: Bearer "
                                             + TOKEN.encode() + b"\r\n" + framing + b"\r\n\r\n{}")
                    self.assertIn(b" 400 ", response.split(b"\r\n")[0])
            backend.assert_not_called()

    def test_non_ascii_authentication_is_rejected_cleanly(self):
        with patch.object(wrapper, "complete") as backend:
            response = self.exchange(b"POST /v1/chat/completions HTTP/1.1\r\nAuthorization: Bearer \xff\r\nContent-Length: 2\r\n\r\n{}")
            self.assertIn(b" 401 ", response.split(b"\r\n")[0])
            backend.assert_not_called()

    def test_slow_request_line_headers_and_body_have_absolute_deadline(self):
        prefixes = [b"P", b"POST /v1/chat/completions HTTP/1.1\r\nX-Slow: ",
                    b"POST /v1/chat/completions HTTP/1.1\r\nAuthorization: Bearer " + TOKEN.encode()
                    + b"\r\nContent-Length: 10000\r\n\r\n"]
        with patch.object(wrapper, "READ_SECONDS", 0.2), patch.object(wrapper, "complete") as backend:
            for prefix in prefixes:
                with self.subTest(prefix=prefix[:20]), socket.create_connection(self.server.server_address, timeout=1) as client:
                    client.sendall(prefix)
                    start = time.monotonic()
                    # Keep sending more often than an inactivity timeout, then
                    # prove the server closed despite continued input.
                    for _ in range(8):
                        try:
                            client.sendall(b"x")
                        except OSError:
                            break
                        time.sleep(0.05)
                    try:
                        while client.recv(8192):
                            pass
                    except ConnectionResetError:
                        pass
                    self.assertLess(time.monotonic() - start, 0.9)
            backend.assert_not_called()

    def test_json_and_request_bounds_reject_before_model(self):
        with patch.object(wrapper, "complete_process") as backend:
            for raw in ('{"messages":[],"Messages":[]}', '{"x":NaN}', '[' * 34 + '0' + ']' * 34):
                with self.subTest(raw=raw), self.assertRaises(ValueError):
                    wrapper.strict_json(raw)
            base = {"messages": [{"role": "user", "content": "task"}]}
            for update in ({"model": "other"}, {"stream": "true"}, {"max_tokens": True}, {"max_tokens": 8193},
                           {"unknown": 1}, {"messages": []}, {"messages": [{"role": "user", "content": "x" * 131072}]},
                           {"tools": [{"type": "function", "function": {"name": "../shell"}}]}):
                with self.subTest(update=list(update)), self.assertRaises(ValueError):
                    wrapper.complete(base | update)
            backend.assert_not_called()

    def test_invalid_backend_proposals_are_rejected(self):
        request = {"messages": [{"role": "user", "content": "task"}],
                   "tools": [{"type": "function", "function": {"name": "read_file"}}]}
        answers = [{"content": "x" * 65537, "tool_calls": []},
                   {"content": "", "tool_calls": [{"name": "execute", "arguments": "{}"}]},
                   {"content": "", "tool_calls": [{"name": "read_file", "arguments": "[]"}]},
                   {"content": "", "tool_calls": [{"name": "read_file", "arguments": "{}"}] * 9},
                   {"content": "", "tool_calls": [], "extra": 1}]
        for answer in answers:
            with self.subTest(answer=list(answer)), patch.object(wrapper, "complete_process", return_value=json.dumps(answer)):
                with self.assertRaises(ValueError):
                    wrapper.complete(request)


if __name__ == "__main__":
    unittest.main()
