"""Install private feedback after a completed, contained schema35 deployment."""
import base64
import copy
import json
import secrets
import time

import bootstrap_database_forward
import bootstrap_prepare as bootstrap
import browser_upgrade_chart
from browser_runtime_upgrade import BrowserUpgrade, BrowserPreflight, PHASES
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
import review_feedback_upgrade_data as compatibility
import review_feedback_upgrade_targets
from runtime_upgrade_pause import no_execution_resources
import upgrade_backup_data as data
from upgrade_fence_client import Fence
from upgrade_migrator import Migrator

VERSION = 'ai-sdlc.review-feedback-runtime-upgrade/v1'


def exact_local_publication(source, revision):
    # A matching SQL catalog is insufficient: an earlier publication can have
    # schema36 but lack this fence protocol. Execute only its exact clean source.
    if (source.git('rev-parse', 'HEAD').decode().strip() != revision
            or source.git('status', '--porcelain', '--untracked-files=all').strip()):
        raise ValueError('Feedback publication requires its exact clean local checkout')


def no_browser_execution(observer, namespace_uid):
    """Original namespace identity brackets one bounded residual-work inventory."""
    for step in (0, 1):
        original = observer.get('namespace', 'ai-sdlc-browser', namespace=None)
        if bootstrap.Preparation.identity(original) != namespace_uid:
            raise ValueError('Original browser namespace changed during feedback maintenance')
        if step == 0:
            resources = observer.get('pods,jobs,cronjobs', namespace='ai-sdlc-browser')
            if not isinstance(resources, dict) or resources.get('items') != []:
                raise ValueError('Browser execution or cleanup resources remain')


