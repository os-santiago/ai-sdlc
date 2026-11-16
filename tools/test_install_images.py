import copy
import hashlib
import io
import json
import os
from pathlib import Path
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
                      for name in images.ARTIFACT_COMPONENTS | {'artifact-builder','build-controller'}}
        lock = {'image': {'repository': images.REGISTRY, 'tag': SHA},
                'components': {key: {'digest': identities[name]['digest']} for key,name in images.COMPONENTS.items()},
                'migrator': {'digest': identities['migrator']['digest']}}
        evidence = {'revision': SHA, 'repository': images.REPOSITORY, 'images': identities}
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)/'publication'
            with patch.object(images, 'resolve', return_value=(lock,evidence)):
                self.assertEqual(images.main(['--include-build','--run',str(RUN),'--revision',SHA,'--output',str(output)]),0)
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


if __name__ == '__main__':
    unittest.main()
