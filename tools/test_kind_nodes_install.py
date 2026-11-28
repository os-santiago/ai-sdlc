import contextlib
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import install
import kind_nodes


class FakeNode:
    """A kind node container: stored HostConfig plus its cgroup files."""

    def __init__(self, pids=2048, memory=0, nano_cpus=0, cluster='c', used=300, memory_used=10 ** 9, apply=True,
                 live=None, apply_fields=('--pids-limit', '--memory', '--cpus')):
        self.host = {'PidsLimit': pids, 'Memory': memory, 'NanoCpus': nano_cpus}
        self.labels = {'io.x-k8s.kind.cluster': cluster, 'io.x-k8s.kind.role': 'control-plane'}
        self.used, self.memory_used, self.apply = used, memory_used, apply
        # The live cgroup value; None mirrors the stored HostConfig.
        self.live, self.apply_fields = live, apply_fields
        self.calls = []

    def run(self, args, **kwargs):
        self.calls.append(args)
        if args[1] == 'inspect':
            return SimpleNamespace(stdout=json.dumps(self.host) + '|' + json.dumps(self.labels) + '\n', returncode=0)
        if args[1] == 'exec':
            name = args[-1].rsplit('/', 1)[1]
            live = self.live if self.live is not None else str(self.host['PidsLimit'] or 'max')
            value = {'pids.max': live, 'pids.current': str(self.used),
                     'memory.current': str(self.memory_used)}[name]
            return SimpleNamespace(stdout=value + '\n', returncode=0)
        if args[1] == 'update':
            if self.apply:
                options = {k: v for k, v in zip(args[2:-1:2], args[3:-1:2]) if k in self.apply_fields}
                if '--pids-limit' in options:
                    self.host['PidsLimit'] = int(options['--pids-limit'])
                    self.live = None
                if '--memory' in options:
                    self.host['Memory'] = int(options['--memory'])
                if '--cpus' in options:
                    self.host['NanoCpus'] = int(float(options['--cpus']) * 1e9)
            return SimpleNamespace(stdout='', returncode=0)
        if args[1:3] == ['get', 'nodes']:
            return SimpleNamespace(stdout=''.join(n + '\n' for n in getattr(self, 'names', ['c-control-plane'])), returncode=0)
        raise AssertionError(args)

    def updates(self):
        return [c for c in self.calls if c[1] == 'update']


