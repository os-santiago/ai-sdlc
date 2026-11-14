#!/usr/bin/env python3
"""Exercise the real local control plane, SCC, and Codex on a disposable clone.

No GitHub credentials, pushes, merges, tags, or production deployments are used.
Raw logs and model credentials stay in a private temporary directory. The JSON
report contains only bounded validation evidence. Requires Linux, Podman, Codex
authentication, and prebuilt component binaries / worker image (see README).
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

import codexcli_openai_wrapper as wrapper

DOC_PATH = "runtime/k8s/README.md"
EXPLANATION = "Use a fresh disposable PostgreSQL database for every run because fixtures share fixed identifiers."


def worker_environment(environment):
    # Never forward ambient GitHub/App credentials or unrelated runtime settings.
    allowed = (
        "DATABASE_URL", "MODEL_API_KEY", "SCC_BINARY", "SC_ROLE",
        "SC_PROVIDER_CONNECT_TIMEOUT_MS", "SC_PROVIDER_ATTEMPT_TIMEOUT_MS",
        "AI_SDLC_HTTP_ADDRESS", "AI_SDLC_HTTP_WRITE_TIMEOUT", "AI_SDLC_ENGINE",
        "AI_SDLC_WORKSPACE_DIR", "AI_SDLC_PROVIDER_BASE_URL", "AI_SDLC_MODEL",
        "AI_SDLC_ADEV_BUNDLE_PATH", "AI_SDLC_ADEV_BUNDLE_SHA256", "AI_SDLC_CONTRACT_PATH",
    )
    return {name: environment[name] for name in allowed if name in environment}


def require_free_ports(ports):
    reservations = []
    try:
        for port in ports:
            listener = socket.socket()
            reservations.append(listener)
            try:
                listener.bind(("127.0.0.1", port))
            except OSError as error:
                raise RuntimeError(f"Local port {port} is unavailable; select other probe ports") from error
    finally:
        for listener in reservations:
            listener.close()


def verify_workspace(target, revision, baseline):
    git = ["git", "-c", f"safe.directory={target}", "-C", str(target)]
    if run(git + ["rev-parse", "HEAD"]) != revision:
        raise RuntimeError("Target HEAD changed despite no-commit contract")
    tracked = run_raw(git + ["diff", "--name-only", "-z", "HEAD", "--"])
    untracked = run_raw(git + ["ls-files", "--others", "--exclude-standard", "-z"])
    changed = sorted(set(filter(None, (tracked + "\0" + untracked).split("\0"))))
    if changed != [DOC_PATH]:
        raise RuntimeError("Independent scope validation failed")
    expected = baseline.replace("go test ./... -v\n```", "go test -p 1 ./... -v\n```\n\n" + EXPLANATION, 1)
    if expected == baseline or (target / DOC_PATH).read_text() != expected:
        raise RuntimeError("Independent exact documentation validation failed")
    run(git + ["diff", "--check", "HEAD", "--"])
    return changed


def run(args, **kwargs):
    return run_raw(args, **kwargs).strip()


def run_raw(args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kwargs).stdout


def request(port, path, data=None):
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}{path}",
        data=None if data is None else json.dumps(data).encode(),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=10) as response:
        return json.load(response)


def wait_for(predicate, seconds, description):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if predicate():
                return
        except (OSError, subprocess.CalledProcessError, urllib.error.URLError):
            pass
        time.sleep(0.5)
    raise RuntimeError(f"Timed out waiting for {description}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binaries", type=Path, required=True)
    parser.add_argument("--worker-image", required=True)
    parser.add_argument("--adev-profile", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--target-ref", default="12d948c79102813c0db2acf63038df80e7c8991a",
                        help="Immutable baseline with the documentation defect; only the disposable clone uses it")
    parser.add_argument("--port-base", type=int, default=18080)
    parser.add_argument("--database-port", type=int, default=15432)
    parser.add_argument("--timeout", type=int, default=420)
    parser.add_argument("--scc-role", choices=["executor", "full"], default="executor",
                        help="Bounded executor plus independent validation, or SCC's full role pipeline")
    parser.add_argument("--diagnostics", action="store_true", help="Print redacted tool errors for this disposable public-repository probe")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[3]
    binary_dir = args.binaries.resolve()
    profile = args.adev_profile.resolve().read_bytes()
    components = ["admission-controller", "orchestrator", "worker", "release-manager", "relay"]
    for component in components:
        if not (binary_dir / component).is_file():
            parser.error(f"Missing compiled component: {component}")
    report = {"source_revision": run(["git", "-C", str(root), "rev-parse", "HEAD"]),
              "adev_sha256": hashlib.sha256(profile).hexdigest(),
              "status": "failed", "model_requests": 0, "proposed_tools": [],
              "scc_role": args.scc_role, "tool_error_categories": [],
              "publication": "not implemented by this runtime; not attempted"}
    report["source_diff_sha256"] = hashlib.sha256(run(["git", "-C", str(root), "diff", "HEAD"]).encode()).hexdigest()
    report["source_dirty"] = bool(run(["git", "-C", str(root), "status", "--porcelain"]))
    report["runner_sha256"] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    report["binary_sha256"] = {component: hashlib.sha256((binary_dir / component).read_bytes()).hexdigest() for component in components}
    report["worker_image_id"] = run(["podman", "image", "inspect", "--format", "{{.Id}}", args.worker_image])
    processes, logs = [], []
    suffix = secrets.token_hex(4)
    pg_name, worker_name = f"ai-sdlc-local-db-{suffix}", f"ai-sdlc-local-worker-{suffix}"
    server = None
    with tempfile.TemporaryDirectory(prefix="ai-sdlc-local-cycle-") as directory:
        state = Path(directory)
        workspace = state / "workspaces"
        try:
            require_free_ports([args.database_port, args.port_base+8] + [args.port_base+i for i in range(1, 6)])
            workspace.mkdir()
            target = workspace / "target"
            run(["git", "clone", "--quiet", "--no-hardlinks", str(root), str(target)])
            run(["git", "-C", str(target), "remote", "remove", "origin"])
            run(["git", "-C", str(target), "switch", "-c", "local-cycle-validation", args.target_ref])
            report["target_revision"] = run(["git", "-C", str(target), "rev-parse", "HEAD"])
            baseline_doc = (target / DOC_PATH).read_text()
            (workspace / "profile.yaml").write_bytes(profile)
            # Narrow local budget; the worker's policy permits one attempt.
            contract = "spec: '1.0.0'\nengine:\n  max_steps: 12\n  max_seconds: 300\nreview:\n  max_fix_iterations: 1\n"
            (workspace / "contract.yaml").write_text(contract)
            # Preserve the real CLI manifest before SCCEngine removes its attempt
            # directory. This observer neither changes arguments nor exit codes.
            observer = workspace / "observe-scc.sh"
            observer.write_text('''#!/bin/sh
summary=""
previous=""
for argument do
  if [ "$previous" = --summary-file ]; then summary="$argument"; fi
  previous="$argument"
done
/usr/local/bin/scc "$@"
status=$?
if [ -f "$summary" ]; then cp "$summary" /workspaces/observed-scc-manifest.json; fi
exit "$status"
''')
            observer.chmod(0o755)
            # Map the non-root container user in rootful and rootless Podman.
            ownership_prefix = [] if os.geteuid() == 0 else ["podman", "unshare"]
            run(ownership_prefix + ["chown", "-R", "1000:1000", str(workspace)])

            password, token = secrets.token_hex(24), secrets.token_hex(32)
            env = {key: value for key, value in os.environ.items() if not key.startswith(("AI_SDLC_", "SC_", "SCC_"))}
            env["POSTGRES_PASSWORD"] = password
            run(["podman", "run", "-d", "--name", pg_name, "-e", "POSTGRES_PASSWORD",
                 "-e", "POSTGRES_DB=cycle", "-p", f"127.0.0.1:{args.database_port}:5432",
                 "docker.io/library/postgres:17-alpine"], env=env)
            wait_for(lambda: run(["podman", "exec", pg_name, "pg_isready", "-h", "127.0.0.1", "-U", "postgres"]), 30, "PostgreSQL TCP readiness")
            env["DATABASE_URL"] = f"postgres://postgres:{password}@127.0.0.1:{args.database_port}/cycle?sslmode=disable"
            env["AI_SDLC_CONTRACT_PATH"] = str(workspace / "contract.yaml")
            env["AI_SDLC_HTTP_WRITE_TIMEOUT"] = "6m"
            env["AI_SDLC_RELAY_POLL_INTERVAL"] = "1s"
            base = args.port_base
            env["AI_SDLC_RELAY_ROUTES"] = json.dumps({
                "IssueAdmissionDecided.v1": f"http://127.0.0.1:{base+2}/v1/plans/events/issue-admission-decided",
                "WorkItemReady.v1": f"http://127.0.0.1:{base+3}/v1/attempts/dispatch",
                "ImplementationAttemptDecided.v1": f"http://127.0.0.1:{base+2}/v1/plans/events/implementation-attempt-decided",
                "ReleaseGateDecided.v1": f"http://127.0.0.1:{base+2}/v1/plans/events/release-gate-decided",
            })

            os.environ["CODEX_WRAPPER_TOKEN"] = token
            original_complete = wrapper.complete
            seen_errors = set()
            def measured_complete(payload):
                report["model_requests"] += 1
                print(f"Model request {report['model_requests']}", flush=True)
                for message in payload.get("messages", []):
                    content = message.get("content", "")
                    if args.diagnostics and message.get("role") == "tool" and isinstance(content, str) and content not in seen_errors:
                        seen_errors.add(content)
                        print("TOOL " + str(message.get("name", "")) + ": " + content.replace(token, "[REDACTED]").replace(password, "[REDACTED]")[:800], flush=True)
                    if message.get("role") != "tool" or not isinstance(content, str) or not content.startswith(("[ERROR]", "Error:")):
                        continue
                    category = "tool-execution"
                    for needle, label in [("not found", "command-or-file-not-found"), ("unified diff", "patch-format"), ("does not match", "patch-mismatch"), ("denied", "permission-denied")]:
                        if needle in content.lower():
                            category = label
                            break
                    if category not in report["tool_error_categories"]:
                        report["tool_error_categories"].append(category)
                    if args.diagnostics and content not in seen_errors:
                        seen_errors.add(content)
                        print(content.replace(token, "[REDACTED]").replace(password, "[REDACTED]")[:1500], flush=True)
                answer = original_complete(payload)
                report["proposed_tools"].extend(call["function"]["name"] for call in answer.get("tool_calls", []))
                return answer
            wrapper.complete = measured_complete
            server = wrapper.HTTPServer(("127.0.0.1", base+8), wrapper.Handler)
            threading.Thread(target=server.serve_forever, daemon=True).start()

            for index, component in enumerate(components, 1):
                component_env = dict(env, AI_SDLC_HTTP_ADDRESS=f"127.0.0.1:{base+index}")
                if component == "worker":
                    component_env.update(
                        AI_SDLC_ENGINE="scc", AI_SDLC_WORKSPACE_DIR="/workspaces/target",
                        MODEL_API_KEY=token, AI_SDLC_PROVIDER_BASE_URL=f"http://127.0.0.1:{base+8}/v1",
                        AI_SDLC_MODEL="codex-cli", AI_SDLC_ADEV_BUNDLE_PATH="/workspaces/profile.yaml",
                        AI_SDLC_ADEV_BUNDLE_SHA256=report["adev_sha256"],
                        AI_SDLC_CONTRACT_PATH="/workspaces/contract.yaml",
                        SC_PROVIDER_CONNECT_TIMEOUT_MS="110000", SC_PROVIDER_ATTEMPT_TIMEOUT_MS="120000",
                        SCC_BINARY="/workspaces/observe-scc.sh",
                    )
                    # Forward only explicit runtime configuration, never host credentials.
                    if args.scc_role == "executor":
                        component_env["SC_ROLE"] = "executor"
                    command = ["podman", "run", "--rm", "--name", worker_name, "--network", "host",
                               "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
                               "--user", "1000:1000", "-v", f"{workspace}:/workspaces:Z",
                               "-v", f"{binary_dir}:/local-bin:ro,Z", "--entrypoint", "/local-bin/worker"]
                    for name in worker_environment(component_env):
                        command += ["-e", name]
                    command += [args.worker_image]
                else:
                    command = [str(binary_dir / component)]
                log = (state / f"{component}.log").open("w")
                logs.append(log)
                processes.append(subprocess.Popen(command, env=component_env, stdout=log, stderr=log))
                wait_for(lambda p=base+index: request(p, "/healthz").get("status") == "healthy", 20, component)

            def sql(query):
                return run(["podman", "exec", pg_name, "psql", "-U", "postgres", "-d", "cycle", "-Atc", query])
            criteria = [
                "Only change runtime/k8s/README.md. In its PostgreSQL test example, replace go test ./... -v with go test -p 1 ./... -v.",
                "Immediately after the closing fence of that PostgreSQL shell example, insert one blank line and exactly this sentence: " + EXPLANATION,
                "Verify with git diff --check. Preserve all other text exactly. Do not commit, push, publish, install packages, or change any other file.",
            ]
            payload = {"correlationId": "local-cycle-validation", "issue": {
                "repository": "os-santiago/ai-sdlc", "number": 1, "revision": "local-validation",
                "title": "docs: clarify local PostgreSQL validation",
                "body": "The local runtime documentation should explain how to run persistence tests safely. This isolated validation changes only the existing build and test documentation and publishes nothing.",
                "acceptanceCriteria": criteria, "requestedBy": "local-validation", "authorizedBy": "operator",
            }}
            report["admission"] = request(base+1, "/v1/admissions/evaluate", payload)["decision"]
            if report["admission"] != "accepted":
                raise RuntimeError("Local work item was not admitted")
            if sql("SELECT count(*) FROM admissions") != "1":
                raise RuntimeError("Admission returned success without persistence")
            deadline = time.monotonic() + args.timeout
            while time.monotonic() < deadline:
                status = sql("SELECT status FROM work_items LIMIT 1")
                if status in ("completed", "blocked", "failed"):
                    report["work_item_status"] = status
                    break
                # Surface a broken wire early, before invoking a model repeatedly.
                if "unexpected status 400" in (state / "relay.log").read_text():
                    raise RuntimeError("Relay event rejected with HTTP 400; wire contract mismatch")
                time.sleep(1)
            else:
                raise RuntimeError("Cycle did not reach a terminal state within its budget")
            report["attempt_count"] = int(sql("SELECT count(*) FROM attempts"))
            report["attempts"] = json.loads(sql("SELECT coalesce(json_agg(json_build_object('exit_code', exit_code, 'validation_passed', validation_passed, 'changed_files', changed_files, 'outcome', outcome)), '[]') FROM attempts"))
            manifest = json.loads((workspace / "observed-scc-manifest.json").read_text())
            report["scc_manifest"] = {key: manifest.get(key) for key in ("version", "success", "exit_reason", "iterations", "tool_calls", "tool_calls_total", "files_changed", "resolution", "duration_ms")}
            target_git = ["git", "-c", f"safe.directory={target}", "-C", str(target)]
            report["observed_head_unchanged"] = run(target_git + ["rev-parse", "HEAD"]) == report["target_revision"]
            report["observed_tracked_changes"] = list(filter(None, run_raw(target_git + ["diff", "--name-only", "-z", "HEAD", "--"]).split("\0")))
            report["observed_untracked_changes"] = list(filter(None, run_raw(target_git + ["ls-files", "--others", "--exclude-standard", "-z"]).split("\0")))
            if report["work_item_status"] != "completed":
                raise RuntimeError("Worker did not complete the real SCC attempt")
            changed = verify_workspace(target, report["target_revision"], baseline_doc)
            expected_attempt = {"exit_code": 0, "validation_passed": True, "changed_files": changed, "outcome": "completed"}
            if report["attempt_count"] != 1 or report["attempts"] != [expected_attempt]:
                raise RuntimeError("Durable attempt evidence does not match independent verification")
            if not report["model_requests"] or not report["proposed_tools"]:
                raise RuntimeError("No real model/tool activity observed")
            report["independent_validation"] = "passed"
            report["changed_files"] = changed
            # A disposable local identifier, not a GitHub PR or a publication.
            snapshot = {"repository": "os-santiago/ai-sdlc", "pullRequest": 1, "correlationId": "local-cycle-validation",
                        "checks": [{"name": "local-scope-and-documentation", "required": True, "status": "completed", "conclusion": "success"}],
                        "issueCoveragePassed": True, "validationEvidencePresent": False}
            report["release_without_evidence"] = request(base+4, "/v1/releases/evaluate", snapshot)["state"]
            snapshot["validationEvidencePresent"] = True
            report["release_with_evidence"] = request(base+4, "/v1/releases/evaluate", snapshot)["state"]
            if report["release_without_evidence"] != "remediation-required" or report["release_with_evidence"] != "approved":
                raise RuntimeError("Release evaluator did not enforce evidence")
            wait_for(lambda: sql("SELECT count(*) FROM outbox WHERE published_at IS NULL") == "0", 15, "outbox drain")
            if sql("SELECT state FROM release_gates") != "approved":
                raise RuntimeError("Release decision was not persisted")
            report["outbox_drained"] = True
            report["release_decision_persisted"] = True
            report["status"] = "passed"
        except Exception as error:
            # Never copy subprocess commands/env or raw backend output into evidence.
            report["failure"] = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        finally:
            subprocess.run(["podman", "stop", "--time", "2", worker_name], capture_output=True)
            for process in reversed(processes):
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
            if server:
                server.shutdown()
                server.server_close()
            for log in logs:
                log.close()
            subprocess.run(["podman", "rm", "-f", pg_name, worker_name], capture_output=True)
            if os.geteuid() != 0 and workspace.exists():
                subprocess.run(["podman", "unshare", "chown", "-R", "0:0", str(workspace)], capture_output=True)
            args.report.parent.mkdir(parents=True, exist_ok=True)
            args.report.write_text(json.dumps(report, indent=2) + "\n")
            print(json.dumps(report, indent=2))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
