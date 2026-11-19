"""Model adapters, startup benchmark and gateway keep one loopback contract."""
import json
import os
import tempfile
import time
import threading
import unittest
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

import model_adapters as adapters
import model_benchmark
import model_gateway

TOOLS = [{"type": "function", "function": {"name": "read_file", "description": "Read a file",
          "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}}]
REQUEST = {"model": "codex-cli", "messages": [{"role": "user", "content": "Read README.md"}], "tools": TOOLS}
TOKEN = "t" * 40


class FakeUpstream(BaseHTTPRequestHandler):
    reply = None
    seen = []

    def log_message(self, *_):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        FakeUpstream.seen.append((self.path, {k.lower(): v for k, v in self.headers.items()}, json.loads(body)))
        status, payload, headers = FakeUpstream.reply(json.loads(body)) if callable(FakeUpstream.reply) else FakeUpstream.reply
        self.send_response(status)
        for key, value in headers.items():
            self.send_header(key, value)
        raw = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


def openai_reply(content="", calls=None, usage=None):
    body = {"choices": [{"index": 0, "message": {"role": "assistant", "content": content, "tool_calls": calls}}]}
    if usage:
        body["usage"] = usage
    return body


READ_CALL = [{"id": "x", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}}]


class UpstreamCase(unittest.TestCase):
    def setUp(self):
        self.server = HTTPServer(("127.0.0.1", 0), FakeUpstream)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base = "http://127.0.0.1:%d" % self.server.server_address[1]
        FakeUpstream.seen = []

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()


class OpenAIAdapterTests(UpstreamCase):
    def adapter(self, credential=""):
        return adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "gemma-4-26b-a4b", 5, credential)

    def test_tool_call_is_normalized_and_upstream_sees_only_validated_input(self):
        FakeUpstream.reply = (200, openai_reply(calls=READ_CALL, usage={"prompt_tokens": 40, "completion_tokens": 9}), {})
        result = self.adapter().complete(REQUEST)
        self.assertEqual(result.message["tool_calls"][0]["function"], {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"})
        self.assertEqual((result.prompt_tokens, result.completion_tokens, result.estimated), (40, 9, False))
        path, headers, sent = FakeUpstream.seen[0]
        self.assertEqual(set(sent), {"model", "messages", "max_tokens", "stream", "tools"})
        direct = adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "gemma-4-26b-a4b", 5, "", True)
        direct.complete(REQUEST)
        self.assertEqual(FakeUpstream.seen[1][2]["chat_template_kwargs"], {"enable_thinking": False})
        self.assertNotIn("authorization", headers)

    def test_credential_is_sent_only_as_bearer_and_think_blocks_are_stripped(self):
        FakeUpstream.reply = (200, openai_reply(content="<think>plan</think>\nDone."), {})
        result = self.adapter("sk-test").complete(REQUEST)
        self.assertEqual(result.message["content"], "Done.")
        self.assertTrue(result.estimated)
        self.assertEqual(FakeUpstream.seen[0][1]["authorization"], "Bearer sk-test")

    def test_unknown_tool_bad_arguments_and_malformed_replies_are_rejected(self):
        for reply in (openai_reply(calls=[{"type": "function", "function": {"name": "run_shell", "arguments": "{}"}}]),
                      openai_reply(calls=[{"type": "function", "function": {"name": "read_file", "arguments": "[]"}}]),
                      {"choices": []}, {"choices": [{"message": {"role": "user", "content": "x"}}]},
                      openai_reply(content="x" * (65 * 1024)),
                      {"choices": [{"finish_reason": "length", "message": {"role": "assistant", "content": "par"}}]}):
            FakeUpstream.reply = (200, reply, {})
            with self.subTest(reply=str(reply)[:60]), self.assertRaises(ValueError):
                self.adapter().complete(REQUEST)

    def test_upstream_errors_redirects_and_oversize_fail_closed(self):
        FakeUpstream.reply = (500, {"error": "x"}, {})
        with self.assertRaises(RuntimeError):
            self.adapter().complete(REQUEST)
        FakeUpstream.reply = (302, b"", {"Location": "http://203.0.113.9/v1/chat/completions"})
        with self.assertRaises(RuntimeError):
            self.adapter().complete(REQUEST)
        self.assertEqual(len(FakeUpstream.seen), 2)
        FakeUpstream.reply = (200, b"x" * (adapters.MAX_RESPONSE_BYTES + 10), {})
        with self.assertRaises(ValueError):
            self.adapter().complete(REQUEST)

    def test_request_contract_and_parameters_are_validated_before_any_call(self):
        FakeUpstream.reply = (200, openai_reply(content="ok"), {})
        for bad in ({"model": "other", "messages": REQUEST["messages"]}, {"messages": []},
                    dict(REQUEST, unexpected=True), dict(REQUEST, temperature="hot"), dict(REQUEST, temperature=3),
                    dict(REQUEST, tool_choice="sometimes"),
                    dict(REQUEST, tool_choice={"type": "function", "function": {"name": "run_shell"}})):
            with self.subTest(bad=str(bad)[:80]), self.assertRaises(ValueError):
                self.adapter().complete(bad)
        self.assertEqual(FakeUpstream.seen, [])
        self.adapter().complete(dict(REQUEST, temperature=0, tool_choice={"type": "function", "function": {"name": "read_file"}}))
        self.assertEqual(FakeUpstream.seen[0][2]["temperature"], 0)


class DripUpstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers["Content-Length"]))
        self.send_response(200)
        self.send_header("Content-Length", "40")
        self.end_headers()
        for _ in range(40):
            try:
                self.wfile.write(b" ")
                self.wfile.flush()
            except OSError:
                return
            time.sleep(0.2)


