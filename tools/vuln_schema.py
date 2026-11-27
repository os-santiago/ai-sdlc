"""Canonical vulnerability finding schema (ai-sdlc.vulnerability-finding/v1).

Provider reports differ; everything after parsing and triage speaks this one
schema. Validation is strict: unknown keys, out-of-range values, malformed
identifiers or oversized text are refused, never repaired. Standard library
only, so the module runs unchanged in the pinned Tekton Python image.
"""
import hashlib
import json
import re
import unicodedata

FINDING_SCHEMA = 'ai-sdlc.vulnerability-finding/v1'
OBSERVATION_SCHEMA = 'ai-sdlc.vulnerability-observation/v1'
FINDINGS_DOCUMENT = 'ai-sdlc.vulnerability-findings/v1'

SEVERITIES = ('critical', 'high', 'medium', 'low', 'note', 'unknown')
SEVERITY_RANK = {name: rank for rank, name in enumerate(reversed(SEVERITIES))}
CATEGORIES = ('code', 'dependency')
FIX_TYPES = ('code-change', 'dependency-update')
DISPOSITIONS = ('actionable', 'likely-false-positive', 'needs-human')
TRIAGE_METHODS = ('deterministic', 'model')
REACHABILITY = ('yes', 'no', 'unknown')
SCOPES = ('runtime', 'development', 'unknown')
RELATIONSHIPS = ('direct', 'transitive', 'unknown')
PROVIDERS = ('github-code-scanning', 'dependabot', 'sarif', 'file')

CWE = re.compile(r'CWE-[1-9][0-9]{0,5}')
CVE = re.compile(r'CVE-[0-9]{4}-[0-9]{4,7}')
GHSA = re.compile(r'GHSA(?:-[23456789cfghjmpqrvwx]{4}){3}')
RULE = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.:/@+-]{0,127}')
REPOSITORY = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9_.-]{1,100}')
REVISION = re.compile(r'[0-9a-f]{40}')
PATH = re.compile(r'[A-Za-z0-9_.@+-]+(?:/[A-Za-z0-9_.@+ -]+)*')
MODULE = re.compile(r'[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*')
ECOSYSTEM = re.compile(r'[a-z0-9][a-z0-9_.-]{0,31}')
PACKAGE = re.compile(r'(?:@[A-Za-z0-9][A-Za-z0-9._-]{0,100}/)?[A-Za-z0-9][A-Za-z0-9._:/+-]{0,200}')
VERSION = re.compile(r'[0-9A-Za-z][0-9A-Za-z.+_~:-]{0,63}')
VERSION_RANGE = re.compile(r'[0-9A-Za-z.,<>=!~^*|+_ -]{1,128}')
PROVIDER_ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.:/#-]{0,127}')
URL = re.compile(r'https://[A-Za-z0-9.-]+(?::[0-9]{1,5})?/[A-Za-z0-9._~/%#?=&+-]{0,400}')
FINGERPRINT = re.compile(r'[0-9a-f]{64}')
FINDING_ID = re.compile(r'vf-[0-9a-f]{16}')

MAX_SOURCES = 200
MAX_EVIDENCE = 5
MAX_TEXT = 300
MAX_TITLE = 120


class SchemaError(ValueError):
    """A document does not satisfy the canonical schema."""


def _fail(where, message):
    raise SchemaError(f'{where}: {message}')


_INVISIBLE = {'Cc', 'Cf', 'Co', 'Cs', 'Zl', 'Zp'}
OVERRIDE = re.compile(r'(?i)\b(ignore|disregard|forget)\b[^.\n]{0,40}?\b(instructions?|prompts?|rules?|polic(?:y|ies))\b')


def clean_text(value, limit=MAX_TEXT):
    """Provider or model text reduced to one bounded, printable line.

    Control and format characters (including bidirectional overrides) are
    removed, whitespace collapses, '@' becomes a full-width at sign so it cannot form a GitHub mention, angle
    brackets cannot open HTML or close an untrusted fence, and backticks
    cannot open code spans. The result is data for display, never markup."""
    if not isinstance(value, str):
        return ''
    text = ''.join(ch if unicodedata.category(ch) not in _INVISIBLE else ' ' for ch in value)
    text = re.sub(r'\s+', ' ', text).strip()
    text = (text.replace('@', '＠').replace('<', '‹').replace('>', '›')
            .replace('`', "'").replace('[', '(').replace(']', ')'))
    text = OVERRIDE.sub('(redacted directive)', text)
    if len(text) > limit:
        text = text[:limit - 1].rstrip() + '…'
    return text


