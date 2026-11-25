"""The copy guard refuses, once per call, a new file whose content repeats a
file the model read in the same conversation."""
import contextlib
import io
import json
import threading
import time
import unittest
import unittest.mock
from http.server import ThreadingHTTPServer

import model_adapters as adapters
import model_gateway
# Helpers only: importing the modules (not their classes) keeps their tests
# from being collected twice.
import test_model_gateway as base
import test_rewrite_guard as rg

EXISTING = rg.EXISTING
NEW = "textutil/textutil_test_new.go"
HEADER = ["package textutil", "", "import (", '\t"strings"', '\t"testing"', ")", ""]


def table_test(name, cases):
    """One Go table test, shaped like the battery 29 file."""
    lines = ["func Test%s(t *testing.T) {" % name, "\ttests := []struct {", "\t\tname string",
             "\t\tin   string", "\t\twant string", "\t}{"]
    lines += ['\t\t{"%s %d", "%s-input-%d", "%s-output-%d"},' % (name, i, name, i, name, i) for i in range(cases)]
    lines += ["\t}", "\tfor _, tt := range tests {", "\t\tt.Run(tt.name, func(t *testing.T) {",
              "\t\t\tif got := %s(tt.in); got != tt.want {" % name,
              '\t\t\t\tt.Errorf("%s(%%q) = %%q, want %%q", tt.in, got, tt.want)' % name,
              "\t\t\t}", "\t\t})", "\t}", "}", ""]
    return lines


# The existing test file: 8 table tests, about 90 distinct non-trivial lines.
SOURCE_LINES = HEADER + sum((table_test(name, 6) for name in
                             ("ReverseASCII", "IsUpper", "CountChar", "TrimSpace", "Title", "Words", "Repeat", "Lower")), [])
SOURCE = "\n".join(SOURCE_LINES)
# A legitimate new test file: the same package, imports and table-test shape,
# but only new tests (two functions, 50 distinct non-trivial lines).
LEGIT_LINES = HEADER + table_test("Capitalize", 18) + table_test("SnakeCase", 18)


def scc_read(text, offset=1, limit=None):
    """SCC's read_file result, as src/tools/read-file.ts builds it: the raw
    lines (no line numbers) from ``offset``, then a note after a blank line."""
    lines = text.split("\n")
    sliced = lines[offset - 1:offset - 1 + limit] if limit else lines[offset - 1:]
    suffix = ("\n\n[%d lines shown, %d total. Use offset=%d&limit=%d to read more.]"
              % (len(sliced), len(lines), offset + len(sliced), limit or 50)
              if len(lines) > len(sliced) else "\n\n[%d lines total]" % len(lines))
    return "\n".join(sliced) + suffix


def scc_prune(content):
    """SCC's shortened old tool output (pruneMessageHistory in
    src/core/agent/message-history.ts): first and last 300 characters."""
    return "%s\n\n... [Tool output of %d characters truncated to save context window] ...\n\n%s" % (
        content[:300], len(content), content[-300:])


def read_result(text, shown=None, total=None):
    note = "[%d lines total]" % total if shown is None else \
        "[%d lines shown, %d total. Use offset=%d&limit=50 to read more.]" % (shown, total, shown + 1)
    return text + "\n\n" + note


READ = rg.conversation(rg.assistant(("r1", "read_file", {"path": EXISTING})),
                       rg.result("r1", read_result(SOURCE, total=len(SOURCE_LINES))))


def copy_refusal(path=NEW, source=EXISTING):
    return adapters.Refusal("copy", path, source)


def copy_correction(path=NEW, source=EXISTING):
    return {"role": "user", "content": adapters.copy_correction_text(path, source)}


def write_lines(path, lines, extra=0, delay=0.0):
    """A streamed write_file: the path, then its content one line per delta,
    then ``extra`` more new lines, ``delay`` seconds apart."""
    def deltas(_body):
        yield base.sse_event(base.chunk(role="assistant", calls=[{"index": 0, "id": "w", "type": "function",
                                                                 "function": {"name": "write_file", "arguments": ""}}]))
        yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "{\"path\": %s, \"content\": \"" % json.dumps(path)}}]))
        body = list(lines) + ["\t// new line %d" % i for i in range(extra)]
        for line in body:
            yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": json.dumps(line + "\n")[1:-1]}}]))
            if delay:
                time.sleep(delay)
        yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "\"}"}}]))
        yield base.sse_event(base.chunk(finish="tool_calls"))
        yield base.sse_event(b"[DONE]")
    return deltas


