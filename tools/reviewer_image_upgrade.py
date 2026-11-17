"""Replace only a published reviewer image after a contained failed review."""
import copy
import json
import re
import secrets
import time

from approval_window import ApprovalWindow, ApprovedReleasePreflight
import bootstrap_prepare as bootstrap
import build_foundation_install as files
import build_runtime_install
import initial_activation
import install
import install_images
import install_upgrade
from installer_process import capture
import konflux_runtime_install as platform
import prepared_resources
import reviewer_upgrade_history
import reviewer_upgrade_targets
import upgrade_backup_data as data
from upgrade_fence_client import Fence

VERSION = 'ai-sdlc.reviewer-image-upgrade/v1'


def publication(source, api, run, revision, catalog, previous):
    if type(run) is not int or not 0 < run <= 2**63-1 or not isinstance(revision, str) or not install_images.SHA.fullmatch(revision):
        raise ValueError('Explicit publication run and immutable target revision required')
    _, selected = install_images.resolve(api, run, revision, include_build=True, include_release=True)
    source.git('merge-base', '--is-ancestor', previous, revision)
    if source.catalog(revision) != catalog:
        raise ValueError('Reviewer update cannot change the database catalog')
    raw = source.git('show', revision + ':runtime/k8s/components/worker/Containerfile').decode('utf-8')
    pins = re.findall(r'^ARG SCC_ENGINE_PIN=([a-f0-9]{40})$', raw, re.MULTILINE)
    if len(pins) != 1:
        raise ValueError('Published worker must pin one immutable SCC revision')
    return {'revision': revision, 'run': run, 'publication': selected['runURL'],
            'workerImage': install_images.REGISTRY + '@' + selected['images']['worker']['digest'],
            'migratorImage': install_images.REGISTRY + '@' + selected['images']['migrator']['digest'],
            'sccRevision': pins[0], 'uniformTargetPublication': False}


