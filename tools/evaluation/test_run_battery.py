import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import run_battery


def state(executions=0, execution='', verifications=0, verification='', reviews=0, review='', calls=0, candidate=False, impossible=False, reason=''):
    return {'executions': executions, 'execution': execution, 'verifications': verifications, 'verification': verification,
            'reviews': reviews, 'review': review, 'modelCalls': calls, 'executionCandidate': candidate, 'handoffImpossible': impossible,
            'handoffReason': reason}


class FakeRuntime:
    def __init__(self, snapshots, blocked=False, reviews=(), images=None):
        self.snapshots, self.is_blocked = list(snapshots), blocked
        self.reviews, self.installed = list(reviews), images or {}

    def snapshot(self, issue):
        return self.snapshots.pop(0) if len(self.snapshots) > 1 else self.snapshots[0]

    def stop_lines(self, since):
        return ['candidate reconciliation stopped safely: worker-exit workerExitCode=23']

    def blocked(self):
        return self.is_blocked

    def review_states(self, issue, since):
        return self.reviews

    def images(self):
        return self.installed


class FakeGitHub:
    def __init__(self, merged=None):
        self.merged, self.labels = merged, []

    def label(self, issue, add):
        self.labels.append((issue, add))

    def merged_pull(self, issue, since):
        return self.merged


