import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import codex_completion_policy as policy
import codex_completion_process as process


class ProcessTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)

    def script(self, content):
        path = self.root / "backend"
        path.write_text(f"#!{sys.executable}\n" + content)
        path.chmod(0o700)
        return str(path)

    def test_real_child_receives_fixed_policy_and_produces_bounded_output(self):
        binary = self.script('''import json, pathlib, sys
if sys.argv[1:] == ["--version"]:
    print("codex-cli 0.169.0")
else:
    assert sys.argv[sys.argv.index("--sandbox") + 1] == "read-only"
    assert "--ignore-user-config" in sys.argv
    assert "goals" in sys.argv
    assert sys.stdin.buffer.read() == b"fixture prompt"
    output = pathlib.Path(sys.argv[sys.argv.index("--output-last-message") + 1])
    assert output.parent == pathlib.Path.cwd()
    assert (output.parent / "codex_completion_catalog.json").stat().st_mode & 0o777 == 0o600
    output.write_text(json.dumps({"content": "fixture", "tool_calls": []}))
''')
        with patch.dict(os.environ, {"CODEX_WRAPPER_BINARY": binary}):
            result = process.complete_process(b"fixture prompt", "{}")
        self.assertEqual(json.loads(result), {"content": "fixture", "tool_calls": []})

    def test_real_child_does_not_receive_unrelated_credentials(self):
        binary = self.script('''import os
for name in ("GH_TOKEN", "GITHUB_TOKEN", "QUAY_TOKEN", "DATABASE_URL", "KUBECONFIG", "CODEX_WRAPPER_TOKEN", "HTTPS_PROXY"):
    assert name not in os.environ, name
assert os.environ["OPENAI_API_KEY"] == "fixture-model-credential"
''')
        secrets = {key: "fixture-canary" for key in (
            "GH_TOKEN", "GITHUB_TOKEN", "QUAY_TOKEN", "DATABASE_URL", "KUBECONFIG", "CODEX_WRAPPER_TOKEN", "HTTPS_PROXY")}
        secrets["OPENAI_API_KEY"] = "fixture-model-credential"
        with patch.dict(os.environ, secrets):
            process.run_process([binary], b"", 2)

    def test_deadline_kills_child_and_same_group_descendant(self):
        marker = self.root / "escaped"
        binary = self.script(f'''import os, pathlib, time
if os.fork() == 0:
    time.sleep(0.8)
    pathlib.Path({str(marker)!r}).write_text("unexpected")
    os._exit(0)
time.sleep(10)
''')
        start = time.monotonic()
        with self.assertRaisesRegex(RuntimeError, "deadline"):
            process.run_process([binary], b"", 0.15)
        self.assertLess(time.monotonic() - start, 1.5)
        time.sleep(0.85)
        self.assertFalse(marker.exists())

    def test_success_still_kills_remaining_same_group_descendant(self):
        marker = self.root / "escaped"
        binary = self.script(f'''import os, pathlib, time
if os.fork() == 0:
    time.sleep(0.7)
    pathlib.Path({str(marker)!r}).write_text("unexpected")
    os._exit(0)
''')
        process.run_process([binary], b"", 2)
        time.sleep(0.8)
        self.assertFalse(marker.exists())

    def test_file_resource_limit_is_enforced_by_real_child(self):
        binary = self.script(f'''import pathlib
pathlib.Path({str(self.root / "large")!r}).write_bytes(b"x" * (9 * 1024 * 1024))
''')
        with self.assertRaises(RuntimeError):
            process.run_process([binary], b"", 2)
        self.assertLessEqual((self.root / "large").stat().st_size, 8 * 1024 * 1024)

    def test_output_rejects_unsafe_files_without_blocking(self):
        regular = self.root / "regular"
        regular.write_text("valid")
        self.assertEqual(process.read_output(regular), "valid")
        paths = []
        for name, content in (("empty", b""), ("large", b"x" * (process.MAX_OUTPUT_BYTES + 1)), ("utf8", b"\xff")):
            path = self.root / name
            path.write_bytes(content)
            paths.append(path)
        link = self.root / "symlink"
        link.symlink_to(regular)
        paths.append(link)
        fifo = self.root / "fifo"
        os.mkfifo(fifo)
        paths += [fifo, self.root]
        hardlink = self.root / "hardlink"
        os.link(regular, hardlink)
        paths += [regular, hardlink]
        start = time.monotonic()
        for path in paths:
            with self.subTest(path=path.name), self.assertRaises((OSError, ValueError)):
                process.read_output(path)
        self.assertLess(time.monotonic() - start, 1)

    def test_version_fails_closed(self):
        binary = self.script('print("codex-cli 0.170.0")\n')
        with self.assertRaisesRegex(RuntimeError, "unsupported"):
            process.verify_binary(binary)

    def test_backend_replacement_at_same_path_fails_before_execution(self):
        marker = self.root / "ran-new-version"
        binary = self.script('print("codex-cli 0.169.0")\n')
        process.verify_binary(binary)
        self.script(f'import pathlib\npathlib.Path({str(marker)!r}).touch()\nprint("codex-cli 0.170.0")\n')
        with self.assertRaisesRegex(RuntimeError, "executable changed"):
            process.verify_binary(binary)
        self.assertFalse(marker.exists())

    def test_policy_digest_is_checked_before_staging(self):
        policy.policy_snapshot.cache_clear()
        self.addCleanup(policy.policy_snapshot.cache_clear)
        with patch.dict(policy.POLICY_HASHES, {"codex_completion_catalog.json": "0" * 64}):
            with self.assertRaisesRegex(RuntimeError, "digest mismatch"):
                policy.stage_policy(self.root)
        self.assertEqual(list(self.root.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
