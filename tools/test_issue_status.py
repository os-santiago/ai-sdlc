import json
import subprocess
import unittest
from unittest.mock import patch

import issue_status


REPOSITORY = 'owner/repo'
ISSUE = 42


def core(**overrides):
    base = {'now': '2026-01-01T00:00:00Z',
            'revision': {'generation': 2, 'observedAt': '2026-01-01T00:00:00Z'},
            'intakeCursor': {'watermark': '2026-01-01T00:00:00Z', 'nextPoll': None,
                             'failures': 0, 'lagSeconds': 30.0},
            'admission': {'decision': 'accepted', 'risk': 'low', 'reasonCount': 1,
                          'requiredGateCount': 0, 'createdAt': '2026-01-01T00:00:00Z'},
            'admissionGeneration': 2, 'planningGeneration': 2,
            'plan': {'plans': 1, 'itemStatuses': {'ready': 1}},
            'attempts': {'count': 0, 'maxAttempt': 0, 'unsafe': False},
            'releaseGates': {}}
    base.update(overrides)
    return base


def observing(**sections):
    return dict({'core': core(), 'execution': None, 'model': None,
                 'evidence': None, 'release': None}, **sections)


class ScopeValidationTests(unittest.TestCase):
    def test_injection_scope_is_rejected_before_sql_construction(self):
        for repository, issue in [("owner/repo'; DELETE FROM plans;--", 1),
                                  ('../repo', 1), ('owner/repo', -1),
                                  ('owner/repo', 0), ('owner/repo', 2**31),
                                  ('owner/repo', '42'), (None, 1)]:
            with self.assertRaises(ValueError):
                issue_status.validate_scope(repository, issue)
            with self.assertRaises(ValueError):
                issue_status.scope_queries(repository, issue)
            with self.assertRaises(ValueError):
                issue_status.observe(['kubectl'], repository, issue)

    def test_every_query_is_a_read_only_transaction(self):
        queries = issue_status.scope_queries(REPOSITORY, ISSUE)
        self.assertEqual({'core', 'execution', 'model', 'evidence', 'release'},
                         set(queries))
        for name, sql in queries.items():
            self.assertTrue(sql.lstrip().startswith('BEGIN READ ONLY'), name)
            self.assertIn('COMMIT;', sql, name)
            self.assertIn("repository='owner/repo'", sql, name)
            self.assertIn('issue_number=42', sql, name)


class ReadOnlyRunnerTests(unittest.TestCase):
    def test_non_read_only_sql_never_reaches_the_cluster(self):
        with patch.object(subprocess, 'run') as command:
            with self.assertRaises(ValueError):
                issue_status.query_read_only(['kubectl'], 'DELETE FROM plans;')
            with self.assertRaises(ValueError):
                issue_status.query_read_only(['kubectl'], '  UPDATE work_items SET status=\'x\';')
            command.assert_not_called()

    def test_exec_invocation_is_the_fixed_read_only_psql(self):
        captured = {}

        def fake_run(argv, **kwargs):
            captured['argv'] = argv
            captured['input'] = kwargs.get('input')
            return subprocess.CompletedProcess(argv, 0, stdout='{"ok": 1}', stderr='')

        with patch.object(subprocess, 'run', side_effect=fake_run):
            result = issue_status.query_read_only(
                ['kubectl', '--context', 'kind-c'], 'BEGIN READ ONLY; SELECT 1; COMMIT;')
        self.assertEqual(result, {'ok': 1})
        argv = captured['argv']
        self.assertEqual(argv[:2], ['kubectl', '--context'])
        self.assertEqual(argv[3:], ['exec', '-i', 'ai-sdlc-postgres-0', '--', 'psql',
                                    '-U', 'ai_sdlc', '-d', 'ai_sdlc', '-XAtq', '-v',
                                    'ON_ERROR_STOP=1'])
        self.assertEqual(captured['input'], 'BEGIN READ ONLY; SELECT 1; COMMIT;')

    def test_failed_observation_returns_none_not_an_exception(self):
        with patch.object(subprocess, 'run',
                          return_value=subprocess.CompletedProcess([], 1, stdout='', stderr='denied')):
            self.assertIsNone(issue_status.query_read_only(
                ['kubectl'], 'BEGIN READ ONLY; SELECT 1; COMMIT;'))