class DesiredResourceTests(unittest.TestCase):
    def test_default_is_8192_or_a_quarter_of_a_small_host_pid_max(self):
        self.assertEqual(kind_nodes.desired(pid_max=4194304, threads_max=446119),
                         {'pidsLimit': 8192, 'pidsLimitExplicit': False, 'memory': None, 'cpus': None})
        self.assertEqual(kind_nodes.desired(pid_max=16384, threads_max=446119)['pidsLimit'], 4096)
        self.assertEqual(kind_nodes.desired(pid_max=4194304, threads_max=16384)['pidsLimit'], 4096)
        with self.assertRaisesRegex(ValueError, 'pid limit'):
            kind_nodes.desired(pid_max=4096, threads_max=446119)  # a quarter (1024) is below the supported minimum

    def test_explicit_limits_are_bounded(self):
        self.assertEqual(kind_nodes.desired(16384, '12g', '6.5', pid_max=4194304, cpu_count=16, threads_max=446119),
                         {'pidsLimit': 16384, 'pidsLimitExplicit': True, 'memory': 12 * 1024 ** 3, 'cpus': 6.5})
        with self.assertRaisesRegex(ValueError, 'threads-max'):
            kind_nodes.desired(40000, pid_max=4194304, threads_max=65536)  # above half of threads-max
        self.assertEqual(kind_nodes.desired(memory='6144m', pid_max=4194304)['memory'], 6 * 1024 ** 3)
        for pids in (1024, 3000000, '8192', True):
            with self.assertRaises(ValueError):
                kind_nodes.desired(pids, pid_max=4194304)
        with self.assertRaises(ValueError):
            kind_nodes.desired(40000, pid_max=65536)  # above half of the host pid_max
        for memory in ('1g', '8G', '8gb', '-1g', '8'):
            with self.assertRaises(ValueError):
                kind_nodes.desired(memory=memory, pid_max=4194304)
        for cpus in ('0', '17', '1.234', 'x', '-2'):
            with self.assertRaises(ValueError):
                kind_nodes.desired(cpus=cpus, pid_max=4194304, cpu_count=16)

    def test_options_are_kind_installation_scopes_only(self):
        args = SimpleNamespace(command='local', platform='kind', node_pids_limit=None, node_memory=None, node_cpus=None)
        self.assertEqual(kind_nodes.validate_options(args)['memory'], None)
        for command, platform in (('status', 'kind'), ('local', 'openshift'), ('local', 'konflux')):
            with self.assertRaisesRegex(ValueError, 'kind platform'):
                kind_nodes.validate_options(SimpleNamespace(command=command, platform=platform, node_pids_limit=8192,
                                                            node_memory=None, node_cpus=None))
        self.assertIsNone(kind_nodes.validate_options(SimpleNamespace(command='local', platform='openshift', node_pids_limit=None,
                                                                      node_memory=None, node_cpus=None)))

    def test_installer_rejects_node_options_before_any_tool_runs(self):
        with patch.object(install, 'tools', side_effect=AssertionError('no tool bootstrap')), \
             self.assertRaisesRegex(ValueError, 'kind platform'):
            install.main(['status', '--node-pids-limit', '8192'])
        with patch.object(install, 'tools', side_effect=AssertionError('no tool bootstrap')), \
             self.assertRaisesRegex(ValueError, 'cannot hold'):
            install.main(['plan', '--node-pids-limit', '2048', '--tekton-ci', '--tekton-ci-candidate-pods', '2'])


class PidBudgetTests(unittest.TestCase):
    def test_podman_default_cannot_hold_a_verification_run(self):
        # Issue #859: the baseline plus three sandboxed pods exceed 2048.
        with self.assertRaisesRegex(ValueError, 'cannot hold'):
            kind_nodes.pid_budget(2048, sandbox=True)
        self.assertEqual(kind_nodes.pid_budget(2048)['required'], 1100)

    def test_the_default_holds_a_verification_run_and_the_default_ci_quotas(self):
        # Every pod, fetch pods included, counts at its 768-pid bound.
        report = kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=1, fetch_pods=1)
        self.assertEqual(report['required'], 1100 + 5 * 768)
        self.assertLessEqual(report['required'], 8192)

    def test_the_budget_boundary_is_exact(self):
        kind_nodes.pid_budget(4940, sandbox=True, candidate_pods=1, fetch_pods=1)
        with self.assertRaisesRegex(ValueError, 'two runs'):
            kind_nodes.pid_budget(4939, sandbox=True, candidate_pods=1, fetch_pods=1)

    def test_quotas_scale_with_the_node_limit(self):
        kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=5, fetch_pods=1)
        with self.assertRaisesRegex(ValueError, 'Tekton CI pod quotas'):
            kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=6, fetch_pods=1)
        with self.assertRaises(ValueError):
            kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=1, fetch_pods=6)
        kind_nodes.pid_budget(16384, sandbox=True, candidate_pods=14, fetch_pods=1)
        with self.assertRaises(ValueError):
            kind_nodes.pid_budget(8192, candidate_pods=-1)

    def test_policy_matches_the_kubelet_pod_limit(self):
        self.assertEqual(kind_nodes.sandbox_pod_pids(), 768)
        self.assertEqual(kind_nodes.policy()['pidsLimit']['default'], 8192)


