"""Upgrade a paused pre-candidate runtime through the additive diagnostic bridge."""
import base64
import copy
import json
import tempfile
import time

import bootstrap_database_forward
import bootstrap_prepare as bootstrap
import build_foundation_install as files
import build_runtime_install
import diagnostic_upgrade_target
from diagnostic_upgrade_data import Compatibility
import initial_activation
import install
import install_images
from installer_process import capture
import konflux_runtime_install as platform
import prepared_configuration
import prepared_resources
from runtime_upgrade_backup import Backup, VERSION as BACKUP_VERSION
from runtime_upgrade_pause import Pause, pause_once
import upgrade_backup_data as data
from upgrade_fence_client import Fence
from upgrade_migrator import Migrator

VERSION = 'ai-sdlc.diagnostic-runtime-upgrade/v1'
PHASES = ('migrated', 'staged', 'warming', 'armed', 'active')


def advance(record, phase, observe, apply, save, phases=PHASES, converge_seconds=120):
    """A lost Helm acknowledgement permits observation, never a repeated send.

    The transition records ``sent`` only immediately before Helm is invoked. A
    transition without ``sent: true`` from this version never reached Helm and
    restarts safely; earlier journals (no ``sent`` key) keep the original
    observation-only reconciliation."""
    if time.time() >= record['expiresAt']:
        raise ValueError('Original upgrade window expired')
    transition = record.get('transition')
    if transition is not None and transition.get('sent') is False:
        if transition['to'] != phase:
            raise ValueError('Original upgrade transition differs')
        record['transition'] = transition = None
        save(record)
    if transition is not None:
        if transition['to'] != phase:
            raise ValueError('Original upgrade transition differs')
        observe(phase)
    else:
        if phase not in phases[1:] or record['completed'] != phases[phases.index(phase) - 1]:
            raise ValueError('Original upgrade phase order changed')
        observe(record['completed'])
        record['transition'] = {'from': record['completed'], 'to': phase, 'sent': False}
        save(record)
        observe(record['completed'])
        record['transition']['sent'] = True
        save(record)
        apply(phase)
        deadline = min(record['expiresAt'], time.time() + converge_seconds)
        while True:
            try:
                observe(phase)
                break
            except ValueError:
                if time.time() >= deadline:
                    raise ValueError('Original upgrade target has not converged') from None
                time.sleep(.5)
    if time.time() >= record['expiresAt']:
        raise ValueError('Original upgrade window expired before the phase was recorded')
    record.update(completed=phase, transition=None)
    save(record)


class OriginalReleasePreflight(initial_activation.Activation):
    """Reuse original release checks without rewriting original activation custody."""
    def __init__(self, upgrade):
        self.upgrade = upgrade
        self.config = upgrade.config
        self.binaries = self.config.binaries
        self.state = self.config.state_dir
        self.original = upgrade.pause.activation.original

    def guard(self):
        return self.upgrade.observe(self.upgrade.record['completed'])[0]


