import contextlib
import copy
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import install
import tekton_ci_install as tekton

UID = '11111111-2222-3333-4444-555555555555'
IMAGE = 'ghcr.io/tektoncd/pipeline/controller-' + 'a' * 32 + ':v9.9.9@sha256:' + 'b' * 64


def synthetic(raw_text):
    raw = raw_text.encode()
    return raw, {'version': 1, 'release': 'v9.9.9', 'url': '', 'sha256': hashlib.sha256(raw).hexdigest(),
                 'bytes': len(raw), 'maxBytes': 4096, 'images': [IMAGE], 'namespaces': ['tekton-pipelines'],
                 'customResourceDefinitions': ['tasks.tekton.dev'],
                 'requiredDeployments': ['tekton-pipelines/tekton-pipelines-controller']}


def release_items(image=IMAGE, extra=()):
    return [
        {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': 'tekton-pipelines', 'labels': {'a': 'b'}}},
        {'apiVersion': 'apiextensions.k8s.io/v1', 'kind': 'CustomResourceDefinition', 'metadata': {'name': 'tasks.tekton.dev'}},
        {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'feature-flags', 'namespace': 'tekton-pipelines'},
         'data': {'set-security-context': 'false', 'coschedule': 'workspaces'}},
        {'apiVersion': 'apps/v1', 'kind': 'Deployment',
         'metadata': {'name': 'tekton-pipelines-controller', 'namespace': 'tekton-pipelines'},
         'spec': {'template': {'spec': {'containers': [{'name': 'c', 'image': image}]}}}},
        *extra]


CONFIG = {'version': 1, 'configMaps': {'tekton-pipelines/feature-flags': {'set-security-context': 'true'}}}


def completed(stdout='', returncode=0):
    return SimpleNamespace(stdout=stdout, returncode=returncode)


class DesiredStateTests(unittest.TestCase):
    def test_committed_desired_state_is_valid_and_pinned(self):
        lock, _ = tekton.lockfile()
        self.assertTrue(all('@sha256:' in image for image in lock['images']))
        config, _ = tekton.configuration()
        flags = config['configMaps']['tekton-pipelines/feature-flags']
        self.assertEqual(flags['set-security-context'], 'true')
        self.assertEqual(flags['coschedule'], 'disabled', 'affinity-assistant pods would deadlock the CI pod quota')
        resolvers = config['configMaps']['tekton-pipelines-resolvers/resolvers-feature-flags']
        self.assertEqual(set(resolvers.values()), {'false'}, 'remote resolvers fetch unreviewed definitions')
        raw, _ = tekton.ci_manifests()
        self.assertEqual(set(raw), set(tekton.CI_FILES))
        plan = tekton.plan()
        self.assertEqual((plan['release'], plan['pipelines']), (lock['release'], list(tekton.PIPELINES)))

    def test_lock_rejects_unpinned_unsorted_or_redirected_inputs(self):
        lock = json.loads((tekton.DESIRED / 'upstream.lock.json').read_text())
        mutations = [
            lambda l: l.update(url='https://example.com/release.yaml'),
            lambda l: l['images'].__setitem__(0, l['images'][0].split('@')[0]),
            lambda l: l['images'].reverse(),
            lambda l: l.update(bytes=l['maxBytes'] + 1),
            lambda l: l.update(sha256='0' * 63),
            lambda l: l['requiredDeployments'].append('zz-other/tekton-x'),
            lambda l: l.update(extra=True),
        ]
        for mutate in mutations:
            changed = copy.deepcopy(lock)
            mutate(changed)
            with tempfile.TemporaryDirectory() as directory, patch.object(tekton, 'DESIRED', Path(directory)):
                (Path(directory) / 'upstream.lock.json').write_text(json.dumps(changed))
                with self.assertRaises(ValueError):
                    tekton.lockfile()

    def test_configuration_must_harden_helpers_and_refuse_inline_specs(self):
        config = json.loads((tekton.DESIRED / 'configuration.json').read_text())
        for key, value in (('set-security-context', 'false'),
                           ('set-security-context-read-only-root-filesystem', 'false'),
                           ('disable-inline-spec', 'pipelinerun')):
            changed = copy.deepcopy(config)
            changed['configMaps']['tekton-pipelines/feature-flags'][key] = value
            with tempfile.TemporaryDirectory() as directory, patch.object(tekton, 'DESIRED', Path(directory)):
                (Path(directory) / 'configuration.json').write_text(json.dumps(changed))
                with self.assertRaises(ValueError):
                    tekton.configuration()

    def test_ci_manifests_refuse_unpinned_images_and_unexpected_files(self):
        with tempfile.TemporaryDirectory() as directory:
            for name in tekton.CI_FILES:
                (Path(directory) / name).write_bytes((tekton.CI_DIR / name).read_bytes())
            with patch.object(tekton, 'CI_DIR', Path(directory)):
                tekton.ci_manifests()
                task = Path(directory) / 'task-source-checkout.yaml'
                task.write_text(task.read_text().replace('@sha256:', ':latest#'))
                with self.assertRaisesRegex(ValueError, 'pinned by digest'):
                    tekton.ci_manifests()
                task.write_bytes((tekton.CI_DIR / 'task-source-checkout.yaml').read_bytes())
                (Path(directory) / 'extra.yaml').write_text('kind: Secret\n')
                with self.assertRaisesRegex(ValueError, 'Unexpected'):
                    tekton.ci_manifests()