class ApplyTests(unittest.TestCase):
    WANT = {'pidsLimit': 8192, 'pidsLimitExplicit': True, 'memory': None, 'cpus': None}

    def test_sets_and_verifies_the_pid_limit_once(self):
        node = FakeNode()
        report = kind_nodes.apply(node.run, 'podman', 'c', 'c-control-plane', self.WANT)
        self.assertEqual(node.updates(), [['podman', 'update', '--pids-limit', '8192', 'c-control-plane']])
        self.assertEqual((report['pidsLimit'], report['changed']), (8192, True))
        again = kind_nodes.apply(node.run, 'podman', 'c', 'c-control-plane', self.WANT)
        self.assertFalse(again['changed'])
        # The pid limit is always rewritten: podman nests the node cgroup and
        # an update sets both levels; it is idempotent.
        self.assertEqual(node.updates()[-1], ['podman', 'update', '--pids-limit', '8192', 'c-control-plane'])

    def test_docker_nodes_without_a_limit_get_one(self):
        node = FakeNode(pids=None)
        self.assertTrue(kind_nodes.apply(node.run, 'docker', 'c', 'c-control-plane', self.WANT)['changed'])
        self.assertEqual(node.host['PidsLimit'], 8192)
        busy = FakeNode(pids=None, used=9000)
        with self.assertRaisesRegex(ValueError, 'refusing to lower'):
            kind_nodes.apply(busy.run, 'docker', 'c', 'c-control-plane', self.WANT)
        self.assertEqual(busy.updates(), [])

    def test_a_default_never_lowers_an_operator_limit(self):
        node = FakeNode(pids=12288)
        report = kind_nodes.apply(node.run, 'podman', 'c', 'c-control-plane', dict(self.WANT, pidsLimitExplicit=False))
        self.assertEqual((report['pidsLimit'], report['changed']), (12288, False))
        legacy = FakeNode(pids=2048)
        self.assertEqual(kind_nodes.apply(legacy.run, 'podman', 'c', 'c-control-plane',
                                          dict(self.WANT, pidsLimitExplicit=False))['pidsLimit'], 8192)
        explicit = FakeNode(pids=12288)
        self.assertEqual(kind_nodes.apply(explicit.run, 'podman', 'c', 'c-control-plane', self.WANT)['pidsLimit'], 8192)

    def test_a_stored_limit_above_the_target_needs_headroom(self):
        # Live cgroup already at the target; only the stored value is above it.
        busy = FakeNode(pids=16384, live='8192', used=7500)
        with self.assertRaisesRegex(ValueError, 'refusing to lower'):
            kind_nodes.apply(busy.run, 'podman', 'c', 'c-control-plane', self.WANT)
        self.assertEqual(busy.updates(), [])

    def test_the_effective_limit_is_the_smallest_node_target(self):
        limits = {'c-control-plane': 16384, 'c-worker': 8192, 'c-worker2': 12288}

        def run(args, **_kwargs):
            if args[1:3] == ['get', 'nodes']:
                return SimpleNamespace(stdout='\n'.join(limits) + '\n')
            return SimpleNamespace(stdout=json.dumps({'PidsLimit': limits[args[-1]]}) + '|' + json.dumps(
                {'io.x-k8s.kind.cluster': 'c', 'io.x-k8s.kind.role': 'worker'}))
        default = dict(self.WANT, pidsLimitExplicit=False)
        self.assertEqual(kind_nodes.effective_limit(run, 'podman', 'c', 'kind', {}, default), 8192)
        self.assertEqual(kind_nodes.effective_limit(run, 'podman', 'c', 'kind', {}, default, ['c-control-plane', 'c-worker2']), 12288)
        self.assertEqual(kind_nodes.effective_limit(run, 'podman', 'c', 'kind', {}, dict(self.WANT, pidsLimit=4096)), 4096)

    def test_live_drift_above_the_target_needs_headroom(self):
        # Stored configuration already at the target, live cgroup unlimited.
        busy = FakeNode(pids=8192, live='max', used=7500)
        with self.assertRaisesRegex(ValueError, 'refusing to lower'):
            kind_nodes.apply(busy.run, 'podman', 'c', 'c-control-plane', self.WANT)
        self.assertEqual(busy.updates(), [])
        idle = FakeNode(pids=8192, live='16384', used=500)
        self.assertTrue(kind_nodes.apply(idle.run, 'podman', 'c', 'c-control-plane', self.WANT)['changed'])

    def test_live_drift_is_applied_again(self):
        node = FakeNode(pids=8192, live='2048')
        self.assertTrue(kind_nodes.apply(node.run, 'podman', 'c', 'c-control-plane', self.WANT)['changed'])
        self.assertEqual(node.updates(), [['podman', 'update', '--pids-limit', '8192', 'c-control-plane']])

    def test_optional_memory_and_cpu_caps(self):
        node = FakeNode()
        want = {'pidsLimit': 8192, 'memory': 8 * 1024 ** 3, 'cpus': 6.5}
        report = kind_nodes.apply(node.run, 'podman', 'c', 'c-control-plane', want)
        self.assertEqual(node.updates(), [['podman', 'update', '--pids-limit', '8192', '--memory', str(8 * 1024 ** 3),
                                           '--memory-swap', str(8 * 1024 ** 3), '--cpus', '6.5', 'c-control-plane']])
        self.assertEqual((report['memory'], report['cpus']), (8 * 1024 ** 3, 6.5))
        # Omitted later: existing caps are kept, not removed.
        report = kind_nodes.apply(node.run, 'podman', 'c', 'c-control-plane', self.WANT)
        self.assertEqual((report['memory'], report['cpus'], report['changed']), (8 * 1024 ** 3, 6.5, False))

    def test_refuses_foreign_containers_unsafe_lowering_and_unapplied_limits(self):
        with self.assertRaisesRegex(ValueError, 'ownership'):
            kind_nodes.apply(FakeNode(cluster='other').run, 'podman', 'c', 'c-control-plane', self.WANT)
        busy = FakeNode(pids=16384, used=7500)  # fits below 8192, but not with 10% headroom
        with self.assertRaisesRegex(ValueError, 'refusing to lower'):
            kind_nodes.apply(busy.run, 'podman', 'c', 'c-control-plane', self.WANT)
        self.assertEqual(busy.updates(), [])
        idle = FakeNode(pids=16384, used=7000)
        self.assertEqual(kind_nodes.apply(idle.run, 'podman', 'c', 'c-control-plane', self.WANT)['pidsLimit'], 8192)
        with self.assertRaisesRegex(ValueError, 'refusing to cap'):
            kind_nodes.apply(FakeNode(memory_used=int(3.8 * 1024 ** 3)).run, 'podman', 'c', 'c-control-plane',
                             dict(self.WANT, memory=4 * 1024 ** 3))
        with self.assertRaisesRegex(ValueError, 'did not apply'):
            kind_nodes.apply(FakeNode(apply=False).run, 'podman', 'c', 'c-control-plane', self.WANT)
        # The live cgroup is checked, not only the stored configuration.
        stuck = FakeNode(apply=False, pids=8192, live='2048')
        with self.assertRaisesRegex(ValueError, 'did not apply'):
            kind_nodes.apply(stuck.run, 'podman', 'c', 'c-control-plane', self.WANT)
        for field, want in (('--memory', dict(self.WANT, memory=8 * 1024 ** 3)), ('--cpus', dict(self.WANT, cpus=4.0))):
            partial = FakeNode(apply_fields=tuple(f for f in ('--pids-limit', '--memory', '--cpus') if f != field))
            with self.assertRaisesRegex(ValueError, 'did not apply'):
                kind_nodes.apply(partial.run, 'podman', 'c', 'c-control-plane', want)

    def test_every_node_of_the_cluster_is_reconciled(self):
        with tempfile.TemporaryDirectory() as root:
            Path(root, 'cgroup.controllers').write_text('pids memory cpu\n')
            node = FakeNode()
            node.names = ['c-control-plane', 'c-worker', 'c-worker2']
            reports = kind_nodes.apply_cluster(node.run, 'podman', 'c', 'kind', {}, self.WANT, cgroup_root=Path(root))
            self.assertEqual([r['node'] for r in reports], ['c-control-plane', 'c-worker', 'c-worker2'])
            self.assertEqual([r['changed'] for r in reports], [True, False, False], 'one shared fake: only the first changed')
            self.assertEqual([u[-1] for u in node.updates()], ['c-control-plane', 'c-worker', 'c-worker2'])
            self.assertEqual(sum(1 for c in node.calls if c[1] == 'inspect'), 6, 'every node inspected and verified')

    def test_cgroup_v1_hosts_are_refused_before_any_update(self):
        node = FakeNode()
        with tempfile.TemporaryDirectory() as root, self.assertRaisesRegex(ValueError, 'cgroup v1'):
            kind_nodes.apply_cluster(node.run, 'podman', 'c', 'kind', {}, self.WANT, cgroup_root=Path(root))
        self.assertEqual(node.calls, [])




