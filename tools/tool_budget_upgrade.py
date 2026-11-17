"""One declared tool-budget transition after a complete diagnostic upgrade."""
import copy
import json
import secrets
import time

import bootstrap_database_forward
import bootstrap_prepare as bootstrap
import build_foundation_install as files
import build_runtime_install
from diagnostic_runtime_upgrade import Upgrade, OriginalReleasePreflight, advance
from diagnostic_upgrade_data import Compatibility
import diagnostic_schema
import initial_activation
import install_images
import konflux_runtime_install as platform
import prepared_configuration
import prepared_resources
from runtime_upgrade_backup import Restore
from runtime_upgrade_pause import pause_once
import tool_budget_targets
import upgrade_backup_data as data
from upgrade_fence_client import Fence

VERSION = 'ai-sdlc.tool-budget-upgrade/v1'
PHASES = ('ready', 'staged', 'warming', 'armed', 'active')


class ToolBudgetUpgrade(Upgrade):
    """Reuse bounded Helm/DB primitives; never change the parent's journal."""
    def __init__(self, config):
        self.config = config
        self.parent = Upgrade(config)
        if (self.parent.record is None or self.parent.record.get('completed') != 'active'
                or self.parent.record.get('transition') or self.parent.record.get('recoveryPause')
                or self.parent.record.get('mode') != 'diagnostic'):
            raise ValueError('Original complete diagnostic activation required')
        self.parent_hash = bootstrap.digest(self.parent.record)
        self.pause = self.parent.pause  # Original release custody for shared preflight.
        self.path = config.state_dir / 'tool-budget-upgrade.json'
        self.directory = config.state_dir / 'tool-budget-backup'
        self.record = files.bounded_json(self.path, 32 * 1024 * 1024) if self.path.exists() or self.path.is_symlink() else None
        self.catalog = self.pause.record['plan']['target']['migrations']
        self.helper = install_images.REGISTRY + '@' + self.parent.record['selection']['selected']['migrator']['digest']
        if len(self.catalog) != 34:
            raise ValueError('Original diagnostic catalog required')
        if self.record is not None and (self.record.get('version') != VERSION
                or self.record.get('parentSHA256') != self.parent_hash):
            raise ValueError('Original tool-budget custody changed')

    def save(self, record):
        platform.write_record(self.path, record)
        self.record = record

    def unchanged(self):
        self.parent.unchanged()
        if bootstrap.digest(files.bounded_json(self.parent.path, 32 * 1024 * 1024)) != self.parent_hash:
            raise ValueError('Original diagnostic activation changed')
        installed = files.bounded_json(self.config.state_dir / 'installation.json', 2 * 1024 * 1024)
        if self.record is None:
            if (installed.get('upgradeTransitionSHA256') != self.parent_hash
                    or installed.get('configuration') != 'active' or installed.get('installationReady') is not True
                    or installed.get('toolBudgetOperation') is not None):
                raise ValueError('Original active installation snapshot required')
        elif installed not in [self.record['installationSource']] + self.record.get('installationReports', []):
            raise ValueError('Original installation snapshot changed during tool-budget update')
        if self.record and (self.record.get('backup') or {}).get('phase') == 'verified':
            data.verify_archives(self.directory, self.record['backup']['archives'])

    def describe(self, phase):
        if phase == 'ready':
            phase = 'paused'
        return self.parent.describe('active') if self.record is None else self.record['targets'][phase]

    def observe(self, phase):
        self.unchanged()
        description = self.describe(phase)
        prepared_resources.observe(self.config, description, self.pause.record['resourceUIDs'])
        observer = bootstrap.Preparation(self.config.base_run, self.config.kubectl, self.config.provider,
            self.config.cluster, self.config.state_dir, description['images'])
        paused = phase in ('paused', 'ready', 'staged')
        if paused:
            observer.use_configured_render(description['documents'])
        else:
            observer.use_activation_render(description['documents'], 'active' if phase == 'source' else phase)
        observed, connection = observer.observe(paused=paused)
        prepared_configuration.same_foundation(self.pause.record['sourceObserved'], observed)
        return observed, connection

    def snapshot(self, database=None):
        database = database or self.database()
        Compatibility(database).quiet()
        names = json.loads(database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        if names != [entry['filename'] for entry in self.catalog]:
            raise ValueError('Original diagnostic schema catalog changed')
        return database.snapshot()

    def retained(self, database=None):
        database = database or self.database()
        if (self.snapshot(database) != self.record['sourceProof']
                or Compatibility(database).roles() != self.record['sourceRoleIdentities']):
            raise ValueError('Original history, schema or roles changed during tool-budget update')

    def compile_targets(self, *, original=False):
        compile_build = lambda request: build_runtime_install.component_call(
            self.config.provider, self.helper, 'compile-build-installation', request)
        return tool_budget_targets.targets(self.parent.describe('active'), self.config.render, compile_build, original=original)

    def original_database_authority(self):
        compatibility = Compatibility(self.database())
        original = self.parent.record['sourceProof']
        for field in ('schema', 'roles', 'permissions', 'tracking'):
            if getattr(compatibility, field)() != original[field]:
                raise ValueError('Original diagnostic database authority changed before tool-budget update')
        compatibility.diagnostic_privileges()
        diagnostic_schema.verify(self.database())
        return original['roles']

    def begin(self):
        if self.record is not None:
            return
        self.observe('source')
        targets = self.compile_targets()
        for description in targets.values():
            if set(prepared_resources.documents(description['documents'])) != set(self.pause.record['resourceUIDs']):
                raise ValueError('Tool budget cannot add or remove resources')
        proof = self.snapshot()
        role_identities = self.original_database_authority()
        self.observe('source')
        now = time.time()
        self.save({'version': VERSION, 'nonce': secrets.token_hex(16), 'parentSHA256': self.parent_hash,
            'createdAt': now, 'expiresAt': initial_activation.activation_expiry(targets['source']['values'], now),
            'mode': 'tool-budget-24', 'completed': 'source', 'transition': None, 'targets': targets,
            'sourceProof': proof, 'sourceRoleIdentities': role_identities,
            'pause': None, 'backup': None, 'recoveryPause': None,
            'installationSource': files.bounded_json(self.config.state_dir / 'installation.json', 2 * 1024 * 1024),
            'installationReports': []})

    def pause_current(self, *, recovery=False):
        self.deadline()
        key = 'recoveryPause' if recovery else 'pause'
        intent = self.record.get(key)
        if intent is not None:
            if intent.get('phase') != 'paused' or intent.get('cleanup') != 'complete':
                raise ValueError('Original pause outcome incomplete; no automatic replay')
            self.observe('paused')
            self.retained()
            return
        phase = self.record['completed']
        observed, connection = self.observe(phase)
        self.retained()
        intent = {'phase': 'planned', 'expiresAt': self.record['expiresAt']}
        with bootstrap_database_forward.original_database(self.config.base_run, self.config.kubectl,
                connection, observed['resources']['pod'], timeout_seconds=240) as (forwarded, port):
            with Fence(self.config.provider, self.helper, forwarded, port, self.catalog,
                       expires_at=self.record['expiresAt']) as fence:
                self.retained(data.Database(fence.command, self.database().prefix))
                intent['fence'] = {'nonce': fence.nonce, 'expiresAt': fence.expiry, 'containerID': fence.container_id}
                self.record[key] = intent
                self.save(self.record)
                pause_once(intent, fence, lambda: self.observe(phase), lambda: self.observe('paused'),
                    lambda: self.apply('paused', fence.command), lambda value: self.save(self.record))
        self.observe('paused')
        self.retained()
        intent.update(phase='paused', cleanup='complete')
        self.save(self.record)

    def fenced_source(self, operation):
        self.deadline()
        observed, connection = self.observe('paused')
        with bootstrap_database_forward.original_database(self.config.base_run, self.config.kubectl,
                connection, observed['resources']['pod'], timeout_seconds=240) as (forwarded, port):
            with Fence(self.config.provider, self.helper, forwarded, port, self.catalog,
                       expires_at=self.record['expiresAt']) as fence:
                self.observe('paused')
                database = data.Database(fence.command, self.database().prefix)
                self.retained(database)
                result = operation(database)
                self.retained(database)
                self.observe('paused')
                fence.check()
        self.observe('paused')
        return result

    def backup(self):
        self.deadline()
        if self.record['backup'] is not None:
            backup = self.record['backup']
            if backup.get('phase') != 'verified' or backup.get('restore', {}).get('state') != 'removed':
                raise ValueError('Original tool-budget backup incomplete; no replay or overwrite')
            data.verify_archives(self.directory, backup['archives'])
            self.retained()
            return
        if self.directory.exists() or self.directory.is_symlink():
            raise ValueError('Unrecorded tool-budget backup cannot be adopted')
        objects = [obj for obj in self.describe('paused')['documents']
                   if obj['kind'] == 'StatefulSet' and obj['metadata']['name'] == 'ai-sdlc-postgres']
        if len(objects) != 1:
            raise ValueError('Original PostgreSQL StatefulSet required')
        containers = objects[0]['spec']['template']['spec']['containers']
        if len(containers) != 1 or containers[0]['name'] != 'postgres':
            raise ValueError('Original PostgreSQL container required')
        backup = {'phase': 'capturing', 'archives': {}, 'retainedData': self.record['sourceProof']}
        self.record['backup'] = backup
        self.save(self.record)
        self.directory.mkdir(mode=0o700)
        def export(database):
            artifacts, proof = database.export()
            if proof != self.record['sourceProof']:
                raise ValueError('Original exported tool-budget source changed')
            backup['archives'] = data.write_archives(self.directory, artifacts)
            self.save(self.record)
        self.fenced_source(export)
        backup['phase'] = 'restoring'
        self.save(self.record)
        restore = Restore(self.config.provider, containers[0]['image'], self.directory, backup,
                          lambda value: self.save(self.record))
        try:
            restore.start()
            restore.execute()
        finally:
            restore.close()
        self.fenced_source(lambda database: None)
        backup.update(phase='verified', verifiedAt=int(time.time() * 1000))
        self.save(self.record)

    def resume(self):
        for phase in PHASES[PHASES.index(self.record['completed']) + 1:]:
            if self.record.get('transition') is None:
                self.observe(self.record['completed'])
                self.retained()
                OriginalReleasePreflight(self).preflight()
                self.retained()
            advance(self.record, phase, self.observe, self.apply, self.save, phases=PHASES)
        self.publish_report(active=True)
        return self.status()

    def run(self):
        self.begin()
        if self.record.get('recoveryPause') is not None or self.record['mode'] != 'tool-budget-24':
            raise ValueError('Original recovery must finish through recover-tool-budget')
        if self.record['completed'] == 'source':
            self.pause_current()
            self.record['completed'] = 'paused'
            self.save(self.record)
            self.publish_report(active=False)
        if self.record['completed'] == 'paused':
            self.backup()
            self.record['completed'] = 'ready'
            self.save(self.record)
        return self.resume()

    def recover(self):
        if self.record is None:
            raise ValueError('Original tool-budget intent required')
        self.deadline()
        if self.record['mode'] == 'original-tools-16':
            return self.resume()
        if self.record.get('transition'):
            advance(self.record, self.record['transition']['to'], self.observe, self.apply, self.save, phases=PHASES)
        backup = self.record.get('backup')
        if backup and backup.get('restore') and backup['restore'].get('state') != 'removed':
            raise ValueError('Original backup helper cleanup unresolved')
        self.pause_current(recovery=True)
        # No database changes occurred. Recovery keeps both failed attempts and
        # returns only the original candidate profile, through the same phases.
        self.record.update(mode='original-tools-16', completed='ready', transition=None,
                           targets=self.compile_targets(original=True))
        self.save(self.record)
        self.publish_report(active=False)
        return self.resume()

    def publish_report(self, *, active):
        self.observe('active' if active else 'paused')
        result = copy.deepcopy(self.record['installationSource'])
        result.update(configuration='active' if active else 'tool-budget-paused', installationReady=active,
            runtimePaused=not active, innerFrozen=not active, outerFrozen=not active,
            deliveryEnabled=active, issueIntakeEnabled=active, runtimeReadinessValidated=active,
            toolBudgetOperation=self.record['nonce'], toolBudgetParentSHA256=self.parent_hash,
            executorToolSteps=(24 if self.record['mode'] == 'tool-budget-24' else 16) if active else 16,
            observedAt=int(time.time() * 1000), workflowEvidence=False, autonomousEndToEndValidated=False)
        reports = self.record['installationReports']
        if reports and {k: v for k, v in reports[-1].items() if k != 'observedAt'} == {k: v for k, v in result.items() if k != 'observedAt'}:
            result = reports[-1]
        else:
            if len(reports) >= 4:
                raise ValueError('Original tool-budget report bound exceeded')
            reports.append(result)
            self.save(self.record)
        platform.write_record(self.config.state_dir / 'installation.json', result)

    def status(self):
        if self.record is None or self.record.get('transition'):
            raise ValueError('Tool-budget update has no complete observed phase')
        self.observe(self.record['completed'])
        return {'version': VERSION, 'phase': self.record['completed'], 'mode': self.record['mode'],
                'issueIntakeEnabled': self.record['completed'] == 'active',
                'originalToolBudget': 16, 'selectedToolBudget': 24 if self.record['mode'] == 'tool-budget-24' else 16,
                'providerQuotasChanged': False, 'workflowEvidence': False, 'autonomousEndToEndValidated': False}
