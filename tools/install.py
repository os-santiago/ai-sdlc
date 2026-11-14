#!/usr/bin/env python3
"""Install the committed Helm desired state; never advance delivery work."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
LOCAL = ROOT / 'runtime/k8s/deploy/local'
CHART = ROOT / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime'
COMPONENTS = ('admissionController', 'orchestrator', 'worker', 'releaseManager', 'relay')
NAMESPACE = 'ai-sdlc-runtime'


def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, text=True, **kwargs)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def tools(cache):
    if platform.system() != 'Linux' or platform.machine() not in ('x86_64', 'amd64'):
        raise ValueError('This release supports Linux amd64. Other platforms are not yet validated.')
    lock = json.loads((ROOT / 'tools/install-tools.lock.json').read_text())
    cache.mkdir(parents=True, exist_ok=True)
    binaries = {}
    for name, entry in lock.items():
        target = cache / (name + '-' + entry['version'])
        if not target.exists() or digest(target.read_bytes()) != entry['binarySha256']:
            print('Downloading verified ' + name + ' ' + entry['version'], file=sys.stderr, flush=True)
            with urllib.request.urlopen(entry['url'], timeout=120) as response:
                data = response.read()
            if digest(data) != entry['sha256']:
                raise ValueError('Download integrity check failed: ' + name)
            if entry.get('member'):
                with tempfile.TemporaryFile() as archive:
                    archive.write(data)
                    archive.seek(0)
                    with tarfile.open(fileobj=archive, mode='r:gz') as tar:
                        data = tar.extractfile(entry['member']).read()
            if digest(data) != entry['binarySha256']:
                raise ValueError('Binary integrity check failed: ' + name)
            with tempfile.NamedTemporaryFile(dir=cache, delete=False) as tmp:
                tmp.write(data)
                temporary = Path(tmp.name)
            temporary.chmod(0o755)
            temporary.replace(target)
        binaries[name] = str(target)
    return binaries


def image_lock(path):
    lock = json.loads(path.read_text())
    if lock.get('image', {}).get('repository') != 'quay.io/os-santiago/ai-sdlc':
        raise ValueError('Unexpected image repository in lock')
    if set(lock.get('components', {})) != set(COMPONENTS):
        raise ValueError('The image lock must contain exactly five runtime components')
    for name in COMPONENTS:
        if not re.fullmatch(r'sha256:[a-f0-9]{64}', lock['components'][name].get('digest', '')):
            raise ValueError('Missing published image digest: ' + name)
    return lock


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['plan', 'local', 'status', 'tools'])
    parser.add_argument('--cluster', default='ai-sdlc')
    parser.add_argument('--provider', choices=['auto', 'docker', 'podman'], default='auto')
    parser.add_argument('--images', type=Path, default=LOCAL / 'images.lock.json')
    parser.add_argument('--timeout', default='10m')
    args = parser.parse_args(argv)
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}', args.cluster):
        raise ValueError('Invalid cluster name')
    if not re.fullmatch(r'[1-9][0-9]*(s|m)', args.timeout):
        raise ValueError('Timeout must be positive seconds or minutes')
    cache = Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/tools'
    binaries = tools(cache)
    if args.command == 'tools':
        print(json.dumps(binaries, indent=2))
        return 0
    image_lock(args.images)
    helm_args = [binaries['helm'], 'template', 'ai-sdlc', CHART, '--namespace', NAMESPACE,
                 '-f', LOCAL / 'values.yaml', '-f', args.images]
    run(helm_args, stdout=subprocess.DEVNULL)
    if args.command == 'plan':
        print(json.dumps({'cluster': args.cluster, 'namespace': NAMESPACE,
                          'chart': str(CHART.relative_to(ROOT)), 'mode': 'maintenance',
                          'images': image_lock(args.images), 'deliveryEnabled': False}, indent=2))
        return 0
    provider = args.provider
    if provider == 'auto':
        provider = next((p for p in ('docker', 'podman') if shutil.which(p)), None)
    if not provider or not shutil.which(provider):
        raise ValueError('Install Docker or Podman, then rerun; the installer does not change host privileges.')
    run([provider, 'info'], stdout=subprocess.DEVNULL)
    env = dict(os.environ, KIND_EXPERIMENTAL_PROVIDER=provider)
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state')) / 'ai-sdlc' / args.cluster
    state.mkdir(parents=True, exist_ok=True, mode=0o700)
    kubeconfig = state / 'kubeconfig'
    kind = binaries['kind']
    clusters = run([kind, 'get', 'clusters'], env=env, capture_output=True).stdout.splitlines()
    marker = state / 'ownership.json'
    if args.cluster in clusters and not marker.exists():
        raise ValueError('Cluster already exists without this installer ownership record; choose a different --cluster.')
    if args.command == 'status' and args.cluster not in clusters:
        raise ValueError('The requested local cluster does not exist')
    if args.cluster not in clusters:
        marker.write_text(json.dumps({'cluster': args.cluster, 'provider': provider}) + '\n')
        run([kind, 'create', 'cluster', '--name', args.cluster, '--config', LOCAL / 'kind.yaml',
             '--kubeconfig', kubeconfig, '--wait', args.timeout], env=env)
    elif json.loads(marker.read_text()) != {'cluster': args.cluster, 'provider': provider}:
        raise ValueError('Cluster provider differs from its ownership record')
    run([kind, 'export', 'kubeconfig', '--name', args.cluster, '--kubeconfig', kubeconfig], env=env)
    kubeconfig.chmod(0o600)
    kubectl = [binaries['kubectl'], '--kubeconfig', kubeconfig, '--context', 'kind-' + args.cluster]
    if args.command == 'status':
        run(kubectl + ['get', 'deployments,statefulsets,pods,pvc', '-n', NAMESPACE])
        print('Installation mode: maintenance. Autonomous issue delivery: unavailable.')
        return 0
    run(kubectl + ['apply', '-f', LOCAL / 'namespace.yaml'])
    # Bootstrap secret boundary: values refer to a Secret, never its contents.
    # An existing Secret is reused unchanged, including on repeat installs.
    result = run(kubectl + ['get', 'secret', 'ai-sdlc-postgres-app', '-n', NAMESPACE,
                            '--ignore-not-found', '-o', 'name'], capture_output=True)
    if not result.stdout.strip():
        password = secrets.token_urlsafe(48)
        secret = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {
            'name': 'ai-sdlc-postgres-app', 'namespace': NAMESPACE,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc'}}, 'type': 'Opaque',
            'stringData': {'password': password,
                           'uri': 'postgresql://ai_sdlc:' + password + '@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable'}}
        run(kubectl + ['create', '-f', '-'], input=json.dumps(secret), stdout=subprocess.DEVNULL)
    # Helm owns all workload resources. No delivery component is called here.
    run([binaries['helm'], 'upgrade', '--install', 'ai-sdlc', CHART,
         '--kubeconfig', kubeconfig, '--kube-context', 'kind-' + args.cluster,
         '--namespace', NAMESPACE, '-f', LOCAL / 'values.yaml', '-f', args.images,
         '--wait', '--timeout', args.timeout])
    run(kubectl + ['rollout', 'status', 'statefulset/ai-sdlc-postgres', '-n', NAMESPACE,
                   '--timeout', args.timeout])
    for name in ('admission-controller', 'orchestrator', 'worker', 'release-manager', 'relay'):
        run(kubectl + ['rollout', 'status', 'deployment/ai-sdlc-' + name, '-n', NAMESPACE,
                       '--timeout', args.timeout])
    report = {'cluster': args.cluster, 'namespace': NAMESPACE, 'provider': provider,
              'mode': 'maintenance', 'installationReady': True, 'deliveryEnabled': False,
              'images': image_lock(args.images)}
    (state / 'installation.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        # Do not serialize process input (which can contain a Secret).
        print('Installation failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
