"""Fail-closed guards of the release refresh of a model-upstream installation."""
import copy
import json
import unittest
from unittest.mock import Mock, patch

import model_upstream_runtime_upgrade as upstream
import release_refresh_runtime_upgrade as refresh
import runtime_upgrade_pause
from test_model_upstream_runtime_upgrade import APPROVALS, DIGEST, DOCS, IMAGES, Targets

MAAS = {'adapter': 'openai', 'url': 'http://10.89.0.1:18090/v1/chat/completions', 'model': 'claude-sonnet',
        'cidr': '10.89.0.1/32', 'port': 18090, 'seconds': 300, 'directAnswers': False,
        'callSeconds': 330, 'reviewCallSeconds': 170, 'credentialSecret': ''}


def installed():
    """A completed model-upstream installation: gateway and windows set."""
    source = Targets().source()
    values = source['values']
    values['modelGateway'] = upstream.gateway_values(MAAS)
    candidate = json.loads(values['candidate']['configJSON'])
    candidate['profile']['callSeconds'] = 330
    values['candidate']['configJSON'] = json.dumps(candidate, sort_keys=True)
    review = json.loads(values['review']['configJSON'])
    review['profile']['callSeconds'] = 170
    values['review']['configJSON'] = json.dumps(review, sort_keys=True)
    return source


def run_targets(source=None, upstream_document=MAAS, approvals=APPROVALS, render_target=lambda v: copy.deepcopy(DOCS)):
    source = source or installed()
    with patch.object(upstream.repair_targets, 'validate_provenance'), \
            patch.object(upstream.repair_targets, 'validate_release'), \
            patch.object(upstream.repair_targets, 'rotated_profile', return_value='{"renewed": true}'), \
            patch.object(upstream, 'pause_values', side_effect=lambda v, c: copy.deepcopy(v)), \
            patch.object(upstream.initial_activation, 'phase_values', side_effect=lambda v, p, c: copy.deepcopy(v)), \
            patch.object(upstream.prepared_resources, 'documents', return_value={'d': 1}):
        return refresh.targets(source, {'images': IMAGES}, dict(upstream_document), approvals,
                               lambda v: copy.deepcopy(DOCS), render_target, Mock(), 1.0, 'uid')


class RefreshTargets(unittest.TestCase):
    def test_only_images_and_renewed_approvals_change(self):
        source = installed()
        result = run_targets(source)
        active, original = result['active']['values'], source['values']
        worker = upstream.install_images.REGISTRY + '@' + DIGEST('b')
        candidate = json.loads(active['candidate']['configJSON'])
        review = json.loads(active['review']['configJSON'])
        self.assertEqual(candidate['placement']['WorkerImage'], worker)
        self.assertEqual((review['workerImage'], review['mailboxImage']), (worker, worker))
        # Windows and the model upstream are inherited byte for byte.
        self.assertEqual(candidate['profile'], json.loads(original['candidate']['configJSON'])['profile'])
        self.assertEqual(review['profile'], json.loads(original['review']['configJSON'])['profile'])
        self.assertEqual(active['modelGateway'], original['modelGateway'])
        self.assertEqual(active['verifier']['digest'], DIGEST('b'))
        self.assertEqual(json.loads(active['provenance']['trustJSON']), APPROVALS['provenance'])
        self.assertEqual(active['release']['browserProfileJSON'], '{"renewed": true}')
        self.assertEqual(result['source'], source)

    def test_a_different_or_missing_upstream_is_refused(self):
        source = installed()
        source['values'].pop('modelGateway')
        with self.assertRaisesRegex(ValueError, 'exact recorded model upstream'):
            run_targets(source)
        with self.assertRaisesRegex(ValueError, 'exact recorded model upstream'):
            run_targets(upstream_document=dict(MAAS, model='other'))
        with self.assertRaises(ValueError):
            run_targets(upstream_document=dict(MAAS, adapter='agent-cli'))

    def test_render_rotation_and_approval_guards_are_shared(self):
        with self.assertRaisesRegex(ValueError, 'identically'):
            run_targets(render_target=lambda v: ['changed'])
        with self.assertRaisesRegex(ValueError, 'rotate'):
            run_targets(approvals={'provenance': {}, 'release': {'tlsCA': 'OTHER'}})
        with self.assertRaisesRegex(ValueError, 'approval'):
            run_targets(approvals={'provenance': {}})

    def test_incomplete_publication_is_refused(self):
        images = dict(IMAGES)
        images.pop('worker')
        with patch.object(refresh, 'publication', wraps=refresh.publication):
            with self.assertRaisesRegex(ValueError, 'publication'):
                upstream.publication(installed(), {'images': images}, APPROVALS, lambda v: copy.deepcopy(DOCS),
                                     lambda v: copy.deepcopy(DOCS), 1.0, 'uid', 'x')


