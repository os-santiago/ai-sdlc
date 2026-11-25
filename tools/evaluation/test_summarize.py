import json
import tempfile
import unittest
from pathlib import Path

import summarize

EVIDENCE = Path(__file__).resolve().parents[2] / 'docs' / 'evidence'


def ok(issue, started='2026-11-23T00:00:00Z', seconds=400, calls=8, **extra):
    return dict({'issue': issue, 'startedAt': started, 'outcome': 'success', 'pullRequest': 1, 'seconds': seconds, 'modelCalls': calls}, **extra)


def stop(issue, stage='execution', reason='uncertain', line='candidate reconciliation stopped safely: model-provider workerExitCode=0',
         started='2026-11-23T00:00:00Z', **extra):
    return dict({'issue': issue, 'startedAt': started, 'outcome': 'failure', 'stage': stage, 'reason': reason, 'seconds': 300,
                 'modelCalls': 5, 'diagnostics': [line] if line else []}, **extra)


def issue(number, *attempts):
    return {'issue': number, 'attempts': list(attempts), 'success': attempts[-1]['outcome'] == 'success'}


def report(*issues, repository='o/r', **extra):
    return dict({'repository': repository, 'attemptBudget': 2, 'issues': list(issues)}, **extra)


class Reports:
    """Writes reports to a temporary directory and summarizes them."""

    def __init__(self, test):
        self.directory = tempfile.TemporaryDirectory()
        test.addCleanup(self.directory.cleanup)
        self.count = 0

    def write(self, data, name=None):
        self.count += 1
        path = Path(self.directory.name) / (name or 'battery-%d.json' % self.count)
        path.write_text(json.dumps(data))
        return path


class WilsonTests(unittest.TestCase):
    def test_matches_published_references(self):
        # Newcombe (1998), Statistics in Medicine 17:857-872, method 3, and
        # R binom::binom.wilson / statsmodels proportion_confint(method='wilson').
        references = {
            (81, 263): (0.2553, 0.3662),
            (15, 148): (0.0624, 0.1605),
            (0, 20): (0.0, 0.1611),
            (1, 29): (0.0061, 0.1718),
            (8, 10): (0.4902, 0.9433),
            (10, 10): (0.7225, 1.0),
        }
        for (x, n), (low, high) in references.items():
            with self.subTest(x=x, n=n):
                got = summarize.wilson(x, n)
                self.assertAlmostEqual(got[0], low, places=4)
                self.assertAlmostEqual(got[1], high, places=4)

    def test_edges_are_exact_and_inputs_bounded(self):
        self.assertEqual(summarize.wilson(0, 5)[0], 0.0)
        self.assertEqual(summarize.wilson(5, 5)[1], 1.0)
        self.assertEqual(summarize.wilson(0, 0), (0.0, 1.0))
        with self.assertRaises(ValueError):
            summarize.wilson(6, 5)

    def test_sixty_issues_need_forty_nine_deliveries(self):
        # 48/60 is exactly 80% but its lower bound is below 70%.
        self.assertLess(summarize.wilson(48, 60)[0], 0.70)
        self.assertGreaterEqual(summarize.wilson(49, 60)[0], 0.70)


class DiagnosticTests(unittest.TestCase):
    def test_name_comes_from_the_component_of_the_stop_stage(self):
        attempt = stop(1, stage='review', line=None, diagnostics=[
            'candidate reconciliation stopped safely: cleanup-pod cleanupPending=true workerExitCode=0',
            'independent review stopped safely: model-mailbox-read'])
        self.assertEqual(summarize.diagnostic_name(attempt), 'model-mailbox-read')
        self.assertEqual(summarize.diagnostic_name(dict(attempt, stage='execution')), 'cleanup-pod')

    def test_missing_or_unbounded_names(self):
        self.assertEqual(summarize.diagnostic_name(stop(1, line=None)), 'none')
        self.assertEqual(summarize.diagnostic_name(stop(1, line='repair handoff stopped safely')), 'unnamed')
        self.assertEqual(summarize.diagnostic_name(stop(1, line='independent review stopped safely: Bad<Name>')), 'unnamed')


