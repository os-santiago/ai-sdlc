"""Strict, credential-free desired state for the supported local installer."""
import hashlib
import json
from pathlib import Path
import re


def _unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('Duplicate installation configuration field')
        result[key] = value
    return result


def load(path):
    path = Path(path).resolve()
    with path.open('rb') as source:
        raw = source.read(16385)
    if len(raw) > 16384:
        raise ValueError('Installation configuration exceeds 16 KiB')
    try:
        config = json.loads(raw, object_pairs_hook=_unique)
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise ValueError('Invalid installation configuration JSON') from exc
    fields = {'apiVersion', 'kind', 'cluster', 'provider', 'repository',
              'environment', 'profile', 'images'}
    if not isinstance(config, dict) or set(config) != fields:
        raise ValueError('Installation configuration requires exactly the documented fields')
    if any(not isinstance(value, str) for value in config.values()):
        raise ValueError('Installation configuration fields must be strings')
    if config['apiVersion'] != 'ai-sdlc.io/local-install/v1' or config['kind'] != 'LocalInstallation':
        raise ValueError('Unsupported installation configuration version or kind')
    if (not re.fullmatch(r'[a-z][a-z0-9-]{0,35}', config['cluster'])
            or config['provider'] not in ('auto', 'docker', 'podman')
            or not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', config['repository'])
            or any(part in ('.', '..') for part in config['repository'].split('/'))
            or not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,62}', config['environment'])
            or config['profile'] not in ('maintenance', 'shadow')
            or not config['images'] or '\x00' in config['images']):
        raise ValueError('Invalid or unsupported local installation scope')
    digest = 'sha256:' + hashlib.sha256(json.dumps(config, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
    values = {key: value for key, value in config.items() if key not in ('apiVersion', 'kind')}
    values['images'] = (path.parent / config['images']).resolve()
    return values, digest


def resolve(args):
    """Reject conflicting CLI scope before any external calls; never import auth."""
    if args.config is None:
        return None
    if args.command not in ('plan', 'local', 'status', 'credentials', 'sandbox'):
        raise ValueError('--config supports plan, local, status, credentials and sandbox only')
    values, digest = load(args.config)
    for key, value in values.items():
        supplied = getattr(args, key)
        if key == 'images' and supplied is not None:
            supplied = supplied.resolve()
        if supplied is not None and supplied != value:
            raise ValueError('CLI option conflicts with installation configuration: ' + key)
        # Status and credential operations address the same scope without
        # changing the installed profile or requesting shadow activation.
        if key != 'profile' or args.command in ('plan', 'local'):
            setattr(args, key, value)
    return digest
