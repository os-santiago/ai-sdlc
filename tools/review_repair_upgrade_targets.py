"""Closed schema37 capability delta: one repair, original keys/budgets/scope.

The same operation installs a separately reviewed, finite approval window. A
schema36 source has no other renewal path once its earlier approvals expired.
"""
import copy
import json
import ssl

import approval_window_targets
import initial_activation
import install_images
import prepared_resources
from runtime_upgrade_pause import pause_values

GRANTS = [
    dict(caller='verifier', target='orchestrator', operation='repair-candidate', minimumLevel='auto-pr', eventType='CandidateRepairRequested'),
    dict(caller='verifier', target='orchestrator', operation='stop-rejected-work', minimumLevel='auto-pr', eventType='RejectedWorkStopped')]
COMPONENTS = {'admissionController': 'admission-controller', 'intake': 'intake', 'orchestrator': 'orchestrator',
              'relay': 'relay', 'releaseManager': 'release-manager', 'worker': 'worker'}


def chart_delta(source, target):
    """Normalize only the reviewed chart additions, with unchanged input values."""
    expected = copy.deepcopy(source)
    for obj in expected:
        name = obj['metadata']['name']
        if obj['kind'] == 'ConfigMap' and name == 'ai-sdlc-config':
            routes = json.loads(obj['data']['AI_SDLC_RELAY_ROUTES'])
            base = routes['plan-admission'].removesuffix('/plan-admission')
            for grant in GRANTS:
                if grant['operation'] in routes:
                    raise ValueError('Source chart already has repair routes')
                routes[grant['operation']] = base + '/' + grant['operation']
            obj['data']['AI_SDLC_RELAY_ROUTES'] = json.dumps(routes, sort_keys=True, separators=(',', ':'))
        if obj['kind'] == 'Deployment' and name == 'ai-sdlc-orchestrator':
            pod = obj['spec']['template']
            reviewer = next(d for d in source if d['kind'] == 'Deployment' and d['metadata']['name'] == 'ai-sdlc-release-manager')
            pod['metadata']['annotations']['checksum/review-config'] = reviewer['spec']['template']['metadata']['annotations']['checksum/review-config']
            pod['spec']['containers'][0]['volumeMounts'].insert(0, dict(name='review-config', mountPath='/etc/ai-sdlc/review', readOnly=True))
            # Position is chart-defined, but volume identity/content is unordered.
            pod['spec']['volumes'].append(dict(name='review-config', configMap=dict(name='ai-sdlc-review')))
    def canonical(documents):
        result = copy.deepcopy(documents)
        for obj in result:
            if obj['kind'] == 'Deployment':
                obj['spec']['template']['spec']['volumes'].sort(key=lambda x: x['name'])
        return prepared_resources.documents(result)
    if canonical(expected) != canonical(target):
        raise ValueError('Repair chart changed outside reviewed routes and public review mount')


def validate_provenance(original, approved, verifier, now):
    """Only the selected verifier image and a new finite window may change."""
    if not isinstance(approved, dict) or set(approved) != set(original):
        raise ValueError('Exact reviewed repair provenance document required')
    if approved.get('verifierImage') != verifier or approved['verifierImage'] == original.get('verifierImage'):
        raise ValueError('Repair trust must select the exact new verifier image')
    restored = dict(copy.deepcopy(approved), verifierImage=original['verifierImage'])
    approval_window_targets.validate(original, restored, now)


def tls_ca_window(raw, approved, now):
    """A rotated application CA must be current for the whole approved window."""
    if not isinstance(raw, str) or not 1 <= len(raw) <= 65536:
        raise ValueError('Bounded rotated release CA required')
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.load_verify_locations(cadata=raw)
    certificates = context.get_ca_certs()
    if len(certificates) != 1:
        raise ValueError('Exactly one rotated release CA certificate required')
    start = ssl.cert_time_to_seconds(certificates[0]['notBefore'])
    end = ssl.cert_time_to_seconds(certificates[0]['notAfter'])
    if not start <= now < end or approved['approvedAt'] < start * 1000 or approved['expiresAt'] > end * 1000:
        raise ValueError('Approved window exceeds the rotated release CA validity')


def validate_release(original, approved, now):
    """Only a new finite window and, after an operator rotation, tlsCA may change."""
    if not isinstance(approved, dict) or set(approved) != set(original):
        raise ValueError('Exact reviewed repair release target required')
    restored = dict(copy.deepcopy(approved), tlsCA=original['tlsCA'])
    approval_window_targets.validate(original, restored, now)
    if approved['tlsCA'] != original['tlsCA']:
        tls_ca_window(approved['tlsCA'], approved, now)


