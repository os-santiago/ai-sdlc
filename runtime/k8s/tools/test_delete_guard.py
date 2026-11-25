"""The destructive-edit guard refuses, once per call, an edit_file patch that
deletes much of a file the model read in the same conversation."""
import contextlib
import io
import json
import threading
import time
import unittest
from http.server import ThreadingHTTPServer

import model_adapters as adapters
import model_gateway
# Helpers only: importing the modules (not their classes) keeps their tests
# from being collected twice.
import test_copy_guard as cg
import test_model_gateway as base
import test_rewrite_guard as rg

KV = "kv/kv.go"


def go_function(name):
    return ["// %s is one of the package's existing functions." % name, "func %s(values map[string]string) bool {" % name,
            "\tfor key, value := range values {", "\t\tif strings.HasPrefix(key, %s) && value != \"\" {" % json.dumps(name.lower()),
            "\t\t\treturn true", "\t\t}", "\t}", "\treturn false", "}", ""]


# The existing kv/kv.go: a header and 15 functions, 156 lines.
KV_LINES = ["package kv", "", "import (", '\t"strings"', ")", ""] + sum(
    (go_function(name) for name in ("Equal", "ParseInts", "Format", "Keys", "Values", "Merge", "Filter", "Get",
                                    "Set", "Delete", "Has", "Len", "Clone", "Invert", "Prefix")), [])
KV_SOURCE = "\n".join(KV_LINES)
TOTAL = len(KV_LINES)
READ_KV = rg.conversation(rg.assistant(("r1", "read_file", {"path": KV})), rg.result("r1", cg.scc_read(KV_SOURCE)))


def hunk(start, removed, added=()):
    return ["@@ -%d,%d +%d,%d @@" % (start, len(removed), start, len(added))] + \
        ["-" + line for line in removed] + ["+" + line for line in added]


def patch(*hunks):
    return ["--- a/" + KV, "+++ b/" + KV] + sum(hunks, [])


HAS_ALL = ["// HasAll reports whether every key is present.", "func HasAll(values map[string]string, keys ...string) bool {",
           "\tfor _, key := range keys {", "\t\tif _, ok := values[key]; !ok {", "\t\t\treturn false", "\t\t}", "\t}",
           "\treturn true", "}", ""]
# The battery 30 shape: most functions deleted, HasAll added.
DESTRUCTIVE = patch(hunk(7, KV_LINES[6:126], HAS_ALL), hunk(150, KV_LINES[149:151]))
SMALL = patch(hunk(154, KV_LINES[153:154], [""] + HAS_ALL))
# A block replaced by one of similar size (a rewritten function body).
REPLACEMENT = patch(hunk(7, KV_LINES[6:66], [line.replace("bool", "(bool, error)") for line in KV_LINES[6:66]][:56]))


def edit_call(lines, path=KV, index=0, call_id="e"):
    return {"index": index, "id": call_id, "type": "function",
            "function": {"name": "edit_file", "arguments": json.dumps({"path": path, "patch": "\n".join(lines) + "\n"})}}


def streamed_edit(lines, path=KV, extra=0, delay=0.0):
    """A streamed edit_file: the path, then the patch one line per delta, then
    ``extra`` more hunks of one added line, ``delay`` seconds apart."""
    def deltas(_body):
        yield base.sse_event(base.chunk(role="assistant", calls=[{"index": 0, "id": "e", "type": "function",
                                                                 "function": {"name": "edit_file", "arguments": ""}}]))
        yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "{\"path\": %s, \"patch\": \"" % json.dumps(path)}}]))
        body = list(lines) + sum((["@@ -%d,0 +%d,1 @@" % (200 + i, 200 + i), "+// note %d" % i] for i in range(extra)), [])
        for line in body:
            yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": json.dumps(line + "\n")[1:-1]}}]))
            if delay:
                time.sleep(delay)
        yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "\"}"}}]))
        yield base.sse_event(base.chunk(finish="tool_calls"))
        yield base.sse_event(b"[DONE]")
    return deltas


