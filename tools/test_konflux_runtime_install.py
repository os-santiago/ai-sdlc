import copy
from contextlib import ExitStack
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

import install
import konflux_runtime_install as capability

UID = 'a0a0a0a0-1111-2222-3333-444444444444'
CLUSTER_UID = 'b0b0b0b0-1111-2222-3333-444444444444'


class FrozenRuntimeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.state = Path(self.temporary.name)
        self.resource = None
        self.posts = 0
        self.lose_ack = False
        self.command = Mock(side_effect=self.api)

    def api(self, args, **kwargs):
        if 'create' in args:
            self.posts += 1
            self.resource = json.loads(kwargs['input'])
            self.resource['metadata']['uid'] = UID
            if self.lose_ack:
                raise subprocess.TimeoutExpired(args, 1)
        return subprocess.CompletedProcess(args, 0, json.dumps(self.resource) if self.resource else '')

    def reconcile(self, create=True):
        return capability.namespace(self.command, ['kubectl'], self.state, CLUSTER_UID, create)

    def test_new_capability_and_reapply_keep_original_uid_and_single_post(self):
        first = self.reconcile()
        self.assertEqual(first['namespaceUID'], UID)
        self.assertEqual(self.reconcile(), first)
        self.assertEqual(self.reconcile(False), first)
        self.assertEqual(self.posts, 1)

    def test_lost_ack_recovers_only_existing_nonce_without_second_post(self):
        self.lose_ack = True
        with self.assertRaises(subprocess.TimeoutExpired):
            self.reconcile()
        with self.assertRaisesRegex(ValueError, 'reconciliation'):
            self.reconcile(False)
        self.assertEqual(self.reconcile()['namespaceUID'], UID)
        self.assertEqual(self.posts, 1)

    def test_absent_after_lost_ack_or_replacement_never_recreates(self):
        self.lose_ack = True
        with self.assertRaises(subprocess.TimeoutExpired):
            self.reconcile()
        original = copy.deepcopy(self.resource)
        self.resource = None
        with self.assertRaisesRegex(ValueError, 'recreation'):
            self.reconcile()
        self.resource = original
        self.reconcile()
        for mutation in ('uid', 'nonce', 'security', 'terminating'):
            self.resource = copy.deepcopy(original)
            meta = self.resource['metadata']
            if mutation == 'uid': meta['uid'] = CLUSTER_UID
            if mutation == 'nonce': meta['annotations']['ai-sdlc.io/runtime-create-nonce'] = 'f' * 48
            if mutation == 'security': meta['labels']['pod-security.kubernetes.io/enforce'] = 'privileged'
            if mutation == 'terminating': meta['deletionTimestamp'] = '2026-01-01T00:00:00Z'
            with self.subTest(mutation=mutation), self.assertRaisesRegex(ValueError, 'identity'):
                self.reconcile()
        self.assertEqual(self.posts, 1)

    def test_foreign_namespace_is_not_adopted(self):
        self.resource = {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': capability.NAMESPACE, 'uid': UID}}
        with self.assertRaisesRegex(ValueError, 'adoption'):
            self.reconcile()
        self.assertFalse((self.state / 'runtime-capability.json').exists())
        self.assertEqual(self.posts, 0)

    def test_provider_requires_original_konflux_marker(self):
        with self.assertRaises(ValueError):
            capability.select_provider('auto', self.state, 'cluster')
        marker = {'version': 1, 'cluster': 'cluster', 'provider': 'podman', 'clusterUID': CLUSTER_UID, 'scope': 'isolated-konflux'}
        (self.state / 'konflux-ownership.json').write_text(json.dumps(marker))
        self.assertEqual(capability.select_provider('auto', self.state, 'cluster'), 'podman')
        with self.assertRaises(ValueError): capability.select_provider('docker', self.state, 'cluster')
        with self.assertRaises(ValueError): capability.verify_cluster(self.command, [], self.state, 'cluster', 'podman', UID)
        (self.state / 'ownership.json').write_text('{}')
        with self.assertRaises(ValueError): capability.select_provider('auto', self.state, 'cluster')

    def test_install_lock_excludes_concurrent_reconcile(self):
        with ExitStack() as first, ExitStack() as second:
            capability.lock_state(self.state, first)
            with self.assertRaisesRegex(ValueError, 'Another'):
                capability.lock_state(self.state, second)
        with ExitStack() as third:
            capability.lock_state(self.state, third)

    def test_activation_and_credentials_rejected_before_tools(self):
        base = ['local', '--platform', 'konflux', '--repository', 'os-santiago/saferus']
        for options in (['--profile', 'shadow'], ['--github-auth', 'gh-cli'],
                        ['--environment', 'real-production'], ['--codex-auth-file', '/private/auth.json']):
            with self.subTest(options=options), patch.object(install, 'tools') as tools, self.assertRaises(ValueError):
                install.main(base + options)
            tools.assert_not_called()

    def test_declarative_schema_and_parser_agree_on_frozen_scope(self):
        import install_config
        import jsonschema
        fixture = json.loads((install.ROOT / 'examples/saferus.konflux-frozen.json').read_text())
        schema = json.loads((install.ROOT / 'spec/local-install.schema.json').read_text())
        path = self.state / 'config.json'
        path.write_text(json.dumps(fixture))
        jsonschema.validate(fixture, schema)
        self.assertEqual(install_config.load(path)[0]['platform'], 'konflux')
        for changes in ({'platform': 'remote'}, {'profile': 'verification'},
                        {'repository': 'other/repo'}, {'environment': 'production'},
                        {'independentReview': False}, {'publication': True}, {'merge': True}):
            invalid = dict(fixture, **changes)
            path.write_text(json.dumps(invalid))
            with self.subTest(changes=changes), self.assertRaises(jsonschema.ValidationError):
                jsonschema.validate(invalid, schema)
            with self.assertRaises(ValueError):
                install_config.load(path)

    def test_unfrozen_existing_authority_rejected(self):
        self.resource = {'data': {'AI_SDLC_RUNTIME_MODE': 'maintenance', 'policy.json': '{"freeze":false}'}}
        with self.assertRaisesRegex(ValueError, 'frozen'):
            capability.require_frozen(self.command, [])


if __name__ == '__main__':
    unittest.main()
