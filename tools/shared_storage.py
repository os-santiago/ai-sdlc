"""ReadWriteMany storage for a multi-node kind cluster, without NFS.

A cluster created with ``./install.sh local --workers N --shared-storage``
mounts one host directory (``<state>/shared``, mode 0700, owned by the
installing user) into every node at ``kind_nodes.SHARED_PATH``. kind's own
local-path provisioner serves it as the StorageClass ``ai-sdlc-shared``
through its ``storageClassConfigs`` ``sharedFileSystemPath`` setting: each
claim gets its own subdirectory, and its PersistentVolume has no node
affinity, so pods on any node can mount it (ReadWriteMany). The default
``standard`` class keeps node-local ReadWriteOnce volumes.

Only the CI caches use the class: the Tekton Maven caches (one claim per
repository) and the dependency work claims that ``dependency_cache_install``
seals for verification. Every guarantee of the node-local layout holds:
candidate code mounts caches read-only (CI admission policy), verification
mounts its sealed claim read-only, and only maven-dependencies, an
administrator-only pipeline on trusted inputs, writes a cache.
"""
import json

import kind_nodes

NAMESPACE = 'local-path-storage'
CONFIGMAP = 'local-path-config'
DEPLOYMENT = 'local-path-provisioner'
PROVISIONER = 'rancher.io/local-path'


def storage_class():
    return {'apiVersion': 'storage.k8s.io/v1', 'kind': 'StorageClass',
            'metadata': {'name': kind_nodes.SHARED_CLASS, 'labels': {'app.kubernetes.io/part-of': 'ai-sdlc'}},
            'provisioner': PROVISIONER, 'volumeBindingMode': 'Immediate', 'reclaimPolicy': 'Delete'}


def desired_config(current, local_classes=('standard',)):
    """kind's provisioner configuration plus the shared class entry; every
    other setting is kept. Once storageClassConfigs exists the provisioner
    refuses every class it does not list, so each existing local-path class
    (kind's standard) is listed with the node-local nodePathMap. A different
    entry for the shared class is refused."""
    config = json.loads(current)
    if not isinstance(config, dict) or 'nodePathMap' not in config:
        raise ValueError('Unexpected local-path provisioner configuration')
    classes = config.setdefault('storageClassConfigs', {})
    before = json.dumps(classes, sort_keys=True)
    for name in local_classes:
        if name != kind_nodes.SHARED_CLASS:
            classes.setdefault(name, {'nodePathMap': config['nodePathMap']})
    entry = {'sharedFileSystemPath': kind_nodes.SHARED_PATH}
    if classes.get(kind_nodes.SHARED_CLASS) not in (None, entry):
        raise ValueError('The local-path configuration already sets a different ai-sdlc-shared class')
    classes[kind_nodes.SHARED_CLASS] = entry
    changed = json.dumps(classes, sort_keys=True) != before
    return json.dumps(config, indent=2, sort_keys=True), changed


def configure(run, kubectl, timeout='5m'):
    """Serve the shared directory as the ReadWriteMany class, idempotently."""
    configmap = json.loads(run(kubectl + ['get', 'configmap', CONFIGMAP, '-n', NAMESPACE, '-o', 'json'],
                               capture_output=True).stdout)
    listed = json.loads(run(kubectl + ['get', 'storageclass', '-o', 'json'], capture_output=True).stdout or '{}')
    local = sorted(c['metadata']['name'] for c in listed.get('items', []) if c.get('provisioner') == PROVISIONER)
    desired, changed = desired_config(configmap['data']['config.json'], local or ('standard',))
    existing = run(kubectl + ['get', 'storageclass', kind_nodes.SHARED_CLASS, '--ignore-not-found', '-o', 'json'],
                   capture_output=True).stdout
    if existing.strip():
        found = json.loads(existing)
        if (found.get('provisioner') != PROVISIONER
                or found.get('metadata', {}).get('labels', {}).get('app.kubernetes.io/part-of') != 'ai-sdlc'):
            raise ValueError('An existing StorageClass ai-sdlc-shared is not this installation\'s')
    else:
        run(kubectl + ['create', '-f', '-'], input=json.dumps(storage_class()), capture_output=True)
    if changed:
        patch = {'data': {'config.json': desired}}
        run(kubectl + ['patch', 'configmap', CONFIGMAP, '-n', NAMESPACE, '--type', 'merge', '-p', json.dumps(patch)],
            capture_output=True)
        # The provisioner reads its configuration at start.
        run(kubectl + ['rollout', 'restart', 'deployment/' + DEPLOYMENT, '-n', NAMESPACE], capture_output=True)
    run(kubectl + ['rollout', 'status', 'deployment/' + DEPLOYMENT, '-n', NAMESPACE, '--timeout', timeout],
        capture_output=True)
    return {'storageClass': kind_nodes.SHARED_CLASS, 'path': kind_nodes.SHARED_PATH, 'accessMode': 'ReadWriteMany',
            'configured': changed}
