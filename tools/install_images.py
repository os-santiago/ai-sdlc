#!/usr/bin/env python3
"""Prepare reviewed installer image pins from a successful, signed main publication."""
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
# Keyless signer identity: only this repository's containers workflow, running
# on refs/heads/main for a push, signs runtime images (containers.yml `sign`).
WORKFLOW_REF = 'refs/heads/main'
CERTIFICATE_IDENTITY = 'https://github.com/' + REPOSITORY + '/.github/workflows/containers.yml@' + WORKFLOW_REF
CERTIFICATE_ISSUER = 'https://token.actions.githubusercontent.com'
# Signing cutover sentinel. A revision whose tree contains this file (read by
# commit, never by ref) publishes signed images and must verify. Only a
# revision without it predates signing and may be accepted unsigned.
CUTOVER_DIRECTORY = '.github'
CUTOVER_FILE = 'image-signing-cutover'
CUTOVER_PATH = CUTOVER_DIRECTORY + '/' + CUTOVER_FILE
OBJECT = re.compile('[a-f0-9]{40}')
VERIFY_SECONDS = 90
VERIFY_DEADLINE = 900


class SignatureError(ValueError):
    """A named, bounded signature failure for one component image."""

    def __init__(self, component):
        super().__init__(component)
        self.component = component


class ImageUnsigned(SignatureError):
    """The registry holds no signature for the digest."""


class ImageSignatureRejected(SignatureError):
    """A signature exists but does not verify for the expected identity, or
    verification could not complete (registry, Rekor or trust root failure)."""


class ImageVerificationTimeout(SignatureError):
    """Verification exceeded its per-image or overall time bound."""


class UnsignedLegacyNotEligible(ValueError):
    """--allow-unsigned-legacy was requested for a revision with the cutover sentinel."""


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


def select_artifacts(catalog, run_id, revision, include_build=False, include_release=False, include_browser=False):
    expected = ARTIFACT_COMPONENTS | ({'artifact-builder', 'build-controller'} if include_build else set()) | ({'deployer'} if include_release else set()) | ({'browser-verifier'} if include_browser else set())
    if include_release and not include_build: raise ValueError('Release images require complete build publication')
    if include_browser and not include_release: raise ValueError('Browser images require complete release publication')
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
        prefix = 'release-image-' if include_release and isinstance(name,str) and name.startswith('release-image-') else 'supply-chain-image-' if include_build and isinstance(name, str) and name.startswith('supply-chain-image-') else 'image-'
        if include_browser and isinstance(name, str) and name.startswith('browser-image-'):
            prefix = 'browser-image-'
        if not isinstance(name, str) or not name.startswith(prefix):
            continue
        component = name[len(prefix):]
        if (prefix == 'supply-chain-image-') != (component in ('artifact-builder', 'build-controller')) or (prefix=='release-image-') != (component=='deployer') or (prefix=='browser-image-') != (component=='browser-verifier'):
            raise ValueError('Unexpected component publication artifact class')
        origin = item.get('workflow_run', {})
        if not isinstance(origin, dict):
            raise ValueError('Invalid artifact workflow origin')
        if (component not in expected or component in selected
                or type(item.get('id')) is not int or item['id'] <= 0 or item['id'] in ids
                or item.get('expired') is not False or not isinstance(item.get('digest'), str)
                or not DIGEST.fullmatch(item['digest'])
                or type(item.get('size_in_bytes')) is not int or not 1 <= item['size_in_bytes'] <= 65536
                or origin.get('id') != run_id or origin.get('head_sha') != revision):
            raise ValueError('Artifact identity, scope or freshness is invalid')
        selected[component] = item
        ids.add(item['id'])
    if set(selected) != expected:
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


def verify_command(cosign, digest, revision):
    if not DIGEST.fullmatch(digest) or not SHA.fullmatch(revision):
        raise ValueError('Signature verification requires a digest and full source SHA')
    # The certificate must name this workflow on main, triggered by a push of
    # the exact revision. The image is addressed by digest only, never by tag.
    return [str(cosign), 'verify',
            '--certificate-identity', CERTIFICATE_IDENTITY,
            '--certificate-oidc-issuer', CERTIFICATE_ISSUER,
            '--certificate-github-workflow-repository', REPOSITORY,
            '--certificate-github-workflow-ref', WORKFLOW_REF,
            '--certificate-github-workflow-trigger', 'push',
            '--certificate-github-workflow-sha', revision,
            REGISTRY + '@' + digest]


