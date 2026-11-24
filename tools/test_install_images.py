import base64
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
        lock, evidence = images.resolve(API(), RUN, SHA)
        self.assertEqual(set(lock), {'image', 'components', 'migrator'})
        self.assertEqual(set(lock['components']), set(images.COMPONENTS))
        self.assertEqual(lock['image']['tag'], SHA)
        self.assertEqual(set(evidence['images']), images.ARTIFACT_COMPONENTS)
        self.assertIn('separate validation', evidence['scope'])



DIGEST_B = 'sha256:' + 'b' * 64


def identities(*components):
    return {name: {'component': name, 'digest': DIGEST_B, 'repository': images.REGISTRY,
                   'revision': SHA, 'platform': 'linux/amd64'} for name in components}


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

    def test_unsigned_legacy_requires_flag_and_a_revision_that_never_signed(self):
        published = identities('worker', 'migrator')
        unsigned = {'worker': images.ImageUnsigned, 'migrator': images.ImageUnsigned}
        legacy_workflow = b'jobs:\n  images:\n    steps: []\n'
        signing_workflow = (Path(__file__).resolve().parents[1] / '.github/workflows/containers.yml').read_bytes()
        with self.assertRaises(images.ImageUnsigned):
            images.verify_publication(FakeVerifier(unsigned), published, SHA)
        result = images.verify_publication(FakeVerifier(unsigned), published, SHA, legacy_workflow)
        self.assertEqual(result['images'], {'worker': 'unsigned-legacy', 'migrator': 'unsigned-legacy'})
        verifier = FakeVerifier(unsigned)
        with self.assertRaises(images.UnsignedLegacyNotEligible):
            images.verify_publication(verifier, published, SHA, signing_workflow)
        self.assertEqual(verifier.calls, [])
        # A present but invalid signature is never downgraded to legacy.
        with self.assertRaises(images.ImageSignatureRejected):
            images.verify_publication(FakeVerifier({'worker': images.ImageSignatureRejected}), published, SHA,
                                      legacy_workflow)

    def test_legacy_workflow_is_read_at_the_exact_revision(self):
        content = base64.encodebytes(b'name: containers\n').decode()
        record = {'type': 'file', 'path': images.WORKFLOW_PATH, 'encoding': 'base64', 'content': content}
        requested = []

        class API:
            def __init__(self, value):
                self.value = value

            def get(self, path):
                requested.append(path)
                return json.dumps(self.value).encode()
        self.assertEqual(images.publication_workflow(API(record), SHA), b'name: containers\n')
        self.assertEqual(requested, ['contents/.github/workflows/containers.yml?ref=' + SHA])
        for change in ({'type': 'dir'}, {'path': 'other.yml'}, {'encoding': 'none'}, {'content': None}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                images.publication_workflow(API(dict(record, **change)), SHA)
        with self.assertRaises(ValueError):
            images.publication_workflow(API(record), 'main')

    def test_cli_writes_no_lock_when_a_signature_fails(self):
        workflow = (Path(__file__).resolve().parents[1] / '.github/workflows/containers.yml').read_bytes()
        responses = {'actions/runs/123': json.dumps(run()).encode(),
                     'contents/.github/workflows/containers.yml?ref=' + SHA: json.dumps(
                         {'type': 'file', 'path': images.WORKFLOW_PATH, 'encoding': 'base64',
                          'content': base64.encodebytes(workflow).decode()}).encode()}
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
        images_job = workflow[:workflow.index('\n  sign:\n')]
        self.assertNotIn('id-token', images_job.split('jobs:', 1)[1])
        self.assertIn("if: github.event_name == 'push' && github.ref == 'refs/heads/main'", sign)
        self.assertIn('cosign sign --yes "$IMAGE"', sign)
        self.assertIn("output.write('image=' + identity['repository'] + '@' + identity['digest']", sign)


if __name__ == '__main__':
    unittest.main()
