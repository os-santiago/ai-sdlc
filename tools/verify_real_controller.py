#!/usr/bin/env python3
"""Opt-in actual Codex/SCC controller conformance in a disposable kind namespace."""
import argparse
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import time
import urllib.request

from verify_lifecycle_model import verify
from verify_workspace_bundle import source_bundle


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'worker-image', 'mailbox-image', 'test-binary',
                 'source-repository', 'source-sha'):
        parser.add_argument('--'+name, required=True)
    args = parser.parse_args()
    # Validate source before starting an authenticated child. verify() restricts
    # the database to the explicitly selected loopback ai_sdlc_test fixture.
    bundle = source_bundle(Path(args.source_repository), args.source_sha)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    token = secrets.token_urlsafe(48)
    wrapper = Path(__file__).resolve().parents[1]/'runtime/k8s/tools/codexcli_openai_wrapper.py'
    env = dict(os.environ, CODEX_WRAPPER_PORT=str(port), CODEX_WRAPPER_TOKEN=token)
    # The completion adapter's child allowlist excludes these ambient credentials.
    process = subprocess.Popen([sys.executable, str(wrapper)], env=env,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        deadline = time.monotonic()+15
        while True:
            if process.poll() is not None:
                raise RuntimeError('Owned completion adapter failed startup')
            try:
                with urllib.request.urlopen(f'http://127.0.0.1:{port}/healthz', timeout=1) as response:
                    if response.status == 200:
                        break
            except OSError:
                if time.monotonic() >= deadline:
                    raise RuntimeError('Owned completion adapter readiness deadline') from None
                time.sleep(.1)
        report = verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.worker_image,
                        args.mailbox_image, args.test_binary, bundle, 'real-model',
                        {'Endpoint': f'http://127.0.0.1:{port}/v1/chat/completions', 'Token': token})
    finally:
        process.send_signal(signal.SIGINT)
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
            raise RuntimeError('Owned completion adapter required forced termination') from None
    # Success is emitted only after controller and wrapper cleanup both completed.
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