class InterpretTests(unittest.TestCase):
    def test_unseen_issue_marks_every_stage_unavailable(self):
        report = issue_status.interpret({'core': {'revision': None}}, REPOSITORY, ISSUE)
        self.assertEqual(report['blockingReasonCode'], 'issue-not-observed')
        for stage in issue_status.STAGES:
            self.assertEqual(report['stages'][stage]['status'], 'unavailable', stage)

    def test_revision_then_pending_admission(self):
        report = issue_status.interpret(observing(core=core(admission=None)), REPOSITORY, ISSUE)
        self.assertEqual(report['stages']['revision']['status'], 'complete')
        self.assertEqual(report['stages']['admission']['status'], 'in-progress')
        self.assertEqual(report['stages']['plan']['status'], 'unavailable')
        self.assertEqual(report['metrics']['intakeLagSeconds'], 30.0)

    def test_denied_and_escalated_admissions_have_stable_codes(self):
        for decision, code in [('rejected', 'authority-denied'),
                               ('needs-human', 'awaiting-human'),
                               ('needs-clarification', 'awaiting-human'),
                               ('requires-decomposition', 'awaiting-human')]:
            observed = observing(core=core(admission={'decision': decision}))
            report = issue_status.interpret(observed, REPOSITORY, ISSUE)
            self.assertEqual(report['stages']['admission']['status'], 'blocked', decision)
            self.assertEqual(report['stages']['admission']['reasonCode'], code, decision)
            self.assertEqual(report['blockingReasonCode'], code, decision)

    def test_blocked_work_items_report_dependency_blocked(self):
        observed = observing(core=core(plan={'plans': 1, 'itemStatuses': {'blocked': 2}}))
        report = issue_status.interpret(observed, REPOSITORY, ISSUE)
        self.assertEqual(report['stages']['plan']['status'], 'blocked')
        self.assertEqual(report['stages']['plan']['reasonCode'], 'dependency-blocked')
        self.assertEqual(report['blockingReasonCode'], 'dependency-blocked')

    def test_privilege_denied_execution_is_observation_denied(self):
        report = issue_status.interpret(observing(), REPOSITORY, ISSUE)
        self.assertEqual(report['stages']['execution']['reasonCode'], 'observation-denied')
        self.assertEqual(report['stages']['verification']['reasonCode'], 'observation-denied')
        self.assertEqual(report['stages']['release']['status'], 'unavailable')

    def test_rate_limit_and_budget_exhaustion_are_distinguished(self):
        limited = observing(model={'budgets': {'count': 1, 'stopped': 0, 'exhausted': 0},
                                   'responseStatuses': {'429': 3}})
        report = issue_status.interpret(limited, REPOSITORY, ISSUE)
        self.assertEqual(report['blockingReasonCode'], 'rate-limited')
        exhausted = observing(model={'budgets': {'count': 1, 'stopped': 1, 'exhausted': 0},
                                     'responseStatuses': {'200': 3}})
        report = issue_status.interpret(exhausted, REPOSITORY, ISSUE)
        self.assertEqual(report['blockingReasonCode'], 'budget-exhausted')
        attempts = observing(execution={'states': {}, 'maxAttempt': 16, 'stopReasons': {},
                                        'lifecyclePhases': {}, 'failures': {'count': 0}},
                             model={'budgets': {}, 'responseStatuses': {}})
        report = issue_status.interpret(attempts, REPOSITORY, ISSUE)
        self.assertEqual(report['stages']['execution']['reasonCode'], 'budget-exhausted')

    def test_revoked_execution_reports_authority_denied(self):
        observed = observing(execution={'states': {'stopped': 1}, 'maxAttempt': 1,
                                        'stopReasons': {'authority-revoked': 1},
                                        'lifecyclePhases': {}, 'failures': {'count': 0}})
        report = issue_status.interpret(observed, REPOSITORY, ISSUE)
        self.assertEqual(report['stages']['execution']['reasonCode'], 'authority-denied')
        self.assertEqual(report['blockingReasonCode'], 'authority-denied')

    def test_failed_evidence_and_changes_requested_are_distinguished(self):
        evidence = {'verification': {'failed': 1},
                    'verificationReasons': {'comparison-failed': 1},
                    'verificationCleanups': 0, 'review': {}, 'reviewReasons': {},
                    'reviewCleanups': 0, 'publication': {}, 'publicationReasons': {},
                    'published': 0, 'merge': {}, 'mergeReasons': {}, 'merged': 0}
        observed = observing(execution={'states': {}, 'maxAttempt': 1, 'stopReasons': {},
                                      'lifecyclePhases': {'candidate-captured': 1},
                                      'failures': {'count': 0}},
                             evidence=evidence)
        report = issue_status.interpret(observed, REPOSITORY, ISSUE)
        self.assertEqual(report['stages']['verification']['reasonCode'], 'evidence-failed')
        changed = dict(evidence, verification={}, verificationReasons={},
                       review={'failed': 1}, reviewReasons={'request-changes': 1})
        report = issue_status.interpret(observing(execution={'states': {}, 'maxAttempt': 1,
                                        'stopReasons': {}, 'lifecyclePhases': {'candidate-captured': 1},
                                        'failures': {'count': 0}}, evidence=changed),
                                        REPOSITORY, ISSUE)
        self.assertEqual(report['stages']['review']['reasonCode'], 'changes-requested')

    def test_complete_chain_reports_production_evidence(self):
        observed = observing(
            execution={'states': {'stopped': 1}, 'maxAttempt': 1, 'stopReasons': {},
                       'lifecyclePhases': {'candidate-captured': 1}, 'failures': {'count': 0},
                       'queueAgeSeconds': None, 'oldestQueued': None},
            evidence={'verification': {'passed': 1}, 'verificationReasons': {},
                      'verificationCleanups': 2, 'review': {'passed': 1}, 'reviewReasons': {},
                      'reviewCleanups': 1, 'publication': {'published': 1},
                      'publicationReasons': {}, 'published': 1,
                      'merge': {'merged': 1}, 'mergeReasons': {}, 'merged': 1},
            release={'builds': 1, 'built': 1, 'provenance': 1, 'promotion': 1,
                     'deployment': {'deployed': 1}, 'deploymentReasons': {},
                     'terminal': {'completed': 1}, 'terminalReasons': {},
                     'production': {'count': 1, 'lastRecordedAt': '2026-01-02T00:00:00Z'}})
        report = issue_status.interpret(observed, REPOSITORY, ISSUE)
        self.assertTrue(report['complete'])
        self.assertEqual(report['blockingReasonCode'], 'none')
        self.assertEqual(report['stages']['deployment']['status'], 'complete')
        self.assertEqual(report['metrics']['terminalFailures']['execution'], 0)
        self.assertEqual(report['metrics']['retryCounts']['verification'], 2)

    def test_output_carries_no_prose_payloads_or_unbounded_fields(self):
        poisoned = observing(core=core(**{'payload': 'ISSUE PROSE', 'title': 'leak',
                                        'transcript': 'model output',
                                        'credential': 's3cr3t'}))
        report = issue_status.interpret(poisoned, REPOSITORY, ISSUE)
        encoded = json.dumps(report)
        for sentinel in ('ISSUE PROSE', 'leak', 'model output', 's3cr3t'):
            self.assertNotIn(sentinel, encoded)
        for stage in issue_status.STAGES:
            code = report['stages'][stage]['reasonCode']
            self.assertIn(code, issue_status.REASON_CODES, stage)


if __name__ == '__main__':
    unittest.main()
