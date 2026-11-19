"""OpenAI-compatible upstream backend keeps the loopback gateway contract."""
import json
import os
import threading
import unittest
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

import openai_upstream_gateway as gateway

TOOLS = [{"type": "function", "function": {"name": "read_file", "description": "Read a file",
          "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}}]
REQUEST = {"model": "codex-cli", "messages": [{"role": "user", "content": "Read README.md"}], "tools": TOOLS}


class FakeUpstream(BaseHTTPRequestHandler):
    reply = None
    seen = []

    def log_message(self, *_):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        FakeUpstream.seen.append(json.loads(body))
        status, payload, headers = FakeUpstream.reply
        self.send_response(status)
        for key, value in headers.items():
            self.send_header(key, value)
        raw = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


def message(content="", calls=None):
    return {"choices": [{"index": 0, "message": {"role": "assistant", "content": content, "tool_calls": calls}}]}


class UpstreamTests(unittest.TestCase):
    def setUp(self):
        self.server = HTTPServer(("127.0.0.1", 0), FakeUpstream)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = "http://127.0.0.1:%d/v1/chat/completions" % self.server.server_address[1]
        FakeUpstream.seen = []

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def complete(self, request=REQUEST):
        return gateway.upstream_complete(request, self.url, "gemma-4-26b-a4b", 5)

    def test_tool_call_is_normalized_and_upstream_sees_only_validated_input(self):
        FakeUpstream.reply = (200, message(calls=[{"id": "x", "type": "function",
                              "function": {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}}]), {})
        result = self.complete()
        self.assertEqual(result["role"], "assistant")
        self.assertEqual(result["tool_calls"][0]["function"], {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"})
        sent = FakeUpstream.seen[0]
        self.assertEqual(sent["model"], "gemma-4-26b-a4b")
        self.assertEqual(sent["chat_template_kwargs"], {"enable_thinking": False})
        self.assertEqual(set(sent), {"model", "messages", "max_tokens", "stream", "chat_template_kwargs", "tools"})

    def test_unknown_tool_bad_arguments_and_malformed_replies_are_rejected(self):
        for reply in (message(calls=[{"type": "function", "function": {"name": "run_shell", "arguments": "{}"}}]),
                      message(calls=[{"type": "function", "function": {"name": "read_file", "arguments": "[]"}}]),
                      {"choices": []}, {"choices": [{"message": {"role": "user", "content": "x"}}]},
                      message(content="x" * (65 * 1024))):
            FakeUpstream.reply = (200, reply, {})
            with self.subTest(reply=str(reply)[:60]), self.assertRaises(ValueError):
                self.complete()

    def test_upstream_errors_redirects_and_oversize_fail_closed(self):
        FakeUpstream.reply = (500, {"error": "x"}, {})
        with self.assertRaises(RuntimeError):
            self.complete()
        FakeUpstream.reply = (302, b"", {"Location": "http://203.0.113.9/v1/chat/completions"})
        with self.assertRaises(RuntimeError):
            self.complete()
        self.assertEqual(len(FakeUpstream.seen), 2)
        FakeUpstream.reply = (200, b"x" * (gateway.MAX_RESPONSE_BYTES + 10), {})
        with self.assertRaises(ValueError):
            self.complete()

    def test_request_contract_is_the_codex_backend_contract(self):
        FakeUpstream.reply = (200, message(content="ok"), {})
        for bad in ({"model": "other", "messages": REQUEST["messages"]}, {"messages": []},
                    dict(REQUEST, unexpected=True)):
            with self.subTest(bad=str(bad)[:60]), self.assertRaises(ValueError):
                self.complete(bad)
        self.assertEqual(FakeUpstream.seen, [])


class ConfigurationTests(unittest.TestCase):
    def test_only_fixed_private_or_tls_upstreams(self):
        ok = {"AI_SDLC_MODEL_UPSTREAM_URL": "http://10.89.0.1:18080/v1/chat/completions",
              "AI_SDLC_MODEL_UPSTREAM_MODEL": "gemma-4-26b-a4b"}
        self.assertEqual(gateway.upstream_config(ok), (ok["AI_SDLC_MODEL_UPSTREAM_URL"], "gemma-4-26b-a4b", 300))
        self.assertEqual(gateway.upstream_config(dict(ok, AI_SDLC_MODEL_UPSTREAM_URL="https://models.example/v1/chat/completions"))[0],
                         "https://models.example/v1/chat/completions")
        for change in ({"AI_SDLC_MODEL_UPSTREAM_URL": "http://models.example/v1/chat/completions"},
                       {"AI_SDLC_MODEL_UPSTREAM_URL": "http://203.0.113.9/v1/chat/completions"},
                       {"AI_SDLC_MODEL_UPSTREAM_URL": "http://127.0.0.1:8080/v1/chat/completions"},
                       {"AI_SDLC_MODEL_UPSTREAM_URL": "http://user:pw@10.89.0.1/v1/chat/completions"},
                       {"AI_SDLC_MODEL_UPSTREAM_URL": "http://10.89.0.1:18080/v1/other"},
                       {"AI_SDLC_MODEL_UPSTREAM_MODEL": ""}, {"AI_SDLC_MODEL_UPSTREAM_MODEL": "bad model"},
                       {"AI_SDLC_MODEL_UPSTREAM_SECONDS": "0"}, {"AI_SDLC_MODEL_UPSTREAM_SECONDS": "601"}):
            with self.subTest(change=change), self.assertRaises(RuntimeError):
                gateway.upstream_config(dict(ok, **change))


@unittest.skipUnless(os.environ.get("AI_SDLC_TEST_MODEL_UPSTREAM"), "requires a real OpenAI-compatible upstream")
class RealUpstreamTests(unittest.TestCase):
    def test_real_upstream_proposes_a_valid_tool_call(self):
        url, model, seconds = gateway.upstream_config({"AI_SDLC_MODEL_UPSTREAM_URL": os.environ["AI_SDLC_TEST_MODEL_UPSTREAM"],
                                                       "AI_SDLC_MODEL_UPSTREAM_MODEL": os.environ.get("AI_SDLC_TEST_MODEL_NAME", "gemma-4-26b-a4b")})
        result = gateway.upstream_complete(REQUEST, url, model, seconds)
        self.assertEqual(result["tool_calls"][0]["function"]["name"], "read_file")
        self.assertEqual(json.loads(result["tool_calls"][0]["function"]["arguments"])["path"], "README.md")


if __name__ == "__main__":
    unittest.main()
