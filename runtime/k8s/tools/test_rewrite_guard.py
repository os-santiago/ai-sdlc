"""The rewrite guard refuses a whole-file write_file over a pre-existing file
once per call, judged only from the request's own conversation."""
import contextlib
import io
import json
import os
import threading
import time
import unittest
import unittest.mock
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer

import model_adapters as adapters
import model_gateway
# Helpers only: importing the module (not its classes) keeps its tests from
# being collected twice.
import test_model_gateway as base


def tool(name, *keys):
    return {"type": "function", "function": {"name": name, "description": name,
            "parameters": {"type": "object", "properties": {key: {"type": "string"} for key in keys}}}}


TOOLS = [tool("read_file", "path"), tool("edit_file", "path", "patch"), tool("write_file", "path", "content"),
         tool("list_dir", "path")]
EXISTING = "textutil/textutil_test.go"
REWRITE = adapters.Refusal("rewrite", EXISTING, None)
TEST_SOURCE = "package textutil\n\nfunc TestTrim(t *testing.T) {}\n"


def assistant(*calls):
    return {"role": "assistant", "content": "", "tool_calls": [
        {"id": call_id, "type": "function", "function": {"name": name, "arguments": json.dumps(arguments)}}
        for call_id, name, arguments in calls]}


def result(call_id, content):
    return {"role": "tool", "tool_call_id": call_id, "content": content}


def conversation(*turns):
    messages = [{"role": "system", "content": "Implement the issue."}, {"role": "user", "content": "Add tests for Trim."}]
    return {"model": "codex-cli", "messages": messages + list(turns), "tools": TOOLS}


# The battery 27 shape: the model read the large existing test file, then
# proposed write_file over it.
READ_EXISTING = conversation(assistant(("r1", "read_file", {"path": "./" + EXISTING})), result("r1", TEST_SOURCE))


def correction(path):
    return {"role": "user", "content": adapters.correction_text(path)}


def write_deltas(path, chunks=1, delay=0.0, call_id="w"):
    """A streamed write_file: the name, then the path, then the content in
    ``chunks`` pieces (a whole-file rewrite takes minutes on a local model)."""
    yield base.sse_event(base.chunk(role="assistant", calls=[{"index": 0, "id": call_id, "type": "function",
                                                             "function": {"name": "write_file", "arguments": ""}}]))
    yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "{\"path\": " + json.dumps(path) + ", "}}]))
    yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "\"content\": \""}}]))
    for _ in range(chunks):
        yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "func TestX(t *testing.T) {}\\n"}}]))
        if delay:
            time.sleep(delay)
    yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "\"}"}}]))
    yield base.sse_event(base.chunk(finish="tool_calls"))
    yield base.sse_event(b"[DONE]")


def rewrite(path=EXISTING, chunks=1, delay=0.0):
    return lambda _body: write_deltas(path, chunks, delay)


def slow_rewrite(_body):
    # About 20 s if read to the end; the guard must stop it at the path.
    return write_deltas(EXISTING, chunks=20000, delay=0.001)


def single_call(name, arguments):
    return base.stream(base.chunk(calls=[{"index": 0, "id": "c", "type": "function",
                                          "function": {"name": name, "arguments": json.dumps(arguments)}}]),
                       base.chunk(finish="tool_calls"))


EDIT = single_call("edit_file", {"path": EXISTING, "patch": "--- a/x\n+++ b/x\n@@ -1,1 +1,2 @@\n a\n+b\n"})
NEW_FILE = single_call("write_file", {"path": "textutil/trim_test.go", "content": TEST_SOURCE})


