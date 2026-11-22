#!/usr/bin/env python3
"""Trace one issue through the delivery chain. Read-only; never advances work."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

import install

# Ordered issue-correlated stages. Stages that do not exist yet are reported
# explicitly 'unavailable'; stages that exist but cannot be observed under
# row-level security report 'observation-denied'.
STAGES = ('revision', 'admission', 'plan', 'execution', 'verification',
          'review', 'publication', 'merge', 'release', 'deployment')

# Stable, bounded reason codes. They distinguish blocked dependencies, denied
# authority, missing credentials, rate limits, exhausted budgets and failed
# evidence without exposing payloads, prose or transcripts.
REASON_CODES = frozenset({
    'none', 'in-progress', 'unavailable', 'issue-not-observed',
    'observation-denied', 'awaiting-human', 'dependency-blocked',
    'authority-denied', 'credentials-missing', 'rate-limited',
    'budget-exhausted', 'evidence-failed', 'changes-requested',
    'expired', 'rejected', 'uncertain', 'stopped'})

DECISIONS = ('accepted', 'rejected', 'needs-clarification', 'needs-human', 'requires-decomposition')

_REASON_MAP = {'revoked': 'authority-denied', 'authority-revoked': 'authority-denied',
               'rejected': 'evidence-failed', 'comparison-failed': 'evidence-failed',
               'request-changes': 'changes-requested', 'expired': 'expired',
               'uncertain': 'uncertain', 'aborted': 'stopped'}


def validate_scope(repository, issue):
    if (not isinstance(repository, str)
            or not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository)
            or any(part in ('.', '..') for part in repository.split('/'))
            or type(issue) is not int or not 1 <= issue <= 2147483647):
        raise ValueError('Invalid issue scope')
    return repository, issue


def _scope(repository, issue):
    return f"repository='{repository}' AND issue_number={issue}"


def _chain(repository, issue):
    # Shared CTE prefix: latest revision, its plans, their work items and the
    # bounded journal chain keyed by work_item_uuid / record references. No
    # payloads, receipts, proofs, credentials or issue text are ever selected.
    return f"""WITH latest AS (
      SELECT * FROM intake_revisions WHERE {_scope(repository, issue)}
        ORDER BY generation DESC LIMIT 1
    ), issue_plans AS (
      SELECT p.id FROM plans p JOIN latest l ON p.correlation_id = l.event_id
    ), issue_items AS (
      SELECT w.uuid FROM work_items w JOIN issue_plans ip ON w.plan_id = ip.id
    ), reqs AS (
      SELECT r.* FROM execution_requests r JOIN issue_items i ON r.work_item_uuid = i.uuid
    ), revs AS (
      SELECT r.* FROM review_runs r JOIN issue_items i ON r.work_item_uuid = i.uuid
    ), pubs AS (
      SELECT p.* FROM publication_requests p JOIN revs r ON p.review_id = r.id
    ), pubrecs AS (
      SELECT pr.* FROM published_records pr JOIN pubs p ON pr.publication_id = p.id
    ), merges AS (
      SELECT m.* FROM merge_requests m JOIN pubrecs pr ON m.publication_id = pr.publication_id
    ), mrecs AS (
      SELECT mr.* FROM merged_records mr JOIN merges m ON mr.merge_id = m.id
    ), builds AS (
      SELECT b.* FROM build_requests b JOIN mrecs mr ON b.merge_id = mr.merge_id
    ), built AS (
      SELECT br.* FROM built_records br JOIN builds b ON br.request_id = b.id
    ), provs AS (
      SELECT pv.* FROM provenance_requests pv JOIN built br ON pv.build_id = br.request_id
    ), provrecs AS (
      SELECT pr.* FROM provenance_records pr JOIN provs pv ON pr.request_id = pv.id
    ), promos AS (
      SELECT pm.* FROM promotion_requests pm JOIN provrecs pr ON pm.provenance_id = pr.request_id
    ), promorecs AS (
      SELECT pr.* FROM promotion_records pr JOIN promos pm ON pr.request_id = pm.id
    ), deploys AS (
      SELECT d.* FROM deployment_requests d JOIN promorecs pr ON d.promotion_id = pr.request_id
    ), terms AS (
      SELECT t.* FROM terminal_requests t JOIN deploys d ON t.deployment_id = d.id
    )"""


def _counts(table, column, source, nonempty=False):
    where = f" WHERE {column}<>''" if nonempty else ''
    return (f"(SELECT coalesce(jsonb_object_agg({column},n),'{{}}'::jsonb) FROM "
            f"(SELECT {column},count(*) n FROM {source}{where} GROUP BY {column}) {table})")


def scope_queries(repository, issue):
    """Bounded read-only observation queries for one repository issue.

    Every statement runs inside BEGIN READ ONLY and selects only statuses,
    enum-bounded reasons, timestamps, counts and digests. Each query is
    independent: a privilege denial on row-level-security journals degrades
    that stage to 'observation-denied' instead of failing the whole report.
    """
    validate_scope(repository, issue)
    scope = _scope(repository, issue)
    chain = _chain(repository, issue)
    queries = {'core': f"""BEGIN READ ONLY;
    {chain}
    SELECT json_build_object(
      'now', now(),
      'revision', (SELECT json_build_object('generation',l.generation,'observedAt',l.observed_at) FROM latest l),
      'intakeCursor', (SELECT json_build_object('watermark',c.watermark,'nextPoll',c.next_poll,
          'failures',c.failures,'lagSeconds',EXTRACT(EPOCH FROM (now()-c.watermark)))
          FROM intake_cursors c WHERE c.repository='{repository}' LIMIT 1),
      'admission', (SELECT json_build_object('decision',a.decision,'risk',a.risk,
          'reasonCount',jsonb_array_length(a.reasons),
          'requiredGateCount',jsonb_array_length(a.required_gates),'createdAt',a.created_at)
          FROM admissions a JOIN latest l ON a.repository=l.repository
          AND a.issue_number=l.issue_number AND a.correlation_id=l.event_id),
      'admissionGeneration', (SELECT generation FROM admission_heads WHERE {scope}),
      'planningGeneration', (SELECT generation FROM planning_heads WHERE {scope}),
      'plan', json_build_object('plans',(SELECT count(*) FROM issue_plans),
          'itemStatuses',{_counts('items', 'status',
              '(SELECT w.status FROM work_items w JOIN issue_plans ip ON w.plan_id=ip.id) items')}),
      'attempts', (SELECT json_build_object('count',count(*),
          'maxAttempt',coalesce(max(t.attempt_number),0),
          'unsafe',coalesce(bool_or(t.unsafe_or_ambiguous),false))
          FROM attempts t JOIN issue_items i ON t.work_item_uuid=i.uuid),
      'releaseGates', {_counts('gates', 'state',
          '(SELECT g.state FROM release_gates g JOIN latest l ON g.correlation_id=l.event_id) gates')});
    COMMIT;""",
    'execution': f"""BEGIN READ ONLY;
    {chain}
    SELECT json_build_object(
      'states', {_counts('rs', 'state', 'reqs rs')},
      'maxAttempt', (SELECT coalesce(max(attempt),0) FROM reqs),
      'stopReasons', {_counts('sr', 'stop_reason', 'reqs sr', True)},
      'oldestQueued', (SELECT min(created_at) FROM reqs WHERE state='queued'),
      'queueAgeSeconds', (SELECT EXTRACT(EPOCH FROM (now()-min(created_at))) FROM reqs WHERE state='queued'),
      'lifecyclePhases', {_counts('lp', 'phase',
          '(SELECT l.phase FROM execution_lifecycles l JOIN reqs r ON l.request_id=r.id) lp')},
      'failures', (SELECT json_build_object('count',count(*),'lastExitCode',max(f.exit_code),
          'lastObservedAt',max(f.observed_at))
          FROM execution_failures f JOIN reqs r ON f.request_id=r.id));
    COMMIT;""",
    'model': f"""BEGIN READ ONLY;
    {chain}
    SELECT json_build_object(
      'budgets', (SELECT json_build_object('count',count(*),
          'stopped',count(*) FILTER (WHERE b.state='stopped'),
          'exhausted',count(*) FILTER (WHERE b.reserved_calls>=b.max_calls),
          'reservedCalls',coalesce(sum(b.reserved_calls),0),
          'maxCalls',coalesce(sum(b.max_calls),0))
          FROM model_budgets b JOIN issue_items i ON b.work_item_uuid=i.uuid),
      'callStates', {_counts('cs', 'state',
          '(SELECT m.state FROM model_calls m JOIN model_allocations a ON m.allocation_id=a.id '
          'JOIN issue_items i ON a.work_item_uuid=i.uuid) cs')},
      'responseStatuses', {_counts('st', 'response_status::text',
          '(SELECT m.response_status::text FROM model_calls m JOIN model_allocations a '
          'ON m.allocation_id=a.id JOIN issue_items i ON a.work_item_uuid=i.uuid '
          'WHERE m.response_status IS NOT NULL) st')});
    COMMIT;""",
    'evidence': f"""BEGIN READ ONLY;
    {chain}
    SELECT json_build_object(
      'verification', {_counts('vs', 'state',
          '(SELECT v.state FROM verification_requests v JOIN reqs r ON v.candidate_id=r.id) vs')},
      'verificationReasons', {_counts('vr', 'reason',
          '(SELECT v.reason FROM verification_requests v JOIN reqs r ON v.candidate_id=r.id) vr', True)},
      'verificationCleanups', (SELECT coalesce(sum(v.cleanup_attempts),0)
          FROM verification_requests v JOIN reqs r ON v.candidate_id=r.id),
      'review', {_counts('rw', 'state', 'revs rw')},
      'reviewReasons', {_counts('rr', 'reason', 'revs rr', True)},
      'reviewCleanups', (SELECT coalesce(sum(r.cleanup_attempts),0) FROM revs r),
      'publication', {_counts('ps', 'state', 'pubs ps')},
      'publicationReasons', {_counts('pr', 'reason', 'pubs pr', True)},
      'published', (SELECT count(*) FROM pubrecs),
      'merge', {_counts('ms', 'state', 'merges ms')},
      'mergeReasons', {_counts('mr', 'reason', 'merges mr', True)},
      'merged', (SELECT count(*) FROM mrecs));
    COMMIT;""",
    'release': f"""BEGIN READ ONLY;
    {chain}
    SELECT json_build_object(
      'builds', (SELECT count(*) FROM builds),
      'built', (SELECT count(*) FROM built),
      'provenance', (SELECT count(*) FROM provrecs),
      'promotion', (SELECT count(*) FROM promorecs),
      'deployment', {_counts('ds', 'state', 'deploys ds')},
      'deploymentReasons', {_counts('dr', 'reason', 'deploys dr', True)},
      'terminal', {_counts('ts', 'state', 'terms ts')},
      'terminalReasons', {_counts('tr', 'reason', 'terms tr', True)},
      'production', (SELECT json_build_object('count',count(*),'lastRecordedAt',max(recorded_at))
          FROM production_records p JOIN terms t ON p.terminal_id=t.id));
    COMMIT;"""}
    return queries


def _total(counts):
    return sum(counts.values()) if isinstance(counts, dict) else 0


def _reason(failed_reasons, default='evidence-failed'):
    for reason, code in _REASON_MAP.items():
        if failed_reasons.get(reason):
            return code
    return default


def interpret(observed, repository, issue):
    """Map bounded observations to the ordered stage chain, stable reason
    codes and bounded-cardinality metrics. Pure: no I/O, no free text."""
    core = observed.get('core') or {}
    stages = {name: {'status': 'unavailable', 'reasonCode': 'unavailable'} for name in STAGES}
    report = {'repository': repository, 'issue': issue, 'complete': False,
              'stages': stages, 'blockingReasonCode': 'issue-not-observed',
              'metrics': {'intakeLagSeconds': None, 'queueAgeSeconds': None,
                          'retryCounts': {'intake': 0, 'execution': 0, 'verification': 0, 'review': 0},
                          'activeWork': {'queued': 0, 'claimed': 0, 'running': 0},
                          'terminalFailures': {'execution': 0, 'verification': 0, 'review': 0,
                                               'publication': 0, 'merge': 0, 'deployment': 0}}}
    metrics = report['metrics']
    cursor = core.get('intakeCursor') or {}
    metrics['intakeLagSeconds'] = cursor.get('lagSeconds')
    metrics['retryCounts']['intake'] = cursor.get('failures') or 0
    revision = core.get('revision')
    if revision is None:
        return report
    report['blockingReasonCode'] = 'in-progress'
    stages['revision'] = {'status': 'complete', 'reasonCode': 'none',
                          'generation': revision.get('generation')}
    admission = core.get('admission') or {}
    decision = admission.get('decision')
    if decision == 'accepted':
        stages['admission'] = {'status': 'complete', 'reasonCode': 'none', 'decision': decision}
    elif decision in DECISIONS:
        code = 'authority-denied' if decision == 'rejected' else 'awaiting-human'
        stages['admission'] = {'status': 'blocked', 'reasonCode': code, 'decision': decision}
        report['blockingReasonCode'] = code
        return report
    else:
        stages['admission'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
        return report
    plan = core.get('plan') or {}
    items = plan.get('itemStatuses') or {}
    if not plan.get('plans'):
        stages['plan'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
        return report
    if items.get('blocked'):
        stages['plan'] = {'status': 'blocked', 'reasonCode': 'dependency-blocked'}
        report['blockingReasonCode'] = 'dependency-blocked'
        return report
    stages['plan'] = {'status': 'complete', 'reasonCode': 'none', 'items': items}
    metrics['activeWork']['running'] = items.get('running', 0) + items.get('ready', 0)
    metrics['retryCounts']['execution'] = (core.get('attempts') or {}).get('maxAttempt') or 0

    execution = observed.get('execution')
    if execution is None:
        stages['execution'] = {'status': 'unavailable', 'reasonCode': 'observation-denied'}
    else:
        states = execution.get('states') or {}
        stops = execution.get('stopReasons') or {}
        failures = execution.get('failures') or {}
        metrics['activeWork']['queued'] = states.get('queued', 0)
        metrics['activeWork']['claimed'] = states.get('claimed', 0)
        metrics['queueAgeSeconds'] = execution.get('queueAgeSeconds')
        metrics['retryCounts']['execution'] = max(metrics['retryCounts']['execution'],
                                                  execution.get('maxAttempt') or 0)
        metrics['terminalFailures']['execution'] = failures.get('count') or 0
        phases = execution.get('lifecyclePhases') or {}
        if stops.get('authority-revoked'):
            stages['execution'] = {'status': 'blocked', 'reasonCode': 'authority-denied'}
            report['blockingReasonCode'] = 'authority-denied'
            return report
        if phases.get('candidate-captured') or phases.get('export-ready'):
            stages['execution'] = {'status': 'complete', 'reasonCode': 'none'}
        elif stops.get('expired'):
            stages['execution'] = {'status': 'failed', 'reasonCode': 'expired'}
        elif failures.get('count'):
            stages['execution'] = {'status': 'failed', 'reasonCode': 'evidence-failed'}
        elif stops.get('aborted') or stops.get('uncertain'):
            stages['execution'] = {'status': 'failed',
                                   'reasonCode': 'stopped' if stops.get('aborted') else 'uncertain'}
        elif states.get('queued') or states.get('claimed'):
            stages['execution'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
        elif execution.get('maxAttempt', 0) >= 16:
            stages['execution'] = {'status': 'blocked', 'reasonCode': 'budget-exhausted'}
            report['blockingReasonCode'] = 'budget-exhausted'
            return report
        else:
            stages['execution'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
    model = observed.get('model') or {}
    budgets = model.get('budgets') or {}
    statuses = model.get('responseStatuses') or {}
    if budgets.get('stopped') or budgets.get('exhausted'):
        report['blockingReasonCode'] = 'budget-exhausted'
        if stages['execution']['status'] in ('in-progress', 'unavailable'):
            stages['execution'] = {'status': 'blocked', 'reasonCode': 'budget-exhausted'}
        return report
    if statuses.get('429'):
        report['blockingReasonCode'] = 'rate-limited'
        if stages['execution']['status'] in ('in-progress', 'unavailable'):
            stages['execution'] = {'status': 'blocked', 'reasonCode': 'rate-limited'}
        return report

    evidence = observed.get('evidence')
    if evidence is None:
        for name in ('verification', 'review', 'publication', 'merge'):
            stages[name] = {'status': 'unavailable', 'reasonCode': 'observation-denied'}
    else:
        metrics['retryCounts']['verification'] = evidence.get('verificationCleanups') or 0
        metrics['retryCounts']['review'] = evidence.get('reviewCleanups') or 0
        metrics['terminalFailures']['verification'] = (evidence.get('verification') or {}).get('failed', 0)
        metrics['terminalFailures']['review'] = (evidence.get('review') or {}).get('failed', 0)
        metrics['terminalFailures']['publication'] = (evidence.get('publication') or {}).get('failed', 0)
        metrics['terminalFailures']['merge'] = (evidence.get('merge') or {}).get('failed', 0)
        metrics['activeWork']['queued'] += ((evidence.get('verification') or {}).get('queued', 0)
                                            + (evidence.get('review') or {}).get('queued', 0)
                                            + (evidence.get('publication') or {}).get('queued', 0)
                                            + (evidence.get('merge') or {}).get('queued', 0))
        reached = stages['execution']['status'] == 'complete'
        vstates = evidence.get('verification') or {}
        vreasons = evidence.get('verificationReasons') or {}
        if vstates.get('failed'):
            code = _reason(vreasons)
            stages['verification'] = {'status': 'failed', 'reasonCode': code}
            report['blockingReasonCode'] = code
            return report
        if vstates.get('passed'):
            stages['verification'] = {'status': 'complete', 'reasonCode': 'none'}
        elif _total(vstates):
            stages['verification'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
        elif reached:
            stages['verification'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
        rstates = evidence.get('review') or {}
        rreasons = evidence.get('reviewReasons') or {}
        if rstates.get('failed'):
            code = _reason(rreasons)
            stages['review'] = {'status': 'failed', 'reasonCode': code}
            report['blockingReasonCode'] = code
            return report
        if rstates.get('passed'):
            stages['review'] = {'status': 'complete', 'reasonCode': 'none'}
        elif _total(rstates):
            stages['review'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
        pstates = evidence.get('publication') or {}
        preasons = evidence.get('publicationReasons') or {}
        if pstates.get('failed'):
            code = _reason(preasons)
            stages['publication'] = {'status': 'failed', 'reasonCode': code}
            report['blockingReasonCode'] = code
            return report
        if evidence.get('published') or pstates.get('published'):
            stages['publication'] = {'status': 'complete', 'reasonCode': 'none'}
        elif _total(pstates):
            stages['publication'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
        mstates = evidence.get('merge') or {}
        mreasons = evidence.get('mergeReasons') or {}
        if mstates.get('failed'):
            code = _reason(mreasons)
            stages['merge'] = {'status': 'failed', 'reasonCode': code}
            report['blockingReasonCode'] = code
            return report
        if evidence.get('merged') or mstates.get('merged'):
            stages['merge'] = {'status': 'complete', 'reasonCode': 'none'}
        elif _total(mstates):
            stages['merge'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
    if stages['merge']['status'] != 'complete':
        return report
    release = observed.get('release')
    if release is None:
        for name in ('release', 'deployment'):
            stages[name] = {'status': 'unavailable', 'reasonCode': 'observation-denied'}
        return report
    dreasons = release.get('deploymentReasons') or {}
    treasons = release.get('terminalReasons') or {}
    metrics['terminalFailures']['deployment'] = ((release.get('deployment') or {}).get('quarantined', 0)
                                                 + (release.get('terminal') or {}).get('quarantined', 0))
    if release.get('builds') or release.get('built'):
        stages['release'] = {'status': 'complete' if release.get('promotion') else 'in-progress',
                             'reasonCode': 'none' if release.get('promotion') else 'in-progress'}
    else:
        stages['release'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
    dstates = release.get('deployment') or {}
    tstates = release.get('terminal') or {}
    production = release.get('production') or {}
    if dstates.get('quarantined') or tstates.get('quarantined'):
        code = _reason(dict(dreasons, **treasons))
        stages['deployment'] = {'status': 'failed', 'reasonCode': code}
        report['blockingReasonCode'] = code
        return report
    if production.get('count') or tstates.get('completed'):
        stages['deployment'] = {'status': 'complete', 'reasonCode': 'none',
                                'recordedAt': production.get('lastRecordedAt')}
        report['complete'] = True
        report['blockingReasonCode'] = 'none'
    elif _total(dstates) or _total(tstates):
        stages['deployment'] = {'status': 'in-progress', 'reasonCode': 'in-progress'}
    return report


def kubectl_base(cluster, context=None):
    cache = Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/tools'
    clients = install.tools(cache)
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state')) / 'ai-sdlc' / cluster
    return [clients['kubectl'], '--kubeconfig', str(state / 'kubeconfig'),
            '--context', context or 'kind-' + cluster, '-n', install.NAMESPACE]


def query_read_only(kubectl, sql, timeout=20):
    if not sql.lstrip().startswith('BEGIN READ ONLY'):
        raise ValueError('Issue status executes only read-only transactions')
    result = subprocess.run(kubectl + ['exec', '-i', 'ai-sdlc-postgres-0', '--',
                            'psql', '-U', 'ai_sdlc', '-d', 'ai_sdlc', '-XAtq', '-v', 'ON_ERROR_STOP=1'],
                            input=sql, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        return None
    return json.loads(result.stdout)


def observe(kubectl, repository, issue):
    """Run every bounded query; privilege denials degrade per stage."""
    validate_scope(repository, issue)
    observed = {}
    queries = scope_queries(repository, issue)
    core = query_read_only(kubectl, queries['core'])
    if core is None:
        raise ValueError('Read-only database observation failed')
    observed['core'] = core
    for name, sql in queries.items():
        if name != 'core':
            observed[name] = query_read_only(kubectl, sql)
    return interpret(observed, repository, issue)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', default='ai-sdlc')
    parser.add_argument('--provider', choices=['docker', 'podman'], required=True)
    parser.add_argument('--context', help='Kubeconfig context; defaults to kind-<cluster>')
    parser.add_argument('--repository', default='os-santiago/ai-sdlc')
    parser.add_argument('--issue', required=True, type=int)
    args = parser.parse_args(argv)
    install.main(['status', '--cluster', args.cluster, '--provider', args.provider])
    report = observe(kubectl_base(args.cluster, args.context), args.repository, args.issue)
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print('Issue status observation failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
