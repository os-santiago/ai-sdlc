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

    def connect(self, uid='uid-1', **overrides):
        run = FakeRun({'get namespace kube-system': (0, uid)})
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

    def test_no_sandbox_check_without_execution(self):
        run = FakeRun({'api-versions': (0, 'security.openshift.io/v1'), 'get storageclass': (0, self.storage)})
        self.assertEqual(openshift_install.preflight(run, ['kubectl'], sandbox=False), '')
        self.assertFalse([c for c in run.calls if 'runtimeclass' in c])


if __name__ == '__main__':
    unittest.main()