# About 20 s if read to the end; the guard must stop it soon after 40 lines.
SLOW_COPY = write_lines(NEW, SOURCE_LINES, extra=20000, delay=0.001)
COPY = write_lines(NEW, SOURCE_LINES)
LEGIT = write_lines(NEW, LEGIT_LINES)


class CopyRules(unittest.TestCase):
    def guard(self, request=READ):
        return adapters.RewriteGuard(request, adapters.validate_request(request)[0], time.monotonic() + 60)

    def message(self, lines, path=NEW):
        return {"role": "assistant", "content": "", "tool_calls": [{"id": "w", "type": "function", "function": {
            "name": "write_file", "arguments": json.dumps({"path": path, "content": "\n".join(lines)})}}]}

    def test_thresholds_separate_copies_from_legitimate_new_tests(self):
        source = adapters.copy_lines(SOURCE)
        legit = adapters.copy_lines("\n".join(LEGIT_LINES))
        shared = legit & source
        # Package, imports and table-test lines only: well below both bounds.
        self.assertGreaterEqual(len(legit), adapters.COPY_MIN_LINES)
        self.assertLess(len(shared), adapters.COPY_MIN_MATCHED)
        self.assertLess(len(shared) / len(legit), adapters.COPY_FRACTION)
        self.assertGreater(len(source), 80)
        guard = self.guard()
        self.assertIsNone(guard.refused(self.message(LEGIT_LINES)))
        self.assertEqual(guard.refused(self.message(SOURCE_LINES)), copy_refusal())
        # Trivial lines never count: braces, blank lines, short imports.
        for line in ("}", "\t})", "", '"os"', ")", "\t}{"):
            self.assertIsNone(adapters.copy_line(line))
        self.assertEqual(adapters.copy_line("  if  got != tt.want {\t"), "if got != tt.want {")

    def test_a_small_new_file_below_the_line_bound_passes(self):
        small = [line for line in SOURCE_LINES if adapters.copy_line(line)][:adapters.COPY_MIN_LINES - 1]
        self.assertIsNone(self.guard().refused(self.message(small)))

    def test_partial_reads_count_with_what_they_hold(self):
        half = len(SOURCE_LINES) // 2
        first = "\n".join(SOURCE_LINES[:half])
        second = "\n".join(SOURCE_LINES[half:])
        both = rg.conversation(rg.assistant(("r1", "read_file", {"path": EXISTING, "limit": half}),
                                            ("r2", "read_file", {"path": "./" + EXISTING, "offset": half + 1})),
                               rg.result("r1", read_result(first, half, len(SOURCE_LINES))),
                               rg.result("r2", read_result(second, total=len(SOURCE_LINES))))
        guard = self.guard(both)
        self.assertEqual(guard.reads[EXISTING], adapters.copy_lines(SOURCE))
        self.assertEqual(guard.refused(self.message(SOURCE_LINES)), copy_refusal())
        # Only the first half read: a copy of the whole file is still refused
        # once its first 40 distinct lines are all known.
        one = rg.conversation(rg.assistant(("r1", "read_file", {"path": EXISTING, "limit": half})),
                              rg.result("r1", read_result(first, half, len(SOURCE_LINES))))
        self.assertEqual(self.guard(one).refused(self.message(SOURCE_LINES)), copy_refusal())
        # SCC's trailing note is not file content.
        self.assertNotIn("[%d lines total]" % len(SOURCE_LINES), guard.reads[EXISTING])

    def test_only_successful_reads_of_other_paths_are_sources(self):
        failed = rg.conversation(rg.assistant(("r1", "read_file", {"path": EXISTING})),
                                 rg.result("r1", "[ERROR] [⚠️ Error] read_file: File too large"))
        self.assertEqual(self.guard(failed).reads, {})
        # The model rewriting its own new file, read back, is not a copy.
        own = rg.conversation(rg.assistant(("w1", "write_file", {"path": NEW, "content": SOURCE})), rg.result("w1", "ok"),
                              rg.assistant(("r1", "read_file", {"path": NEW})), rg.result("r1", read_result(SOURCE, total=9)))
        self.assertIsNone(self.guard(own).refused(self.message(SOURCE_LINES)))

    def test_streamed_strings_decode_escapes_split_across_deltas(self):
        raw = "{\"path\": \"a\", \"content\": \"" + json.dumps("café \"q\"\tx\nline two\\n\n")[1:-1]
        start = adapters.COPY_CONTENT.search(raw).end()
        decoder, lines = adapters._StreamedString(start), []
        for end in range(start, len(raw) + 1):
            lines += decoder.feed(raw[:end])
        lines += decoder.feed(raw + "\"}")
        self.assertEqual(lines, ["café \"q\"\tx", "line two\\n", ""])
        self.assertTrue(decoder.done)


