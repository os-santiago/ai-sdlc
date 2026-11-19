"""Switch the model gateways of a completed schema-37 installation to a fixed
OpenAI-compatible upstream.

No schema change and no approval renewal: the parent's approvals must still be
valid. Only the worker image (which ships the upstream gateway) and the
reviewed `modelGateway` values change; every other value, resource identity,
browser binding and release identity is inherited from the completed repair
operation. Historical workflow authority is never renewed.
"""
import copy
import ipaddress
import json
import secrets
import time
from urllib.parse import urlsplit

import bootstrap_prepare as bootstrap
import browser_upgrade_chart
import build_foundation_install as files
import build_runtime_install
from browser_approval_window import PHASES
from diagnostic_runtime_upgrade import advance
import initial_activation
import install
import install_images
import install_upgrade
import konflux_runtime_install as platform
import prepared_resources
from review_feedback_runtime_upgrade import exact_local_publication
from review_feedback_source import CompletedFeedbackSource
from review_repair_runtime_upgrade import RepairBrowserPreflight, ReviewRepairUpgrade
import review_repair_upgrade_data as compatibility
from runtime_upgrade_pause import pause_values
from tool_budget_upgrade import ToolBudgetUpgrade
import upgrade_backup_data as data
from upgrade_fence_client import Fence

VERSION = 'ai-sdlc.model-upstream-runtime-upgrade/v1'
REPAIR_VERSION = 'ai-sdlc.review-repair-runtime-upgrade/v1'


UPSTREAM_KEYS = {'adapter', 'url', 'model', 'cidr', 'port', 'seconds', 'directAnswers'}
# The chart's per-call ceiling (the broker adds 30 s, bounded at 600).
UPSTREAM_SECONDS = (30, 570)
IN_FLIGHT = install.ROOT / 'runtime/k8s/internal/store/configuration_upgrade_in_flight.sql'


def validate_upstream(value):
    """The reviewed upstream document: URL, model, egress CIDR/port must agree."""
    if not isinstance(value, dict) or set(value) != UPSTREAM_KEYS or value['adapter'] != 'openai':
        raise ValueError('Exact reviewed OpenAI-compatible model upstream document required')
    parts = urlsplit(value['url'])
    if (parts.scheme != 'http' or parts.path != '/v1/chat/completions' or parts.query or parts.fragment
            or parts.username or parts.password or parts.port != value['port']):
        raise ValueError('Model upstream URL must be a fixed private HTTP completions endpoint')
    try:
        host = ipaddress.ip_address(parts.hostname)
        network = ipaddress.ip_network(value['cidr'], strict=True)
    except (TypeError, ValueError):
        raise ValueError('Literal model upstream address and CIDR required') from None
    private = [ipaddress.ip_network(n) for n in ('10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16', 'fc00::/7')]
    if (host not in network or network.num_addresses != 1 or not any(host in n for n in private)
            or type(value['port']) is not int or not 1 <= value['port'] <= 65535
            or type(value['seconds']) is not int or not UPSTREAM_SECONDS[0] <= value['seconds'] <= UPSTREAM_SECONDS[1]
            or type(value['directAnswers']) is not bool
            or not isinstance(value['model'], str) or not 1 <= len(value['model']) <= 128):
        raise ValueError('Model upstream must be one private host with bounded settings')
    return copy.deepcopy(value)


def gateway_values(upstream):
    upstream = validate_upstream(upstream)
    return {'adapter': upstream['adapter'],
            'upstream': {'url': upstream['url'], 'model': upstream['model'], 'cidr': upstream['cidr'],
                         'port': upstream['port'], 'seconds': upstream['seconds'], 'credentialSecret': '',
                         'directAnswers': upstream['directAnswers']},
            'benchmark': {'promptTokens': 16384, 'completionTokens': 4096}}


def in_flight(database):
    """Configuration-only switch: completed history of every stage is kept and
    admitted; only in-flight model-dependent work blocks it (the same predicate
    the configuration fence enforces under its locks)."""
    query = IN_FLIGHT.read_text()
    if database.sql(query.strip().rstrip(';') + ';') != 'f\n':
        raise ValueError('In-flight model-dependent work prevents the model upstream switch; retry when it settles')


