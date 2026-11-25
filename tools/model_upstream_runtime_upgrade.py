"""Switch the model gateways of a completed schema-37 installation to a fixed
OpenAI-compatible upstream, as a uniform same-schema publication.

No schema change. Every component image comes from the selected main
publication; the reviewed modelGateway values and per-call windows are set,
and separately reviewed provenance (new verifier image) and release (new
finite window, same CA) documents are installed. Authority policy, build,
budgets, scope, resource identities and release identities are inherited from
the completed repair operation. Historical workflow authority is never renewed.
"""
import copy
import ipaddress
import json
import re
import secrets
import time
from urllib.parse import urlsplit

import bootstrap_prepare as bootstrap
import browser_upgrade_chart
import build_foundation_install as files
import build_runtime_install
import candidate_install
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
import review_repair_upgrade_targets as repair_targets
from review_repair_runtime_upgrade import RepairApprovalPreflight
from runtime_upgrade_pause import pause_values
from tool_budget_upgrade import ToolBudgetUpgrade
import upgrade_backup_data as data
from upgrade_fence_client import Fence

VERSION = 'ai-sdlc.model-upstream-runtime-upgrade/v1'
CONVERGE_SECONDS = 300
AUTHORITY_FIELDS = ('@schema', '@roles', '@membership', '@role-settings', 'schema_migrations')
REPAIR_VERSION = 'ai-sdlc.review-repair-runtime-upgrade/v1'


UPSTREAM_KEYS = {'adapter', 'url', 'model', 'cidr', 'port', 'seconds', 'directAnswers', 'callSeconds',
                 'reviewCallSeconds', 'credentialSecret'}
# The chart's per-call ceiling (the broker adds 30 s, bounded at 600).
UPSTREAM_SECONDS = (30, 570)
IN_FLIGHT = install.ROOT / 'runtime/k8s/internal/store/configuration_upgrade_in_flight.sql'
SECRET_NAME = re.compile(r'[a-z0-9]([-a-z0-9]{0,251}[a-z0-9])?\Z')


def validate_upstream(value):
    """The reviewed model-as-a-service document: one OpenAI-compatible private
    endpoint whose URL, egress CIDR and port agree, with bounded windows and an
    optional Secret holding the service's bearer token."""
    if not isinstance(value, dict) or set(value) != UPSTREAM_KEYS or value['adapter'] != 'openai':
        raise ValueError('Exact reviewed model upstream document required')
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
            # The worker adopts a benchmarked timeout only within callSeconds - 20.
            or type(value['callSeconds']) is not int or not max(60, value['seconds'] + 20) <= value['callSeconds'] <= 600
            or type(value['reviewCallSeconds']) is not int or not 60 <= value['reviewCallSeconds'] <= 180
            or not isinstance(value['model'], str) or not re.fullmatch(r'[A-Za-z0-9._:/-]{1,128}', value['model'])
            or not isinstance(value['credentialSecret'], str)
            or (value['credentialSecret'] and not SECRET_NAME.fullmatch(value['credentialSecret']))):
        raise ValueError('Model upstream must be one private host with bounded settings')
    return copy.deepcopy(value)


def gateway_values(upstream, answer_tokens=candidate_install.DEFAULT_ANSWER_TOKENS):
    """The reviewed gateway values. The worker benchmark's answer envelope is
    the generator profile's maxTokens, so the derived per-call timeout covers
    the longest answer the profile admits (a gateway too slow for it fails its
    benchmark and never reports ready, instead of timing out mid-answer)."""
    upstream = validate_upstream(upstream)
    if (type(answer_tokens) is not int
            or not candidate_install.MIN_ANSWER_TOKENS <= answer_tokens <= candidate_install.MAX_ANSWER_TOKENS):
        raise ValueError('Generator answer-token bound must be an integer from 4096 to 8192')
    return {'adapter': upstream['adapter'],
            'upstream': {'url': upstream['url'], 'model': upstream['model'], 'cidr': upstream['cidr'],
                         'port': upstream['port'], 'seconds': upstream['seconds'],
                         'credentialSecret': upstream['credentialSecret'], 'directAnswers': upstream['directAnswers']},
            'benchmark': {'promptTokens': 16384, 'completionTokens': answer_tokens},
            'reviewBenchmark': {'promptTokens': 16384, 'completionTokens': 1024}}


