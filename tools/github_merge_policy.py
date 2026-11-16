"""Verify the reviewed Homedir native merge floor without changing remote policy."""
import copy
import json
import subprocess
from pathlib import Path

MANIFEST = json.loads((Path(__file__).resolve().parents[1] / 'runtime/k8s/internal/mergegate/homedir-policy.json').read_text())
REPOSITORY = MANIFEST['repository']
RULESET = MANIFEST['rulesetID']
REQUIRED = {(c['Context'], c['AppID']) for c in MANIFEST['required']}
if (REPOSITORY != 'os-santiago/homedir' or RULESET != 9071701
        or MANIFEST['strictChecks'] is not True or MANIFEST['resolveThreads'] is not True
        or MANIFEST['humanApprovals'] != 0 or MANIFEST['bypassActors'] != []):
    raise ValueError('Unsupported merge policy scope')


def manifest_for(repository):
    names = {'os-santiago/homedir': ('homedir', 9071701),
             'os-santiago/saferus': ('saferus', 27343088)}
    if repository not in names:
        raise ValueError('Unsupported reviewed repository')
    name, ruleset = names[repository]
    value = json.loads((Path(__file__).resolve().parents[1] / f'runtime/k8s/internal/mergegate/{name}-policy.json').read_text())
    if (value['repository'] != repository or value['rulesetID'] != ruleset
            or value['strictChecks'] is not True or value['resolveThreads'] is not True
            or value['humanApprovals'] != 0 or value['bypassActors'] != []):
        raise ValueError('Unsupported merge policy scope')
    return value


def desired(current):
    """Preserve all unrelated rules and fail closed on scope/approval drift."""
    if (current.get('id') != RULESET or current.get('source') != REPOSITORY
            or current.get('source_type') != 'Repository' or current.get('target') != 'branch'
            or current.get('enforcement') != 'active' or not isinstance(current.get('bypass_actors'), list)
            or current.get('conditions') != {'ref_name': {'include': ['~DEFAULT_BRANCH'], 'exclude': []}}):
        raise ValueError('Native merge policy scope is unavailable or changed')
    body = {k: copy.deepcopy(current[k]) for k in ('name', 'target', 'enforcement', 'bypass_actors', 'conditions', 'rules')}
    kinds = [r['type'] for r in body['rules']]
    if kinds.count('pull_request') != 1 or kinds.count('required_status_checks') != 1:
        raise ValueError('Native merge policy requires unique review and check rules')
    for rule in body['rules']:
        p = rule.get('parameters', {})
        if rule['type'] == 'pull_request':
            if p.get('required_approving_review_count') != 0:
                raise ValueError('Refusing to weaken a human approval requirement')
            p['required_review_thread_resolution'] = True
        if rule['type'] == 'required_status_checks':
            checks = p.get('required_status_checks', [])
            if not REQUIRED.issubset({(c.get('context'), c.get('integration_id')) for c in checks}):
                raise ValueError('Required native checks are missing or have a different owner')
            p['strict_required_status_checks_policy'] = True
    body['bypass_actors'] = []
    return body


def _api():
    # Authentication stays inside gh. Never include subprocess output in errors.
    args = ['gh', 'api', '--hostname', 'github.com', '--method', 'GET',
            f'repos/{REPOSITORY}/rulesets/{RULESET}', '-H', 'X-GitHub-Api-Version: 2022-11-28']
    try:
        result = subprocess.run(args, text=True, capture_output=True, timeout=20)
        if result.returncode or len(result.stdout) > 131072:
            raise ValueError('Native merge policy request failed')
        return json.loads(result.stdout)
    except (OSError, subprocess.TimeoutExpired, json.JSONDecodeError):
        raise ValueError('Native merge policy request unavailable') from None


def verify():
    current = _api()
    wanted = desired(current)
    if any(current.get(k) != v for k, v in wanted.items()):
        raise ValueError('Native merge protection must require strict checks, resolved threads and no bypass actors')
    return {'repository': REPOSITORY, 'ruleset': RULESET,
            'verified': True, 'strictChecks': True, 'bypassActors': 0}


if __name__ == '__main__':
    print(json.dumps(verify(), indent=2))
