import importlib.util
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import doctor
import issue_status

spec = importlib.util.spec_from_file_location('installer', Path(__file__).with_name('install.py'))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)

REPOSITORY = 'owner/repo'
ENVIRONMENT = 'ai-sdlc-production'
SECRET_SENTINEL = 'ghp_LIVE_CREDENTIAL_VALUE'


def lock():
    digest = 'sha256:' + 'a' * 64
    return {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'},
            'components': {key: {'digest': digest} for key in installer.COMPONENT_NAMES},
            'migrator': {'digest': digest}}


def deployment(name, ready=1):
    image = 'quay.io/os-santiago/ai-sdlc@' + 'sha256:' + 'a' * 64
    return {'metadata': {'name': name},
            'spec': {'template': {'spec': {'containers': [{'image': image}]}}},
            'status': {'readyReplicas': ready}}


def configmap(mode, repository=REPOSITORY, environment=ENVIRONMENT):
    return {'data': {'AI_SDLC_RUNTIME_MODE': mode,
                     'policy.json': json.dumps({'repository': repository,
                                                'environment': environment})}}


def migration_count():
    return len(list(doctor.MIGRATIONS.glob('*.sql')))


class FakeCluster:
    """Canned read-only cluster; records every kubectl argv and psql stdin."""

    MUTATING = {'apply', 'create', 'replace', 'delete', 'patch', 'edit', 'scale',
                'rollout', 'annotate', 'label', 'taint', 'cordon', 'uncordon',
                'drain', 'set', 'run', 'expose', 'autoscale'}

    def __init__(self, mode='maintenance', deployments=None, statefulsets=None,
                 secrets=None, sql_results=None, missing_config=False):
        self.mode = mode
        self.deployments = deployments
        self.statefulsets = statefulsets
        self.secrets = secrets if secrets is not None else {'ai-sdlc-github-client'}
        self.sql_results = sql_results or {}
        self.missing_config = missing_config
        self.calls = []
        self.stdin = []

    def __call__(self, argv, **kwargs):
        argv = [str(a) for a in argv]
        self.calls.append(argv)
        verb = argv[1] if argv[0] == 'kubectl' else argv[0]
        self.assert_read_only(verb, argv)
        if 'input' in kwargs:
            self.stdin.append(kwargs['input'])
        return self.respond(argv)

    @staticmethod
    def assert_read_only(verb, argv):
        assert verb in ('get', 'exec'), f'mutating kubectl verb: {argv}'
        assert verb not in FakeCluster.MUTATING
        if verb == 'exec':
            assert argv[2:] == ['-i', 'ai-sdlc-postgres-0', '--', 'psql', '-U', 'ai_sdlc',
                                '-d', 'ai_sdlc', '-XAtq', '-v', 'ON_ERROR_STOP=1'], argv

    def respond(self, argv):
        if argv[1] == 'exec':
            for marker, result in self.sql_results.items():
                if marker in self.stdin[-1]:
                    return self.done(result) if result is not None else self.failed()
            return self.done('{"applied": %d}' % migration_count())
        kind, name = argv[2], argv[3] if len(argv) > 3 else None
        if kind == 'secret':
            if name in self.secrets:
                return self.done('secret/' + name + '\n')
            return self.done('')
        if argv[-1] == 'json':
            resource = self.resource(argv)
            if resource is None:
                return self.done('')
            return self.done(json.dumps(resource))
        return self.done('')

    def resource(self, argv):
        kind = argv[2]
        if '-l' in argv:
            items = {'deployments': self.deployments, 'statefulsets': self.statefulsets,
                     'pods': []}
            return {'items': items.get(kind) or []}
        if kind == 'configmap' and argv[3] == 'ai-sdlc-config':
            return None if self.missing_config else configmap(self.mode)
        if kind == 'configmap' and argv[3] == 'ai-sdlc-authority':
            return configmap(self.mode)
        return None

    @staticmethod
    def done(stdout):
        return subprocess.CompletedProcess([], 0, stdout=stdout, stderr='')

    @staticmethod
    def failed():
        return subprocess.CompletedProcess([], 1, stdout='', stderr='permission denied')