class StreamingCopyTests(unittest.TestCase):
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

    def counts(self, completion):
        return (completion.copy_retries, completion.copy_passed, completion.rewrite_retries, completion.rewrite_passed)

    def test_a_copy_is_stopped_early_and_retried_once_with_the_correction(self):
        base.SSEUpstream.script = [SLOW_COPY, LEGIT]
        started = time.monotonic()
        completion = self.adapter().complete(READ)
        self.assertLess(time.monotonic() - started, 5)  # the copy was never streamed to its end
        deadline = time.monotonic() + 3
        while not base.SSEUpstream.disconnected and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertEqual(len(base.SSEUpstream.disconnected), 1)
        self.assertEqual(self.counts(completion), (1, 0, 0, 0))
        first, second = base.SSEUpstream.seen
        self.assertEqual(second["messages"], first["messages"] + [copy_correction()])
        content = json.loads(completion.message["tool_calls"][0]["function"]["arguments"])["content"]
        self.assertTrue(content.startswith("package textutil"))

    def test_legitimate_and_small_new_files_pass_untouched(self):
        small = write_lines(NEW, [line for line in SOURCE_LINES if adapters.copy_line(line)][:adapters.COPY_MIN_LINES - 1])
        for script in (LEGIT, small):
            with self.subTest(script=script):
                base.SSEUpstream.seen, base.SSEUpstream.script = [], [script]
                completion = self.adapter().complete(READ)
                self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (1, (0, 0, 0, 0)))

    def test_a_retry_that_copies_again_is_passed_through(self):
        base.SSEUpstream.script = [COPY, COPY]
        completion = self.adapter().complete(READ)
        self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (2, (1, 1, 0, 0)))

    def test_one_retry_is_shared_with_the_rewrite_guard_and_the_degenerate_retry(self):
        # A rewrite spends the retry; the copy that follows is passed through.
        base.SSEUpstream.script = [rg.rewrite(), COPY]
        completion = self.adapter().complete(READ)
        self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (2, (0, 1, 1, 0)))
        self.assertEqual(base.SSEUpstream.seen[1]["messages"][-1], rg.correction(EXISTING))
        # And the other way round.
        base.SSEUpstream.seen, base.SSEUpstream.script = [], [COPY, rg.rewrite()]
        completion = self.adapter().complete(READ)
        self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (2, (1, 0, 0, 1)))
        # Degenerate, copy, degenerate: no second degenerate retry.
        base.SSEUpstream.seen, base.SSEUpstream.script = [], [base.degenerate_stream, COPY, base.degenerate_stream]
        with self.assertRaises(adapters.DegenerateCompletion) as caught:
            self.adapter().complete(READ)
        error = caught.exception
        self.assertEqual((error.degenerate_retries, error.copy_retries, error.rewrite_retries, len(base.SSEUpstream.seen)), (1, 1, 0, 3))
        # Too little budget for a retry: the copy streams on and is passed through.
        base.SSEUpstream.seen, base.SSEUpstream.script = [], [COPY]
        completion = self.adapter(seconds=5).complete(READ)
        self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (1, (0, 1, 0, 0)))

    def test_each_upstream_attempt_is_judged_on_its_own_stream(self):
        def degenerate_write(path):
            # A write_file to a new path that degenerates inside its content.
            def deltas(_body):
                yield base.sse_event(base.chunk(role="assistant", calls=[{"index": 0, "id": "d", "type": "function",
                                                                         "function": {"name": "write_file", "arguments": ""}}]))
                yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "{\"path\": %s, \"content\": \"" % json.dumps(path)}}]))
                for line in SOURCE_LINES[:20]:
                    yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": json.dumps(line + "\n")[1:-1]}}]))
                for _ in range(5000):
                    yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "??"}}]))
            return deltas
        # Degenerate, then a copy to another path at the same call index: the
        # copy is stopped early and the correction names the retry's path.
        base.SSEUpstream.script = [degenerate_write("textutil/first_try_test.go"), SLOW_COPY, LEGIT]
        started = time.monotonic()
        completion = self.adapter().complete(READ)
        self.assertLess(time.monotonic() - started, 5)
        self.assertEqual((completion.degenerate_retries, len(base.SSEUpstream.seen)), (1, 3))
        self.assertEqual(self.counts(completion), (1, 0, 0, 0))
        self.assertEqual(base.SSEUpstream.seen[2]["messages"][-1], copy_correction(NEW))
        # The same for #797's rewrite check: attempt 2 rewrites a pre-existing file.
        base.SSEUpstream.seen, base.SSEUpstream.script = [], [degenerate_write("textutil/first_try_test.go"), rg.slow_rewrite, LEGIT]
        started = time.monotonic()
        completion = self.adapter().complete(READ)
        self.assertLess(time.monotonic() - started, 5)
        self.assertEqual(self.counts(completion), (0, 0, 1, 0))
        self.assertEqual(base.SSEUpstream.seen[2]["messages"][-1], rg.correction(EXISTING))

    def test_a_streamed_move_is_left_to_the_final_message(self):
        edit_first = base.stream(
            base.chunk(calls=[{"index": 0, "id": "e", "type": "function", "function": {
                "name": "edit_file", "arguments": json.dumps({"path": EXISTING, "patch": "p"})}}]),
            base.chunk(calls=[{"index": 1, "id": "w", "type": "function", "function": {
                "name": "write_file", "arguments": json.dumps({"path": NEW, "content": SOURCE})[:-1]}}]),
            base.chunk(calls=[{"index": 1, "function": {"arguments": "}"}}]),
            base.chunk(finish="tool_calls"))
        base.SSEUpstream.script = [edit_first]
        completion = self.adapter().complete(READ)
        self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (1, (0, 0, 0, 0)))

    def test_disabled_guard_and_a_failing_copy_check_never_refuse(self):
        base.SSEUpstream.script = [COPY]
        completion = self.adapter(guard=False).complete(READ)
        self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (1, (0, 0, 0, 0)))
        base.SSEUpstream.seen = []
        with unittest.mock.patch.object(adapters.RewriteGuard, "read_lines", side_effect=TypeError("boom")):
            completion = self.adapter().complete(READ)
        self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (1, (0, 0, 0, 0)))


