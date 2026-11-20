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


class Generations(unittest.TestCase):
    def setUp(self):
        import pathlib, tempfile
        self.state = pathlib.Path(tempfile.mkdtemp())

    def test_journals_are_numbered_after_the_first(self):
        self.assertEqual(refresh.journal_paths(self.state, 1)[0].name, 'release-refresh-runtime-upgrade.json')
        self.assertEqual(refresh.journal_paths(self.state, 2),
                         (self.state / 'release-refresh-2-runtime-upgrade.json', self.state / 'release-refresh-2-runtime-backup'))
        self.assertEqual(refresh.latest_generation(self.state), 0)
        refresh.journal_paths(self.state, 1)[0].write_text('{}')
        refresh.journal_paths(self.state, 2)[0].write_text('{}')
        self.assertEqual(refresh.latest_generation(self.state), 2)
        # A gap ends the chain: generation 4 without 3 is not selected.
        refresh.journal_paths(self.state, 4)[0].write_text('{}')
        self.assertEqual(refresh.latest_generation(self.state), 2)

    def test_a_later_generation_starts_from_the_previous_completed_refresh(self):
        import pathlib
        config = Mock(state_dir=self.state)
        upstream_record = journal()
        first = journal(version=refresh.VERSION, mode='release-refresh', parentSHA256='upstream-digest',
                        installationReports=[{'configuration': 'active', 'installationReady': True,
                                              'releaseRefreshUpgradeOperation': 'n'}])
        records = {'model-upstream-runtime-upgrade.json': upstream_record,
                   'release-refresh-runtime-upgrade.json': first}
        digests = {id(upstream_record): 'upstream-digest', id(first): 'first-digest'}
        with patch.object(refresh.files, 'bounded_json', side_effect=lambda path, limit: records[pathlib.Path(path).name]), \
                patch.object(refresh.bootstrap, 'digest', side_effect=lambda record: digests[id(record)]), \
                patch.object(refresh, 'CompletedRepairSource') as repair, \
                patch.object(refresh.data, 'verify_archives') as archives:
            repair.return_value.digest.return_value = 'parent'
            repair.return_value.record = {'migratedProof': {'@schema': 's'}}
            source = refresh.completed_source(config, 2)
            self.assertIs(source.record, first)
            self.assertEqual(source.migrated_proof, {'@schema': 's'})
            self.assertEqual(source.parent_source.record, upstream_record)
            self.assertIn(self.state / 'release-refresh-runtime-backup', [c.args[0] for c in archives.call_args_list])
            first['parentSHA256'] = 'other'
            with self.assertRaisesRegex(ValueError, 'ancestry'):
                refresh.completed_source(config, 2)


class Selection(unittest.TestCase):
    def selected(self, latest, completed, published=True, **inputs):
        class Stop(Exception):
            pass
        seen = []
        def source(config, generation):
            seen.append(generation)
            raise Stop()
        record = {'completed': completed, 'nonce': 'n', 'installationReports': [
            {'configuration': 'active' if published else 'release-refresh-upgrade-paused', 'releaseRefreshUpgradeOperation': 'n'}]}
        with patch.object(refresh, 'latest_generation', return_value=latest), \
                patch.object(refresh.files, 'bounded_json', return_value=record), \
                patch.object(refresh, 'completed_source', side_effect=source), self.assertRaises(Stop):
            refresh.ReleaseRefreshUpgrade(Mock(state_dir=__import__('pathlib').Path('/nonexistent-state')), **inputs)
        return seen[0]

    def test_new_inputs_after_a_completed_refresh_start_the_next_generation(self):
        new = {'run': 1, 'revision': 'a' * 40, 'trust_path': 'p', 'target_path': 'r'}
        self.assertEqual(self.selected(0, None, **new), 1)
        self.assertEqual(self.selected(1, 'active', **new), 2)
        self.assertEqual(self.selected(1, 'active'), 1)        # status of the completed generation
        self.assertEqual(self.selected(2, 'warming', **new), 2)  # resume, never skip an unfinished one
        for state in ('source', 'paused', 'ready', 'staged', 'armed'):
            self.assertEqual(self.selected(3, state, **new), 3)
        # Active but the final report was never published (crash window): resume.
        self.assertEqual(self.selected(2, 'active', published=False, **new), 2)

    def test_only_creating_beyond_the_bound_is_refused(self):
        new = {'run': 1, 'revision': 'a' * 40, 'trust_path': 'p', 'target_path': 'r'}
        bound = refresh.MAX_GENERATIONS
        self.assertEqual(self.selected(bound, 'active'), bound)
        self.assertEqual(self.selected(bound, 'warming', **new), bound)
        with patch.object(refresh, 'latest_generation', return_value=bound), \
                patch.object(refresh, 'finished', return_value=True), \
                patch.object(refresh.files, 'bounded_json', return_value={}), self.assertRaisesRegex(ValueError, 'bound'):
            refresh.ReleaseRefreshUpgrade(Mock(state_dir=__import__('pathlib').Path('/nonexistent-state')), **new)

    def test_finished_requires_the_published_report(self):
        record = {'completed': 'active', 'nonce': 'n', 'transition': None, 'recoveryPause': None,
                  'installationReports': [{'configuration': 'active', 'releaseRefreshUpgradeOperation': 'n'}]}
        self.assertTrue(refresh.finished(record))
        for change in ({'completed': 'armed'}, {'transition': {'to': 'active'}}, {'recoveryPause': {}},
                       {'installationReports': []}, {'nonce': 'other'},
                       {'installationReports': [{'configuration': 'release-refresh-upgrade-paused',
                                                 'releaseRefreshUpgradeOperation': 'n'}]}):
            with self.subTest(change=change):
                self.assertFalse(refresh.finished(dict(record, **change)))


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
                     ['release-refresh-status', '--cluster', 'c', '--model-upstream', 'u'],
                     ['release-refresh-status', '--cluster', 'c', '--target-run', '1', '--target-revision', 'a' * 40]):
            with self.subTest(argv=argv), patch.object(runtime_upgrade_pause.platform, 'lock_state') as lock, \
                    self.assertRaises(ValueError):
                runtime_upgrade_pause.main(argv)
            lock.assert_not_called()


if __name__ == '__main__':
    unittest.main()