class GuardRules(unittest.TestCase):
    """Which paths count as pre-existing, from the conversation alone."""

    def existing(self, request):
        messages = adapters.validate_request(request)[0]
        return adapters.RewriteGuard.preexisting(messages)

    def test_path_normalization(self):
        for raw, normalized in (("./a/b.go", "a/b.go"), ("a//b.go", "a/b.go"), ("a/./b.go", "a/b.go"),
                                ("././a.go", "a.go"), ("a/b/", "a/b"), ("/abs//x.go", "/abs/x.go"),
                                ("a/../b.go", "a/../b.go")):
            with self.subTest(raw=raw):
                self.assertEqual(adapters.guard_path(raw), normalized)
        for bad in ("", ".", "./", "//", None, 7, "a\x00b", "a" * (adapters.MAX_GUARD_PATH_CHARS + 1)):
            with self.subTest(bad=bad):
                self.assertIsNone(adapters.guard_path(bad))

    def test_reads_and_edits_mark_files_that_existed_before_the_conversation(self):
        request = conversation(
            assistant(("r1", "read_file", {"path": "./a//one.go"}), ("e1", "edit_file", {"path": "two.go", "patch": "p"})),
            result("r1", "package a"), result("e1", "Patch applied successfully: two.go"))
        self.assertEqual(self.existing(request), {"a/one.go", "two.go"})

    def test_files_the_model_created_are_never_pre_existing(self):
        request = conversation(
            assistant(("w1", "write_file", {"path": "new_test.go", "content": "x"})), result("w1", "File written successfully"),
            assistant(("r1", "read_file", {"path": "./new_test.go"}), ("e1", "edit_file", {"path": "new_test.go", "patch": "p"})),
            result("r1", "x"), result("e1", "Patch applied successfully"))
        self.assertEqual(self.existing(request), frozenset())

    def test_failed_tool_results_prove_existence_only_after_the_file_was_opened(self):
        request = conversation(
            assistant(("r1", "read_file", {"path": "missing.go"}), ("r2", "read_file", {"path": "big.go"}),
                      ("e1", "edit_file", {"path": "stale.go", "patch": "p"}), ("e2", "edit_file", {"path": "bad.go", "patch": "p"}),
                      ("r3", "read_file", {"path": "unanswered.go"})),
            result("r1", "[ERROR] [ℹ️ Expected Error] read_file: ENOENT: no such file or directory"),
            result("r2", "[ERROR] [⚠️ Error] read_file: File too large (2.0 MB). Maximum read size: 1.0 MB."),
            result("e1", "[ERROR] [⚠️ Error] edit_file: Patch does not match file content in stale.go."),
            result("e2", "Error: Failed to apply patch: invalid unified diff format."))
        self.assertEqual(self.existing(request), {"big.go", "stale.go"})

    def test_malformed_history_is_ignored(self):
        request = conversation({"role": "assistant", "content": "", "tool_calls": [
            "x", {"id": "a"}, {"id": "b", "function": {"name": "read_file", "arguments": "{bad"}},
            {"id": "c", "function": {"name": "read_file", "arguments": "[]"}},
            {"id": "d", "function": {"name": 7, "arguments": "{\"path\": \"x\"}"}}]}, result("b", "ok"))
        self.assertEqual(self.existing(request), frozenset())

    def test_streamed_path_is_read_once_its_value_is_complete(self):
        guard = adapters.RewriteGuard(READ_EXISTING, adapters.validate_request(READ_EXISTING)[0], time.monotonic() + 60)
        self.assertIsNone(guard.streamed(0, "write_file", "{\"path\": \"textutil/textutil_te"))
        self.assertIsNone(guard.streamed(0, "write_file", "{\"path\": \"textutil/textutil_test.go\""))
        self.assertIsNone(guard.streamed(1, "edit_file", "{\"path\": \"textutil/textutil_test.go\", "))
        self.assertEqual(guard.streamed(0, "write_file", "{\"path\": \"textutil/textutil_test.go\", \"con"), REWRITE)
        self.assertIsNone(guard.streamed(0, "write_file", "{\"path\": \"textutil/textutil_test.go\", \"content\": \"x"))
        # Complete scalar keys may come first; escapes are decoded.
        self.assertEqual(guard.streamed(2, "write_file", "{\"mode\": 1, \"path\": \"textutil\\/textutil_test.go\"}"), REWRITE)
        self.assertEqual(guard.streamed(3, "write_file", "{\"content\": \"x\", \"path\": \"textutil/textutil_test.go\"}"), REWRITE)
        self.assertIsNone(guard.streamed(4, "write_file", "{\"path\": \"textutil/other.go\", "))
        # A path not within the first scanned characters is left to the final message.
        long_first = "{\"content\": \"" + "x" * adapters.REWRITE_SCAN_CHARS
        self.assertIsNone(guard.streamed(5, "write_file", long_first))
        self.assertIsNone(guard.streamed(5, "write_file", long_first + "\", \"path\": \"textutil/textutil_test.go\"}"))


