"""Claude Code print-mode adapter: completion only, isolated, fail closed."""
import json
import os
import stat
import tempfile
import unittest

import model_adapters as adapters

TOOLS = [{"type": "function", "function": {"name": "read_file", "description": "Read a file",
          "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}}]
REQUEST = {"messages": [{"role": "user", "content": "Read README.md"}], "tools": TOOLS}
FAKE = """#!/usr/bin/env python3
import json, os, sys
record = os.environ["FAKE_RECORD"] if "FAKE_RECORD" in os.environ else %(record)r
json.dump({"argv": sys.argv[1:], "env": dict(os.environ), "cwd": os.getcwd(), "stdin": sys.stdin.read()}, open(record, "w"))
mode = %(mode)r
if mode == "fail": sys.exit(3)
if mode == "sleep":
    import time; time.sleep(5)
answer = {"type": "result", "subtype": "success", "is_error": False,
          "structured_output": {"content": "Reading.", "tool_calls": [{"name": %(tool)r, "arguments": json.dumps({"path": "README.md"})}]},
          "usage": {"input_tokens": 100, "cache_creation_input_tokens": 20, "cache_read_input_tokens": 30, "output_tokens": 12}}
if mode == "error": answer.update(is_error=True, subtype="error_during_execution")
print(json.dumps(answer))
"""


class ClaudeCLITests(unittest.TestCase):
    def adapter(self, mode="ok", tool="read_file", provider="anthropic"):
        self.dir = tempfile.mkdtemp()
        self.record = os.path.join(self.dir, "record.json")
        binary = os.path.join(self.dir, "claude")
        with open(binary, "w") as handle:
            handle.write(FAKE % {"record": self.record, "mode": mode, "tool": tool})
        os.chmod(binary, stat.S_IRWXU)
        vertex = {"project": "proj-12345", "region": "global", "credentialFile": "/creds/sa.json"} if provider == "vertex" else None
        adapter = adapters.ClaudeCLIAdapter("claude-sonnet-5-5", 2, "sk-test" if provider == "anthropic" else "", provider, vertex)
        adapter.binary = binary
        return adapter

    def seen(self):
        return json.load(open(self.record))

    def test_structured_answer_becomes_a_bounded_tool_call_with_usage(self):
        result = self.adapter().complete(REQUEST)
        self.assertEqual(result.message["content"], "Reading.")
        self.assertEqual(json.loads(result.message["tool_calls"][0]["function"]["arguments"]), {"path": "README.md"})
        self.assertEqual((result.prompt_tokens, result.completion_tokens, result.estimated), (150, 12, False))

    def test_invocation_is_completion_only_and_isolated(self):
        os.environ["AI_SDLC_SECRET_PROBE"] = "must-not-leak"
        try:
            self.adapter().complete(REQUEST)
        finally:
            del os.environ["AI_SDLC_SECRET_PROBE"]
        seen = self.seen()
        argv = seen["argv"]
        self.assertEqual(argv[argv.index("--tools") + 1], "")
        for flag in ("-p", "--bare", "--strict-mcp-config", "--no-session-persistence"):
            self.assertIn(flag, argv)
        self.assertEqual(json.loads(argv[argv.index("--json-schema") + 1]), adapters.COMPLETION_SCHEMA)
        self.assertEqual(argv[argv.index("--model") + 1], "claude-sonnet-5-5")
        self.assertNotIn("AI_SDLC_SECRET_PROBE", seen["env"])
        self.assertEqual(seen["env"]["ANTHROPIC_API_KEY"], "sk-test")
        self.assertNotIn("CLAUDE_CODE_USE_VERTEX", seen["env"])
        self.assertEqual(seen["cwd"], seen["env"]["HOME"])
        self.assertFalse(os.path.exists(seen["cwd"]), "per-call directory not removed")
        transcript = seen["stdin"]
        self.assertIn("- read_file: Read a file", transcript)
        self.assertIn("[user]\nRead README.md", transcript)

    def test_vertex_provider_uses_only_its_own_credentials(self):
        self.adapter(provider="vertex").complete(REQUEST)
        env = self.seen()["env"]
        self.assertEqual((env["CLAUDE_CODE_USE_VERTEX"], env["ANTHROPIC_VERTEX_PROJECT_ID"], env["CLOUD_ML_REGION"],
                          env["GOOGLE_APPLICATION_CREDENTIALS"]), ("1", "proj-12345", "global", "/creds/sa.json"))
        self.assertNotIn("ANTHROPIC_API_KEY", env)

    def test_errors_timeouts_and_unknown_tools_fail_closed(self):
        for mode, error in (("fail", RuntimeError), ("sleep", RuntimeError), ("error", ValueError)):
            with self.subTest(mode=mode), self.assertRaises(error):
                self.adapter(mode).complete(REQUEST)
        with self.assertRaises(ValueError):
            self.adapter(tool="run_shell").complete(REQUEST)


class TranscriptTests(unittest.TestCase):
    def test_tool_calls_and_complete_results_are_rendered_in_order(self):
        messages = [{"role": "system", "content": "Policy."}, {"role": "user", "content": "Fix it."},
                    {"role": "assistant", "content": "", "tool_calls": [{"id": "c1", "type": "function",
                     "function": {"name": "read_file", "arguments": "{\"path\":\"a.js\"}"}}]},
                    {"role": "tool", "tool_call_id": "c1", "content": "line1\nline2"}]
        text = adapters.render_transcript(messages, TOOLS, 512)
        order = [text.index(x) for x in ("[system]\nPolicy.", "[user]\nFix it.", "[assistant tool call c1] read_file {\"path\":\"a.js\"}",
                                          "[tool result c1]\nline1\nline2", "at most 512 tokens")]
        self.assertEqual(order, sorted(order))


class ConfigurationTests(unittest.TestCase):
    def test_providers_require_their_credentials(self):
        with tempfile.NamedTemporaryFile("w", delete=False) as handle:
            handle.write("sk-ant-api-test\n")
        try:
            base = {"AI_SDLC_MODEL_ADAPTER": "claude-cli", "AI_SDLC_MODEL_UPSTREAM_MODEL": "claude-sonnet-5-5",
                    "AI_SDLC_MODEL_UPSTREAM_CREDENTIAL_FILE": handle.name}
            adapter = adapters.adapter_from_environ(base)
            self.assertEqual((adapter.name, adapter.provider, adapter.credential), ("claude-cli", "anthropic", "sk-ant-api-test"))
            vertex = adapters.adapter_from_environ(dict(base, AI_SDLC_MODEL_CLAUDE_PROVIDER="vertex",
                                                        AI_SDLC_MODEL_CLAUDE_VERTEX_PROJECT="proj-12345",
                                                        AI_SDLC_MODEL_CLAUDE_VERTEX_REGION="us-east5"))
            self.assertEqual((vertex.provider, vertex.vertex["credentialFile"]), ("vertex", handle.name))
            for change in ({"AI_SDLC_MODEL_UPSTREAM_CREDENTIAL_FILE": ""}, {"AI_SDLC_MODEL_UPSTREAM_MODEL": "bad model"},
                           {"AI_SDLC_MODEL_CLAUDE_PROVIDER": "openai"},
                           {"AI_SDLC_MODEL_CLAUDE_PROVIDER": "vertex", "AI_SDLC_MODEL_CLAUDE_VERTEX_PROJECT": "x",
                            "AI_SDLC_MODEL_CLAUDE_VERTEX_REGION": "global"}):
                with self.subTest(change=change), self.assertRaises(RuntimeError):
                    adapters.adapter_from_environ(dict(base, **change))
        finally:
            os.unlink(handle.name)


@unittest.skipUnless(os.environ.get("AI_SDLC_TEST_CLAUDE_CLI"), "requires an authenticated Claude CLI")
class RealClaudeCLITests(unittest.TestCase):
    def test_real_cli_proposes_a_valid_tool_call(self):
        adapter = adapters.ClaudeCLIAdapter(os.environ.get("AI_SDLC_TEST_CLAUDE_MODEL", "haiku"), 120, "", "vertex",
                                            {"project": "p", "region": "r", "credentialFile": "f"})
        adapter.binary = os.environ["AI_SDLC_TEST_CLAUDE_CLI"]
        adapter.environment = lambda home: dict(os.environ, HOME=os.environ.get("HOME", home))
        result = adapter.complete(REQUEST)
        self.assertEqual(result.message["tool_calls"][0]["function"]["name"], "read_file")


if __name__ == "__main__":
    unittest.main()
