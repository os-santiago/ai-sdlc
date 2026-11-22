#!/usr/bin/env python3
"""Read-only installation diagnostics. Never mutates or advances work."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys

import install
import issue_status

ROOT = Path(__file__).resolve().parents[1]
MIGRATIONS = ROOT / 'runtime/k8s/internal/store/migrations'

CHECKS = ('configuration', 'immutable-artifacts', 'database-migrations',
          'component-readiness', 'app-scopes', 'intake-connectivity',
          'model-connectivity')

RUNTIME_MODES = ('maintenance', 'shadow', 'candidate', 'verification')

# Credential Secret names whose *presence* is checked; values are never read.
GITHUB_CLIENT_SECRET = 'ai-sdlc-github-client'
MODEL_SECRETS = ('ai-sdlc-codex-auth', 'ai-sdlc-model-client',
                 'ai-sdlc-model-provider', 'ai-sdlc-review-model-client')

REASON_CODES = issue_status.REASON_CODES | frozenset({
    'ok', 'not-installed', 'misconfigured', 'image-mismatch',
    'migrations-incomplete', 'not-ready', 'scope-mismatch',
    'connectivity-degraded', 'stale'})


def validate_repository(repository):
    if (not isinstance(repository, str)
            or not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository)
            or any(part in ('.', '..') for part in repository.split('/'))):
        raise ValueError('Invalid repository scope')
    return repository


def read_only_sql(body):
    if 'BEGIN READ ONLY' not in body:
        raise ValueError('Doctor observations require a read-only transaction')
    return body


def intake_sql(repository):
    validate_repository(repository)
    return f"""BEGIN READ ONLY;
    SELECT json_build_object(
      'cursors', count(*),
      'failures', coalesce(max(failures),0),
      'watermark', max(watermark),
      'nextPoll', max(next_poll),
      'lagSeconds', EXTRACT(EPOCH FROM (now()-max(watermark))),
      'revisions', (SELECT count(*) FROM intake_revisions WHERE repository='{repository}'))
    FROM intake_cursors WHERE repository='{repository}';
    COMMIT;"""


def migrations_sql():
    return """BEGIN READ ONLY;
    SELECT json_build_object('applied', count(*)) FROM schema_migrations;
    COMMIT;"""


def model_sql():
    return """BEGIN READ ONLY;
    SELECT json_build_object(
      'calls', count(*),
      'states', (SELECT coalesce(jsonb_object_agg(state,n),'{}'::jsonb) FROM
        (SELECT state,count(*) n FROM model_calls GROUP BY state) s),
      'statuses', (SELECT coalesce(jsonb_object_agg(response_status::text,n),'{}'::jsonb) FROM
        (SELECT response_status::text,count(*) n FROM model_calls
         WHERE response_status IS NOT NULL GROUP BY response_status) s))
    FROM model_calls;
    COMMIT;"""


def _check(status, code, **details):
    if code not in REASON_CODES:
        raise ValueError('Reason code must come from the bounded set')
    return dict({'status': status, 'reasonCode': code}, **details)


def check_configuration(get, repository, environment):
    config = get('configmap', 'ai-sdlc-config')
    if config is None:
        return _check('failed', 'not-installed', detail='ai-sdlc-config ConfigMap is absent')
    mode = config.get('data', {}).get('AI_SDLC_RUNTIME_MODE', '')
    if mode not in RUNTIME_MODES:
        return _check('failed', 'misconfigured', detail='AI_SDLC_RUNTIME_MODE is missing or unsupported')
    authority = get('configmap', 'ai-sdlc-authority')
    if authority is None:
        return _check('failed', 'not-installed', detail='ai-sdlc-authority ConfigMap is absent')
    policy = json.loads(authority.get('data', {}).get('policy.json', '{}'))
    scope = {'repository': policy.get('repository'), 'environment': policy.get('environment')}
    if scope != {'repository': repository, 'environment': environment}:
        return _check('failed', 'scope-mismatch', mode=mode, detail='Installed authority scope differs')
    return _check('passed', 'ok', mode=mode, repository=scope['repository'], environment=scope['environment'])


def check_immutable_artifacts(get_list, lock):
    deployments = {d['metadata']['name']: d for d in get_list('deployments', 'app.kubernetes.io/instance=ai-sdlc')}
    expected = {'ai-sdlc-' + install.COMPONENT_NAMES[key]: lock['image']['repository'] + '@' + value['digest']
                for key, value in lock['components'].items()}
    actual = {}
    mismatched = []
    for name, image in expected.items():
        containers = (deployments.get(name) or {}).get('spec', {}).get('template', {}).get('spec', {}).get('containers', [])
        actual[name] = containers[0]['image'] if containers else None
        if actual[name] != image:
            mismatched.append(name)
    if mismatched:
        return _check('failed', 'image-mismatch', mismatched=sorted(mismatched))
    return _check('passed', 'ok', pinnedComponents=len(expected))


def check_migrations(query, expected_count):
    observed = query(migrations_sql())
    if observed is None:
        return _check('unavailable', 'observation-denied', detail='schema_migrations is not observable')
    data = json.loads(observed) if isinstance(observed, str) else observed
    applied = int(data.get('applied') or 0)
    if applied != expected_count:
        return _check('failed', 'migrations-incomplete', applied=applied, expected=expected_count)
    return _check('passed', 'ok', applied=applied)


def check_readiness(get_list):
    deployments = get_list('deployments', 'app.kubernetes.io/instance=ai-sdlc')
    expected = {'ai-sdlc-' + name for name in install.COMPONENT_NAMES.values()}
    found = {d['metadata']['name'] for d in deployments}
    missing = sorted(expected - found)
    not_ready = sorted(d['metadata']['name'] for d in deployments
                       if d['metadata']['name'] in expected
                       and d.get('status', {}).get('readyReplicas') != 1)
    postgres_ready = _postgres_ready(get_list)
    if missing:
        return _check('failed', 'not-installed', missing=missing)
    if not_ready or not postgres_ready:
        return _check('failed', 'not-ready', notReady=not_ready, postgresReady=postgres_ready)
    return _check('passed', 'ok', readyComponents=len(expected), postgresReady=True)


def _postgres_ready(get_list):
    # Local installs run a postgres StatefulSet; managed deployments run
    # CloudNativePG pods. Both surface only as bounded readiness evidence.
    statefulsets = get_list('statefulsets', 'app.kubernetes.io/instance=ai-sdlc')
    for item in statefulsets:
        if item['metadata']['name'] == 'ai-sdlc-postgres':
            return item.get('status', {}).get('readyReplicas') == 1
    pods = get_list('pods', 'app.kubernetes.io/instance=ai-sdlc')
    postgres = [p for p in pods if p['metadata']['name'].startswith('ai-sdlc-postgres')]
    return bool(postgres) and all(p.get('status', {}).get('phase') == 'Running' for p in postgres[:4])


def check_app_scopes(present, mode):
    github = present(GITHUB_CLIENT_SECRET)
    needs_intake = mode in ('shadow', 'candidate', 'verification')
    detail = {'githubClient': github}
    if needs_intake and not github:
        return _check('failed', 'credentials-missing', **detail)
    return _check('passed', 'ok', **detail)


def check_intake(query, repository, mode):
    if mode not in ('shadow', 'candidate', 'verification'):
        return _check('unavailable', 'unavailable', detail='Issue intake is not enabled in this mode')
    observed = query(intake_sql(repository))
    if observed is None:
        return _check('unavailable', 'observation-denied', detail='Intake journal is not observable')
    data = json.loads(observed) if isinstance(observed, str) else observed
    lag = data.get('lagSeconds')
    failures = data.get('failures') or 0
    if not data.get('cursors'):
        return _check('failed', 'connectivity-degraded', detail='No intake cursor exists for the repository',
                      intakeLagSeconds=lag, intakeFailures=failures)
    if failures:
        return _check('failed', 'connectivity-degraded',
                      intakeLagSeconds=lag, intakeFailures=failures,
                      detail='Intake poll failures are recorded')
    return _check('passed', 'ok', intakeLagSeconds=lag, intakeFailures=failures,
                  observedRevisions=data.get('revisions'))


def check_model(present, query, mode):
    if mode not in ('candidate', 'verification'):
        return _check('unavailable', 'unavailable', detail='Model access is not enabled in this mode')
    secrets = {name: present(name) for name in MODEL_SECRETS}
    if not any(secrets.values()):
        return _check('failed', 'credentials-missing', detail='No model credential Secret is present')
    observed = query(model_sql())
    if observed is None:
        return _check('unavailable', 'observation-denied', credentialSecrets=secrets,
                      detail='Credential present; model call journal is not observable')
    data = json.loads(observed) if isinstance(observed, str) else observed
    statuses = data.get('statuses') or {}
    if statuses.get('429'):
        return _check('failed', 'rate-limited', credentialSecrets=secrets, responseStatuses=statuses)
    if statuses.get('401') or statuses.get('403'):
        return _check('failed', 'authority-denied', credentialSecrets=secrets, responseStatuses=statuses)
    if any(statuses.get(code) for code in ('500', '502', '503', '504')):
        return _check('failed', 'connectivity-degraded', credentialSecrets=secrets, responseStatuses=statuses)
    return _check('passed', 'ok', credentialSecrets=secrets, responseStatuses=statuses,
                  observedCalls=data.get('calls'))


def inspect(get, get_list, present, query, lock, migration_count, repository, environment):
    """Run every read-only check and return the bounded JSON report."""
    config = get('configmap', 'ai-sdlc-config') or {}
    mode = config.get('data', {}).get('AI_SDLC_RUNTIME_MODE', '')
    checks = {
        'configuration': check_configuration(get, repository, environment),
        'immutable-artifacts': check_immutable_artifacts(get_list, lock),
        'database-migrations': check_migrations(query, migration_count),
        'component-readiness': check_readiness(get_list),
        'app-scopes': check_app_scopes(present, mode),
        'intake-connectivity': check_intake(query, repository, mode),
        'model-connectivity': check_model(present, query, mode)}
    failed = [name for name, result in checks.items() if result['status'] == 'failed']
    return {'checks': checks, 'healthy': not failed, 'failedChecks': sorted(failed),
            'deliveryStagesAdvanced': 0, 'arbitraryCommandCapability': False}


def kubectl_base(cluster, context=None):
    return issue_status.kubectl_base(cluster, context)


def readers(kubectl, timeout):
    """Read-only Kubernetes/PostgreSQL readers bound to a kubectl base.

    Only `get` subcommands and a single fixed `exec ... psql` read-only
    transaction exist here; Secret data is never requested (name probes only).
    """
    def get(kind, name):
        result = subprocess.run(kubectl + ['get', kind, name, '--ignore-not-found', '-o', 'json'],
                                text=True, capture_output=True, timeout=timeout)
        if result.returncode or not result.stdout.strip():
            return None
        return json.loads(result.stdout)

    def get_list(kind, selector):
        result = subprocess.run(kubectl + ['get', kind, '-l', selector, '-o', 'json'],
                                text=True, capture_output=True, timeout=timeout)
        if result.returncode:
            return []
        return json.loads(result.stdout).get('items', [])

    def present(name):
        result = subprocess.run(kubectl + ['get', 'secret', name, '--ignore-not-found', '-o', 'name'],
                                text=True, capture_output=True, timeout=timeout)
        return result.returncode == 0 and bool(result.stdout.strip())

    def query(sql):
        return issue_status.query_read_only(kubectl, read_only_sql(sql), timeout=timeout)

    return get, get_list, present, query


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', default='ai-sdlc')
    parser.add_argument('--provider', choices=['docker', 'podman'], required=True)
    parser.add_argument('--context', help='Kubeconfig context; defaults to kind-<cluster>')
    parser.add_argument('--repository', default='os-santiago/ai-sdlc')
    parser.add_argument('--environment', default='ai-sdlc-production')
    parser.add_argument('--timeout', type=int, default=20)
    args = parser.parse_args(argv)
    validate_repository(args.repository)
    if not 1 <= args.timeout <= 120 or not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,62}', args.environment):
        raise ValueError('Invalid doctor bounds')
    install.main(['status', '--cluster', args.cluster, '--provider', args.provider])
    kubectl = kubectl_base(args.cluster, args.context)
    lock = install.image_lock(ROOT / 'runtime/k8s/deploy/local/images.lock.json')
    migration_count = len(list(MIGRATIONS.glob('*.sql')))
    report = inspect(*readers(kubectl, args.timeout),
                     lock, migration_count, args.repository, args.environment)
    report.update(cluster=args.cluster, repository=args.repository, environment=args.environment)
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if report['healthy'] else 1


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print('Doctor diagnostics failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
