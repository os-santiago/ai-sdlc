"""A new finite administrative approval above a completed browser35 source."""
import copy
import json
import secrets
import time

import approval_window_targets
import bootstrap_prepare as bootstrap
import browser_approval_state as history
import browser_upgrade_chart
from browser_runtime_upgrade import BrowserPreflight
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
from review_feedback_runtime_upgrade import ReviewFeedbackUpgrade, exact_local_publication, no_browser_execution
from runtime_upgrade_pause import no_execution_resources
import upgrade_backup_data as data
from upgrade_fence_client import Fence

PHASES = ('ready', 'staged', 'warming', 'armed', 'active')


class ApprovalPreflight(BrowserPreflight):
    """Recompile only the target-date binding; the installed profile stays fixed."""
    def __init__(self, upgrade, *, capture_browser=False):
        super().__init__(upgrade)
        self.capture_browser = capture_browser
        self.renewed_browser = None

    def bind_source(self, name, value):
        if name != 'browser-original-profile' or not self.capture_browser:
            return super().bind_source(name, value)
        original = self.original['sources'][name]
        # browser_profile_install has already validated the real compiler's
        # closed output against the unchanged profile and new reviewed target.
        fields = {'version', 'profile', 'profileSHA256', 'targetSHA256'}
        if (not isinstance(value, dict) or set(value) != fields
                or {k:v for k,v in value.items() if k != 'targetSHA256'}
                   != {k:v for k,v in original.items() if k != 'targetSHA256'}
                or value['targetSHA256'] == original['targetSHA256']
                or self.renewed_browser is not None and self.renewed_browser != value):
            raise ValueError('Browser renewal changed the original profile or target binding')
        self.renewed_browser = copy.deepcopy(value)


