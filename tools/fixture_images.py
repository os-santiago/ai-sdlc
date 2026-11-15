"""Resolve task-owned kind fixture images to real immutable containerd refs."""
import json
import re
import subprocess


def register_digest(provider, node, tag):
    if provider not in ('docker', 'podman') or not re.fullmatch('[a-z0-9-]+-control-plane', node):
        raise ValueError('Expected a task-owned kind control plane')
    if not re.fullmatch(r'localhost/ai-sdlc-[a-z-]+:[a-zA-Z0-9_.-]+', tag):
        raise ValueError('Expected a local AI-SDLC fixture tag')
    prefix = [provider, 'exec', node]

    def inspect(reference):
        result = subprocess.run(prefix + ['crictl', 'inspecti', '-o', 'json', reference],
                                capture_output=True, check=True, timeout=15)
        return json.loads(result.stdout)['status']

    image = inspect(tag)
    digests = {value.rsplit('@', 1)[-1] for value in image['repoDigests']}
    if len(digests) != 1 or not re.fullmatch('sha256:[a-f0-9]{64}', next(iter(digests))):
        raise ValueError('Fixture image has no unambiguous manifest digest')
    reference = tag.rsplit(':', 1)[0] + '@' + next(iter(digests))
    # Docker archive imports may list a generated RepoDigest without registering
    # that name for CRI lookup. Explicitly alias the existing content, never pull.
    subprocess.run(prefix + ['ctr', '-n', 'k8s.io', 'images', 'tag', tag, reference],
                   capture_output=True, timeout=15)
    actual = inspect(reference)
    if actual['id'] != image['id'] or reference not in actual['repoDigests']:
        raise ValueError('Registered fixture digest does not resolve to loaded image')
    return reference