def healthy_deployments():
    return [deployment('ai-sdlc-' + name) for name in installer.COMPONENT_NAMES.values()]


import contextlib


@contextlib.contextmanager
def patched_readers(cluster):
    with patch.object(subprocess, 'run', side_effect=cluster):
        yield doctor.readers(['kubectl'], 5)


def run_inspect(cluster, mode='maintenance'):
    cluster.mode = mode
    with patched_readers(cluster) as readers:
        return doctor.inspect(*readers, lock(), migration_count(), REPOSITORY, ENVIRONMENT)


class DelegationTests(unittest.TestCase):
    def test_install_delegates_doctor_and_issue_status(self):
        for command in ('doctor', 'issue-status'):
            with self.assertRaises(SystemExit) as raised:
                installer.main([command, '--help'])
            self.assertEqual(raised.exception.code, 0, command)

    def test_issue_option_is_bounded_and_status_only(self):
        with self.assertRaises(ValueError):
            installer.main(['status', '--issue', '0'])
        with self.assertRaises(ValueError):
            installer.main(['status', '--issue', str(2**31)])
        with self.assertRaises(ValueError):
            installer.main(['host', '--issue', '3'])
        with self.assertRaises(ValueError):
            installer.main(['credentials', '--issue', '3'])


class DoctorBoundaryTests(unittest.TestCase):
    def test_every_sql_body_is_a_read_only_transaction(self):
        for sql in (doctor.intake_sql(REPOSITORY), doctor.migrations_sql(), doctor.model_sql()):
            self.assertTrue(sql.lstrip().startswith('BEGIN READ ONLY'))
            self.assertIn('COMMIT;', sql)

    def test_injection_repository_is_rejected(self):
        for repository in ("owner/repo'; DROP TABLE plans;--", '../repo', 'owner/../x'):
            with self.assertRaises(ValueError):
                doctor.intake_sql(repository)
        with self.assertRaises(ValueError):
            doctor.read_only_sql('DELETE FROM schema_migrations;')

    def test_healthy_installation_uses_only_get_and_read_only_exec(self):
        cluster = FakeCluster(deployments=healthy_deployments(),
                              statefulsets=[{'metadata': {'name': 'ai-sdlc-postgres'},
                                             'status': {'readyReplicas': 1}}],
                              sql_results={'intake_cursors': '{"cursors":0,"failures":0,"revisions":0}',
                                           'model_calls': '{"calls":0,"states":{},"statuses":{}}'})
        report = run_inspect(cluster)
        self.assertTrue(cluster.calls)
        for argv in cluster.calls:
            self.assertEqual(argv[0], 'kubectl')
            self.assertIn(argv[1], ('get', 'exec'), argv)
        for sql in cluster.stdin:
            self.assertTrue(sql.lstrip().startswith('BEGIN READ ONLY'), sql)
        self.assertFalse(report['failedChecks'])
        self.assertTrue(report['healthy'])
        self.assertEqual(report['deliveryStagesAdvanced'], 0)
        self.assertFalse(report['arbitraryCommandCapability'])

    def test_secret_values_are_never_requested_or_reported(self):
        cluster = FakeCluster(deployments=healthy_deployments(),
                              statefulsets=[{'metadata': {'name': 'ai-sdlc-postgres'},
                                             'status': {'readyReplicas': 1}}])
        report = run_inspect(cluster)
        for argv in cluster.calls:
            if 'secret' in argv:
                # Secret probes resolve names only; data/stringData is never read.
                self.assertEqual(argv[-2:], ['-o', 'name'], argv)
        self.assertNotIn(SECRET_SENTINEL, json.dumps(report))
        self.assertNotIn('stringData', json.dumps(report))

    def test_reason_codes_stay_bounded_across_failures(self):
        cluster = FakeCluster(mode='shadow', deployments=[], statefulsets=[],
                              secrets=set(), missing_config=True,
                              sql_results={'intake_cursors': None, 'model_calls': None})
        report = run_inspect(cluster, mode='shadow')
        self.assertFalse(report['healthy'])
        self.assertIn('configuration', report['failedChecks'])
        self.assertIn('immutable-artifacts', report['failedChecks'])
        for name, check in report['checks'].items():
            self.assertIn(check['status'], ('passed', 'failed', 'unavailable'), name)
            self.assertIn(check['reasonCode'], doctor.REASON_CODES, name)

    def test_missing_credentials_and_model_failures_have_stable_codes(self):
        cluster = FakeCluster(mode='candidate', deployments=healthy_deployments(),
                              statefulsets=[{'metadata': {'name': 'ai-sdlc-postgres'},
                                             'status': {'readyReplicas': 1}}],
                              secrets=set(),
                              sql_results={'intake_cursors': '{"cursors":1,"failures":0,"lagSeconds":5}',
                                           'model_calls': '{"calls":2,"states":{"completed":2},"statuses":{"429":2}}'})
        report = run_inspect(cluster, mode='candidate')
        self.assertEqual(report['checks']['app-scopes']['reasonCode'], 'credentials-missing')
        self.assertEqual(report['checks']['model-connectivity']['reasonCode'], 'credentials-missing')
        cluster.secrets = {'ai-sdlc-codex-auth', 'ai-sdlc-model-client'}
        report = run_inspect(cluster, mode='candidate')
        self.assertEqual(report['checks']['app-scopes']['reasonCode'], 'credentials-missing')
        self.assertEqual(report['checks']['model-connectivity']['reasonCode'], 'rate-limited')

    def test_intake_cursor_failures_report_connectivity_degraded(self):
        cluster = FakeCluster(mode='shadow', deployments=healthy_deployments(),
                              statefulsets=[{'metadata': {'name': 'ai-sdlc-postgres'},
                                             'status': {'readyReplicas': 1}}],
                              sql_results={'intake_cursors': '{"cursors":1,"failures":3,"lagSeconds":900}'})
        report = run_inspect(cluster, mode='shadow')
        check = report['checks']['intake-connectivity']
        self.assertEqual(check['status'], 'failed')
        self.assertEqual(check['reasonCode'], 'connectivity-degraded')
        self.assertEqual(check['intakeFailures'], 3)

    def test_denied_journals_degrade_to_observation_denied(self):
        cluster = FakeCluster(deployments=healthy_deployments(),
                              statefulsets=[{'metadata': {'name': 'ai-sdlc-postgres'},
                                             'status': {'readyReplicas': 1}}],
                              sql_results={'intake_cursors': None,
                                           'model_calls': None,
                                           'schema_migrations': None})
        report = run_inspect(cluster)
        self.assertEqual(report['checks']['database-migrations']['reasonCode'], 'observation-denied')
        self.assertTrue(report['healthy'])

    def test_status_issue_uses_the_same_bounded_observer(self):
        import tempfile
        import os
        with tempfile.TemporaryDirectory() as state_home:
            env = {'XDG_STATE_HOME': state_home, 'XDG_CACHE_HOME': state_home}
            with patch.dict(os.environ, env), \
                 patch.object(issue_status, 'observe', return_value={'issue': 7}) as observe, \
                 patch.object(installer, 'run') as command, \
                 patch.object(installer, 'read_resource', return_value=None), \
                 patch.object(installer, 'tools',
                              return_value={'kubectl': 'kubectl', 'helm': 'helm', 'kind': 'kind'}), \
                 patch.object(installer, 'check_ownership'), \
                 patch.object(installer.openshift_install, 'refuse_other_platform'), \
                 patch('shutil.which', return_value='/usr/bin/docker'), \
                 patch('pathlib.Path.chmod'), \
                 patch('pathlib.Path.exists', return_value=True):
                command.side_effect = lambda argv, **kw: subprocess.CompletedProcess(
                    argv, 0, stdout='ai-sdlc\n' if 'clusters' in argv else 'uid\n', stderr='')
                installer._main(['status', '--provider', 'docker', '--issue', '7',
                                 '--repository', REPOSITORY], None)
        self.assertTrue(observe.called)
        repository, issue = observe.call_args[0][1:]
        self.assertEqual((repository, issue), (REPOSITORY, 7))


if __name__ == '__main__':
    unittest.main()