class ReviewerImageUpgrade(ApprovalWindow):
    def __init__(self, config, run=None, revision=None):
        self.config = config
        self.source_approval = ApprovalWindow(config)
        prior = self.source_approval.record
        if (prior is None or prior.get('completed') != 'active' or prior.get('transition')
                or prior.get('mode') != 'approval-window' or prior.get('recoveryPause')
                or (prior.get('backup') or {}).get('phase') != 'verified'
                or prior['backup'].get('restore', {}).get('state') != 'removed'):
            raise ValueError('Original completed approval-window installation required')
        self.parent, self.pause = self.source_approval.parent, self.source_approval.pause
        self.catalog, self.helper = self.source_approval.catalog, self.source_approval.helper
        self.parent_hash = bootstrap.digest(prior)
        self.path = config.state_dir / 'reviewer-image-upgrade.json'
        self.directory = config.state_dir / 'reviewer-image-backup'
        self.record = files.bounded_json(self.path, 32 * 1024 * 1024) if self.path.exists() or self.path.is_symlink() else None
        self.selection = None
        if (run is None) != (revision is None):
            raise ValueError('Both publication run and target revision are required')
        if self.record is not None:
            if self.record.get('version') != VERSION or self.record.get('parentSHA256') != self.parent_hash:
                raise ValueError('Original reviewer image operation custody changed')
            if run is not None and (run != self.record['selection']['run'] or revision != self.record['selection']['revision']):
                raise ValueError('An existing reviewer operation cannot adopt a new publication')
        elif run is not None:
            previous = self.parent.record['selection']['selected']['worker']['revision']
            self.selection = publication(install_upgrade.Source(install.ROOT), install_images.GitHub(),
                                         run, revision, self.catalog, previous)
        selected = self.record['selection'] if self.record else self.selection
        if selected is not None:
            self.helper = selected['migratorImage']

    def fence(self, connection, port):
        return Fence(self.config.provider, self.helper, connection, port, self.catalog,
                     expires_at=self.record['expiresAt'], contained_review=True)

    def custody(self):
        self.source_approval.custody()
        if bootstrap.digest(files.bounded_json(self.source_approval.path, 32 * 1024 * 1024)) != self.parent_hash:
            raise ValueError('Original approval-window activation changed')
        if self.record and (self.record.get('backup') or {}).get('phase') == 'verified':
            data.verify_archives(self.directory, self.record['backup']['archives'])

    def unchanged(self):
        self.custody()
        installed = files.bounded_json(self.config.state_dir / 'installation.json', 2 * 1024 * 1024)
        if self.record is None:
            reports = self.source_approval.record['installationReports']
            if not reports or installed != reports[-1] or installed.get('installationReady') is not True or installed.get('configuration') != 'active':
                raise ValueError('Original complete approved installation snapshot required')
        elif installed not in [self.record['installationSource']] + self.record.get('installationReports', []):
            raise ValueError('Original installation snapshot changed during reviewer update')

    def describe(self, phase):
        if phase == 'ready':
            phase = 'paused'
        return self.source_approval.describe('active') if self.record is None else self.record['targets'][phase]

    def snapshot(self, database=None):
        database = database or self.database()
        reviewer_upgrade_history.quiet(database)
        names = json.loads(database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        if names != [entry['filename'] for entry in self.catalog]:
            raise ValueError('Original reviewer schema catalog changed')
        return database.snapshot()

    def begin(self):
        if self.record is not None:
            return
        if self.selection is None:
            raise ValueError('Explicit independently reviewed reviewer publication required')
        self.observe('source')
        # Public immutable artifact pull, before recording any installation
        # intent. No worker is executed and no workflow credentials are used.
        image = self.selection['workerImage']
        capture([self.config.provider, 'pull', image], timeout=120)
        inspected = json.loads(capture([self.config.provider, 'image', 'inspect', image]))
        if (len(inspected) != 1 or inspected[0].get('Os') != 'linux'
                or inspected[0].get('Architecture') != 'amd64'
                or image not in inspected[0].get('RepoDigests', [])):
            raise ValueError('Published reviewer image identity or platform differs')
        compile_build = lambda request: build_runtime_install.component_call(
            self.config.provider, self.helper, 'compile-build-installation', request)
        targets = reviewer_upgrade_targets.targets(self.source_approval.describe('active'),
            self.selection['workerImage'], self.config.render, compile_build)
        for description in targets.values():
            if set(prepared_resources.documents(description['documents'])) != set(self.pause.record['resourceUIDs']):
                raise ValueError('Reviewer update cannot add or remove resources')
        ApprovedReleasePreflight(self, targets['active']).preflight()
        proof = self.snapshot()
        roles = self.original_database_authority(proof)
        self.observe('source')
        now = time.time()
        self.save({'version': VERSION, 'nonce': secrets.token_hex(16), 'parentSHA256': self.parent_hash,
            'createdAt': now, 'expiresAt': min(now + 1800, initial_activation.activation_expiry(targets['active']['values'], now)),
            'mode': 'reviewer-image', 'completed': 'source', 'transition': None, 'targets': targets,
            'selection': copy.deepcopy(self.selection), 'sourceProof': proof, 'sourceRoleIdentities': roles,
            'pause': None, 'backup': None, 'recoveryPause': None,
            'installationSource': files.bounded_json(self.config.state_dir / 'installation.json', 2 * 1024 * 1024),
            'installationReports': []})

    def run(self):
        self.begin()
        if self.record.get('recoveryPause') or self.record.get('mode') != 'reviewer-image':
            raise ValueError('Original reviewer image operation required')
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

    def publish_report(self, *, active):
        self.observe('active' if active else 'paused')
        result = copy.deepcopy(self.record['installationSource'])
        result.update(configuration='active' if active else 'reviewer-image-paused', installationReady=active,
            runtimePaused=not active, innerFrozen=not active, outerFrozen=not active,
            deliveryEnabled=active, issueIntakeEnabled=active, runtimeReadinessValidated=active,
            reviewerImageOperation=self.record['nonce'], reviewerImageParentSHA256=self.parent_hash,
            reviewerImageSelection=copy.deepcopy(self.record['selection']),
            observedAt=int(time.time() * 1000), workflowEvidence=False, autonomousEndToEndValidated=False)
        reports = self.record['installationReports']
        if reports and {k: v for k, v in reports[-1].items() if k != 'observedAt'} == {k: v for k, v in result.items() if k != 'observedAt'}:
            result = reports[-1]
        else:
            if len(reports) >= 2:
                raise ValueError('Original reviewer report bound exceeded')
            reports.append(result)
            self.save(self.record)
        platform.write_record(self.config.state_dir / 'installation.json', result)

    def status(self):
        if self.record is None or self.record.get('transition'):
            raise ValueError('Reviewer update has no complete observed phase')
        self.observe(self.record['completed'])
        return {'version': VERSION, 'phase': self.record['completed'],
                'selection': self.record['selection'],
                'issueIntakeEnabled': self.record['completed'] == 'active',
                'administrativeDeadline': int(self.record['expiresAt'] * 1000),
                'previousWorkflowAuthorityRenewed': False, 'workflowEvidence': False,
                'autonomousEndToEndValidated': False}
