#!/usr/bin/env python3
"""Build the flow console demo fixture from committed evaluation battery reports.

The fixture is ai-sdlc.flow-event/v1 JSON lines (docs/operations/flow-console.md).
Each attempt in the battery reports keeps its documented facts: start time,
total seconds, model call count, the stage it reached, its stop reason and
diagnostic, and whether it delivered. The finer timing inside an attempt
(effects, individual model calls, queue waits) is synthesised deterministically
because the reports do not record it, and every stop is named the way the
journal collector names it (stop reason, exit code, lifecycle phase), not with
the log line. A few lanes marked with the attribute source=illustrative add
the states the batteries did not hit: a quarantined publication, a stale
queue wait with uncertain cleanup, a person-blocked admission and in-flight
work.

Usage: python3 tools/flow_console_fixture.py OUTPUT.jsonl [REPORT.json ...]
The output is byte-identical for the same inputs. It reads only the reports.
"""
import datetime as dt
import hashlib
import json
import random
import sys
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_REPORTS = [ROOT / 'docs/evidence/2026-11-25-evaluation-battery-29.json',
                   ROOT / 'docs/evidence/2026-11-25-evaluation-battery-30.json']
COMPONENT = {'intake': 'intake', 'admission': 'admission-controller', 'planning': 'orchestrator', 'execution': 'worker',
             'verification': 'verifier', 'review': 'verifier', 'publication': 'release-manager', 'merge': 'release-manager'}
VERSION = 'ai-sdlc.flow-event/v1'


