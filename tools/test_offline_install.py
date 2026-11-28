import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
import subprocess
from unittest.mock import patch

import offline_runtime as runtime
import offline_probe as probe


class OfflineRuntimeTests(unittest.TestCase):
    def test_foreign_node_configuration_fails_before_any_mutation(self):
        calls = []
        foreign = '''version = 2
imports = ["/unreviewed/override.toml"]
[plugins."io.containerd.grpc.v1.cri".containerd]
default_runtime_name = "runc"
'''
        def run(args, **_kwargs):
            calls.append(args)
            if args[:3] == ['kind', 'get', 'nodes']:
                return subprocess.CompletedProcess(args, 0, 'test-control-plane\n')
            if args[:2] == ['podman', 'inspect']:
                return subprocess.CompletedProcess(args, 0, 'test\n')
            if args[3:5] == ['test', '-e']:
                return subprocess.CompletedProcess(args, 0 if args[-1] == runtime.NODE_CONFIG else 1, '')
            if args[3:] == ['cat', runtime.NODE_CONFIG]:
                return subprocess.CompletedProcess(args, 0, foreign)
            raise AssertionError('Unexpected command before preflight rejection: ' + repr(args))
        with patch.object(runtime.subprocess, 'run', side_effect=run):
            with self.assertRaisesRegex(ValueError, 'imports'):
                runtime.install(['kubectl'], 'podman', 'kind', 'test', 'uid', Path('/unused'), 'unused')
        self.assertTrue(all('tee' not in args and 'cp' not in args and 'systemctl' not in args for args in calls))

    def test_configuration_preserves_default_and_rejects_conflicts(self):
        base = '''version = 2
[plugins."io.containerd.grpc.v1.cri".containerd]
default_runtime_name = "runc"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
runtime_type = "io.containerd.runc.v2"
'''
        fragment = (runtime.MANIFESTS / 'containerd.toml').read_text()
        desired = runtime.node_configuration(base, fragment)
        self.assertTrue(desired.startswith(base))
        self.assertEqual(runtime.node_configuration(desired, fragment), desired)
        for bad in (base.replace('version = 2', 'version = 3'),
                    'imports = ["/etc/containerd/override.toml"]\n' + base,
                    base.replace('default_runtime_name = "runc"', 'default_runtime_name = "other"'),
                    desired.replace('network-nonexistent', 'x').replace('pod_annotations = []', 'pod_annotations = ["*"]'),
                    desired.replace('# BEGIN AI-SDLC OFFLINE RUNTIME v1', '# foreign'),
                    base + '\n# /etc/containerd/ai-sdlc-offline.toml\n'):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                runtime.node_configuration(bad, fragment)
        # A comment/string containing the exact fragment must not bless a
        # different effective handler such as runc.
        disguised = 'notes = """\n' + fragment + '\n"""\n' + base + '''
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.ai-sdlc-offline]
runtime_type = "io.containerd.runc.v2"
'''
        with self.assertRaisesRegex(ValueError, 'Conflicting'):
            runtime.node_configuration(disguised, fragment)

    def fixture(self, root, entries):
        path = root / 'fixture.tar.bz2'
        with tarfile.open(path, 'w:bz2') as tar:
            for name, value, kind in entries:
                info = tarfile.TarInfo(name)
                info.type = kind
                info.size = len(value) if kind == tarfile.REGTYPE else 0
                if kind == tarfile.SYMTYPE:
                    info.linkname = '/bin/sh'
                tar.addfile(info, io.BytesIO(value) if kind == tarfile.REGTYPE else None)
        raw = path.read_bytes()
        lock = {'size': len(raw), 'sha256': runtime.sha256(raw),
                'files': {'runsc': {'size': 4, 'sha256': runtime.sha256(b'safe')}}}
        return path, lock

    def test_archives_cannot_add_links_paths_duplicates_or_unverified_bytes(self):
        cases = [ [('runsc', b'safe', tarfile.REGTYPE), ('../escape', b'evil', tarfile.REGTYPE)],
                  [('runsc', b'', tarfile.SYMTYPE)],
                  [('runsc', b'safe', tarfile.REGTYPE), ('runsc', b'safe', tarfile.REGTYPE)],
                  [('runsc', b'evil', tarfile.REGTYPE)], [] ]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for entries in cases:
                path, lock = self.fixture(root, entries)
                with self.subTest(entries=entries), self.assertRaises(ValueError):
                    runtime.extract_verified(path, root / 'stage', lock)
            self.assertFalse((root / 'escape').exists())
            path, lock = self.fixture(root, [('runsc', b'safe', tarfile.REGTYPE)])
            runtime.extract_verified(path, root / 'success', lock)
            self.assertEqual((root / 'success/runsc').read_bytes(), b'safe')
            path.write_bytes(b'corrupt')
            with self.assertRaisesRegex(ValueError, 'archive integrity'):
                runtime.extract_verified(path, root / 'corrupt', lock)

    def test_runtime_configuration_does_not_allow_network_overrides(self):
        import tomllib
        value = tomllib.loads((runtime.MANIFESTS / 'runsc.toml').read_text())
        self.assertEqual(value['runsc_config']['network'], 'none')
        self.assertEqual(value['runsc_config']['host-uds'], 'none')
        self.assertEqual(value['runsc_config']['allow-flag-override'], 'false')
        fragment = tomllib.loads((runtime.MANIFESTS / 'containerd.toml').read_text())
        handler = fragment['plugins']['io.containerd.grpc.v1.cri']['containerd']['runtimes'][runtime.HANDLER]
        self.assertEqual(handler['pod_annotations'], [])
        self.assertEqual(handler['container_annotations'], [])
        self.assertEqual(handler['options']['ConfigPath'], runtime.SHIM_CONFIG)

    def test_conformance_does_not_accept_missing_or_mixed_results(self):
        good = {name: 'unreachable' for name in probe.NETWORK_KEYS}
        self.assertEqual(probe.checked_network(json.dumps(good), 'unreachable'), good)
        for bad in ({}, {'tcp4': 'unreachable'}, dict(good, tcp4='reachable'), dict(good, extra='unreachable')):
            with self.assertRaises(ValueError):
                probe.checked_network(json.dumps(bad), 'unreachable')
        local = {name: True for name in probe.LOCAL_KEYS}
        self.assertEqual(probe.checked_local(json.dumps(local)), local)
        for bad in ({}, dict(local, gvisor=False), dict(local, gvisor=1)):
            with self.assertRaises(ValueError):
                probe.checked_local(json.dumps(bad))

    def test_probe_has_explicit_restricted_identity_and_no_credentials(self):
        spec = probe.pod('probe', 'offline', 'image@sha256:fixture', runtime.HANDLER)['spec']
        self.assertEqual(spec['runtimeClassName'], runtime.HANDLER)
        self.assertFalse(spec['automountServiceAccountToken'])
        self.assertFalse(spec['enableServiceLinks'])
        self.assertEqual(spec['volumes'], [{'name': 'workspace', 'emptyDir': {'sizeLimit': '64Mi'}}])
        security = spec['containers'][0]['securityContext']
        self.assertTrue(security['runAsNonRoot'])
        self.assertTrue(security['readOnlyRootFilesystem'])
        self.assertFalse(security['allowPrivilegeEscalation'])
        self.assertEqual(security['capabilities'], {'drop': ['ALL']})
        self.assertLessEqual(spec['activeDeadlineSeconds'], 300)


