"""Install separately approved finite windows without renewing workflow history."""
import copy
import json
import secrets
import time

import approval_window_targets
import bootstrap_prepare as bootstrap
import build_foundation_install as files
import build_runtime_install
from diagnostic_runtime_upgrade import OriginalReleasePreflight, advance
import initial_activation
import konflux_runtime_install as platform
import prepared_configuration
import prepared_resources
from tool_budget_upgrade import ToolBudgetUpgrade, PHASES
from runtime_upgrade_pause import no_execution_resources
import upgrade_backup_data as data

VERSION = 'ai-sdlc.approval-window/v1'


class ApprovedReleasePreflight(OriginalReleasePreflight):
    def __init__(self, upgrade, target):
        super().__init__(upgrade)
        self.original = copy.deepcopy(self.original)
        self.original['target'] = target

    def guard(self):
        return self.upgrade.observe(self.upgrade.record['completed'] if self.upgrade.record else 'source')[0]

    def preflight(self):
        approval_window_targets.registry_ca_window(
            json.loads(self.original['target']['values']['provenance']['trustJSON']), time.time())
        return super().preflight()


class ApprovalWindow(ToolBudgetUpgrade):
    """One new approval operation after a completed reviewed 24-tool update."""
    def __init__(self, config, trust_path=None, target_path=None):
        self.config = config
        self.source_budget = ToolBudgetUpgrade(config)
        prior = self.source_budget.record
        if (prior is None or prior.get('completed') != 'active' or prior.get('transition')
                or prior.get('mode') != 'tool-budget-24' or prior.get('recoveryPause')
                or (prior.get('backup') or {}).get('phase') != 'verified'
                or prior['backup'].get('restore', {}).get('state') != 'removed'):
            raise ValueError('Original completed 24-tool installation required; no expired intent renewal')
        self.parent = self.source_budget.parent
        self.pause = self.source_budget.pause
        self.parent_hash = bootstrap.digest(prior)
        self.catalog, self.helper = self.source_budget.catalog, self.source_budget.helper
        self.path = config.state_dir / 'approval-window.json'
        self.directory = config.state_dir / 'approval-window-backup'
        self.record = files.bounded_json(self.path, 32 * 1024 * 1024) if self.path.exists() or self.path.is_symlink() else None
        self.approvals = None
        if (trust_path is None) != (target_path is None):
            raise ValueError('Both separately approved documents are required')
        if trust_path is not None:
            self.approvals = {'provenance': files.bounded_json(trust_path, 128 * 1024),
                              'release': files.bounded_json(target_path, 128 * 1024)}
        if self.record is not None:
            if self.record.get('version') != VERSION or self.record.get('parentSHA256') != self.parent_hash:
                raise ValueError('Original approval operation custody changed')
            if self.approvals is not None and self.approvals != self.record['approvals']:
                raise ValueError('An existing operation cannot adopt a new approval window')

    def unchanged(self):
        self.source_budget.custody()
        if bootstrap.digest(files.bounded_json(self.source_budget.path, 32 * 1024 * 1024)) != self.parent_hash:
            raise ValueError('Original tool-budget activation changed')
        installed = files.bounded_json(self.config.state_dir / 'installation.json', 2 * 1024 * 1024)
        if self.record is None:
            prior = self.source_budget.record['installationReports']
            if (not prior or installed != prior[-1] or installed.get('installationReady') is not True
                    or installed.get('configuration') != 'active' or installed.get('executorToolSteps') != 24):
                raise ValueError('Original complete installation snapshot required')
        elif installed not in [self.record['installationSource']] + self.record.get('installationReports', []):
            raise ValueError('Original installation snapshot changed during approval update')
        if self.record and (self.record.get('backup') or {}).get('phase') == 'verified':
            data.verify_archives(self.directory, self.record['backup']['archives'])

    def describe(self, phase):
        if phase == 'ready':
            phase = 'paused'
        return self.source_budget.describe('active') if self.record is None else self.record['targets'][phase]

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
            values = description['values']
            expired_source = phase == 'source' and any(
                json.loads(values[section][key])['expiresAt'] <= time.time() * 1000
                for section, key in (('provenance', 'trustJSON'), ('release', 'targetJSON')))
            if expired_source:
                observer.use_expired_source_render(description['documents'])
            else:
                observer.use_activation_render(description['documents'], 'active' if phase == 'source' else phase)
        observed, connection = observer.observe(paused=paused)
        if phase == 'source':
            no_execution_resources(observer)
        prepared_configuration.same_foundation(self.pause.record['sourceObserved'], observed)
        return observed, connection

    def capture_first_pause(self, database):
        # Called by the shared pause routine while its write fence is held.
        self.observe('source')
        return super().capture_first_pause(database)

    def begin(self):
        if self.record is not None:
            return
        if self.approvals is None:
            raise ValueError('Explicit independently reviewed approval files required')
        self.observe('source')
        compile_build = lambda request: build_runtime_install.component_call(
            self.config.provider, self.helper, 'compile-build-installation', request)
        targets = approval_window_targets.targets(self.source_budget.describe('active'), self.approvals,
            self.config.render, compile_build, time.time())
        for description in targets.values():
            if set(prepared_resources.documents(description['documents'])) != set(self.pause.record['resourceUIDs']):
                raise ValueError('Approval update cannot add or remove resources')
        # Compilers validate both new approvals against the original current
        # signer/registry/application custody before the first durable intent.
        ApprovedReleasePreflight(self, targets['active']).preflight()
        proof = self.snapshot()
        role_identities = self.original_database_authority(proof)
        self.observe('source')
        now = time.time()
        self.save({'version': VERSION, 'nonce': secrets.token_hex(16), 'parentSHA256': self.parent_hash,
            'createdAt': now, 'expiresAt': initial_activation.activation_expiry(targets['active']['values'], now),
            'mode': 'approval-window', 'completed': 'source', 'transition': None, 'targets': targets,
            'approvals': copy.deepcopy(self.approvals), 'sourceProof': proof, 'sourceRoleIdentities': role_identities,
            'pause': None, 'backup': None, 'recoveryPause': None,
            'installationSource': files.bounded_json(self.config.state_dir / 'installation.json', 2 * 1024 * 1024),
            'installationReports': []})

    def resume(self):
        for phase in PHASES[PHASES.index(self.record['completed']) + 1:]:
            if self.record.get('transition') is None:
                self.observe(self.record['completed'])
                self.retained()
                ApprovedReleasePreflight(self, self.record['targets']['active']).preflight()
                self.retained()
            advance(self.record, phase, self.observe, self.apply, self.save, phases=PHASES)
        self.publish_report(active=True)
        return self.status()

    def run(self):
        self.begin()
        if self.record.get('recoveryPause') or self.record.get('mode') != 'approval-window':
            raise ValueError('Original approval operation required')
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
        result.update(configuration='active' if active else 'approval-window-paused', installationReady=active,
            runtimePaused=not active, innerFrozen=not active, outerFrozen=not active,
            deliveryEnabled=active, issueIntakeEnabled=active, runtimeReadinessValidated=active,
            approvalWindowOperation=self.record['nonce'], approvalWindowParentSHA256=self.parent_hash,
            approvalDocumentsSHA256=bootstrap.digest(self.record['approvals']),
            observedAt=int(time.time() * 1000), workflowEvidence=False, autonomousEndToEndValidated=False)
        reports = self.record['installationReports']
        if reports and {k: v for k, v in reports[-1].items() if k != 'observedAt'} == {k: v for k, v in result.items() if k != 'observedAt'}:
            result = reports[-1]
        else:
            if len(reports) >= 2:
                raise ValueError('Original approval report bound exceeded')
            reports.append(result)
            self.save(self.record)
        platform.write_record(self.config.state_dir / 'installation.json', result)

    def status(self):
        if self.record is None or self.record.get('transition'):
            raise ValueError('Approval update has no complete observed phase')
        self.observe(self.record['completed'])
        return {'version': VERSION, 'phase': self.record['completed'],
                'issueIntakeEnabled': self.record['completed'] == 'active',
                'approvalsValidUntil': min(value['expiresAt'] for value in self.record['approvals'].values()),
                'administrativeDeadline': int(self.record['expiresAt'] * 1000),
                'previousWorkflowAuthorityRenewed': False, 'workflowEvidence': False,
                'autonomousEndToEndValidated': False}
