import copy
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
from unittest.mock import patch
import unittest
import zipfile

import install_images as images

SHA = 'a' * 40
RUN = 123


def run():
    return {'id': RUN, 'head_sha': SHA, 'head_branch': 'main', 'event': 'push',
            'status': 'completed', 'conclusion': 'success', 'path': '.github/workflows/containers.yml',
            'repository': {'full_name': images.REPOSITORY}, 'head_repository': {'full_name': images.REPOSITORY}}


def archive(component, *, changes=None, filename=None, extra=False, raw=None):
    identity = {'component': component, 'digest': 'sha256:' + 'b' * 64, 'repository': images.REGISTRY,
                'revision': SHA, 'platform': 'linux/amd64'}
    identity.update(changes or {})
    stream = io.BytesIO()
    with zipfile.ZipFile(stream, 'w', compression=zipfile.ZIP_DEFLATED) as z:
        z.writestr(filename or component + '.json', raw or json.dumps(identity))
        if extra:
            z.writestr('unexpected.json', '{}')
    data = stream.getvalue()
    metadata = {'id': list(sorted(images.ARTIFACT_COMPONENTS)).index(component) + 1,
                'name': 'image-' + component, 'digest': 'sha256:' + hashlib.sha256(data).hexdigest(),
                'size_in_bytes': len(data), 'expired': False, 'workflow_run': {'id': RUN, 'head_sha': SHA}}
    return data, metadata


