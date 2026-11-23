#!/usr/bin/env python3
"""Run an evaluation battery of issues against an installed ai-sdlc runtime.

For each issue the only input is the authorized trigger label (removed, then
re-added). The runner observes, never advances, the delivery:

  * success: a pull request for the issue is merged within the timeout;
  * failure: the runtime stops the attempt (execution, verification, review or
    publication), recorded with its stage and reason;
  * timeout: neither happened in time.

Results go to a JSON report with per-attempt durations, model calls and stop
reasons, plus the success rates (first attempt, and within the attempt budget).
When an issue's attempts end, the runner removes its trigger label, so no
other installation watching the repository picks it up later. A failed
attempt also records the components' own safe stop lines (for example
`worker-exit ... workerExitCode=23`). The runner stops early, with the reason
`runtime-blocked`, when a verification cleanup blocks all later work.
The runner reads the runtime's database and logs through `kubectl` with the
installation's kubeconfig; it writes nothing to the cluster.
"""
import argparse
import json
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

LABEL = 'ai-sdlc:shadow'
POLL_SECONDS = 20
# Polls an execution stop must persist without a verification handoff.
HANDOFF_POLLS = 2


def run(args, **kwargs):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=120, **kwargs).stdout


class Runtime:
    def __init__(self, kubectl, kubeconfig, namespace='ai-sdlc-runtime'):
        self.base = [kubectl, '--kubeconfig', str(kubeconfig), '-n', namespace]

    def sql(self, query):
        out = run(self.base + ['exec', 'ai-sdlc-postgres-0', '--', 'psql', '-U', 'ai_sdlc', '-d', 'ai_sdlc', '-Atc', query])
        return [line for line in out.splitlines() if line]

    def stop_lines(self, since):
        """The components' safe stop diagnostics since the attempt started."""
        lines = []
        for deployment, container in (('ai-sdlc-worker', 'worker'), ('ai-sdlc-verifier', 'verifier')):
            try:
                out = run(self.base + ['logs', 'deploy/' + deployment, '-c', container, '--since-time=' + since])
            except subprocess.SubprocessError:
                continue
            lines += sorted({line.split(' ', 2)[-1] for line in out.splitlines() if 'stopped safely' in line})
        return lines[:10]

    def blocked(self):
        """A failed verification whose cleanup is not complete blocks all later work."""
        rows = self.sql("SELECT count(*) FROM verification_requests WHERE state='failed' AND cleanup_state<>'complete'")
        return bool(rows) and rows[0] != '0'

    def snapshot(self, issue):
        """This issue's counts and latest states, enough to attribute a new attempt."""
        def one(query):
            rows = self.sql(query)
            return rows[0] if rows else ''
        n = int(issue)
        executions = "FROM execution_requests WHERE convert_from(payload,'UTF8')::json->>'issueNumber' = '%d'" % n
        verifications = "FROM verification_requests WHERE convert_from(payload,'UTF8')::json->>'issueNumber' = '%d'" % n
        reviews = "FROM review_runs WHERE work_item_uuid IN (SELECT work_item_uuid %s)" % executions
        return {
            'executions': int(one('SELECT count(*) ' + executions) or 0),
            'execution': one("SELECT state||'|'||stop_reason %s ORDER BY created_at DESC LIMIT 1" % executions),
            'verifications': int(one('SELECT count(*) ' + verifications) or 0),
            'verification': one("SELECT state||'|'||reason %s ORDER BY created_at DESC LIMIT 1" % verifications),
            'reviews': int(one('SELECT count(*) ' + reviews) or 0),
            'review': one("SELECT state||'|'||reason %s ORDER BY created_at DESC LIMIT 1" % reviews),
            # Model calls are not keyed by issue; attempts run one at a time.
            'modelCalls': int(one('SELECT count(*) FROM model_calls') or 0),
        }