# About 20 s if read to the end; the guard must stop it at its second hunk.
SLOW_DESTRUCTIVE = streamed_edit(DESTRUCTIVE, extra=10000, delay=0.002)
FAST_DESTRUCTIVE = streamed_edit(DESTRUCTIVE)
SMALL_EDIT = streamed_edit(SMALL)
# Judged at the first completed hunk, as while streaming.
REMOVED = 120


def delete_correction(removed=REMOVED, total=TOTAL, path=KV):
    return {"role": "user", "content": adapters.delete_correction_text(path, removed, total)}


class DeleteRules(unittest.TestCase):
    def guard(self, request=READ_KV):
        return adapters.RewriteGuard(request, adapters.validate_request(request)[0], time.monotonic() + 60)

    def message(self, *calls):
        return {"role": "assistant", "content": "", "tool_calls": [
            {"id": "c%d" % i, "type": "function", "function": call["function"]} for i, call in enumerate(calls)]}

    def test_thresholds_separate_destructive_patches_from_legitimate_edits(self):
        guard = self.guard()
        self.assertEqual(guard.totals, {KV: TOTAL})
        self.assertEqual(guard.refused(self.message(edit_call(DESTRUCTIVE))), adapters.Refusal("delete", KV, (REMOVED, TOTAL)))
        self.assertIsNone(guard.refused(self.message(edit_call(SMALL))))
        self.assertIsNone(guard.refused(self.message(edit_call(REPLACEMENT))))
        # 29 lines removed, or 30 lines that are under 40% of a large file, pass.
        self.assertIsNone(guard.refused(self.message(edit_call(patch(hunk(7, KV_LINES[6:35]))))))
        big = rg.conversation(rg.assistant(("r1", "read_file", {"path": KV, "limit": 50})),
                              rg.result("r1", cg.scc_read("\n".join(KV_LINES * 3), limit=50)))
        self.assertEqual(self.guard(big).totals, {KV: 3 * TOTAL})
        self.assertIsNone(self.guard(big).refused(self.message(edit_call(patch(hunk(7, KV_LINES[6:66]))))))
        self.assertIsNotNone(self.guard(big).refused(self.message(edit_call(patch(hunk(7, (KV_LINES * 3)[6:200]))))))

    def test_partial_reads_give_the_whole_files_total(self):
        partial = rg.conversation(rg.assistant(("r1", "read_file", {"path": KV, "offset": 1, "limit": 40})),
                                  rg.result("r1", cg.scc_read(KV_SOURCE, limit=40)))
        self.assertIn("lines shown", cg.scc_read(KV_SOURCE, limit=40))
        self.assertEqual(self.guard(partial).totals, {KV: TOTAL})
        self.assertIsNotNone(self.guard(partial).refused(self.message(edit_call(DESTRUCTIVE))))
        # The latest read gives the total; a read without SCC's note gives none.
        later = rg.conversation(rg.assistant(("r1", "read_file", {"path": KV})), rg.result("r1", cg.scc_read(KV_SOURCE)),
                                rg.assistant(("r2", "read_file", {"path": KV})), rg.result("r2", cg.scc_read("\n".join(KV_LINES * 2))))
        self.assertEqual(self.guard(later).totals, {KV: 2 * TOTAL})
        bare = rg.conversation(rg.assistant(("r1", "read_file", {"path": KV})), rg.result("r1", KV_SOURCE))
        self.assertEqual(self.guard(bare).totals, {})

    def test_files_not_read_or_created_by_the_model_are_not_judged(self):
        other = rg.conversation(rg.assistant(("r1", "read_file", {"path": "kv/other.go"})), rg.result("r1", cg.scc_read(KV_SOURCE)))
        self.assertIsNone(self.guard(other).refused(self.message(edit_call(DESTRUCTIVE))))
        created = rg.conversation(rg.assistant(("w1", "write_file", {"path": KV, "content": KV_SOURCE})), rg.result("w1", "ok"),
                                  rg.assistant(("r1", "read_file", {"path": KV})), rg.result("r1", cg.scc_read(KV_SOURCE)))
        self.assertEqual(self.guard(created).totals, {})

    def test_hunk_lines_count_removed_lines_that_start_with_dashes(self):
        lines = adapters.PatchLines()
        for line in ("--- a/x.sql", "+++ b/x.sql", "@@ -1,4 +1,1 @@", "-- a comment", "--- a rule", "-select 1;", " kept", "+-- new"):
            lines.feed(line)
        self.assertEqual((lines.removed, lines.added), (3, 1))
        self.assertEqual(lines.removed_lines, ["- a comment", "-- a rule", "select 1;"])
        # Before the first hunk, header lines are never counted.
        lines = adapters.PatchLines()
        self.assertFalse(lines.feed("--- a/x"))
        self.assertFalse(lines.feed("@@ -1,1 +1,1 @@"))
        self.assertTrue(lines.feed("@@ -5,1 +5,1 @@"))
        self.assertEqual((lines.removed, lines.added), (0, 0))

    def test_the_removal_half_of_a_move_is_not_destructive(self):
        moved = KV_LINES[6:126]
        write = {"function": {"name": "write_file", "arguments": json.dumps({"path": "kv/moved.go", "content": "\n".join(["package kv", ""] + moved)})}}
        self.assertIsNone(self.guard().refused(self.message(edit_call(DESTRUCTIVE), write)))
        # A new file that holds little of what was removed is no move.
        write["function"]["arguments"] = json.dumps({"path": "kv/moved.go", "content": "\n".join(["package kv", ""] + moved[:20])})
        self.assertIsNotNone(self.guard().refused(self.message(edit_call(DESTRUCTIVE), write)))


