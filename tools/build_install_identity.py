"""Bind a fixture executable to /component in an explicitly pinned local image."""
import hashlib
import json
from pathlib import Path
import re
import secrets
import subprocess
import tempfile


def verify(provider, image, binary):
    if provider not in ('podman', 'docker') or not re.fullmatch(r'sha256:[a-f0-9]{64}', image):
        raise ValueError('Pinned local installer image ID required')

    def call(argv, check=True):
        return subprocess.run([provider, *argv], capture_output=True, text=True,
                              check=check, timeout=30)

    config = json.loads(call(['image', 'inspect', image]).stdout)[0]
    if 'sha256:' + config.get('Id', '').removeprefix('sha256:') != image:
        raise ValueError('Installer image identity changed')
    nonce = secrets.token_hex(16)
    name = 'ai-sdlc-installer-identity-' + nonce
    cid = None
    try:
        cid = call(['create', '--name', name, '--network=none', '--read-only',
                    '--label', 'ai-sdlc.installer-identity=' + nonce, image]).stdout.strip()
        if not re.fullmatch(r'[a-f0-9]{64}', cid):
            cid = None
            raise ValueError('Original inspection container identity unavailable')
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-installer-image-') as directory:
            extracted = Path(directory) / 'component'
            call(['cp', cid + ':/component', str(extracted)])
            if extracted.is_symlink() or not extracted.is_file() or not 0 < extracted.stat().st_size < 128 << 20:
                raise ValueError('Bounded image component required')
            expected = hashlib.sha256(extracted.read_bytes()).hexdigest()
            if hashlib.sha256(binary.read_bytes()).hexdigest() != expected:
                raise ValueError('Installer executable differs from pinned image component')
            return {'imageID': image, 'binarySHA256': expected}
    finally:
        if cid is None:
            found = call(['inspect', name], check=False)
            if found.returncode == 0:
                candidate = json.loads(found.stdout)[0]
                if candidate.get('Config', {}).get('Labels', {}).get('ai-sdlc.installer-identity') == nonce:
                    cid = candidate.get('Id')
        if cid:
            actual = json.loads(call(['inspect', cid]).stdout)[0]
            if actual.get('Id') != cid or actual.get('Config', {}).get('Labels', {}).get('ai-sdlc.installer-identity') != nonce:
                raise ValueError('Inspection container custody changed')
            call(['rm', '-f', '-v', cid])