class SourceRules(unittest.TestCase):
    def reads(self, *turns):
        return adapters.RewriteGuard.read_lines(adapters.validate_request(rg.conversation(*turns))[0])

    def test_scc_read_results_are_raw_lines_with_a_trailing_note(self):
        whole = scc_read(SOURCE)
        self.assertTrue(whole.endswith("\n\n[%d lines total]" % len(SOURCE_LINES)))
        self.assertEqual(adapters.READ_NOTE.sub("", whole), SOURCE)
        part = scc_read(SOURCE, offset=11, limit=20)
        self.assertIn("lines shown", part)
        self.assertEqual(adapters.READ_NOTE.sub("", part), "\n".join(SOURCE_LINES[10:30]))
        reads = self.reads(rg.assistant(("r1", "read_file", {"path": EXISTING, "offset": 11, "limit": 20})), rg.result("r1", part))
        self.assertEqual(reads[EXISTING], adapters.copy_lines("\n".join(SOURCE_LINES[10:30])))
        self.assertFalse(any("lines" in line and "shown" in line for line in reads[EXISTING]))

    def test_a_shortened_old_output_keeps_only_what_remains(self):
        pruned = scc_prune(scc_read(SOURCE))
        reads = self.reads(rg.assistant(("r1", "read_file", {"path": EXISTING})), rg.result("r1", pruned))[EXISTING]
        self.assertFalse(any("truncated to save context window" in line for line in reads))
        self.assertLess(len(reads), adapters.COPY_MIN_MATCHED)  # too little left to judge a copy by

    def test_files_the_model_created_are_never_sources(self):
        reads = self.reads(rg.assistant(("w1", "write_file", {"path": "textutil/helper.go", "content": SOURCE})),
                           rg.result("w1", "File written successfully: textutil/helper.go"),
                           rg.assistant(("r1", "read_file", {"path": "textutil/helper.go"}), ("r2", "read_file", {"path": EXISTING})),
                           rg.result("r1", scc_read(SOURCE)), rg.result("r2", scc_read(SOURCE)))
        self.assertEqual(set(reads), {EXISTING})


