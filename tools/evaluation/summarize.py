#!/usr/bin/env python3
"""Summarize evaluation battery reports per cell, with intervals and the Beta gate.

A cell is one repository and one model path. The summary counts issues, not
attempts: each battery run of an issue is a trial that ends delivered (a
merged pull request) or not delivered within the attempt budget. For each
cell it reports:

  * the rate delivered within two attempts and the first-attempt rate, each
    with a Wilson 95% interval;
  * pass@1 and pass@2 averaged over distinct issues, the per-attempt success
    rate, and how often a second attempt recovers a failed first one;
  * stop classes by stage, runner reason and the components' bounded
    diagnostic name, per attempt and for the final stop of each undelivered
    trial;
  * the reviewer verdict against the attempt outcome, observed when the report
    records review runs and derived from the stop otherwise;
  * the Beta functional gate: at least 60 distinct issues, a rate of at least
    80% within two attempts and a 95% lower bound of at least 70%.

The output is deterministic: the same reports give byte-identical JSON and
Markdown. Pure standard library; see docs/operations/evaluation-harness.md.
"""
import argparse
import hashlib
import json
import math
import re
import statistics
import sys
from collections import Counter
from pathlib import Path

SUMMARY_SCHEMA = 1
# Two-sided 95%: the 0.975 quantile of the standard normal distribution.
Z95 = 1.959963984540054
GATE = {'name': 'beta', 'minIssues': 60, 'minRate': 0.80, 'minLowerBound': 0.70}
UNSPECIFIED = 'unspecified'
MODEL_PATH = re.compile(r'^[a-z0-9][a-z0-9-]{0,39}$')
DIAGNOSTIC = re.compile(r'^[a-z0-9][a-z0-9-]{0,63}$')
# The component whose safe stop line names a stop at each runner stage.
STAGE_COMPONENT = {
    'execution': 'candidate reconciliation',
    'handoff': 'candidate reconciliation',
    'verification': 'verification reconciliation',
    'review': 'independent review',
}
VERDICTS = ('approved', 'changes-requested', 'rejected', 'policy-stop', 'review-stopped', 'not-reached', 'not-observed')
OUTCOMES = ('delivered', 'failure', 'timeout')
# A review stop that is the reviewer's own answer refused by the response policy
# (for example a final answer without a VERDICT line), not an infrastructure stop.
POLICY_STOPS = {'model-response-policy'}


def wilson(successes, n, z=Z95):
    """The Wilson score interval for a binomial proportion, as (low, high).

    It stays inside [0, 1] and keeps a sensible width at small n or at rates
    near 0 or 1, where the normal (Wald) interval fails."""
    if n <= 0:
        return (0.0, 1.0)
    if not 0 <= successes <= n:
        raise ValueError('successes must be between 0 and n')
    p = successes / n
    z2 = z * z
    centre = (p + z2 / (2 * n)) / (1 + z2 / n)
    half = z * math.sqrt(p * (1 - p) / n + z2 / (4 * n * n)) / (1 + z2 / n)
    low, high = max(0.0, centre - half), min(1.0, centre + half)
    # Exact bounds at the edges, free of floating-point residue.
    return (0.0 if successes == 0 else low, 1.0 if successes == n else high)


def rate(successes, n):
    low, high = wilson(successes, n)
    return {'successes': successes, 'n': n, 'rate': r4(successes / n) if n else None,
            'wilson95': [r4(low), r4(high)] if n else None}


def r4(value):
    return round(value, 4)


def parse_line(line):
    """A safe stop line as (component, bounded name): `independent review
    stopped safely: model-mailbox-read` gives ('independent review',
    'model-mailbox-read'). A line without an allowlisted name is `unnamed`."""
    component, _, rest = line.partition(' stopped safely')
    words = rest[1:].split() if rest.startswith(':') else []
    return component, words[0] if words and DIAGNOSTIC.match(words[0]) else 'unnamed'


def stop_lines(attempt):
    return [parse_line(line) for line in attempt.get('diagnostics') or [] if isinstance(line, str) and ' stopped safely' in line]


def diagnostic_name(attempt):
    """The bounded diagnostic name of an attempt's stop, from the line of the
    component that owns the stop stage (the first line if none matches);
    `none` without lines."""
    lines = stop_lines(attempt)
    if not lines:
        return 'none'
    component = STAGE_COMPONENT.get(attempt.get('stage'))
    return next((name for owner, name in lines if owner == component), lines[0][1])