def targets(source, selection, upstream, render_source, render_target, compile_build):
    values = source['values']
    if render_source(values) != source['documents']:
        raise ValueError('Original installed repair render changed')
    if render_target(values) != source['documents']:
        raise ValueError('Target chart must render the unchanged installation identically')
    if values.get('modelGateway', {}).get('adapter', 'agent-cli') != 'agent-cli':
        raise ValueError('Source already uses a model upstream')
    worker = selection['images']['worker']['digest']
    if not install_images.DIGEST.fullmatch(worker):
        raise ValueError('Immutable selected worker image required')
    active = copy.deepcopy(values)
    lock = copy.deepcopy(source['images'])
    active['components']['worker']['digest'] = lock['components']['worker']['digest'] = worker
    active['modelGateway'] = gateway_values(upstream)
    paused = pause_values(values, compile_build)
    staged = pause_values(active, compile_build)
    def description(v, images, render):
        return dict(values=v, images=copy.deepcopy(images), documents=render(v),
                    externalRuntime=copy.deepcopy(source['externalRuntime']))
    result = {'source': copy.deepcopy(source), 'paused': description(paused, source['images'], render_source),
              'staged': description(staged, lock, render_target)}
    for phase in ('warming', 'armed', 'active'):
        result[phase] = description(initial_activation.phase_values(staged, phase, compile_build), lock, render_target)
    if result['active']['values'] != active:
        raise ValueError('Model upstream activation changed its reviewed target')
    inventory = set(prepared_resources.documents(source['documents']))
    if any(set(prepared_resources.documents(v['documents'])) != inventory for v in result.values()):
        raise ValueError('Model upstream switch cannot add or remove Helm resources')
    armed = copy.deepcopy(result['armed']['documents'])
    for obj in armed:
        if obj['kind'] == 'Deployment' and obj['metadata']['name'] == 'ai-sdlc-intake':
            obj['spec']['replicas'] = 1
    if armed != result['active']['documents']:
        raise ValueError('Model upstream activation must enable intake last')
    return result


class CompletedRepairSource:
    """Read-only custody of the completed schema-37 repair installation."""
    def __init__(self, config):
        self.config = config
        self.path = config.state_dir / 'review-repair-runtime-upgrade.json'
        record = files.bounded_json(self.path, 32*1024*1024)
        reports = record.get('installationReports', [])
        if (record.get('version') != REPAIR_VERSION or record.get('mode') != 'review-repair'
                or record.get('completed') != 'active' or record.get('transition') or record.get('recoveryPause')
                or record.get('migrationVerified') is not True
                or (record.get('backup') or {}).get('phase') != 'verified'
                or record['backup'].get('restore', {}).get('state') != 'removed'
                or record.get('rehearsal', {}).get('cleanup') != 'removed'
                or not reports or reports[-1].get('configuration') != 'active'
                or reports[-1].get('installationReady') is not True
                or len((record.get('selection') or {}).get('catalog') or []) != 37
                or reports[-1].get('reviewRepairUpgradeOperation') != record.get('nonce')):
            raise ValueError('Completed original repair installation required')
        self.record = record
        self.original_digest = bootstrap.digest(record)
        self.feedback = CompletedFeedbackSource(config)
        try:
            if self.feedback.digest() != record['parentSHA256']:
                raise ValueError('Original repair ancestry changed')
            self.parent, self.pause = self.feedback.parent, self.feedback.pause
            self.helper = install_images.REGISTRY + '@' + record['selection']['images']['migrator']['digest']
            self.custody()
        except BaseException:
            self.close()
            raise

    def digest(self):
        return self.original_digest

    def custody(self):
        if bootstrap.digest(files.bounded_json(self.path, 32*1024*1024)) != self.original_digest:
            raise ValueError('Original completed repair journal changed')
        self.feedback.custody()
        data.verify_archives(self.config.state_dir / 'review-repair-runtime-backup', self.record['backup']['archives'])

    @property
    def current_report(self):
        return copy.deepcopy(self.record['installationReports'][-1])

    @property
    def browser_compiled(self):
        return copy.deepcopy(self.record['browserCompiled'])

    def describe(self, phase):
        if phase != 'active':
            raise ValueError('Only a completed repair source is selectable')
        self.custody()
        return copy.deepcopy(self.record['targets']['active'])

    def close(self):
        self.feedback.close()


