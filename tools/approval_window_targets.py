"""Explicit new approval timestamps; every other installed field stays fixed."""
import copy
import json
import ssl

import initial_activation
from runtime_upgrade_pause import pause_values


def registry_ca_window(trust, now):
    raw = trust.get('registryCA')
    if not isinstance(raw, str) or not 1 <= len(raw) <= 65536:
        raise ValueError('Original bounded registry CA required')
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.load_verify_locations(cadata=raw)
    certificates = context.get_ca_certs()
    if not 1 <= len(certificates) <= 4:
        raise ValueError('Original registry CA inventory required')
    for certificate in certificates:
        start = ssl.cert_time_to_seconds(certificate['notBefore'])
        end = ssl.cert_time_to_seconds(certificate['notAfter'])
        if (not start <= now < end or trust['approvedAt'] < start * 1000
                or trust['expiresAt'] > end * 1000):
            raise ValueError('New approval window exceeds the original registry CA validity')


def validate(original, approved, now):
    if not isinstance(approved, dict) or set(approved) != set(original):
        raise ValueError('New approval must preserve the original closed document')
    restored = copy.deepcopy(approved)
    for field in ('approvedAt', 'expiresAt'):
        if type(approved.get(field)) is not int or type(original.get(field)) is not int:
            raise ValueError('Explicit integer approval timestamps required')
        restored[field] = original[field]
    if restored != original:
        raise ValueError('Approval renewal cannot change identity, scope, keys or policy')
    if (not original['approvedAt'] < approved['approvedAt'] <= int(now * 1000)
            or approved['expiresAt'] <= max(original['expiresAt'], int(now * 1000))
            or not 0 < approved['expiresAt'] - approved['approvedAt'] <= 86400000):
        raise ValueError('A separately approved, current finite window is required')


def targets(source, approvals, render, compile_build, now):
    if set(approvals) != {'provenance', 'release'}:
        raise ValueError('Both independent approval documents are required')
    values = source['values']
    if render(values) != source['documents']:
        raise ValueError('Original approval source render changed')
    paused = pause_values(values, compile_build)
    staged = copy.deepcopy(paused)
    for section, key in (('provenance', 'trustJSON'), ('release', 'targetJSON')):
        original = json.loads(values[section][key])
        validate(original, approvals[section], now)
        staged[section][key] = json.dumps(approvals[section], sort_keys=True)
    def describe(v):
        return {'values': v, 'documents': render(v), 'images': copy.deepcopy(source['images']),
                'externalRuntime': copy.deepcopy(source['externalRuntime'])}
    result = {'source': copy.deepcopy(source), 'paused': describe(paused), 'staged': describe(staged)}
    for phase in ('warming', 'armed', 'active'):
        result[phase] = describe(initial_activation.phase_values(staged, phase, compile_build))
    restored = copy.deepcopy(result['active']['values'])
    for section, key in (('provenance', 'trustJSON'), ('release', 'targetJSON')):
        restored[section][key] = values[section][key]
    if render(restored) != source['documents']:
        raise ValueError('Approval counterfactual differs from the original installation')
    armed = copy.deepcopy(result['armed']['documents'])
    for obj in armed:
        if obj['kind'] == 'Deployment' and obj['metadata']['name'] == 'ai-sdlc-intake':
            obj['spec']['replicas'] = 1
    if armed != result['active']['documents']:
        raise ValueError('Final approval activation must change only intake replicas')
    return result
