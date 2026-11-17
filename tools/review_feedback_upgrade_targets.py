"""Closed verifier-only rollout; preserve every original delivery capability."""
import copy
import json

from browser_upgrade_targets import validate_trust
import initial_activation
import install_images
import prepared_resources
from runtime_upgrade_pause import pause_values


def targets(source, selection, approved_trust, render_source, render_target, compile_build):
    values = source['values']
    if (render_source(values) != source['documents'] or render_target(values) != source['documents']
            or not values['release'].get('browserProfileJSON')):
        raise ValueError('Original installed browser chart and profile required')
    images = selection['images']
    expected = install_images.ARTIFACT_COMPONENTS | {'artifact-builder', 'build-controller', 'deployer', 'browser-verifier'}
    if set(images) != expected or any(not install_images.DIGEST.fullmatch(x.get('digest', '')) for x in images.values()):
        raise ValueError('Complete immutable schema36 publication required')
    paused = pause_values(values, compile_build)
    staged = copy.deepcopy(paused)
    lock = copy.deepcopy(source['images'])
    staged['migrator']['digest'] = lock['migrator']['digest'] = images['migrator']['digest']
    staged['verifier']['digest'] = images['verifier']['digest']
    validate_trust(json.loads(values['provenance']['trustJSON']), approved_trust,
                   install_images.REGISTRY + '@' + images['verifier']['digest'])
    staged['provenance']['trustJSON'] = json.dumps(approved_trust, sort_keys=True)

    def description(v, selected, render):
        return {'values': v, 'images': copy.deepcopy(selected), 'documents': render(v),
                'externalRuntime': copy.deepcopy(source['externalRuntime'])}

    result = {'source': copy.deepcopy(source),
              'paused': description(paused, source['images'], render_source),
              'staged': description(staged, lock, render_target)}
    for phase in ('warming', 'armed', 'active'):
        result[phase] = description(initial_activation.phase_values(staged, phase, compile_build), lock, render_target)
    restored = copy.deepcopy(result['active']['values'])
    restored['migrator']['digest'] = values['migrator']['digest']
    restored['verifier']['digest'] = values['verifier']['digest']
    restored['provenance']['trustJSON'] = values['provenance']['trustJSON']
    if restored != values:
        raise ValueError('Feedback upgrade changed unrelated configuration')
    original = set(prepared_resources.documents(source['documents']))
    if any(set(prepared_resources.documents(x['documents'])) != original for x in result.values()):
        raise ValueError('Feedback upgrade cannot add or remove Helm resources')
    armed = copy.deepcopy(result['armed']['documents'])
    for obj in armed:
        if obj['kind'] == 'Deployment' and obj['metadata']['name'] == 'ai-sdlc-intake':
            obj['spec']['replicas'] = 1
    if armed != result['active']['documents']:
        raise ValueError('Final feedback activation must change only intake replicas')
    return result