class Cosign:
    def __init__(self, binary):
        self.binary = binary
        self.deadline = time.monotonic() + VERIFY_DEADLINE

    def verify(self, component, digest, revision):
        command = verify_command(self.binary, digest, revision)
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise ImageVerificationTimeout(component)
        with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
            try:
                result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=output, stderr=errors,
                                        timeout=min(VERIFY_SECONDS, remaining), check=False,
                                        preexec_fn=bound_output)
            except subprocess.TimeoutExpired:
                raise ImageVerificationTimeout(component) from None
            if result.returncode == 0 and output.tell() < MAX_RESPONSE:
                return
            # cosign has no machine-readable "unsigned" status; it is recognized
            # from cosign's message. Any other failure, including a changed
            # message, is a rejection: it fails closed and never becomes legacy.
            errors.seek(0)
            if result.returncode != 0 and b'no signatures found' in errors.read(MAX_RESPONSE):
                raise ImageUnsigned(component)
            raise ImageSignatureRejected(component)


def tree_entries(api, tree):
    if not isinstance(tree, str) or not OBJECT.fullmatch(tree):
        raise ValueError('Invalid Git tree identity')
    record = decode(api.get('git/trees/' + tree))
    if (not isinstance(record, dict) or record.get('sha') != tree or record.get('truncated') is not False
            or not isinstance(record.get('tree'), list)):
        raise ValueError('Unexpected or truncated Git tree record')
    entries = {}
    for entry in record['tree']:
        if not isinstance(entry, dict) or not isinstance(entry.get('path'), str) or entry['path'] in entries:
            raise ValueError('Invalid Git tree entry')
        entries[entry['path']] = entry
    return entries


def revision_signs(api, revision):
    """True when the exact commit carries the signing cutover sentinel."""
    if not SHA.fullmatch(revision):
        raise ValueError('Cutover lookup requires a full source SHA')
    # Address the commit object itself: a full SHA cannot be shadowed by a
    # branch or tag name, and the response must name that same commit.
    commit = decode(api.get('git/commits/' + revision))
    if not isinstance(commit, dict) or commit.get('sha') != revision or not isinstance(commit.get('tree'), dict):
        raise ValueError('Unexpected Git commit record')
    directory = tree_entries(api, commit['tree'].get('sha')).get(CUTOVER_DIRECTORY)
    if directory is None:
        return False
    if directory.get('type') != 'tree':
        raise ValueError('Unexpected cutover directory entry')
    sentinel = tree_entries(api, directory.get('sha')).get(CUTOVER_FILE)
    if sentinel is None:
        return False
    # Any entry at the sentinel path means the revision signs; an unexpected
    # entry type cannot make it legacy.
    return True


def verify_publication(verifier, identities, revision, allow_unsigned=False):
    """Verify every image signature; fail closed on the first failure.

    allow_unsigned is true only for a pre-cutover revision whose caller accepts
    legacy images. Present but invalid signatures are never accepted.
    """
    signatures = {}
    for component, identity in sorted(identities.items()):
        try:
            verifier.verify(component, identity['digest'], revision)
            signatures[component] = 'verified'
        except ImageUnsigned:
            if not allow_unsigned:
                raise
            signatures[component] = 'unsigned-legacy'
    return {'certificateIdentity': CERTIFICATE_IDENTITY, 'certificateIssuer': CERTIFICATE_ISSUER,
            'workflowTrigger': 'push', 'cutoverSentinel': CUTOVER_PATH,
            'legacyAccepted': allow_unsigned, 'images': signatures}


def signature_verifier(api, revision, allow_unsigned_legacy, refuse_ineligible=False, cosign=None):
    """Return a resolve() verify callback for one exact revision.

    Unsigned images are accepted only when allow_unsigned_legacy is set and the
    revision lacks the cutover sentinel. For a revision with the sentinel the
    callback verifies strictly, or raises UnsignedLegacyNotEligible when
    refuse_ineligible is set (the operator asked for legacy explicitly).
    cosign is fetched lazily from the pinned tool cache on first use.
    """
    def verify(identities):
        allow = False
        if allow_unsigned_legacy:
            signs = revision_signs(api, revision)
            if signs and refuse_ineligible:
                raise UnsignedLegacyNotEligible('The revision signs its images')
            allow = not signs
        verifier = cosign if cosign is not None else Cosign(pinned_cosign())
        return verify_publication(verifier, identities, revision, allow)
    return verify