class StreamingRewriteTests(unittest.TestCase):
    def setUp(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), base.SSEUpstream)
        self.server.daemon_threads = True
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = "http://127.0.0.1:%d/v1/chat/completions" % self.server.server_address[1]
        base.SSEUpstream.seen, base.SSEUpstream.disconnected = [], []

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def adapter(self, seconds=30, guard=True):
        return adapters.OpenAIAdapter(self.url, "qwen3-coder", seconds, rewrite_guard=guard)

    def names(self, completion):
        return [call["function"]["name"] for call in completion.message.get("tool_calls", [])]

    def wait_disconnected(self, count):
        deadline = time.monotonic() + 3
        while len(base.SSEUpstream.disconnected) < count and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertEqual(len(base.SSEUpstream.disconnected), count)

    def test_rewrite_is_stopped_at_its_path_and_retried_once_with_the_correction(self):
        base.SSEUpstream.script = [slow_rewrite, EDIT]
        started = time.monotonic()
        completion = self.adapter().complete(READ_EXISTING)
        self.assertLess(time.monotonic() - started, 5)  # the content was never waited for
        self.wait_disconnected(1)
        # The retry complies: its edit_file is returned.
        self.assertEqual(self.names(completion), ["edit_file"])
        self.assertEqual((completion.rewrite_retries, completion.rewrite_passed, completion.degenerate_retries), (1, 0, 0))
        first, second = base.SSEUpstream.seen
        self.assertEqual(second["messages"], first["messages"] + [correction(EXISTING)])
        self.assertEqual({k: v for k, v in second.items() if k != "messages"}, {k: v for k, v in first.items() if k != "messages"})

    def test_a_retry_that_rewrites_again_is_passed_through_unchanged(self):
        base.SSEUpstream.script = [rewrite(), rewrite("./" + EXISTING, chunks=3)]
        completion = self.adapter().complete(READ_EXISTING)
        self.assertEqual(len(base.SSEUpstream.seen), 2)
        self.assertEqual((completion.rewrite_retries, completion.rewrite_passed), (1, 1))
        arguments = json.loads(completion.message["tool_calls"][0]["function"]["arguments"])
        self.assertEqual(arguments["path"], "./" + EXISTING)
        self.assertEqual(arguments["content"], "func TestX(t *testing.T) {}\n" * 3)

    def test_new_and_self_created_files_and_edits_are_untouched(self):
        created = conversation(assistant(("w1", "write_file", {"path": "textutil/trim_test.go", "content": "x"})),
                               result("w1", "File written successfully: textutil/trim_test.go"),
                               assistant(("r1", "read_file", {"path": "textutil/trim_test.go"})), result("r1", "x"))
        for request, script in ((READ_EXISTING, NEW_FILE), (created, NEW_FILE), (READ_EXISTING, EDIT),
                                (conversation(), rewrite())):
            with self.subTest(script=script if isinstance(script, bytes) else "rewrite", messages=len(request["messages"])):
                base.SSEUpstream.seen, base.SSEUpstream.script = [], [script]
                completion = self.adapter().complete(request)
                self.assertEqual(len(base.SSEUpstream.seen), 1)
                self.assertEqual((completion.rewrite_retries, completion.rewrite_passed), (0, 0))

    def test_disabled_guard_and_benchmark_probes_never_refuse(self):
        base.SSEUpstream.script = [rewrite()]
        completion = self.adapter(guard=False).complete(READ_EXISTING)
        self.assertEqual((self.names(completion), len(base.SSEUpstream.seen)), (["write_file"], 1))
        self.assertEqual((completion.rewrite_retries, completion.rewrite_passed), (0, 0))
        base.SSEUpstream.seen = []
        self.adapter().complete(READ_EXISTING, probe=True)
        self.assertEqual(len(base.SSEUpstream.seen), 1)
        # Without write_file among the offered tools (the review gateway) it is
        # inert: the proposal is refused as unknown, with no retry first.
        base.SSEUpstream.seen = []
        with self.assertRaisesRegex(ValueError, "unknown proposed tool"):
            self.adapter().complete(dict(READ_EXISTING, tools=TOOLS[:2] + TOOLS[3:]))
        self.assertEqual(len(base.SSEUpstream.seen), 1)

    def test_a_path_after_the_content_is_judged_on_the_final_message(self):
        late = single_call("write_file", {"content": TEST_SOURCE * 100, "path": EXISTING})
        self.assertGreater(len(TEST_SOURCE * 100), adapters.REWRITE_SCAN_CHARS)
        base.SSEUpstream.script = [late, EDIT]
        completion = self.adapter().complete(READ_EXISTING)
        self.assertEqual((self.names(completion), completion.rewrite_retries, completion.rewrite_passed), (["edit_file"], 1, 0))
        self.assertEqual(base.SSEUpstream.disconnected, [])  # read to its end, then refused

    def test_one_deadline_and_one_degenerate_retry_are_shared_with_the_rewrite_retry(self):
        # Degenerate, then a rewrite, then degenerate again: no second degenerate retry.
        base.SSEUpstream.script = [base.degenerate_stream, rewrite(), base.degenerate_stream]
        with self.assertRaises(adapters.DegenerateCompletion) as caught:
            self.adapter().complete(READ_EXISTING)
        self.assertEqual((caught.exception.degenerate_retries, caught.exception.rewrite_retries, len(base.SSEUpstream.seen)), (1, 1, 3))
        self.assertEqual(base.SSEUpstream.seen[2]["messages"], READ_EXISTING["messages"] + [correction(EXISTING)])
        # Too little budget for a retry: the rewrite streams on and is passed through.
        base.SSEUpstream.seen, base.SSEUpstream.script = [], [rewrite(chunks=5)]
        completion = self.adapter(seconds=5).complete(READ_EXISTING)
        self.assertEqual((len(base.SSEUpstream.seen), completion.rewrite_retries, completion.rewrite_passed), (1, 0, 1))
        # A retry that stalls ends at the original deadline, not a fresh one.
        def stalled(_body):
            yield base.sse_event(base.chunk(content="Reading", role="assistant"))
            for _ in range(100):
                yield b": ping\n\n"
                time.sleep(0.1)
        base.SSEUpstream.seen, base.SSEUpstream.script = [], [rewrite(), stalled]
        started = time.monotonic()
        with unittest.mock.patch.object(adapters, "REWRITE_RETRY_MIN_SECONDS", 1), \
                self.assertRaisesRegex(RuntimeError, "deadline") as caught:
            self.adapter(seconds=2).complete(READ_EXISTING)
        self.assertLess(time.monotonic() - started, 3)
        self.assertEqual((caught.exception.rewrite_retries, len(base.SSEUpstream.seen)), (1, 2))

    def test_a_failed_retry_after_an_early_stop_fails_the_call(self):
        # The stopped stream left no complete answer to fall back to.
        failed = base.stream(base.chunk(content="x"), {"error": {"code": 500, "message": "boom"}}, done=False)
        base.SSEUpstream.script = [slow_rewrite, failed]
        with self.assertRaisesRegex(RuntimeError, "completion backend failed") as caught:
            self.adapter().complete(READ_EXISTING)
        self.assertEqual((caught.exception.rewrite_retries, len(base.SSEUpstream.seen)), (1, 2))

    def test_odd_history_or_a_failing_guard_never_fails_the_call(self):
        odd = conversation({"role": "assistant", "content": "", "tool_calls": [
            {"id": ["r1"], "type": "function", "function": {"name": "read_file", "arguments": json.dumps({"path": EXISTING})}},
            {"id": {"r": 2}, "type": "function", "function": {"name": "edit_file", "arguments": json.dumps({"path": EXISTING})}}]},
            result("r1", TEST_SOURCE))
        self.assertEqual(adapters.RewriteGuard.preexisting(adapters.validate_request(odd)[0]), frozenset())
        base.SSEUpstream.script = [rewrite()]
        completion = self.adapter().complete(odd)
        self.assertEqual((completion.rewrite_retries, completion.rewrite_passed, len(base.SSEUpstream.seen)), (0, 0, 1))
        # A guard step that raises is disabled; a failing stream check still
        # leaves the final-message check, which here retries.
        for targets, expected in ((("preexisting",), (0, 1)), (("streamed", "refused"), (0, 1)), (("streamed",), (1, 2))):
            with self.subTest(targets=targets), contextlib.ExitStack() as stack:
                for target in targets:
                    stack.enter_context(unittest.mock.patch.object(adapters.RewriteGuard, target, side_effect=TypeError("boom")))
                base.SSEUpstream.seen, base.SSEUpstream.script = [], [rewrite()]
                completion = self.adapter().complete(READ_EXISTING)
                self.assertEqual(self.names(completion), ["write_file"])
                self.assertEqual((completion.rewrite_retries, len(base.SSEUpstream.seen)), expected)

    def test_a_correction_that_would_leave_the_contract_is_not_sent(self):
        # 256 messages is the contract's limit: no room for the correction.
        full = dict(READ_EXISTING, messages=READ_EXISTING["messages"] + [{"role": "user", "content": "more"}] * (256 - len(READ_EXISTING["messages"])))
        base.SSEUpstream.script = [rewrite()]
        completion = self.adapter().complete(full)
        self.assertEqual((len(base.SSEUpstream.seen), completion.rewrite_retries, completion.rewrite_passed), (1, 0, 1))

    def test_the_correction_bounds_the_path_text(self):
        long_path = "d/" * 300 + "f.go"
        request = conversation(assistant(("r1", "read_file", {"path": long_path})), result("r1", "x"))
        base.SSEUpstream.script = [rewrite(long_path), EDIT]
        self.adapter().complete(request)
        text = base.SSEUpstream.seen[1]["messages"][-1]["content"]
        self.assertIn(long_path[:adapters.MAX_CORRECTION_PATH_CHARS] + "...", text)
        self.assertNotIn(long_path, text)


