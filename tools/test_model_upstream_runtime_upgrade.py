"""Fail-closed guards of the model upstream switch."""
import unittest
from unittest.mock import Mock, patch

import model_upstream_runtime_upgrade as upgrade
import runtime_upgrade_pause

import copy
import json

GOOD = {'adapter': 'openai', 'url': 'http://10.89.0.1:18080/v1/chat/completions', 'model': 'gemma-4-26b-a4b',
        'cidr': '10.89.0.1/32', 'port': 18080, 'seconds': 300, 'directAnswers': True,
        'callSeconds': 330, 'reviewCallSeconds': 170}
DIGEST = lambda c: 'sha256:' + c * 64
IMAGES = {name: {'digest': DIGEST('b')} for name in upgrade.install_images.ARTIFACT_COMPONENTS
          | {'artifact-builder', 'build-controller', 'deployer', 'browser-verifier'}}
DOCS = [{'kind': 'Deployment', 'metadata': {'name': 'ai-sdlc-intake'}, 'spec': {'replicas': 1}}]
APPROVALS = {'provenance': {'p': 1}, 'release': {'tlsCA': 'CA', 'namespace': 'saferus'}}


class UpstreamDocument(unittest.TestCase):
    def test_reviewed_document_must_be_one_private_host_with_matching_egress(self):
        self.assertEqual(upgrade.validate_upstream(dict(GOOD)), GOOD)
        for change in ({'cidr': '10.89.0.0/24'}, {'cidr': '10.89.0.2/32'}, {'port': 18081},
                       {'url': 'http://10.89.0.1/v1/chat/completions'}, {'url': 'https://10.89.0.1:18080/v1/chat/completions'},
                       {'url': 'http://203.0.113.9:18080/v1/chat/completions', 'cidr': '203.0.113.9/32'},
                       {'url': 'http://host.example:18080/v1/chat/completions'}, {'seconds': 571}, {'seconds': 29}, {'model': ''}, {'callSeconds': 319}, {'callSeconds': 601},
                       {'reviewCallSeconds': 59}, {'reviewCallSeconds': 181},
                       {'extra': True}, {'adapter': 'anthropic'}, {'adapter': 'agent-cli'}, {'directAnswers': 'yes'}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                upgrade.validate_upstream(dict(GOOD, **change))


class Targets(unittest.TestCase):
    def source(self):
        candidate = {'profile': {'maxRepairs': 1, 'maxSeconds': 600, 'maxSteps': 24},
                     'placement': {'WorkerImage': 'w0', 'MailboxImage': 'm0', 'Namespace': 'n'}}
        review = {'profile': {'maxSeconds': 180, 'maxSteps': 8}, 'workerImage': 'w0', 'mailboxImage': 'm0'}
        values = {'components': {k: {'digest': DIGEST('a')} for k in upgrade.repair_targets.COMPONENTS},
                  'migrator': {'digest': DIGEST('a')}, 'verifier': {'digest': DIGEST('a')},
                  'build': {'digest': DIGEST('a'), 'configJSON': '{"profile": {}}'},
                  'release': {'digest': DIGEST('a'), 'targetJSON': json.dumps({'tlsCA': 'CA'}), 'browserProfileJSON': '{}'},
                  'provenance': {'trustJSON': '{}'}, 'intake': {'paused': False},
                  'candidate': {'configJSON': json.dumps(candidate)}, 'review': {'configJSON': json.dumps(review)}}
        return {'values': values, 'documents': DOCS, 'externalRuntime': {},
                'images': {'components': {k: {'digest': DIGEST('a')} for k in upgrade.repair_targets.COMPONENTS},
                           'migrator': {'digest': DIGEST('a')}}}

    def run_targets(self, source=None, upstream=GOOD, approvals=APPROVALS, render_target=lambda v: copy.deepcopy(DOCS)):
        source = source or self.source()
        with patch.object(upgrade.repair_targets, 'validate_provenance'), \
                patch.object(upgrade.repair_targets, 'validate_release'), \
                patch.object(upgrade.repair_targets, 'rotated_profile', return_value='{"renewed": true}'), \
                patch.object(upgrade, 'pause_values', side_effect=lambda v, c: copy.deepcopy(v)), \
                patch.object(upgrade.initial_activation, 'phase_values', side_effect=lambda v, p, c: copy.deepcopy(v)), \
                patch.object(upgrade.prepared_resources, 'documents', return_value={'d': 1}):
            return upgrade.targets(source, {'images': IMAGES}, dict(upstream), approvals,
                                   lambda v: copy.deepcopy(DOCS), render_target, Mock(), 1.0, 'uid')

    def test_closed_delta_sets_gateway_windows_images_and_renewed_approvals(self):
        active = self.run_targets()['active']['values']
        candidate = json.loads(active['candidate']['configJSON'])
        review = json.loads(active['review']['configJSON'])
        worker = upgrade.install_images.REGISTRY + '@' + DIGEST('b')
        self.assertEqual(candidate['profile']['callSeconds'], 330)
        self.assertEqual(candidate['placement'], {'WorkerImage': worker, 'MailboxImage': worker, 'Namespace': 'n'})
        self.assertEqual((review['profile']['callSeconds'], review['workerImage'], review['mailboxImage']), (170, worker, worker))
        self.assertEqual(active['modelGateway']['adapter'], 'openai')
        self.assertEqual(json.loads(active['provenance']['trustJSON']), APPROVALS['provenance'])
        self.assertEqual(active['release']['browserProfileJSON'], '{"renewed": true}')
        self.assertEqual(active['verifier']['digest'], DIGEST('b'))
        self.assertEqual(active['intake'], {'paused': False})

    def test_target_chart_must_render_the_unchanged_installation_identically(self):
        with self.assertRaisesRegex(ValueError, 'identically'):
            self.run_targets(render_target=lambda v: ['changed'])

    def test_existing_upstream_windows_rotation_and_missing_approvals_are_refused(self):
        source = self.source()
        source['values']['modelGateway'] = {'adapter': 'openai'}
        with self.assertRaisesRegex(ValueError, 'already'):
            self.run_targets(source)
        source = self.source()
        candidate = json.loads(source['values']['candidate']['configJSON'])
        candidate['profile']['callSeconds'] = 60
        source['values']['candidate']['configJSON'] = json.dumps(candidate)
        with self.assertRaisesRegex(ValueError, 'call window'):
            self.run_targets(source)
        with self.assertRaisesRegex(ValueError, 'rotate'):
            self.run_targets(approvals={'provenance': {}, 'release': {'tlsCA': 'OTHER'}})
        with self.assertRaisesRegex(ValueError, 'approval'):
            self.run_targets(approvals={'provenance': {}})


class History(unittest.TestCase):
    def test_completed_history_is_admitted_and_only_in_flight_work_blocks(self):
        database = Mock()
        database.sql.return_value = 'f\n'
        upgrade.in_flight(database)
        query = database.sql.call_args[0][0]
        for table in ('execution_requests', 'model_calls', 'review_runs', 'review_model_calls', 'verification_effects',
                      'repair_model_budgets', 'response_handed_off_at'):
            self.assertIn(table, query)
        for history in ('review_feedback', 'execution_repairs', 'deployment_browser'):
            self.assertNotIn(history, query)
        database.sql.return_value = 't\n'
        with self.assertRaisesRegex(ValueError, 'In-flight'):
            upgrade.in_flight(database)
        database.sql.side_effect = RuntimeError('unavailable')
        with self.assertRaises(RuntimeError):
            upgrade.in_flight(database)

    def test_in_flight_work_is_refused_before_the_journal_exists(self):
        item = object.__new__(upgrade.ModelUpstreamUpgrade)
        item.record, item.selection, item.upstream = None, {'images': IMAGES}, dict(GOOD)
        item.approvals = APPROVALS
        item.observe, item.save = Mock(), Mock()
        item.source_browser = Mock()
        item.source_browser.describe.return_value = Targets().source()
        item.source_browser.record = {'resourceUIDs': {'d': 1}}
        item.config, item.helper, item.source_render = Mock(), 'h', Mock(return_value=['doc'])
        item.config.render.return_value = ['doc']
        database = Mock()
        database.sql.return_value = 't\n'
        item.database = Mock(return_value=database)
        item.config.read.side_effect = lambda kind, name, ns: (
            {'immutable': True, 'data': {'ca.crt': 'CA'}, 'metadata': {'uid': 'u'}} if name == 'approved-console-ca'
            else {'data': {'ca.crt': 'CA'}})
        with patch.object(upgrade, 'targets', return_value={}), \
                patch.object(upgrade.build_runtime_install, 'component_call', return_value={
                    'version': 'ai-sdlc.provenance-installation/v1', 'trust': APPROVALS['provenance'], 'trustSHA256': 'x',
                    'profileSHA256': 'y', 'verifierImage': upgrade.install_images.REGISTRY + '@' + DIGEST('b')}), \
                self.assertRaisesRegex(ValueError, 'In-flight'):
            item.begin()
        item.save.assert_not_called()

    def test_first_pause_adopts_the_fenced_snapshot_when_queued_work_progressed(self):
        item = object.__new__(upgrade.ModelUpstreamUpgrade)
        item.record = {'completed': 'source', 'pause': None, 'backup': None, 'transition': None, 'recoveryPause': None,
                       'sourceProof': {'review_runs': {'rows': 1}, 'work_items': {'rows': 1}}}
        item.observe, item.original_database_authority = Mock(), Mock()
        progressed = {'review_runs': {'rows': 2}, 'work_items': {'rows': 1}}
        item.snapshot = Mock(return_value=progressed)
        self.assertEqual(item.capture_first_pause(Mock()), progressed)
        item.snapshot.return_value = {'review_runs': {'rows': 2}}
        with self.assertRaisesRegex(ValueError, 'inventory'):
            item.capture_first_pause(Mock())
        item.record['fencedSourceProof'] = progressed
        with self.assertRaisesRegex(ValueError, 'untouched'):
            item.capture_first_pause(Mock())

    def test_approval_renewal_binds_the_repair_recorded_release_identities(self):
        recorded = {'tlsSecretUID': {'uid': 'u', 'sha256': 'rotated'}}
        item = Mock()
        item.source_browser.record = {'releaseIdentities': recorded}
        with patch.object(upgrade.RepairApprovalPreflight, '__init__', lambda self, u, capture_browser=False:
                          setattr(self, 'original', {'sources': {'release-original-identities': {'tlsSecretUID': {'uid': 'u', 'sha256': 'initial'}}}})):
            preflight = upgrade.UpstreamApprovalPreflight(item, capture_browser=True)
        self.assertEqual(preflight.original['sources']['release-original-identities'], recorded)

    def test_the_switch_uses_the_configuration_fence(self):
        item = object.__new__(upgrade.ModelUpstreamUpgrade)
        item.config, item.helper, item.catalog, item.record = Mock(provider='podman'), 'h', [], {'expiresAt': 1}
        with patch.object(upgrade, 'Fence') as fence:
            item.fence('c', 1)
        self.assertEqual(fence.call_args.kwargs, {'expires_at': 1, 'configuration': True})

    def test_gateway_values_select_the_openai_adapter_with_bounded_benchmark(self):
        values = upgrade.gateway_values(dict(GOOD))
        self.assertEqual(values['adapter'], 'openai')
        self.assertEqual(values['upstream']['credentialSecret'], '')
        self.assertTrue(values['upstream']['directAnswers'])
        self.assertEqual(values['benchmark'], {'promptTokens': 16384, 'completionTokens': 4096})


class Guards(unittest.TestCase):
    def test_begin_requires_publication_and_document_before_any_effect(self):
        for selection, document, approvals in ((None, GOOD, APPROVALS), ({'images': {}}, None, APPROVALS), ({'images': {}}, GOOD, None)):
            item = object.__new__(upgrade.ModelUpstreamUpgrade)
            item.record, item.selection, item.upstream, item.approvals = None, selection, document, approvals
            item.observe, item.save = Mock(), Mock()
            with self.subTest(selection=selection), self.assertRaises(ValueError):
                item.begin()
            item.observe.assert_not_called()
            item.save.assert_not_called()

    def test_cli_accepts_only_the_upstream_document(self):
        for argv in (['model-upstream', '--cluster', 'c', '--model-upstream', 'u'],
                     ['model-upstream', '--cluster', 'c', '--model-upstream', 'u', '--provenance-trust', 'p'],
                     ['model-upstream', '--cluster', 'c', '--provenance-trust', 'p', '--release-target', 'r'],
                     ['model-upstream-status', '--cluster', 'c', '--model-upstream', 'u'],
                     ['model-upstream-status', '--cluster', 'c', '--provenance-trust', 'p'],
                     ['review-repair', '--cluster', 'c', '--provenance-trust', 'p', '--release-target', 'r', '--model-upstream', 'u']):
            with self.subTest(argv=argv), patch.object(runtime_upgrade_pause.platform, 'lock_state') as lock, \
                    self.assertRaises(ValueError):
                runtime_upgrade_pause.main(argv)
            lock.assert_not_called()


if __name__ == '__main__':
    unittest.main()