class DeadlineTests(unittest.TestCase):
    def test_a_dripped_body_cannot_outlive_the_total_deadline(self):
        server = HTTPServer(("127.0.0.1", 0), DripUpstream)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        adapter = adapters.OpenAIAdapter("http://127.0.0.1:%d/v1/chat/completions" % server.server_address[1], "m", 1)
        started = time.monotonic()
        with self.assertRaises(RuntimeError):
            adapter.complete(REQUEST)
        self.assertLess(time.monotonic() - started, 2.5)


class AnthropicAdapterTests(UpstreamCase):
    def adapter(self):
        return adapters.AnthropicAdapter(self.base + "/v1/messages", "claude-sonnet-5-5", 5, "key-1")

    def test_conversation_and_tools_translate_to_messages_api_and_back(self):
        FakeUpstream.reply = (200, {"role": "assistant", "content": [
            {"type": "text", "text": "Reading."},
            {"type": "tool_use", "id": "tu1", "name": "read_file", "input": {"path": "README.md"}}],
            "usage": {"input_tokens": 50, "output_tokens": 12}}, {})
        request = {"messages": [
            {"role": "system", "content": "Policy."},
            {"role": "user", "content": "Read README.md"},
            {"role": "assistant", "content": "", "tool_calls": READ_CALL},
            {"role": "tool", "tool_call_id": "x", "content": "# Title"},
            {"role": "user", "content": "Again."}], "tools": TOOLS, "tool_choice": "required", "temperature": 1.5}
        result = self.adapter().complete(request)
        self.assertEqual(result.message["content"], "Reading.")
        self.assertEqual(json.loads(result.message["tool_calls"][0]["function"]["arguments"]), {"path": "README.md"})
        self.assertEqual((result.prompt_tokens, result.completion_tokens, result.estimated), (50, 12, False))
        path, headers, sent = FakeUpstream.seen[0]
        self.assertEqual(path, "/v1/messages")
        self.assertEqual((headers["x-api-key"], headers["anthropic-version"]), ("key-1", "2023-06-01"))
        self.assertEqual(sent["system"], "Policy.")
        self.assertEqual([t["role"] for t in sent["messages"]], ["user", "assistant", "user"])
        self.assertEqual(sent["messages"][1]["content"][0], {"type": "tool_use", "id": "x", "name": "read_file", "input": {"path": "README.md"}})
        self.assertEqual(sent["messages"][2]["content"][0]["type"], "tool_result")
        self.assertEqual(sent["tools"][0]["input_schema"]["required"], ["path"])
        self.assertEqual((sent["tool_choice"], sent["temperature"], sent["max_tokens"]), ({"type": "any"}, 1.0, 2048))

    def test_tool_choice_none_keeps_tools_for_tool_history(self):
        FakeUpstream.reply = (200, {"role": "assistant", "content": [{"type": "text", "text": "ok"}]}, {})
        request = {"messages": [{"role": "user", "content": "x"}, {"role": "assistant", "content": "", "tool_calls": READ_CALL},
                                {"role": "tool", "tool_call_id": "x", "content": "y"}], "tools": TOOLS, "tool_choice": "none"}
        self.adapter().complete(request)
        sent = FakeUpstream.seen[0][2]
        self.assertEqual((sent["tool_choice"], len(sent["tools"])), ({"type": "none"}, 1))

    def test_unknown_blocks_and_unknown_tools_fail_closed(self):
        for content in ([{"type": "image", "source": {}}], [{"type": "tool_use", "name": "run_shell", "input": {}}]):
            FakeUpstream.reply = (200, {"role": "assistant", "content": content}, {})
            with self.subTest(content=content), self.assertRaises(ValueError):
                self.adapter().complete(REQUEST)
        FakeUpstream.reply = (200, {"role": "assistant", "stop_reason": "max_tokens", "content": [{"type": "text", "text": "par"}]}, {})
        with self.assertRaisesRegex(ValueError, "truncated"):
            self.adapter().complete(REQUEST)