class GitHub:
    def __init__(self, repository):
        self.repository = repository

    def label(self, issue, add):
        flag = '--add-label' if add else '--remove-label'
        result = subprocess.run(['gh', 'issue', 'edit', str(issue), '--repo', self.repository, flag, LABEL],
                                capture_output=True, text=True, timeout=60)
        if add and result.returncode != 0:
            raise RuntimeError('could not add the trigger label to issue %d' % issue)

    def merged_pull(self, issue, since):
        """A merged pull request from the runtime's branch for this issue."""
        out = run(['gh', 'pr', 'list', '--repo', self.repository, '--state', 'merged', '--limit', '50',
                   '--json', 'number,headRefName,mergedAt'])
        for pr in json.loads(out):
            if pr['headRefName'].startswith('ai-sdlc/issue-%d-' % issue) and pr['mergedAt'] >= since:
                return pr['number']
        return None


def attempt(runtime, github, issue, timeout):
    github.label(issue, False)
    time.sleep(40)
    before = runtime.snapshot(issue)
    since = datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    started = time.monotonic()
    github.label(issue, True)
    result = {'issue': issue, 'startedAt': since}
    stop_polls = 0
    while time.monotonic() - started < timeout:
        time.sleep(POLL_SECONDS)
        merged = github.merged_pull(issue, since)
        now = runtime.snapshot(issue)
        new_execution = now['executions'] > before['executions']
        stage = None
        if merged:
            result.update(outcome='success', pullRequest=merged)
            break
        handed_off = now['verifications'] > before['verifications']
        if new_execution and now['execution'].startswith('stopped|') and now['execution'] != 'stopped|candidate-captured' and not handed_off:
            # A stop after capture (for example a failed pod cleanup) can
            # still hand the candidate to verification a few seconds later.
            # Conclude only when the next poll still shows no handoff.
            stop_polls += 1
            if stop_polls >= HANDOFF_POLLS:
                stage = ('execution', now['execution'].split('|', 1)[1])
        elif now['verifications'] > before['verifications'] and now['verification'].startswith('failed|'):
            stage = ('verification', now['verification'].split('|', 1)[1])
        elif now['reviews'] > before['reviews'] and now['review'].startswith('failed|') and now['review'] != 'failed|request-changes':
            # A request for changes leads to a repair, not a final stop.
            stage = ('review', now['review'].split('|', 1)[1])
        if stage:
            result.update(outcome='failure', stage=stage[0], reason=stage[1])
            break
    else:
        result.update(outcome='timeout')
    after = runtime.snapshot(issue)
    result['seconds'] = round(time.monotonic() - started)
    result['modelCalls'] = after['modelCalls'] - before['modelCalls']
    if result['outcome'] != 'success':
        result['diagnostics'] = runtime.stop_lines(since)
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--repository', required=True)
    parser.add_argument('--issues', required=True, help='Comma-separated issue numbers')
    parser.add_argument('--kubectl', required=True)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--attempts', type=int, default=2)
    parser.add_argument('--timeout-minutes', type=int, default=15)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args(argv)
    runtime, github = Runtime(args.kubectl, args.kubeconfig), GitHub(args.repository)
    report = {'repository': args.repository, 'attemptBudget': args.attempts, 'issues': []}
    for issue in [int(n) for n in args.issues.split(',')]:
        if runtime.blocked():
            report['stopped'] = 'runtime-blocked'
            break
        attempts = []
        for _ in range(args.attempts):
            if attempts and runtime.blocked():
                break
            result = attempt(runtime, github, issue, args.timeout_minutes * 60)
            attempts.append(result)
            print(json.dumps(result), flush=True)
            args.output.write_text(json.dumps(report | {'inProgress': attempts}, indent=2) + '\n')
            if result['outcome'] == 'success':
                break
        # Leave no trigger label behind: another installation watching the
        # same repository must not pick the issue up after the battery.
        github.label(issue, False)
        report['issues'].append({'issue': issue, 'attempts': attempts,
                                 'success': attempts[-1]['outcome'] == 'success'})
        args.output.write_text(json.dumps(report, indent=2) + '\n')
    total = len(report['issues'])
    report.pop('inProgress', None)
    first = sum(1 for i in report['issues'] if i['attempts'][0]['outcome'] == 'success')
    within = sum(1 for i in report['issues'] if i['success'])
    report['summary'] = {'issues': total, 'firstAttemptSuccess': first, 'successWithinBudget': within,
                         'firstAttemptRate': round(first / total, 3) if total else 0,
                         'rateWithinBudget': round(within / total, 3) if total else 0}
    args.output.write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report['summary']))
    return 0


if __name__ == '__main__':
    sys.exit(main())