class SandboxTaskLimitTests(unittest.TestCase):
    def fake(self, existing=None, reported=('307', '768'), pids_max='2048', kubelet='kind: KubeletConfiguration\n'):
        files = {'/sys/fs/cgroup/pids.max': pids_max + '\n', runtime.KUBELET_CONFIG: kubelet}
        if existing is not None:
            files[runtime.TASKS_DROPIN] = existing
        calls, shown = [], list(reported)

        def command(args, **_kwargs):
            calls.append(args)
            if args[-3:] == ['-p', 'DefaultTasksMax', '--value']:
                return (shown.pop(0) if len(shown) > 1 else shown[0]) + '\n'
            return ''
        return files, calls, files.get, files.__setitem__, command

    def desired(self):
        return (runtime.MANIFESTS / 'systemd-tasks.conf').read_text()

    def test_drop_in_is_written_once_and_applied_with_a_reexec(self):
        files, calls, read, write, command = self.fake()
        self.assertEqual(runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n']), 768)
        self.assertIn('DefaultTasksMax=37.5%', files[runtime.TASKS_DROPIN])
        self.assertIn(['podman', 'exec', 'n', 'systemctl', 'daemon-reexec'], calls)
        files2, calls2, read2, write2, command2 = self.fake(existing=self.desired(), reported=('768',))
        self.assertEqual(runtime.sandbox_task_limit(read2, write2, command2, ['podman', 'exec', 'n']), 768)
        self.assertFalse(any('daemon-reexec' in c for c in calls2), 'reapplication changes nothing')

    def test_the_value_follows_the_node_pid_limit(self):
        # 37.5% of the installer default node limit, not a number sized for 2048.
        files, calls, read, write, command = self.fake(pids_max='8192', reported=('1228', '3072'))
        self.assertEqual(runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n']), 3072)
        # A node raised after the drop-in: systemd already resolves the share again.
        files, calls, read, write, command = self.fake(existing=self.desired(), pids_max='16384', reported=('6144',))
        self.assertEqual(runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n']), 6144)
        self.assertFalse(any('daemon-reexec' in c for c in calls))

    def test_the_value_follows_systemds_smallest_base(self):
        # systemd scales the smallest of pids.max, threads-max and pid_max - 1.
        files, calls, read, write, command = self.fake(pids_max='65536', reported=('4915', '12288'))
        files['/proc/sys/kernel/threads-max'] = '32768\n'
        files['/proc/sys/kernel/pid_max'] = '4194304\n'
        self.assertEqual(runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n']), 12288)
        self.assertEqual(runtime.system_tasks_max({'/proc/sys/kernel/pid_max': '4097\n'}.get, 8192), 4096)
        self.assertEqual(runtime.system_tasks_max({}.get, None), None)

    def test_an_earlier_ai_sdlc_value_is_replaced(self):
        files, calls, read, write, command = self.fake(existing=runtime.DROPIN_MARK + ' old\n[Manager]\nDefaultTasksMax=768\n',
                                                       pids_max='8192', reported=('768', '3072'))
        self.assertEqual(runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n']), 3072)
        self.assertEqual(files[runtime.TASKS_DROPIN], self.desired())
        self.assertIn(['podman', 'exec', 'n', 'systemctl', 'daemon-reexec'], calls)

    def test_a_node_without_pid_limit_is_never_lowered(self):
        # Docker kind nodes have no pid limit and a far larger systemd default.
        files, calls, read, write, command = self.fake(pids_max='max', reported=('629145',))
        self.assertEqual(runtime.sandbox_task_limit(read, write, command, ['docker', 'exec', 'n']), 629145)
        self.assertNotIn(runtime.TASKS_DROPIN, files)
        self.assertFalse(any('daemon-reexec' in c for c in calls))
        files, calls, read, write, command = self.fake(pids_max='max', reported=('infinity',))
        self.assertIsNone(runtime.sandbox_task_limit(read, write, command, ['docker', 'exec', 'n']))
        self.assertNotIn(runtime.TASKS_DROPIN, files)

    def test_foreign_drop_in_small_node_or_unapplied_limit_is_refused(self):
        _, calls, read, write, command = self.fake(existing='[Manager]\nDefaultTasksMax=infinity\n')
        with self.assertRaisesRegex(ValueError, 'Conflicting'):
            runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n'])
        _, _, read, write, command = self.fake(pids_max='2000')  # 37.5% is below the 768 pod limit
        with self.assertRaisesRegex(ValueError, 'too small'):
            runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n'])
        _, _, read, write, command = self.fake(reported=('307', '307'))
        with self.assertRaisesRegex(ValueError, 'did not apply'):
            runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n'])

    def test_the_share_is_a_bounded_percentage(self):
        self.assertEqual(runtime.tasks_share('[Manager]\nDefaultTasksMax=37.5%\n'), 3750)
        for value in ('768', '50%', '0%', '12.345%', 'x%'):
            with self.assertRaises(ValueError):
                runtime.tasks_share('[Manager]\nDefaultTasksMax=' + value + '\n')

    @staticmethod
    def moving(files, command):
        def run(args, **kwargs):
            if args[3:5] == ['mv', '-T'] and args[-2] in files:
                files[args[-1]] = files.pop(args[-2])
            if args[3:5] == ['rm', '-f']:
                files.pop(args[-1], None)
            return command(args, **kwargs)
        return run

    def test_every_pod_gets_a_bounded_pid_limit_atomically(self):
        files, calls, read, write, command = self.fake()
        command = self.moving(files, command)
        self.assertEqual(runtime.pod_pid_limit(read, write, command, ['podman', 'exec', 'n'], lambda: 768), 768)
        self.assertTrue(files[runtime.KUBELET_CONFIG].endswith(runtime.KUBELET_MARK + '\npodPidsLimit: 768\n'))
        moves = [c for c in calls if c[3:5] == ['mv', '-T']]
        self.assertEqual(len(moves), 1, 'staged, then moved into place')
        self.assertIn(['podman', 'exec', 'n', 'systemctl', 'restart', 'kubelet'], calls)
        # From kind.yaml or a previous run: unchanged, only confirmed.
        files, calls, read, write, command = self.fake(kubelet='kind: KubeletConfiguration\npodPidsLimit: 768\n')
        runtime.pod_pid_limit(read, write, command, ['podman', 'exec', 'n'], lambda: 768)
        self.assertEqual(calls, [])
        files, calls, read, write, command = self.fake(kubelet='podPidsLimit: 4096\n')
        with self.assertRaisesRegex(ValueError, 'different podPidsLimit'):
            runtime.pod_pid_limit(read, write, command, ['podman', 'exec', 'n'], lambda: 768)

    def test_a_written_but_unapplied_value_restarts_the_kubelet(self):
        # An earlier run moved the file into place and died before the restart.
        files, calls, read, write, command = self.fake(kubelet='kind: KubeletConfiguration\n' + runtime.KUBELET_MARK + '\npodPidsLimit: 768\n')
        reported = iter([None, 768])
        self.assertEqual(runtime.pod_pid_limit(read, write, command, ['podman', 'exec', 'n'], lambda: next(reported)), 768)
        self.assertEqual(calls, [['podman', 'exec', 'n', 'systemctl', 'restart', 'kubelet']])
        files, calls, read, write, command = self.fake(kubelet='podPidsLimit: 768\n')
        with self.assertRaisesRegex(ValueError, 'after a restart'):
            runtime.pod_pid_limit(read, write, command, ['podman', 'exec', 'n'], lambda: None)

    def test_an_earlier_reviewed_value_is_upgraded(self):
        # kind.yaml of the previous revision set 1024 without the marker.
        files, calls, read, write, command = self.fake(kubelet='kind: KubeletConfiguration\npodPidsLimit: 1024\n')
        command = self.moving(files, command)
        self.assertEqual(runtime.pod_pid_limit(read, write, command, ['podman', 'exec', 'n'], lambda: 768), 768)
        self.assertEqual(files[runtime.KUBELET_CONFIG].count('podPidsLimit:'), 1)

    def test_a_kubelet_that_does_not_apply_the_value_is_restored(self):
        original = 'kind: KubeletConfiguration\n'
        files, calls, read, write, command = self.fake(kubelet=original)

        move = self.moving(files, command)
        with self.assertRaisesRegex(ValueError, 'restored'):
            runtime.pod_pid_limit(read, write, move, ['podman', 'exec', 'n'], lambda: None)
        self.assertEqual(files[runtime.KUBELET_CONFIG], original)
        self.assertEqual(sum(1 for c in calls if c[3:] == ['systemctl', 'restart', 'kubelet']), 2)

    def test_limits_are_bounded_and_kind_config_matches(self):
        share = runtime.tasks_share(self.desired())
        pods = json.loads((runtime.MANIFESTS / 'kubelet-pods.json').read_text())['podPidsLimit']
        self.assertTrue(603 < pods, 'above the measured sandbox peak (603)')
        self.assertEqual(2048 * share // 10000, pods, 'the smallest supported node (2048) still reaches the pod limit')
        self.assertLess(share, 5000, 'no scope may take half of the node')
        self.assertIn('podPidsLimit: %d' % pods, (runtime.MANIFESTS.parent / 'kind.yaml').read_text())


if __name__ == '__main__':
    unittest.main()
