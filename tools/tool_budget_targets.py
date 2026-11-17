"""The reviewed 16-to-24 SCC tool-step change, without provider quota changes."""
import copy
import json

import initial_activation
from runtime_upgrade_pause import pause_values


def targets(source, render, compile_build, *, original=False):
    values = source['values']
    candidate = json.loads(values['candidate']['configJSON'])
    profile = candidate.get('profile', {})
    if (candidate.get('version') != 'ai-sdlc.candidate-runtime/v1'
            or profile.get('version') != 'ai-sdlc.executor-profile/v1'
            or profile.get('maxSteps') != 16 or profile.get('maxSeconds') != 600
            or profile.get('maxTokens') != 4096
            or candidate.get('model') != {'Limits': {'Calls': 16, 'InputBytes': 2097152,
                'OutputBytes': 4194304}, 'ResponseLimit': 262144}):
        raise ValueError('Original bounded 16-tool generator profile required')
    if render(values) != source['documents']:
        raise ValueError('Original tool-budget source render changed')
    paused = pause_values(values, compile_build)
    staged = copy.deepcopy(paused)
    changed = copy.deepcopy(candidate)
    changed['profile']['maxSteps'] = 16 if original else 24
    staged['candidate']['configJSON'] = json.dumps(changed, sort_keys=True)
    # Compare parsed configuration, without treating JSON whitespace as authority.
    restored = copy.deepcopy(staged)
    restored['candidate']['configJSON'] = paused['candidate']['configJSON']
    changed['profile']['maxSteps'] = 16
    if restored != paused or changed != candidate:
        raise ValueError('Tool budget changed unrelated configuration')
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
        raise ValueError('Final tool-budget activation must change only intake replicas')
    counterfactual = copy.deepcopy(result['active']['values'])
    counterfactual['candidate']['configJSON'] = values['candidate']['configJSON']
    if render(counterfactual) != source['documents']:
        raise ValueError('Tool-budget counterfactual differs from original resources')
    return result
