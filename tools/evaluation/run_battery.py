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
`runtime-blocked`, when a verification cleanup or an execution cleanup that will not complete blocks all later work.
The runner reads the runtime's database and logs through `kubectl` with the
installation's kubeconfig; it writes nothing to the cluster.

The report names its evaluation cell (the repository and a short model-path
label such as `local-llama` or `agent-cli`) and, when given, the installation
revision and the image lock digest, so `summarize.py` can group batteries per
cell. Each attempt also records the furthest stage it reached and the review
runs created during it (`state|reason`). All of these fields are additive:
reports written before them still parse.
"""
import argparse
import hashlib
import json
import re
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

LABEL = 'ai-sdlc:shadow'
REPORT_SCHEMA = 2
MODEL_PATH = re.compile(r'^[a-z0-9][a-z0-9-]{0,39}$')
REPOSITORY = re.compile(r'^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$')
# The furthest stage an attempt reached, in delivery order.
STAGES = ('intake', 'execution', 'verification', 'review', 'merge')
POLL_SECONDS = 20
# An execution cleanup that will not complete: the runtime blocks all worker
# work until an operator acts (store.ExecutionCleanupBlocked). Normal retries
# (pending with fewer than three attempts) are not terminal.
# cleanup_attempts rises when an attempt is claimed, so a third attempt is only
# terminal once its retry time (cleanup_after) has passed.
TERMINAL_CLEANUP = "(l.cleanup_state='uncertain' OR (l.cleanup_state='pending' AND l.cleanup_attempts>=3 AND l.cleanup_after<=now()))"


RETRY_DELAYS = (5, 15, 45)


def run(args, retry=True, **kwargs):
    """Runs a read-only observation command. A transient failure (a forge
    or cluster API blip, a timeout) is retried with backoff, so one blip does
    not abort a battery of many hours; the last failure is raised."""
    for delay in (RETRY_DELAYS if retry else ()) + (None,):
        try:
            return subprocess.run(args, check=True, capture_output=True, text=True, timeout=120, **kwargs).stdout
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
            if delay is None:
                raise
            time.sleep(delay)


class Runtime:
    def __init__(self, kubectl, kubeconfig, namespace='ai-sdlc-runtime', repository=None):
        self.base = [kubectl, '--kubeconfig', str(kubeconfig), '-n', namespace]
        if repository is not None and not REPOSITORY.match(repository):
            raise ValueError('repository must be OWNER/NAME')
        self.repository = repository

    def issue_filter(self, issue):
        """Matches a request payload to this battery's issue, and to its
        repository when known, so an installation serving several
        repositories does not mix issues that share a number."""
        clause = "convert_from(payload,'UTF8')::json->>'issueNumber' = '%d'" % int(issue)
        if self.repository:
            # GitHub names are case-insensitive; REPOSITORY admits no quotes.
            clause += " AND lower(convert_from(payload,'UTF8')::json->>'repository') = '%s'" % self.repository.lower()
        return clause

    def sql(self, query):
        out = run(self.base + ['exec', 'ai-sdlc-postgres-0', '--', 'psql', '-U', 'ai_sdlc', '-d', 'ai_sdlc', '-Atc', query])
        return [line for line in out.splitlines() if line]

    def stop_lines(self, since):
        """The components' safe stop diagnostics since the attempt started."""
        lines = []
        for deployment, container in (('ai-sdlc-worker', 'worker'), ('ai-sdlc-verifier', 'verifier')):
            try:
                # Diagnostics are best effort: no retry ladder, a failure is skipped.
                out = run(self.base + ['logs', 'deploy/' + deployment, '-c', container, '--since-time=' + since], retry=False)
            except subprocess.SubprocessError:
                continue
            lines += sorted({line.split(' ', 2)[-1] for line in out.splitlines() if 'stopped safely' in line})
        return lines[:10]

    def review_states(self, issue, since):
        """The review runs for this issue created since the attempt started,
        oldest first, as `state|reason` (for example `failed|request-changes`
        before a repair, then `passed`)."""
        executions = "SELECT work_item_uuid FROM execution_requests WHERE " + self.issue_filter(issue)
        return self.sql("SELECT state||'|'||reason FROM review_runs WHERE work_item_uuid IN (%s) "
                        "AND created_at >= '%s'::timestamptz ORDER BY created_at, id" % (executions, since))

    def images(self):
        """The installed deployments' images, best effort, so a report can be
        tied to the revision that produced it."""
        try:
            out = run(self.base + ['get', 'deployments', '-o',
                                   'jsonpath={range .items[*]}{.metadata.name}={.spec.template.spec.containers[0].image}{"\\n"}{end}'], retry=False)
        except subprocess.SubprocessError:
            return {}
        return dict(sorted(line.split('=', 1) for line in out.splitlines() if '=' in line))

    def blocked(self):
        """A failed verification whose cleanup is not complete, or an execution
        cleanup that will not complete, blocks all later work."""
        rows = self.sql("SELECT count(*) FROM verification_requests WHERE state='failed' AND cleanup_state<>'complete'")
        if bool(rows) and rows[0] != '0':
            return True
        rows = self.sql("SELECT count(*) FROM execution_lifecycles l JOIN execution_requests r ON r.id=l.request_id "
                        "WHERE r.state='stopped' AND " + TERMINAL_CLEANUP)
        return bool(rows) and rows[0] != '0'

    def snapshot(self, issue):
        """This issue's counts and latest states, enough to attribute a new attempt."""
        def one(query):
            rows = self.sql(query)
            return rows[0] if rows else ''
        executions = "FROM execution_requests WHERE " + self.issue_filter(issue)
        verifications = "FROM verification_requests WHERE " + self.issue_filter(issue)
        reviews = "FROM review_runs WHERE work_item_uuid IN (SELECT work_item_uuid %s)" % executions
        return {
            'executions': int(one('SELECT count(*) ' + executions) or 0),
            'execution': one("SELECT state||'|'||stop_reason %s ORDER BY created_at DESC, id DESC LIMIT 1" % executions),
            # A captured candidate can still be handed to verification after
            # its execution stopped (for example after a failed pod cleanup).
            'executionCandidate': one("SELECT count(*) FROM execution_candidates WHERE request_id=(SELECT id %s ORDER BY created_at DESC, id DESC LIMIT 1)" % executions) not in ('', '0'),
            # The handoff can no longer happen: cleanup ended uncertain or ran
            # out of retries, the handoff was refused, or the request expired
            # without one.
            'handoffImpossible': one(("SELECT count(*) FROM execution_requests r JOIN execution_lifecycles l ON l.request_id=r.id "
                                      "WHERE r.id=(SELECT id %s ORDER BY created_at DESC, id DESC LIMIT 1) AND (" + TERMINAL_CLEANUP + " "
                                      "OR EXISTS(SELECT 1 FROM verification_handoffs h WHERE h.candidate_id=r.id AND h.state='rejected') "
                                      "OR (r.expires_at<=now() AND NOT EXISTS(SELECT 1 FROM verification_handoffs h WHERE h.candidate_id=r.id)))") % executions) not in ('', '0'),
            'handoffReason': one("SELECT reason FROM verification_handoffs WHERE state='rejected' AND candidate_id=(SELECT id %s ORDER BY created_at DESC, id DESC LIMIT 1)" % executions),
            'verifications': int(one('SELECT count(*) ' + verifications) or 0),
            'verification': one("SELECT state||'|'||reason %s ORDER BY created_at DESC, id DESC LIMIT 1" % verifications),
            'reviews': int(one('SELECT count(*) ' + reviews) or 0),
            'review': one("SELECT state||'|'||reason %s ORDER BY created_at DESC, id DESC LIMIT 1" % reviews),
            # Model calls are not keyed by issue; attempts run one at a time.
            'modelCalls': int(one('SELECT count(*) FROM model_calls') or 0),
        }