class MoveTests(base.UpstreamCase):
    def call(self, name, **arguments):
        return {"id": name, "type": "function", "function": {"name": name, "arguments": json.dumps(dict(arguments))}}

    def test_a_move_with_its_removal_in_the_same_answer_is_not_a_copy(self):
        move = [self.call("write_file", path=NEW, content=SOURCE), self.call("edit_file", path="./" + EXISTING, patch="p")]
        base.FakeUpstream.reply = (200, base.openai_reply(calls=move), {})
        completion = adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "m", 30).complete(READ)
        self.assertEqual((completion.copy_retries, completion.copy_passed, len(base.FakeUpstream.seen)), (0, 0, 1))
        # An edit of another file is no move.
        base.FakeUpstream.seen = []
        other = [move[0], self.call("edit_file", path="textutil/textutil.go", patch="p")]
        replies = [(200, base.openai_reply(calls=other), {}), (200, base.openai_reply(calls=move), {})]
        base.FakeUpstream.reply = lambda _body: replies[len(base.FakeUpstream.seen) - 1]
        completion = adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "m", 30).complete(READ)
        self.assertEqual((completion.copy_retries, completion.copy_passed, len(base.FakeUpstream.seen)), (1, 0, 2))
        # The correction names moving as the legitimate exception.
        text = base.FakeUpstream.seen[1][2]["messages"][-1]["content"]
        self.assertIn("unless the issue asks you to move it", text)
        self.assertIn("remove it from %s with edit_file" % json.dumps(EXISTING), text)


class FinalMessageCopyTests(base.UpstreamCase):
    def reply(self, *lines_per_call):
        calls = [{"id": "w", "type": "function", "function": {"name": "write_file",
                  "arguments": json.dumps({"path": NEW, "content": "\n".join(lines)})}} for lines in lines_per_call]
        return (200, base.openai_reply(calls=calls), {})

    def test_a_json_answer_is_judged_on_its_final_message(self):
        replies = [self.reply(SOURCE_LINES), self.reply(LEGIT_LINES)]
        base.FakeUpstream.reply = lambda _body: replies[len(base.FakeUpstream.seen) - 1]
        completion = adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "m", 30).complete(READ)
        self.assertEqual((completion.copy_retries, completion.copy_passed, len(base.FakeUpstream.seen)), (1, 0, 2))
        self.assertEqual(base.FakeUpstream.seen[1][2]["messages"][-1], copy_correction())

    def test_a_failed_retry_returns_the_kept_copy(self):
        base.FakeUpstream.reply = lambda _body: self.reply(SOURCE_LINES) if len(base.FakeUpstream.seen) == 1 else (500, {"error": "x"}, {})
        completion = adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "m", 30).complete(READ)
        self.assertEqual((completion.copy_retries, completion.copy_passed, len(base.FakeUpstream.seen)), (1, 1, 2))


class CopyLoggingTests(unittest.TestCase):
    def test_copy_counts_are_logged_as_counts_only(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            model_gateway.log_call("ok", time.monotonic(), 30, copy_retries=1, copy_passed=1)
            model_gateway.log_call("failed", time.monotonic(), 30, reason="deadline", copy_retries=1)
            model_gateway.log_call("ok", time.monotonic(), 30, copy_retries=2, copy_passed=True)
        events = [json.loads(line) for line in out.getvalue().splitlines()]
        self.assertEqual([(e.get("copyRetries"), e.get("copyPassed")) for e in events], [(1, 1), (1, None), (None, None)])


if __name__ == "__main__":
    unittest.main()