class VerdictTests(unittest.TestCase):
    def test_observed_reviews_decide(self):
        self.assertEqual(summarize.attempt_verdict(ok(1, reviews=['failed|request-changes', 'passed|'])), ('approved', 'observed'))
        self.assertEqual(summarize.attempt_verdict(stop(1, reviews=[])), ('not-reached', 'observed'))
        self.assertEqual(summarize.attempt_verdict(stop(1, stage='review', reason='rejected', reviews=['failed|rejected'])), ('rejected', 'observed'))
        policy = stop(1, stage='review', line='independent review stopped safely: model-response-policy', reviews=['failed|uncertain'])
        self.assertEqual(summarize.attempt_verdict(policy), ('policy-stop', 'observed'))

    def test_older_reports_derive_the_verdict(self):
        self.assertEqual(summarize.attempt_verdict(ok(1)), ('approved', 'derived'))
        self.assertEqual(summarize.attempt_verdict(stop(1, stage='review', line='independent review stopped safely: model-response-policy')),
                         ('policy-stop', 'derived'))
        self.assertEqual(summarize.attempt_verdict(stop(1, stage='review', line='independent review stopped safely: model-drive-observe')),
                         ('review-stopped', 'derived'))
        self.assertEqual(summarize.attempt_verdict(stop(1, stage='verification', reason='comparison-failed', line=None)), ('not-reached', 'derived'))
        repair = {'issue': 1, 'outcome': 'timeout', 'diagnostics': ['independent review stopped safely: request-changes', 'repair handoff stopped safely']}
        self.assertEqual(summarize.attempt_verdict(repair), ('changes-requested', 'derived'))
        self.assertEqual(summarize.attempt_verdict({'issue': 1, 'outcome': 'timeout'}), ('not-observed', 'derived'))


class SummaryTests(unittest.TestCase):
    def test_counts_issues_not_attempts(self):
        reports = Reports(self)
        path = reports.write(report(issue(1, ok(1)), issue(2, stop(2), ok(2)), issue(3, stop(3), stop(3)), cell={'modelPath': 'local-llama'}))
        cell = summarize.summarize([path])['cells'][0]
        self.assertEqual((cell['trials'], cell['attempts']), (3, 5))
        self.assertEqual(cell['deliveredWithinTwo']['successes'], 2)
        self.assertEqual(cell['firstAttempt']['successes'], 1)
        self.assertEqual(cell['secondAttemptRecovery'], summarize.rate(1, 2))
        self.assertEqual(cell['passAtK'], {'1': 0.3333, '2': 0.6667})
        self.assertEqual(cell['finalStops'], [{'stage': 'execution', 'reason': 'uncertain', 'diagnostic': 'model-provider', 'count': 1}])
        self.assertEqual(cell['stops'][0]['count'], 3)

    def test_cells_split_by_repository_and_model_path(self):
        reports = Reports(self)
        a = reports.write(report(issue(1, ok(1)), cell={'repository': 'o/r', 'modelPath': 'local-llama'}))
        b = reports.write(report(issue(1, ok(1))))
        c = reports.write(report(issue(1, ok(1)), repository='o/other'))
        summary = summarize.summarize([a, b, c], labels={str(c): 'agent-cli'})
        keys = [(cell['repository'], cell['modelPath']) for cell in summary['cells']]
        self.assertEqual(keys, [('o/other', 'agent-cli'), ('o/r', 'local-llama'), ('o/r', 'unspecified')])
        unspecified = summary['cells'][2]['gate']
        self.assertFalse(unspecified['passed'])
        self.assertIn('the model path is unspecified', unspecified['reasons'][0])

    def test_recorded_model_path_wins_over_the_override(self):
        reports = Reports(self)
        path = reports.write(report(issue(1, ok(1)), cell={'repository': 'o/r', 'modelPath': 'local-llama'}))
        self.assertEqual(summarize.summarize([path], model_path='agent-cli')['cells'][0]['modelPath'], 'local-llama')

    def test_dedupe_rules(self):
        reports = Reports(self)
        first = reports.write(report(issue(87, stop(87, started='2026-11-23T01:00:00Z'), stop(87, started='2026-11-23T01:10:00Z')),
                                     issue(5, ok(5, started='2026-11-23T01:20:00Z'))))
        second = reports.write(report(issue(87, ok(87, started='2026-11-23T05:00:00Z'))))
        trial = summarize.summarize([first, second, first], model_path='agent-cli')
        cell = trial['cells'][0]
        # The same file twice adds nothing; each battery run of #87 is a trial.
        self.assertEqual((trial['duplicateTrialsDropped'], cell['trials'], cell['distinctIssues']), (2, 3, 2))
        self.assertEqual(cell['deliveredWithinTwo']['successes'], 2)
        self.assertEqual(cell['passAtK']['2'], 0.75)  # (#5: 1 + #87: 1/2) / 2
        earliest = summarize.summarize([second, first], model_path='agent-cli', rule='first')['cells'][0]
        self.assertEqual((earliest['trials'], earliest['deliveredWithinTwo']['successes']), (2, 1))

    def test_partial_reports(self):
        reports = Reports(self)
        undecided = reports.write(report(issue(1, ok(1)), inProgress=[stop(2)], stopped='operator-stopped'))
        cell = summarize.summarize([undecided], model_path='agent-cli')['cells'][0]
        self.assertEqual((cell['trials'], cell['incompleteTrials']), (1, 1))
        self.assertIn('1 incomplete trials excluded', cell['gate']['warnings'])
        decided = reports.write(report(issue(1, ok(1)), inProgress=[stop(2), stop(2)]))
        self.assertEqual(summarize.summarize([decided], model_path='agent-cli')['cells'][0]['trials'], 2)
        delivered = reports.write(report(inProgress=[ok(3)]))
        self.assertEqual(summarize.summarize([delivered], model_path='agent-cli')['cells'][0]['deliveredWithinTwo']['successes'], 1)

    def test_truncated_trial_counts_as_not_delivered(self):
        reports = Reports(self)
        path = reports.write(report(issue(1, stop(1)), stopped='runtime-blocked'))
        cell = summarize.summarize([path], model_path='agent-cli')['cells'][0]
        self.assertEqual((cell['trials'], cell['truncatedTrials'], cell['deliveredWithinTwo']['successes']), (1, 1, 0))

    def test_success_on_a_third_attempt_is_not_within_two(self):
        reports = Reports(self)
        path = reports.write(report(issue(1, stop(1), stop(1), ok(1)), attemptBudget=3))
        cell = summarize.summarize([path], model_path='agent-cli')['cells'][0]
        self.assertEqual(cell['deliveredWithinTwo']['successes'], 0)

    def test_output_is_deterministic(self):
        reports = Reports(self)
        a = reports.write(report(issue(2, stop(2)), issue(1, ok(1))))
        b = reports.write(report(issue(3, ok(3))))
        one = summarize.summarize([a, b], model_path='agent-cli')
        two = summarize.summarize([b, a], model_path='agent-cli')
        self.assertEqual(json.dumps(one, sort_keys=True), json.dumps(two, sort_keys=True))
        self.assertEqual(summarize.markdown(one), summarize.markdown(two))

    def test_not_a_report_is_refused(self):
        reports = Reports(self)
        with self.assertRaises(ValueError):
            summarize.summarize([reports.write({'issues': 'x'})])


