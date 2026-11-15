#!/usr/bin/env python3
"""Deterministic helpers retained from the retired direct-endpoint probe.

The CLI cannot launch services or supply workflow evidence. Autonomous work
must enter through issues; see runtime/k8s/README.md for the integration status.
"""
import socket
import subprocess

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



def main():
    raise SystemExit("Legacy direct-endpoint probe retired; use ./install.sh and issue-only intake. See runtime/k8s/README.md.")


if __name__ == "__main__":
    raise SystemExit(main())
