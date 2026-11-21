"""Installer support for a reviewed per-repository project manifest.

The manifest is installation configuration (ai-sdlc.project/v1), never read
from the target repository. The pinned migrator compiles it, the authority
policy binds its digest, and the chart mounts it read-only.
"""
import copy
import json
from pathlib import Path

REFERENCES = ('os-santiago/homedir', 'os-santiago/saferus')
MAX_BYTES = 16384
MERGE_NAME = 'AI-SDLC low-risk merge protection'


def _unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('Duplicate project manifest field')
        result[key] = value
    return result


def load(path, repository):
    """Read a bounded manifest for exactly the installation repository."""
    with Path(path).open('rb') as source:
        raw = source.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        raise ValueError('Project manifest exceeds 16 KiB')
    try:
        manifest = json.loads(raw, object_pairs_hook=_unique)
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise ValueError('Invalid project manifest JSON') from exc
    if (not isinstance(manifest, dict) or manifest.get('version') != 'ai-sdlc.project/v1'
            or manifest.get('repository') != repository):
        raise ValueError('Project manifest must declare ai-sdlc.project/v1 for the installation repository')
    if repository in REFERENCES:
        raise ValueError('A reference repository uses its built-in manifest')
    if not isinstance(manifest.get('verification'), dict) or not isinstance(manifest.get('merge'), dict):
        raise ValueError('Project manifest requires verification and merge sections')
    return manifest


def reviewed(repository, manifest):
    """A repository may be published to when it is a reference or installed."""
    return repository in REFERENCES or (manifest is not None and manifest.get('repository') == repository)


def compile_installation(run, provider, image, policy, manifest):
    """Compile with the pinned migrator; it refuses references and mismatches."""
    result = run([provider, 'run', '--rm', '-i', '--network=none', '--log-driver=none', image,
                  'compile-project-installation'], input=json.dumps({'policy': policy, 'manifest': manifest}),
                 capture_output=True)
    if len(result.stdout) > 4 * MAX_BYTES:
        raise ValueError('Project installation output exceeded its bound')
    compiled = json.loads(result.stdout)
    if (not isinstance(compiled, dict) or compiled.get('version') != 'ai-sdlc.project-installation/v1'
            or not isinstance(compiled.get('manifestJSON'), str) or not isinstance(compiled.get('manifestSHA256'), str)):
        raise ValueError('Unexpected project installation output')
    return compiled


def bind(policy, compiled):
    """Bind the digest; an existing installation keeps its reviewed manifest."""
    current = policy.get('projectSHA256')
    if current not in (None, '', compiled['manifestSHA256']):
        raise ValueError('Installed authority binds a different project manifest; rotate it through reviewed configuration')
    policy['projectSHA256'] = compiled['manifestSHA256']
    values = {'manifestJSON': compiled['manifestJSON']}
    if compiled.get('verifierQuota'):
        values['verifierQuota'] = compiled['verifierQuota']
    return values


def merge_value(manifest):
    """The merge floor in the shape of the reviewed reference policies."""
    merge = manifest['merge']
    return {'repository': manifest['repository'], 'rulesetID': merge.get('rulesetID'),
            'required': [{'Context': c.get('context'), 'AppID': c.get('appID')} for c in merge.get('required', [])],
            'strictChecks': merge.get('strictChecks'), 'resolveThreads': merge.get('resolveThreads'),
            'humanApprovals': merge.get('humanApprovals'), 'bypassActors': []}


def ruleset_body(manifest):
    """The additive native protection rule the installer creates."""
    merge = manifest['merge']
    return copy.deepcopy({
        'name': MERGE_NAME, 'target': 'branch', 'enforcement': 'active', 'bypass_actors': [],
        'conditions': {'ref_name': {'include': ['refs/heads/' + manifest.get('baseBranch', 'main')], 'exclude': []}},
        'rules': [
            {'type': 'pull_request', 'parameters': {
                'required_approving_review_count': 0, 'dismiss_stale_reviews_on_push': False,
                'require_code_owner_review': False, 'require_last_push_approval': False,
                'required_review_thread_resolution': True, 'allowed_merge_methods': ['merge', 'squash', 'rebase']}},
            {'type': 'required_status_checks', 'parameters': {
                'strict_required_status_checks_policy': True, 'do_not_enforce_on_create': False,
                'required_status_checks': [{'context': c.get('context'), 'integration_id': c.get('appID')}
                                           for c in merge.get('required', [])]}}]})


def require_ruleset(manifest, native):
    """The manifest must name the ruleset the installer created or adopted."""
    if manifest['merge'].get('rulesetID') != native['RulesetID']:
        raise ValueError('Set merge.rulesetID to %d in the project manifest, then rerun the installation'
                         % native['RulesetID'])
