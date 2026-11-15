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


def require(condition, message):
    if not condition:
        raise ValueError(message)


def verify_deployments(deployments, lock):
    names = {'admissionController': 'admission-controller', 'orchestrator': 'orchestrator',
             'worker': 'worker', 'releaseManager': 'release-manager', 'relay': 'relay'}
    expected = {'ai-sdlc-' + names[key]: lock['image']['repository'] + '@' + value['digest']
                for key, value in lock['components'].items()}
    require(len(deployments) == 5, 'Expected exactly five component Deployments')
    actual = {}
    for deployment in deployments:
        name = deployment['metadata']['name']
        require(deployment.get('status', {}).get('readyReplicas') == 1, 'Component is not ready: ' + name)
        containers = deployment['spec']['template']['spec']['containers']
        require(len(containers) == 1, 'Expected one container per component: ' + name)
        actual[name] = containers[0]['image']
    require(actual == expected, 'Component-to-image mapping differs from the committed lock')


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
        component_secrets = {}
        names = ['ai-sdlc-database-roles'] + [prefix + role for role in install.ROLES
                  for prefix in ('ai-sdlc-identity-', 'ai-sdlc-database-')]
        for name in names:
            resource = json.loads(read('get', 'secret', name, '-o', 'json'))
            component_secrets[name] = {'uid': resource['metadata']['uid'],
                'digest': hashlib.sha256(json.dumps(resource['data'], sort_keys=True).encode()).hexdigest()}
        authority = json.loads(read('get', 'configmap', 'ai-sdlc-authority', '-o', 'json'))
        authority_snapshot = {'uid': authority['metadata']['uid'], 'data': authority['data']}
        claims = json.loads(read('get', 'pvc', '-o', 'json'))['items']
        pvc = next(p for p in claims if p['metadata']['name'] == 'data-ai-sdlc-postgres-0')
        migrations = read('exec', 'ai-sdlc-postgres-0', '--', 'psql', '-U', 'ai_sdlc', '-d', 'ai_sdlc', '-Atc',
                          'SELECT count(*) FROM schema_migrations;')
        require(int(migrations) > 0, 'Database migration evidence is missing')
        return {'secretUID': secret['metadata']['uid'],
                'secretDigest': hashlib.sha256(json.dumps(secret['data'], sort_keys=True).encode()).hexdigest(),
                'componentSecrets': component_secrets, 'authority': authority_snapshot,
                'pvcUID': pvc['metadata']['uid'], 'volume': pvc['spec']['volumeName'],
                'migrations': migrations}

    config = json.loads(read('get', 'configmap', 'ai-sdlc-config', '-o', 'json'))
    require(config['data']['AI_SDLC_RUNTIME_MODE'] == 'maintenance', 'Verification requires maintenance mode')
    before = snapshot()
    install.main(['local', '--cluster', args.cluster, '--provider', args.provider])
    require(before == snapshot(), 'Reapplication changed credentials, persistent storage or migration state')
    deployments = json.loads(read('get', 'deployments', '-l', 'app.kubernetes.io/instance=ai-sdlc', '-o', 'json'))['items']
    verify_deployments(deployments, install.image_lock(install.LOCAL / 'images.lock.json'))
    worker_uid = read('exec', 'deployment/ai-sdlc-worker', '--', 'id', '-u')
    require(worker_uid == '1000', 'Worker must run with numeric UID 1000')
    report = {'installation': 'passed', 'reapplication': 'passed', 'readyComponents': 5,
              'postgresReady': True, 'credentialsPreserved': True, 'persistentVolumePreserved': True,
              'migrationsPreserved': True, 'authorityPreserved': True, 'workerUID': 1000,
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
