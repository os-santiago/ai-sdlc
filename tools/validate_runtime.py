#!/usr/bin/env python3
"""Run complete local Go validation with owned, disposable PostgreSQL resources."""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import subprocess
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
OWNER_LABEL = 'ai-sdlc.io/validation-run'
REQUIRED_TESTS = {
    ('internal/execution', 'TestExecutionFailureObservationIsImmutableAndNotAuthority'),
    ('internal/execution', 'TestTerminalSignedHTTPBoundaries'),
    ('internal/execution', 'TestProductionCompletionAuthenticatesFullHistoryAndOriginalWork'),
    ('internal/execution', 'TestProductionCompletionRejectsIncompleteTamperedOrForeignFacts'),
    ('internal/execution', 'TestProductionCompletionHandoffLostCommitAndAtomicFailure'),
    ('internal/execution', 'TestProductionCompletionConsumerLostCommitACKIsIdempotent'),
    ('internal/execution', 'TestProductionCompletionConcurrentPlannerWithdrawalPreservesFact'),
    ('internal/execution', 'TestTerminalJournalOrderedEffectsAndClosureFact'),
    ('internal/execution', 'TestTerminalJournalRejectsUnacknowledgedAndTamperedEffects'),
    ('internal/execution', 'TestTerminalJournalLostCommitACKConservativeRecovery'),
    ('internal/execution', 'TestTerminalJournalRejectsCrossStepRelabelledHistory'),
    ('internal/execution', 'TestTerminalJournalSelectionFiltersChangedIssueAndConfiguration'),
    ('internal/execution', 'TestTerminalJournalWithdrawalDuringSendWait'),
    ('internal/execution', 'TestTerminalJournalFinishLostCommitACKPreservesCompletedFact'),
    ('internal/execution', 'TestTerminalControllerRecoversOnlyOriginalACKs'),
    ('internal/execution', 'TestTerminalControllerExcludesReplicaAndReleasesSession'),
    ('internal/execution', 'TestTerminalControllerCancelsWhenOriginalBackendDies'),
    ('internal/execution', 'TestTerminalPlanDerivesOnlyAuthenticatedSourceAndArtifact'),
    ('internal/execution', 'TestCompletionAuthenticatedHistoryAndSignedHandoff'),
    ('internal/execution', 'TestCompletionRejectsTamperedHistoryAndWithdrawal'),
    ('internal/execution', 'TestCompletionNeverTreatsRollbackAsRelease'),
    ('internal/execution', 'TestCompletionWindowDoesNotRenewExpiredDeployment'),
    ('internal/execution', 'TestCompletionLostCommitAcknowledgementDoesNotRedispatch'),
    ('internal/execution', 'TestCompletionWithdrawalDuringOutboxWait'),
    ('internal/execution', 'TestDeploymentRecoveryUsesOnlyOriginalAcknowledgedEffects'),
    ('internal/execution', 'TestDeploymentControllerExcludesReplicaAndReleasesSession'),
    ('internal/execution', 'TestDeploymentControllerCancelsWhenOriginalBackendDies'),
    ('internal/execution', 'TestDeploymentSignedHandoffInboxAndRuntime'),
    ('internal/execution', 'TestDeploymentRuntimeContainsFailuresAndQuarantinesAmbiguity'),
    ('internal/execution', 'TestDeploymentHandoffFailureIsAtomic'),
    ('internal/execution', 'TestDeploymentSelectorsExcludeWithdrawnIssues'),
    ('internal/execution', 'TestDeploymentRuntimeIndependentTargetLoaderForContainment'),
    ('internal/execution', 'TestDeploymentHandoffWithdrawalDuringOutboxWait'),
    ('internal/execution', 'TestDeploymentMountedTargetSurvivesOnlyAdvancePolicyWithdrawal'),
    ('internal/execution', 'TestDeploymentJournalExclusiveIntentSendAndCompletion'),
    ('internal/execution', 'TestDeploymentJournalRollbackRequiresOwnedAckAndSurvivesFreeze'),
    ('internal/execution', 'TestDeploymentJournalUncertainSendCannotBeRepeatedOrAdopted'),
    ('internal/execution', 'TestDeploymentJournalRejectsForeignAuthorityAndHealth'),
    ('internal/execution', 'TestDeploymentCommandBindsHistoryAndCannotExtendPlan'),
    ('internal/execution', 'TestDeploymentJournalRevocationDuringBlockedHistory'),
    ('internal/execution', 'TestDeploymentJournalLostFactAndCompletionAcknowledgements'),
    ('internal/execution', 'TestDeploymentCommandPreservesSubmillisecondHistoryOrder'),
    ('internal/execution', 'TestDeploymentHealthExpiryWhileInsertWaits'),
    ('internal/store', 'TestConcurrentFreshMigrations'),
    ('internal/intake', 'TestDurableDedupeAndWithdrawal'),
    ('internal/execution', 'TestSignedExecutionDuplicateDeliveryAndSingleClaim'),
    ('internal/execution', 'TestCandidateCapturePersistsExactDeltaAndSealsBudget'),
    ('internal/store', 'TestModelReservationConcurrentIdempotenceAndSingleClaim'),
}


