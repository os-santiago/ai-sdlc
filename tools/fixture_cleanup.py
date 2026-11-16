"""Failure-preserving cleanup for explicitly isolated administrative fixtures."""
import json
import re


def recover_namespace_uid(call, namespace, label, nonce):
    """Recover only a cleanup identity after an attempted create; never resume work."""
    raw = call(['get', 'namespace', namespace, '--ignore-not-found', '-o', 'json'])
    if not raw.strip():
        return None
    metadata = json.loads(raw)['metadata']
    uid = metadata.get('uid', '')
    if (metadata.get('name') != namespace or metadata.get('labels', {}).get(label) != nonce
            or not re.fullmatch(r'[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}', uid)):
        raise ValueError('Refusing cleanup identity from an unrelated namespace')
    return uid


def finish_fixture(report, persist, cleanup):
    """Report write errors cannot suppress cleanup; cleanup errors remain visible."""
    write_errors = []

    def checkpoint():
        try:
            persist()
        except OSError as exc:
            write_errors.append(exc)
            report['status'] = 'failed'
            report['reportWriteFailure'] = type(exc).__name__

    checkpoint()
    try:
        cleanup(checkpoint)
    except BaseException as exc:
        report['status'] = 'failed'
        report['cleanupComplete'] = False
        report['cleanupFailure'] = type(exc).__name__
        raise
    finally:
        checkpoint()
    if write_errors:
        raise write_errors[0]


def create_global_resource(create, persist, intents, resource, value, namespace_uid, nonce):
    """Persist ownership intent before POST; ACK loss is cleanup-only recovery."""
    value = json.loads(json.dumps(value))
    metadata = value['metadata']
    metadata.setdefault('labels', {})['ai-sdlc.closed-build-fixture'] = nonce
    metadata.setdefault('annotations', {})['ai-sdlc.fixture-namespace-uid'] = namespace_uid
    intent = {'resource': resource, 'name': metadata['name'], 'apiVersion': value['apiVersion'],
              'kind': value['kind'], 'namespaceUID': namespace_uid, 'nonce': nonce, 'uid': None}
    intents.append(intent)
    persist()
    result = create(value)
    intent['uid'] = global_resource_uid(result, intent)
    persist()
    return result


def global_resource_uid(obj, intent):
    metadata = obj.get('metadata', {})
    uid = metadata.get('uid', '')
    if (obj.get('apiVersion') != intent['apiVersion'] or obj.get('kind') != intent['kind']
            or metadata.get('name') != intent['name'] or metadata.get('namespace')
            or metadata.get('labels', {}).get('ai-sdlc.closed-build-fixture') != intent['nonce']
            or metadata.get('annotations', {}).get('ai-sdlc.fixture-namespace-uid') != intent['namespaceUID']
            or not re.fullmatch(r'[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}', uid)
            or intent.get('uid') is not None and intent['uid'] != uid):
        raise ValueError('Refusing unrelated global fixture cleanup identity')
    return uid


def recover_global_resources(call, intents):
    """Resolve all uncertain creates before any storage cleanup, never resume work."""
    for intent in intents:
        raw = call(['get', intent['resource'], intent['name'], '--ignore-not-found', '-o', 'json'])
        if not raw.strip():
            intent['absent'] = True
            continue
        intent['uid'] = global_resource_uid(json.loads(raw), intent)
        intent['absent'] = False


_FIXTURE_KINDS = {'Namespace': ('namespace', False), 'PersistentVolume': ('pv', False),
                  'PersistentVolumeClaim': ('pvc', True)}


def create_slot_resource(create, persist, intents, value, cluster_uid, nonce):
    """Journal API-only slot fixture ownership before every possible create."""
    value = json.loads(json.dumps(value))
    resource, namespaced = _FIXTURE_KINDS[value['kind']]
    metadata = value['metadata']
    namespace = metadata.get('namespace')
    if value.get('apiVersion') != 'v1' or bool(namespace) != namespaced:
        raise ValueError('Unsupported slot fixture resource')
    metadata.setdefault('labels', {})['ai-sdlc.slot-api-fixture'] = nonce
    metadata.setdefault('annotations', {})['ai-sdlc.fixture-cluster-uid'] = cluster_uid
    intent = {'resource': resource, 'kind': value['kind'], 'name': metadata['name'],
              'namespace': namespace, 'clusterUID': cluster_uid, 'nonce': nonce, 'uid': None}
    intents.append(intent)
    persist()
    result = create(value)
    intent['uid'] = slot_resource_uid(result, intent)
    persist()
    return result


def slot_resource_uid(obj, intent):
    metadata = obj.get('metadata', {})
    uid = metadata.get('uid', '')
    if (obj.get('apiVersion') != 'v1' or obj.get('kind') != intent['kind']
            or metadata.get('name') != intent['name'] or metadata.get('namespace') != intent['namespace']
            or metadata.get('labels', {}).get('ai-sdlc.slot-api-fixture') != intent['nonce']
            or metadata.get('annotations', {}).get('ai-sdlc.fixture-cluster-uid') != intent['clusterUID']
            or not re.fullmatch(r'[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}', uid)
            or intent.get('uid') is not None and intent['uid'] != uid):
        raise ValueError('Refusing unrelated slot fixture cleanup identity')
    return uid


def recover_slot_resources(call, intents):
    for intent in intents:
        raw = call(['get', intent['resource'], intent['name']]
                   + (['-n', intent['namespace']] if intent['namespace'] else [])
                   + ['--ignore-not-found', '-o', 'json'])
        intent['absent'] = not raw.strip()
        if not intent['absent']:
            intent['uid'] = slot_resource_uid(json.loads(raw), intent)