def stop_class(attempt):
    outcome = attempt.get('outcome')
    if outcome == 'timeout':
        return ('timeout', attempt.get('executionStop') or 'timeout', diagnostic_name(attempt))
    return (attempt.get('stage') or 'unknown', attempt.get('reason') or 'unknown', diagnostic_name(attempt))


def review_verdict(state):
    """Maps a review run `state|reason` to a verdict."""
    status, _, reason = state.partition('|')
    if status == 'passed':
        return 'approved'
    if status == 'failed':
        return {'request-changes': 'changes-requested', 'rejected': 'rejected'}.get(reason, 'review-stopped')
    return 'not-observed'  # queued or claimed: no decision yet


def attempt_verdict(attempt):
    """The last reviewer verdict of an attempt and whether it was observed.

    Reports with `reviews` give it directly. Older reports derive it from the
    outcome and the stop: a merge implies an approval (the merge gate requires
    one); a review stop is a policy stop when the reviewer's answer was refused
    by the response policy, a request for changes or rejection when its
    diagnostic says so, and a stop without a verdict otherwise; an execution,
    handoff or verification stop that shows no review activity never reached
    review. A timeout without review data is not observed."""
    outcome = attempt.get('outcome')
    reviews = attempt.get('reviews')
    if isinstance(reviews, list):
        if not reviews:
            return 'not-reached', 'observed'
        verdict = review_verdict(reviews[-1])
        if verdict == 'review-stopped' and attempt.get('stage') == 'review' and diagnostic_name(attempt) in POLICY_STOPS:
            verdict = 'policy-stop'
        return verdict, 'observed'
    names = stop_lines(attempt)
    reviewer = {name for component, name in names if component == 'independent review'}
    if outcome == 'success':
        return 'approved', 'derived'
    if attempt.get('stage') == 'review':
        name = diagnostic_name(attempt)
        if name in POLICY_STOPS:
            return 'policy-stop', 'derived'
        if name == 'request-changes':
            return 'changes-requested', 'derived'
        if name == 'rejected' or attempt.get('reason') == 'rejected':
            return 'rejected', 'derived'
        return 'review-stopped', 'derived'
    if 'request-changes' in reviewer or any(component == 'repair handoff' for component, _ in names):
        return 'changes-requested', 'derived'
    if outcome == 'failure' and not reviewer:
        return 'not-reached', 'derived'
    return 'not-observed', 'derived'


def outcome_column(attempt):
    outcome = attempt.get('outcome')
    return 'delivered' if outcome == 'success' else outcome if outcome in OUTCOMES else 'failure'


def load(path):
    """A report and its digest. Fails on a file that is not a battery report."""
    data = Path(path).read_bytes()
    report = json.loads(data)
    if not isinstance(report, dict) or not isinstance(report.get('issues'), list) or not report.get('repository'):
        raise ValueError('%s is not a battery report' % path)
    return report, 'sha256:' + hashlib.sha256(data).hexdigest()


def trials(path, report, model_path):
    """The report's trials: one per issue run, with its cell and completeness.

    A trial in `issues` is complete as the runner recorded it. A trial in
    `inProgress` (a battery stopped mid-issue) is complete only when it already
    succeeded or used its whole attempt budget; otherwise it is incomplete and
    stays out of the rates."""
    budget = int(report.get('attemptBudget') or 2)
    cell = report.get('cell') if isinstance(report.get('cell'), dict) else {}
    repository = cell.get('repository') or report['repository']
    path_label = cell.get('modelPath') or model_path or UNSPECIFIED
    installation = report.get('installation') if isinstance(report.get('installation'), dict) else {}
    entries = [(entry.get('issue'), entry.get('attempts') or [], True) for entry in report['issues']]
    progress = report.get('inProgress') or []
    if progress:
        succeeded = any(a.get('outcome') == 'success' for a in progress)
        entries.append((progress[0].get('issue'), progress, succeeded or len(progress) >= budget))
    out = []
    for issue, attempts, complete in entries:
        if not attempts or issue is None:
            continue
        success_at = next((n for n, a in enumerate(attempts, 1) if a.get('outcome') == 'success'), None)
        out.append({
            'repository': repository, 'modelPath': path_label, 'issue': int(issue),
            'startedAt': attempts[0].get('startedAt') or '', 'source': str(path),
            'revision': installation.get('revision'),
            'attempts': attempts, 'complete': complete, 'successAt': success_at,
            # Fewer than two attempts without success (a blocked runtime, an
            # operator stop or a budget of one): not delivered, and reported.
            'truncated': complete and success_at is None and len(attempts) < 2,
        })
    return out


