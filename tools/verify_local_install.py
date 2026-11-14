#!/usr/bin/env python3
"""Verify Helm reapplication preserves local infrastructure, without work intake."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

import install


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', default='ai-sdlc')
    parser.add_argument('--provider', choices=['docker', 'podman'], required=True)
    args = parser.parse_args()
    # Verify ownership through the normal entrypoint before observing resources.
    install.main(['status', '--cluster', args.cluster, '--provider', args.provider])
    cache = Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/tools'
    tools = install.tools(cache)
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state')) / 'ai-sdlc' / args.cluster
    k = [tools['kubectl'], '--kubeconfig', str(state / 'kubeconfig'), '--context', 'kind-' + args.cluster,
         '-n', install.NAMESPACE]

    def read(*args):
        return subprocess.check_output(k + list(args), text=True).strip()

    def snapshot():
        secret = json.loads(read('get', 'secret', 'ai-sdlc-postgres-app', '-o', 'json'))
        claims = json.loads(read('get', 'pvc', '-o', 'json'))['items']
        pvc = next(p for p in claims if p['metadata']['name'] == 'data-ai-sdlc-postgres-0')
        migrations = read('exec', 'ai-sdlc-postgres-0', '--', 'psql', '-U', 'ai_sdlc', '-d', 'ai_sdlc', '-Atc',
                          'SELECT count(*) FROM schema_migrations;')
        assert int(migrations) > 0, 'Database migration evidence is missing'
        return {'secretUID': secret['metadata']['uid'],
                'secretDigest': hashlib.sha256(json.dumps(secret['data'], sort_keys=True).encode()).hexdigest(),
                'pvcUID': pvc['metadata']['uid'], 'volume': pvc['spec']['volumeName'],
                'migrations': migrations}

    config = json.loads(read('get', 'configmap', 'ai-sdlc-config', '-o', 'json'))
    assert config['data']['AI_SDLC_RUNTIME_MODE'] == 'maintenance'
    before = snapshot()
    install.main(['local', '--cluster', args.cluster, '--provider', args.provider])
    assert before == snapshot(), 'Reapplication changed credentials, persistent storage or migration state'
    deployments = json.loads(read('get', 'deployments', '-l', 'app.kubernetes.io/instance=ai-sdlc', '-o', 'json'))['items']
    assert len(deployments) == 5
    expected = set(v['digest'] for v in install.image_lock(install.LOCAL / 'images.lock.json')['components'].values())
    actual = set()
    for d in deployments:
        assert d['status']['readyReplicas'] == 1
        assert len(d['spec']['template']['spec']['containers']) == 1
        actual.add(d['spec']['template']['spec']['containers'][0]['image'].split('@')[1])
    assert actual == expected, 'Running desired images differ from the committed lock'
    worker_uid = read('exec', 'deployment/ai-sdlc-worker', '--', 'id', '-u')
    assert worker_uid == '1000'
    report = {'installation': 'passed', 'reapplication': 'passed', 'readyComponents': 5,
              'postgresReady': True, 'credentialsPreserved': True, 'persistentVolumePreserved': True,
              'migrationsPreserved': True, 'workerUID': 1000,
              'deliveryEnabled': False, 'provider': args.provider,
              'sourceRevision': install.image_lock(install.LOCAL / 'images.lock.json')['image']['tag']}
    (state / 'verification.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    try:
        main()
    except (AssertionError, OSError, ValueError, subprocess.CalledProcessError) as error:
        print('Installation verification failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