class ReleaseTests(unittest.TestCase):
    def test_release_must_match_digest_and_locked_images(self):
        raw, lock = synthetic('image: ' + IMAGE + '\n')
        tekton.verify_release(raw, lock)
        with self.assertRaisesRegex(ValueError, 'integrity'):
            tekton.verify_release(raw + b'\n', lock)
        other = IMAGE.replace('b' * 64, 'c' * 64)
        raw, lock = synthetic('image: ' + IMAGE + '\nargs: [' + other + ']\n')
        with self.assertRaisesRegex(ValueError, 'differ'):
            tekton.verify_release(raw, lock)
        raw, lock = synthetic('image: ' + IMAGE + '\nimage: docker.io/library/busybox:latest\n')
        with self.assertRaisesRegex(ValueError, 'not pinned'):
            tekton.verify_release(raw, lock)

    def test_fetch_uses_verified_cache_and_bounds_download(self):
        raw, lock = synthetic('image: ' + IMAGE + '\n')
        with tempfile.TemporaryDirectory() as directory:
            cache = Path(directory)
            opener = Mock(side_effect=lambda *a, **k: contextlib.nullcontext(io.BytesIO(raw)))
            self.assertEqual(tekton.fetch_release(cache, lock, opener), raw)
            self.assertEqual(opener.call_count, 1)
            self.assertEqual(tekton.fetch_release(cache, lock, opener), raw)
            self.assertEqual(opener.call_count, 1, 'a verified cached copy is not downloaded again')
            (cache / 'tekton-pipeline-v9.9.9.yaml').write_bytes(b'tampered')
            self.assertEqual(tekton.fetch_release(cache, lock, opener), raw)
            self.assertEqual(opener.call_count, 2)

    def test_fetch_refuses_oversized_or_corrupt_download_without_caching(self):
        raw, lock = synthetic('image: ' + IMAGE + '\n')
        with tempfile.TemporaryDirectory() as directory:
            cache = Path(directory)
            big = Mock(return_value=contextlib.nullcontext(io.BytesIO(b'x' * (lock['maxBytes'] + 10))))
            with self.assertRaisesRegex(ValueError, 'size bound'):
                tekton.fetch_release(cache, lock, big)
            corrupt = Mock(return_value=contextlib.nullcontext(io.BytesIO(raw[:-1] + b'!')))
            with self.assertRaisesRegex(ValueError, 'integrity'):
                tekton.fetch_release(cache, lock, corrupt)
            self.assertEqual(list(cache.iterdir()), [])