class Upgrade:
    def __init__(self, config):
        self.config = config
        self.pause = Pause(config)
        if self.pause.record is None or self.pause.record.get('phase') != 'paused':
            raise ValueError('Original completed administrative pause required')
        self.pause_hash = bootstrap.digest(self.pause.record)
        self.backup_path = config.state_dir / 'runtime-upgrade-backup.json'
        self.backup = files.bounded_json(self.backup_path, 2 * 1024 * 1024)
        if (self.backup.get('version') != BACKUP_VERSION or self.backup.get('phase') != 'verified'
                or self.backup.get('pauseSHA256') != self.pause_hash
                or self.backup.get('restore', {}).get('state') != 'removed'):
            raise ValueError('Original verified and cleaned-up backup required')
        self.backup_hash = bootstrap.digest(self.backup)
        self.path = config.state_dir / 'diagnostic-runtime-upgrade.json'
        self.record = files.bounded_json(self.path, 32 * 1024 * 1024) if self.path.exists() or self.path.is_symlink() else None
        if self.record is not None and (self.record.get('version') != VERSION
                or self.record.get('pauseSHA256') != self.pause_hash or self.record.get('backupSHA256') != self.backup_hash):
            raise ValueError('Original diagnostic transition custody changed')

    def save(self, record):
        platform.write_record(self.path, record)
        self.record = record

    def unchanged(self):
        self.pause.unchanged()
        if (bootstrap.digest(files.bounded_json(self.pause.path, 16 * 1024 * 1024)) != self.pause_hash
                or bootstrap.digest(files.bounded_json(self.backup_path, 2 * 1024 * 1024)) != self.backup_hash):
            raise ValueError('Original pause or backup record changed')
        data.verify_archives(self.config.state_dir / 'runtime-upgrade-backup', self.backup['archives'])

    def describe(self, phase):
        return self.pause.record['target'] if phase in ('source', 'migrated') else self.record['targets'][phase]

    def observe(self, phase):
        self.unchanged()
        description = self.describe(phase)
        prepared_resources.observe(self.config, description, self.pause.record['resourceUIDs'])
        observer = bootstrap.Preparation(self.config.base_run, self.config.kubectl, self.config.provider,
            self.config.cluster, self.config.state_dir, description['images'])
        paused = phase in ('source', 'migrated', 'staged')
        if paused:
            observer.use_configured_render(description['documents'])
        else:
            observer.use_activation_render(description['documents'], phase)
        observed, connection = observer.observe(paused=paused)
        prepared_configuration.same_foundation(self.pause.record['sourceObserved'], observed)
        return observed, connection

    def database(self):
        return data.Database(capture, self.config.kubectl + ['exec', 'ai-sdlc-postgres-0', '-n', platform.NAMESPACE, '--'])

    def begin(self):
        if self.record is not None:
            return
        verified = Backup(self.config)
        verified.status()
        self.observe('source')
        plan = self.pause.record['plan']
        helper = install_images.REGISTRY + '@' + plan['target']['images']['migrator']['digest']
        compile_build = lambda request: build_runtime_install.component_call(self.config.provider, helper, 'compile-build-installation', request)
        targets, selection = diagnostic_upgrade_target.targets(self.pause.record['target'], plan, self.config.render, compile_build)
        def inspect(database):
            proof = Compatibility(database).source(plan['source']['migrations'])
            if proof['data'] != self.backup['retainedData']:
                raise ValueError('Current source differs from the restored backup')
            return proof
        proof = verified.source(inspect)
        now = time.time()
        self.save({'version': VERSION, 'pauseSHA256': self.pause_hash, 'backupSHA256': self.backup_hash,
            'createdAt': now, 'expiresAt': initial_activation.activation_expiry(self.pause.record['target']['values'], now),
            'mode': 'diagnostic', 'completed': 'source', 'transition': None, 'targets': targets,
            'selection': selection, 'sourceProof': proof, 'migration': {}})

    def deadline(self):
        if time.time() >= self.record['expiresAt']:
            raise ValueError('Original upgrade window expired; preserve paused custody')

    def migrate(self):
        self.deadline()
        observed, connection = self.observe('source')
        intent = self.record['migration']
        if intent:
            if intent.get('state') != 'completed' or intent.get('cleanup') != 'removed':
                raise ValueError('Original migration outcome incomplete; no automatic replay')
        else:
            if Compatibility(self.database()).source(self.pause.record['plan']['source']['migrations']) != self.record['sourceProof']:
                raise ValueError('Original migration source changed')
            resource = self.config.read('secret', 'ai-sdlc-database-roles')
            if set(resource.get('data', {})) != {'passwords.json'}:
                raise ValueError('Original role credential document required')
            passwords = json.loads(base64.b64decode(resource['data']['passwords.json'], validate=True))
            self.observe('source')
            image = install_images.REGISTRY + '@' + self.record['selection']['selected']['migrator']['digest']
            with bootstrap_database_forward.original_database(self.config.base_run, self.config.kubectl,
                    connection, observed['resources']['pod'], timeout_seconds=240) as (forwarded, port):
                helper = Migrator(self.config.provider, image, forwarded, port, passwords, intent, lambda value: self.save(self.record),
                                  expires_at=self.record['expiresAt'], guard=lambda: self.observe('source'))
                try:
                    helper.run()
                finally:
                    helper.close()
        self.observe('source')
        self.record['compatibility'] = Compatibility(self.database()).verify(self.record['sourceProof'])
        self.observe('source')
        self.record['completed'] = 'migrated'
        self.save(self.record)

    def apply(self, phase, command=capture):
        self.deadline()
        description = self.describe(phase)
        self.unchanged()
        if self.config.render(description['values']) != description['documents']:
            raise ValueError('Original diagnostic target render changed')
        remaining = min(60, self.record['expiresAt'] - time.time())
        if remaining <= 0:
            raise ValueError('Original upgrade window expired before Helm')
        with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as values:
            json.dump(description['values'], values)
            values.flush()
            command([self.config.binaries['helm'], 'upgrade', 'ai-sdlc', str(install.CHART),
                '--kubeconfig', str(self.config.state_dir / 'kubeconfig'), '--kube-context', 'kind-' + self.config.cluster,
                '-n', platform.NAMESPACE, '-f', values.name, '--timeout', str(max(1, int(remaining))) + 's'], timeout=remaining)

    def resume(self):
        for phase in PHASES[PHASES.index(self.record['completed']) + 1:]:
            if self.record.get('transition') is None:
                self.observe(self.record['completed'])
                OriginalReleasePreflight(self).preflight()
                if phase in ('armed', 'active'):
                    Compatibility(self.database()).quiet()
                    self.observe(self.record['completed'])
            advance(self.record, phase, self.observe, self.apply, self.save)
        self.publish_report()
        return self.status()

    def run(self):
        self.begin()
        if self.record.get('recoveryPause') is not None:
            raise ValueError('Original recovery intent must be completed through recovery')
        if self.record['completed'] == 'source':
            self.migrate()
        return self.resume()

    def recover(self):
        """Revert only component selection; retain all additive schema and history."""
        if self.record is None:
            raise ValueError('Original upgrade intent required for recovery')
        self.deadline()
        if self.record.get('transition') is not None:
            phase = self.record['transition']['to']
            advance(self.record, phase, self.observe, self.apply, self.save)  # observation only
        if self.record.get('recoveryPause') is not None:
            recovery = self.record['recoveryPause']
            if recovery.get('phase') != 'paused' or recovery.get('cleanup') != 'complete':
                raise ValueError('Recovery pause outcome incomplete; no automatic replay')
            self.observe('source')
            compatible = Compatibility(self.database()).verify(self.record['sourceProof'], allow_unmigrated=True)
            if compatible['catalogSize'] != recovery['catalogSize']:
                raise ValueError('Original completed recovery source changed')
            return self.finish_recovery(compatible)
        phase = self.record['completed']
        intent = self.record['migration']
        if intent and intent.get('cleanup') != 'removed':
            raise ValueError('Original migration helper cleanup is unresolved')
        observed, connection = self.observe(phase)
        compatible = Compatibility(self.database()).verify(self.record['sourceProof'], allow_unmigrated=True)
        plan = self.pause.record['plan']
        catalog = plan['target' if compatible['catalogSize'] == 34 else 'source']['migrations']
        image = install_images.REGISTRY + '@' + plan['target']['images']['migrator']['digest']
        pause_record = {'phase': 'planned', 'expiresAt': self.record['expiresAt']}
        with bootstrap_database_forward.original_database(self.config.base_run, self.config.kubectl,
                connection, observed['resources']['pod'], timeout_seconds=240) as (forwarded, port):
            with Fence(self.config.provider, image, forwarded, port, catalog, expires_at=self.record['expiresAt']) as fence:
                if Compatibility(data.Database(fence.command, self.database().prefix)).verify(self.record['sourceProof'], allow_unmigrated=True) != compatible:
                    raise ValueError('Original recovery database changed before exclusion')
                pause_record['fence'] = {'nonce': fence.nonce, 'expiresAt': fence.expiry, 'containerID': fence.container_id}
                self.record['recoveryPause'] = pause_record
                self.save(self.record)
                pause_once(pause_record, fence, lambda: self.observe(phase), lambda: self.observe('source'),
                    lambda: self.apply('source', fence.command), lambda value: self.save(self.record))
        self.observe('source')
        if Compatibility(self.database()).verify(self.record['sourceProof'], allow_unmigrated=True) != compatible:
            raise ValueError('Original recovery history changed after releasing exclusion')
        pause_record.update(phase='paused', cleanup='complete', catalogSize=compatible['catalogSize'])
        self.save(self.record)
        return self.finish_recovery(compatible)

    def finish_recovery(self, compatible):
        self.observe('source')
        source = self.pause.record['target']
        image = install_images.REGISTRY + '@' + self.pause.record['plan']['target']['images']['migrator']['digest']
        compile_build = lambda request: build_runtime_install.component_call(self.config.provider, image, 'compile-build-installation', request)
        targets = {'staged': copy.deepcopy(source)}
        for name in ('warming', 'armed', 'active'):
            values = initial_activation.phase_values(source['values'], name, compile_build)
            targets[name] = dict(copy.deepcopy(source), values=values, documents=self.config.render(values))
        self.record.update(mode='original-components', completed='staged', transition=None, targets=targets,
                           recoveryProof=copy.deepcopy(self.record['recoveryPause']),
                           recoveryPause=None, recoveredCatalogSize=compatible['catalogSize'])
        self.save(self.record)
        return self.resume()

    def publish_report(self):
        observed, _ = self.observe('active')
        prior = self.pause.installation()
        prior.update(configuration='active', installationReady=True, runtimePaused=False, innerFrozen=False,
            outerFrozen=False, deliveryEnabled=True, issueIntakeEnabled=True, runtimeReadinessValidated=True,
            images=self.describe('active')['images'], diagnosticUpgradeMode=self.record['mode'],
            componentSelection=self.record['selection'] if self.record['mode'] == 'diagnostic' else None,
            upgradeTransitionSHA256=bootstrap.digest(self.record), observedAt=int(time.time() * 1000),
            workflowEvidence=False, autonomousEndToEndValidated=False)
        platform.write_record(self.config.state_dir / 'installation.json', prior)

    def status(self):
        if self.record is None or self.record.get('transition') or self.record.get('recoveryPause'):
            raise ValueError('Diagnostic upgrade has no complete observed phase')
        self.observe(self.record['completed'])
        return {'version': VERSION, 'mode': self.record['mode'], 'phase': self.record['completed'],
                'issueIntakeEnabled': self.record['completed'] == 'active',
                'upgradeOperationWindowValid': time.time() < self.record['expiresAt'],
                'workflowEvidence': False, 'autonomousEndToEndValidated': False}