class GateTests(unittest.TestCase):
    def cell(self, delivered, n, distinct=None, model_path='local-llama'):
        within = summarize.rate(delivered, n)
        return {'modelPath': model_path, 'distinctIssues': n if distinct is None else distinct, 'deliveredWithinTwo': within,
                'revisions': [], 'incompleteTrials': 0, 'truncatedTrials': 0}

    def test_gate_needs_size_rate_and_lower_bound(self):
        self.assertTrue(summarize.gate(self.cell(49, 60))['passed'])
        exact = summarize.gate(self.cell(48, 60))
        self.assertFalse(exact['passed'])
        self.assertEqual(len(exact['reasons']), 1)
        self.assertIn('lower bound 68.2%', exact['reasons'][0])
        small = summarize.gate(self.cell(59, 59))
        self.assertEqual(small['reasons'], ['59 distinct issues, at least 60 needed'])
        low = summarize.gate(self.cell(70, 100))
        self.assertEqual(len(low['reasons']), 2)

    def test_repeated_trials_do_not_replace_distinct_issues(self):
        result = summarize.gate(self.cell(90, 100, distinct=40))
        self.assertEqual(result['reasons'], ['40 distinct issues, at least 60 needed'])

    def test_no_trials(self):
        self.assertIn('no complete trials', summarize.gate(self.cell(0, 0))['reasons'])

    def test_overall_needs_a_two_by_two_grid(self):
        def cell(repository, path, passed):
            return {'repository': repository, 'modelPath': path, 'gate': {'passed': passed}}
        three = [cell('a', 'x', True), cell('a', 'y', True), cell('b', 'x', True), cell('b', 'y', False)]
        self.assertFalse(summarize.overall(three)['passed'])
        four = three[:3] + [cell('b', 'y', True)]
        self.assertEqual(summarize.overall(four)['grid'], {'repositories': ['a', 'b'], 'modelPaths': ['x', 'y']})


class EvidenceTests(unittest.TestCase):
    def test_committed_batteries_parse_and_match_their_own_summaries(self):
        paths = sorted(EVIDENCE.glob('*-evaluation-battery-*.json'))
        self.assertTrue(paths)
        for path in paths:
            data = json.loads(path.read_text())
            cell = summarize.summarize([path], model_path='agent-cli')['cells'][0]
            if 'summary' in data:
                with self.subTest(path=path.name):
                    self.assertEqual(cell['trials'], data['summary']['issues'])
                    self.assertEqual(cell['deliveredWithinTwo']['successes'], data['summary']['successWithinBudget'])
                    self.assertEqual(cell['firstAttempt']['successes'], data['summary']['firstAttemptSuccess'])

    def test_command_line_writes_both_outputs(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            paths = [str(p) for p in sorted(EVIDENCE.glob('*-evaluation-battery-*.json'))]
            summarize.main(paths + ['--model-path', 'agent-cli', '--json', str(out / 's.json'), '--markdown', str(out / 's.md')])
            self.assertEqual(json.loads((out / 's.json').read_text())['cells'][0]['modelPath'], 'agent-cli')
            self.assertIn('Beta gate', (out / 's.md').read_text())


if __name__ == '__main__':
    unittest.main()
