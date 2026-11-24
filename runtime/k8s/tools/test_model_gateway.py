"""Model adapters, startup benchmark and gateway keep one loopback contract."""
import json
import os
import tempfile
import time
import threading
import unittest
import unittest.mock
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer, ThreadingHTTPServer

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
        self.assertEqual(set(sent), {"model", "messages", "max_tokens", "stream", "stream_options", "tools"})
        self.assertEqual((sent["stream"], sent["stream_options"]), (True, {"include_usage": True}))
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


class HeaderDrip(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers["Content-Length"]))
        for byte in b"HTTP/1.1 200 OK\r\nX-Slow: " + b"a" * 40:
            try:
                self.wfile.write(bytes([byte]))
                self.wfile.flush()
            except OSError:
                return
            time.sleep(0.2)


class DeadlineTests(unittest.TestCase):
    def test_dripped_headers_cannot_outlive_the_total_deadline(self):
        server = HTTPServer(("127.0.0.1", 0), HeaderDrip)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        adapter = adapters.OpenAIAdapter("http://127.0.0.1:%d/v1/chat/completions" % server.server_address[1], "m", 1)
        started = time.monotonic()
        with self.assertRaises(RuntimeError):
            adapter.complete(REQUEST)
        self.assertLess(time.monotonic() - started, 2.5)

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


def sse_event(value):
    return b"data: " + (value if isinstance(value, bytes) else json.dumps(value).encode()) + b"\n\n"


def chunk(content=None, calls=None, finish=None, role=None, usage=None, choices=True):
    """One ``chat.completion.chunk`` as llama.cpp streams it."""
    body = {"object": "chat.completion.chunk", "choices": []}
    if choices:
        delta = {}
        if role:
            delta["role"] = role
        if content is not None:
            delta["content"] = content
        if calls is not None:
            delta["tool_calls"] = calls
        body["choices"] = [{"index": 0, "delta": delta, "finish_reason": finish}]
    if usage is not None:
        body["usage"] = usage
    return body


def stream(*chunks, done=True):
    return b"".join(sse_event(c) for c in chunks) + (sse_event(b"[DONE]") if done else b"")


def degenerate_stream(_body):
    """Starts like a real answer, then emits ``?`` until it is disconnected."""
    yield sse_event(chunk(content="I will read the file", role="assistant"))
    for _ in range(20000):
        yield sse_event(chunk(content="??"))
        time.sleep(0.001)


class SSEUpstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"
    script = []
    seen = []
    disconnected = []

    def log_message(self, *_):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        SSEUpstream.seen.append(body)
        reply = SSEUpstream.script[min(len(SSEUpstream.seen), len(SSEUpstream.script)) - 1]
        pieces = reply(body) if callable(reply) else [reply]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        try:
            for piece in pieces:
                self.wfile.write(piece)
                self.wfile.flush()
        except OSError:
            SSEUpstream.disconnected.append(time.monotonic())