class ConfigurationTests(unittest.TestCase):
    OK = {"AI_SDLC_MODEL_ADAPTER": "openai", "AI_SDLC_MODEL_UPSTREAM_URL": "http://10.89.0.1:18080/v1/chat/completions",
          "AI_SDLC_MODEL_UPSTREAM_MODEL": "gemma-4-26b-a4b"}

    def test_each_adapter_accepts_only_its_fixed_private_or_tls_endpoint(self):
        adapter = adapters.adapter_from_environ(self.OK)
        self.assertEqual((adapter.name, adapter.url, adapter.model, adapter.seconds),
                         ("openai", self.OK["AI_SDLC_MODEL_UPSTREAM_URL"], "gemma-4-26b-a4b", 300))
        self.assertEqual(adapters.adapter_from_environ({"AI_SDLC_MODEL_ADAPTER": "agent-cli"}).name, "agent-cli")
        for change in ({"AI_SDLC_MODEL_ADAPTER": "mystery"},
                       {"AI_SDLC_MODEL_UPSTREAM_URL": "http://models.example/v1/chat/completions"},
                       {"AI_SDLC_MODEL_UPSTREAM_URL": "http://203.0.113.9/v1/chat/completions"},
                       {"AI_SDLC_MODEL_UPSTREAM_URL": "http://127.0.0.1:8080/v1/chat/completions"},
                       {"AI_SDLC_MODEL_UPSTREAM_URL": "http://user:pw@10.89.0.1/v1/chat/completions"},
                       {"AI_SDLC_MODEL_UPSTREAM_URL": "http://10.89.0.1:18080/v1/other"},
                       {"AI_SDLC_MODEL_UPSTREAM_MODEL": ""}, {"AI_SDLC_MODEL_UPSTREAM_MODEL": "bad model"},
                       {"AI_SDLC_MODEL_UPSTREAM_SECONDS": "0"}, {"AI_SDLC_MODEL_UPSTREAM_SECONDS": "571"},
                       {"AI_SDLC_MODEL_ADAPTER": "anthropic", "AI_SDLC_MODEL_UPSTREAM_URL": "https://api.example/v1/messages"},
                       {"AI_SDLC_MODEL_UPSTREAM_CREDENTIAL_FILE": "/nonexistent/key"}):
            with self.subTest(change=change), self.assertRaises(RuntimeError):
                adapters.adapter_from_environ(dict(self.OK, **change))

    def test_anthropic_requires_a_mounted_credential(self):
        with tempfile.NamedTemporaryFile("w", delete=False) as handle:
            handle.write("key-1\n")
        try:
            adapter = adapters.adapter_from_environ({"AI_SDLC_MODEL_ADAPTER": "anthropic",
                                                     "AI_SDLC_MODEL_UPSTREAM_URL": "https://api.example/v1/messages",
                                                     "AI_SDLC_MODEL_UPSTREAM_MODEL": "claude-sonnet-5-5",
                                                     "AI_SDLC_MODEL_UPSTREAM_CREDENTIAL_FILE": handle.name})
            self.assertEqual((adapter.name, adapter.credential), ("anthropic", "key-1"))
        finally:
            os.unlink(handle.name)


