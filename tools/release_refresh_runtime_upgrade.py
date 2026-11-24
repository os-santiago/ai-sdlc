"""Install a newer exact-main publication into a completed model-upstream
installation, as a uniform same-schema publication with no configuration
change.

Every runtime component image (orchestrator, intake, admission, worker,
mailbox, relay, release-manager, verifier, migrator, build-controller and
deployer) comes from the selected main publication (a descendant of the
installed one, with the identical schema catalog). The browser-verifier and
artifact-builder images stay as pinned by the reviewed release and build
profiles, exactly as in the model-upstream switch. Separately
reviewed provenance (new verifier image) and release (new finite window, same
CA) documents are installed. The model upstream, per-call windows, authority
policy, build, budgets, scope, resource identities and release identities are
inherited from the completed model-upstream operation. Historical workflow
authority is never renewed.
"""
import copy
import time

import browser_upgrade_chart
import bootstrap_prepare as bootstrap
import build_foundation_install as files
import install
import install_images
import install_upgrade
import model_upstream_runtime_upgrade as upstream_upgrade
from model_upstream_runtime_upgrade import (CompletedRepairSource, ModelUpstreamUpgrade, gateway_values, phase_targets,
                                            publication, publication_lock, recorded_answer_tokens, validate_upstream,
                                            without_publication)
from review_feedback_runtime_upgrade import exact_local_publication
import upgrade_backup_data as data

VERSION = 'ai-sdlc.release-refresh-runtime-upgrade/v1'


def targets(source, selection, upstream, approvals, render_source, render_target, compile_build, now, browser_ca_uid):
    """Images and renewed approvals only: the installed model upstream and
    every other installation value are unchanged (closed delta)."""
    upstream = validate_upstream(upstream)
    if source['values'].get('modelGateway') != gateway_values(upstream, recorded_answer_tokens(source['values'])):
        raise ValueError('Release refresh requires the exact recorded model upstream')
    values, active = publication(source, selection, approvals, render_source, render_target, now, browser_ca_uid,
                                 'model upstream')
    if without_publication(active, values) != values:
        raise ValueError('Release refresh changed unrelated installation values')
    return phase_targets(source, values, active, publication_lock(source, selection), render_source, render_target,
                         compile_build, 'Release refresh')


MAX_GENERATIONS = 64


def journal_paths(state_dir, generation):
    """Refresh journals are chained: generation 1 keeps the original names,
    each later generation has its own journal and backup directory."""
    if generation == 1:
        return state_dir / 'release-refresh-runtime-upgrade.json', state_dir / 'release-refresh-runtime-backup'
    return (state_dir / ('release-refresh-%d-runtime-upgrade.json' % generation),
            state_dir / ('release-refresh-%d-runtime-backup' % generation))


def latest_generation(state_dir):
    generation = 0
    while generation < MAX_GENERATIONS:
        path = journal_paths(state_dir, generation + 1)[0]
        if not (path.exists() or path.is_symlink()):
            break
        generation += 1
    return generation


def finished(record):
    """A generation is finished only once its final active report is
    published; 'active' alone may precede it (resumed, never skipped)."""
    reports = record.get('installationReports') or []
    return (record.get('completed') == 'active' and not record.get('transition') and record.get('recoveryPause') is None
            and bool(reports) and reports[-1].get('configuration') == 'active'
            and reports[-1].get('releaseRefreshUpgradeOperation') == record.get('nonce'))


class CompletedJournalSource:
    """Read-only custody of one completed configuration-only operation and,
    through its parent, of every completed operation it was derived from."""
    def __init__(self, config, path, backup, version, mode, report, label, parent):
        self.config, self.path, self.backup, self.label = config, path, backup, label
        record = files.bounded_json(self.path, 32*1024*1024)
        reports = record.get('installationReports', [])
        if (record.get('version') != version or record.get('mode') != mode
                or record.get('completed') != 'active' or record.get('transition') or record.get('recoveryPause')
                or record.get('migrationVerified') is not False or record.get('migration') != {}
                or (record.get('backup') or {}).get('phase') != 'verified'
                or record['backup'].get('restore', {}).get('state') != 'removed'
                or not reports or reports[-1].get('configuration') != 'active'
                or reports[-1].get('installationReady') is not True
                or len((record.get('selection') or {}).get('catalog') or []) != 37
                or reports[-1].get(report + 'UpgradeOperation') != record.get('nonce')):
            raise ValueError('Completed ' + label + ' installation required')
        self.record = record
        self.original_digest = bootstrap.digest(record)
        self.parent_source = parent(config)
        try:
            if self.parent_source.digest() != record['parentSHA256']:
                raise ValueError(label.capitalize() + ' ancestry changed')
            self.parent, self.pause = self.parent_source.parent, self.parent_source.pause
            self.helper = install_images.REGISTRY + '@' + record['selection']['images']['migrator']['digest']
            self.custody()
        except BaseException:
            self.close()
            raise

    def digest(self):
        return self.original_digest

    def custody(self):
        if bootstrap.digest(files.bounded_json(self.path, 32*1024*1024)) != self.original_digest:
            raise ValueError('Original completed ' + self.label + ' journal changed')
        self.parent_source.custody()
        data.verify_archives(self.backup, self.record['backup']['archives'])

    @property
    def migrated_proof(self):
        # No migration happened since the schema-37 repair: its verified proof
        # remains the database authority oracle.
        inherited = getattr(self.parent_source, 'migrated_proof', None)
        if isinstance(inherited, dict):
            return copy.deepcopy(inherited)
        return copy.deepcopy(self.parent_source.record['migratedProof'])

    @property
    def current_report(self):
        return copy.deepcopy(self.record['installationReports'][-1])

    @property
    def browser_compiled(self):
        return copy.deepcopy(self.record['browserCompiled'])

    def describe(self, phase):
        if phase != 'active':
            raise ValueError('Only a completed ' + self.label + ' source is selectable')
        self.custody()
        return copy.deepcopy(self.record['targets']['active'])

    def close(self):
        self.parent_source.close()