class ImageResolutionTests(unittest.TestCase):
    def test_browser_publication_is_opt_in_and_preserves_original_twelve(self):
        catalog={'total_count':13,'artifacts':[archive(c)[1] for c in sorted(images.ARTIFACT_COMPONENTS)]}
        for index, component in enumerate(('artifact-builder','build-controller'),100):
            catalog['artifacts'].append(dict(archive('worker')[1],id=index,name='supply-chain-image-'+component))
        catalog['artifacts'].append(dict(archive('worker')[1],id=102,name='release-image-deployer'))
        catalog['artifacts'].append(dict(archive('worker')[1],id=103,name='browser-image-browser-verifier'))
        original=images.select_artifacts(catalog,RUN,SHA,True,True)
        self.assertEqual(set(original),images.ARTIFACT_COMPONENTS|{'artifact-builder','build-controller','deployer'})
        selected=images.select_artifacts(catalog,RUN,SHA,True,True,True)
        self.assertEqual(set(selected),set(original)|{'browser-verifier'})
        self.assertEqual({key:selected[key] for key in original},original)
        with self.assertRaises(ValueError):images.select_artifacts(catalog,RUN,SHA,True,False,True)
        for fault in ('missing','class','expired','revision','duplicate'):
            value=copy.deepcopy(catalog)
            if fault=='missing':value['artifacts'].pop();value['total_count']-=1
            elif fault=='class':value['artifacts'][-1]['name']='image-browser-verifier'
            elif fault=='expired':value['artifacts'][-1]['expired']=True
            elif fault=='revision':value['artifacts'][-1]['workflow_run']['head_sha']='f'*40
            else:value['artifacts'].append(dict(value['artifacts'][-1],id=104));value['total_count']+=1
            with self.subTest(fault=fault),self.assertRaises(ValueError):images.select_artifacts(value,RUN,SHA,True,True,True)

    def test_console_image_class_never_enters_an_installer_lock(self):
        # The optional read-only flow console publishes its own artifact class,
        # which no installer selection admits: every existing lock is unchanged.
        catalog={'total_count':len(images.ARTIFACT_COMPONENTS),'artifacts':[archive(c)[1] for c in sorted(images.ARTIFACT_COMPONENTS)]}
        for index, component in enumerate(('artifact-builder','build-controller'),100):
            catalog['artifacts'].append(dict(archive('worker')[1],id=index,name='supply-chain-image-'+component))
        catalog['artifacts'].append(dict(archive('worker')[1],id=102,name='release-image-deployer'))
        catalog['artifacts'].append(dict(archive('worker')[1],id=103,name='browser-image-browser-verifier'))
        catalog['total_count']=len(catalog['artifacts'])
        before={flags:images.select_artifacts(catalog,RUN,SHA,*flags) for flags in ((),(True,),(True,True),(True,True,True))}
        catalog['artifacts'].append(dict(archive('worker')[1],id=104,name='console-image-flow-console'))
        catalog['total_count']+=1
        for flags, selected in before.items():
            with self.subTest(flags=flags):
                self.assertEqual(images.select_artifacts(catalog,RUN,SHA,*flags),selected)
                self.assertNotIn('flow-console',selected)
        workflow=(Path(__file__).resolve().parents[1]/'.github/workflows/containers.yml').read_text()
        self.assertEqual(workflow.count("matrix.component == 'flow-console' && 'console-image'"),2)

    def test_rejects_unreviewed_or_failed_run_before_artifacts(self):
        cases = [{'event': 'pull_request'}, {'head_branch': 'build/unreviewed'}, {'head_sha': 'c' * 40},
                 {'conclusion': 'failure'}, {'status': 'in_progress'}, {'path': '.github/workflows/other.yml'},
                 {'head_repository': {'full_name': 'other/ai-sdlc'}}, {'repository': None}]
        for changes in cases:
            with self.subTest(changes=changes):
                value = run()
                value.update(changes)
                with self.assertRaises(ValueError):
                    images.validate_run(value, RUN, SHA)

    def test_requires_complete_unique_current_artifact_set(self):
        catalog = {'total_count': 9, 'artifacts': [archive(c)[1] for c in sorted(images.ARTIFACT_COMPONENTS)]}
        self.assertEqual(len(images.select_artifacts(catalog, RUN, SHA)), 9)
        cases = [lambda j: j['artifacts'].pop(),
                 lambda j: j['artifacts'].__setitem__(0, j['artifacts'][1]),
                 lambda j: j['artifacts'][0].update(expired=True),
                 lambda j: j['artifacts'][0].update(workflow_run={'id': RUN + 1, 'head_sha': SHA}),
                 lambda j: j['artifacts'][0].update(digest=''),
                 lambda j: j['artifacts'][0].update(size_in_bytes=65537)]
        for mutation in cases:
            value = copy.deepcopy(catalog)
            mutation(value)
            with self.assertRaises(ValueError):
                images.select_artifacts(value, RUN, SHA)

    def test_build_images_require_complete_matching_supplemental_artifact_class(self):
        catalog={'total_count':11,'artifacts':[archive(c)[1] for c in sorted(images.ARTIFACT_COMPONENTS)]}
        for index,component in enumerate(('artifact-builder','build-controller'),100):
            metadata=dict(archive('worker')[1],id=index,name='supply-chain-image-'+component)
            catalog['artifacts'].append(metadata)
        selected=images.select_artifacts(catalog,RUN,SHA,include_build=True)
        self.assertEqual(set(selected),images.ARTIFACT_COMPONENTS|{'artifact-builder','build-controller'})
        self.assertEqual(len(images.select_artifacts(catalog,RUN,SHA)),9)
        for fault in ('missing','class','revision'):
            changed=copy.deepcopy(catalog)
            if fault=='missing':changed['artifacts'].pop();changed['total_count']-=1
            elif fault=='class':changed['artifacts'][-1]['name']='image-build-controller'
            else:changed['artifacts'][-1]['workflow_run']['head_sha']='f'*40
            with self.assertRaises(ValueError):images.select_artifacts(changed,RUN,SHA,include_build=True)

    def test_release_images_are_explicit_complete_and_do_not_change_old_catalogs(self):
        catalog={'total_count':12,'artifacts':[archive(c)[1] for c in sorted(images.ARTIFACT_COMPONENTS)]}
        for index,component in enumerate(('artifact-builder','build-controller'),100):
            catalog['artifacts'].append(dict(archive('worker')[1],id=index,name='supply-chain-image-'+component))
        catalog['artifacts'].append(dict(archive('worker')[1],id=102,name='release-image-deployer'))
        self.assertEqual(set(images.select_artifacts(catalog,RUN,SHA,True,True)),images.ARTIFACT_COMPONENTS|{'artifact-builder','build-controller','deployer'})
        self.assertEqual(len(images.select_artifacts(catalog,RUN,SHA,True)),11)
        self.assertEqual(len(images.select_artifacts(catalog,RUN,SHA)),9)
        with self.assertRaises(ValueError):images.select_artifacts(catalog,RUN,SHA,False,True)
        for fault in ('missing','class','expired','revision'):
            value=copy.deepcopy(catalog)
            if fault=='missing':value['artifacts'].pop();value['total_count']-=1
            elif fault=='class':value['artifacts'][-1]['name']='image-deployer'
            elif fault=='expired':value['artifacts'][-1]['expired']=True
            else:value['artifacts'][-1]['workflow_run']['head_sha']='f'*40
            with self.assertRaises(ValueError):images.select_artifacts(value,RUN,SHA,True,True)

    def test_rejects_tampering_wrong_source_and_archive_paths(self):
        cases = [{'changes': {'revision': 'c' * 40}}, {'changes': {'repository': 'other/image'}},
                 {'changes': {'platform': 'linux/arm64'}}, {'changes': {'digest': 'mutable:tag'}},
                 {'filename': '../worker.json'}, {'extra': True},
                 {'raw': '{"component":"worker","component":"worker"}'}, {'raw': 'x' * 4097}]
        for options in cases:
            with self.subTest(options=options):
                data, metadata = archive('worker', **options)
                with self.assertRaises(ValueError):
                    images.image_identity(data, metadata, 'worker', SHA)
        data, metadata = archive('worker')
        with self.assertRaises(ValueError):
            images.image_identity(data + b'x', metadata, 'worker', SHA)

    def test_real_child_output_and_time_are_bounded(self):
        with tempfile.TemporaryDirectory() as directory:
            executable = Path(directory) / 'gh'
            executable.write_text('#!/usr/bin/env python3\nimport os\nos.write(1, b"x" * (2 * 1024 * 1024))\n')
            executable.chmod(0o700)
            with patch.dict(os.environ, {'PATH': directory + os.pathsep + os.environ['PATH']}):
                with self.assertRaises(ValueError):
                    images.GitHub().get('fixture')
                executable.write_text('#!/usr/bin/env python3\nimport time\ntime.sleep(5)\n')
                api = images.GitHub()
                api.deadline = time.monotonic() + 0.1
                started = time.monotonic()
                with self.assertRaises(subprocess.TimeoutExpired):
                    api.get('fixture')
                self.assertLess(time.monotonic() - started, 2)

    def test_cli_output_is_consumable_by_execution_installer(self):
        import candidate_install
        identities = {name: {'component': name, 'digest': 'sha256:'+'b'*64,
                            'repository': images.REGISTRY, 'revision': SHA,
                            'platform': 'linux/amd64'}
                      for name in images.ARTIFACT_COMPONENTS | {'artifact-builder','build-controller','deployer'}}
        lock = {'image': {'repository': images.REGISTRY, 'tag': SHA},
                'components': {key: {'digest': identities[name]['digest']} for key,name in images.COMPONENTS.items()},
                'migrator': {'digest': identities['migrator']['digest']}}
        evidence = {'revision': SHA, 'repository': images.REPOSITORY, 'images': identities}
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)/'publication'
            with patch.object(images, 'resolve', return_value=(lock,evidence)), patch.object(images, 'pinned_cosign', return_value='cosign'):
                self.assertEqual(images.main(['--include-build','--include-release','--run',str(RUN),'--revision',SHA,'--output',str(output)]),0)
            self.assertEqual(json.loads((output/'release-images.json').read_text()),{'version':'ai-sdlc.release-images/v1','revision':SHA,'deployerDigest':identities['deployer']['digest']})
            self.assertEqual((output/'evidence.json').read_bytes(),(output/'images.evidence.json').read_bytes())
            actual = candidate_install.desired_values(Path(__file__).resolve().parents[1],output/'images.lock.json',lock,
                                                     verification=True,review=True,publication=True,merge=True)
            self.assertTrue(actual['merge']['enabled'])
            self.assertEqual(actual['verifier']['digest'],identities['verifier']['digest'])
            self.assertEqual(json.loads((output/'build-images.json').read_text())['controllerDigest'],identities['build-controller']['digest'])

    def test_resolves_exact_publication_into_installer_shape(self):
        responses = {'actions/runs/123': json.dumps(run()).encode()}
        catalog = {'total_count': 9, 'artifacts': []}
        for component in sorted(images.ARTIFACT_COMPONENTS):
            data, metadata = archive(component)
            catalog['artifacts'].append(metadata)
            responses['actions/artifacts/' + str(metadata['id']) + '/zip'] = data
        responses['actions/runs/123/artifacts?per_page=100'] = json.dumps(catalog).encode()
        class API:
            def get(self, path):
                return responses[path]
        lock, evidence = images.resolve(API(), RUN, SHA, verify=False)
        self.assertEqual(set(lock), {'image', 'components', 'migrator'})
        self.assertEqual(set(lock['components']), set(images.COMPONENTS))
        self.assertEqual(lock['image']['tag'], SHA)
        self.assertEqual(set(evidence['images']), images.ARTIFACT_COMPONENTS)
        self.assertIn('separate validation', evidence['scope'])