class StreamingDeleteTests(unittest.TestCase):
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
        return (completion.delete_retries, completion.delete_passed, completion.rewrite_retries, completion.rewrite_passed,
                completion.copy_retries, completion.copy_passed)

    def test_a_destructive_patch_is_stopped_at_its_hunk_and_retried_once(self):
        base.SSEUpstream.script = [SLOW_DESTRUCTIVE, SMALL_EDIT]
        started = time.monotonic()
        completion = self.adapter().complete(READ_KV)
        self.assertLess(time.monotonic() - started, 5)
        deadline = time.monotonic() + 3
        while not base.SSEUpstream.disconnected and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertEqual(len(base.SSEUpstream.disconnected), 1)
        self.assertEqual(self.counts(completion), (1, 0, 0, 0, 0, 0))
        first, second = base.SSEUpstream.seen
        # The count is the completed first hunk's: 120 removed lines.
        self.assertEqual(second["messages"], first["messages"] + [delete_correction(120)])
        self.assertIn("HasAll", json.loads(completion.message["tool_calls"][0]["function"]["arguments"])["patch"])

    def test_legitimate_edits_and_unread_files_pass_untouched(self):
        unread = rg.conversation(rg.assistant(("r1", "read_file", {"path": "kv/other.go"})), rg.result("r1", cg.scc_read(KV_SOURCE)))
        for request, script in ((READ_KV, SMALL_EDIT), (READ_KV, streamed_edit(REPLACEMENT)), (unread, FAST_DESTRUCTIVE)):
            with self.subTest(messages=len(request["messages"])):
                base.SSEUpstream.seen, base.SSEUpstream.script = [], [script]
                completion = self.adapter().complete(request)
                self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (1, (0, 0, 0, 0, 0, 0)))

    def test_a_retry_that_deletes_again_is_passed_through(self):
        base.SSEUpstream.script = [FAST_DESTRUCTIVE, FAST_DESTRUCTIVE]
        completion = self.adapter().complete(READ_KV)
        self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (2, (1, 1, 0, 0, 0, 0)))
        base.SSEUpstream.seen, base.SSEUpstream.script = [], [FAST_DESTRUCTIVE]
        completion = self.adapter(guard=False).complete(READ_KV)
        self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (1, (0, 0, 0, 0, 0, 0)))

    def test_one_retry_is_shared_with_the_other_guards_and_the_degenerate_retry(self):
        both = rg.conversation(rg.assistant(("r1", "read_file", {"path": KV}), ("r2", "read_file", {"path": rg.EXISTING})),
                               rg.result("r1", cg.scc_read(KV_SOURCE)), rg.result("r2", cg.scc_read(cg.SOURCE)))
        for script, expected in (([rg.rewrite(), FAST_DESTRUCTIVE], (0, 1, 1, 0, 0, 0)),
                                 ([FAST_DESTRUCTIVE, rg.rewrite()], (1, 0, 0, 1, 0, 0)),
                                 ([FAST_DESTRUCTIVE, cg.COPY], (1, 0, 0, 0, 0, 1)),
                                 ([cg.COPY, FAST_DESTRUCTIVE], (0, 1, 0, 0, 1, 0))):
            with self.subTest(expected=expected):
                base.SSEUpstream.seen, base.SSEUpstream.script = [], script
                completion = self.adapter().complete(both)
                self.assertEqual((len(base.SSEUpstream.seen), self.counts(completion)), (2, expected))
        base.SSEUpstream.seen, base.SSEUpstream.script = [], [base.degenerate_stream, FAST_DESTRUCTIVE, base.degenerate_stream]
        with self.assertRaises(adapters.DegenerateCompletion) as caught:
            self.adapter().complete(READ_KV)
        error = caught.exception
        self.assertEqual((error.degenerate_retries, error.delete_retries, len(base.SSEUpstream.seen)), (1, 1, 3))

    def test_each_upstream_attempt_is_judged_on_its_own_stream(self):
        def degenerate_edit(_body):
            # An edit of an unread file that degenerates inside its patch.
            yield base.sse_event(base.chunk(role="assistant", calls=[{"index": 0, "id": "d", "type": "function",
                                                                     "function": {"name": "edit_file", "arguments": ""}}]))
            yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "{\"path\": \"kv/unread.go\", \"patch\": \"@@ -1,1 +1,1 @@\\n"}}]))
            for _ in range(5000):
                yield base.sse_event(base.chunk(calls=[{"index": 0, "function": {"arguments": "??"}}]))
        base.SSEUpstream.script = [degenerate_edit, SLOW_DESTRUCTIVE, SMALL_EDIT]
        started = time.monotonic()
        completion = self.adapter().complete(READ_KV)
        self.assertLess(time.monotonic() - started, 5)
        self.assertEqual((completion.degenerate_retries, len(base.SSEUpstream.seen)), (1, 3))
        self.assertEqual(self.counts(completion), (1, 0, 0, 0, 0, 0))
        self.assertEqual(base.SSEUpstream.seen[2]["messages"][-1], delete_correction(120))