def journal(**changes):
    record = {'version': upstream.VERSION, 'mode': 'model-upstream', 'completed': 'active', 'transition': None,
              'recoveryPause': None, 'migrationVerified': False, 'migration': {}, 'nonce': 'n',
              'backup': {'phase': 'verified', 'restore': {'state': 'removed'}, 'archives': {}},
              'selection': {'catalog': [{}] * 37, 'images': {'migrator': {'digest': DIGEST('a')}}},
              'installationReports': [{'configuration': 'active', 'installationReady': True,
                                       'modelUpstreamUpgradeOperation': 'n'}],
              'parentSHA256': 'parent'}
    record.update(changes)
    return record


class Source(unittest.TestCase):
    def open(self, record, parent='parent'):
        import pathlib
        config = Mock(state_dir=pathlib.Path('/nonexistent-state'))
        with patch.object(refresh.files, 'bounded_json', return_value=record), \
                patch.object(refresh, 'CompletedRepairSource') as repair, \
                patch.object(refresh.data, 'verify_archives'):
            repair.return_value.digest.return_value = parent
            repair.return_value.record = {'migratedProof': {'@schema': 's'}}
            return refresh.CompletedUpstreamSource(config), repair.return_value

    def test_completed_model_upstream_installation_is_the_only_source(self):
        source, repair = self.open(journal())
        self.assertEqual(source.migrated_proof, {'@schema': 's'})
        self.assertIs(source.pause, repair.pause)
        for change in ({'completed': 'warming'}, {'mode': 'review-repair'}, {'transition': {'to': 'armed'}},
                       {'migrationVerified': True}, {'backup': {'phase': 'capturing'}},
                       {'installationReports': [{'configuration': 'active', 'installationReady': True,
                                                 'modelUpstreamUpgradeOperation': 'other'}]},
                       {'selection': {'catalog': [{}] * 36}}, {'version': 'other'}):
            with self.subTest(change=change), self.assertRaisesRegex(ValueError, 'Completed model upstream'):
                self.open(journal(**change))

    def test_changed_ancestry_is_refused_and_closes_the_parent(self):
        with self.assertRaisesRegex(ValueError, 'ancestry'):
            self.open(journal(), parent='other')


class Upgrade(unittest.TestCase):
    def test_refresh_reuses_the_switch_with_its_own_identity(self):
        self.assertTrue(issubclass(refresh.ReleaseRefreshUpgrade, upstream.ModelUpstreamUpgrade))
        self.assertEqual((refresh.ReleaseRefreshUpgrade.MODE, refresh.ReleaseRefreshUpgrade.VERSION),
                         ('release-refresh', 'ai-sdlc.release-refresh-runtime-upgrade/v1'))
        item = object.__new__(refresh.ReleaseRefreshUpgrade)
        item.source_browser = Mock(migrated_proof={'@schema': 'repair'})
        self.assertEqual(item.migrated_proof(), {'@schema': 'repair'})
        item.config, item.helper, item.catalog, item.record = Mock(provider='podman'), 'h', [], {'expiresAt': 1}
        with patch.object(upstream, 'Fence') as fence:
            item.fence('c', 1)
        self.assertEqual(fence.call_args.kwargs, {'expires_at': 1, 'configuration': True})

    def test_report_names_the_refresh_operation(self):
        item = object.__new__(refresh.ReleaseRefreshUpgrade)
        item.observe, item.save, item.config = Mock(), Mock(), Mock(state_dir=__import__('pathlib').Path('/nonexistent-state'))
        item.parent_hash, item.selection = 'parent', {'run': 1}
        item.record = {'installationSource': {'modelUpstreamUpgradeOperation': 'm'}, 'installationReports': [],
                       'nonce': 'r', 'upstream': MAAS, 'approvalsSHA256': 'a'}
        with patch.object(upstream.platform, 'write_record') as write:
            item.publish_report(active=False)
        report = write.call_args[0][1]
        self.assertEqual((report['releaseRefreshUpgradeOperation'], report['modelUpstreamUpgradeOperation']), ('r', 'm'))
        self.assertEqual(report['configuration'], 'release-refresh-upgrade-paused')
        self.assertFalse(report['issueIntakeEnabled'])


class Cli(unittest.TestCase):
    def test_refresh_takes_both_approvals_and_status_takes_none(self):
        for argv in (['release-refresh', '--cluster', 'c'],
                     ['release-refresh', '--cluster', 'c', '--provenance-trust', 'p'],
                     ['release-refresh', '--cluster', 'c', '--provenance-trust', 'p', '--release-target', 'r',
                      '--model-upstream', 'u'],
                     ['release-refresh-status', '--cluster', 'c', '--provenance-trust', 'p'],
                     ['release-refresh-status', '--cluster', 'c', '--model-upstream', 'u']):
            with self.subTest(argv=argv), patch.object(runtime_upgrade_pause.platform, 'lock_state') as lock, \
                    self.assertRaises(ValueError):
                runtime_upgrade_pause.main(argv)
            lock.assert_not_called()


if __name__ == '__main__':
    unittest.main()