class WorkerTopologyTests(unittest.TestCase):
    def test_zero_workers_is_the_committed_kind_manifest(self):
        self.assertEqual(kind_nodes.cluster_config(0), (kind_nodes.LOCAL / 'kind.yaml').read_text())
        self.assertEqual(kind_nodes.cluster_config(None), (kind_nodes.LOCAL / 'kind.yaml').read_text())

    def test_workers_share_the_pinned_image_and_carry_the_sandbox_taint(self):
        config = kind_nodes.cluster_config(2)
        image = kind_nodes.IMAGE_LINE.findall((kind_nodes.LOCAL / 'kind.yaml').read_text())[0]
        self.assertEqual(config.count('  - role: worker\n'), 2)
        self.assertEqual(config.count('    image: ' + image + '\n'), 3)
        self.assertEqual(config.count('      ai-sdlc.io/node-role: sandbox\n'), 2)
        self.assertEqual(config.count('            - key: ai-sdlc.io/sandbox\n'), 2)
        self.assertTrue(config.startswith((kind_nodes.LOCAL / 'kind.yaml').read_text().rstrip('\n')))
        for workers in (-1, 5, '2', True):
            with self.assertRaises(ValueError):
                kind_nodes.validate_workers(workers)

    def fake(self, nodes):
        def run(args, **_kwargs):
            if args[1:3] == ['get', 'nodes']:
                return SimpleNamespace(stdout='\n'.join(nodes) + '\n')
            if args[1] == 'inspect' and 'HostConfig' in args[3]:
                cluster, role = nodes[args[-1]]
                return SimpleNamespace(stdout='{"PidsLimit": 8192}|' + json.dumps(
                    {'io.x-k8s.kind.cluster': cluster, 'io.x-k8s.kind.role': role}))
            if args[1] == 'inspect':
                return SimpleNamespace(stdout=json.dumps({'kind': {'IPAddress': '10.89.0.%d' % (10 + sorted(nodes).index(args[-1]))}}))
            raise AssertionError(args)
        return run

    def test_topology_accepts_only_kind_naming_and_labels(self):
        nodes = {'c-control-plane': ('c', 'control-plane'), 'c-worker': ('c', 'worker'), 'c-worker2': ('c', 'worker')}
        topology = kind_nodes.topology(self.fake(nodes), 'podman', 'kind', 'c', {})
        self.assertEqual(topology, {'controlPlane': 'c-control-plane', 'workers': ['c-worker', 'c-worker2']})
        self.assertEqual(kind_nodes.sandbox_nodes(topology), ['c-worker', 'c-worker2'])
        self.assertEqual(kind_nodes.sandbox_nodes({'controlPlane': 'c-control-plane', 'workers': []}), ['c-control-plane'])
        self.assertEqual(kind_nodes.node_addresses(self.fake(nodes), 'podman', ['c-control-plane', 'c-worker']),
                         {'c-control-plane': '10.89.0.10', 'c-worker': '10.89.0.11'})
        for bad in ({'c-control-plane': ('other', 'control-plane')},
                    {'c-control-plane': ('c', 'control-plane'), 'c-extra': ('c', 'worker')},
                    {'c-worker': ('c', 'worker')}):
            with self.assertRaises(ValueError):
                kind_nodes.topology(self.fake(bad), 'podman', 'kind', 'c', {})

    def test_schedule_untaints_the_control_plane_and_restores_worker_marks(self):
        calls = []
        nodes = {'c-control-plane': {'metadata': {'labels': {}}, 'spec': {'taints': [{'key': kind_nodes.CONTROL_PLANE_TAINT, 'effect': 'NoSchedule'}]}},
                 'c-worker': {'metadata': {'labels': {kind_nodes.ROLE_LABEL: 'sandbox'}}, 'spec': {'taints': [dict(kind_nodes.SANDBOX_TAINT)]}},
                 'c-worker2': {'metadata': {'labels': {}}, 'spec': {}}}

        def run(args, **_kwargs):
            calls.append(args[1:])
            if args[1:3] == ['get', 'node']:
                return SimpleNamespace(stdout=json.dumps(nodes[args[3]]))
            return SimpleNamespace(stdout='')
        kind_nodes.schedule(run, ['kubectl'], {'controlPlane': 'c-control-plane', 'workers': ['c-worker', 'c-worker2']})
        changes = [c for c in calls if c[0] != 'get']
        self.assertEqual(changes, [['taint', 'node', 'c-control-plane', kind_nodes.CONTROL_PLANE_TAINT + ':NoSchedule-'],
                                   ['taint', 'node', 'c-worker2', '--overwrite', 'ai-sdlc.io/sandbox=true:NoSchedule'],
                                   ['label', 'node', 'c-worker2', '--overwrite', 'ai-sdlc.io/node-role=sandbox']])
        calls.clear()
        kind_nodes.schedule(run, ['kubectl'], {'controlPlane': 'c-control-plane', 'workers': []})
        self.assertEqual(calls, [], 'a single-node cluster is left as it is')

    def test_sandboxed_and_ci_pods_select_prepared_workers(self):
        scheduling = kind_nodes.runtime_class_scheduling()
        self.assertEqual(scheduling['nodeSelector'], {kind_nodes.READY_LABEL: kind_nodes.READY})
        self.assertEqual(scheduling['tolerations'], [{'key': 'ai-sdlc.io/sandbox', 'operator': 'Equal', 'value': 'true',
                                                      'effect': 'NoSchedule'}])
        template = kind_nodes.tekton_pod_template()
        self.assertEqual(template['nodeSelector'], {kind_nodes.ROLE_LABEL: kind_nodes.ROLE_SANDBOX})
        self.assertEqual(template['tolerations'], scheduling['tolerations'])

    def test_installer_refuses_workers_outside_kind_installation_scopes(self):
        for argv in (['status', '--workers', '2'], ['credentials', '--github-auth', 'gh-cli', '--workers', '1'],
                     ['plan', '--workers', '9']):
            with patch.object(install, 'tools', side_effect=AssertionError('no tool bootstrap')), \
                 self.assertRaisesRegex(ValueError, 'workers'):
                install.main(argv)