class FinalMessageDeleteTests(base.UpstreamCase):
    def test_a_json_answer_is_judged_and_a_failed_retry_keeps_it(self):
        reply = (200, base.openai_reply(calls=[edit_call(DESTRUCTIVE)]), {})
        base.FakeUpstream.reply = lambda _body: reply if len(base.FakeUpstream.seen) == 1 else (500, {"error": "x"}, {})
        completion = adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "m", 30).complete(READ_KV)
        self.assertEqual((completion.delete_retries, completion.delete_passed, len(base.FakeUpstream.seen)), (1, 1, 2))
        self.assertEqual(base.FakeUpstream.seen[1][2]["messages"][-1], delete_correction())


class DeleteLoggingTests(unittest.TestCase):
    def test_delete_counts_are_logged_as_counts_only(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            model_gateway.log_call("ok", time.monotonic(), 30, delete_retries=1, delete_passed=1)
            model_gateway.log_call("failed", time.monotonic(), 30, reason="deadline", delete_retries=1)
            model_gateway.log_call("ok", time.monotonic(), 30, delete_retries=2, delete_passed=True)
        events = [json.loads(line) for line in out.getvalue().splitlines()]
        self.assertEqual([(e.get("deleteRetries"), e.get("deletePassed")) for e in events], [(1, 1), (1, None), (None, None)])


if __name__ == "__main__":
    unittest.main()