def parse_time(value):
    return dt.datetime.strptime(value, '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=dt.timezone.utc)


def stamp(t):
    return t.astimezone(dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S.') + '%03dZ' % (t.microsecond // 1000)


class Lane:
    def __init__(self, out, repository, issue, work_item):
        self.out, self.repository, self.issue, self.work_item = out, repository, issue, work_item

    def emit(self, stage, kind, t, step='', attempt=0, diagnostic='', duration=None, latency=None, **attrs):
        key = '|'.join([self.repository, str(self.issue), str(attempt), stage, step, kind, stamp(t), diagnostic])
        event = {'version': VERSION, 'id': hashlib.sha256(key.encode()).hexdigest()[:24], 'time': stamp(t),
                 'component': COMPONENT[stage], 'repository': self.repository, 'issue': self.issue}
        if stage not in ('intake', 'admission'):
            event['workItem'] = self.work_item
        if attempt:
            event['attempt'] = attempt
        event['stage'] = stage
        if step:
            event['step'] = step
        event['kind'] = kind
        if diagnostic:
            event['diagnostic'] = diagnostic
        if duration:
            event['durationMs'] = int(duration * 1000)
        if latency:
            event['latencyMs'] = int(latency * 1000)
        attrs = {k: str(v) for k, v in attrs.items() if v is not None and v != ''}
        if attrs:
            event['attributes'] = attrs
        self.out.append(event)


def seconds(n):
    return dt.timedelta(seconds=n)


def front(lane, start, generation, rng, decision='accepted'):
    """Intake observation, admission decision and planning before an attempt."""
    observed = start - seconds(rng.uniform(35, 55))
    lane.emit('intake', 'completed', observed, 'observed', generation=generation)
    decided = observed + seconds(rng.uniform(3, 6))
    if decision == 'accepted':
        lane.emit('admission', 'completed', decided, 'decision', duration=rng.uniform(0.3, 1.2), decision='accepted', risk='low')
        lane.emit('planning', 'completed', decided + seconds(rng.uniform(2, 4)), 'work-item', status='ready')
    else:
        lane.emit('admission', 'blocked', decided, 'decision', diagnostic=decision, duration=rng.uniform(0.3, 1.2), decision=decision, risk='medium')


def stage_queue(lane, stage, t, attempt, rng, wait):
    lane.emit(stage, 'waiting', t, 'queued', attempt, waitedMs=int(wait * 1000))
    lane.emit(stage, 'started', t + seconds(wait), 'claim', attempt)
    return t + seconds(wait)


def effects(lane, stage, t, attempt, rng, kinds, spread):
    for kind in kinds:
        took = rng.uniform(*spread)
        t = t + seconds(took)
        lane.emit(stage, 'progressed', t, kind, attempt, duration=took)
    return t


def execution(lane, start, attempt, total, calls, rng, outcome, extra):
    """Generation: claim, executor effects, model calls, then candidate or stop."""
    claim = stage_queue(lane, 'execution', start, attempt, rng, rng.uniform(2, 6))
    t = effects(lane, 'execution', claim, attempt, rng, ['config', 'pod', 'source', 'start'], (2, 9))
    if outcome.get('phase') == 'reserved':
        end = claim + seconds(total)
        lane.emit('execution', 'stopped-safely', end, 'stop', attempt, 'uncertain', duration=(end - claim).total_seconds(),
                  reason='uncertain', phase='reserved', modelCalls=0)
        return end
    end = start + seconds(total)
    export_at = end - seconds(rng.uniform(6, 12))
    window = (export_at - t).total_seconds()
    previous = t
    degraded_at = None
    for n in range(1, calls + 1):
        at = t + seconds(window * n / (calls + 0.6) + rng.uniform(-4, 4))
        if at <= previous:
            at = previous + seconds(1)
        lane.emit('execution', 'progressed', at, 'model-call', attempt, latency=(at - previous).total_seconds(),
                  sequence=n, inputBytes=rng.randint(18000, 61000), outputBytes=rng.randint(400, 9000), responseStatus=200)
        previous = at
    if extra.get('degraded'):
        degraded_at = previous + seconds(rng.uniform(20, 60))
        status = extra['degraded']
        attrs = {'sequence': calls + 1, 'inputBytes': rng.randint(30000, 61000)}
        if status.startswith('model-status-'):
            attrs['responseStatus'] = status.rsplit('-', 1)[1]
        lane.emit('execution', 'degraded', degraded_at, 'model-call', attempt, status, **attrs)
    if outcome['kind'] == 'completed':
        lane.emit('execution', 'progressed', end, 'export', attempt, duration=(end - export_at).total_seconds())
        lane.emit('execution', 'completed', end, 'candidate', attempt, duration=(end - claim).total_seconds(),
                  changes=rng.randint(2, 6), modelCalls=calls, exitCode=outcome.get('exitCode'))
    else:
        if outcome.get('phase') == 'export-issued':
            lane.emit('execution', 'progressed', export_at, 'export', attempt, duration=rng.uniform(1, 3))
        lane.emit('execution', 'stopped-safely', end, 'stop', attempt, outcome['diagnostic'], duration=(end - claim).total_seconds(),
                  reason='uncertain', phase=outcome['phase'], exitCode=outcome.get('exitCode'), modelCalls=calls)
    return end


def verdict_stage(lane, stage, t, attempt, rng, kinds, spread, wait, result='completed', reason='', calls=0):
    claim = stage_queue(lane, stage, t, attempt, rng, wait)
    t = effects(lane, stage, claim, attempt, rng, kinds, spread)
    previous = claim
    for n in range(1, calls + 1):
        took = rng.uniform(25, 75)
        at = t + seconds(took)
        lane.emit(stage, 'progressed', at, 'model-call', attempt, latency=took, sequence=n,
                  inputBytes=rng.randint(40000, 98000), outputBytes=rng.randint(300, 1800))
        t = previous = at
    end = t + seconds(rng.uniform(2, 6))
    lane.emit(stage, result, end, 'verdict', attempt, reason if result != 'completed' else '', duration=(end - claim).total_seconds(),
              reason=reason or None, effects=len(kinds))
    return end


DIAGNOSTIC = {
    # Battery log stop name -> (journal phase, how the collector names the stop, degraded marker)
    'model-provider': ('start-ready', 'uncertain', 'model-status-502'),
    'model-authorization-publication-snapshot': ('start-ready', 'uncertain', 'model-call-stopped'),
    'export': ('export-issued', 'uncertain', None),
    'readiness-request': ('reserved', 'uncertain', None),
}


def attempt_outcome(a):
    if a['outcome'] == 'success':
        return {'kind': 'completed'}, {}
    text = (a.get('diagnostics') or [''])[0]
    if a['stage'] == 'verification':
        return {'kind': 'verification-failed', 'reason': a['reason']}, {}
    name = text.split('stopped safely: ', 1)[-1].split(' ', 1)[0]
    if name == 'worker-exit':
        code = text.split('workerExitCode=', 1)[1].split(' ', 1)[0]
        return {'kind': 'stopped', 'phase': 'start-ready', 'diagnostic': 'worker-exit-' + code, 'exitCode': code}, {}
    phase, diagnostic, degraded = DIAGNOSTIC.get(name, ('start-ready', 'uncertain', None))
    return {'kind': 'stopped', 'phase': phase, 'diagnostic': diagnostic}, ({'degraded': degraded} if degraded else {})


def battery_lane(out, repository, record):
    issue = record['issue']
    rng = random.Random(issue * 7919)
    work_item = str(uuid.UUID(hashlib.sha256(('%s#%d' % (repository, issue)).encode()).hexdigest()[:32], version=4))
    lane = Lane(out, repository, issue, work_item)
    for index, a in enumerate(record['attempts'], 1):
        start = parse_time(a['startedAt'])
        total = a['seconds']
        front(lane, start, index, rng)
        if index > 1:
            lane.emit('execution', 'retried', start, 'attempt', index, previousAttempt=index - 1)
        outcome, extra = attempt_outcome(a)
        if outcome['kind'] == 'stopped':
            execution(lane, start, index, total, a['modelCalls'], rng, outcome, extra)
            continue
        share = rng.uniform(0.52, 0.64) if outcome['kind'] == 'completed' else 0.74
        t = execution(lane, start, index, total * share, a['modelCalls'], rng, {'kind': 'completed'}, {})
        verify_kinds = ['config', 'baseline', 'candidate', 'reference', 'tests']
        end = start + seconds(total)
        if outcome['kind'] == 'verification-failed':
            verdict_stage(lane, 'verification', t + seconds(2), index, rng, verify_kinds,
                          ((end - t).total_seconds() / 6.5, (end - t).total_seconds() / 5.5), rng.uniform(2, 5), 'failed', outcome['reason'])
            continue
        t = verdict_stage(lane, 'verification', t + seconds(2), index, rng, verify_kinds, (12, 30), rng.uniform(2, 5))
        t = verdict_stage(lane, 'review', t + seconds(2), index, rng, ['config', 'pod', 'import', 'start'], (2, 6), rng.uniform(2, 8), calls=rng.randint(2, 4))
        t = verdict_stage(lane, 'publication', t + seconds(2), index, rng, ['tree', 'commit', 'branch', 'pull-request'], (1, 3), rng.uniform(1, 4))
        remaining = max(30, (end - t).total_seconds() - 8)
        claim = stage_queue(lane, 'merge', t + seconds(2), index, rng, remaining * 0.82)
        merged = claim + seconds(remaining * 0.18)
        lane.emit('merge', 'progressed', merged - seconds(1), 'merge', index, duration=1.2)
        lane.emit('merge', 'completed', merged, 'verdict', index, duration=(merged - claim).total_seconds(), effects=1)


def illustrative(out, repository, end):
    """States the batteries did not reach, clearly marked source=illustrative."""
    def lane(issue):
        work_item = str(uuid.UUID(hashlib.sha256(('%s#%d' % (repository, issue)).encode()).hexdigest()[:32], version=4))
        return Lane(out, repository, issue, work_item), random.Random(issue * 104729)
    # 223: delivered up to publication, whose pull-request write is unresolved: quarantined.
    l, rng = lane(223)
    start = end - seconds(2400)
    front(l, start, 1, rng)
    t = execution(l, start, 1, 700, 19, rng, {'kind': 'completed'}, {})
    t = verdict_stage(l, 'verification', t + seconds(2), 1, rng, ['config', 'baseline', 'candidate', 'reference', 'tests'], (12, 30), 3)
    t = verdict_stage(l, 'review', t + seconds(2), 1, rng, ['config', 'pod', 'import', 'start'], (2, 6), 4, calls=3)
    claim = stage_queue(l, 'publication', t + seconds(2), 1, rng, 2)
    t2 = effects(l, 'publication', claim, 1, rng, ['tree', 'commit', 'branch'], (1, 3))
    stop = t2 + seconds(31)
    l.emit('publication', 'stopped-safely', stop, 'verdict', 1, 'uncertain', duration=(stop - claim).total_seconds(), reason='uncertain', effects=4, source='illustrative')
    l.emit('publication', 'quarantined', stop, 'pull-request', 1, 'unresolved-effect', reason='uncertain', source='illustrative')
    # 224: execution waited past the stale threshold (busy model), then ran; verification cleanup uncertain.
    l, rng = lane(224)
    start = end - seconds(3300)
    front(l, start, 1, rng)
    l.emit('execution', 'waiting', start, 'queued', 1, waitedMs=14 * 60 * 1000)
    l.emit('execution', 'waiting', start + seconds(600), 'queued', 1, 'stale-wait', waitedMs=14 * 60 * 1000, source='illustrative')
    claim = start + seconds(14 * 60)
    l.emit('execution', 'started', claim, 'claim', 1)
    t = effects(l, 'execution', claim, 1, rng, ['config', 'pod', 'source', 'start'], (2, 9))
    previous = t
    for n in range(1, 15):
        at = previous + seconds(rng.uniform(25, 45))
        l.emit('execution', 'progressed', at, 'model-call', 1, latency=(at - previous).total_seconds(), sequence=n, inputBytes=rng.randint(18000, 61000), outputBytes=rng.randint(400, 9000), responseStatus=200)
        previous = at
    done = previous + seconds(9)
    l.emit('execution', 'progressed', done, 'export', 1, duration=6)
    l.emit('execution', 'completed', done, 'candidate', 1, duration=(done - claim).total_seconds(), changes=3, modelCalls=14)
    vt = verdict_stage(l, 'verification', done + seconds(2), 1, rng, ['config', 'baseline', 'candidate', 'reference', 'tests'], (12, 30), 3, 'stopped-safely', 'expired')
    l.emit('verification', 'quarantined', vt, 'cleanup', 1, 'cleanup-uncertain', cleanup='uncertain', cleanupAttempts=3, source='illustrative')
    # 225: admission needs a person.
    l, rng = lane(225)
    front(l, end - seconds(1500), 1, rng, decision='needs-clarification')
    # 226: in flight now: review running with a lapsed-lease-free claim.
    l, rng = lane(226)
    start = end - seconds(1260)
    front(l, start, 1, rng)
    t = execution(l, start, 1, 640, 17, rng, {'kind': 'completed'}, {})
    t = verdict_stage(l, 'verification', t + seconds(2), 1, rng, ['config', 'baseline', 'candidate', 'reference', 'tests'], (12, 30), 3)
    claim = stage_queue(l, 'review', t + seconds(2), 1, rng, 5)
    t = effects(l, 'review', claim, 1, rng, ['config', 'pod', 'import', 'start'], (2, 6))
    l.emit('review', 'progressed', t + seconds(48), 'model-call', 1, latency=48.2, sequence=1, inputBytes=81234, outputBytes=912)
    # 227: in flight: generation running, one degraded call already retried by the gateway.
    l, rng = lane(227)
    start = end - seconds(420)
    front(l, start, 1, rng)
    claim = stage_queue(l, 'execution', start, 1, rng, 4)
    t = effects(l, 'execution', claim, 1, rng, ['config', 'pod', 'source', 'start'], (2, 9))
    previous = t
    for n in range(1, 8):
        at = previous + seconds(rng.uniform(25, 45))
        l.emit('execution', 'progressed', at, 'model-call', 1, latency=(at - previous).total_seconds(), sequence=n, inputBytes=rng.randint(18000, 61000), outputBytes=rng.randint(400, 9000), responseStatus=200)
        previous = at
    l.emit('execution', 'degraded', previous + seconds(20), 'model-call', 1, 'model-status-503', sequence=8, inputBytes=40211, responseStatus=503, source='illustrative')
    # 228: queued for verification now (waiting).
    l, rng = lane(228)
    start = end - seconds(900)
    front(l, start, 1, rng)
    t = execution(l, start, 1, 760, 22, rng, {'kind': 'completed'}, {})
    l.emit('verification', 'waiting', t + seconds(3), 'queued', 1)


def main(argv):
    if len(argv) < 2:
        raise SystemExit(__doc__)
    reports = [Path(p) for p in argv[2:]] or DEFAULT_REPORTS
    out = []
    last = None
    repository = None
    for path in reports:
        report = json.loads(path.read_text())
        repository = report['repository']
        for record in report['issues']:
            battery_lane(out, repository, record)
            for a in record['attempts']:
                t = parse_time(a['startedAt']) + seconds(a['seconds'])
                last = t if last is None or t > last else last
    illustrative(out, repository, last + seconds(240))
    out.sort(key=lambda e: (e['time'], e['id']))
    seen = set()
    for e in out:
        assert e['id'] not in seen, e
        seen.add(e['id'])
    header = '# ai-sdlc.flow-event/v1 demo fixture generated by tools/flow_console_fixture.py from ' + ', '.join(p.name for p in reports) + '\n'
    Path(argv[1]).write_text(header + ''.join(json.dumps(e, separators=(',', ':')) + '\n' for e in out))


if __name__ == '__main__':
    main(sys.argv)
