import copy
from contextlib import ExitStack
import json
import os
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
        fixture = json.loads((install.ROOT / 'examples/saferus-konflux-frozen.local-install.json').read_text())
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

    def pause_journal_fixture(self):
        state = Path(self.temporary.name) / 'ai-sdlc' / 'cluster'
        state.mkdir(parents=True)
        marker = {'version': 1, 'cluster': 'cluster', 'provider': 'podman',
                  'clusterUID': CLUSTER_UID, 'scope': 'isolated-konflux'}
        (state / 'konflux-ownership.json').write_text(json.dumps(marker))
        return state

    def test_local_refuses_unreleased_upgrade_pause_journal_without_effects(self):
        state = self.pause_journal_fixture()
        journal = {'version': 'ai-sdlc.runtime-upgrade-pause/v1', 'phase': 'planned'}
        args = ['local', '--platform', 'konflux', '--cluster', 'cluster', '--repository', 'os-santiago/saferus']
        for phase, pattern in (('planned', 'incomplete'), ('pausing', 'incomplete'), ('paused', 'remains paused')):
            journal['phase'] = phase
            (state / 'runtime-upgrade-pause.json').write_text(json.dumps(journal))
            with self.subTest(phase=phase), \
                    patch.dict(os.environ, {'XDG_STATE_HOME': self.temporary.name}), \
                    patch.object(install, 'tools', return_value={'helm': 'helm', 'kind': 'kind', 'kubectl': 'kubectl'}), \
                    patch.object(install.konflux_runtime_install, 'lock_state'), \
                    patch.object(install.konflux_runtime_install, 'select_provider', return_value='podman'), \
                    patch.object(install, 'run') as run, \
                    self.assertRaisesRegex(ValueError, pattern):
                install.main(args)
            # The read-only Helm template preview is the only permitted call;
            # no mutating effect may run while the journal is unreleased.
            run.assert_called_once()
            self.assertEqual(run.call_args.args[0][1], 'template')
            self.assertTrue((state / 'runtime-upgrade-pause.json').exists())

    def test_local_proceeds_when_upgrade_pause_journal_is_released(self):
        state = self.pause_journal_fixture()
        journal = {'version': 'ai-sdlc.runtime-upgrade-pause/v1', 'phase': 'paused'}
        (state / 'runtime-upgrade-pause.json').write_text(json.dumps(journal))
        # A completed transition or recover-original republishes an active,
        # unpaused installation report while retaining the journal as custody.
        (state / 'installation.json').write_text(json.dumps({'configuration': 'active', 'runtimePaused': False}))
        (state / 'kubeconfig').write_text('{}')

        def api(args, **kwargs):
            args = [str(a) for a in args]
            if args[:2] == ['kind', 'get'] and 'clusters' in args:
                return subprocess.CompletedProcess(args, 0, 'cluster\n')
            if 'kube-system' in args:
                return subprocess.CompletedProcess(args, 0, CLUSTER_UID)
            return subprocess.CompletedProcess(args, 0, '')

        args = ['local', '--platform', 'konflux', '--cluster', 'cluster', '--repository', 'os-santiago/saferus']
        with patch.dict(os.environ, {'XDG_STATE_HOME': self.temporary.name}), \
                patch.object(install, 'tools', return_value={'helm': 'helm', 'kind': 'kind', 'kubectl': 'kubectl'}), \
                patch.object(install, 'host_prerequisites'), \
                patch.object(install, 'configure_identities', return_value={'authority': {'policyJSON': '{}'}}), \
                patch.object(install.network_probe, 'verify', return_value='verified'), \
                patch.object(install.konflux_runtime_install, 'lock_state'), \
                patch.object(install.konflux_runtime_install, 'select_provider', return_value='podman'), \
                patch.object(install.konflux_runtime_install, 'verify_cluster'), \
                patch.object(install.konflux_runtime_install, 'namespace'), \
                patch.object(install.konflux_runtime_install, 'require_frozen'), \
                patch.object(install, 'run', side_effect=api) as run:
            self.assertEqual(install.main(args), 0)
        self.assertTrue(any('upgrade' in call.args[0] and '--install' in call.args[0]
                            for call in run.call_args_list))
        report = json.loads((state / 'installation.json').read_text())
        self.assertEqual(report['cluster'], 'cluster')
        # local's own report must preserve the released signal; otherwise the
        # retained journal would block every subsequent reconcile forever.
        self.assertFalse(report['runtimePaused'])

    def test_local_report_release_signal_survives_repeated_reconcile(self):
        state = self.pause_journal_fixture()
        journal = {'version': 'ai-sdlc.runtime-upgrade-pause/v1', 'phase': 'paused'}
        (state / 'runtime-upgrade-pause.json').write_text(json.dumps(journal))
        (state / 'installation.json').write_text(json.dumps({'configuration': 'active', 'runtimePaused': False}))
        (state / 'kubeconfig').write_text('{}')

        def api(args, **kwargs):
            args = [str(a) for a in args]
            if args[:2] == ['kind', 'get'] and 'clusters' in args:
                return subprocess.CompletedProcess(args, 0, 'cluster\n')
            if 'kube-system' in args:
                return subprocess.CompletedProcess(args, 0, CLUSTER_UID)
            return subprocess.CompletedProcess(args, 0, '')

        args = ['local', '--platform', 'konflux', '--cluster', 'cluster', '--repository', 'os-santiago/saferus']
        with patch.dict(os.environ, {'XDG_STATE_HOME': self.temporary.name}), \
                patch.object(install, 'tools', return_value={'helm': 'helm', 'kind': 'kind', 'kubectl': 'kubectl'}), \
                patch.object(install, 'host_prerequisites'), \
                patch.object(install, 'configure_identities', return_value={'authority': {'policyJSON': '{}'}}), \
                patch.object(install.network_probe, 'verify', return_value='verified'), \
                patch.object(install.konflux_runtime_install, 'lock_state'), \
                patch.object(install.konflux_runtime_install, 'select_provider', return_value='podman'), \
                patch.object(install.konflux_runtime_install, 'verify_cluster'), \
                patch.object(install.konflux_runtime_install, 'namespace'), \
                patch.object(install.konflux_runtime_install, 'require_frozen'), \
                patch.object(install, 'run', side_effect=api):
            for _ in range(2):
                self.assertEqual(install.main(args), 0)


if __name__ == '__main__':
    unittest.main()