class NonStreamingRewriteTests(base.UpstreamCase):
    def test_anthropic_final_message_is_refused_and_retried_once(self):
        replies = [{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "write_file",
                                                     "input": {"path": EXISTING, "content": "x"}}], "stop_reason": "tool_use"},
                   {"role": "assistant", "content": [{"type": "tool_use", "id": "t2", "name": "edit_file",
                                                     "input": {"path": EXISTING, "patch": "p"}}], "stop_reason": "tool_use"}]
        base.FakeUpstream.reply = lambda _body: (200, replies[min(len(base.FakeUpstream.seen), 2) - 1], {})
        adapter = adapters.AnthropicAdapter(self.base + "/v1/messages", "claude-test", 30, "k")
        completion = adapter.complete(READ_EXISTING)
        self.assertEqual(completion.message["tool_calls"][0]["function"]["name"], "edit_file")
        self.assertEqual((completion.rewrite_retries, completion.rewrite_passed, len(base.FakeUpstream.seen)), (1, 0, 2))
        last = base.FakeUpstream.seen[1][2]["messages"][-1]
        self.assertEqual(last["content"][-1], {"type": "text", "text": adapters.correction_text(EXISTING)})

    def test_openai_json_answer_rewriting_twice_is_passed_through(self):
        call = [{"id": "w", "type": "function", "function": {"name": "write_file",
                 "arguments": json.dumps({"path": EXISTING, "content": "x"})}}]
        base.FakeUpstream.reply = (200, base.openai_reply(calls=call), {})
        completion = adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "m", 30).complete(READ_EXISTING)
        self.assertEqual((completion.rewrite_retries, completion.rewrite_passed, len(base.FakeUpstream.seen)), (1, 1, 2))
        self.assertEqual(base.FakeUpstream.seen[1][2]["messages"][-1], correction(EXISTING))

    def test_agent_cli_ignores_the_deadline_so_its_rewrite_is_passed_through(self):
        seen = []
        def complete(request):
            seen.append(request)
            return {"role": "assistant", "content": "", "tool_calls": [
                {"id": "c", "type": "function", "function": {"name": "write_file", "arguments": json.dumps({"path": EXISTING, "content": "x"})}}]}
        with unittest.mock.patch("codexcli_openai_wrapper.complete", complete):
            completion = adapters.AgentCLIAdapter(seconds=300).complete(READ_EXISTING)
        self.assertEqual((completion.rewrite_retries, completion.rewrite_passed, len(seen)), (0, 1, 1))

    def test_a_failed_retry_returns_the_complete_refused_answer(self):
        call = [{"id": "w", "type": "function", "function": {"name": "write_file",
                 "arguments": json.dumps({"path": EXISTING, "content": "x"})}}]
        for failure in ((500, {"error": "x"}, {}), (200, base.openai_reply(content="par") | {"choices": [
                {"index": 0, "finish_reason": "length", "message": {"role": "assistant", "content": "par"}}]}, {})):
            with self.subTest(status=failure[0]):
                base.FakeUpstream.seen = []
                base.FakeUpstream.reply = lambda _body, failure=failure: (200, base.openai_reply(calls=call), {}) \
                    if len(base.FakeUpstream.seen) == 1 else failure
                completion = adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "m", 30).complete(READ_EXISTING)
                self.assertEqual(len(base.FakeUpstream.seen), 2)
                self.assertEqual((completion.rewrite_retries, completion.rewrite_passed), (1, 1))
                self.assertEqual(json.loads(completion.message["tool_calls"][0]["function"]["arguments"])["path"], EXISTING)


