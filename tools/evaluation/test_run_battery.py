import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import run_battery


def state(executions=0, execution='', verifications=0, verification='', reviews=0, review='', calls=0, candidate=False):
    return {'executions': executions, 'execution': execution, 'verifications': verifications, 'verification': verification,
            'reviews': reviews, 'review': review, 'modelCalls': calls, 'executionCandidate': candidate}


class FakeRuntime:
    def __init__(self, snapshots, blocked=False):
        self.snapshots, self.is_blocked = list(snapshots), blocked

    def snapshot(self, issue):
        return self.snapshots.pop(0) if len(self.snapshots) > 1 else self.snapshots[0]

    def stop_lines(self, since):
        return ['candidate reconciliation stopped safely: worker-exit workerExitCode=23']

    def blocked(self):
        return self.is_blocked


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

    def test_label_is_removed_then_added(self):
        github = FakeGitHub(merged=3)
        run_battery.attempt(FakeRuntime([state()]), github, 9, 60)
        self.assertEqual(github.labels, [(9, False), (9, True)])


@mock.patch('run_battery.time.sleep', lambda _: None)
class MainTests(unittest.TestCase):
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


if __name__ == '__main__':
    unittest.main()
