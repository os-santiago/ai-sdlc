import json
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import install
import kind_nodes


class FakeNode:
    """A kind node container: stored HostConfig plus its cgroup files."""

    def __init__(self, pids=2048, memory=0, nano_cpus=0, cluster='c', used=300, memory_used=10 ** 9, apply=True):
        self.host = {'PidsLimit': pids, 'Memory': memory, 'NanoCpus': nano_cpus}
        self.labels = {'io.x-k8s.kind.cluster': cluster, 'io.x-k8s.kind.role': 'control-plane'}
        self.used, self.memory_used, self.apply = used, memory_used, apply
        self.calls = []

    def run(self, args, **kwargs):
        self.calls.append(args)
        if args[1] == 'inspect':
            return SimpleNamespace(stdout=json.dumps(self.host) + '|' + json.dumps(self.labels) + '\n', returncode=0)
        if args[1] == 'exec':
            name = args[-1].rsplit('/', 1)[1]
            value = {'pids.max': str(self.host['PidsLimit'] or 'max'), 'pids.current': str(self.used),
                     'memory.current': str(self.memory_used)}[name]
            return SimpleNamespace(stdout=value + '\n', returncode=0)
        if args[1] == 'update':
            if self.apply:
                options = dict(zip(args[2:-1:2], args[3:-1:2]))
                if '--pids-limit' in options:
                    self.host['PidsLimit'] = int(options['--pids-limit'])
                if '--memory' in options:
                    self.host['Memory'] = int(options['--memory'])
                if '--cpus' in options:
                    self.host['NanoCpus'] = int(float(options['--cpus']) * 1e9)
            return SimpleNamespace(stdout='', returncode=0)
        if args[1:3] == ['get', 'nodes']:
            return SimpleNamespace(stdout='c-control-plane\n', returncode=0)
        raise AssertionError(args)

    def updates(self):
        return [c for c in self.calls if c[1] == 'update']


class DesiredResourceTests(unittest.TestCase):
    def test_default_is_8192_or_a_quarter_of_a_small_host_pid_max(self):
        self.assertEqual(kind_nodes.desired(pid_max=4194304), {'pidsLimit': 8192, 'memory': None, 'cpus': None})
        self.assertEqual(kind_nodes.desired(pid_max=16384)['pidsLimit'], 4096)
        with self.assertRaisesRegex(ValueError, 'pid limit'):
            kind_nodes.desired(pid_max=4096)  # a quarter (1024) is below the supported minimum

    def test_explicit_limits_are_bounded(self):
        self.assertEqual(kind_nodes.desired(16384, '12g', '6.5', pid_max=4194304, cpu_count=16),
                         {'pidsLimit': 16384, 'memory': 12 * 1024 ** 3, 'cpus': 6.5})
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
        report = kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=1, fetch_pods=1)
        self.assertEqual(report['required'], 1100 + 4 * 768 + 150)
        self.assertLessEqual(report['required'], 8192)

    def test_quotas_scale_with_the_node_limit(self):
        kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=6, fetch_pods=1)
        with self.assertRaisesRegex(ValueError, 'Tekton CI pod quotas'):
            kind_nodes.pid_budget(8192, sandbox=True, candidate_pods=7, fetch_pods=1)
        kind_nodes.pid_budget(16384, sandbox=True, candidate_pods=16, fetch_pods=4)
        with self.assertRaises(ValueError):
            kind_nodes.pid_budget(8192, candidate_pods=-1)

    def test_policy_matches_the_kubelet_pod_limit(self):
        self.assertEqual(kind_nodes.sandbox_pod_pids(), 768)
        self.assertEqual(kind_nodes.policy()['pidsLimit']['default'], 8192)


class ApplyTests(unittest.TestCase):
    WANT = {'pidsLimit': 8192, 'memory': None, 'cpus': None}

    def test_sets_and_verifies_the_pid_limit_once(self):
        node = FakeNode()
        report = kind_nodes.apply(node.run, 'podman', 'c', 'c-control-plane', self.WANT)
        self.assertEqual(node.updates(), [['podman', 'update', '--pids-limit', '8192', 'c-control-plane']])
        self.assertEqual((report['pidsLimit'], report['changed']), (8192, True))
        again = kind_nodes.apply(node.run, 'podman', 'c', 'c-control-plane', self.WANT)
        self.assertFalse(again['changed'])
        self.assertEqual(len(node.updates()), 1, 'reapplication changes nothing')

    def test_docker_nodes_without_a_limit_get_one(self):
        node = FakeNode(pids=None)
        self.assertTrue(kind_nodes.apply(node.run, 'docker', 'c', 'c-control-plane', self.WANT)['changed'])
        self.assertEqual(node.host['PidsLimit'], 8192)

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
        busy = FakeNode(pids=16384, used=9000)
        with self.assertRaisesRegex(ValueError, 'refusing to lower'):
            kind_nodes.apply(busy.run, 'podman', 'c', 'c-control-plane', self.WANT)
        self.assertEqual(busy.updates(), [])
        idle = FakeNode(pids=16384, used=500)
        self.assertEqual(kind_nodes.apply(idle.run, 'podman', 'c', 'c-control-plane', self.WANT)['pidsLimit'], 8192)
        with self.assertRaisesRegex(ValueError, 'refusing to cap'):
            kind_nodes.apply(FakeNode(memory_used=5 * 1024 ** 3).run, 'podman', 'c', 'c-control-plane',
                             dict(self.WANT, memory=4 * 1024 ** 3))
        with self.assertRaisesRegex(ValueError, 'did not apply'):
            kind_nodes.apply(FakeNode(apply=False).run, 'podman', 'c', 'c-control-plane', self.WANT)

    def test_every_node_of_the_cluster_is_reconciled(self):
        node = FakeNode()
        reports = kind_nodes.apply_cluster(node.run, 'podman', 'c', 'kind', {}, self.WANT)
        self.assertEqual([r['node'] for r in reports], ['c-control-plane'])


if __name__ == '__main__':
    unittest.main()
