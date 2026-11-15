#!/usr/bin/env python3
"""Prepare reviewed installer image pins from a successful main publication."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import re
import resource
import subprocess
import tempfile
import time
import zipfile

REPOSITORY = 'os-santiago/ai-sdlc'
REGISTRY = 'quay.io/os-santiago/ai-sdlc'
COMPONENTS = {'intake': 'intake', 'admissionController': 'admission-controller',
              'orchestrator': 'orchestrator', 'worker': 'worker',
              'releaseManager': 'release-manager', 'relay': 'relay'}
ARTIFACT_COMPONENTS = set(COMPONENTS.values()) | {'migrator', 'model-mailbox', 'verifier'}
SHA = re.compile('[a-f0-9]{40}')
DIGEST = re.compile('sha256:[a-f0-9]{64}')
MAX_RESPONSE = 1024 * 1024


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('Duplicate JSON field')
        result[key] = value
    return result


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique_object)


def validate_run(run, run_id, revision):
    if (not isinstance(run, dict) or run.get('id') != run_id
            or run.get('head_sha') != revision or run.get('head_branch') != 'main'
            or run.get('event') != 'push' or run.get('status') != 'completed'
            or run.get('conclusion') != 'success'
            or run.get('path') != '.github/workflows/containers.yml'
            or not isinstance(run.get('repository'), dict) or run['repository'].get('full_name') != REPOSITORY
            or not isinstance(run.get('head_repository'), dict) or run['head_repository'].get('full_name') != REPOSITORY):
        raise ValueError('Expected a successful exact-revision main container publication')


def select_artifacts(catalog, run_id, revision):
    if (not isinstance(catalog, dict) or type(catalog.get('total_count')) is not int
            or not isinstance(catalog.get('artifacts'), list)
            or not 1 <= catalog['total_count'] <= 100
            or len(catalog['artifacts']) != catalog['total_count']):
        raise ValueError('Incomplete or oversized publication artifact catalog')
    selected = {}
    ids = set()
    for item in catalog['artifacts']:
        if not isinstance(item, dict):
            raise ValueError('Invalid artifact record')
        name = item.get('name', '')
        if not isinstance(name, str) or not name.startswith('image-'):
            continue
        component = name[len('image-'):]
        origin = item.get('workflow_run', {})
        if not isinstance(origin, dict):
            raise ValueError('Invalid artifact workflow origin')
        if (component not in ARTIFACT_COMPONENTS or component in selected
                or type(item.get('id')) is not int or item['id'] <= 0 or item['id'] in ids
                or item.get('expired') is not False or not isinstance(item.get('digest'), str)
                or not DIGEST.fullmatch(item['digest'])
                or type(item.get('size_in_bytes')) is not int or not 1 <= item['size_in_bytes'] <= 65536
                or origin.get('id') != run_id or origin.get('head_sha') != revision):
            raise ValueError('Artifact identity, scope or freshness is invalid')
        selected[component] = item
        ids.add(item['id'])
    if set(selected) != ARTIFACT_COMPONENTS:
        raise ValueError('Publication lacks one or more component image identities')
    return selected


def image_identity(raw, metadata, component, revision):
    if (len(raw) != metadata['size_in_bytes']
            or 'sha256:' + hashlib.sha256(raw).hexdigest() != metadata['digest']):
        raise ValueError('Artifact archive digest or size mismatch')
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        entries = archive.infolist()
        if (len(entries) != 1 or entries[0].filename != component + '.json'
                or not 1 <= entries[0].file_size <= 4096 or entries[0].flag_bits & 1
                or entries[0].compress_type not in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED)):
            raise ValueError('Unexpected image identity archive layout')
        identity = decode(archive.read(entries[0]))
    if (not isinstance(identity, dict)
            or set(identity) != {'component', 'digest', 'repository', 'revision', 'platform'}
            or identity['component'] != component or identity['repository'] != REGISTRY
            or identity['revision'] != revision or identity['platform'] != 'linux/amd64'
            or not isinstance(identity['digest'], str) or not DIGEST.fullmatch(identity['digest'])):
        raise ValueError('Published image identity does not match the requested source')
    return identity


def bound_output():
    resource.setrlimit(resource.RLIMIT_FSIZE, (MAX_RESPONSE, MAX_RESPONSE))


class GitHub:
    def __init__(self):
        self.deadline = time.monotonic() + 180

    def get(self, path):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Image resolution deadline exceeded')
        # Fixed GitHub host and repository; archive locations are never accepted
        # from issue text, artifact content or operator-supplied URLs.
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(['gh', 'api', '--hostname', 'github.com',
                                     'repos/' + REPOSITORY + '/' + path],
                                    stdout=output, stderr=subprocess.DEVNULL,
                                    timeout=min(30, remaining), check=False, preexec_fn=bound_output)
            if result.returncode:
                raise ValueError('GitHub publication read failed')
            if output.tell() >= MAX_RESPONSE:
                raise ValueError('GitHub publication response exceeded its bound')
            output.seek(0)
            return output.read(MAX_RESPONSE + 1)


def resolve(api, run_id, revision):
    run = decode(api.get('actions/runs/' + str(run_id)))
    validate_run(run, run_id, revision)
    artifacts = select_artifacts(decode(api.get('actions/runs/' + str(run_id) + '/artifacts?per_page=100')), run_id, revision)
    identities = {}
    for component, artifact in sorted(artifacts.items()):
        identities[component] = image_identity(api.get('actions/artifacts/' + str(artifact['id']) + '/zip'), artifact, component, revision)
    lock = {'image': {'repository': REGISTRY, 'tag': revision},
            'components': {key: {'digest': identities[component]['digest']} for key, component in COMPONENTS.items()},
            'migrator': {'digest': identities['migrator']['digest']}}
    evidence = {'version': 1, 'repository': REPOSITORY, 'revision': revision, 'run': run_id,
                'runURL': 'https://github.com/' + REPOSITORY + '/actions/runs/' + str(run_id),
                'images': identities,
                'artifacts': {key: {'id': value['id'], 'digest': value['digest']} for key, value in artifacts.items()},
                'scope': 'GitHub publication identity only; registry pulls, signed provenance and deployment require separate validation.'}
    return lock, evidence


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', type=int, required=True, help='Successful containers workflow run on main')
    parser.add_argument('--revision', required=True, help='Exact reviewed source commit')
    parser.add_argument('--output', type=Path, required=True, help='New directory for image lock and evidence')
    args = parser.parse_args(argv)
    if args.run <= 0 or args.run > 2**63 - 1 or not SHA.fullmatch(args.revision):
        parser.error('A positive run ID and full lowercase source SHA are required')
    if args.output.exists():
        parser.error('Output directory must not already exist')
    try:
        lock, evidence = resolve(GitHub(), args.run, args.revision)
        args.output.mkdir(parents=True, mode=0o700, exist_ok=False)
        (args.output / 'images.lock.json').write_text(json.dumps(lock, indent=2) + '\n')
        (args.output / 'evidence.json').write_text(json.dumps(evidence, indent=2) + '\n')
    except (ValueError, OSError, subprocess.SubprocessError, zipfile.BadZipFile) as exc:
        parser.exit(1, 'Image resolution failed: ' + type(exc).__name__ + '\n')
    print('Review installer pins: ' + str(args.output / 'images.lock.json'))
    print('Publication evidence: ' + str(args.output / 'evidence.json'))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
