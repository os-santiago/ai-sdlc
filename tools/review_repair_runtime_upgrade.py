"""Install one bounded review repair above a completed schema36 installation.

The operation also installs a separately reviewed finite approval window: a
completed feedback installation has no other renewal path after its earlier
approvals expire. It never renews historical workflow authority.
"""
import base64
import copy
import json
import secrets
import time

import bootstrap_database_forward
import bootstrap_prepare as bootstrap
import browser_upgrade_chart
from browser_approval_window import ApprovalPreflight
from browser_runtime_upgrade import BrowserPreflight, PHASES
import build_foundation_install as files
import build_runtime_install
from diagnostic_runtime_upgrade import advance
import initial_activation
import install
import install_images
import install_upgrade
import konflux_runtime_install as platform
import prepared_configuration
import prepared_resources
from review_feedback_runtime_upgrade import ReviewFeedbackUpgrade, exact_local_publication, no_browser_execution
from review_feedback_source import CompletedFeedbackSource
import review_repair_rehearsal
import review_repair_upgrade_data as compatibility
import review_repair_upgrade_targets
from runtime_upgrade_backup import Restore
from runtime_upgrade_pause import no_execution_resources
import upgrade_backup_data as data
from upgrade_fence_client import Fence
from upgrade_migrator import Migrator

VERSION = 'ai-sdlc.review-repair-runtime-upgrade/v1'


def rotated(target_values, original_values):
    return (json.loads(target_values['release']['targetJSON'])['tlsCA']
            != json.loads(original_values['release']['targetJSON'])['tlsCA'])


class RepairBrowserPreflight(BrowserPreflight):
    """Later phases bind the identities recorded by the capture preflight."""
    def __init__(self, upgrade):
        super().__init__(upgrade)
        if upgrade.record.get('releaseIdentities') is not None:
            self.original['sources']['release-original-identities'] = upgrade.record['releaseIdentities']


class RepairApprovalPreflight(ApprovalPreflight):
    """Like the browser approval binding, but a rotated CA may change only the
    profile's CA digest/object and the console TLS Secret content digest."""
    renewed_identities = None

    def bind_source(self, name, value):
        if name == 'release-original-identities' and self.capture_browser:
            original = self.original['sources'][name]
            changed = {field for field in set(original) | set(value) if original.get(field) != value.get(field)}
            if (not isinstance(value, dict) or set(value) != set(original)
                    or changed - {'tlsSecretUID'}
                    or value['tlsSecretUID'].get('uid') != original['tlsSecretUID'].get('uid')
                    or set(value['tlsSecretUID']) != set(original['tlsSecretUID'])
                    or bool(changed) != rotated(self.original['target']['values'], self.upgrade.source_browser.describe('active')['values'])
                    or self.renewed_identities is not None and self.renewed_identities != value):
                raise ValueError('Repair renewal changed release identities beyond the rotated TLS content')
            self.renewed_identities = copy.deepcopy(value)
            return
        if name != 'browser-original-profile' or not self.capture_browser:
            return super().bind_source(name, value)
        original = self.original['sources'][name]
        profile = review_repair_upgrade_targets.expected_compiled_profile(original['profile'],
                                                                         self.original['target']['values'])
        if (not isinstance(value, dict) or set(value) != {'version', 'profile', 'profileSHA256', 'targetSHA256'}
                or value['version'] != original['version'] or value['profile'] != profile
                or (profile == original['profile']) != (value['profileSHA256'] == original['profileSHA256'])
                or value['targetSHA256'] == original['targetSHA256']
                or self.expected_target_digest is None or value['targetSHA256'] != self.expected_target_digest
                or self.renewed_browser is not None and self.renewed_browser != value):
            raise ValueError('Repair renewal changed the browser profile or target binding')
        self.renewed_browser = copy.deepcopy(value)