class ValidationError(Exception):
    pass


def test_evidence(path):
    passed, skipped, packages = set(), [], set()
    with path.open(encoding='utf-8') as stream:
        for line in stream:
            try:
                event = json.loads(line)
            except (ValueError, TypeError):
                continue  # Go may print dependency-download diagnostics too.
            if not isinstance(event, dict):
                continue
            package = event.get('Package', '').split('/runtime/k8s/')[-1]
            test = event.get('Test')
            if event.get('Action') == 'pass':
                if test:
                    passed.add((package, test))
                else:
                    packages.add(package)
            elif event.get('Action') == 'skip' and test:
                skipped.append(package + '/' + test)
    missing = sorted('/'.join(item) for item in REQUIRED_TESTS - passed)
    return {'passedTests': len(passed), 'passedPackages': len(packages),
            'skippedTests': sorted(skipped), 'missingDatabaseProof': missing}


class Run:
    def __init__(self, provider, root, output, cache, timeout, lock):
        self.provider, self.root, self.output, self.cache = provider, root, output, cache
        self.lock = lock
        self.environment = None
        self.identity = uuid.uuid4().hex
        self.names = ['ai-sdlc-validation-' + self.identity + suffix for suffix in ('-db', '-go')]
        self.deadline = time.monotonic() + timeout
        self.report = {'version': 1, 'run': self.identity, 'provider': provider,
                       'images': lock, 'status': 'failed', 'stages': [], 'cleanup': []}

    def remaining(self, limit=60):
        available = self.deadline - time.monotonic()
        if available <= 0:
            raise ValidationError('Overall validation deadline exceeded')
        return min(limit, available)

    def call(self, args, *, log=None, limit=60, check=True):
        try:
            result = subprocess.run([self.provider] + args, stdout=log or subprocess.PIPE,
                                    stderr=subprocess.STDOUT, text=True, env=self.environment,
                                    timeout=self.remaining(limit), check=False)
        except subprocess.TimeoutExpired as exc:
            raise ValidationError('Container operation exceeded its deadline') from exc
        if check and result.returncode:
            raise ValidationError('Container operation failed; inspect the stage log')
        return result

    def stage(self, name, args, limit=900):
        record = {'name': name, 'status': 'failed', 'log': name + '.log'}
        self.report['stages'].append(record)
        start = time.monotonic()
        print('Validating ' + name + '...', flush=True)
        try:
            with (self.output / record['log']).open('x', encoding='utf-8') as log:
                self.call(args, log=log, limit=limit)
            record['status'] = 'passed'
        finally:
            record['seconds'] = round(time.monotonic() - start, 3)

    def database_diagnostics(self):
        # Report only allowlisted state and error classifications; PostgreSQL
        # logs or container configuration can contain fixture credential data.
        try:
            result = subprocess.run([self.provider, 'container', 'inspect', self.names[0]],
                                    capture_output=True, text=True, timeout=10, check=True)
            item = json.loads(result.stdout)[0]
            if item.get('Config', {}).get('Labels', {}).get(OWNER_LABEL) != self.identity:
                return
            state = item.get('State', {})
            logs = subprocess.run([self.provider, 'logs', '--tail', '40', item['Id']],
                                  capture_output=True, text=True, timeout=10, check=False)
            diagnostic = (logs.stdout + logs.stderr)[-262144:].lower()
            self.report['databaseDiagnostics'] = {
                'status': state.get('Status'), 'exitCode': state.get('ExitCode'),
                'oomKilled': bool(state.get('OOMKilled')),
                'noSpaceReported': 'no space left on device' in diagnostic,
                'outOfMemoryReported': 'out of memory' in diagnostic,
                'walMaxMB': 128, 'walMinMB': 32, 'checkpointSeconds': 30,
            }
        except (subprocess.SubprocessError, OSError, ValueError, KeyError, IndexError):
            self.report['databaseDiagnostics'] = {'unavailable': True}

    def cleanup(self):
        # Cleanup has its own bounded window, including after the work deadline.
        for name in reversed(self.names):
            entry = {'container': name, 'status': 'failed'}
            self.report['cleanup'].append(entry)
            try:
                result = subprocess.run([self.provider, 'container', 'inspect', name],
                                        capture_output=True, text=True, timeout=10, check=False)
                if result.returncode:
                    # Do not interpret a daemon failure as proof of absence.
                    absent = subprocess.run([self.provider, 'ps', '-a', '--format', '{{.Names}}'],
                                            capture_output=True, text=True, timeout=10, check=True)
                    if name in absent.stdout.splitlines():
                        raise ValidationError('Container remains but cannot be inspected')
                    entry['status'] = 'absent'
                    continue
                data = json.loads(result.stdout)
                if (not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], dict)
                        or not isinstance(data[0].get('Config'), dict)
                        or not isinstance(data[0]['Config'].get('Labels'), dict)
                        or data[0]['Config']['Labels'].get(OWNER_LABEL) != self.identity
                        or not re.fullmatch('[a-f0-9]{64}', data[0].get('Id', ''))):
                    entry['status'] = 'refused-unowned'
                    continue
                # Delete the observed object, never a replacement using its name.
                subprocess.run([self.provider, 'rm', '-f', '-v', data[0]['Id']], capture_output=True,
                               timeout=15, check=True)
                entry['status'] = 'removed'
            except (subprocess.SubprocessError, OSError, ValueError, ValidationError):
                entry['status'] = 'failed'

    def execute(self):
        started = time.monotonic()
        try:
            commit = subprocess.run(['git', '-C', str(self.root), 'rev-parse', 'HEAD'],
                                    capture_output=True, text=True, check=True, timeout=10).stdout.strip()
            dirty = subprocess.run(['git', '-C', str(self.root), 'status', '--porcelain', '-z'],
                                   capture_output=True, check=True, timeout=10).stdout
            common_result = subprocess.run(['git', '-C', str(self.root), 'rev-parse', '--path-format=absolute', '--git-common-dir'],
                                           capture_output=True, text=True, check=True, timeout=10)
            common = Path(common_result.stdout.strip()).resolve()
            if ':' in str(common) or common == Path('/') or not (common / 'HEAD').is_file() or not (common / 'objects').is_dir():
                raise ValidationError('Unsupported Git metadata directory')
            source_mounts = ['-v', str(self.root) + ':' + str(self.root) + ':ro,z']
            external_metadata = not common.is_relative_to(self.root)
            if external_metadata:
                # Preserve absolute worktree gitdir references without mounting
                # the parent workspace or unrelated working trees.
                source_mounts += ['-v', str(common) + ':' + str(common) + ':ro,z']
            self.report['source'] = {'commit': commit, 'dirty': bool(dirty),
                                     'gitLayout': 'worktree' if external_metadata else 'checkout'}
            password = secrets.token_urlsafe(36)
            self.environment = dict(os.environ, POSTGRES_PASSWORD=password,
                                    PGPASSWORD=password, POSTGRES_DB='ai_sdlc_test',
                                    AI_SDLC_TEST_DATABASE_URL='postgres://postgres:' + password +
                                    '@127.0.0.1:5432/ai_sdlc_test?sslmode=disable')
            # The CLI inherits values by name; credentials never enter argv,
            # host files, reports or image configuration in source control.
            fixture_env = [item for name in ('POSTGRES_PASSWORD', 'PGPASSWORD', 'POSTGRES_DB', 'AI_SDLC_TEST_DATABASE_URL')
                           for item in ('--env', name)]
            db, runner = self.names
            shared = ['--platform', self.lock['platform'], '--label', OWNER_LABEL + '=' + self.identity]
            self.stage('database-start', ['run', '-d', '--name', db, *shared, *fixture_env,
                                         '--tmpfs', '/var/lib/postgresql/data:rw,size=512m',
                                         self.lock['postgres'], 'postgres', '-c', 'listen_addresses=127.0.0.1',
                                         '-c', 'max_wal_size=128MB', '-c', 'min_wal_size=32MB',
                                         '-c', 'checkpoint_timeout=30s'], 180)
            inspected = self.call(['container', 'inspect', db])
            database = json.loads(inspected.stdout)[0]
            mounts = database['Mounts']
            tmpfs = database.get('HostConfig', {}).get('Tmpfs', {})
            if (any(item.get('Type') == 'volume' for item in mounts)
                    or '/var/lib/postgresql/data' not in tmpfs):
                raise ValidationError('Database storage is not exclusively disposable tmpfs')
            actual_fs = self.call(['exec', db, 'stat', '-f', '-c', '%T', '/var/lib/postgresql/data'])
            if actual_fs.stdout.strip() != 'tmpfs':
                raise ValidationError('Database files are not on the expected memory filesystem')
            self.report['databaseStorage'] = 'tmpfs-512MiB'
            readiness_record = {'name': 'database-ready', 'status': 'failed', 'log': 'database-ready.log'}
            self.report['stages'].append(readiness_record)
            readiness_started = time.monotonic()
            readiness = readiness_started + min(30, self.remaining())
            try:
                with (self.output / 'database-ready.log').open('x', encoding='utf-8') as log:
                    while True:
                        result = self.call(['exec', db, 'psql', '-h', '127.0.0.1', '-U', 'postgres', '-d', 'ai_sdlc_test', '-qAtc', 'SELECT 1'], log=log, limit=5, check=False)
                        if result.returncode == 0:
                            break
                        if time.monotonic() >= readiness:
                            raise ValidationError('PostgreSQL readiness deadline exceeded')
                        time.sleep(0.25)
                readiness_record['status'] = 'passed'
            finally:
                readiness_record['seconds'] = round(time.monotonic() - readiness_started, 3)
            self.cache.mkdir(parents=True, exist_ok=True)
            for child in ('mod', 'build'):
                (self.cache / child).mkdir(exist_ok=True)
            self.stage('runner-start', ['run', '-d', '--name', runner, *shared,
                       '--network', 'container:' + db, *fixture_env,
                       '-e', 'GOFLAGS=-mod=readonly',
                       *source_mounts,
                       '-v', str(self.cache / 'mod') + ':/go/pkg/mod:z',
                       '-v', str(self.cache / 'build') + ':/root/.cache/go-build:z',
                       '-w', str(self.root / 'runtime/k8s'), self.lock['go'], 'sleep', str(int(self.remaining(3600)) + 60)], 180)
            self.stage('git-dependency', ['exec', runner, 'apk', 'add', '--no-cache', 'git'], 120)
            self.stage('git-trust', ['exec', runner, 'git', 'config', '--global', '--add', 'safe.directory', str(self.root)], 10)
            self.stage('build', ['exec', runner, 'go', 'build', './...'])
            self.stage('vet', ['exec', runner, 'go', 'vet', './...'])
            self.stage('test', ['exec', runner, 'go', 'test', '-json', '-count=1', '-timeout=25m', '-p', '1', './...'], 1800)
            evidence = test_evidence(self.output / 'test.log')
            self.report['tests'] = evidence
            if evidence['missingDatabaseProof']:
                raise ValidationError('Required PostgreSQL tests did not pass; see missingDatabaseProof')
            self.report['status'] = 'passed'
        except (ValidationError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as exc:
            # Never include subprocess arguments or environment values in the report.
            self.report['error'] = str(exc) if isinstance(exc, ValidationError) else type(exc).__name__
        finally:
            self.database_diagnostics()
            self.cleanup()
            self.environment = None
            if any(item['status'] not in ('removed', 'absent') for item in self.report['cleanup']):
                self.report['status'] = 'failed'
                self.report['cleanupRequired'] = True
            self.report['seconds'] = round(time.monotonic() - started, 3)
            (self.output / 'report.json').write_text(json.dumps(self.report, indent=2) + '\n')
        return 0 if self.report['status'] == 'passed' else 1


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--provider', choices=['auto', 'podman', 'docker'], default='auto')
    parser.add_argument('--timeout', type=int, default=2100, help='Overall work deadline, 60–3600 seconds')
    parser.add_argument('--output', type=Path, help='New private directory for logs and report')
    args = parser.parse_args(argv)
    if not 60 <= args.timeout <= 3600:
        parser.error('--timeout must be between 60 and 3600 seconds')
    provider = args.provider
    if provider == 'auto':
        provider = next((name for name in ('podman', 'docker') if shutil.which(name)), None)
    if not provider or not shutil.which(provider):
        parser.error('Install Docker or Podman first')
    lock = json.loads((ROOT / 'tools/runtime-validation.lock.json').read_text())
    if (set(lock) != {'version', 'platform', 'go', 'postgres'} or lock['version'] != 1
            or lock['platform'] != 'linux/amd64'
            or any(not re.fullmatch(r'docker.io/library/[a-z]+@sha256:[a-f0-9]{64}', lock[key]) for key in ('go', 'postgres'))):
        parser.error('Invalid committed validation image lock')
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state'))
    output = (args.output or state / 'ai-sdlc/validation' / uuid.uuid4().hex).absolute()
    if ':' in str(ROOT) or ':' in str(output):
        parser.error('Container mount paths cannot contain colons')
    output.mkdir(parents=True, mode=0o700, exist_ok=False)
    cache = Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/validation/go'
    if ':' in str(cache):
        parser.error('Container cache path cannot contain colons')
    previous = signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        result = Run(provider, ROOT, output, cache.absolute(), args.timeout, lock).execute()
    finally:
        signal.signal(signal.SIGTERM, previous)
    print('Validation report: ' + str(output / 'report.json'))
    return result


if __name__ == '__main__':
    sys.exit(main())