class GitHub:
    def __init__(self, repository):
        self.repository = repository

    def label(self, issue, add):
        """Adds or removes the trigger label. A transient forge failure is
        retried with the same backoff as observations: battery 29 aborted
        after many hours when one label edit met a GitHub API outage.
        Editing a label is idempotent, so repeating it is safe."""
        flag = '--add-label' if add else '--remove-label'
        for delay in RETRY_DELAYS + (None,):
            try:
                result = subprocess.run(['gh', 'issue', 'edit', str(issue), '--repo', self.repository, flag, LABEL],
                                        capture_output=True, text=True, timeout=60)
                if result.returncode == 0:
                    return
            except subprocess.TimeoutExpired:
                pass
            if delay is None:
                break
            time.sleep(delay)
        if add:
            raise RuntimeError('could not add the trigger label to issue %d' % issue)

    def merged_pull(self, issue, since):
        """A merged pull request from the runtime's branch for this issue."""
        out = run(['gh', 'pr', 'list', '--repo', self.repository, '--state', 'merged', '--limit', '50',
                   '--json', 'number,headRefName,mergedAt'])
        for pr in json.loads(out):
            if pr['headRefName'].startswith('ai-sdlc/issue-%d-' % issue) and pr['mergedAt'] >= since:
                return pr['number']
        return None