@mock.patch('run_battery.time.sleep', lambda _: None)
class AttemptTests(unittest.TestCase):
    def test_merged_pull_request_is_success_without_diagnostics(self):
        result = run_battery.attempt(FakeRuntime([state(), state(calls=7)]), FakeGitHub(merged=12), 4, 60)
        self.assertEqual((result['outcome'], result['pullRequest'], result['modelCalls']), ('success', 12, 7))
        self.assertNotIn('diagnostics', result)

    def test_new_stopped_execution_is_a_failure_with_stop_lines(self):
        result = run_battery.attempt(FakeRuntime([state(), state(executions=1, execution='stopped|uncertain', calls=12)]), FakeGitHub(), 7, 60)
        self.assertEqual((result['outcome'], result['stage'], result['reason']), ('failure', 'execution', 'uncertain'))
        self.assertIn('workerExitCode=23', result['diagnostics'][0])

    def test_stop_after_capture_waits_for_the_handoff(self):
        stopped = state(executions=1, execution='stopped|uncertain', candidate=True)
        with mock.patch('run_battery.time.monotonic', side_effect=[0, 0, 20, 40, 61, 61]):
            result = run_battery.attempt(FakeRuntime([state(), stopped, stopped]), FakeGitHub(), 7, 60)
        self.assertEqual((result['outcome'], result['executionStop']), ('timeout', 'uncertain'))
        # The handed-off candidate decides the attempt.
        handed = dict(stopped, verifications=1, verification='failed|comparison-failed')
        refused = run_battery.attempt(FakeRuntime([state(), stopped, handed]), FakeGitHub(), 7, 60)
        self.assertEqual((refused['outcome'], refused['stage'], refused['reason'], refused['executionStop']),
                         ('failure', 'verification', 'comparison-failed', 'uncertain'))

    def test_stop_whose_handoff_can_no_longer_happen_is_final(self):
        dead = state(executions=1, execution='stopped|uncertain', candidate=True, impossible=True)
        result = run_battery.attempt(FakeRuntime([state(), dead]), FakeGitHub(), 7, 60)
        self.assertEqual((result['outcome'], result['stage'], result['reason']), ('failure', 'execution', 'uncertain'))

    def test_captured_candidate_whose_handoff_is_rejected_is_a_failure(self):
        rejected = state(executions=1, execution='stopped|candidate-captured', candidate=True, impossible=True, reason='unsupported-recipe')
        result = run_battery.attempt(FakeRuntime([state(), rejected]), FakeGitHub(), 7, 60)
        self.assertEqual((result['outcome'], result['stage'], result['reason']), ('failure', 'handoff', 'unsupported-recipe'))
        sent = dict(rejected, handoffImpossible=False, handoffReason='')
        with mock.patch('run_battery.time.monotonic', side_effect=[0, 0, 30, 61, 61]):
            pending = run_battery.attempt(FakeRuntime([state(), sent]), FakeGitHub(), 7, 60)
        self.assertEqual(pending['outcome'], 'timeout')

    def test_execution_stop_survives_into_a_verification_failure(self):
        stopped = state(executions=1, execution='stopped|uncertain', candidate=True)
        refused = dict(stopped, verifications=1, verification='failed|comparison-failed')
        result = run_battery.attempt(FakeRuntime([state(), refused]), FakeGitHub(), 7, 60)
        self.assertEqual((result['stage'], result['executionStop']), ('verification', 'uncertain'))

    def test_stop_without_a_candidate_is_final_at_once(self):
        result = run_battery.attempt(FakeRuntime([state(), state(executions=1, execution='stopped|uncertain')]), FakeGitHub(), 7, 60)
        self.assertEqual((result['outcome'], result['stage']), ('failure', 'execution'))
        self.assertNotIn('executionStop', result)

    def test_repair_stop_after_an_earlier_verification_is_a_failure(self):
        verified = state(executions=1, execution='stopped|candidate-captured', verifications=1, verification='passed|', reviews=1, review='failed|request-changes', candidate=True)
        repair_running = dict(verified, executions=2, execution='claimed|', executionCandidate=False)
        repair_stopped = dict(repair_running, execution='stopped|uncertain')
        result = run_battery.attempt(FakeRuntime([state(), verified, repair_running, repair_stopped]), FakeGitHub(), 7, 60)
        self.assertEqual((result['outcome'], result['stage'], result['reason']), ('failure', 'execution', 'uncertain'))

    def test_captured_candidate_and_old_failures_are_not_attributed(self):
        old = state(executions=1, execution='stopped|uncertain', verifications=1, verification='failed|uncertain')
        captured = dict(old, executions=2, execution='stopped|candidate-captured')
        with mock.patch('run_battery.time.monotonic', side_effect=[0, 0, 30, 61, 61]):
            result = run_battery.attempt(FakeRuntime([old, captured]), FakeGitHub(), 5, 60)
        self.assertEqual(result['outcome'], 'timeout')

    def test_review_request_for_changes_is_not_final(self):
        repair = state(reviews=1, review='failed|request-changes')
        with mock.patch('run_battery.time.monotonic', side_effect=[0, 0, 30, 61, 61]):
            result = run_battery.attempt(FakeRuntime([state(), repair]), FakeGitHub(), 5, 60)
        self.assertEqual(result['outcome'], 'timeout')
        rejected = run_battery.attempt(FakeRuntime([state(), state(reviews=1, review='failed|rejected')]), FakeGitHub(), 5, 60)
        self.assertEqual((rejected['outcome'], rejected['stage'], rejected['reason']), ('failure', 'review', 'rejected'))

    def test_attempt_records_the_stage_reached_and_its_reviews(self):
        merged = run_battery.attempt(FakeRuntime([state(), state(executions=1, verifications=1, reviews=2)],
                                                 reviews=['failed|request-changes', 'passed|']), FakeGitHub(merged=5), 4, 60)
        self.assertEqual((merged['stageReached'], merged['reviews']), ('merge', ['failed|request-changes', 'passed|']))
        refused = run_battery.attempt(FakeRuntime([state(), state(executions=1, verifications=1, verification='failed|comparison-failed')]),
                                      FakeGitHub(), 4, 60)
        self.assertEqual((refused['stageReached'], refused['reviews']), ('verification', []))

    def test_unobservable_reviews_leave_the_field_out(self):
        runtime = FakeRuntime([state(), state(executions=1, execution='stopped|uncertain')])
        runtime.review_states = mock.Mock(side_effect=run_battery.subprocess.CalledProcessError(1, 'kubectl'))
        result = run_battery.attempt(runtime, FakeGitHub(), 4, 60)
        self.assertEqual(result['stageReached'], 'execution')
        self.assertNotIn('reviews', result)

    def test_label_is_removed_then_added(self):
        github = FakeGitHub(merged=3)
        run_battery.attempt(FakeRuntime([state()]), github, 9, 60)
        self.assertEqual(github.labels, [(9, False), (9, True)])


