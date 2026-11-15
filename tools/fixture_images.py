"""Resolve task-owned kind fixture images to real immutable containerd refs."""
import json
import re
import subprocess
import time


def register_digest(provider, node, tag):
    if provider not in ('docker', 'podman') or not re.fullmatch('[a-z0-9-]+-control-plane', node):
        raise ValueError('Expected a task-owned kind control plane')
    if not re.fullmatch(r'localhost/ai-sdlc-[a-z-]+:[a-zA-Z0-9_.-]+', tag):
        raise ValueError('Expected a local AI-SDLC fixture tag')
    prefix = [provider, 'exec', node]

    def inspect(reference, timeout=15):
        result = subprocess.run(prefix + ['crictl', 'inspecti', '-o', 'json', reference],
                                capture_output=True, check=True, timeout=timeout)
        return json.loads(result.stdout)['status']

    image = inspect(tag)
    digests = {value.rsplit('@', 1)[-1] for value in image['repoDigests']}
    if len(digests) != 1 or not re.fullmatch('sha256:[a-f0-9]{64}', next(iter(digests))):
        raise ValueError('Fixture image has no unambiguous manifest digest')
    reference = tag.rsplit(':', 1)[0] + '@' + next(iter(digests))
    def descriptor(name):
        result = subprocess.run(prefix + ['ctr', '-n', 'k8s.io', 'images', 'ls', 'name==' + name],
                                capture_output=True, check=True, timeout=15, text=True)
        rows = [line.split() for line in result.stdout.splitlines()[1:] if line.strip()]
        if not rows:
            return None
        if len(rows) != 1 or len(rows[0]) < 3 or rows[0][0] != name:
            raise ValueError('Ambiguous containerd fixture descriptor')
        return rows[0][2]

    expected = next(iter(digests))
    if descriptor(tag) != expected:
        raise ValueError('Loaded fixture tag descriptor disagrees with CRI digest')
    existing = descriptor(reference)
    if existing is not None and existing != expected:
        raise ValueError('Existing fixture alias points to a different descriptor')
    # Docker archive imports may list a generated RepoDigest without registering
    # that name for CRI lookup. Explicitly alias the existing content, never pull.
    if existing is None:
        result = subprocess.run(prefix + ['ctr', '-n', 'k8s.io', 'images', 'tag', '--local', tag, reference],
                                capture_output=True, timeout=15)
        if result.returncode:
            raise ValueError('Local fixture alias registration failed: ' + result.stderr.decode(errors='replace')[-1024:])
    if descriptor(reference) != expected:
        raise ValueError('Registered fixture alias descriptor mismatch')
    # ImageCreate is asynchronous with respect to the CRI image cache. Poll only
    # observation after successful registration; never retag or pull on failure.
    deadline = time.monotonic() + 10
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Fixture alias did not become visible in CRI before deadline')
        try:
            actual = inspect(reference, timeout=remaining)
        except subprocess.CalledProcessError as error:
            if time.monotonic() >= deadline:
                detail = (error.stderr or b'').decode(errors='replace')[-1024:]
                raise ValueError('Fixture alias did not become visible in CRI: ' + detail) from error
            time.sleep(0.1)
            continue
        if time.monotonic() >= deadline:
            raise ValueError('Fixture alias observation exceeded CRI deadline')
        if actual['id'] != image['id'] or reference not in actual['repoDigests']:
            raise ValueError('Registered fixture digest does not resolve to loaded image')
        return reference