def severity_max(values):
    ranked = [v for v in values if v in SEVERITY_RANK]
    return max(ranked, key=SEVERITY_RANK.__getitem__) if ranked else 'unknown'


def severity_at_least(value, threshold):
    return SEVERITY_RANK.get(value, 0) >= SEVERITY_RANK[threshold]


def primary_cwe(cwes):
    """The fingerprint weakness: the lowest-numbered CWE (usually the most
    general, so two tools that tag different children still agree)."""
    return min(cwes, key=lambda c: int(c.split('-')[1])) if cwes else None


def fingerprint_key(category, *, path=None, weakness=None, ecosystem=None, manifest=None,
                    package=None, version=None, fixed_version=None):
    """The stable identity of a finding, independent of provider and alert numbers.

    code:       weakness (primary CWE, else rule id) and file path;
    dependency: ecosystem, manifest, package, installed version and the
                version that fixes it: two installed versions of one package
                (ajv 6.x and 8.x) are separate findings with separate fixes,
                and a new advisory that needs a newer version is a new finding.
    """
    if category == 'code':
        if not path or not weakness:
            raise SchemaError('code fingerprint needs a path and a weakness')
        parts = ['code', weakness, path]
    elif category == 'dependency':
        if not ecosystem or not package:
            raise SchemaError('dependency fingerprint needs an ecosystem and a package')
        parts = ['dependency', ecosystem, manifest or '', package, version or '', fixed_version or '']
    else:
        raise SchemaError('unknown category')
    return '\x1f'.join(parts)


def fingerprint(repository, key):
    return hashlib.sha256(f'{repository}\x1e{key}'.encode()).hexdigest()


def finding_id(fp):
    return 'vf-' + fp[:16]


def _exact_keys(where, value, required, optional=()):
    if not isinstance(value, dict):
        _fail(where, 'must be an object')
    missing = set(required) - set(value)
    unknown = set(value) - set(required) - set(optional)
    if missing:
        _fail(where, f'missing {sorted(missing)}')
    if unknown:
        _fail(where, f'unknown {sorted(unknown)}')


def _string(where, value, pattern=None, limit=MAX_TEXT, allow_empty=False):
    if not isinstance(value, str) or (not value and not allow_empty) or len(value) > limit:
        _fail(where, 'must be a bounded string')
    if pattern is not None and value and not pattern.fullmatch(value):
        _fail(where, f'has an invalid format: {value[:60]!r}')
    if value and clean_text(value, limit) != value and pattern is None:
        _fail(where, 'must be clean display text')


def _optional(where, value, pattern, limit=MAX_TEXT):
    if value is not None:
        _string(where, value, pattern, limit)


def _string_list(where, value, pattern, limit, unique=True):
    if not isinstance(value, list) or len(value) > limit:
        _fail(where, f'must be a list of at most {limit}')
    for i, item in enumerate(value):
        _string(f'{where}[{i}]', item, pattern)
    if unique and (len(set(value)) != len(value) or value != sorted(value)):
        _fail(where, 'must be sorted and unique')


def _enum(where, value, allowed):
    if value not in allowed:
        _fail(where, f'must be one of {list(allowed)}')


def _int(where, value, low, high):
    if type(value) is not int or not low <= value <= high:
        _fail(where, f'must be an integer from {low} to {high}')


def validate_source(where, source):
    _exact_keys(where, source, ('provider', 'tool', 'providerId'), ('url',))
    _enum(f'{where}.provider', source['provider'], PROVIDERS)
    _string(f'{where}.tool', source['tool'], RULE, 64)
    _string(f'{where}.providerId', source['providerId'], PROVIDER_ID, 128)
    _optional(f'{where}.url', source.get('url'), URL, 512)


