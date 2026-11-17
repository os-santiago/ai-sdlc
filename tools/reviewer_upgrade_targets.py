"""Change the reviewer worker image only, preserving all existing authority."""
import copy
import json
import re

import initial_activation
import install_images
from runtime_upgrade_pause import pause_values


def targets(source, image, render, compile_build):
    if not isinstance(image, str) or not re.fullmatch(re.escape(install_images.REGISTRY) + r'@sha256:[a-f0-9]{64}', image):
        raise ValueError('Published immutable reviewer worker image required')
    values = source['values']
    original = json.loads(values['review']['configJSON'])
    if original.get('workerImage') == image or not original.get('workerImage'):
        raise ValueError('A different reviewer worker image is required')
    if render(values) != source['documents']:
        raise ValueError('Original reviewer source render changed')
    paused = pause_values(values, compile_build)
    staged = copy.deepcopy(paused)
    changed = copy.deepcopy(original)
    changed['workerImage'] = image
    staged['review']['configJSON'] = json.dumps(changed, sort_keys=True)
    def describe(v):
        return {'values': v, 'documents': render(v), 'images': copy.deepcopy(source['images']),
                'externalRuntime': copy.deepcopy(source['externalRuntime'])}
    result = {'source': copy.deepcopy(source), 'paused': describe(paused), 'staged': describe(staged)}
    for phase in ('warming', 'armed', 'active'):
        result[phase] = describe(initial_activation.phase_values(staged, phase, compile_build))
    armed = copy.deepcopy(result['armed']['documents'])
    for document in armed:
        if document['kind'] == 'Deployment' and document['metadata']['name'] == 'ai-sdlc-intake':
            document['spec']['replicas'] = 1
    if armed != result['active']['documents']:
        raise ValueError('Final reviewer activation must change only intake replicas')
    restored = copy.deepcopy(result['active']['values'])
    restored['review']['configJSON'] = values['review']['configJSON']
    if render(restored) != source['documents']:
        raise ValueError('Reviewer update changed unrelated resources or authority')
    return result
