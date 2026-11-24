"""Hunk counts of proposed edit_file patches are recomputed, never content."""
import contextlib
import io
import json
import os
import threading
import unittest
import unittest.mock
import urllib.request

import model_adapters as adapters
import model_gateway
from test_model_gateway import TOKEN, FakeUpstream, UpstreamCase, openai_reply

# Evaluation batteries 20 and 21: a local Qwen3-Coder model declared 3/8 for a
# hunk of 9 context and 9 added lines, and SCC refused the call.
REAL = """--- a/mathutil/mathutil.go
+++ b/mathutil/mathutil.go
@@ -390,3 +390,8 @@
 // Half returns n/2 rounded toward zero.
 // Examples:
 //   Half(5) = 2
 //   Half(-5) = -2
 //   Half(4) = 2
 //   Half(0) = 0
 func Half(n int) int {
 \treturn n / 2
 }
+
+// Negate returns the negation of n (-n).
+// Examples:
+//   Negate(5) = -5
+//   Negate(-3) = 3
+//   Negate(0) = 0
+func Negate(n int) int {
+\treturn -n
+}"""

MULTI = """--- a/shapes.go
+++ b/shapes.go
@@ -1,4 +1,4 @@
 package shapes

-import "math"
+import (
+\t"fmt"
+\t"math"
+)

@@ -16,3 +16,3 @@ func (c Circle) Perimeter() float64 {
 func (c Circle) Perimeter() float64 {
 \treturn 2 * math.Pi * c.R
 }
+
+// String describes the circle.
"""

EDIT_TOOLS = [{"type": "function", "function": {"name": name, "description": name, "parameters": {"type": "object"}}}
              for name in ("edit_file", "read_file", "write_file")]
EDIT_REQUEST = {"messages": [{"role": "user", "content": "Add Negate."}], "tools": EDIT_TOOLS}


def headers(patch):
    return [line for line in patch.split("\n") if line.startswith("@@")]


def body(patch):
    return [line for line in patch.split("\n") if not line.startswith("@@")]