TOOLS2 = TOOLS + [{"type": "function", "function": {"name": "list_dir", "description": "List a directory",
                   "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}}]
REQUEST2 = dict(REQUEST, tools=TOOLS2)


class StreamingTests(unittest.TestCase):
    """The openai adapter streams, assembles the non-streaming answer and
    guards it against degenerate output with one bounded retry."""

    def setUp(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), SSEUpstream)
        self.server.daemon_threads = True
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = "http://127.0.0.1:%d/v1/chat/completions" % self.server.server_address[1]
        SSEUpstream.seen, SSEUpstream.disconnected = [], []

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def adapter(self, seconds=30):
        return adapters.OpenAIAdapter(self.url, "qwen3-coder", seconds)

    @staticmethod
    def shape(message):
        """The message without the per-call random ids the gateway assigns."""
        calls = [call["function"] for call in message.get("tool_calls", [])]
        return message["content"], calls

    def test_streamed_content_and_tool_calls_assemble_like_the_non_streaming_answer(self):
        content = "<think>plan</think>\nReading both."
        calls = [{"id": "a", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\": \"README.md\"}"}},
                 {"id": "b", "type": "function", "function": {"name": "list_dir", "arguments": "{\"path\": \"docs\"}"}}]
        usage = {"prompt_tokens": 120, "completion_tokens": 31, "total_tokens": 151}
        # Content split per token; each call's id, name and type only in its
        # first delta, its arguments split across deltas and interleaved by
        # index; a keep-alive comment, CRLF line ends and a usage-only chunk.
        events = [chunk(role="assistant", content="")] + [chunk(content=piece) for piece in ("<think>pl", "an</think>\nRe", "ading", " both.")]
        events += [chunk(calls=[{"index": 0, "id": "a", "type": "function", "function": {"name": "read_file", "arguments": ""}}]),
                   chunk(calls=[{"index": 0, "function": {"arguments": "{\"path\": "}}]),
                   chunk(calls=[{"index": 1, "id": "b", "type": "function", "function": {"name": "list_dir", "arguments": "{\"pa"}}]),
                   chunk(calls=[{"index": 0, "function": {"arguments": "\"README.md\"}"}}]),
                   chunk(calls=[{"index": 1, "function": {"arguments": "th\": \"docs\"}"}}]),
                   chunk(finish="tool_calls"), chunk(usage=usage, choices=False)]
        body = stream(*events).replace(b"\n\n", b"\r\n\r\n", 3)
        SSEUpstream.script = [b": ping\n\n" + body]
        streamed = self.adapter().complete(REQUEST2)

        upstream = HTTPServer(("127.0.0.1", 0), FakeUpstream)
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        self.addCleanup(upstream.server_close)
        self.addCleanup(upstream.shutdown)
        FakeUpstream.seen = []
        FakeUpstream.reply = (200, dict(openai_reply(content, calls, usage), choices=[{"index": 0, "finish_reason": "tool_calls",
                              "message": {"role": "assistant", "content": content, "tool_calls": calls}}]), {})
        plain = adapters.OpenAIAdapter("http://127.0.0.1:%d/v1/chat/completions" % upstream.server_address[1], "qwen3-coder", 30).complete(REQUEST2)

        self.assertEqual(self.shape(streamed.message), self.shape(plain.message))
        self.assertEqual(self.shape(streamed.message), ("Reading both.", [c["function"] for c in calls]))
        self.assertEqual(set(streamed.message), set(plain.message))
        self.assertEqual([c["type"] for c in streamed.message["tool_calls"]], ["function", "function"])
        self.assertEqual((streamed.prompt_tokens, streamed.completion_tokens, streamed.estimated), (120, 31, False))
        self.assertEqual((streamed.degenerate_retries, len(SSEUpstream.seen)), (0, 1))
        self.assertEqual(SSEUpstream.seen[0]["stream_options"], {"include_usage": True})

    def test_usage_is_taken_from_any_chunk_and_estimated_when_absent(self):
        SSEUpstream.script = [stream(chunk(content="Done.", role="assistant"),
                                     chunk(finish="stop", usage={"prompt_tokens": 9, "completion_tokens": 2}))]
        result = self.adapter().complete(REQUEST)
        self.assertEqual((result.message["content"], result.prompt_tokens, result.completion_tokens, result.estimated), ("Done.", 9, 2, False))
        # A server without include_usage: estimates, as without usage before.
        SSEUpstream.script = [stream(chunk(content="Done.", role="assistant"), chunk(finish="stop"))]
        result = self.adapter().complete(REQUEST)
        self.assertTrue(result.estimated)
        self.assertEqual(result.completion_tokens, 1)
        # Malformed counts are not trusted either.
        SSEUpstream.script = [stream(chunk(content="Done."), chunk(finish="stop", usage={"prompt_tokens": -1, "completion_tokens": "2"}))]
        self.assertTrue(self.adapter().complete(REQUEST).estimated)

    def test_degenerate_first_answer_is_disconnected_and_retried_once(self):
        clean = stream(chunk(calls=[{"index": 0, "id": "c", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}}]),
                       chunk(finish="tool_calls"))
        SSEUpstream.script = [degenerate_stream, clean]
        started = time.monotonic()
        result = self.adapter().complete(REQUEST)
        self.assertLess(time.monotonic() - started, 5)
        self.assertEqual(result.degenerate_retries, 1)
        self.assertEqual(result.message["tool_calls"][0]["function"]["name"], "read_file")
        self.assertEqual(len(SSEUpstream.seen), 2)
        self.assertEqual(SSEUpstream.seen[0], SSEUpstream.seen[1])  # the same request, unchanged
        # The upstream saw the first connection close: llama.cpp then cancels the generation.
        deadline = time.monotonic() + 3
        while not SSEUpstream.disconnected and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertEqual(len(SSEUpstream.disconnected), 1)

    def test_a_second_degenerate_answer_fails_the_call_as_degenerate(self):
        SSEUpstream.script = [degenerate_stream]
        with self.assertRaises(adapters.DegenerateCompletion) as caught:
            self.adapter().complete(REQUEST)
        self.assertEqual((caught.exception.degenerate_retries, len(SSEUpstream.seen)), (1, 2))
        self.assertEqual(model_gateway.failure_class(caught.exception), "degenerate")
        # Corrupted tool arguments are guarded too.
        def corrupted_arguments(_body):
            yield sse_event(chunk(calls=[{"index": 0, "id": "x", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\""}}]))
            for _ in range(5000):
                yield sse_event(chunk(calls=[{"index": 0, "function": {"arguments": "a?"}}]))
        SSEUpstream.seen, SSEUpstream.script = [], [corrupted_arguments]
        with self.assertRaises(adapters.DegenerateCompletion):
            self.adapter().complete(REQUEST)
        self.assertEqual(len(SSEUpstream.seen), 2)

    def test_no_retry_without_budget_and_the_retry_shares_one_deadline(self):
        # 5 s of budget is below the retry minimum: fail at once, no retry.
        SSEUpstream.script = [degenerate_stream]
        with self.assertRaises(adapters.DegenerateCompletion) as caught:
            self.adapter(seconds=5).complete(REQUEST)
        self.assertEqual((caught.exception.degenerate_retries, len(SSEUpstream.seen)), (0, 1))
        # A retry that stalls ends at the original deadline, not a fresh one.
        def stalled(_body):
            yield sse_event(chunk(content="Reading", role="assistant"))
            for _ in range(100):
                yield b": ping\n\n"
                time.sleep(0.1)
        SSEUpstream.seen, SSEUpstream.script = [], [degenerate_stream, stalled]
        started = time.monotonic()
        with unittest.mock.patch.object(adapters, "DEGENERATE_RETRY_MIN_SECONDS", 1), \
                self.assertRaisesRegex(RuntimeError, "deadline") as caught:
            self.adapter(seconds=2).complete(REQUEST)
        self.assertLess(time.monotonic() - started, 3)
        self.assertEqual((caught.exception.degenerate_retries, len(SSEUpstream.seen)), (1, 2))

    def test_other_failures_are_never_retried(self):
        for reply in (stream(chunk(content="par", role="assistant"), chunk(finish="length")),
                      stream(chunk(content="x"), {"error": {"code": 500, "message": "boom"}}, done=False),
                      stream(chunk(content="cut off"), done=False),
                      # [DONE] without a finish reason is not a complete answer.
                      stream(chunk(content="no finish", role="assistant")),
                      # Bare-CR line ends are not split: one oversized or invalid line.
                      stream(chunk(content="x"), chunk(finish="stop")).replace(b"\n", b"\r"),
                      # A multi-line event over the per-event bound.
                      b"".join(b"data: " + b"x" * 1000 + b"\n" for _ in range(300)) + b"\n" + stream(chunk(finish="stop")),
                      stream(chunk(calls=[{"index": 0, "function": {"name": "run_shell", "arguments": "{}"}}]), chunk(finish="tool_calls")),
                      stream(chunk(calls=[{"index": 8, "function": {"name": "read_file", "arguments": "{}"}}])),
                      b"data: {not json\n\n",
                      stream(*[chunk(content="word " * 1000) for _ in range(60)])):
            SSEUpstream.seen, SSEUpstream.script = [], [reply]
            with self.subTest(reply=reply[:80]), self.assertRaises((ValueError, RuntimeError)) as caught:
                self.adapter().complete(REQUEST)
            self.assertNotIsInstance(caught.exception, adapters.DegenerateCompletion)
            self.assertEqual(len(SSEUpstream.seen), 1)
        classes = []
        for reply in (stream(chunk(content="par", role="assistant"), chunk(finish="length")),
                      stream(chunk(content="x"), {"error": {"code": 500}}, done=False),
                      stream(*[chunk(content="word " * 1000) for _ in range(60)]),
                      b"".join(b"data: " + b"x" * 1000 + b"\n" for _ in range(300)) + b"\n" + stream(chunk(finish="stop"))):
            SSEUpstream.script = [reply]
            try:
                self.adapter().complete(REQUEST)
            except Exception as error:
                classes.append(model_gateway.failure_class(error))
        self.assertEqual(classes, ["truncated", "backend", "oversized", "oversized"])

    def test_events_follow_the_sse_format(self):
        # Several data lines join with a line feed up to the blank line (with
        # or without the space after the colon, CRLF or LF); comments, other
        # fields and empty events are skipped; the end of the stream ends the
        # last event.
        first = json.dumps(chunk(content="Hel", role="assistant"), indent=1).encode().split(b"\n")
        events = (b"event: message\r\nid: 1\r\n" + b"\r\n".join(b"data: " + line for line in first[:3]) + b"\r\n: ping\r\n"
                  + b"\r\n".join(b"data:" + line for line in first[3:]) + b"\r\n\r\n"
                  + b"data:\n\n" + b"data\n\n" + b": comment only\n\n"
                  + sse_event(chunk(content="lo")) + b"data: " + json.dumps(chunk(finish="stop")).encode())
        SSEUpstream.script = [events]
        self.assertEqual(self.adapter().complete(REQUEST).message["content"], "Hello")
        # [DONE] split from nothing else still ends the stream; later bytes are ignored.
        SSEUpstream.script = [stream(chunk(content="ok"), chunk(finish="stop")) + b"data: {not json\n\n"]
        self.assertEqual(self.adapter().complete(REQUEST).message["content"], "ok")

    def test_reasoning_fields_are_watched_and_lone_surrogates_judged_as_before(self):
        # vLLM streams reasoning as "reasoning", llama.cpp as "reasoning_content".
        for key in ("reasoning", "reasoning_content"):
            def looping(_body, key=key):
                for _ in range(5000):
                    yield sse_event({"choices": [{"index": 0, "delta": {key: "?"}, "finish_reason": None}]})
            SSEUpstream.seen, SSEUpstream.script = [], [looping]
            with self.subTest(key=key), self.assertRaises(adapters.DegenerateCompletion):
                self.adapter().complete(REQUEST)
        # Reasoning text is never returned.
        SSEUpstream.script = [stream({"choices": [{"index": 0, "delta": {"reasoning": "plan"}}]}, chunk(content="Done."), chunk(finish="stop"))]
        self.assertEqual(self.adapter().complete(REQUEST).message["content"], "Done.")
        # A lone surrogate reaches the same check as in a JSON answer, with the same outcome.
        raw = b'data: {"choices": [{"index": 0, "delta": {"content": "x\\ud800"}}]}\n\n' + stream(chunk(finish="stop"))
        SSEUpstream.script = [raw]
        with self.assertRaises(ValueError) as streamed:
            self.adapter().complete(REQUEST)
        upstream = HTTPServer(("127.0.0.1", 0), FakeUpstream)
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        self.addCleanup(upstream.server_close)
        self.addCleanup(upstream.shutdown)
        FakeUpstream.reply = (200, b'{"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "x\\ud800"}}]}', {})
        with self.assertRaises(ValueError) as plain:
            adapters.OpenAIAdapter("http://127.0.0.1:%d/v1/chat/completions" % upstream.server_address[1], "m", 30).complete(REQUEST)
        self.assertEqual(type(streamed.exception), type(plain.exception))
        self.assertNotIsInstance(streamed.exception, adapters.DegenerateCompletion)

    def test_truncation_is_refused_except_for_benchmark_probes(self):
        SSEUpstream.script = [stream(chunk(content="1 2 3", role="assistant"), chunk(finish="length", usage={"prompt_tokens": 5, "completion_tokens": 3}))]
        with self.assertRaisesRegex(ValueError, "truncated"):
            self.adapter().complete(REQUEST)
        result = self.adapter().complete(REQUEST, probe=True)
        self.assertEqual((result.message["content"], result.completion_tokens), ("1 2 3", 3))

    def test_legitimate_code_with_long_separators_is_not_flagged(self):
        code = ("// " + "-" * 300 + "\n" + "=" * 1000 + "\n# " + "#" * 400 + "\n/" + "*" * 600 + "/\n"
                + "x = 1  " + " " * 3000 + "\n" + "\n" * 600 + "    " * 300 + "return 0\n"
                + "-" * 1023 + "\n" + "- " * 500 + "\n" + "| --- " * 150 + "|\n" + "0, " * 160 + "\n"
                + "ZERO = \"" + "A" * 256 + "\"\n" + "MASK = 0x" + "ff" * 128 + "\n" + "# " + "─" * 300 + "\n"
                + "DATA = [" + "0," * 255 + "0]\n" + "|---" * 128 + "|\n")
        arguments = json.dumps({"path": "src/" + "a" * 200 + ".c"})
        events = [chunk(content=code[i:i + 7], role="assistant" if not i else None) for i in range(0, len(code), 7)]
        events += [chunk(calls=[{"index": 0, "id": "c", "type": "function", "function": {"name": "read_file", "arguments": arguments}}]), chunk(finish="tool_calls")]
        SSEUpstream.script = [stream(*events)]
        result = self.adapter().complete(REQUEST)
        self.assertEqual(result.message["content"], code)
        self.assertEqual(result.degenerate_retries, 0)

    def test_guard_thresholds(self):
        guard = adapters.DegenerateGuard
        flagged = ["?" * 256, "ok " + "?" * 256, "G" * 512, "a" * 512, "0" * 512, "ab" * 512, "abc" * 342, "¿?" * 512,
                   "-" * 1024, "=" * 1024, "|" * 1024, "─" * 1024, "- " * 512, "|---" * 256, "0," * 512,
                   "ab\n" + "xy" * 512]
        clean = ["?" * 255, "G" * 511, "-" * 1023, "=" * 1023, "- " * 511, " " * 5000, "\n" * 5000, "\t" * 5000,
                 ("?" * 200 + "\n") * 10, ("ab" * 600 + "\\n") * 2, "abcde" * 300, "the same words " * 100,
                 # One-line data and drawings the audit found: base64 of 192 zero
                 # bytes, hex of zeros and ff, a written-out "a" * 256, a minified
                 # list of 256 zeros, box-drawing and table rules.
                 "\"" + "A" * 256 + "\"", "0" * 256, "ff" * 128, "\"" + "a" * 256 + "\"", "[" + "0," * 255 + "0]",
                 "─" * 256, "═" * 1023, "│║┄┈" * 200, "|---" * 128, "ab" * 511]
        for text in flagged:
            with self.subTest(flagged=text[:20]):
                self.assertTrue(guard().feed("content", text))
        for text in clean:
            with self.subTest(clean=text[:20]):
                self.assertFalse(guard().feed("content", text))
        # Streamed one character at a time, the same text trips at the same point.
        incremental = guard()
        self.assertFalse(any(incremental.feed("content", "?") for _ in range(255)))
        self.assertTrue(incremental.feed("content", "?"))
        # Channels are separate: two calls' arguments never join into one run.
        split = guard()
        self.assertFalse(split.feed(("arguments", 0), "?" * 200) or split.feed(("arguments", 1), "?" * 200))


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
                       {"AI_SDLC_MODEL_UPSTREAM_SECONDS": "0"}, {"AI_SDLC_MODEL_UPSTREAM_SECONDS": "1"},
                       {"AI_SDLC_MODEL_UPSTREAM_SECONDS": "29"}, {"AI_SDLC_MODEL_UPSTREAM_SECONDS": "571"},
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

    def __init__(self, clock, prefill_rate=500.0, generation_rate=30.0, tool=True, seconds=300, latency=0.0):
        super().__init__("", "fake-model", seconds)
        self.clock, self.prefill_rate, self.generation_rate, self.tool = clock, prefill_rate, generation_rate, tool
        self.latency = latency

    def backend(self, request, messages, tools, names, tokens, seconds):
        prompt = max(1, len(json.dumps(messages)) // 4)
        completion = min(tokens, 150)
        self.clock[0] += self.latency + prompt / self.prefill_rate + completion / self.generation_rate
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

    def test_fixed_latency_is_measured_once_not_extrapolated(self):
        # A wrapper with 6 s fixed latency per call: the probes' latency must
        # not be multiplied into the 16k-token planning envelope.
        plain = model_benchmark.benchmark(ScriptedAdapter(self.clock), 300)
        slow_start = model_benchmark.benchmark(ScriptedAdapter(self.clock, latency=3.0), 300)
        self.assertAlmostEqual(slow_start["latencySeconds"], 3.0, delta=0.5)
        self.assertAlmostEqual(slow_start["promptTokensPerSecond"], plain["promptTokensPerSecond"], delta=60)
        for profile in (plain, slow_start):
            # Never below the measured unpadded estimate; never above the
            # former derivation (safety applied to the latency-inflated rates).
            self.assertGreaterEqual(profile["timeoutSeconds"], profile["unpaddedSeconds"] + 5)
            self.assertLessEqual(profile["timeoutSeconds"], 1.5 * profile["unpaddedSeconds"] + 5 + 1)
        self.assertLess(slow_start["timeoutSeconds"], 1.5 * slow_start["unpaddedSeconds"] + 5 - 1.5 * 3 * 2)

    def test_queued_latency_probes_are_bounded_by_the_tool_probe(self):
        class QueuedLatency(ScriptedAdapter):
            # Both minimal calls wait behind another gateway; nothing else does.
            def backend(self, request, messages, tools, names, tokens, seconds):
                if tokens == 1 and "Run " in json.dumps(messages) and "Line 1:" not in json.dumps(messages):
                    self.clock[0] += 30.0
                return super().backend(request, messages, tools, names, tokens, seconds)
        plain = model_benchmark.benchmark(ScriptedAdapter(self.clock), 300)
        queued = model_benchmark.benchmark(QueuedLatency(self.clock), 300)
        self.assertLessEqual(queued["latencySeconds"], queued["toolCallSeconds"])
        self.assertGreaterEqual(queued["timeoutSeconds"], queued["unpaddedSeconds"] + 5)

    def test_latency_can_at_most_double_a_rate(self):
        class NoisyLatency(ScriptedAdapter):
            # The latency probes are slow, the real probes are not.
            def backend(self, request, messages, tools, names, tokens, seconds):
                if tokens == 1 and "single word OK" in json.dumps(messages) and "Line 1:" not in json.dumps(messages):
                    self.clock[0] += 60.0
                return super().backend(request, messages, tools, names, tokens, seconds)
        plain = model_benchmark.benchmark(ScriptedAdapter(self.clock), 300)
        noisy = model_benchmark.benchmark(NoisyLatency(self.clock), 300)
        self.assertLessEqual(noisy["promptTokensPerSecond"], 2 * plain["promptTokensPerSecond"] + 1)
        self.assertLessEqual(noisy["completionTokensPerSecond"], 2 * plain["completionTokensPerSecond"] + 1)
        self.assertGreaterEqual(noisy["timeoutSeconds"], noisy["unpaddedSeconds"] + 5)

    def test_a_short_generation_probe_cannot_inflate_the_decoding_rate(self):
        class ShortGeneration(ScriptedAdapter):
            # Generation finishes almost instantly after a 1 s fixed latency.
            def backend(self, request, messages, tools, names, tokens, seconds):
                result = super().backend(request, messages, tools, names, tokens, seconds)
                if tokens == 192:
                    self.clock[0] -= result.completion_tokens / self.generation_rate - 0.1
                return result
        plain = model_benchmark.benchmark(ScriptedAdapter(self.clock, latency=1.0), 300)
        short = model_benchmark.benchmark(ShortGeneration(self.clock, latency=1.0), 300)
        self.assertGreaterEqual(short["timeoutSeconds"], short["unpaddedSeconds"] + 5)
        # 150 tokens decoded in ~0.1 s raw (net of a 0.004 s prompt share) is
        # ~1500 tok/s; latency subtraction may at most double that.
        self.assertLessEqual(short["completionTokensPerSecond"], 2 * 150 / 0.09)

    def test_fast_models_get_the_floor_and_slow_or_toolless_models_fail_closed(self):
        self.assertEqual(model_benchmark.benchmark(ScriptedAdapter(self.clock, 50000, 5000), 300)["timeoutSeconds"], 30)
        with self.assertRaisesRegex(model_benchmark.BenchmarkFailed, "too slow"):
            model_benchmark.benchmark(ScriptedAdapter(self.clock, 100, 5), 300)
        with self.assertRaisesRegex(model_benchmark.BenchmarkFailed, "tool call"):
            model_benchmark.benchmark(ScriptedAdapter(self.clock, tool=False), 300)

    def test_zero_reported_prompt_tokens_fail_with_a_diagnostic(self):
        class ZeroPrompt(ScriptedAdapter):
            def backend(self, request, messages, tools, names, tokens, seconds):
                result = super().backend(request, messages, tools, names, tokens, seconds)
                result.prompt_tokens = 0
                return result
        with self.assertRaisesRegex(model_benchmark.BenchmarkFailed, "unmeasurable"):
            model_benchmark.benchmark(ZeroPrompt(self.clock), 300)

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
        # The scripted profile (~259 s of a 300 s ceiling) triggers the recheck;
        # its randomized pause must not slow the suite down.
        sleep = unittest.mock.patch.object(model_gateway.time, "sleep")
        self.sleep = sleep.start()
        self.addCleanup(sleep.stop)

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

    def call_with_header(self, seconds):
        request = urllib.request.Request(self.base + "/v1/chat/completions", data=json.dumps(REQUEST).encode(),
                                         headers={"Authorization": "Bearer " + TOKEN, model_gateway.CALL_SECONDS_HEADER: seconds})
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status, json.loads(response.read())

    def test_not_ready_and_no_completions_until_benchmarked_then_profile_is_served(self):
        self.assertEqual(self.call("/readyz"), (503, {"ready": False}))
        self.assertEqual(self.call("/v1/chat/completions", REQUEST)[0], 503)
        # Unauthenticated callers learn nothing and never take the model slot.
        self.assertEqual(self.call("/v1/chat/completions", REQUEST, token="x" * 40)[0], 401)
        self.assertTrue(self.server.model_slot.acquire(blocking=False))
        self.server.model_slot.release()
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

    def test_completions_run_to_the_broker_wait_capped_by_the_ceiling(self):
        original = model_benchmark.time.monotonic
        model_benchmark.time.monotonic = lambda: self.clock[0]
        try:
            profile = model_gateway.run_benchmark(self.server, 16384, 4096, attempts=1)
        finally:
            model_benchmark.time.monotonic = original
        seen = []
        real = self.adapter.complete
        def recording(request, seconds=None, probe=False):
            seen.append(seconds)
            return real(request, seconds, probe)
        self.adapter.complete = recording
        # Without the broker's header, the benchmarked timeout applies (older brokers).
        self.assertEqual(self.call("/v1/chat/completions", REQUEST)[0], 200)
        self.assertEqual(seen, [profile["timeoutSeconds"]])
        # With it, the call may run to the broker's remaining wait, capped by the ceiling.
        for header, expected in (("120", 120), (str(self.adapter.seconds + 100), self.adapter.seconds), ("0", profile["timeoutSeconds"]), ("x", profile["timeoutSeconds"]),
                                 ("601", profile["timeoutSeconds"]), ("\u00b2", profile["timeoutSeconds"])):
            seen.clear()
            status, _ = self.call_with_header(header)
            self.assertEqual((status, seen), (200, [expected]))

    def test_each_completion_logs_one_bounded_event_without_content(self):
        original = model_benchmark.time.monotonic
        model_benchmark.time.monotonic = lambda: self.clock[0]
        try:
            model_gateway.run_benchmark(self.server, 16384, 4096, attempts=1)
        finally:
            model_benchmark.time.monotonic = original
        import contextlib, io
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(self.call("/v1/chat/completions", REQUEST)[0], 200)
            real = self.adapter.complete
            def failing(request, seconds=None, probe=False):
                raise ValueError("truncated completion with secret-fixture text")
            self.adapter.complete = failing
            # The response mapping is unchanged: ValueError 400, others 502.
            self.assertEqual(self.call("/v1/chat/completions", REQUEST)[0], 400)
            def deadline(request, seconds=None, probe=False):
                raise RuntimeError("completion deadline exceeded")
            self.adapter.complete = deadline
            self.assertEqual(self.call("/v1/chat/completions", REQUEST)[0], 502)
            self.adapter.complete = real
        events = [json.loads(line) for line in out.getvalue().splitlines() if '"model-call"' in line]
        self.assertEqual([e["outcome"] for e in events], ["ok", "failed", "failed"])
        self.assertIn("promptTokens", events[0])
        self.assertIn("estimated", events[0])
        self.assertNotIn("reason", events[0])
        self.assertEqual([events[1]["reason"], events[2]["reason"]], ["truncated", "deadline"])
        self.assertNotIn("secret-fixture", out.getvalue())
        self.assertEqual(model_gateway.failure_class(RuntimeError("completion deadline exceeded")), "deadline")
        self.assertEqual(model_gateway.failure_class(ValueError("invalid assistant tool call")), "invalid")
        self.assertEqual(model_gateway.failure_class(RuntimeError("anything else")), "other")

    def test_degenerate_retries_are_logged_with_the_bounded_reason(self):
        original = model_benchmark.time.monotonic
        model_benchmark.time.monotonic = lambda: self.clock[0]
        try:
            model_gateway.run_benchmark(self.server, 16384, 4096, attempts=1)
        finally:
            model_benchmark.time.monotonic = original
        import contextlib, io
        real = self.adapter.complete
        def retried(request, seconds=None, probe=False):
            completion = real(request, seconds, probe)
            completion.degenerate_retries = 1
            return completion
        def degenerate(retries):
            def fail(request, seconds=None, probe=False):
                raise adapters.DegenerateCompletion(retries)
            return fail
        def deadline_after_retry(request, seconds=None, probe=False):
            error = RuntimeError("completion deadline exceeded")
            error.degenerate_retries = 1
            raise error
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            for complete, status in ((real, 200), (retried, 200), (degenerate(1), 400), (degenerate(0), 400), (deadline_after_retry, 502)):
                self.adapter.complete = complete
                self.assertEqual(self.call("/v1/chat/completions", REQUEST)[0], status)
        self.adapter.complete = real
        events = [json.loads(line) for line in out.getvalue().splitlines() if '"model-call"' in line]
        self.assertEqual([(e["outcome"], e.get("reason"), e.get("degenerateRetries")) for e in events],
                         [("ok", None, None), ("ok", None, 1), ("failed", "degenerate", 1), ("failed", "degenerate", 0),
                          ("failed", "deadline", 1)])

    def test_failed_benchmark_never_becomes_ready(self):
        self.adapter.tool = False
        self.assertIsNone(model_gateway.run_benchmark(self.server, 16384, 4096, attempts=2, pause=0))
        self.assertEqual(self.call("/readyz")[0], 503)

    def test_production_backoff_is_randomized_between_attempts_only(self):
        self.adapter.tool = False
        with unittest.mock.patch.object(model_gateway.random, "uniform", return_value=7.5) as uniform, \
                unittest.mock.patch.object(model_gateway.time, "sleep") as sleep:
            self.assertIsNone(model_gateway.run_benchmark(self.server, 16384, 4096))
        self.assertEqual(uniform.call_count, model_gateway.ATTEMPTS - 1)
        uniform.assert_called_with(*model_gateway.RETRY_SECONDS)
        self.assertEqual([c.args for c in sleep.call_args_list], [(7.5,)] * (model_gateway.ATTEMPTS - 1))

    def scripted_profiles(self, *outcomes):
        """Replaces the benchmark with scripted outcomes: a timeout or a failure."""
        calls = []
        def fake(adapter, ceiling, prompt_tokens, completion_tokens):
            calls.append(ceiling)
            outcome = outcomes[len(calls) - 1]
            if isinstance(outcome, Exception):
                raise outcome
            return {"timeoutSeconds": outcome}
        patcher = unittest.mock.patch.object(model_gateway, "benchmark", side_effect=fake)
        patcher.start()
        self.addCleanup(patcher.stop)
        return calls

    def test_first_attempt_waits_a_bounded_random_stagger(self):
        calls = self.scripted_profiles(100)
        with unittest.mock.patch.object(model_gateway.random, "uniform", return_value=12.5) as uniform:
            profile = model_gateway.run_benchmark(self.server, 16384, 4096, stagger=30)
        self.assertEqual(profile, {"timeoutSeconds": 100})
        uniform.assert_called_once_with(0, 30)
        self.assertEqual([c.args for c in self.sleep.call_args_list], [(12.5,)])
        self.assertEqual(len(calls), 1)

    def test_no_stagger_by_default_or_when_disabled(self):
        self.scripted_profiles(100, 100)
        with unittest.mock.patch.object(model_gateway.random, "uniform") as uniform:
            model_gateway.run_benchmark(self.server, 16384, 4096)
            model_gateway.run_benchmark(self.server, 16384, 4096, stagger=0)
        uniform.assert_not_called()
        self.sleep.assert_not_called()

    def test_stagger_setting_is_bounded(self):
        self.assertEqual(model_gateway.stagger_seconds({}), 30)
        self.assertEqual(model_gateway.stagger_seconds({model_gateway.STAGGER_ENV: "0"}), 0)
        self.assertEqual(model_gateway.stagger_seconds({model_gateway.STAGGER_ENV: "120"}), 120)
        for bad in ("121", "-1", "", "x", "1.5", "\u00b2"):
            with self.subTest(bad=bad), self.assertRaises(RuntimeError):
                model_gateway.stagger_seconds({model_gateway.STAGGER_ENV: bad})

    def test_profile_near_the_ceiling_is_measured_once_more_and_the_faster_kept(self):
        # Ceiling 300 s: 250 s exceeds 0.8 x 300; a concurrent benchmark may have inflated it.
        calls = self.scripted_profiles(250, 120)
        with unittest.mock.patch.object(model_gateway.random, "uniform", return_value=9.0):
            self.assertEqual(model_gateway.run_benchmark(self.server, 16384, 4096), {"timeoutSeconds": 120})
        self.assertEqual(len(calls), 2)
        self.assertEqual([c.args for c in self.sleep.call_args_list], [(9.0,)])
        self.assertEqual(self.call("/v1/gateway/profile"), (200, {"timeoutSeconds": 120}))

    def test_recheck_keeps_the_first_profile_when_not_faster_or_failed(self):
        calls = self.scripted_profiles(250, 260, 250, model_benchmark.BenchmarkFailed("model too slow for the installation ceiling"))
        for _ in range(2):
            self.assertEqual(model_gateway.run_benchmark(self.server, 16384, 4096, pause=0), {"timeoutSeconds": 250})
        self.assertEqual(len(calls), 4)

    def test_profile_well_within_the_ceiling_is_not_rechecked(self):
        calls = self.scripted_profiles(240)
        self.assertEqual(model_gateway.run_benchmark(self.server, 16384, 4096, pause=0), {"timeoutSeconds": 240})
        self.assertEqual(len(calls), 1)

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