@mock.patch('run_battery.time.sleep', lambda _: None)
class MainTests(unittest.TestCase):
    def setUp(self):
        self.enterContext(contextlib.redirect_stdout(io.StringIO()))

    def test_blocked_runtime_stops_the_battery(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch('run_battery.Runtime', return_value=FakeRuntime([state()], blocked=True)), \
                mock.patch('run_battery.GitHub', return_value=FakeGitHub()):
            output = Path(directory) / 'report.json'
            run_battery.main(['--repository', 'o/r', '--issues', '4,5', '--kubectl', 'k', '--kubeconfig', 'c', '--output', str(output)])
            report = json.loads(output.read_text())
        self.assertEqual(report['stopped'], 'runtime-blocked')
        self.assertEqual(report['summary']['issues'], 0)

    def test_label_is_removed_after_the_last_attempt(self):
        github = FakeGitHub()
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch('run_battery.Runtime', return_value=FakeRuntime([state()])), \
                mock.patch('run_battery.GitHub', return_value=github), \
                mock.patch('run_battery.attempt', side_effect=lambda *a: {'outcome': 'success'}):
            run_battery.main(['--repository', 'o/r', '--issues', '4,5', '--kubectl', 'k', '--kubeconfig', 'c', '--output', str(Path(directory) / 'r.json')])
        self.assertEqual(github.labels, [(4, False), (5, False)])

    def test_rates_count_first_attempt_and_budget(self):
        results = iter([{'outcome': 'failure'}, {'outcome': 'success'}, {'outcome': 'success'}])
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch('run_battery.Runtime', return_value=FakeRuntime([state()])), \
                mock.patch('run_battery.GitHub', return_value=FakeGitHub()), \
                mock.patch('run_battery.attempt', side_effect=lambda *a: dict(next(results))):
            output = Path(directory) / 'report.json'
            run_battery.main(['--repository', 'o/r', '--issues', '4,5', '--kubectl', 'k', '--kubeconfig', 'c', '--output', str(output)])
            summary = json.loads(output.read_text())['summary']
        self.assertEqual((summary['firstAttemptSuccess'], summary['successWithinBudget'], summary['rateWithinBudget']), (1, 2, 1.0))

    def test_report_names_its_cell_and_installation(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch('run_battery.Runtime', return_value=FakeRuntime([state()], images={'ai-sdlc-worker': 'q/w@sha256:1'})), \
                mock.patch('run_battery.GitHub', return_value=FakeGitHub()), \
                mock.patch('run_battery.attempt', side_effect=lambda *a: {'outcome': 'success'}):
            lock = Path(directory) / 'images.lock.json'
            lock.write_text('{"image": {"tag": "abc123"}}\n')
            output = Path(directory) / 'report.json'
            run_battery.main(['--repository', 'o/r', '--issues', '4', '--kubectl', 'k', '--kubeconfig', 'c', '--output', str(output),
                              '--cell-model-path', 'local-llama', '--image-lock', str(lock)])
            report = json.loads(output.read_text())
        self.assertEqual(report['schemaVersion'], 2)
        self.assertEqual(report['cell'], {'repository': 'o/r', 'modelPath': 'local-llama'})
        self.assertEqual(report['installation']['revision'], 'abc123')
        self.assertEqual(report['installation']['imageLockDigest'],
                         'sha256:' + run_battery.hashlib.sha256(b'{"image": {"tag": "abc123"}}\n').hexdigest())
        self.assertEqual(report['installation']['images'], {'ai-sdlc-worker': 'q/w@sha256:1'})

    def test_model_path_label_is_bounded(self):
        with mock.patch('sys.stderr'), self.assertRaises(SystemExit):
            run_battery.main(['--repository', 'o/r', '--issues', '4', '--kubectl', 'k', '--kubeconfig', 'c', '--output', 'r.json',
                              '--cell-model-path', 'Local Llama'])


class QueryTests(unittest.TestCase):
    def test_terminal_cleanup_waits_for_the_third_retry_window(self):
        self.assertIn("cleanup_attempts>=3 AND l.cleanup_after<=now()", run_battery.TERMINAL_CLEANUP)
        self.assertIn("cleanup_state='uncertain'", run_battery.TERMINAL_CLEANUP)

    def test_blocked_counts_only_stopped_executions_with_terminal_cleanup(self):
        runtime = run_battery.Runtime('kubectl', 'kubeconfig')
        queries = []
        with mock.patch.object(run_battery.Runtime, 'sql', lambda self, q: queries.append(q) or ['0']):
            self.assertFalse(runtime.blocked())
        self.assertEqual(len(queries), 2)
        self.assertIn("r.state='stopped'", queries[1])
        self.assertIn(run_battery.TERMINAL_CLEANUP, queries[1])
        with mock.patch.object(run_battery.Runtime, 'sql', lambda self, q: ['1'] if 'execution_lifecycles' in q else ['0']):
            self.assertTrue(runtime.blocked())

    def test_snapshot_queries_bind_the_latest_execution(self):
        runtime = run_battery.Runtime('kubectl', 'kubeconfig')
        queries = []
        with mock.patch.object(run_battery.Runtime, 'sql', lambda self, q: queries.append(q) or ['0']):
            runtime.snapshot(7)
        joined = '\n'.join(queries)
        self.assertIn("'issueNumber' = '7'", joined)
        self.assertGreaterEqual(joined.count('ORDER BY created_at DESC, id DESC LIMIT 1'), 6)
        self.assertIn("h.state='rejected'", joined)
        self.assertIn("r.expires_at<=now() AND NOT EXISTS", joined)

    def test_queries_are_bound_to_the_repository(self):
        runtime = run_battery.Runtime('kubectl', 'kubeconfig', repository='Owner/Repo')
        queries = []
        with mock.patch.object(run_battery.Runtime, 'sql', lambda self, q: queries.append(q) or ['0']):
            runtime.snapshot(7)
            runtime.review_states(7, '2026-11-24T00:00:00Z')
        bound = "'issueNumber' = '7' AND lower(convert_from(payload,'UTF8')::json->>'repository') = 'owner/repo'"
        issue_queries = [q for q in queries if "'issueNumber'" in q]
        self.assertTrue(issue_queries)
        for query in issue_queries:
            self.assertEqual(query.count("'issueNumber' = '7'"), query.count(bound))
        self.assertIn(bound, queries[-1])
        with self.assertRaises(ValueError):
            run_battery.Runtime('kubectl', 'kubeconfig', repository="o/r' OR '1'='1")

    def test_review_states_are_bound_to_the_issue_and_attempt(self):
        runtime = run_battery.Runtime('kubectl', 'kubeconfig')
        queries = []
        with mock.patch.object(run_battery.Runtime, 'sql', lambda self, q: queries.append(q) or ['passed|']):
            self.assertEqual(runtime.review_states(7, '2026-11-24T00:00:00Z'), ['passed|'])
        self.assertIn("'issueNumber' = '7'", queries[0])
        self.assertIn("created_at >= '2026-11-24T00:00:00Z'::timestamptz ORDER BY created_at, id", queries[0])



class RunRetryTests(unittest.TestCase):
    def test_transient_failures_are_retried_then_raised(self):
        calls = []
        def flaky(args, **kwargs):
            calls.append(args)
            if len(calls) < 3:
                raise run_battery.subprocess.CalledProcessError(1, args)
            return mock.Mock(stdout="ok")
        with mock.patch('run_battery.subprocess.run', side_effect=flaky), mock.patch('run_battery.time.sleep') as sleep:
            self.assertEqual(run_battery.run(['gh', 'x']), "ok")
        self.assertEqual((len(calls), sleep.call_count), (3, 2))
        def failing(args, **kwargs):
            raise run_battery.subprocess.TimeoutExpired(args, 120)
        with mock.patch('run_battery.subprocess.run', side_effect=failing) as runs, mock.patch('run_battery.time.sleep') as sleep:
            with self.assertRaises(run_battery.subprocess.TimeoutExpired):
                run_battery.run(['gh', 'x'])
        self.assertEqual((runs.call_count, [c.args[0] for c in sleep.call_args_list]), (4, [5, 15, 45]))
        with mock.patch('run_battery.subprocess.run', side_effect=failing) as runs, mock.patch('run_battery.time.sleep') as sleep:
            with self.assertRaises(run_battery.subprocess.TimeoutExpired):
                run_battery.run(['kubectl', 'logs'], retry=False)
        self.assertEqual((runs.call_count, sleep.call_count), (1, 0))

if __name__ == '__main__':
    unittest.main()


class ForgeLabelRetryTest(unittest.TestCase):
    """A transient forge failure while labeling is retried (battery 29 aborted
    on one failed label edit during a GitHub API outage)."""

    def forge(self):
        forge = run_battery.GitHub.__new__(run_battery.GitHub)
        forge.repository = 'fixture/product'
        return forge

    def test_a_transient_failure_is_retried(self):
        results = [mock.Mock(returncode=1), mock.Mock(returncode=1), mock.Mock(returncode=0)]
        with mock.patch.object(run_battery.subprocess, 'run', side_effect=results) as run, \
                mock.patch.object(run_battery.time, 'sleep') as sleep:
            self.forge().label(195, True)
        self.assertEqual(run.call_count, 3)
        self.assertEqual(sleep.call_count, 2)

    def test_a_persistent_failure_still_stops_an_add_but_not_a_remove(self):
        with mock.patch.object(run_battery.subprocess, 'run', return_value=mock.Mock(returncode=1)), \
                mock.patch.object(run_battery.time, 'sleep'):
            with self.assertRaises(RuntimeError):
                self.forge().label(195, True)
            self.forge().label(195, False)

    def test_a_timeout_is_retried(self):
        results = [run_battery.subprocess.TimeoutExpired('gh', 60), mock.Mock(returncode=0)]
        with mock.patch.object(run_battery.subprocess, 'run', side_effect=results) as run, \
                mock.patch.object(run_battery.time, 'sleep'):
            self.forge().label(195, True)
        self.assertEqual(run.call_count, 2)