class RecountPatchTests(unittest.TestCase):
    def test_the_battery_example_gets_the_counts_of_its_body(self):
        fixed = adapters.recount_patch(REAL)
        self.assertEqual(headers(fixed), ["@@ -390,9 +390,18 @@"])
        self.assertEqual(body(fixed), body(REAL))
        self.assertFalse(fixed.endswith("\n"))
        self.assertIsNone(adapters.recount_patch(fixed))
        # Over-counted, omitted and off-by-one headers converge on the same counts.
        for header in ("@@ -390,12 +390,21 @@", "@@ -390 +390 @@", "@@ -390,9 +390,17 @@"):
            with self.subTest(header=header):
                self.assertEqual(adapters.recount_patch(REAL.replace("@@ -390,3 +390,8 @@", header)), fixed)

    def test_every_hunk_is_recounted_and_starts_and_section_text_are_kept(self):
        fixed = adapters.recount_patch(MULTI)
        # The blank line ending the first hunk precedes the next header: the
        # parser skips it, so it is not counted; the interior one is context.
        self.assertEqual(headers(fixed), ["@@ -1,3 +1,6 @@", "@@ -16,3 +16,5 @@ func (c Circle) Perimeter() float64 {"])
        self.assertEqual(body(fixed), body(MULTI))
        self.assertTrue(fixed.endswith("\n"))

    def test_bare_blank_lines_inside_a_hunk_count_as_context(self):
        patch = "@@ -1,2 +1,3 @@\n a\n\n b\n+c\n"
        self.assertEqual(adapters.recount_patch(patch), "@@ -1,3 +1,4 @@\n a\n\n b\n+c\n")

    def test_declared_counts_that_include_trailing_blank_lines_are_kept(self):
        # The parser reads as many trailing blank lines as the counts declare.
        for patch in ("@@ -1,2 +1,3 @@\n a\n+b\n\n", "@@ -1,1 +1,2 @@\n a\n+b\n\n\n", "@@ -1,3 +1,4 @@\n a\n+b\n\n\n"):
            with self.subTest(patch=patch):
                self.assertIsNone(adapters.recount_patch(patch))

    def test_no_newline_markers_are_not_counted(self):
        patch = "@@ -1,1 +1,1 @@\n-a\n\\ No newline at end of file\n+b\n+c\n\\ No newline at end of file\n"
        self.assertEqual(headers(adapters.recount_patch(patch)), ["@@ -1,1 +1,2 @@"])

    def test_correct_and_ambiguous_patches_are_left_unchanged(self):
        cases = {
            "correct": "--- a/x\n+++ b/x\n@@ -1,2 +1,3 @@\n a\n+b\n c\n",
            "correct omitted counts": "@@ -1 +1 @@\n-a\n+b\n",
            "correct insertion": "@@ -3,0 +4,2 @@\n+a\n+b\n",
            "no hunk": "--- a/x\n+++ b/x\n a\n",
            "empty": "",
            "malformed header": "@@ -1,2 +1,3@@\n a\n+b\n c\n",
            "lone added file header": "@@ -1,1 +1,1 @@\n-a\n+b\n+++ x\n",
            "tab file header": "@@ -1,1 +1,1 @@\n-a\n+b\n---\tx\n",
            "header without starts": "@@ -a,1 +1,1 @@\n a\n",
            "section text without a space": "@@ -1,1 +1,2 @@x\n a\n+b\n c\n",
            "huge start": "@@ -1234567890,1 +1,2 @@\n a\n+b\n c\n",
            "non-ASCII digits": "@@ -١,1 +1,2 @@\n a\n+b\n c\n",
            "unknown prefix": "@@ -1,1 +1,1 @@\n a\n*b\n",
            "unknown backslash line": "@@ -1,1 +1,1 @@\n a\n+b\n\\ something else\n",
            "blank before a marker": "@@ -1,1 +1,1 @@\n-a\n+b\n\n\\ No newline at end of file\n",
            "carriage return": "@@ -1,1 +1,1 @@\r\n a\r\n+b\r\n",
            "second file": "--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n a\n+b\n--- a/y\n+++ b/y\n@@ -1,1 +1,1 @@\n-c\n+d\n",
            "second file in the preamble": "--- a/x\n+++ b/x\n--- a/y\n+++ b/y\n@@ -1,1 +1,1 @@\n a\n+b\n",
            "git separator": "@@ -1,1 +1,1 @@\n a\n+b\ndiff --git a/y b/y\n@@ -1,1 +1,1 @@\n-c\n+d\n",
            "only blank lines": "@@ -1,1 +1,1 @@\n\n\n",
            "side becomes empty": "@@ -3,1 +3,2 @@\n+a\n+b\n",
            "side becomes non-empty": "@@ -3,0 +3,1 @@\n a\n+b\n",
            "zero start with lines": "@@ -0,0 +0,1 @@\n+a\n+b\n",
            "over the bound": "@@ -1,1 +1,1 @@\n" + " a\n" * (adapters.MAX_EDIT_ARGUMENT_BYTES // 3),
            "too many hunks": "@@ -1,1 +1,1 @@\n a\n+b\n" * (adapters.MAX_EDIT_HUNKS + 1),
            "not text": None,
        }
        for name, patch in cases.items():
            with self.subTest(name):
                self.assertIsNone(adapters.recount_patch(patch))

    def test_work_is_linear_in_the_patch_size(self):
        hunk = "@@ -1,1 +1,1 @@\n" + " a\n" * 30 + "+b\n" * 30
        fixed = adapters.recount_patch(hunk * 16)
        self.assertEqual(set(headers(fixed)), {"@@ -1,30 +1,60 @@"})


def edit_call(arguments, name="edit_file"):
    return {"id": "x", "type": "function", "function": {"name": name, "arguments": arguments}}


class RecountMessageTests(unittest.TestCase):
    def test_only_the_patch_of_an_edit_file_call_changes_and_other_keys_survive(self):
        arguments = json.dumps({"path": "mathutil/mathutil.go", "patch": REAL, "note": "café"})
        read = json.dumps({"path": "a.go"})
        message = {"role": "assistant", "content": "", "tool_calls": [edit_call(read, "read_file"), edit_call(arguments)]}
        self.assertEqual(adapters.recount_edit_patches(message), 1)
        self.assertEqual(message["tool_calls"][0]["function"]["arguments"], read)
        repaired = json.loads(message["tool_calls"][1]["function"]["arguments"])
        self.assertEqual(list(repaired), ["path", "patch", "note"])
        self.assertEqual((repaired["path"], repaired["note"]), ("mathutil/mathutil.go", "café"))
        self.assertEqual(repaired["patch"], adapters.recount_patch(REAL))

    def test_a_failing_repair_leaves_the_call_unchanged(self):
        # A lone surrogate is valid escaped JSON but cannot be encoded as UTF-8.
        for raw in ('{"path":"f","patch":"@@ -1,9 +1,9 @@\\n-a\\n+\\ud800\\n"}',
                    '{"path":"\\ud800","patch":"@@ -1,9 +1,9 @@\\n-a\\n+b\\n"}'):
            message = {"role": "assistant", "content": "", "tool_calls": [edit_call(raw)]}
            self.assertEqual(adapters.recount_edit_patches(message), 0)
            self.assertEqual(message["tool_calls"][0]["function"]["arguments"], raw)

    def test_messages_without_a_repair_are_byte_identical(self):
        correct = adapters.recount_patch(REAL)
        for calls in ([], [edit_call(json.dumps({"path": "a", "patch": REAL}), "write_file")],
                      [edit_call('{"path": "a",   "patch": %s}' % json.dumps(correct))],
                      [edit_call(json.dumps({"path": "a", "patch": 7}))],
                      [edit_call(json.dumps({"path": "a"}))],
                      [edit_call("[1]")], [edit_call("{not json")],
                      [edit_call(json.dumps({"path": "a", "patch": REAL, "pad": "x" * adapters.MAX_EDIT_ARGUMENT_BYTES}))]):
            message = {"role": "assistant", "content": "ok", "tool_calls": calls} if calls else {"role": "assistant", "content": "ok"}
            before = json.dumps(message)
            with self.subTest(calls=before[:80]):
                self.assertEqual(adapters.recount_edit_patches(message), 0)
                self.assertEqual(json.dumps(message), before)


class AdapterPathTests(UpstreamCase):
    """Every adapter returns its message through ``Adapter.complete``."""

    def assert_repaired(self, completion):
        self.assertEqual(completion.edit_patches_recounted, 1)
        patch = json.loads(completion.message["tool_calls"][0]["function"]["arguments"])["patch"]
        self.assertEqual(headers(patch), ["@@ -390,9 +390,18 @@"])

    def test_openai_adapter(self):
        call = [{"id": "c", "type": "function", "function": {"name": "edit_file", "arguments": json.dumps({"path": "m.go", "patch": REAL})}}]
        FakeUpstream.reply = (200, openai_reply(calls=call), {})
        adapter = adapters.OpenAIAdapter(self.base + "/v1/chat/completions", "qwen3-coder", 5)
        self.assert_repaired(adapter.complete(EDIT_REQUEST))
        FakeUpstream.reply = (200, openai_reply(content="Done."), {})
        self.assertEqual(adapter.complete(EDIT_REQUEST).edit_patches_recounted, 0)

    def test_anthropic_adapter(self):
        FakeUpstream.reply = (200, {"role": "assistant", "content": [
            {"type": "tool_use", "id": "t", "name": "edit_file", "input": {"path": "m.go", "patch": REAL}}]}, {})
        adapter = adapters.AnthropicAdapter(self.base + "/v1/messages", "claude-sonnet-5-5", 5, "key-1")
        self.assert_repaired(adapter.complete(EDIT_REQUEST))

    def test_agent_cli_adapter(self):
        names = {"edit_file", "read_file", "write_file"}
        message = adapters.assistant_message("", [{"name": "edit_file", "arguments": json.dumps({"path": "m.go", "patch": REAL})}], names)
        with unittest.mock.patch("codexcli_openai_wrapper.complete", return_value=message):
            self.assert_repaired(adapters.AgentCLIAdapter().complete(EDIT_REQUEST))


class EditAdapter(adapters.Adapter):
    name = "openai"

    def backend(self, request, messages, tools, names, tokens, seconds):
        calls = [{"name": "edit_file", "arguments": json.dumps({"path": "m.go", "patch": REAL})}]
        return adapters.Completion(adapters.assistant_message("", calls, names), 10, 5, False)


class GatewayEventTests(unittest.TestCase):
    def test_the_model_call_event_counts_repairs_without_patch_content(self):
        os.environ["CODEX_WRAPPER_TOKEN"] = TOKEN
        self.addCleanup(os.environ.pop, "CODEX_WRAPPER_TOKEN", None)
        server = model_gateway.GatewayServer(("127.0.0.1", 0), model_gateway.GatewayHandler, EditAdapter("", "m", 60))
        server.profile = {"timeoutSeconds": 60}
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        request = urllib.request.Request("http://127.0.0.1:%d/v1/chat/completions" % server.server_address[1],
                                         data=json.dumps(EDIT_REQUEST).encode(), headers={"Authorization": "Bearer " + TOKEN})
        out = io.StringIO()
        with contextlib.redirect_stdout(out), urllib.request.urlopen(request, timeout=5) as response:
            answer = json.loads(response.read())
        patch = json.loads(answer["choices"][0]["message"]["tool_calls"][0]["function"]["arguments"])["patch"]
        self.assertEqual(headers(patch), ["@@ -390,9 +390,18 @@"])
        events = [json.loads(line) for line in out.getvalue().splitlines() if '"model-call"' in line]
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]["editPatchesRecounted"], 1)
        self.assertNotIn("Negate", out.getvalue())
        self.assertNotIn("@@", out.getvalue())

    def test_events_without_repairs_omit_the_field(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            model_gateway.log_call("ok", 0.0, 60, prompt=1, completion=1, estimated=False)
            model_gateway.log_call("ok", 0.0, 60, prompt=1, completion=1, estimated=False, recounted=99)
        self.assertTrue(all("editPatchesRecounted" not in json.loads(line) for line in out.getvalue().splitlines()))


if __name__ == "__main__":
    unittest.main()