def validate_component(where, component, category):
    if category == 'code':
        _exact_keys(where, component, ('kind', 'path', 'lines'))
        if component['kind'] != 'file':
            _fail(f'{where}.kind', 'must be file for a code finding')
        _string(f'{where}.path', component['path'], PATH, 512)
        if any(part in ('.', '..') for part in component['path'].split('/')):
            _fail(f'{where}.path', 'must be a normalized relative path')
        lines = component['lines']
        if not isinstance(lines, list) or len(lines) > 50:
            _fail(f'{where}.lines', 'must be a list of at most 50 ranges')
        for i, line in enumerate(lines):
            _exact_keys(f'{where}.lines[{i}]', line, ('start', 'end'))
            _int(f'{where}.lines[{i}].start', line['start'], 1, 10_000_000)
            _int(f'{where}.lines[{i}].end', line['end'], line['start'], 10_000_000)
    else:
        _exact_keys(where, component, ('kind', 'ecosystem', 'name', 'versions', 'manifest', 'fixedVersion',
                                       'vulnerableRange', 'scope', 'relationship'))
        if component['kind'] != 'package':
            _fail(f'{where}.kind', 'must be package for a dependency finding')
        _string(f'{where}.ecosystem', component['ecosystem'], ECOSYSTEM, 32)
        _string(f'{where}.name', component['name'], PACKAGE, 214)
        _string_list(f'{where}.versions', component['versions'], VERSION, 20)
        _optional(f'{where}.manifest', component['manifest'], PATH, 512)
        _optional(f'{where}.fixedVersion', component['fixedVersion'], VERSION, 64)
        _optional(f'{where}.vulnerableRange', component['vulnerableRange'], VERSION_RANGE, 128)
        _enum(f'{where}.scope', component['scope'], SCOPES)
        _enum(f'{where}.relationship', component['relationship'], RELATIONSHIPS)


FINDING_KEYS = ('schema', 'id', 'fingerprint', 'repository', 'revision', 'category', 'title', 'sources',
                'identifiers', 'severity', 'component', 'module', 'evidence', 'fixType', 'disposition',
                'correlation', 'confidence', 'triage')


def validate_finding(finding, where='finding'):
    """Validate one canonical finding. Returns it unchanged or raises SchemaError."""
    _exact_keys(where, finding, FINDING_KEYS)
    if finding['schema'] != FINDING_SCHEMA:
        _fail(f'{where}.schema', f'must be {FINDING_SCHEMA}')
    _string(f'{where}.repository', finding['repository'], REPOSITORY, 201)
    _optional(f'{where}.revision', finding['revision'], REVISION, 40)
    _enum(f'{where}.category', finding['category'], CATEGORIES)
    _string(f'{where}.title', finding['title'], None, MAX_TITLE)
    sources = finding['sources']
    if not isinstance(sources, list) or not 1 <= len(sources) <= MAX_SOURCES:
        _fail(f'{where}.sources', f'must hold 1 to {MAX_SOURCES} sources')
    for i, source in enumerate(sources):
        validate_source(f'{where}.sources[{i}]', source)
    keys = [(s['provider'], s['providerId']) for s in sources]
    if len(set(keys)) != len(keys) or keys != sorted(keys):
        _fail(f'{where}.sources', 'must be sorted and unique by provider and providerId')
    ids = finding['identifiers']
    _exact_keys(f'{where}.identifiers', ids, ('cwe', 'cve', 'ghsa', 'rules'))
    _string_list(f'{where}.identifiers.cwe', ids['cwe'], CWE, 20)
    _string_list(f'{where}.identifiers.cve', ids['cve'], CVE, 100)
    _string_list(f'{where}.identifiers.ghsa', ids['ghsa'], GHSA, 100)
    _string_list(f'{where}.identifiers.rules', ids['rules'], RULE, 20)
    _enum(f'{where}.severity', finding['severity'], SEVERITIES)
    validate_component(f'{where}.component', finding['component'], finding['category'])
    _optional(f'{where}.module', finding['module'], MODULE, 200)
    evidence = finding['evidence']
    if not isinstance(evidence, list) or not 1 <= len(evidence) <= MAX_EVIDENCE:
        _fail(f'{where}.evidence', f'must hold 1 to {MAX_EVIDENCE} items')
    for i, item in enumerate(evidence):
        _string(f'{where}.evidence[{i}]', item, None, MAX_TEXT)
    _enum(f'{where}.fixType', finding['fixType'], FIX_TYPES)
    expected_fix = 'code-change' if finding['category'] == 'code' else 'dependency-update'
    if finding['fixType'] != expected_fix:
        _fail(f'{where}.fixType', f'a {finding["category"]} finding is fixed by {expected_fix}')
    _enum(f'{where}.disposition', finding['disposition'], DISPOSITIONS)
    correlation = finding['correlation']
    _exact_keys(f'{where}.correlation', correlation, ('pathExists', 'reachable'))
    if correlation['pathExists'] not in (True, False, None):
        _fail(f'{where}.correlation.pathExists', 'must be true, false or null')
    _enum(f'{where}.correlation.reachable', correlation['reachable'], REACHABILITY)
    confidence = finding['confidence']
    if type(confidence) not in (int, float) or not 0 <= confidence <= 1:
        _fail(f'{where}.confidence', 'must be a number from 0 to 1')
    triage = finding['triage']
    _exact_keys(f'{where}.triage', triage, ('method', 'model'))
    _enum(f'{where}.triage.method', triage['method'], TRIAGE_METHODS)
    if triage['method'] == 'model':
        _string(f'{where}.triage.model', triage['model'], RULE, 128)
    elif triage['model'] is not None:
        _fail(f'{where}.triage.model', 'must be null for deterministic triage')
    _string(f'{where}.fingerprint', finding['fingerprint'], FINGERPRINT, 64)
    if finding['id'] != finding_id(finding['fingerprint']):
        _fail(f'{where}.id', 'must be derived from the fingerprint')
    expected = fingerprint(finding['repository'], finding_key(finding))
    if finding['fingerprint'] != expected:
        _fail(f'{where}.fingerprint', 'does not match the finding identity')
    return finding