class ModelUpstreamUpgrade(ReviewRepairUpgrade):
    def __init__(self, config, run=None, revision=None, upstream_path=None):
        self.config = config
        self.source_chart = None
        self.source_browser = CompletedRepairSource(config)
        try:
            prior = self.source_browser.record
            self.parent, self.pause = self.source_browser.parent, self.source_browser.pause
            self.parent_hash = self.source_browser.digest()
            self.catalog = prior['selection']['catalog']
            self.path = config.state_dir / 'model-upstream-runtime-upgrade.json'
            self.directory = config.state_dir / 'model-upstream-runtime-backup'
            self.source_chart = browser_upgrade_chart.OriginalChart(install.ROOT,
                prior['selection']['revision'], prior['chartSHA256'])
            self.target_chart = install.CHART
            self.record = files.bounded_json(self.path, 32*1024*1024) if self.path.exists() or self.path.is_symlink() else None
            if (run is None) != (revision is None):
                raise ValueError('Both immutable model upstream publication inputs are required')
            self.upstream = files.bounded_json(upstream_path, 16*1024) if upstream_path is not None else None
            self.approvals = prior['approvals']
            self.selection = None
            if self.record is not None:
                if self.record.get('version') != VERSION or self.record.get('parentSHA256') != self.parent_hash:
                    raise ValueError('Original model upstream operation custody changed')
                if run is not None and (run != self.record['selection']['run'] or revision != self.record['selection']['revision']):
                    raise ValueError('Existing model upstream operation cannot select another publication')
                if self.upstream is not None and self.upstream != self.record['upstream']:
                    raise ValueError('Original reviewed model upstream changed')
                if browser_upgrade_chart.digest(self.target_chart) != self.record['chartSHA256']:
                    raise ValueError('Original target model upstream chart changed')
                self.selection, self.upstream = self.record['selection'], self.record['upstream']
            elif run is not None:
                if type(run) is not int or not 0 < run <= 2**63-1 or not install_images.SHA.fullmatch(revision):
                    raise ValueError('Explicit exact-main model upstream publication required')
                source = install_upgrade.Source(install.ROOT)
                exact_local_publication(source, revision)
                source.git('merge-base', '--is-ancestor', prior['selection']['revision'], revision)
                catalog = source.catalog(revision)
                if catalog != self.catalog:
                    raise ValueError('Model upstream switch cannot change the schema catalog')
                _, publication = install_images.resolve(install_images.GitHub(), run, revision,
                    include_build=True, include_release=True, include_browser=True)
                self.selection = dict(publication, catalog=catalog)
            self.helper = (install_images.REGISTRY + '@' + self.selection['images']['migrator']['digest']) if self.selection else self.source_browser.helper
            if self.selection is not None:
                exact_local_publication(install_upgrade.Source(install.ROOT), self.selection['revision'])
        except BaseException:
            self.close()
            raise

    def original_database_authority(self, proof=None, database=None):
        database = database or self.database()
        current = proof if proof is not None else self.snapshot(database)
        original = self.source_browser.record['migratedProof']
        for field in ('@schema', '@roles', '@membership', '@role-settings', 'schema_migrations'):
            if current[field] != original[field]:
                raise ValueError('Original schema37 database authority changed')
        roles = compatibility.Compatibility(database).roles()
        if roles != self.source_browser.record['sourceRoleIdentities']:
            raise ValueError('Original schema37 role identities changed')
        return roles

    def snapshot(self, database=None):
        database = database or self.database()
        in_flight(database)
        names = json.loads(database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        if names != [x['filename'] for x in self.catalog]:
            raise ValueError('Original schema37 catalog changed')
        return database.snapshot()

    def retained(self, database=None):
        database = database or self.database()
        if (self.snapshot(database) != self.retained_proof()
                or compatibility.Compatibility(database).roles() != self.record['sourceRoleIdentities']):
            raise ValueError('Original history or authority changed during the model upstream switch')

    def capture_first_pause(self, database):
        """No migration exists, so the fenced snapshot is adopted as the retained
        proof: queued work may legitimately progress between begin() and the
        fence. Under the fence the in-flight predicate, the exact catalog, the
        table inventory and the original database authority are re-proven."""
        self.observe('source')
        if (self.record['completed'] != 'source' or self.record.get('pause') is not None
                or self.record.get('backup') is not None or self.record.get('transition') is not None
                or self.record.get('recoveryPause') is not None or self.record.get('fencedSourceProof') is not None):
            raise ValueError('Only the untouched first pause can capture the fenced source')
        current = self.snapshot(database)
        if set(current) != set(self.record['sourceProof']):
            raise ValueError('Original table inventory changed before the first pause')
        self.original_database_authority(current, database)
        return current

    def fence(self, connection, port):
        return Fence(self.config.provider, self.helper, connection, port, self.catalog,
                     expires_at=self.record['expiresAt'], configuration=True)

    def backup(self):
        # Plain fenced export and verified offline restore; no migration exists.
        return ToolBudgetUpgrade.backup(self)

    def begin(self):
        if self.record is not None:
            return
        if self.selection is None or self.upstream is None:
            raise ValueError('Explicit reviewed model upstream publication and document required')
        validate_upstream(self.upstream)
        self.observe('source')
        source = self.source_browser.describe('active')
        compile_build = lambda request: build_runtime_install.component_call(
            self.config.provider, self.helper, 'compile-build-installation', request)
        result = targets(source, self.selection, self.upstream, self.source_render, self.config.render, compile_build)
        database = self.database()
        # Checked before the journal exists: a refusal leaves nothing to resume.
        in_flight(database)
        proof = compatibility.Compatibility(database).observe()
        roles = self.original_database_authority(proof['data'], database)
        self.observe('source')
        now = time.time()
        self.save({'version': VERSION, 'nonce': secrets.token_hex(16), 'parentSHA256': self.parent_hash,
            'createdAt': now, 'expiresAt': initial_activation.activation_expiry(result['active']['values'], now),
            'chartSHA256': browser_upgrade_chart.digest(self.target_chart), 'sourceChartSHA256': self.source_chart.expected,
            'mode': 'model-upstream', 'completed': 'source', 'transition': None,
            'selection': copy.deepcopy(self.selection), 'upstream': copy.deepcopy(self.upstream),
            'approvals': copy.deepcopy(self.approvals), 'approvalsSHA256': bootstrap.digest(self.approvals),
            'targets': result, 'sourceProof': proof['data'], 'sourceCompatibility': proof, 'sourceRoleIdentities': roles,
            'resourceUIDs': copy.deepcopy(self.source_browser.record['resourceUIDs']),
            'browserCompiled': self.source_browser.browser_compiled,
            'releaseIdentities': copy.deepcopy(self.source_browser.record.get('releaseIdentities')),
            'pause': None, 'backup': None, 'recoveryPause': None, 'migration': {}, 'migrationVerified': False,
            'rehearsal': {}, 'installationSource': self.source_browser.current_report, 'installationReports': []})

    def run(self):
        self.begin()
        self.deadline()
        if self.record.get('mode') != 'model-upstream' or self.record.get('recoveryPause'):
            raise ValueError('Original model upstream operation required')
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
                RepairBrowserPreflight(self).preflight()
                self.retained()
            advance(self.record, phase, self.observe, self.apply, self.save, phases=PHASES)
        self.publish_report(active=True)
        return self.status()

    def publish_report(self, *, active):
        self.observe('active' if active else 'paused')
        result = copy.deepcopy(self.record['installationSource'])
        result.update(configuration='active' if active else 'model-upstream-upgrade-paused', installationReady=active,
            runtimePaused=not active, innerFrozen=not active, outerFrozen=not active, deliveryEnabled=active,
            issueIntakeEnabled=active, runtimeReadinessValidated=active, modelUpstreamUpgradeOperation=self.record['nonce'],
            modelUpstreamUpgradeParentSHA256=self.parent_hash, modelUpstreamSelection=copy.deepcopy(self.selection),
            modelUpstream=copy.deepcopy(self.record['upstream']), uniformTargetPublication=False,
            observedAt=int(time.time()*1000), previousWorkflowAuthorityRenewed=False, workflowEvidence=False,
            autonomousEndToEndValidated=False)
        reports = self.record['installationReports']
        if reports and {k: v for k, v in reports[-1].items() if k != 'observedAt'} == {k: v for k, v in result.items() if k != 'observedAt'}:
            result = reports[-1]
        else:
            if len(reports) >= 2:
                raise ValueError('Original model upstream report bound exceeded')
            reports.append(result)
            self.save(self.record)
        platform.write_record(self.config.state_dir / 'installation.json', result)

    def status(self):
        if self.record is None or self.record.get('transition'):
            raise ValueError('Complete model upstream phase required')
        self.observe(self.record['completed'])
        return {'version': VERSION, 'phase': self.record['completed'], 'catalogSize': len(self.catalog),
            'issueIntakeEnabled': self.record['completed'] == 'active', 'modelUpstream': self.record['upstream'],
            'administrativeDeadline': int(self.record['expiresAt']*1000),
            'previousWorkflowAuthorityRenewed': False, 'workflowEvidence': False,
            'autonomousEndToEndValidated': False}
