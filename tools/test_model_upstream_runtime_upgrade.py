"""Fail-closed guards of the model upstream switch."""
import unittest
from unittest.mock import Mock, patch

import model_upstream_runtime_upgrade as upgrade
import runtime_upgrade_pause

GOOD = {'adapter': 'openai', 'url': 'http://10.89.0.1:18080/v1/chat/completions', 'model': 'gemma-4-26b-a4b',
        'cidr': '10.89.0.1/32', 'port': 18080, 'seconds': 300, 'directAnswers': True}


class UpstreamDocument(unittest.TestCase):
    def test_reviewed_document_must_be_one_private_host_with_matching_egress(self):
        self.assertEqual(upgrade.validate_upstream(dict(GOOD)), GOOD)
        for change in ({'cidr': '10.89.0.0/24'}, {'cidr': '10.89.0.2/32'}, {'port': 18081},
                       {'url': 'http://10.89.0.1/v1/chat/completions'}, {'url': 'https://10.89.0.1:18080/v1/chat/completions'},
                       {'url': 'http://203.0.113.9:18080/v1/chat/completions', 'cidr': '203.0.113.9/32'},
                       {'url': 'http://host.example:18080/v1/chat/completions'}, {'seconds': 571}, {'seconds': 29}, {'model': ''},
                       {'extra': True}, {'adapter': 'anthropic'}, {'adapter': 'agent-cli'}, {'directAnswers': 'yes'}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                upgrade.validate_upstream(dict(GOOD, **change))


class Targets(unittest.TestCase):
    def source(self):
        return {'values': {'components': {'worker': {'digest': 'sha256:' + 'a'*64}}}, 'documents': ['doc'],
                'images': {'components': {'worker': {'digest': 'sha256:' + 'a'*64}}}, 'externalRuntime': {}}

    def test_target_chart_must_render_the_unchanged_installation_identically(self):
        selection = {'images': {'worker': {'digest': 'sha256:' + 'b'*64}}}
        with self.assertRaisesRegex(ValueError, 'identically'):
            upgrade.targets(self.source(), selection, GOOD, lambda v: ['doc'], lambda v: ['changed'], Mock())

    def test_existing_upstream_is_not_adopted(self):
        source = self.source()
        source['values']['modelGateway'] = {'adapter': 'openai'}
        with self.assertRaisesRegex(ValueError, 'already'):
            upgrade.targets(source, {'images': {'worker': {'digest': 'sha256:' + 'b'*64}}}, GOOD,
                            lambda v: ['doc'], lambda v: ['doc'], Mock())


class History(unittest.TestCase):
    def test_completed_history_is_admitted_and_only_in_flight_work_blocks(self):
        database = Mock()
        database.sql.return_value = 'f\n'
        upgrade.in_flight(database)
        query = database.sql.call_args[0][0]
        for table in ('execution_requests', 'model_calls', 'review_runs', 'review_model_calls', 'verification_effects'):
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
        item.record, item.selection, item.upstream = None, {'images': {'worker': {'digest': 'sha256:' + 'b'*64}}}, dict(GOOD)
        item.observe, item.save = Mock(), Mock()
        item.source_browser = Mock()
        item.source_browser.describe.return_value = Targets().source()
        item.config, item.helper, item.source_render = Mock(), 'h', Mock(return_value=['doc'])
        item.config.render.return_value = ['doc']
        database = Mock()
        database.sql.return_value = 't\n'
        item.database = Mock(return_value=database)
        with patch.object(upgrade, 'targets', return_value={}), self.assertRaisesRegex(ValueError, 'In-flight'):
            item.begin()
        item.save.assert_not_called()

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
        for selection, document in ((None, GOOD), ({'images': {}}, None)):
            item = object.__new__(upgrade.ModelUpstreamUpgrade)
            item.record, item.selection, item.upstream = None, selection, document
            item.observe, item.save = Mock(), Mock()
            with self.subTest(selection=selection), self.assertRaises(ValueError):
                item.begin()
            item.observe.assert_not_called()
            item.save.assert_not_called()

    def test_cli_accepts_only_the_upstream_document(self):
        for argv in (['model-upstream', '--cluster', 'c'],
                     ['model-upstream', '--cluster', 'c', '--model-upstream', 'u', '--provenance-trust', 'p'],
                     ['model-upstream-status', '--cluster', 'c', '--model-upstream', 'u'],
                     ['review-repair', '--cluster', 'c', '--provenance-trust', 'p', '--release-target', 'r', '--model-upstream', 'u']):
            with self.subTest(argv=argv), patch.object(runtime_upgrade_pause.platform, 'lock_state') as lock, \
                    self.assertRaises(ValueError):
                runtime_upgrade_pause.main(argv)
            lock.assert_not_called()


if __name__ == '__main__':
    unittest.main()
