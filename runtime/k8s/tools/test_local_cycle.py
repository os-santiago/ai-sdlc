import tempfile
import socket
import unittest
from pathlib import Path

from validate_local_cycle import DOC_PATH, EXPLANATION, run, verify_workspace, worker_environment, require_free_ports


class IndependentVerificationTests(unittest.TestCase):
    def test_worker_does_not_receive_ambient_credentials_or_checkout_settings(self):
        environment = {"AI_SDLC_GH_PAT": "sentinel-pat", "AI_SDLC_GH_APP_PRIVATE_KEY": "sentinel-key",
                       "AI_SDLC_GH_APP_ID": "123", "GH_TOKEN": "sentinel-gh", "GITHUB_TOKEN": "sentinel-github",
                       "AI_SDLC_BASE_REF": "unrelated", "SC_ROLE": "executor", "MODEL_API_KEY": "local-wrapper",
                       "AI_SDLC_WORKSPACE_DIR": "/workspaces/target"}
        forwarded = worker_environment(environment)
        self.assertEqual(forwarded, {"SC_ROLE": "executor", "MODEL_API_KEY": "local-wrapper", "AI_SDLC_WORKSPACE_DIR": "/workspaces/target"})
        self.assertEqual(environment["AI_SDLC_GH_PAT"], "sentinel-pat")

    def test_occupied_port_is_rejected_before_startup(self):
        with socket.socket() as occupied:
            occupied.bind(("127.0.0.1", 0))
            with self.assertRaises(RuntimeError):
                require_free_ports([occupied.getsockname()[1]])

    def test_valid_diff_and_rejects_staged_untracked_committed_or_unrelated_edits(self):
        for mutation in ("none", "staged", "untracked", "whitespace-name", "commit", "unrelated-text"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                target = Path(directory)
                git = ["git", "-C", directory]
                run(git + ["init", "-q"])
                run(git + ["config", "user.name", "Local test"])
                run(git + ["config", "user.email", "test@example.invalid"])
                doc = target / DOC_PATH
                doc.parent.mkdir(parents=True)
                baseline = "# Test\n\n```bash\ngo test ./... -v\n```\n"
                doc.write_text(baseline)
                run(git + ["add", "."])
                run(git + ["commit", "-qm", "baseline"])
                revision = run(git + ["rev-parse", "HEAD"])
                doc.write_text(baseline.replace("go test ./... -v\n```", "go test -p 1 ./... -v\n```\n\n" + EXPLANATION))
                if mutation in ("staged", "untracked", "whitespace-name"):
                    name = "   " if mutation == "whitespace-name" else "outside-scope.txt"
                    (target / name).write_text("unexpected")
                    if mutation == "staged":
                        run(git + ["add", "outside-scope.txt"])
                elif mutation == "commit":
                    run(git + ["add", "."])
                    run(git + ["commit", "-qm", "unexpected"])
                elif mutation == "unrelated-text":
                    doc.write_text(doc.read_text() + "Unrequested text.\n")
                if mutation == "none":
                    self.assertEqual(verify_workspace(target, revision, baseline), [DOC_PATH])
                else:
                    with self.assertRaises(RuntimeError):
                        verify_workspace(target, revision, baseline)


if __name__ == "__main__":
    unittest.main()