def stage_reached(result, before, after):
    """The furthest stage the attempt reached, from the snapshots around it."""
    if result['outcome'] == 'success':
        return 'merge'
    if after['reviews'] > before['reviews']:
        return 'review'
    if after['verifications'] > before['verifications']:
        return 'verification'
    if after['executions'] > before['executions']:
        return 'execution'
    return 'intake'


def image_lock_digest(path):
    """The sha256 of the image lock file and its image tag, if it has one."""
    data = Path(path).read_bytes()
    try:
        tag = json.loads(data).get('image', {}).get('tag')
    except (ValueError, AttributeError):
        tag = None
    return 'sha256:' + hashlib.sha256(data).hexdigest(), tag if isinstance(tag, str) else None


def attempt(runtime, github, issue, timeout):
    github.label(issue, False)
    time.sleep(40)
    before = runtime.snapshot(issue)
    since = datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    github.label(issue, True)
    # The attempt's budget and its reported seconds start once the label is
    # on: a label edit retried through a forge outage is not runtime time.
    started = time.monotonic()
    result = {'issue': issue, 'startedAt': since}
    # Verifications are compared per execution: a repair execution that stops
    # after an earlier verification in the same attempt is still a failure.
    seen_executions, handoff_base = before['executions'], before['verifications']
    while time.monotonic() - started < timeout:
        time.sleep(POLL_SECONDS)
        merged = github.merged_pull(issue, since)
        now = runtime.snapshot(issue)
        new_execution = now['executions'] > before['executions']
        stage = None
        if merged:
            result.update(outcome='success', pullRequest=merged)
            break
        if now['executions'] != seen_executions:
            seen_executions, handoff_base = now['executions'], now['verifications']
        stopped = new_execution and now['execution'].startswith('stopped|') and now['execution'] != 'stopped|candidate-captured'
        if stopped and now['verifications'] > handoff_base:
            # Already handed off: the verification below decides.
            result['executionStop'] = now['execution'].split('|', 1)[1]
            stopped = False
        elif stopped and now.get('executionCandidate') and not now.get('handoffImpossible'):
            # The stopped execution captured a candidate: the runtime hands it
            # to verification once its cleanup completes, so the attempt goes on.
            result['executionStop'] = now['execution'].split('|', 1)[1]
            stopped = False
        captured = new_execution and now['execution'] == 'stopped|candidate-captured'
        if stopped:
            stage = ('execution', now['execution'].split('|', 1)[1])
        elif captured and now.get('handoffImpossible') and now['verifications'] <= handoff_base:
            # The captured candidate will never reach verification, for
            # example a rejected handoff (unsupported recipe, revoked).
            stage = ('handoff', now.get('handoffReason') or 'impossible')
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
    result['stageReached'] = stage_reached(result, before, after)
    try:
        result['reviews'] = runtime.review_states(issue, since)
    except subprocess.SubprocessError:
        pass  # Best effort: the summary then derives the verdict from the stop.
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
    parser.add_argument('--cell-model-path', help='Short label of the model path for the evaluation cell, for example local-llama or agent-cli')
    parser.add_argument('--installation-revision', help='Source revision of the installed images (defaults to the image lock tag)')
    parser.add_argument('--image-lock', type=Path, help='Image lock file the installation used; its sha256 is recorded')
    args = parser.parse_args(argv)
    if args.cell_model_path is not None and not MODEL_PATH.match(args.cell_model_path):
        parser.error('--cell-model-path must be a short lowercase label such as local-llama or agent-cli')
    if not REPOSITORY.match(args.repository):
        parser.error('--repository must be OWNER/NAME')
    runtime, github = Runtime(args.kubectl, args.kubeconfig, repository=args.repository), GitHub(args.repository)
    report = {'schemaVersion': REPORT_SCHEMA, 'repository': args.repository, 'attemptBudget': args.attempts,
              'cell': {'repository': args.repository, 'modelPath': args.cell_model_path}}
    installation = {}
    if args.image_lock:
        installation['imageLockDigest'], tag = image_lock_digest(args.image_lock)
        if tag and not args.installation_revision:
            installation['revision'] = tag
    if args.installation_revision:
        installation['revision'] = args.installation_revision
    images = runtime.images()
    if images:
        installation['images'] = images
    report['installation'] = dict(sorted(installation.items()))
    report['issues'] = []
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