class ConfigurationAndLoggingTests(unittest.TestCase):
    def test_environment_enables_by_default_and_refuses_bad_values(self):
        env = {"AI_SDLC_MODEL_UPSTREAM_URL": "http://192.168.1.10:8080/v1/chat/completions", "AI_SDLC_MODEL_UPSTREAM_MODEL": "m"}
        self.assertTrue(adapters.adapter_from_environ(env).rewrite_guard)
        self.assertTrue(adapters.adapter_from_environ(dict(env, AI_SDLC_MODEL_GATEWAY_REWRITE_GUARD="true")).rewrite_guard)
        self.assertFalse(adapters.adapter_from_environ(dict(env, AI_SDLC_MODEL_GATEWAY_REWRITE_GUARD="false")).rewrite_guard)
        self.assertFalse(adapters.adapter_from_environ({"AI_SDLC_MODEL_ADAPTER": "agent-cli",
                                                        "AI_SDLC_MODEL_GATEWAY_REWRITE_GUARD": "false"}).rewrite_guard)
        for bad in ("", "yes", "TRUE", "1"):
            with self.subTest(bad=bad), self.assertRaisesRegex(RuntimeError, "rewrite guard"):
                adapters.adapter_from_environ(dict(env, AI_SDLC_MODEL_GATEWAY_REWRITE_GUARD=bad))

    def test_gateway_event_counts_rewrite_retries_without_paths(self):
        os.environ["CODEX_WRAPPER_TOKEN"] = base.TOKEN
        self.addCleanup(os.environ.pop, "CODEX_WRAPPER_TOKEN", None)
        adapter = adapters.Adapter(seconds=30)
        outcomes = []
        def complete(request, seconds=None, probe=False):
            retries, passed, fail = outcomes.pop(0)
            if fail:
                error = RuntimeError("completion deadline exceeded")
                error.rewrite_retries = retries
                raise error
            message = {"role": "assistant", "content": "", "tool_calls": [
                {"id": "c", "type": "function", "function": {"name": "write_file", "arguments": json.dumps({"path": EXISTING, "content": "x"})}}]}
            return adapters.Completion(message, 10, 2, False, rewrite_retries=retries, rewrite_passed=passed)
        adapter.complete = complete
        server = model_gateway.GatewayServer(("127.0.0.1", 0), model_gateway.GatewayHandler, adapter)
        server.profile = {"timeoutSeconds": 30}
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            for outcome in ((0, 0, False), (1, 0, False), (1, 1, False), (1, 0, True)):
                outcomes.append(outcome)
                request = urllib.request.Request("http://127.0.0.1:%d/v1/chat/completions" % server.server_address[1],
                                                 data=json.dumps(READ_EXISTING).encode(), headers={"Authorization": "Bearer " + base.TOKEN})
                try:
                    urllib.request.urlopen(request, timeout=5).close()
                except urllib.error.HTTPError as error:
                    self.assertEqual(error.code, 502)
                    error.close()
        events = [json.loads(line) for line in out.getvalue().splitlines() if '"model-call"' in line]
        self.assertEqual([(e["outcome"], e.get("rewriteRetries"), e.get("rewritePassed")) for e in events],
                         [("ok", None, None), ("ok", 1, None), ("ok", 1, 1), ("failed", 1, None)])
        self.assertNotIn("textutil", out.getvalue())
        # Out-of-range values are never logged.
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            model_gateway.log_call("ok", time.monotonic(), 30, rewrite_retries=2, rewrite_passed=True)
        self.assertNotIn("rewrite", out.getvalue())


if __name__ == "__main__":
    unittest.main()