def dedupe(all_trials, rule):
    """Applies the dedupe rule within each cell.

    The same trial read twice (same cell, issue and first-attempt start) is
    always counted once. Then `trial` keeps every battery run of an issue as
    its own trial; `first` keeps only each issue's earliest run, which is free
    of the selection bias of re-running issues that failed."""
    seen, unique = set(), []
    for t in sorted(all_trials, key=lambda t: (t['repository'], t['modelPath'], t['issue'], t['startedAt'], t['source'])):
        key = (t['repository'], t['modelPath'], t['issue'], t['startedAt'])
        if key in seen:
            continue
        seen.add(key)
        unique.append(t)
    duplicates = len(all_trials) - len(unique)
    if rule == 'first':
        firsts = {}
        for t in unique:
            if t['complete']:
                firsts.setdefault((t['repository'], t['modelPath'], t['issue']), t)
        incomplete = [t for t in unique if not t['complete'] and (t['repository'], t['modelPath'], t['issue']) not in firsts]
        unique = sorted(firsts.values(), key=lambda t: (t['repository'], t['modelPath'], t['issue'])) + incomplete
    return unique, duplicates


def counter_rows(counter, fields):
    rows = [dict(zip(fields, key), count=count) for key, count in counter.items()]
    return sorted(rows, key=lambda row: (-row['count'],) + tuple(row[f] for f in fields))


def summarize_cell(cell_trials):
    complete = [t for t in cell_trials if t['complete']]
    incomplete = [t for t in cell_trials if not t['complete']]
    n = len(complete)
    issues = sorted({t['issue'] for t in complete})
    within = sum(1 for t in complete if t['successAt'] is not None and t['successAt'] <= 2)
    first = sum(1 for t in complete if t['successAt'] == 1)
    attempts = [a for t in complete for a in t['attempts']]
    attempt_successes = sum(1 for a in attempts if a.get('outcome') == 'success')
    second = [t for t in complete if len(t['attempts']) >= 2 and t['successAt'] != 1]
    by_issue = {}
    for t in complete:
        by_issue.setdefault(t['issue'], []).append(t)
    pass_at = {}
    for k in (1, 2):
        per_issue = [sum(1 for t in ts if t['successAt'] is not None and t['successAt'] <= k) / len(ts) for ts in by_issue.values()]
        pass_at[str(k)] = r4(sum(per_issue) / len(per_issue)) if per_issue else None
    stops, finals = Counter(), Counter()
    matrix = {verdict: dict.fromkeys(OUTCOMES, 0) for verdict in VERDICTS}
    sources, repairs = Counter(), Counter()
    for t in complete:
        for a in t['attempts']:
            if a.get('outcome') != 'success':
                stops[stop_class(a)] += 1
            verdict, source = attempt_verdict(a)
            matrix[verdict][outcome_column(a)] += 1
            sources[source] += 1
            if any(review_verdict(s) == 'changes-requested' for s in a.get('reviews') or []):
                repairs[outcome_column(a)] += 1
        if t['successAt'] is None:
            finals[stop_class(t['attempts'][-1])] += 1
    delivered_seconds = [sum(a.get('seconds') or 0 for a in t['attempts']) for t in complete if t['successAt'] is not None]
    revisions = sorted({t['revision'] for t in complete if t['revision']})
    cell = {
        'repository': cell_trials[0]['repository'], 'modelPath': cell_trials[0]['modelPath'],
        'trials': n, 'distinctIssues': len(issues), 'incompleteTrials': len(incomplete),
        'truncatedTrials': sum(1 for t in complete if t['truncated']),
        'attempts': len(attempts),
        'deliveredWithinTwo': rate(within, n),
        'firstAttempt': rate(first, n),
        'passAtK': pass_at,
        'attemptSuccess': rate(attempt_successes, len(attempts)),
        'secondAttemptRecovery': rate(sum(1 for t in second if t['successAt'] == 2), len(second)),
        'medianSecondsToDelivery': round(statistics.median(delivered_seconds)) if delivered_seconds else None,
        'modelCalls': sum(a.get('modelCalls') or 0 for a in attempts),
        'stops': counter_rows(stops, ('stage', 'reason', 'diagnostic')),
        'finalStops': counter_rows(finals, ('stage', 'reason', 'diagnostic')),
        'reviewer': {'matrix': {v: row for v, row in matrix.items() if any(row.values())},
                     'observedAttempts': sources['observed'], 'derivedAttempts': sources['derived'],
                     'changesRequested': {o: repairs[o] for o in OUTCOMES if repairs[o]}},
        'revisions': revisions,
        'sources': sorted({t['source'] for t in cell_trials}, key=natural),
    }
    cell['gate'] = gate(cell)
    return cell