def recorded_answer_tokens(values):
    """The generator profile's recorded per-answer token bound."""
    return json.loads(values['candidate']['configJSON'])['profile'].get('maxTokens')


SECRET_KEYS_TEMPLATE = '{{.type}}{{range $k, $v := .data}} {{$k}}{{end}}'


def credential_secret(keys, upstream):
    """An optional service bearer Secret must exist as an Opaque Secret with
    exactly one 'credential' key. Only its type and key names are queried;
    the value never reaches this process."""
    if not upstream['credentialSecret']:
        return
    observed = keys(upstream['credentialSecret'], platform.NAMESPACE)
    if observed != ('Opaque', ['credential']):
        raise ValueError('The reviewed model service Secret must be Opaque with exactly one credential key')


def secret_keys(config):
    """Type and key names of a Secret through a go-template (no values)."""
    def keys(name, namespace):
        raw = config.base_run(config.kubectl + ['get', 'secret', name, '-n', namespace, '--ignore-not-found',
                                                '-o', 'go-template=' + SECRET_KEYS_TEMPLATE],
                              capture_output=True, timeout=20).stdout
        text = raw.decode() if isinstance(raw, bytes) else raw
        if not text.strip() or len(text) > 4096:
            return None
        kind, *names = text.split()
        return kind, sorted(names)
    return keys


def in_flight(database):
    """Configuration-only switch: completed history of every stage is kept and
    admitted; only in-flight model-dependent work blocks it (the same predicate
    the configuration fence enforces under its locks)."""
    query = IN_FLIGHT.read_text()
    if database.sql(query.strip().rstrip(';') + ';') != 'f\n':
        raise ValueError('In-flight model-dependent work prevents the configuration switch; retry when it settles')


def publication(source, selection, approvals, render_source, render_target, now, browser_ca_uid, label):
    """The uniform same-schema publication shared by every configuration-only
    switch: every component image from the selected main publication and the
    independently reviewed provenance (new verifier) and release (same CA)
    documents. Returns the source values and the new active values."""
    if not isinstance(approvals, dict) or set(approvals) != {'provenance', 'release'}:
        raise ValueError('Both independently reviewed approval documents are required')
    values = source['values']
    if render_source(values) != source['documents']:
        raise ValueError('Original installed ' + label + ' render changed')
    if render_target(values) != source['documents']:
        raise ValueError('Target chart must render the unchanged installation identically')
    images = selection['images']
    expected = install_images.ARTIFACT_COMPONENTS | {'artifact-builder', 'build-controller', 'deployer', 'browser-verifier'}
    if set(images) != expected or any(not install_images.DIGEST.fullmatch(x.get('digest', '')) for x in images.values()):
        raise ValueError('Complete immutable schema37 publication required')
    active = copy.deepcopy(values)
    for key, component in repair_targets.COMPONENTS.items():
        active['components'][key]['digest'] = images[component]['digest']
    active['migrator']['digest'] = images['migrator']['digest']
    active['verifier']['digest'] = images['verifier']['digest']
    active['build']['digest'] = images['build-controller']['digest']
    active['release']['digest'] = images['deployer']['digest']
    worker = install_images.REGISTRY + '@' + images['worker']['digest']
    mailbox = install_images.REGISTRY + '@' + images['model-mailbox']['digest']
    candidate = json.loads(values['candidate']['configJSON'])
    candidate['placement'].update(WorkerImage=worker, MailboxImage=mailbox)
    active['candidate']['configJSON'] = json.dumps(candidate, sort_keys=True)
    review = json.loads(values['review']['configJSON'])
    review.update(workerImage=worker, mailboxImage=mailbox)
    active['review']['configJSON'] = json.dumps(review, sort_keys=True)
    repair_targets.validate_provenance(json.loads(values['provenance']['trustJSON']), approvals['provenance'],
                                       install_images.REGISTRY + '@' + images['verifier']['digest'], now)
    repair_targets.validate_release(json.loads(values['release']['targetJSON']), approvals['release'], now)
    if approvals['release']['tlsCA'] != json.loads(values['release']['targetJSON'])['tlsCA']:
        raise ValueError('A configuration-only switch cannot rotate the release CA')
    active['provenance']['trustJSON'] = json.dumps(approvals['provenance'], sort_keys=True)
    active['release']['targetJSON'] = json.dumps(approvals['release'], sort_keys=True)
    active['release']['browserProfileJSON'] = repair_targets.rotated_profile(values['release']['browserProfileJSON'],
        json.loads(values['release']['targetJSON']), approvals['release'], browser_ca_uid)
    return values, active