DIGEST_B = 'sha256:' + 'b' * 64


def identities(*components):
    return {name: {'component': name, 'digest': DIGEST_B, 'repository': images.REGISTRY,
                   'revision': SHA, 'platform': 'linux/amd64'} for name in components}


def git_responses(revision, sentinel=True):
    """GitHub git-object responses for a commit with or without the cutover sentinel."""
    root, github, blob = 'c' * 40, 'd' * 40, 'e' * 40
    entries = [{'path': 'workflows', 'mode': '040000', 'type': 'tree', 'sha': 'f' * 40}]
    if sentinel:
        entries.append({'path': images.CUTOVER_FILE, 'mode': '100644', 'type': 'blob', 'sha': blob})
    return {'git/commits/' + revision: json.dumps({'sha': revision, 'tree': {'sha': root}}).encode(),
            'git/trees/' + root: json.dumps({'sha': root, 'truncated': False, 'tree': [
                {'path': '.github', 'mode': '040000', 'type': 'tree', 'sha': github},
                {'path': 'README.md', 'mode': '100644', 'type': 'blob', 'sha': blob}]}).encode(),
            'git/trees/' + github: json.dumps({'sha': github, 'truncated': False, 'tree': entries}).encode()}


class Responses:
    def __init__(self, responses):
        self.responses = responses
        self.requested = []

    def get(self, path):
        self.requested.append(path)
        return self.responses[path]


