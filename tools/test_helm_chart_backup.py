"""The opt-in scheduled database backup: rendered chart objects and the backup script.

The chart tests render the chart with the pinned Helm. The script tests run
files/backup.sh under /bin/sh with stub pg_dump and pg_restore commands, so
they need no database, container runtime or cluster.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import time
import unittest

import yaml

import test_helm
import test_release_helm
from test_helm import CHART, HELM

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = CHART / 'files/backup.sh'
RUNBOOK = ROOT / 'docs/operations/backup-and-restore.md'
PINNED = 'registry.example/postgres@sha256:' + 'c' * 64
FAKE_PASSWORD = 'fixture-Password_0123456789abcdef'
BACKUP_NAME = re.compile(r'ai-sdlc-[0-9]{8}T[0-9]{6}Z\.dump\Z')


def docs(result):
    return [d for d in yaml.safe_load_all(result.stdout) if d]


def find(documents, kind, name):
    matches = [d for d in documents if d['kind'] == kind and d['metadata']['name'] == name]
    return matches[0] if len(matches) == 1 else None


def backup_pod(documents):
    cronjob = find(documents, 'CronJob', 'ai-sdlc-backup')
    return cronjob['spec']['jobTemplate']['spec']['template']['spec']


class BackupChartTests(unittest.TestCase):
    chart = test_helm.ChartTests()

    def render(self, values, *args):
        result = self.chart.render_build(values, *args)
        self.assertEqual(result.returncode, 0, result.stderr)
        return docs(result)

    def test_backup_is_disabled_by_default_and_renders_nothing(self):
        defaults = subprocess.run([HELM, 'template', 'ai-sdlc', str(CHART), '--namespace', 'ai-sdlc-runtime'],
                                  capture_output=True, text=True)
        self.assertEqual(defaults.returncode, 0, defaults.stderr)
        local = self.chart.render()
        self.assertEqual(local.returncode, 0, local.stderr)
        for result in (defaults, local):
            self.assertNotIn('backup', result.stdout)
            self.assertNotIn('CronJob', [d['kind'] for d in docs(result)])
        # An explicit false, even with other backup settings, renders the same bytes.
        off = self.chart.render_build({'backup': {'enabled': False, 'retention': 3,
                                                  'storage': {'existingClaim': 'x', 'size': '1Gi', 'storageClass': ''}}})
        self.assertEqual(off.stdout, local.stdout)
        internal = find(docs(local), 'NetworkPolicy', 'ai-sdlc-internal')
        self.assertNotIn('matchExpressions', internal['spec']['podSelector'])

    def test_enabled_renders_the_cronjob_volume_and_network_policy(self):
        documents = self.render({'backup': {'enabled': True}})
        cronjob = find(documents, 'CronJob', 'ai-sdlc-backup')
        spec = cronjob['spec']
        self.assertEqual((spec['schedule'], spec['timeZone'], spec['concurrencyPolicy'], spec['startingDeadlineSeconds']),
                         ('17 2 * * *', 'Etc/UTC', 'Forbid', 600))
        job = spec['jobTemplate']['spec']
        self.assertEqual((job['backoffLimit'], job['activeDeadlineSeconds']), (1, 3600))
        pod = job['template']['spec']
        self.assertEqual(pod['restartPolicy'], 'Never')
        container, = pod['containers']
        # The bundled server's image, so pg_dump matches its major version.
        values = yaml.safe_load((CHART / 'values.yaml').read_text())
        self.assertEqual(container['image'], values['postgres']['local']['image'])
        self.assertEqual(container['command'], ['/bin/sh', '-c'])
        self.assertEqual(container['args'], [SCRIPT.read_text()])
        env = {e['name']: e for e in container['env']}
        self.assertEqual(env['BACKUP_RETENTION']['value'], '7')
        # No run outlives the Job deadline, so an older volume lock is stale.
        self.assertEqual(env['BACKUP_LOCK_STALE_SECONDS']['value'], '3600')
        self.assertEqual(env['BACKUP_DIR']['value'], '/backup')
        self.assertEqual(container['resources'], values['backup']['resources'])
        self.assertEqual({m['mountPath']: m['name'] for m in container['volumeMounts']}, {'/backup': 'backup', '/tmp': 'tmp'})
        volumes = {v['name']: v for v in pod['volumes']}
        self.assertEqual(volumes['backup']['persistentVolumeClaim'], {'claimName': 'ai-sdlc-backup'})
        self.assertIn('emptyDir', volumes['tmp'])
        claim = find(documents, 'PersistentVolumeClaim', 'ai-sdlc-backup')
        self.assertEqual(claim['spec']['resources']['requests']['storage'], '5Gi')
        self.assertNotIn('storageClassName', claim['spec'])
        self.assertEqual(claim['metadata']['annotations'], {'helm.sh/resource-policy': 'keep'})
        # The backup pod accepts nothing and reaches only DNS and the bundled PostgreSQL pod.
        policy = find(documents, 'NetworkPolicy', 'ai-sdlc-backup')
        self.assertEqual(policy['spec']['podSelector']['matchLabels']['app.kubernetes.io/component'], 'backup')
        self.assertEqual(policy['spec']['policyTypes'], ['Ingress', 'Egress'])
        self.assertEqual(policy['spec']['ingress'], [])
        dns, database = policy['spec']['egress']
        self.assertEqual(dns['to'], [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'kube-system'}}}])
        self.assertEqual(database, {'to': [{'podSelector': {'matchLabels': {
            'app.kubernetes.io/name': 'ai-sdlc-k8s-runtime', 'app.kubernetes.io/instance': 'ai-sdlc',
            'app.kubernetes.io/component': 'postgres'}}}], 'ports': [{'protocol': 'TCP', 'port': 5432}]})
        # The pod carries the chart's selector labels, which the PostgreSQL pod
        # admits, but the internal policy excludes it so it gains no other peer.
        labels = job['template']['metadata']['labels']
        self.assertEqual(labels['app.kubernetes.io/instance'], 'ai-sdlc')
        internal = find(documents, 'NetworkPolicy', 'ai-sdlc-internal')
        self.assertEqual(internal['spec']['podSelector']['matchExpressions'],
                         [{'key': 'app.kubernetes.io/component', 'operator': 'NotIn', 'values': ['backup']}])
        # Without the chart's network policies, the backup adds none.
        plain = self.render({'backup': {'enabled': True}, 'networkPolicy': {'enabled': False, 'databaseEgressCIDRs': []}})
        self.assertEqual([d for d in plain if d['kind'] == 'NetworkPolicy'], [])

    def test_internal_policy_keeps_every_existing_exclusion(self):
        values = self.chart.build_values()
        values['backup'] = {'enabled': True}
        internal = find(self.render(values), 'NetworkPolicy', 'ai-sdlc-internal')
        excluded = [e['values'] for e in internal['spec']['podSelector']['matchExpressions']]
        self.assertEqual(excluded, [['build-controller'], ['backup']])
        without = find(self.render(self.chart.build_values()), 'NetworkPolicy', 'ai-sdlc-internal')
        self.assertEqual([e['values'] for e in without['spec']['podSelector']['matchExpressions']], [['build-controller']])

    def test_only_the_backup_policy_selects_the_backup_pod(self):
        """NetworkPolicies are additive: any other policy selecting the pod would widen its egress."""
        values = test_release_helm.ReleaseChartTests().values()
        values['backup'] = {'enabled': True}
        values['metrics'] = {'enabled': True, 'scrapeFrom': [{'namespaceSelector': {'matchLabels': {'name': 'monitoring'}}}]}
        documents = self.render(values)
        cronjob = find(documents, 'CronJob', 'ai-sdlc-backup')
        labels = cronjob['spec']['jobTemplate']['spec']['template']['metadata']['labels']

        def selects(selector):
            if any(labels.get(key) != value for key, value in (selector.get('matchLabels') or {}).items()):
                return False
            for expression in selector.get('matchExpressions') or []:
                value, operator = labels.get(expression['key']), expression['operator']
                if (operator == 'In' and value not in expression['values']) or (operator == 'NotIn' and value in expression['values']) \
                        or (operator == 'Exists' and value is None) or (operator == 'DoesNotExist' and value is not None):
                    return False
            return True
        selecting = [d['metadata']['name'] for d in documents if d['kind'] == 'NetworkPolicy'
                     and d['metadata']['namespace'] == cronjob['metadata']['namespace'] and selects(d['spec']['podSelector'])]
        self.assertEqual(selecting, ['ai-sdlc-backup'])

    def test_no_secret_value_is_inlined(self):
        for values in ({'backup': {'enabled': True}},
                       {'backup': {'enabled': True}, 'postgres': {'local': {'enabled': False}, 'cloudNativePG': {'enabled': True}}}):
            documents = self.render(values)
            pod = backup_pod(documents)
            container, = pod['containers']
            for item in container['env']:
                if item['name'] == 'DATABASE_URL':
                    self.assertEqual(set(item), {'name', 'valueFrom'})
                    self.assertEqual(set(item['valueFrom']), {'secretKeyRef'})
                else:
                    self.assertNotRegex(item['value'], r'postgres(ql)?://|password', item['name'])
            self.assertNotIn('envFrom', container)
            text = json.dumps(pod)
            self.assertNotRegex(text, r'postgres(ql)?://[^"\s]*:[^"\s]*@')
            self.assertNotIn('PGPASSWORD=', ' '.join(container['command']))
            self.assertFalse([d for d in documents if d['kind'] == 'Secret'])
        # CloudNativePG: the operator's generated <cluster>-app Secret and its uri key.
        cnpg = backup_pod(self.render({'backup': {'enabled': True},
                                       'postgres': {'local': {'enabled': False}, 'cloudNativePG': {'enabled': True}}}))
        ref = next(e for e in cnpg['containers'][0]['env'] if e['name'] == 'DATABASE_URL')['valueFrom']['secretKeyRef']
        self.assertEqual(ref, {'name': 'ai-sdlc-postgres-app', 'key': 'uri'})

    def test_restricted_security_context(self):
        pod = backup_pod(self.render({'backup': {'enabled': True}}))
        self.assertFalse(pod['automountServiceAccountToken'])
        self.assertFalse(pod['enableServiceLinks'])
        self.assertNotIn('serviceAccountName', pod)
        self.assertEqual(pod['securityContext'], {'runAsNonRoot': True, 'runAsUser': 70, 'runAsGroup': 70, 'fsGroup': 70,
                                                  'seccompProfile': {'type': 'RuntimeDefault'}})
        container, = pod['containers']
        self.assertEqual(container['securityContext'], {'allowPrivilegeEscalation': False, 'capabilities': {'drop': ['ALL']},
                                                        'readOnlyRootFilesystem': True})
        self.assertNotIn('hostNetwork', pod)
        # OpenShift restricted-v2 assigns the UID and fsGroup: the chart sets none.
        documents = self.render({'backup': {'enabled': True}, 'openshift': {'enabled': True, 'route': {'enabled': False, 'host': ''}}})
        openshift = backup_pod(documents)
        self.assertEqual(openshift['securityContext'], {'runAsNonRoot': True, 'seccompProfile': {'type': 'RuntimeDefault'}})
        self.assertEqual(openshift['containers'][0]['securityContext'], container['securityContext'])
        dns = find(documents, 'NetworkPolicy', 'ai-sdlc-backup')['spec']['egress'][0]
        self.assertEqual(dns['ports'], [{'protocol': 'UDP', 'port': 5353}, {'protocol': 'TCP', 'port': 5353}])

    def test_external_postgres_target(self):
        external = {'local': {'enabled': False}, 'cloudNativePG': {'enabled': False},
                    'databaseURLSecret': {'name': 'external-db-owner', 'key': 'url'}}
        documents = self.render({'backup': {'enabled': True}, 'postgres': external,
                                 'networkPolicy': {'enabled': True, 'databaseEgressCIDRs': ['10.20.0.0/24']}})
        pod = backup_pod(documents)
        ref = next(e for e in pod['containers'][0]['env'] if e['name'] == 'DATABASE_URL')['valueFrom']['secretKeyRef']
        self.assertEqual(ref, {'name': 'external-db-owner', 'key': 'url'})
        # The same Secret the migrator uses.
        migrator = next(d for d in documents if d['kind'] == 'Job' and d['metadata']['name'].startswith('ai-sdlc-migrate'))
        migrator_ref = next(e for e in migrator['spec']['template']['spec']['containers'][0]['env']
                            if e['name'] == 'DATABASE_URL')['valueFrom']['secretKeyRef']
        self.assertEqual(ref, migrator_ref)
        egress = find(documents, 'NetworkPolicy', 'ai-sdlc-backup')['spec']['egress']
        self.assertEqual(egress[1:], [{'to': [{'ipBlock': {'cidr': '10.20.0.0/24'}}], 'ports': [{'protocol': 'TCP', 'port': 5432}]}])
        self.assertNotIn('podSelector', json.dumps(egress))

    def test_storage_image_and_schedule_settings(self):
        documents = self.render({'backup': {'enabled': True, 'schedule': '0 */6 * * *', 'timeZone': '', 'retention': 30,
                                            'image': PINNED, 'storage': {'existingClaim': 'site-backups', 'size': '1Gi', 'storageClass': ''},
                                            'startingDeadlineSeconds': 120, 'backoffLimit': 0, 'activeDeadlineSeconds': 900}})
        self.assertFalse([d for d in documents if d['kind'] == 'PersistentVolumeClaim' and 'backup' in d['metadata']['name']])
        cronjob = find(documents, 'CronJob', 'ai-sdlc-backup')
        self.assertEqual(cronjob['spec']['schedule'], '0 */6 * * *')
        self.assertNotIn('timeZone', cronjob['spec'])
        self.assertEqual(cronjob['spec']['startingDeadlineSeconds'], 120)
        job = cronjob['spec']['jobTemplate']['spec']
        self.assertEqual((job['backoffLimit'], job['activeDeadlineSeconds']), (0, 900))
        pod = job['template']['spec']
        self.assertEqual(pod['containers'][0]['image'], PINNED)
        self.assertEqual(next(v for v in pod['volumes'] if v['name'] == 'backup')['persistentVolumeClaim'], {'claimName': 'site-backups'})
        self.assertEqual(next(e for e in pod['containers'][0]['env'] if e['name'] == 'BACKUP_RETENTION')['value'], '30')
        created = self.render({'backup': {'enabled': True, 'storage': {'existingClaim': '', 'size': '20Gi', 'storageClass': 'fast'}}})
        claim = find(created, 'PersistentVolumeClaim', 'ai-sdlc-backup')
        self.assertEqual((claim['spec']['storageClassName'], claim['spec']['resources']['requests']['storage']), ('fast', '20Gi'))

    def test_invalid_settings_are_refused(self):
        unpinned = self.chart.render_build({'backup': {'enabled': True, 'image': 'docker.io/library/postgres:17-alpine'}})
        self.assertNotEqual(unpinned.returncode, 0)
        local_unpinned = self.chart.render_build({'backup': {'enabled': True},
                                                  'postgres': {'local': {'image': 'docker.io/library/postgres:17-alpine'}}})
        self.assertNotEqual(local_unpinned.returncode, 0)
        self.assertIn('must be pinned by digest', local_unpinned.stderr)
        for bad in ({'retention': 0}, {'retention': '7'}, {'schedule': 'daily'}, {'backoffLimit': -1},
                    {'enabled': 'yes'}, {'password': 'x'}, {'storage': {'existingClaim': '', 'size': '1Gi', 'storageClass': '', 'x': 1}}):
            with self.subTest(bad=bad):
                self.assertNotEqual(self.chart.render_build({'backup': dict({'enabled': True}, **bad)}).returncode, 0)


STUB_PG_DUMP = r'''#!/bin/sh
# Records what the backup script passes, then writes a fake archive. A
# complete fake archive starts with PGDMP and ends with an END line.
printf '%s\n' "$@" > "$STUB_LOG.argv"
printf '%s' "${PGPASSWORD-<unset>}" > "$STUB_LOG.password"
[ -z "${STUB_DUMP_FAIL:-}" ] || exit 1
[ -z "${STUB_SLEEP:-}" ] || sleep "$STUB_SLEEP"
for arg in "$@"; do
  case "$arg" in --file=*) file="${arg#--file=}" ;; esac
done
if [ -n "${STUB_TRUNCATE:-}" ]; then
  printf 'PGDMP fake archive\n' > "$file"
else
  printf 'PGDMP fake archive\nEND\n' > "$file"
fi
'''

STUB_PG_RESTORE = r'''#!/bin/sh
# --list reads only the header, like the real table of contents at the start
# of a custom-format archive. --file=/dev/null reads to the end.
case "$1" in
  --list)
    [ -z "${STUB_RESTORE_FAIL:-}" ] || exit 1
    head -c 5 "$2" | grep -q PGDMP
    exit
    ;;
  --file=/dev/null)
    [ -z "${STUB_RESTORE_FAIL:-}" ] || exit 1
    head -c 5 "$2" | grep -q PGDMP && [ "$(tail -n 1 "$2")" = END ]
    exit
    ;;
esac
printf '%s\n' "$@" > "$STUB_LOG.restore"
printf '%s' "${PGPASSWORD-<unset>}" > "$STUB_LOG.restore-password"
[ -z "${STUB_APPLY_FAIL:-}" ]
'''

STUB_PSQL = r'''#!/bin/sh
printf '%s\n' "$@" > "$STUB_LOG.psql"
[ -z "${STUB_PSQL_FAIL:-}" ] || exit 2
echo "${STUB_SESSIONS:-0}"
'''

COMPLETE = 'PGDMP fake archive\nEND\n'
LOCK = '.ai-sdlc-backup.lock'


class BackupScriptTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        base = Path(self.temporary.name)
        self.bin = base / 'bin'
        self.dir = base / 'backup'
        self.log = base / 'stub'
        self.bin.mkdir()
        self.dir.mkdir()
        for name, body in (('pg_dump', STUB_PG_DUMP), ('pg_restore', STUB_PG_RESTORE), ('psql', STUB_PSQL)):
            path = self.bin / name
            path.write_text(body)
            path.chmod(path.stat().st_mode | stat.S_IXUSR)

    def tearDown(self):
        self.temporary.cleanup()

    def environment(self, url, retention, extra):
        env = {'PATH': '%s:%s' % (self.bin, os.environ.get('PATH', '/usr/bin:/bin')), 'BACKUP_DIR': str(self.dir),
               'BACKUP_RETENTION': retention, 'DATABASE_URL': url, 'STUB_LOG': str(self.log)}
        env.update(extra)
        return env

    def stub_log(self, suffix):
        path = Path(str(self.log) + suffix)
        return path.read_text() if path.exists() else None

    def assert_no_secret_leaked(self, result, secrets):
        """No secret in the output, or in any command line a stub received."""
        argv = '\n'.join(self.stub_log(s) or '' for s in ('.argv', '.restore', '.psql'))
        for secret in secrets:
            self.assertNotIn(secret, result.stdout + result.stderr)
            self.assertNotIn(secret, argv)

    def run_backup(self, url='postgresql://ai_sdlc:%s@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable' % FAKE_PASSWORD,
                   retention='3', secrets=(FAKE_PASSWORD,), **extra):
        result = subprocess.run(['/bin/sh', '-c', SCRIPT.read_text()], env=self.environment(url, retention, extra),
                                capture_output=True, text=True, timeout=60)
        self.assert_no_secret_leaked(result, secrets)
        return result

    def backups(self):
        return sorted(p.name for p in self.dir.iterdir())

    def dumps(self):
        return [name for name in self.backups() if BACKUP_NAME.match(name)]

    def test_writes_one_timestamped_archive_and_keeps_the_password_off_the_command_line(self):
        result = self.run_backup()
        self.assertEqual(result.returncode, 0, result.stderr)
        name, = self.dumps()
        self.assertEqual(self.backups(), [name, name + '.sha256'])
        self.assertEqual((self.dir / name).read_text(), COMPLETE)
        digest = hashlib.sha256(COMPLETE.encode()).hexdigest()
        self.assertEqual((self.dir / (name + '.sha256')).read_text(), '%s  %s\n' % (digest, name))
        # Owner read-write, fsGroup read, nobody else.
        for file in (name, name + '.sha256'):
            self.assertEqual(stat.S_IMODE((self.dir / file).stat().st_mode), 0o640)
        argv = self.stub_log('.argv').splitlines()
        self.assertEqual(argv[:2], ['--format=custom', '--no-password'])
        self.assertEqual(argv[-1], '--dbname=postgresql://ai_sdlc@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable')
        # The dump is written to a hidden temporary name, then renamed.
        self.assertRegex(argv[2], r'^--file=%s/\.ai-sdlc-[0-9]{8}T[0-9]{6}Z\.dump\.partial$' % re.escape(str(self.dir)))
        self.assertEqual(self.stub_log('.password'), FAKE_PASSWORD)
        self.assertFalse((self.dir / LOCK).exists())

    def test_prunes_to_the_newest_retention_and_ignores_other_files(self):
        old = ['ai-sdlc-20260101T000000Z.dump', 'ai-sdlc-20260102T000000Z.dump', 'ai-sdlc-20260103T000000Z.dump',
               'ai-sdlc-20260104T000000Z.dump']
        for name in old:
            (self.dir / name).write_text(COMPLETE)
            (self.dir / (name + '.sha256')).write_text('x')
        for name in ('notes.txt', 'ai-sdlc-manual.dump'):
            (self.dir / name).write_text('x')
        (self.dir / '.ai-sdlc-20260105T000000Z.dump.partial').write_text('half')
        (self.dir / '.ai-sdlc-20260105T000000Z.dump.sha256.partial').write_text('half')
        result = self.run_backup(retention='2')
        self.assertEqual(result.returncode, 0, result.stderr)
        new, = [f for f in self.dumps() if f not in old]
        self.assertEqual(self.backups(), sorted(['ai-sdlc-20260104T000000Z.dump', 'ai-sdlc-20260104T000000Z.dump.sha256',
                                                 new, new + '.sha256', 'notes.txt', 'ai-sdlc-manual.dump']))
        for name in old[:3]:
            self.assertIn('pruned ' + name, result.stdout)

    def test_prune_never_deletes_this_runs_backup_when_the_clock_went_back(self):
        future = ['ai-sdlc-20990101T000000Z.dump', 'ai-sdlc-20990102T000000Z.dump']
        for name in future:
            (self.dir / name).write_text(COMPLETE)
        for retention, kept in (('2', future[1:]), ('1', [])):
            with self.subTest(retention=retention):
                result = self.run_backup(retention=retention)
                self.assertEqual(result.returncode, 0, result.stderr)
                current = self.dumps()
                newest = [f for f in current if f not in future]
                self.assertEqual(len(newest), 1)
                self.assertEqual(current, sorted(newest + kept))
                for old in newest:
                    (self.dir / old).unlink()
                    (self.dir / (old + '.sha256')).unlink()

    def test_a_failed_or_truncated_dump_leaves_no_file_and_prunes_nothing(self):
        existing = ['ai-sdlc-20260101T000000Z.dump', 'ai-sdlc-20260102T000000Z.dump']
        for name in existing:
            (self.dir / name).write_text(COMPLETE)
        for flag, message in (('STUB_DUMP_FAIL', 'pg_dump failed'), ('STUB_RESTORE_FAIL', 'incomplete or unreadable'),
                              ('STUB_TRUNCATE', 'incomplete or unreadable')):
            with self.subTest(flag=flag):
                result = self.run_backup(retention='1', **{flag: '1'})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertIn('no backup was written', result.stderr)
                self.assertEqual(self.backups(), existing)

    def test_url_forms(self):
        secret = 'Fixture-Secret-0123456789'
        cases = [
            ('postgres://owner:%s@db.example:6543/app' % secret, '--dbname=postgres://owner@db.example:6543/app', secret),
            ('postgresql://owner:%s@db.example/app?sslmode=verify-full&sslrootcert=/etc/ca.crt' % secret,
             '--dbname=postgresql://owner@db.example/app?sslmode=verify-full&sslrootcert=/etc/ca.crt', secret),
            # Percent-encoded reserved characters are decoded into PGPASSWORD.
            ('postgresql://owner:a%2Fb%40c%3Ad%25e%3Ff%23g%20h-' + secret + '@db.example/app',
             '--dbname=postgresql://owner@db.example/app', 'a/b@c:d%e?f#g h-' + secret),
            # Raw "@", ":", "?" and "#" in the password are accepted.
            ('postgresql://owner:p@ss:w?rd#-%s@db.example/app' % secret, '--dbname=postgresql://owner@db.example/app',
             'p@ss:w?rd#-' + secret),
            ('postgresql://owner:%s@db.example?sslmode=require' % secret, '--dbname=postgresql://owner@db.example?sslmode=require',
             secret),
            ('postgresql://owner@db.example/app', '--dbname=postgresql://owner@db.example/app', '<unset>'),
        ]
        for url, expected, password in cases:
            with self.subTest(url=expected, password=password != '<unset>'):
                for old in self.dir.iterdir():
                    old.unlink()
                result = self.run_backup(url=url, secrets=(secret, password) if password != '<unset>' else (secret,))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.stub_log('.argv').splitlines()[-1], expected)
                self.assertEqual(self.stub_log('.password'), password)

    def test_refuses_bad_inputs_without_echoing_them(self):
        secret = 'Fixture-Secret-0123456789'
        cases = [
            ({'url': 'mysql://u:%s@h/d' % secret}, 'postgresql:// URL'),
            # A raw "/" would leave the password in the URL passed to pg_dump.
            ({'url': 'postgresql://owner:abc/%s@h:5432/d' % secret}, "raw '/'"),
            ({'url': 'postgresql://owner:%s/x@h/d' % secret}, "raw '/'"),
            ({'url': 'postgresql://owner@h/d?sslmode=require&password=%s' % secret}, 'password query parameter'),
            ({'url': 'postgresql://owner@h/d?PASSWORD=%s' % secret}, 'password query parameter'),
            ({'url': 'postgresql://owner@h?password=%s' % secret}, 'password query parameter'),
            ({'url': 'postgresql://owner:%s%%2@h/d' % secret}, 'invalid percent escape'),
            ({'url': 'postgresql://owner:%s%%zz@h/d' % secret}, 'invalid percent escape'),
            ({'url': 'postgresql://owner:%s%%00@h/d' % secret}, 'encoded NUL'),
            ({'url': 'postgresql:///d'}, 'no host'),
            ({'url': 'postgresql://owner:%s@/d' % secret}, 'no host'),
            ({'retention': '0'}, 'BACKUP_RETENTION'), ({'retention': '-1'}, 'BACKUP_RETENTION'),
            ({'retention': 'seven'}, 'BACKUP_RETENTION'), ({'retention': '07'}, 'BACKUP_RETENTION'),
            ({'BACKUP_LOCK_STALE_SECONDS': 'x'}, 'BACKUP_LOCK_STALE_SECONDS'),
            ({'BACKUP_DIR': str(self.dir / 'missing')}, 'missing'),
        ]
        for env, message in cases:
            with self.subTest(env=env):
                result = self.run_backup(secrets=(secret, 'abc/' + secret, secret[:12]), **env)
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue(result.stderr.startswith('backup: '), result.stderr)
                self.assertIn(message, result.stderr)
                self.assertEqual(self.backups(), [])
                self.assertIsNone(self.stub_log('.argv'), 'pg_dump ran')

    def test_a_held_lock_refuses_and_leaves_the_other_runs_files(self):
        lock = self.dir / LOCK
        lock.mkdir()
        partial = self.dir / '.ai-sdlc-20260105T000000Z.dump.partial'
        partial.write_text('in progress')
        result = self.run_backup()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('holds the volume lock', result.stderr)
        self.assertTrue(lock.is_dir())
        self.assertEqual(partial.read_text(), 'in progress')
        self.assertIsNone(self.stub_log('.argv'))
        # A restore takes the same lock.
        (self.dir / 'ai-sdlc-20260101T000000Z.dump').write_text(COMPLETE)
        restore = self.run_backup(RESTORE_FILE='ai-sdlc-20260101T000000Z.dump')
        self.assertIn('holds the volume lock', restore.stderr)
        self.assertIsNone(self.stub_log('.restore'))

    def test_a_stale_lock_is_removed_and_every_exit_releases_the_lock(self):
        lock = self.dir / LOCK
        lock.mkdir()
        old = time.time() - 120
        os.utime(lock, (old, old))
        result = self.run_backup(BACKUP_LOCK_STALE_SECONDS='60')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('removing a lock older than 60 seconds', result.stdout)
        self.assertFalse(lock.exists())
        failed = self.run_backup(STUB_DUMP_FAIL='1')
        self.assertNotEqual(failed.returncode, 0)
        self.assertFalse(lock.exists())

    def test_concurrent_runs_never_share_the_volume(self):
        env = self.environment('postgresql://ai_sdlc:%s@h/d' % FAKE_PASSWORD, '5', {'STUB_SLEEP': '3'})
        first = subprocess.Popen(['/bin/sh', '-c', SCRIPT.read_text()], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        deadline = time.time() + 10
        while not (self.dir / LOCK).exists() and time.time() < deadline:
            time.sleep(0.05)
        second = subprocess.run(['/bin/sh', '-c', SCRIPT.read_text()], env=dict(env, STUB_SLEEP=''),
                                capture_output=True, text=True, timeout=60)
        out, err = first.communicate(timeout=60)
        self.assertEqual(first.returncode, 0, err)
        self.assertNotEqual(second.returncode, 0)
        self.assertIn('holds the volume lock', second.stderr)
        self.assertEqual(len(self.dumps()), 1)
        self.assertFalse((self.dir / LOCK).exists())

    def test_restore_replaces_the_database_in_one_transaction_only_when_no_component_is_connected(self):
        name = 'ai-sdlc-20260101T000000Z.dump'
        (self.dir / name).write_text(COMPLETE)
        (self.dir / (name + '.sha256')).write_text('%s  %s\n' % (hashlib.sha256(COMPLETE.encode()).hexdigest(), name))
        (self.dir / 'ai-sdlc-20251231T000000Z.dump').write_text(COMPLETE)
        result = self.run_backup(retention='1', RESTORE_FILE=name)
        self.assertEqual(result.returncode, 0, result.stderr)
        argv = self.stub_log('.restore').splitlines()
        self.assertEqual(argv, ['--clean', '--if-exists', '--no-owner', '--single-transaction', '--exit-on-error', '--no-password',
                                '--dbname=postgresql://ai_sdlc@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable', str(self.dir / name)])
        self.assertEqual(self.stub_log('.restore-password'), FAKE_PASSWORD)
        # A restore neither writes a backup nor prunes one, and releases the lock.
        self.assertEqual(self.backups(), ['ai-sdlc-20251231T000000Z.dump', name, name + '.sha256'])
        self.assertIsNone(self.stub_log('.argv'))
        (self.dir / 'ai-sdlc-20260102T000000Z.dump').write_text('PGDMP truncated\n')
        (self.dir / 'ai-sdlc-20260103T000000Z.dump').write_text(COMPLETE)
        (self.dir / 'ai-sdlc-20260103T000000Z.dump.sha256').write_text('%s  ai-sdlc-20260103T000000Z.dump\n' % ('0' * 64))
        for extra, message in (({'STUB_SESSIONS': '2'}, 'scale the components to zero first'),
                               ({'STUB_PSQL_FAIL': '1'}, 'cannot connect'),
                               ({'STUB_APPLY_FAIL': '1'}, 'rolled back'),
                               ({'STUB_RESTORE_FAIL': '1'}, 'unreadable'),
                               ({'RESTORE_FILE': 'ai-sdlc-20260102T000000Z.dump'}, 'incomplete'),
                               ({'RESTORE_FILE': 'ai-sdlc-20260103T000000Z.dump'}, 'does not match its checksum'),
                               ({'RESTORE_FILE': 'ai-sdlc-20260104T000000Z.dump'}, 'not on the backup volume'),
                               ({'RESTORE_FILE': '../etc/passwd'}, 'must name a backup file'),
                               ({'RESTORE_FILE': 'x/' + name}, 'must name a backup file'),
                               ({'RESTORE_FILE': name + '\nfoo'}, 'must name a backup file'),
                               ({'RESTORE_FILE': 'foo\n' + name}, 'must name a backup file')):
            with self.subTest(extra=extra):
                Path(str(self.log) + '.restore').unlink(missing_ok=True)
                result = self.run_backup(**dict({'RESTORE_FILE': name}, **extra))
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertFalse((self.dir / LOCK).exists())
                if 'STUB_APPLY_FAIL' not in extra:
                    self.assertIsNone(self.stub_log('.restore'))

    def test_runbook_covers_the_required_procedures(self):
        text = RUNBOOK.read_text()
        for phrase in ('backup.enabled', 'pg_restore --list', 'pg_restore --file=/dev/null', '--replicas=0',
                       'row-level security', 'not backed up', 'Test the restore', '<fullname>-backup', '5432'):
            self.assertIn(phrase, text)
        self.assertNotIn('two runs never overlap', text)
        self.assertNotRegex(text, r'postgresql://[^:\s]+:(?!\$\{?password|<password>)[A-Za-z0-9_-]{16,}@')


if __name__ == '__main__':
    unittest.main()