def gate(cell):
    """Whether the cell meets the Beta functional gate, and why not."""
    reasons = []
    within = cell['deliveredWithinTwo']
    if cell['modelPath'] == UNSPECIFIED:
        reasons.append('the model path is unspecified: record --cell-model-path or pass --model-path')
    if cell['distinctIssues'] < GATE['minIssues']:
        reasons.append('%d distinct issues, at least %d needed' % (cell['distinctIssues'], GATE['minIssues']))
    if within['n'] and within['rate'] < GATE['minRate']:
        reasons.append('delivered within two attempts %.1f%%, at least %.0f%% needed' % (100 * within['rate'], 100 * GATE['minRate']))
    if within['n'] and within['wilson95'][0] < GATE['minLowerBound']:
        reasons.append('95%% lower bound %.1f%%, at least %.0f%% needed' % (100 * within['wilson95'][0], 100 * GATE['minLowerBound']))
    if not within['n']:
        reasons.append('no complete trials')
    warnings = []
    if len(cell['revisions']) > 1:
        warnings.append('the trials span %d installation revisions' % len(cell['revisions']))
    if cell['incompleteTrials']:
        warnings.append('%d incomplete trials excluded' % cell['incompleteTrials'])
    if cell['truncatedTrials']:
        warnings.append('%d undelivered trials had fewer than two attempts and count as not delivered' % cell['truncatedTrials'])
    return {'passed': not reasons, 'reasons': reasons, 'warnings': warnings}


def overall(cells):
    """The plan asks for two repositories and two model paths, every cell
    passing: some 2x2 grid of passing cells. Language is not in the reports."""
    passing = {(c['repository'], c['modelPath']) for c in cells if c['gate']['passed']}
    repositories = sorted({r for r, _ in passing})
    paths = sorted({m for _, m in passing})
    grid = None
    for i, r1 in enumerate(repositories):
        for r2 in repositories[i + 1:]:
            for j, m1 in enumerate(paths):
                for m2 in paths[j + 1:]:
                    if grid is None and {(r1, m1), (r1, m2), (r2, m1), (r2, m2)} <= passing:
                        grid = {'repositories': [r1, r2], 'modelPaths': [m1, m2]}
    return {'passingCells': len(passing), 'grid': grid, 'passed': grid is not None,
            'note': 'The two repositories must also be in different languages; check that by hand.'}


def summarize(paths, model_path=None, labels=None, rule='trial'):
    labels = {str(Path(p)): label for p, label in (labels or {}).items()}
    inputs, collected = [], []
    for path in paths:
        report, digest = load(path)
        label = labels.get(str(Path(path)), model_path)
        found = trials(path, report, label)
        collected += found
        inputs.append({'path': str(path), 'sha256': digest, 'repository': report['repository'],
                       'modelPath': found[0]['modelPath'] if found else (label or UNSPECIFIED),
                       'trials': len(found), 'partial': bool(report.get('inProgress')),
                       'stopped': report.get('stopped')})
    unique, duplicates = dedupe(collected, rule)
    groups = {}
    for t in unique:
        groups.setdefault((t['repository'], t['modelPath']), []).append(t)
    cells = [summarize_cell(groups[key]) for key in sorted(groups)]
    return {'schemaVersion': SUMMARY_SCHEMA, 'dedupe': rule, 'duplicateTrialsDropped': duplicates,
            'gateCriteria': GATE, 'inputs': sorted(inputs, key=lambda i: natural(i['path'])), 'cells': cells, 'overall': overall(cells)}


def natural(text):
    """Sorts battery-9 before battery-10."""
    return [(0, int(part), '') if part.isdigit() else (1, 0, part) for part in re.split(r'(\d+)', text)]


def pct(r):
    if not r['n']:
        return 'n/a'
    return '%d/%d = %.1f%% [%.1f%%, %.1f%%]' % (r['successes'], r['n'], 100 * r['rate'], 100 * r['wilson95'][0], 100 * r['wilson95'][1])


