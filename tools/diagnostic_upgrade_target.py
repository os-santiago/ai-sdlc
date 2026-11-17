"""Closed worker/gateway diagnostic delta; never a uniform runtime publication."""
import copy
import re

import initial_activation
import install_images
import prepared_resources

CHANGED_CONTAINERS = {('ai-sdlc-worker', 'worker'), ('ai-sdlc-worker', 'model-gateway'), ('ai-sdlc-verifier', 'model-gateway')}
MIGRATION_SHA256 = '574b526073715930603e7db7f47aaaeb55eb0c105f93e09d245d84146d7bfa68'


def image_delta(before, after, source_image, target_image):
    """Require precisely the three known worker-image consumers to change."""
    original = prepared_resources.documents(before)
    changed = prepared_resources.documents(copy.deepcopy(after))
    if set(original) != set(changed):
        raise ValueError('Diagnostic update cannot add or remove resources')
    found = set()
    for identity, target in changed.items():
        source = original[identity]
        if target.get('kind') == 'Deployment':
            name = target['metadata']['name']
            sources = {c['name']: c for c in source['spec']['template']['spec']['containers']}
            for container in target['spec']['template']['spec']['containers']:
                pair = (name, container['name'])
                if pair in CHANGED_CONTAINERS:
                    if sources.get(container['name'], {}).get('image') != source_image or container.get('image') != target_image:
                        raise ValueError('Original diagnostic image consumer changed')
                    found.add(pair)
                    container['image'] = source_image
        if target != source:
            raise ValueError('Diagnostic update changed authority or unrelated resources')
    if found != CHANGED_CONTAINERS or source_image == target_image:
        raise ValueError('Complete explicit worker and gateway image delta required')


def targets(source, plan, render, compile_build):
    """Derive from custodied paused values; preserve executor placement and trust."""
    values = source['values']
    if (values.get('runtime', {}).get('paused') is not True
            or values['runtime'].get('intakePaused') is not True
            or values.get('migrator', {}).get('enabled') is not False):
        raise ValueError('Original completed runtime pause required')
    if (len(plan['source']['migrations']) != 33 or len(plan['target']['migrations']) != 34
            or [m['filename'] for m in plan['pendingMigrations']] != ['0034_execution_failures.sql']
            or plan['pendingMigrations'][0].get('sha256') != MIGRATION_SHA256):
        raise ValueError('Only the reviewed additive diagnostic bridge is supported')
    images = copy.deepcopy(source['images'])
    if images['image']['repository'] != install_images.REGISTRY:
        raise ValueError('Original published registry required')
    for key, component in install_images.COMPONENTS.items():
        if images['components'][key]['digest'] != plan['source']['images'][component]['digest']:
            raise ValueError('Original component publication differs')
    if images['migrator']['digest'] != plan['source']['images']['migrator']['digest']:
        raise ValueError('Original migrator publication differs')
    target_values = copy.deepcopy(values)
    selected = {}
    for name in ('worker', 'migrator'):
        digest = plan['target']['images'][name]['digest']
        if not re.fullmatch(r'sha256:[a-f0-9]{64}', digest):
            raise ValueError('Immutable reviewed diagnostic image required')
        selected[name] = {'digest': digest, 'publication': plan['target']['publication'],
                          'revision': plan['target']['revision']}
        location = target_values['components']['worker'] if name == 'worker' else target_values['migrator']
        location['digest'] = digest
        location = images['components']['worker'] if name == 'worker' else images['migrator']
        location['digest'] = digest
    def describe(v):
        return {'images': copy.deepcopy(images), 'values': v, 'documents': render(v),
                'externalRuntime': copy.deepcopy(source['externalRuntime'])}
    result = {'staged': describe(target_values)}
    prefix = install_images.REGISTRY + '@'
    if render(values) != source['documents']:
        raise ValueError('Original reviewed paused render changed')
    image_delta(source['documents'], result['staged']['documents'],
                prefix + source['images']['components']['worker']['digest'], prefix + selected['worker']['digest'])
    for phase in ('warming', 'armed', 'active'):
        result[phase] = describe(initial_activation.phase_values(target_values, phase, compile_build))
    armed = copy.deepcopy(result['armed']['documents'])
    for document in armed:
        if document['kind'] == 'Deployment' and document['metadata']['name'] == 'ai-sdlc-intake':
            document['spec']['replicas'] = 1
    if armed != result['active']['documents']:
        raise ValueError('Final reactivation must change only intake replicas')
    return result, {'kind': 'explicit-component-selection', 'basePublication': plan['source']['publication'],
                    'baseRevision': plan['source']['revision'], 'selected': selected,
                    'changedContainers': [list(v) for v in sorted(CHANGED_CONTAINERS)],
                    'uniformTargetPublication': False}