class WorkerBudgetTests(unittest.TestCase):
    def test_worker_max_pods_hold_the_pid_limit(self):
        # (8192 - 400) // 768 - 1 = 9 pods, plus kindnet, kube-proxy and one helper slot.
        self.assertEqual(kind_nodes.worker_capacity(8192), 9)
        self.assertEqual(kind_nodes.max_pods(8192), 12)
        for limit in (4096, 8192, 16384):
            self.assertLessEqual(400 + (kind_nodes.worker_capacity(limit) + 1) * 768, limit)

    def test_quotas_scale_with_the_worker_count(self):
        report = kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=2, fetch_pods=2, workers=2)
        self.assertEqual((report['workerPodCapacity'], report['requiredPods']), (9, 7))
        kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=10, fetch_pods=5, workers=2)
        with self.assertRaisesRegex(ValueError, 'workers'):
            kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=10, fetch_pods=6, workers=2)
        with self.assertRaisesRegex(ValueError, 'workers'):
            kind_nodes.pid_budget(2048, sandbox=True, candidate_pods=1, fetch_pods=1, workers=1)

    def test_plan_reports_scaled_default_quotas(self):
        out = io.StringIO()
        with patch.object(install, 'tools', return_value={'helm': 'helm', 'kind': 'kind', 'kubectl': 'kubectl'}), \
             patch.object(install, 'run'), contextlib.redirect_stdout(out):
            install.main(['plan', '--workers', '2', '--tekton-ci'])
        plan = json.loads(out.getvalue())
        self.assertEqual(plan['tektonCI']['podQuotas'], {'candidate': 2, 'fetch': 2})
        self.assertEqual(plan['nodeResources']['workers'], 2)
        self.assertEqual(plan['nodeResources']['pidBudget']['workerMaxPods'], 12)