class ReviewFeedbackUpgrade(BrowserUpgrade):
    """Reuse the original bounded pause/backup/Helm machinery, never its intent."""
    def __init__(self, config, run=None, revision=None, trust_path=None):
        self.config = config
        self.source_browser = BrowserUpgrade(config)
        prior = self.source_browser.record
        if (prior is None or prior.get('completed') != 'active' or prior.get('transition')
                or prior.get('recoveryPause') or prior.get('mode') != 'browser-evidence'
                or prior.get('migrationVerified') is not True
                or (prior.get('backup') or {}).get('phase') != 'verified'
                or prior['backup'].get('restore', {}).get('state') != 'removed'):
            self.source_browser.close()
            raise ValueError('Original completed schema35 browser installation required')
        self.parent, self.pause = self.source_browser.parent, self.source_browser.pause
        self.parent_hash = bootstrap.digest(prior)
        self.catalog = prior['selection']['catalog']
        self.path = config.state_dir / 'review-feedback-runtime-upgrade.json'
        self.directory = config.state_dir / 'review-feedback-runtime-backup'
        self.source_chart = None
        try:
            self.source_chart = browser_upgrade_chart.OriginalChart(install.ROOT,
                prior['selection']['revision'], prior['chartSHA256'])
            self.target_chart = install.CHART
            if browser_upgrade_chart.digest(self.target_chart) != self.source_chart.expected:
                raise ValueError('Feedback update cannot change the original published chart')
            self.record = files.bounded_json(self.path, 32*1024*1024) if self.path.exists() or self.path.is_symlink() else None
            self.approved_trust = files.bounded_json(trust_path, 128*1024) if trust_path is not None else None
            self.selection = None
            if (run is None) != (revision is None):
                raise ValueError('Both immutable feedback publication inputs are required')
            if self.record is not None:
                if self.record.get('version') != VERSION or self.record.get('parentSHA256') != self.parent_hash:
                    raise ValueError('Original feedback operation custody changed')
                if run is not None and (run != self.record['selection']['run'] or revision != self.record['selection']['revision']):
                    raise ValueError('Existing feedback operation cannot select another publication')
                if self.approved_trust is not None and self.approved_trust != self.record['approvedTrust']:
                    raise ValueError('Original reviewed feedback trust changed')
                self.selection = self.record['selection']
                self.approved_trust = self.record['approvedTrust']
                if browser_upgrade_chart.digest(self.target_chart) != self.record['chartSHA256']:
                    raise ValueError('Original target feedback chart changed')
            elif run is not None:
                if type(run) is not int or not 0 < run <= 2**63-1 or not install_images.SHA.fullmatch(revision):
                    raise ValueError('Explicit exact-main feedback publication required')
                source = install_upgrade.Source(install.ROOT)
                exact_local_publication(source, revision)
                source.git('merge-base', '--is-ancestor', prior['selection']['revision'], revision)
                catalog = source.catalog(revision)
                if (len(self.catalog) != 35 or len(catalog) != 36 or catalog[:35] != self.catalog
                        or catalog[-1]['filename'] != compatibility.MIGRATION):
                    raise ValueError('Only the exact locally reviewed 35-to-36 feedback bridge is supported')
                _, publication = install_images.resolve(install_images.GitHub(), run, revision,
                    include_build=True, include_release=True, include_browser=True)
                self.selection = dict(publication, catalog=catalog)
            self.helper = (install_images.REGISTRY + '@' + self.selection['images']['migrator']['digest']) if self.selection else self.source_browser.helper
            if self.selection is not None:
                exact_local_publication(install_upgrade.Source(install.ROOT), self.selection['revision'])
        except BaseException:
            self.close()
            raise

    def close(self):
        if self.source_chart is not None:
            self.source_chart.close()
        self.source_browser.close()

    def custody(self):
        if browser_upgrade_chart.digest(self.target_chart) != self.source_chart.expected:
            raise ValueError('Feedback update cannot change the original published chart')
        with self.source_chart.selected():
            self.source_browser.custody()
        if bootstrap.digest(files.bounded_json(self.source_browser.path, 32*1024*1024)) != self.parent_hash:
            raise ValueError('Original browser installation custody changed')
        if self.record and (self.record.get('backup') or {}).get('phase') == 'verified':
            data.verify_archives(self.directory, self.record['backup']['archives'])
        if self.record and browser_upgrade_chart.digest(self.target_chart) != self.record['chartSHA256']:
            raise ValueError('Original feedback target chart changed')

    def unchanged(self):
        self.custody()
        observed = files.bounded_json(self.config.state_dir / 'installation.json', 2*1024*1024)
        if self.record is None:
            reports = self.source_browser.record['installationReports']
            if not reports or observed != reports[-1] or observed.get('configuration') != 'active' or observed.get('installationReady') is not True:
                raise ValueError('Original completed browser installation report required')
        elif observed not in [self.record['installationSource']] + self.record.get('installationReports', []):
            raise ValueError('Original installation report changed during feedback update')

    def describe(self, phase):
        if self.record is None:
            return self.source_browser.describe('active')
        return self.record['targets']['paused' if phase in ('ready', 'prepared', 'migrated') else phase]

    def observe(self, phase):
        self.unchanged()
        description = self.describe(phase)
        ids = self.source_browser.record['resourceUIDs'] if self.record is None else self.record['resourceUIDs']
        prepared_resources.observe(self.config, description, ids)
        observer = bootstrap.Preparation(self.config.base_run, self.config.kubectl, self.config.provider,
            self.config.cluster, self.config.state_dir, description['images'])
        paused = phase in ('paused', 'ready', 'prepared', 'migrated', 'staged')
        if paused:
            observer.use_configured_render(description['documents'])
        else:
            observer.use_activation_render(description['documents'], 'active' if phase == 'source' else phase)
        observed, connection = observer.observe(paused=paused)
        prepared_configuration.same_foundation(self.pause.record['sourceObserved'], observed)
        if phase in ('source', 'paused', 'ready', 'prepared', 'migrated'):
            no_execution_resources(observer)
        if phase != 'active':
            profile = json.loads(description['values']['release']['browserProfileJSON'])
            no_browser_execution(observer, profile['namespaceUID'])
        return observed, connection

    def original_database_authority(self, proof=None, database=None):
        database = database or self.database()
        current = proof if proof is not None else self.snapshot(database)
        # Rows may legitimately evolve after activation. Metadata, credentials,
        # role settings/membership and migration timestamps may not. The oracle
        # is the original proven schema35 snapshot, never the new observation.
        original = self.source_browser.record['migratedProof']
        for field in ('@schema', '@roles', '@membership', '@role-settings', 'schema_migrations'):
            if current[field] != original[field]:
                raise ValueError('Original schema35 database authority changed')
        roles = compatibility.Compatibility(database).roles()
        if roles != self.source_browser.record['sourceRoleIdentities']:
            raise ValueError('Original schema35 role identities changed')
        return roles

    def snapshot(self, database=None):
        database = database or self.database()
        compatibility.Compatibility(database).quiet()
        names = json.loads(database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        expected = self.selection['catalog'] if self.record and self.record.get('migrationVerified') else self.catalog
        if names != [x['filename'] for x in expected]:
            raise ValueError('Original feedback schema phase changed')
        return database.snapshot()

    def retained(self, database=None):
        database = database or self.database()
        if self.record.get('migrationVerified'):
            original = dict(self.record['sourceCompatibility'], data=self.retained_proof())
            compatibility.Compatibility(database).verify(original, self.selection['catalog'])
            if self.snapshot(database) != self.record['migratedProof']:
                raise ValueError('Post-migration history changed before feedback activation')
        elif (self.snapshot(database) != self.retained_proof()
                or compatibility.Compatibility(database).roles() != self.record['sourceRoleIdentities']):
            raise ValueError('Original history or authority changed during feedback preparation')

    def fence(self, connection, port):
        catalog = self.selection['catalog'] if self.record.get('migrationVerified') else self.catalog
        return Fence(self.config.provider, self.helper, connection, port, catalog,
                     expires_at=self.record['expiresAt'], review_feedback=True)

    def begin(self):
        if self.record is not None:
            return
        if self.selection is None:
            raise ValueError('Explicit reviewed feedback publication required')
        self.observe('source')
        source = self.source_browser.describe('active')
        compile_build = lambda request: build_runtime_install.component_call(
            self.config.provider, self.helper, 'compile-build-installation', request)
        targets = review_feedback_upgrade_targets.targets(source, self.selection, self.approved_trust,
            self.source_render, self.config.render, compile_build)
        verifier = install_images.REGISTRY + '@' + self.selection['images']['verifier']['digest']
        compiled = build_runtime_install.component_call(self.config.provider, self.helper, 'compile-provenance-installation',
            {'trust': self.approved_trust, 'profile': json.loads(source['values']['build']['configJSON'])['profile'], 'verifierImage': verifier})
        if (set(compiled) != {'version', 'trust', 'trustSHA256', 'profileSHA256', 'verifierImage'}
                or compiled['version'] != 'ai-sdlc.provenance-installation/v1' or compiled['trust'] != self.approved_trust
                or compiled['verifierImage'] != verifier):
            raise ValueError('Reviewed feedback trust compiler changed input')
        proof = compatibility.Compatibility(self.database()).source(self.catalog)
        roles = self.original_database_authority(proof['data'])
        self.observe('source')
        now = time.time()
        self.save({'version': VERSION, 'nonce': secrets.token_hex(16), 'parentSHA256': self.parent_hash,
            'createdAt': now, 'expiresAt': initial_activation.activation_expiry(source['values'], now),
            'chartSHA256': browser_upgrade_chart.digest(self.target_chart), 'sourceChartSHA256': self.source_chart.expected,
            'mode': 'review-feedback', 'completed': 'source', 'transition': None,
            'selection': copy.deepcopy(self.selection), 'approvedTrust': copy.deepcopy(self.approved_trust),
            'approvedTrustSHA256': bootstrap.digest(self.approved_trust), 'targets': targets,
            'sourceProof': proof['data'], 'sourceCompatibility': proof, 'sourceRoleIdentities': roles,
            'resourceUIDs': copy.deepcopy(self.source_browser.record['resourceUIDs']),
            'browserCompiled': copy.deepcopy(self.source_browser.record['browserCompiled']),
            'pause': None, 'backup': None, 'recoveryPause': None, 'migration': {}, 'migrationVerified': False,
            'installationSource': files.bounded_json(self.config.state_dir / 'installation.json', 2*1024*1024),
            'installationReports': []})

    def migrate(self):
        self.deadline()
        observed, connection = self.observe('ready')
        self.retained()
        intent = self.record['migration']
        if intent:
            raise ValueError('Original feedback migration outcome cannot be replayed')
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
        self.observe('ready')
        original = dict(self.record['sourceCompatibility'], data=self.retained_proof())
        self.record['compatibility'] = compatibility.Compatibility(self.database()).verify(original, self.selection['catalog'])
        self.record['migrationVerified'] = True
        self.record['migratedProof'] = self.snapshot()
        self.record['completed'] = 'migrated'
        self.save(self.record)

    def run(self):
        self.begin()
        self.deadline()
        if self.record.get('mode') != 'review-feedback' or self.record.get('recoveryPause'):
            raise ValueError('Original feedback operation required')
        if self.record['completed'] == 'source':
            BrowserPreflight(self).preflight()
            self.pause_current()
            self.record['completed'] = 'paused'
            self.save(self.record)
            self.publish_report(active=False)
        if self.record['completed'] == 'paused':
            self.backup()
            self.record['completed'] = 'ready'
            self.save(self.record)
        if self.record['completed'] == 'ready':
            BrowserPreflight(self).preflight()
            self.migrate()
        for phase in PHASES[PHASES.index(self.record['completed']) + 1:]:
            if self.record.get('transition') is None:
                self.observe(self.record['completed'])
                self.retained()
                BrowserPreflight(self).preflight()
                self.retained()
            advance(self.record, phase, self.observe, self.apply, self.save, phases=PHASES)
        self.publish_report(active=True)
        return self.status()

    def publish_report(self, *, active):
        self.observe('active' if active else 'paused')
        result = copy.deepcopy(self.record['installationSource'])
        result.update(configuration='active' if active else 'review-feedback-upgrade-paused', installationReady=active,
            runtimePaused=not active, innerFrozen=not active, outerFrozen=not active, deliveryEnabled=active,
            issueIntakeEnabled=active, runtimeReadinessValidated=active, reviewFeedbackUpgradeOperation=self.record['nonce'],
            reviewFeedbackUpgradeParentSHA256=self.parent_hash, reviewFeedbackUpgradeSelection=copy.deepcopy(self.selection),
            uniformTargetPublication=False, observedAt=int(time.time()*1000), workflowEvidence=False,
            autonomousEndToEndValidated=False)
        reports = self.record['installationReports']
        if reports and {k: v for k, v in reports[-1].items() if k != 'observedAt'} == {k: v for k, v in result.items() if k != 'observedAt'}:
            result = reports[-1]
        else:
            if len(reports) >= 2:
                raise ValueError('Original feedback report bound exceeded')
            reports.append(result)
            self.save(self.record)
        platform.write_record(self.config.state_dir / 'installation.json', result)

    def status(self):
        if self.record is None or self.record.get('transition'):
            raise ValueError('Complete feedback phase required')
        self.observe(self.record['completed'])
        return {'version': VERSION, 'phase': self.record['completed'],
            'catalogSize': 36 if self.record['migrationVerified'] else 35,
            'issueIntakeEnabled': self.record['completed'] == 'active',
            'administrativeDeadline': int(self.record['expiresAt']*1000),
            'previousWorkflowAuthorityRenewed': False, 'workflowEvidence': False,
            'autonomousEndToEndValidated': False}