def without_publication(active, values):
    """The active values with every publication field restored to the source;
    the caller compares the result to prove a closed delta."""
    restored = copy.deepcopy(active)
    for section, key in (('candidate', 'configJSON'), ('review', 'configJSON'), ('provenance', 'trustJSON'),
                         ('release', 'targetJSON'), ('release', 'browserProfileJSON')):
        restored[section][key] = values[section][key]
    for key in repair_targets.COMPONENTS:
        restored['components'][key]['digest'] = values['components'][key]['digest']
    for section in ('migrator', 'verifier', 'build', 'release'):
        restored[section]['digest'] = values[section]['digest']
    return restored


def publication_lock(source, selection):
    lock = copy.deepcopy(source['images'])
    for key, component in repair_targets.COMPONENTS.items():
        lock['components'][key]['digest'] = selection['images'][component]['digest']
    lock['migrator']['digest'] = selection['images']['migrator']['digest']
    return lock


def phase_targets(source, values, active, lock, render_source, render_target, compile_build, label):
    """Paused, staged, warming, armed and active descriptions: no resource is
    added or removed and intake is enabled last."""
    paused = pause_values(values, compile_build)
    staged = pause_values(active, compile_build)
    def description(v, selected, render):
        return dict(values=v, images=copy.deepcopy(selected), documents=render(v),
                    externalRuntime=copy.deepcopy(source['externalRuntime']))
    result = {'source': copy.deepcopy(source), 'paused': description(paused, source['images'], render_source),
              'staged': description(staged, lock, render_target)}
    for phase in ('warming', 'armed', 'active'):
        result[phase] = description(initial_activation.phase_values(staged, phase, compile_build), lock, render_target)
    if result['active']['values'] != active:
        raise ValueError(label + ' activation changed its reviewed target')
    inventory = set(prepared_resources.documents(source['documents']))
    if any(set(prepared_resources.documents(v['documents'])) != inventory for v in result.values()):
        raise ValueError(label + ' cannot add or remove Helm resources')
    armed = copy.deepcopy(result['armed']['documents'])
    for obj in armed:
        if obj['kind'] == 'Deployment' and obj['metadata']['name'] == 'ai-sdlc-intake':
            obj['spec']['replicas'] = 1
    if armed != result['active']['documents']:
        raise ValueError(label + ' activation must enable intake last')
    return result


def targets(source, selection, upstream, approvals, render_source, render_target, compile_build, now, browser_ca_uid):
    """Uniform same-schema publication with the reviewed model upstream, the
    per-call windows and renewed approvals. Authority, budgets, build and
    scope are inherited unchanged; no migration exists."""
    if not isinstance(approvals, dict) or set(approvals) != {'provenance', 'release'}:
        raise ValueError('Both independently reviewed approval documents are required')
    upstream = validate_upstream(upstream)
    if source['values'].get('modelGateway', {}).get('adapter', 'agent-cli') != 'agent-cli':
        raise ValueError('Source already uses a model upstream')
    values, active = publication(source, selection, approvals, render_source, render_target, now, browser_ca_uid, 'repair')
    candidate = json.loads(active['candidate']['configJSON'])
    profile = candidate['profile']
    if profile.get('maxRepairs') != 1 or 'callSeconds' in profile or upstream['callSeconds'] > profile['maxSeconds']:
        raise ValueError('Completed repair generator profile without a call window required')
    profile['callSeconds'] = upstream['callSeconds']
    active['candidate']['configJSON'] = json.dumps(candidate, sort_keys=True)
    review = json.loads(active['review']['configJSON'])
    if 'callSeconds' in review['profile'] or upstream['reviewCallSeconds'] > review['profile']['maxSeconds']:
        raise ValueError('Reviewer profile without a call window required')
    review['profile']['callSeconds'] = upstream['reviewCallSeconds']
    active['review']['configJSON'] = json.dumps(review, sort_keys=True)
    # The recorded answer bound is inherited unchanged and sizes the benchmark.
    active['modelGateway'] = gateway_values(upstream, recorded_answer_tokens(active))
    # Closed delta: nothing else in the installation values may change.
    restored = without_publication(active, values)
    restored.pop('modelGateway')
    original = copy.deepcopy(values)
    original.pop('modelGateway', None)
    if restored != original:
        raise ValueError('Model upstream switch changed unrelated installation values')
    return phase_targets(source, values, active, publication_lock(source, selection), render_source, render_target,
                         compile_build, 'Model upstream')