def markdown(summary):
    lines = ['# Evaluation summary', '',
             'Dedupe rule: `%s`. Gate: at least %d distinct issues, delivered within two attempts at least %.0f%%, '
             '95%% Wilson lower bound at least %.0f%%.' % (summary['dedupe'], GATE['minIssues'], 100 * GATE['minRate'], 100 * GATE['minLowerBound']),
             '', '| Report | Model path | Trials | Partial |', '|---|---|---|---|']
    for i in summary['inputs']:
        lines.append('| `%s` | `%s` | %d | %s |' % (Path(i['path']).name, i['modelPath'], i['trials'], 'yes' if i['partial'] else 'no'))
    if summary['duplicateTrialsDropped']:
        lines += ['', '%d duplicate trials dropped.' % summary['duplicateTrialsDropped']]
    for c in summary['cells']:
        g = c['gate']
        lines += ['', '## `%s` × `%s`' % (c['repository'], c['modelPath']), '',
                  '**Beta gate: %s.**%s' % ('met' if g['passed'] else 'not met', (' ' + '; '.join(g['reasons']) + '.') if g['reasons'] else ''), '']
        if g['warnings']:
            lines += ['Warnings: ' + '; '.join(g['warnings']) + '.', '']
        lines += ['| Measure | Value |', '|---|---|',
                  '| Trials (distinct issues) | %d (%d) |' % (c['trials'], c['distinctIssues']),
                  '| Delivered within two attempts | %s |' % pct(c['deliveredWithinTwo']),
                  '| First attempt | %s |' % pct(c['firstAttempt']),
                  '| pass@1, pass@2 (per distinct issue) | %s, %s |' % tuple('n/a' if v is None else '%.3f' % v for v in (c['passAtK']['1'], c['passAtK']['2'])),
                  '| Attempt success | %s |' % pct(c['attemptSuccess']),
                  '| Second attempt recovers a failed first | %s |' % pct(c['secondAttemptRecovery']),
                  '| Median seconds to delivery | %s |' % ('n/a' if c['medianSecondsToDelivery'] is None else c['medianSecondsToDelivery']),
                  '| Model calls | %d over %d attempts |' % (c['modelCalls'], c['attempts']),
                  '| Installation revisions | %s |' % (', '.join('`%s`' % r for r in c['revisions']) or 'not recorded')]
        for title, rows in (('Stops by attempt', c['stops']), ('Final stop of undelivered trials', c['finalStops'])):
            if rows:
                lines += ['', '### ' + title, '', '| Stage | Reason | Diagnostic | Count |', '|---|---|---|---|']
                lines += ['| %s | %s | `%s` | %d |' % (r['stage'], r['reason'], r['diagnostic'], r['count']) for r in rows]
        rv = c['reviewer']
        lines += ['', '### Reviewer verdict by attempt outcome', '',
                  'Observed for %d attempts, derived from the stop for %d.' % (rv['observedAttempts'], rv['derivedAttempts']), '',
                  '| Last verdict | ' + ' | '.join(OUTCOMES) + ' |', '|---' * (len(OUTCOMES) + 1) + '|']
        lines += ['| %s | %s |' % (v, ' | '.join(str(row[o]) for o in OUTCOMES)) for v, row in rv['matrix'].items()]
    o = summary['overall']
    lines += ['', '## Overall', '',
              'Passing cells: %d. Two repositories × two model paths all passing: %s. %s' % (
                  o['passingCells'], 'yes, %s' % json.dumps(o['grid'], sort_keys=True) if o['passed'] else 'no', o['note'])]
    return '\n'.join(lines) + '\n'


def parse_label(value):
    path, sep, label = value.rpartition('=')
    if not sep or not path or not MODEL_PATH.match(label):
        raise argparse.ArgumentTypeError('expected REPORT=LABEL with a short lowercase label')
    return path, label


def model_label(value):
    if not MODEL_PATH.match(value):
        raise argparse.ArgumentTypeError('expected a short lowercase label such as local-llama or agent-cli')
    return value


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('reports', nargs='+', type=Path, help='Battery report JSON files, partial ones included')
    parser.add_argument('--model-path', type=model_label, help='Model path for reports that do not record one')
    parser.add_argument('--label', type=parse_label, action='append', default=[], metavar='REPORT=LABEL',
                        help='Model path for one report that does not record one (repeatable)')
    parser.add_argument('--dedupe', choices=('trial', 'first'), default='trial',
                        help='trial: every battery run of an issue is a trial; first: only each issue\'s earliest run')
    parser.add_argument('--json', type=Path, help='Write the JSON summary here (default: standard output)')
    parser.add_argument('--markdown', type=Path, help='Write the Markdown summary here')
    args = parser.parse_args(argv)
    try:
        summary = summarize(args.reports, args.model_path, dict(args.label), args.dedupe)
    except (OSError, ValueError) as error:
        parser.error(str(error))
    text = json.dumps(summary, indent=2, sort_keys=True) + '\n'
    if args.json:
        args.json.write_text(text)
    else:
        sys.stdout.write(text)
    if args.markdown:
        args.markdown.write_text(markdown(summary))
    return 0


if __name__ == '__main__':
    sys.exit(main())
