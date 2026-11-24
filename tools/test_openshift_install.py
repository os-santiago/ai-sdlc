import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

import openshift_install


def args(**overrides):
    values = dict(platform='openshift', kubeconfig=None, context='admin', command='local', cluster='crc')
    values.update(overrides)
    return SimpleNamespace(**values)


class FakeRun:
    def __init__(self, outputs):
        self.outputs, self.calls = outputs, []

    def __call__(self, command, **kwargs):
        command = [str(c) for c in command]
        self.calls.append(command)
        for needle, (code, out) in self.outputs.items():
            if needle in ' '.join(command):
                if code and kwargs.get('check', True):
                    raise subprocess.CalledProcessError(code, command)
                return SimpleNamespace(returncode=code, stdout=out)
        raise AssertionError('unexpected command ' + ' '.join(command))


class OptionTests(unittest.TestCase):
    def test_existing_cluster_flags_belong_to_openshift(self):
        with self.assertRaises(ValueError):
            openshift_install.validate_options(args(platform='kind', kubeconfig=Path('k')))
        openshift_install.validate_options(args(platform='kind', context=None))
        with self.assertRaises(ValueError):
            openshift_install.validate_options(args(kubeconfig=None))
        with self.assertRaises(ValueError):
            openshift_install.validate_options(args(kubeconfig=Path('k'), command='sandbox'))

    def test_runtime_class_selects_an_existing_openshift_class_only(self):
        openshift_install.validate_options(args(kubeconfig=Path('k'), runtime_class='kata'))
        # kind and Konflux install and use their own ai-sdlc-offline class.
        openshift_install.validate_options(args(platform='kind', context=None, runtime_class='ai-sdlc-offline'))
        with self.assertRaises(ValueError):
            openshift_install.validate_options(args(platform='kind', context=None, runtime_class='kata'))
        openshift_install.validate_options(args(kubeconfig=Path('k')))


class ConnectTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        self.state = self.root / 'state'
        self.state.mkdir()
        self.source = self.root / 'admin.kubeconfig'
        self.source.write_text('apiVersion: v1\n')

    def tearDown(self):
        self.directory.cleanup()

    def connect(self, uid='uid-1', apis='v1\nsecurity.openshift.io/v1\n', **overrides):
        run = FakeRun({'get namespace kube-system': (0, uid), 'api-versions': (0, apis)})
        return openshift_install.connect(run, {'kubectl': 'kubectl'}, self.state, args(kubeconfig=self.source, **overrides))

    def test_first_local_use_records_the_cluster_and_copies_a_private_kubeconfig(self):
        kubeconfig, kubectl, uid, created = self.connect()
        self.assertTrue(created)
        self.assertEqual(uid, 'uid-1')
        self.assertEqual(kubectl[-2:], ['--context', 'admin'])
        self.assertEqual(oct(kubeconfig.stat().st_mode & 0o777), '0o600')
        self.assertEqual(json.loads((self.state / openshift_install.MARKER).read_text())['clusterUID'], 'uid-1')
        self.assertFalse(self.connect()[3])

    def test_a_different_cluster_or_context_is_refused(self):
        self.connect()
        with self.assertRaises(ValueError):
            self.connect(uid='uid-2')
        with self.assertRaises(ValueError):
            self.connect(context='other')

    def test_a_cluster_that_is_not_openshift_is_never_recorded(self):
        with self.assertRaises(ValueError):
            self.connect(apis='v1\napps/v1\n')
        self.assertFalse((self.state / openshift_install.MARKER).exists())

    def test_kind_or_konflux_state_is_refused_before_any_copy(self):
        for name in openshift_install.OTHER_MARKERS:
            with self.subTest(marker=name):
                (self.state / name).write_text('{}')
                with self.assertRaises(ValueError):
                    self.connect()
                self.assertFalse((self.state / 'kubeconfig').exists())
                (self.state / name).unlink()

    def test_kind_paths_refuse_openshift_state(self):
        openshift_install.refuse_other_platform(self.state)
        self.connect()
        with self.assertRaises(ValueError):
            openshift_install.refuse_other_platform(self.state)

    def test_status_never_records_a_cluster(self):
        with self.assertRaises(ValueError):
            self.connect(command='status')
        self.assertFalse((self.state / openshift_install.MARKER).exists())


class PreflightTests(unittest.TestCase):
    storage = json.dumps({'items': [{'metadata': {'annotations': {'storageclass.kubernetes.io/is-default-class': 'true'}}}]})

    def test_requires_openshift_default_storage_and_sandbox_runtime(self):
        ok = {'api-versions': (0, 'v1\nsecurity.openshift.io/v1\n'), 'get storageclass': (0, self.storage),
              'get runtimeclass ai-sdlc-offline': (0, json.dumps({'handler': 'kata'}))}
        self.assertEqual(openshift_install.preflight(FakeRun(ok), ['kubectl'], sandbox=True), 'kata')
        for key, value in (('api-versions', (0, 'v1\n')), ('get storageclass', (0, '{"items": []}')),
                           ('get runtimeclass ai-sdlc-offline', (1, ''))):
            with self.subTest(missing=key), self.assertRaises(ValueError):
                openshift_install.preflight(FakeRun(dict(ok, **{key: value})), ['kubectl'], sandbox=True)

    def test_checks_the_selected_runtime_class(self):
        ok = {'api-versions': (0, 'v1\nsecurity.openshift.io/v1\n'), 'get storageclass': (0, self.storage),
              'get runtimeclass kata ': (0, json.dumps({'handler': 'kata'}))}
        run = FakeRun(ok)
        self.assertEqual(openshift_install.preflight(run, ['kubectl'], sandbox=True, runtime_class='kata'), 'kata')
        self.assertFalse([c for c in run.calls if 'ai-sdlc-offline' in c])
        with self.assertRaisesRegex(ValueError, 'RuntimeClass kata'):
            openshift_install.preflight(FakeRun(dict(ok, **{'get runtimeclass kata ': (1, '')})), ['kubectl'], sandbox=True, runtime_class='kata')

    def test_no_sandbox_check_without_execution(self):
        run = FakeRun({'api-versions': (0, 'security.openshift.io/v1'), 'get storageclass': (0, self.storage)})
        self.assertEqual(openshift_install.preflight(run, ['kubectl'], sandbox=False), '')
        self.assertFalse([c for c in run.calls if 'runtimeclass' in c])


if __name__ == '__main__':
    unittest.main()