def CompletedUpstreamSource(config):
    return CompletedJournalSource(config, config.state_dir / 'model-upstream-runtime-upgrade.json',
                                  config.state_dir / 'model-upstream-runtime-backup', upstream_upgrade.VERSION,
                                  'model-upstream', 'modelUpstream', 'model upstream', CompletedRepairSource)


def completed_source(config, generation):
    """The completed operation a refresh of the given generation starts from:
    the model-upstream switch for generation 1, else the previous refresh."""
    if generation == 1:
        return CompletedUpstreamSource(config)
    path, backup = journal_paths(config.state_dir, generation - 1)
    return CompletedJournalSource(config, path, backup, VERSION, 'release-refresh', 'releaseRefresh',
                                  'release refresh', lambda c: completed_source(c, generation - 1))


class ReleaseRefreshUpgrade(ModelUpstreamUpgrade):
    VERSION, MODE, REPORT, LABEL = VERSION, 'release-refresh', 'releaseRefresh', 'release refresh'

    def __init__(self, config, run=None, revision=None, trust_path=None, target_path=None):
        self.config = config
        self.source_chart = None
        # Resume the latest generation unless it completed; then start the next.
        latest = latest_generation(config.state_dir)
        self.generation = max(latest, 1)
        if latest and finished(files.bounded_json(journal_paths(config.state_dir, latest)[0], 32*1024*1024)):
            if run is not None or trust_path is not None:
                if latest == MAX_GENERATIONS:
                    raise ValueError('Release refresh generation bound reached')
                self.generation = latest + 1
        self.source_browser = completed_source(config, self.generation)
        try:
            prior = self.source_browser.record
            self.parent, self.pause = self.source_browser.parent, self.source_browser.pause
            self.parent_hash = self.source_browser.digest()
            self.catalog = prior['selection']['catalog']
            self.path, self.directory = journal_paths(config.state_dir, self.generation)
            self.source_chart = browser_upgrade_chart.OriginalChart(install.ROOT,
                prior['selection']['revision'], prior['chartSHA256'])
            self.target_chart = install.CHART
            self.record = files.bounded_json(self.path, 32*1024*1024) if self.path.exists() or self.path.is_symlink() else None
            if (run is None) != (revision is None) or (trust_path is None) != (target_path is None):
                raise ValueError('Both immutable publication inputs and both approval documents are required together')
            self.approvals = ({'provenance': files.bounded_json(trust_path, 128*1024),
                               'release': files.bounded_json(target_path, 128*1024)}
                              if trust_path is not None else None)
            # The upstream is inherited, never reviewed anew: refresh is images only.
            self.upstream = copy.deepcopy(prior['upstream'])
            self.selection = None
            if self.record is not None:
                if self.record.get('version') != VERSION or self.record.get('parentSHA256') != self.parent_hash:
                    raise ValueError('Original release refresh operation custody changed')
                if run is not None and (run != self.record['selection']['run'] or revision != self.record['selection']['revision']):
                    raise ValueError('Existing release refresh operation cannot select another publication')
                if self.approvals is not None and self.approvals != self.record['approvals']:
                    raise ValueError('Original reviewed release refresh approvals changed')
                if self.record['upstream'] != self.upstream:
                    raise ValueError('Original model upstream changed')
                if browser_upgrade_chart.digest(self.target_chart) != self.record['chartSHA256']:
                    raise ValueError('Original target release refresh chart changed')
                self.selection, self.approvals = self.record['selection'], self.record['approvals']
            elif run is not None:
                if type(run) is not int or not 0 < run <= 2**63-1 or not install_images.SHA.fullmatch(revision):
                    raise ValueError('Explicit exact-main release refresh publication required')
                if revision == prior['selection']['revision']:
                    raise ValueError('Release refresh requires a newer publication')
                source = install_upgrade.Source(install.ROOT)
                exact_local_publication(source, revision)
                source.git('merge-base', '--is-ancestor', prior['selection']['revision'], revision)
                catalog = source.catalog(revision)
                if catalog != self.catalog:
                    raise ValueError('Release refresh cannot change the schema catalog')
                _, publication_record = install_images.resolve(install_images.GitHub(), run, revision,
                    include_build=True, include_release=True, include_browser=True)
                self.selection = dict(publication_record, catalog=catalog)
            self.helper = (install_images.REGISTRY + '@' + self.selection['images']['migrator']['digest']) if self.selection else self.source_browser.helper
            if self.selection is not None:
                exact_local_publication(install_upgrade.Source(install.ROOT), self.selection['revision'])
        except BaseException:
            self.close()
            raise

    def targets(self, source, compile_build, browser_ca_uid):
        return targets(source, self.selection, self.upstream, self.approvals, self.source_render, self.config.render,
                       compile_build, time.time(), browser_ca_uid)

    def migrated_proof(self):
        return self.source_browser.migrated_proof