def finding_key(finding):
    component = finding['component']
    if finding['category'] == 'code':
        weakness = primary_cwe(finding['identifiers']['cwe']) or (finding['identifiers']['rules'] or [None])[0]
        return fingerprint_key('code', path=component['path'], weakness=weakness)
    return fingerprint_key('dependency', ecosystem=component['ecosystem'], manifest=component['manifest'],
                           package=component['name'], version=','.join(component['versions']),
                           fixed_version=component['fixedVersion'])


def validate_document(document):
    """Validate a findings document: the triage stage's output artifact."""
    _exact_keys('document', document, ('schema', 'repository', 'revision', 'findings', 'rejected'))
    if document['schema'] != FINDINGS_DOCUMENT:
        _fail('document.schema', f'must be {FINDINGS_DOCUMENT}')
    _string('document.repository', document['repository'], REPOSITORY, 201)
    _optional('document.revision', document['revision'], REVISION, 40)
    findings = document['findings']
    if not isinstance(findings, list) or len(findings) > 2000:
        _fail('document.findings', 'must be a list of at most 2000 findings')
    seen = set()
    for i, finding in enumerate(findings):
        validate_finding(finding, f'document.findings[{i}]')
        if finding['repository'] != document['repository']:
            _fail(f'document.findings[{i}].repository', 'must equal the document repository')
        if finding['fingerprint'] in seen:
            _fail(f'document.findings[{i}].fingerprint', 'is duplicated')
        seen.add(finding['fingerprint'])
    rejected = document['rejected']
    if not isinstance(rejected, list) or len(rejected) > 2000:
        _fail('document.rejected', 'must be a list')
    for i, item in enumerate(rejected):
        _exact_keys(f'document.rejected[{i}]', item, ('observation', 'reason'))
        _string(f'document.rejected[{i}].observation', item['observation'], PROVIDER_ID, 128)
        _string(f'document.rejected[{i}].reason', item['reason'], None, MAX_TEXT)
    return document


def dumps(value):
    """Canonical JSON for artifacts: sorted keys, UTF-8, trailing newline."""
    return json.dumps(value, sort_keys=True, ensure_ascii=False, indent=1) + '\n'