class InstallerWiringTests(unittest.TestCase):
    """local and sandbox reconcile the nodes, against installed quotas, before any later effect."""

    class Reached(Exception):
        pass

    def drive(self, argv, installed_candidate=0, runtime_class=False, installed_fetch=0, node_limit=8192, existing=True,
              calls=None):
        import os
        import subprocess
        import offline_runtime
        applied = []

        def run(command, **kwargs):
            argv_ = [str(v) for v in command]
            if calls is not None:
                calls.append(argv_)
            if argv_[:3] == ['kind', 'get', 'clusters']:
                return subprocess.CompletedProcess(argv_, 0, 'c\n' if existing else '')
            if argv_[:3] == ['kind', 'export', 'kubeconfig']:
                Path(argv_[-1]).write_text('fixture only\n')
            if 'kube-system' in argv_ and 'jsonpath={.metadata.uid}' in argv_:
                return subprocess.CompletedProcess(argv_, 0, 'uid')
            if argv_[:3] == ['kind', 'get', 'nodes']:
                return subprocess.CompletedProcess(argv_, 0, 'c-control-plane\n')
            if argv_[:2] == ['podman', 'inspect'] and 'HostConfig' in argv_[3]:
                return subprocess.CompletedProcess(argv_, 0, json.dumps({'PidsLimit': node_limit}) + '|' + json.dumps(
                    {'io.x-k8s.kind.cluster': 'c', 'io.x-k8s.kind.role': 'control-plane'}))
            if 'resourcequota' in argv_:
                return subprocess.CompletedProcess(argv_, 0, json.dumps({'items': [
                    {'metadata': {'name': 'ai-sdlc-ci-candidate-pods'}, 'spec': {'hard': {'pods': str(installed_candidate)}}},
                    {'metadata': {'name': 'ai-sdlc-ci-fetch-pods'}, 'spec': {'hard': {'pods': str(installed_fetch)}}}]}))
            if 'runtimeclass' in argv_:
                return subprocess.CompletedProcess(argv_, 0, 'runtimeclass.node.k8s.io/ai-sdlc-offline\n' if runtime_class else '')
            return subprocess.CompletedProcess(argv_, 0, '')

        def apply_cluster(*args, **kwargs):
            applied.append(args)
            raise self.Reached()
        policy = {'metadata': {'annotations': {'ai-sdlc.io/cluster-uid': 'uid'}},
                  'data': {'policy.json': json.dumps({'repository': 'os-santiago/ai-sdlc', 'environment': 'ai-sdlc-production'})}}
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / 'ai-sdlc' / 'c'
            state.mkdir(parents=True)
            (state / 'ownership.json').write_text(json.dumps({'cluster': 'c', 'provider': 'podman', 'clusterUID': 'uid'}))
            with patch.dict(os.environ, XDG_STATE_HOME=directory), \
                 patch.object(install, 'tools', return_value={name: name for name in ('helm', 'kind', 'kubectl')}), \
                 patch.object(install.shutil, 'which', return_value='/fixture/provider'), \
                 patch.object(install, 'host_prerequisites'), patch.object(install, 'run', side_effect=run), \
                 patch.object(install, 'read_resource', return_value=policy), \
                 patch.object(kind_nodes, 'apply_cluster', side_effect=apply_cluster), \
                 patch.object(offline_runtime, 'install', side_effect=AssertionError('nodes first')):
                install.main(argv + ['--cluster', 'c', '--provider', 'podman'])
        return applied

    def test_local_reconciles_the_nodes(self):
        with self.assertRaises(self.Reached):
            self.drive(['local'])

    def test_sandbox_reconciles_the_nodes_before_the_offline_runtime(self):
        with self.assertRaises(self.Reached):
            self.drive(['sandbox'])

    def test_a_new_cluster_is_refused_before_it_is_created(self):
        calls = []
        with self.assertRaisesRegex(ValueError, 'cannot hold'):
            self.drive(['local', '--node-pids-limit', '2048', '--tekton-ci', '--tekton-ci-candidate-pods', '2'],
                       existing=False, calls=calls)
        self.assertFalse(any(c[:3] == ['kind', 'create', 'cluster'] for c in calls), 'refused before kind create')

    def test_installed_quotas_count_when_tekton_ci_is_omitted(self):
        # 1100 + 6 x 768 = 5708 > 4096; the requested quotas alone (none) would fit.
        with self.assertRaisesRegex(ValueError, 'cannot hold'):
            self.drive(['local', '--node-pids-limit', '4096'], installed_candidate=6)
        with self.assertRaises(self.Reached):
            self.drive(['local', '--node-pids-limit', '4096'], installed_candidate=0)
        with self.assertRaisesRegex(ValueError, 'cannot hold'):
            self.drive(['local', '--node-pids-limit', '4096'], installed_fetch=6)

    def test_the_budget_uses_the_larger_limit_a_default_keeps(self):
        # After --node-pids-limit 16384 with ten candidates, a plain run keeps
        # 16384 and must not be refused against the 8192 default.
        with self.assertRaises(self.Reached):
            self.drive(['local'], installed_candidate=10, node_limit=16384)
        with self.assertRaises(self.Reached):
            self.drive(['sandbox'], installed_candidate=10, node_limit=16384)
        with self.assertRaisesRegex(ValueError, 'limit 8192 cannot hold'):
            self.drive(['local'], installed_candidate=10, node_limit=4096)
        with self.assertRaisesRegex(ValueError, 'limit 8192 cannot hold'):
            self.drive(['local', '--node-pids-limit', '8192'], installed_candidate=10, node_limit=16384)

    def test_a_verification_run_counts_for_sandbox_and_an_installed_runtime(self):
        # 1100 + 3 x 768 = 3404 > 3000.
        with self.assertRaisesRegex(ValueError, 'cannot hold'):
            self.drive(['sandbox', '--node-pids-limit', '3000'])
        with self.assertRaisesRegex(ValueError, 'cannot hold'):
            self.drive(['local', '--node-pids-limit', '3000'], runtime_class=True)
        with self.assertRaises(self.Reached):
            self.drive(['local', '--node-pids-limit', '3000'])


if __name__ == '__main__':
    unittest.main()
