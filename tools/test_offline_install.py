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
    def fake(self, existing=None, reported=('307', '1536')):
        files = {} if existing is None else {runtime.TASKS_DROPIN: existing}
        calls, shown = [], list(reported)

        def command(args, **_kwargs):
            calls.append(args)
            if args[-3:] == ['-p', 'DefaultTasksMax', '--value']:
                return (shown.pop(0) if len(shown) > 1 else shown[0]) + '\n'
            return ''
        return files, calls, files.get, files.__setitem__, command

    def test_drop_in_is_written_once_and_applied_with_a_reexec(self):
        files, calls, read, write, command = self.fake()
        self.assertEqual(runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n']), 1536)
        self.assertIn('DefaultTasksMax=1536', files[runtime.TASKS_DROPIN])
        self.assertIn(['podman', 'exec', 'n', 'systemctl', 'daemon-reexec'], calls)
        # Reapplication: same file, already applied, nothing re-executed.
        files2, calls2, read2, write2, command2 = self.fake(existing=files[runtime.TASKS_DROPIN], reported=('1536',))
        self.assertEqual(runtime.sandbox_task_limit(read2, write2, command2, ['podman', 'exec', 'n']), 1536)
        self.assertFalse(any('daemon-reexec' in c for c in calls2))

    def test_foreign_drop_in_or_unapplied_limit_is_refused(self):
        _, calls, read, write, command = self.fake(existing='[Manager]\nDefaultTasksMax=infinity\n')
        with self.assertRaisesRegex(ValueError, 'Conflicting'):
            runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n'])
        self.assertEqual(calls, [])
        _, _, read, write, command = self.fake(reported=('307', '307'))
        with self.assertRaisesRegex(ValueError, 'did not apply'):
            runtime.sandbox_task_limit(read, write, command, ['podman', 'exec', 'n'])

    def test_limit_is_bounded_below_the_kind_node_pid_limit(self):
        conf = (runtime.MANIFESTS / 'systemd-tasks.conf').read_text()
        value = int(next(l.split('=')[1] for l in conf.splitlines() if l.startswith('DefaultTasksMax=')))
        self.assertTrue(307 < value < 2048)


if __name__ == '__main__':
    unittest.main()