class ResourceTests(unittest.TestCase):
    def test_desired_tekton_applies_reviewed_configuration_and_ownership(self):
        _, lock = synthetic('')
        items = tekton.desired_tekton(release_items(), lock, CONFIG, UID)
        flags = next(i for i in items if i['kind'] == 'ConfigMap')
        self.assertEqual(flags['data'], {'set-security-context': 'true', 'coschedule': 'workspaces'})
        for item in items:
            self.assertEqual(item['metadata']['annotations'][tekton.OWNER_ANNOTATION], UID)
            self.assertEqual(item['metadata']['labels'][tekton.OWNER_LABEL], 'owned')
        self.assertEqual(items[0]['metadata']['labels']['a'], 'b', 'upstream labels are preserved')

    def test_desired_tekton_refuses_drift_from_the_lock(self):
        _, lock = synthetic('')
        cases = [
            (release_items(image=IMAGE.replace('b' * 64, 'c' * 64)), CONFIG, 'image differs'),
            (release_items()[1:], CONFIG, 'locked inventory'),
            (release_items(extra=[{'kind': 'Namespace', 'metadata': {'name': 'other'}}]), CONFIG, 'unlocked namespace'),
            (release_items(extra=[{'kind': 'Role', 'metadata': {'name': 'r', 'namespace': 'default'}}]), CONFIG, 'outside'),
            (release_items(), {'version': 1, 'configMaps': {'tekton-pipelines/feature-flags': {'typo': 'true'}}}, 'unknown keys'),
            (release_items(), {'version': 1, 'configMaps': {'tekton-pipelines/missing': {'a': 'b'}}}, 'unknown ConfigMap'),
        ]
        for items, config, message in cases:
            with self.assertRaisesRegex(ValueError, message):
                tekton.desired_tekton(items, lock, config, UID)

    def test_desired_ci_refuses_credentials_api_access_and_namespace_escape(self):
        base = {'kind': 'Pipeline', 'metadata': {'name': 'p', 'namespace': 'ai-sdlc-ci'}}
        self.assertEqual(tekton.desired_ci([copy.deepcopy(base)], UID)[0]['metadata']['annotations'][tekton.OWNER_ANNOTATION], UID)
        for item in ({'kind': 'Secret', 'metadata': {'name': 's', 'namespace': 'ai-sdlc-ci'}},
                     {'kind': 'RoleBinding', 'metadata': {'name': 'r', 'namespace': 'ai-sdlc-ci'}},
                     {'kind': 'ServiceAccount', 'metadata': {'name': 'a', 'namespace': 'ai-sdlc-ci'}},
                     {'kind': 'Pipeline', 'metadata': {'name': 'p', 'namespace': 'default'}},
                     {'kind': 'Namespace', 'metadata': {'name': 'other'}},
                     {'kind': 'ValidatingAdmissionPolicy', 'metadata': {'name': 'other-policy'}},
                     {'kind': 'ClusterRole', 'metadata': {'name': 'ai-sdlc-ci-x'}}):
            with self.assertRaises(ValueError):
                tekton.desired_ci([item], UID)
        policy = {'kind': 'ValidatingAdmissionPolicy', 'metadata': {'name': 'ai-sdlc-ci-pipelineruns'}}
        self.assertEqual(tekton.desired_ci([policy], UID)[0]['metadata']['annotations'][tekton.OWNER_ANNOTATION], UID)

    def test_one_cache_claim_per_repository(self):
        claims = tekton.cache_claims(['os-santiago/quarkus-super-heroes', 'os-santiago/ai-sdlc', 'os-santiago/ai-sdlc'])
        self.assertEqual(len(claims), 2)
        names = {c['metadata']['annotations']['ai-sdlc.io/repository']: c['metadata']['name'] for c in claims}
        self.assertEqual(names['os-santiago/ai-sdlc'], tekton.cache_claim('os-santiago/ai-sdlc'))
        self.assertNotEqual(names['os-santiago/ai-sdlc'], names['os-santiago/quarkus-super-heroes'])
        self.assertRegex(names['os-santiago/ai-sdlc'], r'^ai-sdlc-ci-maven-cache-[0-9a-f]{16}$')
        for bad in ('Owner/Repo', 'a/..', 'x', 'a/b/c'):
            with self.assertRaises(ValueError):
                tekton.cache_claim(bad)

    def test_existing_cluster_scoped_objects_without_this_installation_mark_are_never_adopted(self):
        items = [{'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': 'tekton-pipelines'}},
                 {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRole', 'metadata': {'name': 'tekton-pipelines-controller-cluster-access'}},
                 {'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingWebhookConfiguration', 'metadata': {'name': 'validation.webhook.pipeline.tekton.dev'}},
                 {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'feature-flags', 'namespace': 'tekton-pipelines'}}]
        foreign = json.dumps({'metadata': {'annotations': {tekton.OWNER_ANNOTATION: 'other'}}})
        run = Mock(side_effect=[completed(''), completed(''), completed(foreign)])
        with self.assertRaisesRegex(ValueError, 'validatingwebhookconfiguration.admissionregistration.k8s.io .*not owned'):
            tekton.require_ownership(run, ['kubectl'], items, UID)
        self.assertEqual([c.args[0][2] for c in run.call_args_list],
                         ['namespace', 'clusterrole.rbac.authorization.k8s.io', 'validatingwebhookconfiguration.admissionregistration.k8s.io'])
        owned = json.dumps({'metadata': {'annotations': {tekton.OWNER_ANNOTATION: UID}}})
        run = Mock(return_value=completed(owned))
        tekton.require_ownership(run, ['kubectl'], items, UID)
        self.assertEqual(run.call_count, 3, 'namespaced objects inside owned namespaces are not read')
        unmarked = json.dumps({'metadata': {}})
        with self.assertRaisesRegex(ValueError, 'not owned'):
            tekton.require_ownership(Mock(return_value=completed(unmarked)), ['kubectl'], items, UID)

    def test_apply_is_server_side_under_one_manager_without_forcing_conflicts(self):
        run = Mock(return_value=completed())
        tekton.apply(run, ['kubectl'], [{'kind': 'ConfigMap'}])
        argv = run.call_args.args[0]
        self.assertIn('--server-side', argv)
        self.assertEqual(argv[argv.index('--field-manager') + 1], 'ai-sdlc-tekton-ci')
        self.assertNotIn('--force-conflicts', argv)
        self.assertEqual(json.loads(run.call_args.kwargs['input'])['kind'], 'List')
        tekton.apply(run, ['kubectl'], [], dry_run=True)
        self.assertIn('--dry-run=server', run.call_args.args[0])
        self.assertFalse(run.call_args.kwargs['check'])

    def test_objects_parses_concatenated_kubectl_documents(self):
        stdout = json.dumps({'kind': 'Namespace', 'metadata': {'name': 'a'}}) + '\n' + json.dumps(
            {'kind': 'List', 'items': [{'kind': 'ConfigMap', 'metadata': {'name': 'b'}}]})
        items = tekton.objects(Mock(return_value=completed(stdout)), ['kubectl'], b'---')
        self.assertEqual([i['metadata']['name'] for i in items], ['a', 'b'])
        with self.assertRaises(ValueError):
            tekton.objects(Mock(return_value=completed('{"kind": "X", "metadata": {}}')), ['kubectl'], b'')

    def test_admission_gate_retries_then_gives_up(self):
        run = Mock(side_effect=[completed(returncode=1), completed(returncode=1), completed()])
        sleep = Mock()
        self.assertEqual(tekton.wait_admission(run, ['kubectl'], [], sleep=sleep), 3)
        run = Mock(return_value=completed(returncode=1))
        with self.assertRaisesRegex(ValueError, 'admission'):
            tekton.wait_admission(run, ['kubectl'], [], sleep=sleep)
        self.assertEqual(run.call_count, tekton.ADMISSION_ATTEMPTS)