class FakeVerifier:
    def __init__(self, outcomes=None):
        self.outcomes = outcomes or {}
        self.calls = []

    def verify(self, component, digest, revision):
        self.calls.append((component, digest, revision))
        outcome = self.outcomes.get(component)
        if outcome is not None:
            raise outcome(component)


def completed(code):
    def fake_run(command, stdout, stderr, **kwargs):
        return subprocess.CompletedProcess(command, code)
    return fake_run


class SignatureVerificationTests(unittest.TestCase):
    def test_command_binds_identity_issuer_trigger_revision_and_digest(self):
        command = images.verify_command('/cache/cosign-3.0.6', DIGEST_B, SHA)
        self.assertEqual(command[:2], ['/cache/cosign-3.0.6', 'verify'])
        options = dict(zip(command[2:-1:2], command[3:-1:2]))
        self.assertEqual(options, {
            '--certificate-identity': 'https://github.com/os-santiago/ai-sdlc/.github/workflows/containers.yml@refs/heads/main',
            '--certificate-oidc-issuer': 'https://token.actions.githubusercontent.com',
            '--certificate-github-workflow-repository': 'os-santiago/ai-sdlc',
            '--certificate-github-workflow-ref': 'refs/heads/main',
            '--certificate-github-workflow-trigger': 'push',
            '--certificate-github-workflow-sha': SHA})
        self.assertEqual(command[-1], 'quay.io/os-santiago/ai-sdlc@' + DIGEST_B)
        self.assertNotIn('--certificate-identity-regexp', command)
        self.assertFalse(any(arg.startswith('--insecure') or arg.startswith('--allow') for arg in command))
        for digest, revision in (('latest', SHA), ('sha256:' + 'B' * 64, SHA), (DIGEST_B, 'main'), (DIGEST_B, SHA[:12])):
            with self.subTest(digest=digest, revision=revision), self.assertRaises(ValueError):
                images.verify_command('cosign', digest, revision)

    def test_cosign_outcomes_map_to_named_fail_closed_errors(self):
        verifier = images.Cosign('cosign')
        with patch.object(images.subprocess, 'run', side_effect=completed(0)) as child:
            verifier.verify('worker', DIGEST_B, SHA)
        self.assertEqual(child.call_args.args[0], images.verify_command('cosign', DIGEST_B, SHA))
        self.assertIs(child.call_args.kwargs['stdin'], subprocess.DEVNULL)
        self.assertLessEqual(child.call_args.kwargs['timeout'], images.VERIFY_SECONDS)

        def failing(message):
            def fake_run(command, stdout, stderr, **kwargs):
                stderr.write(message)
                return subprocess.CompletedProcess(command, 10)
            return fake_run
        cases = [(b'Error: no signatures found\n', images.ImageUnsigned),
                 (b'Error: no matching signatures: certificate identity mismatch\n', images.ImageSignatureRejected),
                 (b'Error: GET https://quay.io/v2/: connection refused\n', images.ImageSignatureRejected),
                 (b'', images.ImageSignatureRejected)]
        for message, error in cases:
            with self.subTest(message=message), patch.object(images.subprocess, 'run', side_effect=failing(message)):
                with self.assertRaises(error) as raised:
                    verifier.verify('worker', DIGEST_B, SHA)
                self.assertEqual(raised.exception.component, 'worker')
        with patch.object(images.subprocess, 'run', side_effect=subprocess.TimeoutExpired('cosign', 1)):
            with self.assertRaises(images.ImageVerificationTimeout):
                verifier.verify('worker', DIGEST_B, SHA)
        verifier.deadline = time.monotonic() - 1
        with patch.object(images.subprocess, 'run') as child, self.assertRaises(images.ImageVerificationTimeout):
            verifier.verify('worker', DIGEST_B, SHA)
        child.assert_not_called()

    def test_real_child_output_is_bounded(self):
        with tempfile.TemporaryDirectory() as directory:
            executable = Path(directory) / 'cosign'
            executable.write_text('#!/usr/bin/env python3\nimport os\nos.write(1, b"x" * (2 * 1024 * 1024))\n')
            executable.chmod(0o700)
            with self.assertRaises(images.ImageSignatureRejected):
                images.Cosign(executable).verify('worker', DIGEST_B, SHA)

    def test_every_published_image_must_verify(self):
        published = identities('worker', 'migrator', 'verifier')
        verifier = FakeVerifier()
        result = images.verify_publication(verifier, published, SHA)
        self.assertEqual(result['images'], {name: 'verified' for name in published})
        self.assertEqual(result['certificateIdentity'], images.CERTIFICATE_IDENTITY)
        self.assertEqual(sorted(call[0] for call in verifier.calls), sorted(published))
        self.assertTrue(all(call[1:] == (DIGEST_B, SHA) for call in verifier.calls))
        for error in (images.ImageUnsigned, images.ImageSignatureRejected, images.ImageVerificationTimeout):
            with self.subTest(error=error), self.assertRaises(error):
                images.verify_publication(FakeVerifier({'verifier': error}), published, SHA)

    def test_unsigned_images_are_accepted_only_when_allowed(self):
        published = identities('worker', 'migrator')
        unsigned = {'worker': images.ImageUnsigned, 'migrator': images.ImageUnsigned}
        with self.assertRaises(images.ImageUnsigned):
            images.verify_publication(FakeVerifier(unsigned), published, SHA)
        result = images.verify_publication(FakeVerifier(unsigned), published, SHA, allow_unsigned=True)
        self.assertEqual(result['images'], {'worker': 'unsigned-legacy', 'migrator': 'unsigned-legacy'})
        self.assertTrue(result['legacyAccepted'])
        # A present but invalid signature is never downgraded to legacy.
        for error in (images.ImageSignatureRejected, images.ImageVerificationTimeout):
            with self.subTest(error=error), self.assertRaises(error):
                images.verify_publication(FakeVerifier({'worker': error}), published, SHA, allow_unsigned=True)

    def test_cutover_sentinel_is_read_from_the_exact_commit_tree(self):
        with_sentinel = Responses(git_responses(SHA))
        self.assertTrue(images.revision_signs(with_sentinel, SHA))
        self.assertEqual(with_sentinel.requested, ['git/commits/' + SHA, 'git/trees/' + 'c' * 40, 'git/trees/' + 'd' * 40])
        self.assertFalse(images.revision_signs(Responses(git_responses(SHA, sentinel=False)), SHA))
        no_github = git_responses(SHA)
        no_github['git/trees/' + 'c' * 40] = json.dumps({'sha': 'c' * 40, 'truncated': False, 'tree': []}).encode()
        self.assertFalse(images.revision_signs(Responses(no_github), SHA))
        # Any entry at the sentinel path counts as signing, whatever its type.
        odd = git_responses(SHA)
        odd['git/trees/' + 'd' * 40] = json.dumps({'sha': 'd' * 40, 'truncated': False, 'tree': [
            {'path': images.CUTOVER_FILE, 'mode': '120000', 'type': 'blob', 'sha': 'e' * 40}]}).encode()
        self.assertTrue(images.revision_signs(Responses(odd), SHA))

        def changed(path, value):
            responses = git_responses(SHA)
            responses[path] = json.dumps(value).encode()
            return Responses(responses)
        faults = [changed('git/commits/' + SHA, {'sha': 'b' * 40, 'tree': {'sha': 'c' * 40}}),
                  changed('git/commits/' + SHA, {'sha': SHA, 'tree': {'sha': 'main'}}),
                  changed('git/trees/' + 'c' * 40, {'sha': 'c' * 40, 'truncated': True, 'tree': []}),
                  changed('git/trees/' + 'c' * 40, {'sha': 'b' * 40, 'truncated': False, 'tree': []}),
                  changed('git/trees/' + 'c' * 40, {'sha': 'c' * 40, 'truncated': False, 'tree': [
                      {'path': '.github', 'type': 'blob', 'sha': 'd' * 40}]}),
                  changed('git/trees/' + 'c' * 40, {'sha': 'c' * 40, 'truncated': False, 'tree': [
                      {'path': '.github', 'type': 'tree', 'sha': 'd' * 40}, {'path': '.github', 'type': 'tree', 'sha': 'd' * 40}]})]
        for index, api in enumerate(faults):
            with self.subTest(fault=index), self.assertRaises(ValueError):
                images.revision_signs(api, SHA)
        for revision in ('main', SHA[:12], 'refs/heads/main'):
            with self.subTest(revision=revision), self.assertRaises(ValueError):
                images.revision_signs(Responses({}), revision)

    def test_signature_verifier_applies_the_cutover(self):
        published = identities('worker', 'migrator')
        unsigned = {'worker': images.ImageUnsigned, 'migrator': images.ImageUnsigned}
        legacy_api, signing_api = Responses(git_responses(SHA, sentinel=False)), Responses(git_responses(SHA))
        # Strict by default: the sentinel is not even consulted.
        strict = Responses({})
        with self.assertRaises(images.ImageUnsigned):
            images.signature_verifier(strict, SHA, False, cosign=FakeVerifier(unsigned))(published)
        self.assertEqual(strict.requested, [])
        result = images.signature_verifier(legacy_api, SHA, True, cosign=FakeVerifier(unsigned))(published)
        self.assertEqual(set(result['images'].values()), {'unsigned-legacy'})
        # Library callers: a signing revision is verified strictly.
        with self.assertRaises(images.ImageUnsigned):
            images.signature_verifier(signing_api, SHA, True, cosign=FakeVerifier(unsigned))(published)
        # The operator flag on a signing revision is refused before any cosign call.
        verifier = FakeVerifier(unsigned)
        with self.assertRaises(images.UnsignedLegacyNotEligible):
            images.signature_verifier(signing_api, SHA, True, refuse_ineligible=True, cosign=verifier)(published)
        self.assertEqual(verifier.calls, [])
        # cosign is fetched lazily, only when verification runs.
        with patch.object(images, 'pinned_cosign', side_effect=AssertionError('fetched eagerly')):
            callback = images.signature_verifier(Responses({}), SHA, False)
        with patch.object(images, 'pinned_cosign', return_value='cosign') as fetch, \
                patch.object(images.subprocess, 'run', side_effect=completed(0)):
            self.assertEqual(set(callback(published)['images'].values()), {'verified'})
        fetch.assert_called_once_with()

    def test_resolve_requires_an_explicit_verification_decision(self):
        with self.assertRaises(TypeError):
            images.resolve(Responses({}), RUN, SHA)
        for value in (None, True, 'skip'):
            with self.subTest(verify=value), self.assertRaises(ValueError):
                images.resolve(Responses({}), RUN, SHA, verify=value)

    def test_every_library_caller_decides_verification(self):
        root = Path(__file__).resolve().parent
        callers = 0
        for path in sorted(root.glob('*.py')):
            if path.name in ('install_images.py', 'test_install_images.py'):
                continue
            text = path.read_text()
            for match in re.finditer(r'install_images\.resolve\(', text):
                call = text[match.start():text.index('\n\n', match.start()) if '\n\n' in text[match.start():] else len(text)]
                callers += 1
                with self.subTest(caller=path.name):
                    self.assertIn('verify=install_images.signature_verifier(', call[:400])
        self.assertGreaterEqual(callers, 12)

    def test_sentinel_and_signing_workflow_stay_consistent(self):
        root = Path(__file__).resolve().parents[1]
        workflow = (root / '.github/workflows/containers.yml').read_text()
        sentinel = root / images.CUTOVER_PATH
        self.assertEqual('cosign sign' in workflow, sentinel.is_file())
        self.assertTrue(sentinel.is_file(), 'containers.yml signs, so the cutover sentinel must exist')

    def test_cli_writes_no_lock_when_a_signature_fails(self):
        responses = {'actions/runs/123': json.dumps(run()).encode(), **git_responses(SHA)}
        catalog = {'total_count': 9, 'artifacts': []}
        for component in sorted(images.ARTIFACT_COMPONENTS):
            data, metadata = archive(component)
            catalog['artifacts'].append(metadata)
            responses['actions/artifacts/' + str(metadata['id']) + '/zip'] = data
        responses['actions/runs/123/artifacts?per_page=100'] = json.dumps(catalog).encode()

        class API:
            def get(self, path):
                return responses[path]

        def unsigned_run(command, stdout, stderr, **kwargs):
            stderr.write(b'Error: no signatures found\n')
            return subprocess.CompletedProcess(command, 10)

        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'publication'
            arguments = ['--run', str(RUN), '--revision', SHA, '--output', str(output)]
            with patch.object(images, 'GitHub', API), patch.object(images, 'pinned_cosign', return_value='cosign'), \
                    patch.object(images.subprocess, 'run', side_effect=unsigned_run), \
                    patch('sys.stderr', new_callable=io.StringIO) as stderr, self.assertRaises(SystemExit) as exit_:
                images.main(arguments)
            self.assertEqual(exit_.exception.code, 1)
            self.assertIn('ImageUnsigned (', stderr.getvalue())
            self.assertFalse(output.exists())
            with patch.object(images, 'GitHub', API), patch.object(images, 'pinned_cosign', return_value='cosign'), \
                    patch.object(images.subprocess, 'run', side_effect=unsigned_run), \
                    patch('sys.stderr', new_callable=io.StringIO) as stderr, self.assertRaises(SystemExit):
                images.main(arguments + ['--allow-unsigned-legacy'])
            self.assertIn('UnsignedLegacyNotEligible', stderr.getvalue())
            self.assertFalse(output.exists())
            with patch.object(images, 'GitHub', API), patch.object(images, 'pinned_cosign', return_value='cosign'), \
                    patch.object(images.subprocess, 'run', side_effect=completed(0)) as child, patch('sys.stdout', new_callable=io.StringIO):
                self.assertEqual(images.main(arguments), 0)
            self.assertEqual(child.call_count, len(images.ARTIFACT_COMPONENTS))
            evidence = json.loads((output / 'images.evidence.json').read_text())
            self.assertEqual(evidence['signatures']['images'], {name: 'verified' for name in images.ARTIFACT_COMPONENTS})
            self.assertIn('keyless image signatures', evidence['scope'])
            lock = json.loads((output / 'images.lock.json').read_text())
            self.assertEqual(set(lock), {'image', 'components', 'migrator'})

    def test_pinned_cosign_uses_the_checksummed_installer_tool_cache(self):
        import install
        lock = json.loads((Path(__file__).resolve().parent / 'install-tools.lock.json').read_text())
        entry = lock['cosign']
        self.assertEqual(entry['version'], '3.0.6')
        self.assertTrue(entry['url'].startswith('https://github.com/sigstore/cosign/releases/download/v3.0.6/'))
        self.assertRegex(entry['sha256'], '^[a-f0-9]{64}$')
        self.assertNotIn('cosign', install.CLUSTER_TOOLS)
        with patch.object(install, 'tools', return_value={'cosign': '/cache/cosign-3.0.6'}) as fetch:
            self.assertEqual(images.pinned_cosign(), '/cache/cosign-3.0.6')
        self.assertEqual(fetch.call_args.args[1], ('cosign',))
        with self.assertRaises(ValueError), patch.object(install.platform, 'system', return_value='Linux'), \
                patch.object(install.platform, 'machine', return_value='x86_64'):
            install.tools(Path(tempfile.gettempdir()) / 'unused-ai-sdlc-tools', ('unpinned',))

    def test_workflow_signs_every_published_component_by_digest_only(self):
        workflow = (Path(__file__).resolve().parents[1] / '.github/workflows/containers.yml').read_text()
        matrices = re.findall(r'^ +component: \[([^\]]+)\]$', workflow, re.MULTILINE)
        self.assertEqual(len(matrices), 2)
        self.assertEqual(matrices[0], matrices[1])
        self.assertTrue(images.ARTIFACT_COMPONENTS <= {name.strip() for name in matrices[0].split(',')})
        sign = workflow[workflow.index('\n  sign:\n'):]
        images_job = workflow[:workflow.index('\n  sign:\n')].split('jobs:', 1)[1]
        self.assertNotIn('id-token', images_job)
        self.assertIn("if: github.event_name == 'push' && github.ref == 'refs/heads/main'", sign)
        self.assertIn('cosign sign --yes "$IMAGE"', sign)
        self.assertIn("output.write('image=' + identity['repository'] + '@' + identity['digest']", sign)
        # The OIDC job runs only the first-party artifact download; SBOM
        # generation and registry login actions stay out of it.
        self.assertEqual(set(re.findall(r'uses: ([^@\s]+)@', sign)), {'actions/download-artifact'})
        self.assertIn('anchore/sbom-action', images_job)
        self.assertNotIn('docker/login-action', sign)
        self.assertIn('--password-stdin', sign)
        # The workflow's cosign pin is the installer's pin.
        entry = json.loads((Path(__file__).resolve().parent / 'install-tools.lock.json').read_text())['cosign']
        self.assertIn('COSIGN_URL: ' + entry['url'] + '\n', sign)
        self.assertIn('COSIGN_SHA256: ' + entry['sha256'] + '\n', sign)
        self.assertIn('sha256sum -c -', sign)

if __name__ == '__main__':
    unittest.main()