class BrowserApproval(ReviewFeedbackUpgrade):
    """Reuse real containment/backup/Helm operations, never the old intent."""
    def __init__(self, config, run=None, revision=None, trust_path=None, target_path=None):
        self.config = config
        feedback = config.state_dir / 'review-feedback-runtime-upgrade.json'
        if feedback.exists() or feedback.is_symlink():
            raise ValueError('Feedback installation already owns this browser source')
        if (trust_path is None) != (target_path is None) or (run is None) != (revision is None):
            raise ValueError('Complete explicit approval and helper publication inputs required')
        self.approvals = ({'provenance': files.bounded_json(trust_path, 128*1024),
                           'release': files.bounded_json(target_path, 128*1024)}
                          if trust_path is not None else None)
        # The initial reader accepts no unfinished descendant. Inspect the
        # journal separately to select an exact pending operation for resume.
        from browser_runtime_upgrade import BrowserUpgrade
        original = BrowserUpgrade(config)
        try:
            original_digest = bootstrap.digest(original.record)
            self.journal = history.Journal(config.state_dir, original_digest)
            self.position = self.journal.select(self.approvals)
        finally:
            original.close()
        operations = self.journal.value['operations']
        self.record = copy.deepcopy(operations[self.position]) if self.position < len(operations) else None
        self.source_browser = history.CurrentSource(config, self.position,
            self.record['nonce'] if self.record else None)
        self.source_chart = None
        try:
            self.parent, self.pause = self.source_browser.parent, self.source_browser.pause
            self.parent_hash = self.source_browser.digest()
            prior = self.source_browser.record
            self.catalog = prior['selection']['catalog']
            if len(self.catalog) != 35:
                raise ValueError('Only the original browser schema35 is supported')
            self.source_chart = browser_upgrade_chart.OriginalChart(install.ROOT,
                prior['selection']['revision'], prior['chartSHA256'])
            self.target_chart = install.CHART
            if browser_upgrade_chart.digest(self.target_chart) != self.source_chart.expected:
                raise ValueError('Browser approval cannot change the original chart')
            self.path = self.journal.path
            if self.record is not None:
                if self.record['parentSHA256'] != self.parent_hash:
                    raise ValueError('Original approval source changed')
                self.selection = self.record['helperSelection']
                if run is not None and (run != self.selection['run'] or revision != self.selection['revision']):
                    raise ValueError('Recorded approval helper cannot change')
                self.approvals = self.record['approvals']
                self.directory = config.state_dir / ('browser-approval-backup-' + self.record['nonce'])
            else:
                if run is None or type(run) is not int or not 0 < run <= 2**63-1:
                    raise ValueError('Explicit successful main helper publication required')
                source = install_upgrade.Source(install.ROOT)
                exact_local_publication(source, revision)
                source.git('merge-base', '--is-ancestor', prior['selection']['revision'], revision)
                catalog = source.catalog(revision)
                if (len(catalog) != 36 or catalog[:35] != self.catalog
                        or catalog[-1]['filename'] != compatibility.MIGRATION):
                    raise ValueError('Reviewed schema35-compatible feedback fence required')
                _, selected = install_images.resolve(install_images.GitHub(), run, revision,
                    include_build=True, include_release=True, include_browser=True)
                self.selection = dict(selected, catalog=catalog)
                self.directory = None
            exact_local_publication(install_upgrade.Source(install.ROOT), self.selection['revision'])
            self.helper = install_images.REGISTRY + '@' + self.selection['images']['migrator']['digest']
        except BaseException:
            self.close()
            raise

    def save(self, record):
        self.journal.save(self.position, record)
        self.record = record

    def custody(self):
        feedback = self.config.state_dir / 'review-feedback-runtime-upgrade.json'
        if feedback.exists() or feedback.is_symlink():
            raise ValueError('Feedback installation already owns this browser source')
        if browser_upgrade_chart.digest(self.target_chart) != self.source_chart.expected:
            raise ValueError('Original browser approval chart changed')
        with self.source_chart.selected():
            self.source_browser.custody()
        self.journal.custody()
        if self.source_browser.digest() != self.parent_hash:
            raise ValueError('Original approval source prefix changed')
        if self.record and (self.record.get('backup') or {}).get('phase') == 'verified':
            data.verify_archives(self.directory, self.record['backup']['archives'])

    def unchanged(self):
        self.custody()
        current = files.bounded_json(self.config.state_dir / 'installation.json', 2*1024*1024)
        expected = ([self.record['installationSource']] + self.record['installationReports']
                    if self.record else [self.source_browser.current_report])
        if current not in expected:
            raise ValueError('Original approval installation report changed')

    def describe(self, phase):
        if self.record is None:
            return self.source_browser.describe('active')
        return self.record['targets']['paused' if phase == 'ready' else phase]

    def observe(self, phase):
        self.unchanged()
        description = self.describe(phase)
        identities = self.source_browser.record['resourceUIDs']
        prepared_resources.observe(self.config, description, identities)
        observer = bootstrap.Preparation(self.config.base_run, self.config.kubectl, self.config.provider,
            self.config.cluster, self.config.state_dir, description['images'])
        paused = phase in ('paused', 'ready', 'staged')
        if paused:
            observer.use_configured_render(description['documents'])
        elif phase == 'source':
            # Expired source grants observation/containment only, not readiness.
            observer.use_expired_source_render(description['documents'])
        else:
            observer.use_activation_render(description['documents'], phase)
        observed, connection = observer.observe(paused=paused)
        prepared_configuration.same_foundation(self.pause.record['sourceObserved'], observed)
        if phase in ('source', 'paused', 'ready'):
            no_execution_resources(observer)
        if phase != 'active':
            profile = json.loads(description['values']['release']['browserProfileJSON'])
            no_browser_execution(observer, profile['namespaceUID'])
        return observed, connection

    def fence(self, connection, port):
        return Fence(self.config.provider, self.helper, connection, port, self.catalog,
                     expires_at=self.record['expiresAt'], review_feedback=True)

    def begin(self):
        if self.record is not None:
            return
        if self.approvals is None:
            raise ValueError('Explicit independently reviewed approval documents required')
        self.observe('source')
        source = self.source_browser.describe('active')
        compile_build = lambda request: build_runtime_install.component_call(
            self.config.provider, self.helper, 'compile-build-installation', request)
        targets = approval_window_targets.targets(source, self.approvals,
            self.config.render, compile_build, time.time())
        identities = self.source_browser.record['resourceUIDs']
        if any(set(prepared_resources.documents(target['documents'])) != set(identities)
               for target in targets.values()):
            raise ValueError('Approval cannot add or remove original resources')
        proof = compatibility.Compatibility(self.database()).source(self.catalog)
        roles = self.original_database_authority(proof['data'])
        # Real target validation before the first durable operation intent.
        preview = {'targets': targets, 'browserCompiled': self.source_browser.browser_compiled}
        self.record = preview
        try:
            # The preflight guard must still observe the untouched source.
            preflight = ApprovalPreflight(self, capture_browser=True)
        finally:
            self.record = None
        preflight.preflight()
        if preflight.renewed_browser is None:
            raise ValueError('Actual browser compilation for the new target is required')
        preview['browserCompiled'] = preflight.renewed_browser
        self.observe('source')
        now, nonce = time.time(), secrets.token_hex(16)
        self.source_browser.allowed_nonce = nonce
        self.directory = self.config.state_dir / ('browser-approval-backup-' + nonce)
        self.save({'version': history.OPERATION, 'mode': 'browser-approval', 'nonce': nonce,
            'parentSHA256': self.parent_hash, 'sourceApprovalCount': self.position,
            'createdAt': now, 'expiresAt': initial_activation.activation_expiry(targets['active']['values'], now),
            'completed': 'source', 'transition': None, 'accepted': False,
            'approvals': copy.deepcopy(self.approvals), 'approvalsSHA256': bootstrap.digest(self.approvals),
            'helperSelection': copy.deepcopy(self.selection), 'targets': targets,
            'sourceProof': proof['data'], 'sourceCompatibility': proof, 'sourceRoleIdentities': roles,
            'resourceUIDs': copy.deepcopy(identities), 'browserCompiled': preview['browserCompiled'],
            'migrationVerified': False, 'pause': None, 'backup': None, 'recoveryPause': None,
            'installationSource': self.source_browser.current_report, 'installationReports': []})

    def run(self):
        self.begin()
        if self.record['accepted']:
            self.publish_report(active=True)
            return self.status()
        self.deadline()
        if self.record.get('recoveryPause'):
            raise ValueError('Unresolved approval containment requires review')
        if self.record['completed'] == 'source':
            self.pause_current()
            self.record['completed'] = 'paused'
            self.save(self.record)
            self.publish_report(active=False)
        if self.record['completed'] == 'paused':
            self.backup()
            self.record['completed'] = 'ready'
            self.save(self.record)
        for phase in PHASES[PHASES.index(self.record['completed']) + 1:]:
            if self.record.get('transition') is None:
                self.observe(self.record['completed'])
                self.retained()
                BrowserPreflight(self).preflight()
                self.retained()
            advance(self.record, phase, self.observe, self.apply, self.save, phases=PHASES)
        self.publish_report(active=True)
        return self.status()

    def capture_first_pause(self, database):
        self.observe('source')
        return super().capture_first_pause(database)

    def publish_report(self, *, active):
        self.observe('active' if active else 'paused')
        report = copy.deepcopy(self.record['installationSource'])
        report.update(configuration='active' if active else 'browser-approval-paused',
            installationReady=active, runtimePaused=not active, innerFrozen=not active, outerFrozen=not active,
            deliveryEnabled=active, issueIntakeEnabled=active, runtimeReadinessValidated=active,
            browserApprovalOperation=self.record['nonce'], browserApprovalSourceSHA256=self.parent_hash,
            approvalDocumentsSHA256=self.record['approvalsSHA256'], observedAt=int(time.time()*1000),
            previousWorkflowAuthorityRenewed=False, workflowEvidence=False, autonomousEndToEndValidated=False)
        reports = self.record['installationReports']
        if reports and {k:v for k,v in reports[-1].items() if k != 'observedAt'} == {k:v for k,v in report.items() if k != 'observedAt'}:
            report = reports[-1]
        else:
            if len(reports) >= 2:
                raise ValueError('Original browser approval report bound exceeded')
            reports.append(report)
            self.record['accepted'] = active
            self.save(self.record)
        platform.write_record(self.config.state_dir / 'installation.json', report)

    def status(self):
        if self.record is None or self.record.get('transition'):
            raise ValueError('Complete observed browser approval phase required')
        self.observe(self.record['completed'])
        return {'version': history.OPERATION, 'phase': self.record['completed'],
            'accepted': self.record['accepted'], 'operation': self.record['nonce'],
            'catalogSize': 35, 'approvalsValidUntil': min(v['expiresAt'] for v in self.record['approvals'].values()),
            'administrativeDeadline': int(self.record['expiresAt']*1000),
            'previousWorkflowAuthorityRenewed': False, 'workflowEvidence': False,
            'autonomousEndToEndValidated': False}
