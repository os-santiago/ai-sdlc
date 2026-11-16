"""Candidate-only installation inputs; never emits a workflow command."""
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import secrets


def desired_values(root, image_lock_path, lock, verification=False, review=False, publication=False, merge=False):
    if merge and not publication:
        raise ValueError('Merge requires verified publication')
    if publication and (not review or not verification):
        raise ValueError('Publication requires verification and independent review')
    if review and not verification:
        raise ValueError('Independent review requires verification')
    evidence_path = Path(image_lock_path).with_name('images.evidence.json')
    with evidence_path.open('rb') as stream:
        raw = stream.read(65537)
    if len(raw) > 65536:
        raise ValueError('Execution image evidence exceeds bound')
    evidence = json.loads(raw)
    if evidence.get('revision') != lock['image']['tag'] or evidence.get('repository') != 'os-santiago/ai-sdlc':
        raise ValueError('Execution images require matching reviewed publication evidence')
    images = evidence.get('images', {})
    for name, digest in [('worker', lock['components']['worker']['digest']),
                         ('model-mailbox', images.get('model-mailbox', {}).get('digest'))]:
        item = images.get(name, {})
        if (item.get('revision') != lock['image']['tag'] or item.get('repository') != lock['image']['repository']
                or item.get('platform') != 'linux/amd64' or item.get('digest') != digest
                or not isinstance(digest, str) or not re.fullmatch(r'sha256:[a-f0-9]{64}', digest)):
            raise ValueError('Invalid execution image publication identity')
    adev = (root / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/files/adev-profile.yaml').read_text()
    profile = {'version': 'ai-sdlc.executor-profile/v1', 'model': 'codex-cli',
               'maxSteps': 16, 'maxSeconds': 600, 'maxTokens': 4096,
               'aDevSHA256': hashlib.sha256(adev.encode()).hexdigest()}
    config = {'version': 'ai-sdlc.candidate-runtime/v1', 'profile': profile, 'aDev': adev,
              'placement': {'Namespace': 'ai-sdlc-executors',
                            'WorkerImage': lock['image']['repository'] + '@' + images['worker']['digest'],
                            'MailboxImage': lock['image']['repository'] + '@' + images['model-mailbox']['digest']},
              'model': {'Limits': {'Calls': 16, 'InputBytes': 2097152, 'OutputBytes': 4194304},
                        'ResponseLimit': 262144}}
    result = {'candidate': {'configJSON': json.dumps(config, sort_keys=True), 'apiServerCIDRs': []}}
    if verification:
        item = images.get('verifier', {})
        value = item.get('digest')
        if (item.get('component') != 'verifier' or item.get('revision') != lock['image']['tag'] or item.get('repository') != lock['image']['repository']
                or item.get('platform') != 'linux/amd64' or not isinstance(value, str)
                or not re.fullmatch(r'sha256:[a-f0-9]{64}', value)):
            raise ValueError('Verification requires matching reviewed verifier image publication')
        result['verifier'] = {'digest': value}
    if review:
        review_profile = dict(profile, version='ai-sdlc.reviewer-profile/v1', maxSteps=8, maxSeconds=180)
        review_config = {'version': 'ai-sdlc.independent-review-runtime/v1', 'profile': review_profile,
                         'aDev': adev, 'workerImage': config['placement']['WorkerImage'],
                         'mailboxImage': config['placement']['MailboxImage']}
        result['review'] = {'enabled': True, 'configJSON': json.dumps(review_config, sort_keys=True)}
    if publication:
        result['publication'] = {'enabled': True}
    if merge:
        result['merge'] = {'enabled': True}
    return result


def model_credentials(kubectl, cluster_uid, auth_file, refresh=False, installer=None):
    # Imported here so the pure desired-state function has no installer effects.
    from install import owned_secret, create_owned_secret, run
    if installer is not None:
        owned_secret, create_owned_secret, run = installer.owned_secret, installer.create_owned_secret, installer.run
    current = owned_secret(kubectl, 'ai-sdlc-codex-auth', cluster_uid)
    if current is None or refresh:
        if auth_file is None:
            raise ValueError('Candidate installation needs --codex-auth-file or an existing owned model Secret')
        with Path(auth_file).open('rb') as stream:
            raw = stream.read(16385)
        if not 0 < len(raw) <= 16384 or not isinstance(json.loads(raw), dict):
            raise ValueError('Invalid model authentication document')
        if current is None:
            create_owned_secret(kubectl, 'ai-sdlc-codex-auth', {'auth.json': raw.decode()}, cluster_uid)
        else:
            # Explicit refresh affects credentials only, not workflow state.
            import subprocess
            run(kubectl + ['patch', 'secret', 'ai-sdlc-codex-auth', '-n', 'ai-sdlc-runtime', '--type=merge', '--patch-file=/dev/stdin'],
                input=json.dumps({'stringData': {'auth.json': raw.decode()}}), stdout=subprocess.DEVNULL)
    elif set(current) != {'auth.json'}:
        raise ValueError('Unexpected model authentication Secret shape')
    client = owned_secret(kubectl, 'ai-sdlc-model-client', cluster_uid)
    if client is None:
        create_owned_secret(kubectl, 'ai-sdlc-model-client', {'token': secrets.token_urlsafe(48)}, cluster_uid)
    elif set(client) != {'token'} or not re.fullmatch(r'[A-Za-z0-9_-]{32,128}', client['token']):
        raise ValueError('Invalid model client identity')

    # The public resource version changes the Pod template on explicit refresh.
    # A new Pod gets a new emptyDir, while CLI refreshes survive container restarts.
    resource = json.loads(run(kubectl + ["get", "secret", "ai-sdlc-codex-auth", "-n", "ai-sdlc-runtime", "-o", "json"], capture_output=True).stdout)
    revision = resource.get("metadata", {}).get("resourceVersion", "")
    if not re.fullmatch(r"[0-9]{1,64}", revision):
        raise ValueError("Invalid model credential resource version")
    return revision


def api_cidrs(kubectl):
    from install import run
    service = json.loads(run(kubectl + ['get', 'service', 'kubernetes', '-n', 'default', '-o', 'json'], capture_output=True).stdout)
    endpoints = json.loads(run(kubectl + ['get', 'endpoints', 'kubernetes', '-n', 'default', '-o', 'json'], capture_output=True).stdout)
    addresses = [service['spec']['clusterIP']] + [a['ip'] for s in endpoints.get('subsets', []) for a in s.get('addresses', [])]
    if not 2 <= len(addresses) <= 8:
        raise ValueError('Unsupported Kubernetes API endpoint inventory')
    return sorted({str(ipaddress.IPv4Address(value)) + '/32' for value in addresses})


def review_credentials(kubectl, cluster_uid, installer=None):
    from install import owned_secret, create_owned_secret
    if installer is not None:
        owned_secret, create_owned_secret = installer.owned_secret, installer.create_owned_secret
    current = owned_secret(kubectl, 'ai-sdlc-review-model-client', cluster_uid)
    if current is None:
        create_owned_secret(kubectl, 'ai-sdlc-review-model-client', {'token': secrets.token_urlsafe(48)}, cluster_uid)
    elif set(current) != {'token'} or not re.fullmatch(r'[A-Za-z0-9_-]{32,128}', current['token']):
        raise ValueError('Invalid independent review model client identity')
