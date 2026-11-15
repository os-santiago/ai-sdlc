"""Explicit local GitHub client bootstrap. Never used by generated workloads."""
import json
import re
import subprocess
import urllib.error
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def validate_token(token):
    if not isinstance(token, str) or not re.fullmatch(r'[A-Za-z0-9_]{20,512}', token):
        raise ValueError('GitHub client credential has an invalid format')


def from_gh():
    # Keep stdout/stderr private even on authentication failures. The command
    # retrieves the active credential; it does not mint or narrow a new token.
    try:
        result = subprocess.run(['gh', 'auth', 'token', '--hostname', 'github.com'],
                                capture_output=True, text=True, timeout=20)
    except (OSError, subprocess.TimeoutExpired):
        raise ValueError('GitHub CLI credential lookup failed; authenticate gh first') from None
    if result.returncode:
        raise ValueError('GitHub CLI credential unavailable; authenticate gh first')
    token = result.stdout.strip()
    validate_token(token)
    return token


def request(token, path):
    opener = urllib.request.build_opener(NoRedirect())
    req = urllib.request.Request('https://api.github.com' + path, headers={
        'Authorization': 'Bearer ' + token,
        'Accept': 'application/vnd.github+json',
        'X-GitHub-Api-Version': '2022-11-28',
        'User-Agent': 'ai-sdlc-local-installer'})
    try:
        with opener.open(req, timeout=20) as response:
            raw = response.read(1024 * 1024 + 1)
        if len(raw) > 1024 * 1024:
            raise ValueError('response size')
        value = json.loads(raw)
        if not isinstance(value, dict):
            raise ValueError('response shape')
        return value
    except (OSError, ValueError):
        # API bodies, redirects and credentials never enter diagnostic output.
        raise ValueError('GitHub client validation failed; check authentication and repository access') from None


def inspect(token, repository):
    validate_token(token)
    if (not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository)
            or any(part in ('.', '..') for part in repository.split('/'))):
        raise ValueError('Invalid GitHub repository scope')
    account = request(token, '/user')
    repo = request(token, '/repos/' + repository)
    if (type(account.get('id')) is not int or account['id'] <= 0
            or not isinstance(account.get('login'), str) or not account['login']
            or repo.get('full_name', '').lower() != repository.lower()
            or repo.get('permissions', {}).get('push') is not True
            or repo.get('archived') is not False):
        raise ValueError('GitHub client must identify an account with write access to the configured repository')
    return {'version': 1, 'mode': 'gh-cli', 'host': 'github.com',
            'repository': repository, 'actorID': account['id'],
            'actorLogin': account['login'], 'token': token}


def configure(installer, kubectl, cluster_uid, repository, refresh=False, name='ai-sdlc-github-client'):
    if name not in ('ai-sdlc-github-client', 'ai-sdlc-publication-client'):
        raise ValueError('Unsupported GitHub credential destination')
    current = installer.owned_secret(kubectl, name, cluster_uid)
    if current is not None:
        if set(current) != {'credential.json'}:
            raise ValueError('Unexpected GitHub client Secret shape')
        try:
            credential = json.loads(current['credential.json'])
        except ValueError:
            raise ValueError('Invalid GitHub client Secret') from None
        if (not isinstance(credential, dict)
                or set(credential) != {'version', 'mode', 'host', 'repository', 'actorID', 'actorLogin', 'token'}
                or credential.get('version') != 1 or credential.get('mode') != 'gh-cli'
                or credential.get('host') != 'github.com' or credential.get('repository') != repository):
            raise ValueError('Existing GitHub client scope or mode differs')
        if not refresh:
            observed = inspect(credential['token'], repository)
            if observed != credential:
                raise ValueError('GitHub client identity changed; explicitly refresh the credential')
            return {'mode': 'gh-cli', 'repository': repository, 'credentialReady': True,
                    'accountWriteRoleVerified': True, 'writeCapabilitiesVerified': False}
    credential = inspect(from_gh(), repository)
    values = {'credential.json': json.dumps(credential, sort_keys=True)}
    if current is None:
        installer.create_owned_secret(kubectl, name, values, cluster_uid)
    else:
        resource = installer.read_resource(kubectl, 'secret', name)
        # Preserve resourceVersion for optimistic concurrency; never overwrite a
        # credential replaced or re-owned since the first read.
        if (resource.get('metadata', {}).get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid
                or installer.owned_secret(kubectl, name, cluster_uid) != current):
            raise ValueError('GitHub client Secret changed during refresh')
        resource.pop('data', None)
        resource['stringData'] = values
        installer.run(kubectl + ['replace', '-f', '-'], input=json.dumps(resource),
                      capture_output=True)
    return {'mode': 'gh-cli', 'repository': repository, 'credentialReady': True,
            'accountWriteRoleVerified': True, 'writeCapabilitiesVerified': False}