def resolve(api, run_id, revision, include_build=False, include_release=False, include_browser=False, *, verify):
    """Resolve a publication. verify is required: a signature_verifier()
    callback, or False only where a caller documents why it cannot verify."""
    if verify is not False and not callable(verify):
        raise ValueError('resolve() requires a signature verifier or an explicit verify=False')
    run = decode(api.get('actions/runs/' + str(run_id)))
    validate_run(run, run_id, revision)
    artifacts = select_artifacts(decode(api.get('actions/runs/' + str(run_id) + '/artifacts?per_page=100')), run_id, revision, include_build, include_release, include_browser)
    identities = {}
    for component, artifact in sorted(artifacts.items()):
        identities[component] = image_identity(api.get('actions/artifacts/' + str(artifact['id']) + '/zip'), artifact, component, revision)
    signatures = verify(identities) if verify is not False else None
    lock = {'image': {'repository': REGISTRY, 'tag': revision},
            'components': {key: {'digest': identities[component]['digest']} for key, component in COMPONENTS.items()},
            'migrator': {'digest': identities['migrator']['digest']}}
    evidence = {'version': 1, 'repository': REPOSITORY, 'revision': revision, 'run': run_id,
                'runURL': 'https://github.com/' + REPOSITORY + '/actions/runs/' + str(run_id),
                'images': identities,
                'artifacts': {key: {'id': value['id'], 'digest': value['digest']} for key, value in artifacts.items()},
                'scope': 'GitHub publication identity only; registry pulls, signed provenance and deployment require separate validation.'}
    if signatures is not None:
        evidence['signatures'] = signatures
        evidence['scope'] = ('GitHub publication identity and keyless image signatures; registry pulls, '
                             'SBOM attestations, signed provenance and deployment require separate validation.')
    return lock, evidence


def pinned_cosign():
    # Same pinned, checksum-verified tool cache as `./install.sh tools`.
    import install
    return install.tools(install.tool_cache(), ('cosign',))['cosign']


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--include-release', action='store_true', help='Also require the exact-main deployer image; requires --include-build')
    parser.add_argument('--include-browser', action='store_true', help='Also require the exact-main browser image; requires --include-release')
    parser.add_argument('--include-build', action='store_true', help='Also require exact-main publisher and build-controller identities')
    parser.add_argument('--run', type=int, required=True, help='Successful containers workflow run on main')
    parser.add_argument('--revision', required=True, help='Exact reviewed source commit')
    parser.add_argument('--output', type=Path, required=True, help='New directory for image lock and evidence')
    parser.add_argument('--allow-unsigned-legacy', action='store_true',
                        help='Accept unsigned images only for a revision without the signing cutover sentinel ('
                             + CUTOVER_PATH + '); signatures that exist must still verify')
    args = parser.parse_args(argv)
    if args.run <= 0 or args.run > 2**63 - 1 or not SHA.fullmatch(args.revision):
        parser.error('A positive run ID and full lowercase source SHA are required')
    if args.output.exists():
        parser.error('Output directory must not already exist')
    try:
        verifier = Cosign(pinned_cosign())
        api = GitHub()
        verify = signature_verifier(api, args.revision, args.allow_unsigned_legacy, refuse_ineligible=True, cosign=verifier)
        lock, evidence = resolve(api, args.run, args.revision, args.include_build, args.include_release, args.include_browser,
                                 verify=verify)
        args.output.mkdir(parents=True, mode=0o700, exist_ok=False)
        (args.output / 'images.lock.json').write_text(json.dumps(lock, indent=2) + '\n')
        evidence_json = json.dumps(evidence, indent=2) + '\n'
        (args.output / 'evidence.json').write_text(evidence_json)
        # Execution installation consumes this sibling of images.lock.json.
        (args.output / 'images.evidence.json').write_text(evidence_json)
        if args.include_browser:
            browser={'version':'ai-sdlc.browser-images/v1','revision':args.revision,
                     'image':REGISTRY+'@'+evidence['images']['browser-verifier']['digest']}
            (args.output/'browser-images.json').write_text(json.dumps(browser,indent=2)+'\n')
        if args.include_release:
            release={'version':'ai-sdlc.release-images/v1','revision':args.revision,'deployerDigest':evidence['images']['deployer']['digest']}
            (args.output/'release-images.json').write_text(json.dumps(release,indent=2)+'\n')
        if args.include_build:
            build = {'version': 'ai-sdlc.build-images/v1', 'revision': args.revision,
                     'publisherImage': REGISTRY+'@'+evidence['images']['artifact-builder']['digest'],
                     'controllerDigest': evidence['images']['build-controller']['digest']}
            (args.output / 'build-images.json').write_text(json.dumps(build, indent=2)+'\n')
    except SignatureError as exc:
        # Component names come from the fixed publication set.
        parser.exit(1, 'Image signature verification failed: ' + type(exc).__name__ + ' (' + exc.component + ')\n')
    except (ValueError, OSError, subprocess.SubprocessError, zipfile.BadZipFile) as exc:
        parser.exit(1, 'Image resolution failed: ' + type(exc).__name__ + '\n')
    print('Review installer pins: ' + str(args.output / 'images.lock.json'))
    print('Publication evidence: ' + str(args.output / 'evidence.json'))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
