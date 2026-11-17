"""Closed schema-35 image/health delta, retaining existing delivery authority."""
import copy
import json

import initial_activation
import install_images
import prepared_resources
from runtime_upgrade_pause import pause_values


def validate_trust(original, approved, verifier):
    restored=copy.deepcopy(approved)
    if not isinstance(approved,dict) or set(approved)!=set(original):raise ValueError('Exact reviewed browser provenance document required')
    restored['verifierImage']=original['verifierImage']
    if restored!=original or approved['verifierImage']!=verifier or approved['verifierImage']==original['verifierImage']:
        raise ValueError('Browser trust may change only the selected verifier image')


def targets(source, selection, profile, approved_trust, render_source, render_target, compile_build):
    if render_source(source['values']) != source['documents']:
        raise ValueError('Original source chart or values changed')
    if 'browserProfileJSON' in source['values']['release']:
        raise ValueError('Browser upgrade requires the exact pre-browser source')
    images = selection['images']
    expected = install_images.ARTIFACT_COMPONENTS | {'artifact-builder','build-controller','deployer','browser-verifier'}
    if set(images) != expected or any(not install_images.DIGEST.fullmatch(x.get('digest','')) for x in images.values()):
        raise ValueError('Complete immutable schema-35 publication required')
    prefix = install_images.REGISTRY + '@'
    if profile.get('image') != prefix + images['browser-verifier']['digest']:
        raise ValueError('Browser profile must use the exact selected publication')
    values = source['values']
    paused = pause_values(values, compile_build)
    staged = copy.deepcopy(paused)
    lock = copy.deepcopy(source['images'])
    # All long-running database consumers must understand catalog35. Existing
    # isolated worker/mailbox/publisher jobs have no database access and retain
    # their already approved pins, budgets, policies and placement.
    for key, component in install_images.COMPONENTS.items():
        staged['components'][key]['digest'] = images[component]['digest']
        lock['components'][key]['digest'] = images[component]['digest']
    staged['migrator']['digest'] = lock['migrator']['digest'] = images['migrator']['digest']
    staged['verifier']['digest'] = images['verifier']['digest']
    staged['build']['digest'] = images['build-controller']['digest']
    staged['release']['digest'] = images['deployer']['digest']
    staged['release']['browserProfileJSON'] = json.dumps(profile,sort_keys=True)
    validate_trust(json.loads(staged['provenance']['trustJSON']),approved_trust,prefix+images['verifier']['digest'])
    trust=copy.deepcopy(approved_trust)
    staged['provenance']['trustJSON'] = json.dumps(trust,sort_keys=True)
    # tag remains the original configuration identity, while each changed
    # component's immutable digest and publication are recorded separately.
    def description(v, selected_lock, render):
        return {'values':v,'images':copy.deepcopy(selected_lock),'documents':render(v),
                'externalRuntime':copy.deepcopy(source['externalRuntime'])}
    result = {'source':copy.deepcopy(source),'paused':description(paused,source['images'],render_source),
              'staged':description(staged,lock,render_target)}
    for phase in ('warming','armed','active'):
        result[phase] = description(initial_activation.phase_values(staged,phase,compile_build),lock,render_target)
    # Counterfactual restores only declared configuration/image fields. This
    # rejects accidental credential, scope, key, quota, deadline or policy edits.
    restored = copy.deepcopy(result['active']['values'])
    for field in ('components','migrator','verifier','build','release','provenance'):
        restored[field] = copy.deepcopy(values[field])
    if restored != values:
        raise ValueError('Browser upgrade changed unrelated configuration')
    for field in ('approvedAt','expiresAt','publisherImage','registryCA'):
        if trust.get(field) != json.loads(values['provenance']['trustJSON']).get(field):
            raise ValueError('Browser upgrade renewed authority or replaced provenance custody')
    original = set(prepared_resources.documents(source['documents']))
    target = set(prepared_resources.documents(result['staged']['documents']))
    added = target-original
    permitted = {'v1/ConfigMap/ai-sdlc-runtime/ai-sdlc-browser-profile',
                 'rbac.authorization.k8s.io/v1/Role/ai-sdlc-browser/ai-sdlc-browser-verification',
                 'rbac.authorization.k8s.io/v1/RoleBinding/ai-sdlc-browser/ai-sdlc-browser-verification'}
    if original-target or added != permitted:
        raise ValueError('Unexpected browser Helm resource inventory delta')
    armed = copy.deepcopy(result['armed']['documents'])
    for obj in armed:
        if obj['kind']=='Deployment' and obj['metadata']['name']=='ai-sdlc-intake':obj['spec']['replicas']=1
    if armed != result['active']['documents']:
        raise ValueError('Final browser activation must change only intake replicas')
    return result