def targets(source, selection, approvals, render_source, render_target, compile_build, now):
    if not isinstance(approvals, dict) or set(approvals) != {'provenance', 'release'}:
        raise ValueError('Both independently reviewed approval documents are required')
    values = source['values']
    if render_source(values) != source['documents']:
        raise ValueError('Original installed feedback render changed')
    chart_delta(source['documents'], render_target(values))
    images = selection['images']
    expected = install_images.ARTIFACT_COMPONENTS | {'artifact-builder', 'build-controller', 'deployer', 'browser-verifier'}
    if set(images) != expected or any(not install_images.DIGEST.fullmatch(x.get('digest', '')) for x in images.values()):
        raise ValueError('Complete immutable schema37 publication required')
    active = copy.deepcopy(values)
    lock = copy.deepcopy(source['images'])
    for key, component in COMPONENTS.items():
        active['components'][key]['digest'] = lock['components'][key]['digest'] = images[component]['digest']
    active['migrator']['digest'] = lock['migrator']['digest'] = images['migrator']['digest']
    active['verifier']['digest'] = images['verifier']['digest']
    active['build']['digest'] = images['build-controller']['digest']
    active['release']['digest'] = images['deployer']['digest']
    worker = install_images.REGISTRY + '@' + images['worker']['digest']
    candidate = json.loads(values['candidate']['configJSON'])
    if candidate['profile'].get('maxRepairs', 0) != 0:
        raise ValueError('Source generator already enables repair')
    candidate['profile']['maxRepairs'] = 1
    candidate['placement']['WorkerImage'] = worker
    active['candidate']['configJSON'] = json.dumps(candidate, sort_keys=True)
    review = json.loads(values['review']['configJSON'])
    review['workerImage'] = worker
    active['review']['configJSON'] = json.dumps(review, sort_keys=True)
    inner = json.loads(values['authority']['policyJSON'])
    if any(g['operation'] in {x['operation'] for x in GRANTS} for g in inner['grants']):
        raise ValueError('Source policy already grants repair authority')
    inner['grants'] += copy.deepcopy(GRANTS)
    inner['grants'].sort(key=lambda x: (x['caller'], x['operation'], x['target']))
    active['authority']['policyJSON'] = json.dumps(inner, sort_keys=True)
    compiled = json.loads(values['build']['configJSON'])
    request = {k:copy.deepcopy(compiled[k]) for k in ('profile','slots','issueMaxAgeSeconds','outer')}
    request.update(inner=inner, intake=json.loads(values['intake']['configJSON']))
    changed = compile_build(request)
    if changed != dict(compiled, inner=inner):
        raise ValueError('Repair compiler changed original build authority')
    active['build']['configJSON'] = json.dumps(changed, sort_keys=True)
    validate_provenance(json.loads(values['provenance']['trustJSON']), approvals['provenance'],
                        install_images.REGISTRY + '@' + images['verifier']['digest'], now)
    validate_release(json.loads(values['release']['targetJSON']), approvals['release'], now)
    active['provenance']['trustJSON'] = json.dumps(approvals['provenance'], sort_keys=True)
    active['release']['targetJSON'] = json.dumps(approvals['release'], sort_keys=True)
    paused = pause_values(values, compile_build)
    staged = pause_values(active, compile_build)
    def description(v, selected, render):
        return dict(values=v, images=copy.deepcopy(selected), documents=render(v), externalRuntime=copy.deepcopy(source['externalRuntime']))
    result = {'source':copy.deepcopy(source), 'paused':description(paused, source['images'], render_source),
              'staged':description(staged, lock, render_target)}
    for phase in ('warming','armed','active'):
        result[phase] = description(initial_activation.phase_values(staged,phase,compile_build), lock, render_target)
    if result['active']['values'] != active:
        raise ValueError('Repair activation changed its reviewed target')
    inventory = set(prepared_resources.documents(source['documents']))
    if any(set(prepared_resources.documents(v['documents'])) != inventory for v in result.values()):
        raise ValueError('Repair installation cannot add or remove Helm resources')
    armed = copy.deepcopy(result['armed']['documents'])
    for obj in armed:
        if obj['kind'] == 'Deployment' and obj['metadata']['name'] == 'ai-sdlc-intake':
            obj['spec']['replicas'] = 1
    if armed != result['active']['documents']:
        raise ValueError('Repair activation must enable intake last')
    return result