class ScriptedAdapter(adapters.Adapter):
    """Deterministic adapter: each probe advances a fake clock."""
    name = "openai"
    reports_usage = True

    def __init__(self, clock, prefill_rate=500.0, generation_rate=30.0, tool=True, seconds=300):
        super().__init__("", "fake-model", seconds)
        self.clock, self.prefill_rate, self.generation_rate, self.tool = clock, prefill_rate, generation_rate, tool

    def backend(self, request, messages, tools, names, tokens, seconds):
        prompt = max(1, len(json.dumps(messages)) // 4)
        completion = min(tokens, 150)
        self.clock[0] += prompt / self.prefill_rate + completion / self.generation_rate
        calls = [{"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}] if tools and self.tool else []
        return adapters.Completion(adapters.assistant_message("", calls, names), prompt, completion, False)


class BenchmarkTests(unittest.TestCase):
    def setUp(self):
        self.clock = [0.0]
        original = model_benchmark.time.monotonic
        model_benchmark.time.monotonic = lambda: self.clock[0]
        self.addCleanup(setattr, model_benchmark.time, "monotonic", original)

    def test_timeout_is_derived_from_measured_throughput_for_the_planning_envelope(self):
        profile = model_benchmark.benchmark(ScriptedAdapter(self.clock), 300)
        self.assertEqual(profile["basis"], "throughput")
        self.assertAlmostEqual(profile["promptTokensPerSecond"], 500, delta=60)
        self.assertAlmostEqual(profile["completionTokensPerSecond"], 30, delta=2)
        expected = 1.5 * (16384 / profile["promptTokensPerSecond"] + 4096 / profile["completionTokensPerSecond"]) + 5
        self.assertAlmostEqual(profile["timeoutSeconds"], expected, delta=8)
        self.assertLessEqual(profile["timeoutSeconds"], 300)

    def test_fast_models_get_the_floor_and_slow_or_toolless_models_fail_closed(self):
        self.assertEqual(model_benchmark.benchmark(ScriptedAdapter(self.clock, 50000, 5000), 300)["timeoutSeconds"], 30)
        with self.assertRaisesRegex(model_benchmark.BenchmarkFailed, "too slow"):
            model_benchmark.benchmark(ScriptedAdapter(self.clock, 100, 5), 300)
        with self.assertRaisesRegex(model_benchmark.BenchmarkFailed, "tool call"):
            model_benchmark.benchmark(ScriptedAdapter(self.clock, tool=False), 300)

    def test_prefill_probes_are_never_identical(self):
        self.assertNotEqual(model_benchmark.prefill_probe(), model_benchmark.prefill_probe())

    def test_adapters_without_usage_are_bounded_by_tool_call_latency(self):
        adapter = ScriptedAdapter(self.clock)
        adapter.reports_usage = False
        profile = model_benchmark.benchmark(adapter, 300)
        self.assertEqual(profile["basis"], "tool-call-latency")
        self.assertEqual(profile["timeoutSeconds"], max(30, int(1.5 * 4 * profile["toolCallSeconds"] + 0.999) + 5))


class GatewayTests(unittest.TestCase):
    def setUp(self):
        os.environ["CODEX_WRAPPER_TOKEN"] = TOKEN
        self.clock = [0.0]
        self.adapter = ScriptedAdapter(self.clock)
        self.server = model_gateway.GatewayServer(("127.0.0.1", 0), model_gateway.GatewayHandler, self.adapter)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base = "http://127.0.0.1:%d" % self.server.server_address[1]

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        os.environ.pop("CODEX_WRAPPER_TOKEN", None)

    def call(self, path, body=None, token=TOKEN):
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.base + path, data=data, headers={"Authorization": "Bearer " + token})
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as error:
            return error.code, json.loads(error.read())

    def test_not_ready_and_no_completions_until_benchmarked_then_profile_is_served(self):
        self.assertEqual(self.call("/readyz"), (503, {"ready": False}))
        self.assertEqual(self.call("/v1/chat/completions", REQUEST)[0], 503)
        self.assertEqual(self.call("/readyz", token="x" * 40)[0], 401)
        original = model_benchmark.time.monotonic
        model_benchmark.time.monotonic = lambda: self.clock[0]
        try:
            profile = model_gateway.run_benchmark(self.server, 16384, 4096, attempts=1)
        finally:
            model_benchmark.time.monotonic = original
        self.assertEqual(self.call("/readyz"), (200, {"ready": True}))
        self.assertEqual(self.call("/v1/gateway/profile"), (200, {"timeoutSeconds": profile["timeoutSeconds"]}))
        status, answer = self.call("/v1/chat/completions", REQUEST)
        self.assertEqual(status, 200)
        self.assertEqual(answer["choices"][0]["message"]["tool_calls"][0]["function"]["name"], "read_file")

    def test_failed_benchmark_never_becomes_ready(self):
        self.adapter.tool = False
        self.assertIsNone(model_gateway.run_benchmark(self.server, 16384, 4096, attempts=2, pause=0))
        self.assertEqual(self.call("/readyz")[0], 503)

    def test_envelope_is_bounded(self):
        self.assertEqual(model_gateway.envelope({}), [16384, 4096])
        for bad in ({"AI_SDLC_MODEL_BENCHMARK_PROMPT_TOKENS": "1"}, {"AI_SDLC_MODEL_BENCHMARK_COMPLETION_TOKENS": "x"}):
            with self.subTest(bad=bad), self.assertRaises(RuntimeError):
                model_gateway.envelope(bad)


@unittest.skipUnless(os.environ.get("AI_SDLC_TEST_MODEL_UPSTREAM"), "requires a real OpenAI-compatible upstream")
class RealUpstreamTests(unittest.TestCase):
    def test_real_upstream_benchmarks_and_proposes_a_valid_tool_call(self):
        adapter = adapters.adapter_from_environ({"AI_SDLC_MODEL_ADAPTER": "openai",
                                                 "AI_SDLC_MODEL_UPSTREAM_URL": os.environ["AI_SDLC_TEST_MODEL_UPSTREAM"],
                                                 "AI_SDLC_MODEL_UPSTREAM_MODEL": os.environ.get("AI_SDLC_TEST_MODEL_NAME", "gemma-4-26b-a4b"),
                                                 "AI_SDLC_MODEL_UPSTREAM_DIRECT_ANSWERS": os.environ.get("AI_SDLC_TEST_MODEL_DIRECT", "true")})
        profile = model_benchmark.benchmark(adapter, adapter.seconds)
        print(json.dumps(profile))
        self.assertTrue(profile["toolCall"])
        self.assertLessEqual(profile["timeoutSeconds"], adapter.seconds)


if __name__ == "__main__":
    unittest.main()
