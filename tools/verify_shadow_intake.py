#!/usr/bin/env python3
"""Observe a real issue's signed shadow path. Never inject or advance work."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

import install


def observation_sql(repository, issue):
    if (not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository)
            or any(part in ('.', '..') for part in repository.split('/'))
            or type(issue) is not int or issue < 1):
        raise ValueError('Invalid issue scope')
    # Both interpolated values are constrained above; this query is read-only
    # and never returns payloads, credentials, issue text or raw error bodies.
    return f"""BEGIN READ ONLY;
    WITH latest AS (
      SELECT * FROM intake_revisions WHERE repository='{repository}'
        AND issue_number={issue} ORDER BY generation DESC LIMIT 1
    ) SELECT json_build_object(
      'generation', coalesce((SELECT generation FROM latest),0),
      'admission', (SELECT decision FROM admissions a JOIN latest l
        ON a.repository=l.repository AND a.issue_number=l.issue_number
        AND a.correlation_id=l.event_id),
      'admissionGeneration', (SELECT generation FROM admission_heads
        WHERE repository='{repository}' AND issue_number={issue}),
      'planningGeneration', (SELECT generation FROM planning_heads
        WHERE repository='{repository}' AND issue_number={issue}),
      'plans', (SELECT count(*) FROM plans p JOIN latest l ON p.correlation_id=l.event_id),
      'blockedItems', (SELECT count(*) FROM work_items w JOIN plans p ON w.plan_id=p.id
        JOIN latest l ON p.correlation_id=l.event_id WHERE w.status='blocked'),
      'signedDeliveries', (SELECT count(*) FROM authorized_outbox o JOIN latest l
        ON o.event_id IN (l.event_id,'admission-'||l.event_id)
        WHERE o.published_at IS NOT NULL AND o.operation IN ('admit-issue','plan-admission')),
      'signedReceipts', (SELECT count(*) FROM authorized_inbox i JOIN latest l
        ON i.event_id IN (l.event_id,'admission-'||l.event_id)),
      'executionRequests', (SELECT count(*) FROM authorized_outbox
        WHERE operation NOT IN ('admit-issue','plan-admission')),
      'attempts', (SELECT count(*) FROM attempts),
      'releaseGates', (SELECT count(*) FROM release_gates));
    COMMIT;"""


def verified(observed, minimum_generation, expected):
    generation = observed.get('generation', 0)
    return (generation >= minimum_generation
            and observed.get('admissionGeneration') == generation
            and observed.get('planningGeneration') == generation
            and observed.get('admission') == expected
            and observed.get('plans') == (1 if expected == 'accepted' else 0)
            and observed.get('blockedItems') == (1 if expected == 'accepted' else 0)
            and observed.get('signedDeliveries') == 2
            and observed.get('signedReceipts') == 2
            and observed.get('executionRequests') == 0
            and observed.get('attempts') == 0
            and observed.get('releaseGates') == 0)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', default='ai-sdlc')
    parser.add_argument('--provider', choices=['docker', 'podman'], required=True)
    parser.add_argument('--repository', default='os-santiago/ai-sdlc')
    parser.add_argument('--issue', required=True, type=int)
    parser.add_argument('--minimum-generation', type=int, default=1)
    parser.add_argument('--expected', choices=['accepted', 'rejected'], default='accepted')
    parser.add_argument('--timeout', type=int, default=180)
    args = parser.parse_args()
    if not 1 <= args.timeout <= 600 or args.minimum_generation < 1:
        raise ValueError('Invalid verification bounds')
    sql = observation_sql(args.repository, args.issue)
    install.main(['status', '--cluster', args.cluster, '--provider', args.provider])
    cache = Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/tools'
    clients = install.tools(cache)
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state')) / 'ai-sdlc' / args.cluster
    kubectl = [clients['kubectl'], '--kubeconfig', str(state / 'kubeconfig'),
               '--context', 'kind-' + args.cluster, '-n', install.NAMESPACE]
    config = json.loads(subprocess.check_output(kubectl + ['get', 'configmap', 'ai-sdlc-config', '-o', 'json'], text=True, timeout=20))
    if config['data']['AI_SDLC_RUNTIME_MODE'] != 'shadow':
        raise ValueError('Verification requires the shadow profile')
    deadline = time.monotonic() + args.timeout
    while True:
        result = subprocess.run(kubectl + ['exec', '-i', 'ai-sdlc-postgres-0', '--',
                                'psql', '-U', 'ai_sdlc', '-d', 'ai_sdlc', '-XAtq', '-v', 'ON_ERROR_STOP=1'],
                                input=sql, text=True, capture_output=True, timeout=20)
        if result.returncode:
            raise ValueError('Read-only database observation failed')
        observed = json.loads(result.stdout)
        print(json.dumps(observed, sort_keys=True), flush=True)
        if verified(observed, args.minimum_generation, args.expected):
            report = dict(observed, shadowIntake='passed', repository=args.repository,
                          issue=args.issue, autonomousDeliveryVerified=False)
            (state / 'shadow-verification.json').write_text(json.dumps(report, indent=2) + '\n')
            return
        if time.monotonic() >= deadline:
            raise ValueError('Shadow intake has not reached the expected observable state')
        time.sleep(5)


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError):
        print('Shadow verification failed; inspect component readiness and bounded logs.', file=sys.stderr)
        sys.exit(1)