class ReviewRepairUpgrade(ReviewFeedbackUpgrade):
    """Reuse bounded pause/backup/Helm machinery; never the feedback intent."""
    def __init__(self, config, run=None, revision=None, trust_path=None, target_path=None):
        self.config = config
        self.source_chart = None
        self.source_browser = CompletedFeedbackSource(config)
        try:
            prior = self.source_browser.record
            self.parent, self.pause = self.source_browser.parent, self.source_browser.pause
            self.parent_hash = self.source_browser.digest()
            self.catalog = prior['selection']['catalog']
            self.path = config.state_dir / 'review-repair-runtime-upgrade.json'
            self.directory = config.state_dir / 'review-repair-runtime-backup'
            self.source_chart = browser_upgrade_chart.OriginalChart(install.ROOT,
                prior['selection']['revision'], prior['chartSHA256'])
            self.target_chart = install.CHART
            self.record = files.bounded_json(self.path, 32*1024*1024) if self.path.exists() or self.path.is_symlink() else None
            if (trust_path is None) != (target_path is None) or (run is None) != (revision is None):
                raise ValueError('Complete explicit approval and repair publication inputs required')
            self.approvals = ({'provenance': files.bounded_json(trust_path, 128*1024),
                               'release': files.bounded_json(target_path, 128*1024)}
                              if trust_path is not None else None)
            self.selection = None
            if self.record is not None:
                if self.record.get('version') != VERSION or self.record.get('parentSHA256') != self.parent_hash:
                    raise ValueError('Original repair operation custody changed')
                if run is not None and (run != self.record['selection']['run'] or revision != self.record['selection']['revision']):
                    raise ValueError('Existing repair operation cannot select another publication')
                if self.approvals is not None and self.approvals != self.record['approvals']:
                    raise ValueError('Original reviewed repair approvals changed')
                if browser_upgrade_chart.digest(self.target_chart) != self.record['chartSHA256']:
                    raise ValueError('Original target repair chart changed')
                self.selection = self.record['selection']
                self.approvals = self.record['approvals']
            elif run is not None:
                if type(run) is not int or not 0 < run <= 2**63-1 or not install_images.SHA.fullmatch(revision):
                    raise ValueError('Explicit exact-main repair publication required')
                source = install_upgrade.Source(install.ROOT)
                exact_local_publication(source, revision)
                source.git('merge-base', '--is-ancestor', prior['selection']['revision'], revision)
                catalog = source.catalog(revision)
                if (len(self.catalog) != 36 or len(catalog) != 37 or catalog[:36] != self.catalog
                        or catalog[-1]['filename'] != compatibility.MIGRATION):
                    raise ValueError('Only the exact locally reviewed 36-to-37 repair bridge is supported')
                _, publication = install_images.resolve(install_images.GitHub(), run, revision,
                    include_build=True, include_release=True, include_browser=True)
                self.selection = dict(publication, catalog=catalog)
            self.helper = (install_images.REGISTRY + '@' + self.selection['images']['migrator']['digest']) if self.selection else self.source_browser.helper
            if self.selection is not None:
                exact_local_publication(install_upgrade.Source(install.ROOT), self.selection['revision'])
        except BaseException:
            self.close()
            raise

    # The feedback source is immutable: only the newer target chart changes.
    def custody(self):
        self.source_browser.custody()
        if self.record and (self.record.get('backup') or {}).get('phase') == 'verified':
            data.verify_archives(self.directory, self.record['backup']['archives'])
        if self.record and browser_upgrade_chart.digest(self.target_chart) != self.record['chartSHA256']:
            raise ValueError('Original repair target chart changed')

    def unchanged(self):
        self.custody()
        observed = files.bounded_json(self.config.state_dir / 'installation.json', 2*1024*1024)
        expected = ([self.record['installationSource']] + self.record['installationReports']
                    if self.record else [self.source_browser.current_report])
        if observed not in expected:
            raise ValueError('Original installation report changed during repair update')

    def describe(self, phase):
        if self.record is None:
            return self.source_browser.describe('active')
        return self.record['targets']['paused' if phase in ('ready', 'prepared', 'migrated') else phase]

    def observe(self, phase):
        self.unchanged()
        description = self.describe(phase)
        prepared_resources.observe(self.config, description, self.source_browser.record['resourceUIDs'])
        observer = bootstrap.Preparation(self.config.base_run, self.config.kubectl, self.config.provider,
            self.config.cluster, self.config.state_dir, description['images'])
        paused = phase in ('paused', 'ready', 'prepared', 'migrated', 'staged')
        if paused:
            observer.use_configured_render(description['documents'])
        elif phase == 'source' and self.source_approvals_expired(description):
            # Expired approvals grant observation and containment only.
            observer.use_expired_source_render(description['documents'])
        elif phase == 'source':
            observer.use_activation_render(description['documents'], 'active')
        else:
            observer.use_activation_render(description['documents'], phase)
        observed, connection = observer.observe(paused=paused)
        prepared_configuration.same_foundation(self.pause.record['sourceObserved'], observed)
        if phase in ('source', 'paused', 'ready', 'prepared', 'migrated'):
            no_execution_resources(observer)
        if phase != 'active':
            profile = json.loads(description['values']['release']['browserProfileJSON'])
            no_browser_execution(observer, profile['namespaceUID'])
        return observed, connection

    @staticmethod
    def source_approvals_expired(description, now=None):
        values = description['values']
        now = time.time() if now is None else now
        expiries = [json.loads(values[section][key])['expiresAt']
                    for section, key in (('provenance', 'trustJSON'), ('release', 'targetJSON'))]
        if any(type(value) is not int for value in expiries):
            raise ValueError('Explicit original approval expiry required')
        return min(expiries) <= now * 1000

    def original_database_authority(self, proof=None, database=None):
        database = database or self.database()
        current = proof if proof is not None else self.snapshot(database)
        # Rows evolve after activation. Metadata, credentials, role settings,
        # membership and migration tracking may not: the oracle is the original
        # verified schema36 snapshot, never the new observation.
        original = self.source_browser.record['migratedProof']
        for field in ('@schema', '@roles', '@membership', '@role-settings', 'schema_migrations'):
            if current[field] != original[field]:
                raise ValueError('Original schema36 database authority changed')
        roles = compatibility.Compatibility(database).roles()
        if roles != self.source_browser.record['sourceRoleIdentities']:
            raise ValueError('Original schema36 role identities changed')
        return roles

    def snapshot(self, database=None):
        database = database or self.database()
        compatibility.Compatibility(database).quiet()
        names = json.loads(database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        expected = self.selection['catalog'] if self.record and self.record.get('migrationVerified') else self.catalog
        if names != [x['filename'] for x in expected]:
            raise ValueError('Original repair schema phase changed')
        return database.snapshot()

    def retained(self, database=None):
        database = database or self.database()
        if self.record.get('migrationVerified'):
            compatibility.Compatibility(database).verify(self.paused_source(),
                self.record['rehearsal']['expected'], self.selection['catalog'])
            if self.snapshot(database) != self.record['migratedProof']:
                raise ValueError('Post-migration history changed before repair activation')
        elif (self.snapshot(database) != self.retained_proof()
                or compatibility.Compatibility(database).roles() != self.record['sourceRoleIdentities']):
            raise ValueError('Original history or authority changed during repair preparation')

    def paused_source(self):
        # Rows may evolve before the fenced pause; the oracle is the paused proof.
        return dict(self.record['sourceCompatibility'], data=self.retained_proof())

    def fence(self, connection, port):
        catalog = self.selection['catalog'] if self.record.get('migrationVerified') else self.catalog
        return Fence(self.config.provider, self.helper, connection, port, catalog,
                     expires_at=self.record['expiresAt'], review_repair=True)

    def begin(self):
        if self.record is not None:
            return
        if self.selection is None or self.approvals is None:
            raise ValueError('Explicit reviewed repair publication and approval documents required')
        self.observe('source')
        source = self.source_browser.describe('active')
        compile_build = lambda request: build_runtime_install.component_call(
            self.config.provider, self.helper, 'compile-build-installation', request)
        browser_ca = self.config.read('configmap', 'approved-console-ca', 'ai-sdlc-browser')
        if (not isinstance(browser_ca, dict) or browser_ca.get('immutable') is not True
                or browser_ca.get('data') != {'ca.crt': self.approvals['release']['tlsCA']}):
            raise ValueError('Browser platform CA must be an immutable object holding exactly the reviewed CA')
        targets = review_repair_upgrade_targets.targets(source, self.selection, self.approvals,
            self.source_render, self.config.render, compile_build, time.time(), browser_ca['metadata']['uid'])
        identities = self.source_browser.record['resourceUIDs']
        if any(set(prepared_resources.documents(t['documents'])) != set(identities) for t in targets.values()):
            raise ValueError('Repair cannot add or remove original resources')
        verifier = install_images.REGISTRY + '@' + self.selection['images']['verifier']['digest']
        compiled = build_runtime_install.component_call(self.config.provider, self.helper, 'compile-provenance-installation',
            {'trust': self.approvals['provenance'], 'profile': json.loads(source['values']['build']['configJSON'])['profile'],
             'verifierImage': verifier})
        if (set(compiled) != {'version', 'trust', 'trustSHA256', 'profileSHA256', 'verifierImage'}
                or compiled['version'] != 'ai-sdlc.provenance-installation/v1'
                or compiled['trust'] != self.approvals['provenance'] or compiled['verifierImage'] != verifier):
            raise ValueError('Reviewed repair trust compiler changed input')
        release = self.approvals['release']
        installed_ca = self.config.read('configmap', 'saferus-ca', release['namespace'])
        if (not isinstance(installed_ca, dict) or installed_ca.get('data', {}).get('ca.crt') != release['tlsCA']):
            raise ValueError('Reviewed release CA differs from the installed application CA')
        proof = compatibility.Compatibility(self.database()).source(self.catalog)
        roles = self.original_database_authority(proof['data'])
        # Bind the browser profile to the new reviewed release target before
        # the first durable intent; the profile itself must stay identical.
        preview = {'targets': targets, 'browserCompiled': self.source_browser.browser_compiled}
        self.record = preview
        try:
            preflight = RepairApprovalPreflight(self, capture_browser=True)
        finally:
            self.record = None
        preflight.preflight()
        if preflight.renewed_browser is None or preflight.renewed_identities is None:
            raise ValueError('Actual browser and release compilation for the new target is required')
        self.observe('source')
        now = time.time()
        self.save({'version': VERSION, 'nonce': secrets.token_hex(16), 'parentSHA256': self.parent_hash,
            'createdAt': now, 'expiresAt': initial_activation.activation_expiry(targets['active']['values'], now),
            'chartSHA256': browser_upgrade_chart.digest(self.target_chart), 'sourceChartSHA256': self.source_chart.expected,
            'mode': 'review-repair', 'completed': 'source', 'transition': None,
            'selection': copy.deepcopy(self.selection), 'approvals': copy.deepcopy(self.approvals),
            'approvalsSHA256': bootstrap.digest(self.approvals), 'targets': targets,
            'sourceProof': proof['data'], 'sourceCompatibility': proof, 'sourceRoleIdentities': roles,
            'resourceUIDs': copy.deepcopy(identities), 'browserCompiled': preflight.renewed_browser,
            'releaseIdentities': preflight.renewed_identities,
            'pause': None, 'backup': None, 'rehearsal': {}, 'recoveryPause': None, 'migration': {},
            'migrationVerified': False, 'installationSource': self.source_browser.current_report,
            'installationReports': []})

    def capture_first_pause(self, database):
        self.observe('source')
        return super().capture_first_pause(database)

    def backup(self):
        """Restore the fenced export, rehearse the exact migration there, then remove it."""
        self.deadline()
        backup = self.record['backup']
        if backup is not None:
            if (backup.get('phase') != 'verified' or backup.get('restore', {}).get('state') != 'removed'
                    or self.record['rehearsal'].get('state') != 'completed'
                    or self.record['rehearsal'].get('cleanup') != 'removed'):
                raise ValueError('Original repair backup or rehearsal incomplete; no replay or overwrite')
            data.verify_archives(self.directory, backup['archives'])
            self.retained()
            return
        if self.directory.exists() or self.directory.is_symlink():
            raise ValueError('Unrecorded repair backup cannot be adopted')
        objects = [obj for obj in self.describe('paused')['documents']
                   if obj['kind'] == 'StatefulSet' and obj['metadata']['name'] == 'ai-sdlc-postgres']
        if len(objects) != 1 or [c['name'] for c in objects[0]['spec']['template']['spec']['containers']] != ['postgres']:
            raise ValueError('Original PostgreSQL StatefulSet required')
        image = objects[0]['spec']['template']['spec']['containers'][0]['image']
        secret = self.config.read('secret', 'ai-sdlc-database-roles')
        if set(secret.get('data', {})) != {'passwords.json'}:
            raise ValueError('Original role credentials required')
        passwords = json.loads(base64.b64decode(secret['data']['passwords.json'], validate=True))
        connection = self.config.read('secret', 'ai-sdlc-postgres-app')
        url = base64.b64decode(connection['data']['uri'], validate=True).decode()
        backup = {'phase': 'capturing', 'archives': {}, 'retainedData': self.retained_proof()}
        self.record['backup'] = backup
        self.save(self.record)
        self.directory.mkdir(mode=0o700)
        def export(database):
            artifacts, proof = database.export()
            if proof != self.retained_proof():
                raise ValueError('Original exported repair source changed')
            backup['archives'] = data.write_archives(self.directory, artifacts)
            self.save(self.record)
        self.fenced_source(export)
        backup['phase'] = 'restoring'
        self.save(self.record)
        restore = Restore(self.config.provider, image, self.directory, backup, lambda value: self.save(self.record))
        rehearsal = None
        try:
            restore.start()
            restore.execute()
            rehearsal = review_repair_rehearsal.Rehearsal(restore, self.helper, url, passwords,
                self.record['rehearsal'], lambda: self.save(self.record), self.record['expiresAt'],
                lambda: self.observe('paused'))
            rehearsal.run(self.paused_source(), self.selection['catalog'])
        finally:
            try:
                if rehearsal is not None:
                    rehearsal.close()
            finally:
                restore.close()
        self.fenced_source(lambda database: None)
        backup.update(phase='verified', verifiedAt=int(time.time() * 1000))
        self.save(self.record)

    def migrate(self):
        self.deadline()
        observed, connection = self.observe('ready')
        self.retained()
        intent = self.record['migration']
        if intent and (intent.get('state') != 'completed' or intent.get('cleanup') != 'removed'):
            raise ValueError('Original repair migration outcome cannot be replayed')
        if not intent:
            secret = self.config.read('secret', 'ai-sdlc-database-roles')
            if set(secret.get('data', {})) != {'passwords.json'}:
                raise ValueError('Original role credentials required')
            passwords = json.loads(base64.b64decode(secret['data']['passwords.json'], validate=True))
            with bootstrap_database_forward.original_database(self.config.base_run, self.config.kubectl,
                    connection, observed['resources']['pod'], timeout_seconds=240) as (forwarded, port):
                helper = Migrator(self.config.provider, self.helper, forwarded, port, passwords, intent,
                    lambda _: self.save(self.record), expires_at=self.record['expiresAt'], guard=lambda: self.observe('ready'))
                try:
                    helper.run()
                finally:
                    helper.close()
        # A completed, cleaned migration whose verification failed is only
        # re-verified; the migrator is never invoked again.
        self.observe('ready')
        self.record['compatibility'] = compatibility.Compatibility(self.database()).verify(
            self.paused_source(), self.record['rehearsal']['expected'], self.selection['catalog'])
        self.record['migrationVerified'] = True
        self.record['migratedProof'] = self.snapshot()
        self.record['completed'] = 'migrated'
        self.save(self.record)

    def run(self):
        self.begin()
        self.deadline()
        if self.record.get('mode') != 'review-repair' or self.record.get('recoveryPause'):
            raise ValueError('Original repair operation required')
        if self.record['completed'] == 'source':
            self.pause_current()
            self.record['completed'] = 'paused'
            self.save(self.record)
            self.publish_report(active=False)
        if self.record['completed'] == 'paused':
            self.backup()
            self.record['completed'] = 'ready'
            self.save(self.record)
        if self.record['completed'] == 'ready':
            RepairBrowserPreflight(self).preflight()
            self.migrate()
        for phase in PHASES[PHASES.index(self.record['completed']) + 1:]:
            if self.record.get('transition') is None:
                self.observe(self.record['completed'])
                self.retained()
                RepairBrowserPreflight(self).preflight()
                self.retained()
            advance(self.record, phase, self.observe, self.apply, self.save, phases=PHASES)
        self.publish_report(active=True)
        return self.status()

    def publish_report(self, *, active):
        self.observe('active' if active else 'paused')
        result = copy.deepcopy(self.record['installationSource'])
        result.update(configuration='active' if active else 'review-repair-upgrade-paused', installationReady=active,
            runtimePaused=not active, innerFrozen=not active, outerFrozen=not active, deliveryEnabled=active,
            issueIntakeEnabled=active, runtimeReadinessValidated=active, reviewRepairUpgradeOperation=self.record['nonce'],
            reviewRepairUpgradeParentSHA256=self.parent_hash, reviewRepairUpgradeSelection=copy.deepcopy(self.selection),
            approvalDocumentsSHA256=self.record['approvalsSHA256'], uniformTargetPublication=False,
            observedAt=int(time.time()*1000), previousWorkflowAuthorityRenewed=False, workflowEvidence=False,
            autonomousEndToEndValidated=False)
        reports = self.record['installationReports']
        if reports and {k: v for k, v in reports[-1].items() if k != 'observedAt'} == {k: v for k, v in result.items() if k != 'observedAt'}:
            result = reports[-1]
        else:
            if len(reports) >= 2:
                raise ValueError('Original repair report bound exceeded')
            reports.append(result)
            self.save(self.record)
        platform.write_record(self.config.state_dir / 'installation.json', result)

    def status(self):
        if self.record is None or self.record.get('transition'):
            raise ValueError('Complete repair phase required')
        self.observe(self.record['completed'])
        return {'version': VERSION, 'phase': self.record['completed'],
            'catalogSize': 37 if self.record['migrationVerified'] else 36,
            'issueIntakeEnabled': self.record['completed'] == 'active',
            'approvalsValidUntil': min(v['expiresAt'] for v in self.record['approvals'].values()),
            'administrativeDeadline': int(self.record['expiresAt']*1000),
            'previousWorkflowAuthorityRenewed': False, 'workflowEvidence': False,
            'autonomousEndToEndValidated': False}