class ObservationTests(unittest.TestCase):
    def observe(self, generation=2, ready=1, flags=None,
                pipeline='\n'.join('pipeline.tekton.dev/' + p for p in tekton.PIPELINES),
                tasks='\n'.join('task.tekton.dev/' + t for t in tekton.TASKS)):
        lock = {'release': 'v9.9.9', 'requiredDeployments': ['tekton-pipelines/tekton-pipelines-controller']}
        deployment = {'metadata': {'generation': 2}, 'spec': {'replicas': 1},
                      'status': {'observedGeneration': generation, 'updatedReplicas': 1,
                                 'readyReplicas': ready, 'availableReplicas': ready}}
        wanted = tekton.configuration()[0]['configMaps']['tekton-pipelines/feature-flags']
        configmap = {'data': dict(wanted, **(flags or {}))}
        run = Mock(side_effect=[completed(json.dumps(deployment)), completed(json.dumps(configmap)), completed(pipeline),
                                completed(tasks)])
        return tekton.observe(run, ['kubectl'], lock)

    def test_ready_requires_current_generation_hardening_and_every_pipeline(self):
        self.assertTrue(self.observe()['ready'])
        self.assertFalse(self.observe(generation=1)['ready'])
        self.assertFalse(self.observe(ready=0)['ready'])
        self.assertFalse(self.observe(flags={'set-security-context': 'false'})['ready'])
        self.assertFalse(self.observe(pipeline='')['ready'])
        self.assertFalse(self.observe(pipeline='pipeline.tekton.dev/' + tekton.PIPELINES[0])['ready'], 'every catalog pipeline')
        self.assertFalse(self.observe(tasks='task.tekton.dev/' + tekton.TASKS[0])['ready'], 'every catalog task')