class UpstreamApprovalPreflight(RepairApprovalPreflight):
    """Approval renewal against the completed repair installation: its recorded
    (already rotated) release identities are the original, and nothing may
    change because this switch never rotates the CA."""
    def __init__(self, upgrade, *, capture_browser=False):
        super().__init__(upgrade, capture_browser=capture_browser)
        self.original['sources']['release-original-identities'] = copy.deepcopy(
            upgrade.source_browser.record['releaseIdentities'])


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
    VERSION, MODE, REPORT, LABEL = VERSION, 'model-upstream', 'modelUpstream', 'model upstream switch'

    def __init__(self, config, run=None, revision=None, upstream_path=None, trust_path=None, target_path=None):
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
            if (trust_path is None) != (target_path is None) or (upstream_path is None) != (trust_path is None):
                raise ValueError('The reviewed model upstream and both approval documents are required together')
            self.approvals = ({'provenance': files.bounded_json(trust_path, 128*1024),
                               'release': files.bounded_json(target_path, 128*1024)}
                              if trust_path is not None else None)
            self.selection = None
            if self.record is not None:
                if self.record.get('version') != VERSION or self.record.get('parentSHA256') != self.parent_hash:
                    raise ValueError('Original model upstream operation custody changed')
                if run is not None and (run != self.record['selection']['run'] or revision != self.record['selection']['revision']):
                    raise ValueError('Existing model upstream operation cannot select another publication')
                if self.upstream is not None and self.upstream != self.record['upstream']:
                    raise ValueError('Original reviewed model upstream changed')
                if self.approvals is not None and self.approvals != self.record['approvals']:
                    raise ValueError('Original reviewed model upstream approvals changed')
                if browser_upgrade_chart.digest(self.target_chart) != self.record['chartSHA256']:
                    raise ValueError('Original target model upstream chart changed')
                self.selection, self.upstream = self.record['selection'], self.record['upstream']
                self.approvals = self.record['approvals']
            elif run is not None:
                if type(run) is not int or not 0 < run <= 2**63-1 or not install_images.SHA.fullmatch(revision):
                    raise ValueError('Explicit exact-main model upstream publication required')
                source = install_upgrade.Source(install.ROOT)
                exact_local_publication(source, revision)
                source.git('merge-base', '--is-ancestor', prior['selection']['revision'], revision)
                catalog = source.catalog(revision)
                if catalog != self.catalog:
                    raise ValueError('Model upstream switch cannot change the schema catalog')
                api = install_images.GitHub()
                # Verify signatures; revisions without the cutover sentinel predate signing.
                _, publication = install_images.resolve(api, run, revision,
                    include_build=True, include_release=True, include_browser=True,
                    verify=install_images.signature_verifier(api, revision, allow_unsigned_legacy=True))
                self.selection = dict(publication, catalog=catalog)
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
        return self.source_browser.record['migratedProof']

    def original_database_authority(self, proof=None, database=None):
        database = database or self.database()
        current = proof if proof is not None else self.snapshot(database)
        original = self.migrated_proof()
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
        """Until the backup is verified the paused database must equal the
        fenced proof row for row. From 'ready' on, the restarted runtime may
        legitimately progress queued or downstream work (there is no migration
        to protect), so later phases re-prove only database authority: schema,
        roles, memberships, role settings, catalog and role identities."""
        database = database or self.database()
        roles = compatibility.Compatibility(database).roles()
        if self.record['completed'] in ('source', 'paused'):
            if self.snapshot(database) != self.retained_proof() or roles != self.record['sourceRoleIdentities']:
                raise ValueError('Original history or authority changed during the ' + self.LABEL)
            return
        names = json.loads(database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        current = database.snapshot()
        proof = self.retained_proof()
        if (names != [x['filename'] for x in self.catalog] or roles != self.record['sourceRoleIdentities']
                or any(current[field] != proof[field] for field in AUTHORITY_FIELDS)):
            raise ValueError('Original database authority changed during the ' + self.LABEL)

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
        self.deadline()
        backup = self.record.get('backup')
        if (backup is not None and backup.get('phase') == 'capturing' and backup.get('archives') == {}
                and (not self.directory.exists() or (self.directory.is_dir() and not self.directory.is_symlink()
                                                     and not any(self.directory.iterdir())))):
            # The fence was never acquired (or nothing was exported): no archive
            # exists, so the capture can be retried. Partial files still refuse.
            if self.directory.exists():
                self.directory.rmdir()
            self.record['backup'] = None
            self.save(self.record)
        return ToolBudgetUpgrade.backup(self)

    def begin(self):
        if self.record is not None:
            return
        if self.selection is None or self.upstream is None or self.approvals is None:
            raise ValueError('Explicit reviewed ' + self.LABEL + ' publication, document and approvals required')
        validate_upstream(self.upstream)
        self.observe('source')
        credential_secret(secret_keys(self.config), self.upstream)
        source = self.source_browser.describe('active')
        compile_build = lambda request: build_runtime_install.component_call(
            self.config.provider, self.helper, 'compile-build-installation', request)
        browser_ca = self.config.read('configmap', 'approved-console-ca', 'ai-sdlc-browser')
        if (not isinstance(browser_ca, dict) or browser_ca.get('immutable') is not True
                or browser_ca.get('data') != {'ca.crt': self.approvals['release']['tlsCA']}):
            raise ValueError('Browser platform CA must be an immutable object holding exactly the reviewed CA')
        result = self.targets(source, compile_build, browser_ca['metadata']['uid'])
        identities = self.source_browser.record['resourceUIDs']
        if any(set(prepared_resources.documents(t['documents'])) != set(identities) for t in result.values()):
            raise ValueError(self.LABEL.capitalize() + ' cannot add or remove original resources')
        verifier = install_images.REGISTRY + '@' + self.selection['images']['verifier']['digest']
        compiled = build_runtime_install.component_call(self.config.provider, self.helper, 'compile-provenance-installation',
            {'trust': self.approvals['provenance'], 'profile': json.loads(source['values']['build']['configJSON'])['profile'],
             'verifierImage': verifier})
        if (set(compiled) != {'version', 'trust', 'trustSHA256', 'profileSHA256', 'verifierImage'}
                or compiled['version'] != 'ai-sdlc.provenance-installation/v1'
                or compiled['trust'] != self.approvals['provenance'] or compiled['verifierImage'] != verifier):
            raise ValueError('Reviewed ' + self.LABEL + ' trust compiler changed input')
        release = self.approvals['release']
        installed_ca = self.config.read('configmap', 'saferus-ca', release['namespace'])
        if not isinstance(installed_ca, dict) or installed_ca.get('data', {}).get('ca.crt') != release['tlsCA']:
            raise ValueError('Reviewed release CA differs from the installed application CA')
        database = self.database()
        # Checked before the journal exists: a refusal leaves nothing to resume.
        in_flight(database)
        proof = compatibility.Compatibility(database).observe()
        roles = self.original_database_authority(proof['data'], database)
        # Bind the browser profile to the renewed release target before the
        # first durable intent; the profile itself must stay identical.
        self.record = {'targets': result, 'browserCompiled': self.source_browser.browser_compiled}
        try:
            preflight = UpstreamApprovalPreflight(self, capture_browser=True)
        finally:
            self.record = None
        preflight.preflight()
        if preflight.renewed_browser is None or preflight.renewed_identities is None:
            raise ValueError('Actual browser and release compilation for the new target is required')
        self.observe('source')
        now = time.time()
        self.save({'version': self.VERSION, 'nonce': secrets.token_hex(16), 'parentSHA256': self.parent_hash,
            'createdAt': now, 'expiresAt': initial_activation.activation_expiry(result['active']['values'], now),
            'chartSHA256': browser_upgrade_chart.digest(self.target_chart), 'sourceChartSHA256': self.source_chart.expected,
            'mode': self.MODE, 'completed': 'source', 'transition': None,
            'selection': copy.deepcopy(self.selection), 'upstream': copy.deepcopy(self.upstream),
            'approvals': copy.deepcopy(self.approvals), 'approvalsSHA256': bootstrap.digest(self.approvals),
            'targets': result, 'sourceProof': proof['data'], 'sourceCompatibility': proof, 'sourceRoleIdentities': roles,
            'resourceUIDs': copy.deepcopy(identities), 'browserCompiled': preflight.renewed_browser,
            'releaseIdentities': preflight.renewed_identities,
            'pause': None, 'backup': None, 'recoveryPause': None, 'migration': {}, 'migrationVerified': False,
            'rehearsal': {}, 'installationSource': self.source_browser.current_report, 'installationReports': []})

    def run(self):
        self.begin()
        self.deadline()
        if self.record.get('mode') != self.MODE or self.record.get('recoveryPause'):
            raise ValueError('Original ' + self.LABEL + ' operation required')
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
            # The gateways need the service Secret at every rollout, including
            # when a pending transition is reconciled.
            credential_secret(secret_keys(self.config), self.record['upstream'])
            if self.record.get('transition') is None:
                self.observe(self.record['completed'])
                self.retained()
                RepairBrowserPreflight(self).preflight()
                self.retained()
            # Each rollout restarts the model gateways, which benchmark the
            # model before the worker and verifier report ready.
            advance(self.record, phase, self.observe, self.apply, self.save, phases=PHASES,
                    converge_seconds=CONVERGE_SECONDS)
        self.publish_report(active=True)
        return self.status()

    def publish_report(self, *, active):
        self.observe('active' if active else 'paused')
        result = copy.deepcopy(self.record['installationSource'])
        result.update(configuration='active' if active else self.MODE + '-upgrade-paused', installationReady=active,
            runtimePaused=not active, innerFrozen=not active, outerFrozen=not active, deliveryEnabled=active,
            issueIntakeEnabled=active, runtimeReadinessValidated=active,
            modelUpstream=copy.deepcopy(self.record['upstream']), approvalDocumentsSHA256=self.record['approvalsSHA256'],
            uniformTargetPublication=True,
            observedAt=int(time.time()*1000), previousWorkflowAuthorityRenewed=False, workflowEvidence=False,
            autonomousEndToEndValidated=False)
        result.update({self.REPORT + 'UpgradeOperation': self.record['nonce'],
                       self.REPORT + 'UpgradeParentSHA256': self.parent_hash,
                       self.REPORT + 'Selection': copy.deepcopy(self.selection)})
        reports = self.record['installationReports']
        if reports and {k: v for k, v in reports[-1].items() if k != 'observedAt'} == {k: v for k, v in result.items() if k != 'observedAt'}:
            result = reports[-1]
        else:
            if len(reports) >= 2:
                raise ValueError('Original ' + self.LABEL + ' report bound exceeded')
            reports.append(result)
            self.save(self.record)
        platform.write_record(self.config.state_dir / 'installation.json', result)

    def status(self):
        if self.record is None or self.record.get('transition'):
            raise ValueError('Complete ' + self.LABEL + ' phase required')
        self.observe(self.record['completed'])
        return {'version': self.VERSION, 'phase': self.record['completed'], 'catalogSize': len(self.catalog),
            'issueIntakeEnabled': self.record['completed'] == 'active', 'modelUpstream': self.record['upstream'],
            'approvalsValidUntil': min(v['expiresAt'] for v in self.record['approvals'].values()),
            'administrativeDeadline': int(self.record['expiresAt']*1000),
            'previousWorkflowAuthorityRenewed': False, 'workflowEvidence': False,
            'autonomousEndToEndValidated': False}
