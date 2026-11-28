import json
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import install
import kind_nodes
import shared_storage
import tekton_ci_install as tekton

KIND_CONFIG = json.dumps({'nodePathMap': [{'node': 'DEFAULT_PATH_FOR_NON_LISTED_NODES', 'paths': ['/var/local-path-provisioner']}]})


class FakeCluster:
    def __init__(self, config=KIND_CONFIG, storage_class=None):
        self.config, self.storage_class, self.calls = config, storage_class, []

    def run(self, args, input=None, **_kwargs):
        self.calls.append(args[1:])
        if args[1:3] == ['get', 'configmap']:
            return SimpleNamespace(stdout=json.dumps({'data': {'config.json': self.config}}))
        if args[1:3] == ['get', 'storageclass']:
            return SimpleNamespace(stdout=json.dumps(self.storage_class) if self.storage_class else '')
        if args[1] == 'create':
            self.storage_class = json.loads(input)
        if args[1:3] == ['patch', 'configmap']:
            self.config = json.loads(args[-1])['data']['config.json']
        return SimpleNamespace(stdout='')

    def verbs(self):
        return [c[:2] for c in self.calls if c[0] != 'get']


class SharedStorageTests(unittest.TestCase):
    def test_configures_the_shared_class_once_and_keeps_kinds_settings(self):
        cluster = FakeCluster()
        report = shared_storage.configure(cluster.run, ['kubectl'])
        self.assertTrue(report['configured'])
        config = json.loads(cluster.config)
        # Once storageClassConfigs exists the provisioner refuses unlisted
        # classes, so kind's standard class keeps its node-local paths.
        self.assertEqual(config['storageClassConfigs'], {'ai-sdlc-shared': {'sharedFileSystemPath': kind_nodes.SHARED_PATH},
                                                         'standard': {'nodePathMap': json.loads(KIND_CONFIG)['nodePathMap']}})
        self.assertEqual(config['nodePathMap'], json.loads(KIND_CONFIG)['nodePathMap'])
        self.assertEqual(cluster.storage_class['provisioner'], 'rancher.io/local-path')
        self.assertIn(['rollout', 'restart'], cluster.verbs(), 'the provisioner reads its configuration at start')
        cluster.calls.clear()
        self.assertFalse(shared_storage.configure(cluster.run, ['kubectl'])['configured'])
        self.assertEqual(cluster.verbs(), [['rollout', 'status']], 'reapplication restarts nothing')

    def test_refuses_foreign_class_or_entry(self):
        with self.assertRaisesRegex(ValueError, 'not this installation'):
            shared_storage.configure(FakeCluster(storage_class={'provisioner': 'nfs', 'metadata': {}}).run, ['kubectl'])
        foreign = json.dumps(dict(json.loads(KIND_CONFIG), storageClassConfigs={'ai-sdlc-shared': {'sharedFileSystemPath': '/x'}}))
        with self.assertRaisesRegex(ValueError, 'different'):
            shared_storage.configure(FakeCluster(config=foreign).run, ['kubectl'])
        with self.assertRaisesRegex(ValueError, 'Unexpected'):
            shared_storage.desired_config('[]')

    def test_every_node_mounts_the_shared_directory(self):
        config = kind_nodes.cluster_config(2, '/var/lib/ai-sdlc/c/shared')
        mount = ('    extraMounts:\n      - hostPath: /var/lib/ai-sdlc/c/shared\n        containerPath: '
                 + kind_nodes.SHARED_PATH + '\n')
        self.assertEqual(config.count(mount), 3, 'control-plane and both workers')
        self.assertNotIn(kind_nodes.CI_LABEL, config, 'no CI worker with shared caches')
        self.assertNotIn('extraMounts', kind_nodes.cluster_config(2))
        for bad in ('relative/dir', '/a"b', '/a\nb'):
            with self.assertRaises(ValueError):
                kind_nodes.cluster_config(2, bad)
        with self.assertRaises(ValueError):
            kind_nodes.cluster_config(0, '/x')

    def test_shared_mount_detection_refuses_partial_mounts(self):
        def run(mounted):
            return lambda args, **_k: SimpleNamespace(stdout=json.dumps(
                [{'Destination': kind_nodes.SHARED_PATH, 'Source': mounted[args[-1]]}] if mounted[args[-1]]
                else [{'Destination': '/x', 'Source': '/y'}]))
        names = ['c-control-plane', 'c-worker']
        both = {'c-control-plane': '/state/c/shared', 'c-worker': '/state/c/shared'}
        self.assertTrue(kind_nodes.shared_mounted(run(both), 'podman', names))
        self.assertEqual(kind_nodes.shared_source(run(both), 'podman', names), '/state/c/shared')
        self.assertIsNone(kind_nodes.shared_source(run({'c-control-plane': None, 'c-worker': None}), 'podman', names))
        with self.assertRaisesRegex(ValueError, 'Only some'):
            kind_nodes.shared_mounted(run({'c-control-plane': '/state/c/shared', 'c-worker': None}), 'podman', names)
        with self.assertRaisesRegex(ValueError, 'different shared storage directories'):
            kind_nodes.shared_source(run({'c-control-plane': '/a', 'c-worker': '/b'}), 'podman', names)

    def test_shared_caches_spread_ci_and_scale_the_quotas(self):
        self.assertEqual(kind_nodes.tekton_pod_template(True)['nodeSelector'], {kind_nodes.ROLE_LABEL: kind_nodes.ROLE_SANDBOX})
        self.assertEqual(kind_nodes.tekton_pod_template()['nodeSelector'], {kind_nodes.CI_LABEL: kind_nodes.CI})
        self.assertEqual(tekton.pod_quotas(workers=3, shared=True), {'candidate': 3, 'fetch': 3})
        self.assertEqual(tekton.pod_quotas(workers=3), {'candidate': 1, 'fetch': 1})
        # 3 + 6 + 6 = 15 pods fit two workers of 9 each only when they spread.
        kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=6, fetch_pods=6, workers=2, shared=True)
        with self.assertRaises(ValueError):
            kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=6, fetch_pods=6, workers=2)
        with self.assertRaisesRegex(ValueError, 'hold 18 pods'):
            kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=10, fetch_pods=6, workers=2, shared=True)
        claims = tekton.cache_claims(['a/b'], kind_nodes.SHARED_CLASS)
        self.assertEqual(claims[0]['spec']['accessModes'], ['ReadWriteMany'])
        self.assertEqual(claims[0]['spec']['storageClassName'], 'ai-sdlc-shared')
        self.assertEqual(tekton.cache_claims(['a/b'])[0]['spec']['accessModes'], ['ReadWriteOnce'])
        self.assertNotIn('storageClassName', tekton.cache_claims(['a/b'])[0]['spec'])

    def test_schedule_removes_the_ci_label_with_shared_caches(self):
        calls = []
        nodes = {'c-control-plane': {'metadata': {'labels': {}}, 'spec': {}},
                 'c-worker': {'metadata': {'labels': {kind_nodes.ROLE_LABEL: 'sandbox', kind_nodes.CI_LABEL: 'true'}},
                              'spec': {'taints': [dict(kind_nodes.SANDBOX_TAINT)]}}}

        def run(args, **_k):
            calls.append(args[1:])
            return SimpleNamespace(stdout=json.dumps(nodes[args[3]]) if args[1:3] == ['get', 'node'] else '')
        kind_nodes.schedule(run, ['kubectl'], {'controlPlane': 'c-control-plane', 'workers': ['c-worker']}, shared=True)
        self.assertEqual([c for c in calls if c[0] != 'get'], [['label', 'node', 'c-worker', 'ai-sdlc.io/ci-node-']])

    def test_option_scope_is_checked_before_any_effect(self):
        for argv in (['status', '--shared-storage'], ['plan', '--shared-storage', '--workers', '0'],
                     ['plan', '--shared-storage'], ['sandbox', '--shared-storage']):
            with patch.object(install, 'tools', side_effect=AssertionError('no tool bootstrap')), \
                 self.assertRaisesRegex(ValueError, 'shared-storage'):
                install.main(argv)


    def test_a_new_cluster_without_workers_is_refused_before_creation(self):
        import os
        import subprocess
        import tempfile
        from pathlib import Path
        calls = []

        def run(command, **_kwargs):
            argv = [str(v) for v in command]
            calls.append(argv)
            return subprocess.CompletedProcess(argv, 0, '')
        with tempfile.TemporaryDirectory() as directory, \
             patch.dict(os.environ, XDG_STATE_HOME=directory), \
             patch.object(install, 'tools', return_value={name: name for name in ('helm', 'kind', 'kubectl')}), \
             patch.object(install.shutil, 'which', return_value='/fixture/provider'), \
             patch.object(install, 'host_prerequisites'), patch.object(install, 'run', side_effect=run), \
             self.assertRaisesRegex(ValueError, 'needs workers'):
            install.main(['local', '--shared-storage', '--cluster', 'c', '--provider', 'podman'])
        self.assertFalse(any(c[:3] == ['kind', 'create', 'cluster'] for c in calls))


    # The committed (default-mode) policy. Shared storage must never edit
    # it; a deliberate policy change updates this digest in the same commit.
    DEFAULT_POLICY_SHA256 = '7f3e53d8288a23c5b5d349f6a03f16587aa5c8a3309a90597ea18c097f28063b'

    def test_the_default_policy_admits_only_the_ci_worker(self):
        import hashlib
        raw = (tekton.CI_DIR / 'admission-policy.yaml').read_bytes()
        self.assertEqual(hashlib.sha256(raw).hexdigest(), self.DEFAULT_POLICY_SHA256,
                         'admission-policy.yaml changed: review it and update DEFAULT_POLICY_SHA256')
        text = raw.decode()
        self.assertEqual(text.count(tekton.CI_PLACEMENT), 1)
        self.assertNotIn(tekton.SHARED_PLACEMENT, text)

    def policy(self, name='ai-sdlc-ci-pipelineruns', expressions=None):
        return {'kind': 'ValidatingAdmissionPolicy', 'metadata': {'name': name},
                'spec': {'validations': [{'expression': e} for e in (expressions or ['x.' + tekton.CI_PLACEMENT])]}}

    def test_the_shared_rewrite_touches_exactly_one_placement_of_one_policy(self):
        other = self.policy('ai-sdlc-ci-other')
        items = tekton.shared_placement([self.policy(), other])
        self.assertIn(tekton.SHARED_PLACEMENT, items[0]['spec']['validations'][0]['expression'])
        self.assertIn(tekton.CI_PLACEMENT, other['spec']['validations'][0]['expression'], 'other policies stay as they are')
        with self.assertRaisesRegex(ValueError, 'exactly one'):
            tekton.shared_placement([self.policy(expressions=['a.' + tekton.CI_PLACEMENT, 'b.' + tekton.CI_PLACEMENT])])
        with self.assertRaisesRegex(ValueError, 'exactly one'):
            tekton.shared_placement([self.policy(expressions=['nothing'])])
        with self.assertRaisesRegex(ValueError, 'exactly one'):
            tekton.shared_placement([other])

    def test_the_host_directory_is_0700_and_repaired(self):
        import os
        import stat
        import tempfile
        from pathlib import Path
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'shared'
            self.assertFalse(shared_storage.ensure_host_dir(path)['repaired'])
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700)
            path.chmod(0o755)
            self.assertTrue(shared_storage.ensure_host_dir(path)['repaired'])
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700)
            with patch.object(os, 'geteuid', return_value=os.geteuid() + 1), \
                 self.assertRaisesRegex(ValueError, 'owned by the installing user'):
                shared_storage.ensure_host_dir(path)
            link = Path(root) / 'link'
            link.symlink_to(path)
            with self.assertRaisesRegex(ValueError, 'symlink'):
                shared_storage.ensure_host_dir(link)
            (Path(root) / 'file').write_text('x')
            with self.assertRaises(ValueError):
                shared_storage.ensure_host_dir(Path(root) / 'file')

    def test_storage_class_binds_at_once_and_deletes_with_its_claim(self):
        value = shared_storage.storage_class()
        self.assertEqual((value['volumeBindingMode'], value['reclaimPolicy'], value['provisioner']),
                         ('Immediate', 'Delete', 'rancher.io/local-path'))

    def test_shared_budget_bounds_every_worker(self):
        # One verification run must still fit one worker (a sealed cache may be RWO).
        with patch.object(kind_nodes, 'policy', return_value=dict(kind_nodes.policy(),
                          pidBudget=dict(kind_nodes.policy()['pidBudget'], workerBaseline=400))):
            with self.assertRaisesRegex(ValueError, 'hold'):
                kind_nodes.pid_budget(3000, sandbox=True, candidate_pods=1, fetch_pods=1, workers=4, shared=True)
        # The control-plane baseline must fit the limit too.
        raised = dict(kind_nodes.policy(), pidBudget=dict(kind_nodes.policy()['pidBudget'], nodeBaseline=5000))
        with patch.object(kind_nodes, 'policy', return_value=raised):
            with self.assertRaises(ValueError):
                kind_nodes.pid_budget(4096, sandbox=False, candidate_pods=1, fetch_pods=1, workers=2, shared=True)
        report = kind_nodes.pid_budget(8192, workers=2, shared=True)
        self.assertTrue(report['sharedStorage'])
        self.assertNotIn('sharedStorage', kind_nodes.pid_budget(8192, workers=2), 'default output unchanged')

    def test_plan_honours_shared_storage(self):
        import contextlib
        import io
        out = io.StringIO()
        with patch.object(install, 'tools', return_value={'helm': 'helm', 'kind': 'kind', 'kubectl': 'kubectl'}), \
             patch.object(install, 'run'), contextlib.redirect_stdout(out):
            install.main(['plan', '--workers', '2', '--shared-storage', '--tekton-ci'])
        plan = json.loads(out.getvalue())
        self.assertEqual(plan['tektonCI']['podQuotas'], {'candidate': 2, 'fetch': 2})
        self.assertTrue(plan['nodeResources']['pidBudget']['sharedStorage'])


if __name__ == '__main__':
    unittest.main()
