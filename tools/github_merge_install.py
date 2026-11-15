"""Install one additive protection rule. Existing rules are never overwritten."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import stat
import tempfile

import github_merge_policy as floor

ROOT = Path(__file__).resolve().parents[1]
BODY = json.loads((ROOT / 'runtime/k8s/deploy/github/homedir-merge-ruleset.json').read_text())
NAME = 'AI-SDLC low-risk merge protection'
BASE = 'repos/os-santiago/homedir/rulesets'
HASH = hashlib.sha256(json.dumps(BODY, sort_keys=True).encode()).hexdigest()


def validate(value):
    if (type(value.get('id')) is not int or value['id'] <= 0
            or value.get('name') != NAME or value.get('source') != floor.REPOSITORY
            or value.get('source_type') != 'Repository' or value.get('target') != 'branch'
            or value.get('enforcement') != 'active' or value.get('bypass_actors') != []
            or value.get('conditions') != BODY['conditions']):
        raise ValueError('Dedicated native merge protection has invalid scope')
    rules = value.get('rules', [])
    if not isinstance(rules, list) or len(rules) != 2 or {r.get('type') for r in rules} != {'pull_request', 'required_status_checks'}:
        raise ValueError('Dedicated merge protection rules changed')
    for rule in rules:
        p = rule.get('parameters', {})
        if rule['type'] == 'pull_request':
            if p.get('required_review_thread_resolution') is not True or type(p.get('required_approving_review_count')) is not int or p['required_approving_review_count'] != 0:
                raise ValueError('Dedicated native review policy changed')
        else:
            checks = p.get('required_status_checks', [])
            if (p.get('strict_required_status_checks_policy') is not True
                    or len(checks) != len(floor.REQUIRED)
                    or {(c.get('context'), c.get('integration_id')) for c in checks} != floor.REQUIRED):
                raise ValueError('Dedicated native check policy changed')
    return {'RulesetID': value['id'], 'Required': floor.MANIFEST['required'], 'Name': NAME}


def request(method, path, body=None):
    if not ((method == 'GET' and (path == BASE + '?per_page=100&includes_parents=true' or path.startswith(BASE + '/') and path.removeprefix(BASE + '/').isdigit()))
            or (method == 'POST' and path == BASE and body == BODY)):
        raise ValueError('Unsupported native protection operation')
    args = ['gh', 'api', '--hostname', 'github.com', '--include', '--method', method, path,
            '-H', 'X-GitHub-Api-Version: 2022-11-28']
    if body is not None:
        args += ['--input', '-']
    try:
        result = subprocess.run(args, input=None if body is None else json.dumps(body),
                                text=True, capture_output=True, timeout=20)
        if result.returncode or len(result.stdout) > 131072:
            raise ValueError('Native protection request failed; no write retry')
        headers, sep, raw = result.stdout.replace('\r\n', '\n').partition('\n\n')
        if not sep or any(line.lower().startswith('link:') for line in headers.splitlines()):
            raise ValueError('Native protection response is incomplete')
        return json.loads(raw)
    except (OSError, subprocess.TimeoutExpired, json.JSONDecodeError):
        raise ValueError('Native protection unavailable; no write retry') from None


def candidates():
    rows = request('GET', BASE + '?per_page=100&includes_parents=true')
    if not isinstance(rows, list) or len(rows) >= 100:
        raise ValueError('Native protection inventory unavailable')
    found = [r for r in rows if r.get('name') == NAME]
    if len(found) > 1:
        raise ValueError('Ambiguous dedicated native protection')
    return found


def _save(path, value):
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.merge-protection-')
    try:
        with os.fdopen(fd, 'w') as file:
            json.dump(value, file, sort_keys=True)
            file.flush()
            os.fsync(file.fileno())
        os.replace(name, path)
        fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def reconcile(state):
    """The local installer owns this lock and receipt; neither enters a workspace."""
    state = Path(state)
    if not state.is_absolute():
        raise ValueError('Native protection state must be absolute')
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    metadata = state.lstat()
    if not stat.S_ISDIR(metadata.st_mode) or metadata.st_uid != os.getuid() or metadata.st_mode & 0o022:
        raise ValueError('Native protection state must be an owned directory')
    # Persist every newly-created ancestor entry before any remote POST. Syncing
    # only the receipt's directory does not persist that directory in its parent.
    for directory in [state, *state.parents]:
        parent_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(parent_fd)
        finally:
            os.close(parent_fd)
    fd = os.open(state / 'merge-protection.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        metadata = os.fstat(fd)
        if not stat.S_ISREG(metadata.st_mode) or metadata.st_uid != os.getuid():
            raise ValueError('Native protection lock is not owned regular state')
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError('Native protection installation already running') from None
        return _reconcile(state / 'merge-protection.json')
    finally:
        os.close(fd)


def _reconcile(path):
    # Validate the reviewed manifest itself against the runtime floor before I/O.
    validate(dict(BODY, id=1, source=floor.REPOSITORY, source_type='Repository'))
    receipt = None
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        fd = None
    if fd is not None:
        try:
            metadata = os.fstat(fd)
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_uid != os.getuid() or metadata.st_size > 4096:
                raise ValueError('Invalid native protection receipt')
            raw = os.read(fd, 4097)
            if len(raw) > 4096:
                raise ValueError('Native protection receipt exceeds limit')
            receipt = json.loads(raw)
        finally:
            os.close(fd)
        if (set(receipt) != {'repository', 'manifestSHA256', 'state', 'id'}
                or receipt['repository'] != floor.REPOSITORY or receipt['manifestSHA256'] != HASH
                or receipt['state'] != 'ready' or type(receipt['id']) is not int or receipt['id'] <= 0):
            raise ValueError('Native protection installation is uncertain or has changed scope')
    matches = candidates()
    if receipt is not None and (len(matches) != 1 or matches[0].get('id') != receipt['id']):
        raise ValueError('Native protection identity changed')
    if matches:
        ruleset = request('GET', BASE + '/' + str(matches[0]['id']))
    else:
        # Durable pre-send intent: a lost response cannot trigger another POST.
        intent = {'repository': floor.REPOSITORY, 'manifestSHA256': HASH, 'state': 'sending', 'id': 0}
        _save(path, intent)
        ruleset = request('POST', BASE, BODY)
    policy = validate(ruleset)
    observed = request('GET', BASE + '/' + str(policy['RulesetID']))
    if validate(observed) != policy:
        raise ValueError('Native protection changed before activation')
    matches = candidates()
    if len(matches) != 1 or matches[0].get('id') != policy['RulesetID']:
        raise ValueError('Native protection identity became ambiguous')
    _save(path, {'repository': floor.REPOSITORY, 'manifestSHA256': HASH, 'state': 'ready', 'id': policy['RulesetID']})
    return policy