class InstallerOptionTests(unittest.TestCase):
    def test_option_is_limited_to_kind_installation_scopes(self):
        tekton.validate_options(SimpleNamespace(tekton_ci=None, command='tools', platform='konflux'))
        tekton.validate_options(SimpleNamespace(tekton_ci=True, command='local', platform='kind'))
        for command, platform in (('credentials', 'kind'), ('sandbox', 'kind'), ('local', 'konflux'), ('plan', 'openshift')):
            with self.assertRaises(ValueError):
                tekton.validate_options(SimpleNamespace(tekton_ci=True, command=command, platform=platform))
        with self.assertRaisesRegex(ValueError, 'requires --tekton-ci'):
            tekton.validate_options(SimpleNamespace(tekton_ci=None, tekton_ci_repository=['a/b'], command='local', platform='kind'))
        with self.assertRaises(ValueError):
            tekton.validate_options(SimpleNamespace(tekton_ci=True, tekton_ci_repository=['A/B'], command='local', platform='kind'))

    def test_installer_entrypoint_rejects_option_outside_its_scope_before_effects(self):
        with patch.object(install, 'tools', side_effect=AssertionError('no tool bootstrap')), \
             self.assertRaisesRegex(ValueError, '--tekton-ci requires'):
            install.main(['credentials', '--github-auth', 'gh-cli', '--tekton-ci'])

    def test_install_orders_ownership_release_readiness_admission_then_ci(self):
        calls = []
        lock = tekton.lockfile()[0]
        with patch.object(tekton, 'fetch_release', return_value=b'raw'), \
             patch.object(tekton, 'verify_release', side_effect=lambda *a: calls.append('verify')), \
             patch.object(tekton, 'require_ownership', side_effect=lambda *a: calls.append('own')), \
             patch.object(tekton, 'objects', side_effect=lambda run, k, raw: calls.append('objects') or []), \
             patch.object(tekton, 'desired_tekton', side_effect=lambda *a: ['tekton']), \
             patch.object(tekton, 'desired_ci', side_effect=lambda items, uid: [{'kind': 'Namespace'}, {'kind': 'Task'}] + items), \
             patch.object(tekton, 'apply', side_effect=lambda run, k, items, dry_run=False: calls.append(
                 ('apply', items[0] if isinstance(items[0], str) else ('namespace' if items[0]['kind'] == 'Namespace' else 'ci')))), \
             patch.object(tekton, 'wait_ready', side_effect=lambda *a: calls.append('ready')), \
             patch.object(tekton, 'wait_admission', side_effect=lambda *a: calls.append('admission') or 1), \
             patch.object(tekton, 'observe', return_value={'ready': True, 'release': lock['release']}):
            report = tekton.install(Mock(), ['kubectl'], UID, Path('/nonexistent'), '5m', ['os-santiago/quarkus-super-heroes'])
        self.assertEqual(calls, ['verify', 'objects', 'own', ('apply', 'tekton'), 'ready', 'objects', 'own',
                                 ('apply', 'namespace'), 'admission', ('apply', 'ci')])
        self.assertTrue(report['ready'])
        self.assertEqual(report['cacheClaims'], {'os-santiago/quarkus-super-heroes': tekton.cache_claim('os-santiago/quarkus-super-heroes')})
        with patch.object(tekton, 'fetch_release', return_value=b'raw'), \
             patch.object(tekton, 'verify_release'), patch.object(tekton, 'require_ownership'), \
             patch.object(tekton, 'objects', return_value=[]), patch.object(tekton, 'desired_tekton', return_value=[]), \
             patch.object(tekton, 'desired_ci', return_value=[]), patch.object(tekton, 'apply'), \
             patch.object(tekton, 'wait_ready'), patch.object(tekton, 'wait_admission', return_value=1), \
             patch.object(tekton, 'observe', return_value={'ready': False}), \
             self.assertRaisesRegex(ValueError, 'not ready'):
            tekton.install(Mock(), ['kubectl'], UID, Path('/nonexistent'), '5m', ['a/b'])


if __name__ == '__main__':
    unittest.main()
