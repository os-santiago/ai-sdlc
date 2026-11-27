"""Deterministic issue rendering, policy and de-duplication.

Each canonical finding, or a bounded group of dependency findings of one
manifest, becomes one issue in AI-SDLC's issue format
(spec/definition-of-ready.md): a conventional title, a short description,
context, `## Acceptance criteria` with test-verifiable bullets, and the
provider references. One hidden marker per finding,

    <!-- ai-sdlc-finding: <fingerprint> -->

lets a re-run recognize every finding already filed (open or closed), so it
never files it twice.

Only report-stated facts and fixed templates reach an issue. Model free
text never does: the issue is the next agent's task input.

The trigger label is applied only by policy. Everything else is left for
human triage with a different label. Dependency updates are handed to the
deterministic lockfile-update Tekton pipeline, not to generation.
"""
import copy
import json
import re

import vuln_schema as schema

POLICY_SCHEMA = 'ai-sdlc.vulnerability-intake-policy/v1'
PLAN_SCHEMA = 'ai-sdlc.vulnerability-issue-plan/v1'
MARKER = re.compile(r'<!-- ai-sdlc-finding: ([0-9a-f]{64}) -->')
LABEL = re.compile(r'[^,\n\r]{1,50}')
MAX_BODY = 60_000

DEFAULT_POLICY = {
    'schema': POLICY_SCHEMA,
    # Findings below this severity are recorded in the triage artifact but not filed.
    'minSeverity': 'medium',
    'triggerLabel': 'ai-sdlc:shadow',
    'trigger': {'minSeverity': 'high', 'fixTypes': ['code-change'], 'dispositions': ['actionable'],
                'minConfidence': 0.7, 'requirePathExists': True},
    'labels': {'base': ['security', 'vulnerability-intake'], 'triage': 'needs-triage',
               'dependency': 'dependency-update'},
    # The npm lockfile-update task accepts at most 16 packages per run.
    'dependencyGroupSize': 16,
    'maxIssuesPerRun': 25,
    # The account that files the intake's issues. Only its issues are trusted to carry
    # de-duplication markers; anyone else could suppress a finding by pasting a marker.
    'intakeLogin': None,
}

LOCK_UPDATE = {'npm': {'pipeline': 'ai-sdlc-npm-lock-update', 'lockfiles': ('package-lock.json', 'npm-shrinkwrap.json')}}


class PolicyError(ValueError):
    pass


def load_policy(value=None):
    """A policy document merged over the defaults, then validated."""
    policy = copy.deepcopy(DEFAULT_POLICY)
    if value:
        if not isinstance(value, dict) or value.get('schema') != POLICY_SCHEMA:
            raise PolicyError(f'Policy must declare schema {POLICY_SCHEMA}')
        unknown = set(value) - set(DEFAULT_POLICY)
        if unknown:
            raise PolicyError(f'Unknown policy keys {sorted(unknown)}')
        for key, item in value.items():
            if isinstance(item, dict):
                extra = set(item) - set(DEFAULT_POLICY[key])
                if extra:
                    raise PolicyError(f'Unknown policy keys {key}.{sorted(extra)}')
                policy[key].update(item)
            else:
                policy[key] = item
    trigger = policy['trigger']
    if (policy['minSeverity'] not in schema.SEVERITIES or trigger['minSeverity'] not in schema.SEVERITIES
            or not set(trigger['fixTypes']) <= set(schema.FIX_TYPES)
            or not set(trigger['dispositions']) <= set(schema.DISPOSITIONS)
            or type(trigger['minConfidence']) not in (int, float) or not 0 <= trigger['minConfidence'] <= 1
            or type(trigger['requirePathExists']) is not bool
            or type(policy['dependencyGroupSize']) is not int or not 1 <= policy['dependencyGroupSize'] <= 16
            or type(policy['maxIssuesPerRun']) is not int or not 0 <= policy['maxIssuesPerRun'] <= 200
            or not (policy['intakeLogin'] is None or (isinstance(policy['intakeLogin'], str)
                                                       and re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:\[bot\])?',
                                                                        policy['intakeLogin'])))):
        raise PolicyError('Policy values are out of range')
    labels = [policy['triggerLabel'], policy['labels']['triage'], policy['labels']['dependency'],
              *policy['labels']['base']]
    if not all(isinstance(label, str) and LABEL.fullmatch(label) for label in labels):
        raise PolicyError('Policy labels are invalid')
    if policy['triggerLabel'] in (policy['labels']['triage'], policy['labels']['dependency'], *policy['labels']['base']):
        raise PolicyError('The trigger label cannot double as another label')
    return policy


def triggers(finding, policy):
    """Whether policy arms a finding for the autonomous flow."""
    rule = policy['trigger']
    return (schema.severity_at_least(finding['severity'], rule['minSeverity'])
            and finding['fixType'] in rule['fixTypes']
            and finding['disposition'] in rule['dispositions']
            and finding['confidence'] >= rule['minConfidence']
            and (finding['correlation']['pathExists'] is True or not rule['requirePathExists'])
            and all(s['provider'] != 'file' or s['tool'] != 'unknown' for s in finding['sources']))


# ---------------------------------------------------------------- templates

def _code(text):
    """Inline code that cannot be closed by its content."""
    return '`' + text.replace('`', "'") + '`'


def _lines(component):
    starts = sorted({r['start'] for r in component['lines']})
    if not starts:
        return 'the reported location'
    return ('line ' if len(starts) == 1 else 'lines ') + ', '.join(str(s) for s in starts[:12])


def acceptance_code(finding):
    path = finding['component']['path']
    where = f'{_code(path)} at {_lines(finding["component"])}'
    cwe = set(finding['identifiers']['cwe'])
    if cwe & {'CWE-79', 'CWE-80', 'CWE-116'}:
        return [
            f'{where} no longer inserts a data-derived value into the page as HTML: the value is inserted as text '
            '(for example with `textContent`) or HTML-escaped before insertion.',
            'A test renders the affected view with a value containing `<img src=x onerror=alert(1)>` and asserts '
            'that the markup appears as escaped text and no element is created from it.',
        ]
    if 'CWE-117' in cwe:
        return [
            f'{where} removes or encodes carriage-return and line-feed characters from user-provided values before '
            'they are written to a log entry.',
            'A test logs a value containing a newline followed by a forged entry and asserts that exactly one log '
            'entry is produced and that it contains no raw line break.',
        ]
    if cwe & {'CWE-829', 'CWE-830'}:
        return [
            f'Every external script or stylesheet element in {_code(path)} carries an `integrity` attribute with a '
            '`sha384-` or `sha512-` digest and `crossorigin="anonymous"`, or the resource is served from the '
            'application itself.',
            f'A test reads {_code(path)} and asserts that every `<script src>` and `<link rel="stylesheet">` that '
            'references another origin has a non-empty `integrity` attribute.',
        ]
    rule = (finding['identifiers']['rules'] or ['the reported rule'])[0]
    return [
        f'The weakness reported by {_code(rule)} ({", ".join(finding["identifiers"]["cwe"]) or "no CWE"}) is removed '
        f'from {where}.',
        'A test exercises the reported input path with a malicious value and asserts the safe behavior.',
    ]


def acceptance_dependencies(manifest, findings):
    directory = manifest.rsplit('/', 1)[0] if '/' in manifest else '.'
    targets = ', '.join(f'{_code(f["component"]["name"])} {", ".join(f["component"]["versions"]) or "any"} → ≥ '
                        f'{f["component"]["fixedVersion"]}' for f in findings)
    return [
        f'{_code(manifest)} no longer resolves any listed package to a vulnerable version; each listed installed '
        f'version is replaced by its fixed version or later: {targets}.',
        f'Dependency ranges in {_code(directory + "/package.json" if directory != "." else "package.json")} change '
        'only where a fixed version is outside the current range.',
    ]


# ---------------------------------------------------------------- rendering

PROVIDER_NAMES = {'github-code-scanning': 'GitHub code scanning alert', 'dependabot': 'Dependabot alert',
                  'sarif': 'SARIF result', 'file': 'Report finding'}


def _references(findings):
    out = []
    for finding in findings:
        for source in finding['sources']:
            label = f'{PROVIDER_NAMES[source["provider"]]} {source["tool"]} {source["providerId"]}'
            out.append(f'- [{schema.clean_text(label, 160)}]({source["url"]})' if source.get('url') else f'- {schema.clean_text(label, 160)}')
        for ghsa in finding['identifiers']['ghsa']:
            out.append(f'- [{ghsa}](https://github.com/advisories/{ghsa})')
    unique = list(dict.fromkeys(out))
    if len(unique) > 60:
        unique = unique[:60] + [f'- … and {len(unique) - 60} more references in the triage artifact']
    return unique


def _markers(findings):
    return [f'<!-- ai-sdlc-finding: {f["fingerprint"]} -->' for f in findings]


def _title(text):
    return schema.clean_text(text, 90)


def render_code(finding, policy):
    component = finding['component']
    module = finding['module']
    short = (finding['title'].split(' in ', 1)[0] or 'Security weakness')
    lead = short[:1].lower() + short[1:] if not short[:2].isupper() else short
    title = _title(f'fix(security): {lead} in {module or component["path"].rsplit("/", 1)[-1]}')
    sources = sorted({s['tool'] for s in finding['sources']})
    armed = triggers(finding, policy)
    if armed:
        # An armed issue is the next agent's task input: no untrusted scanner prose in it,
        # only the rule identity and location.
        rules = ', '.join(_code(r) for r in finding['identifiers']['rules']) or 'no rule id'
        evidence = ['Reported by ' + ', '.join(sources) + f' ({rules}) at {_code(component["path"])}, '
                    f'{_lines(component)}. Scanner messages are kept in the triage artifact.']
    else:
        evidence = ['Reported evidence (scanner text, quoted as data):', '',
                    *[f'> {item}' for item in finding['evidence']]]
    lines = [
        f'{short} reported by {", ".join(sources)} in {_code(component["path"])} ({_lines(component)}). '
        f'Severity {finding["severity"]}. Fix it in the code so the weakness no longer exists.',
        '',
        '## Context',
        '',
        f'- Module: {_code(module) if module else "not determined"}',
        f'- File: {_code(component["path"])}, {_lines(component)}',
        f'- Weakness: {", ".join(finding["identifiers"]["cwe"]) or "not classified"}'
        + (f' (rules {", ".join(_code(r) for r in finding["identifiers"]["rules"])})' if finding['identifiers']['rules'] else ''),
        f'- Severity: {finding["severity"]}; triage: {finding["disposition"]}, confidence {finding["confidence"]:.2f} '
        f'({finding["triage"]["method"]})',
        '',
        *evidence,
        '',
        '## Acceptance criteria',
        '',
        *[f'- {item}' for item in acceptance_code(finding)],
        '',
        '## References',
        '',
        *_references([finding]),
        '',
        *_markers([finding]),
    ]
    labels = list(policy['labels']['base']) + [policy['triggerLabel'] if armed else policy['labels']['triage']]
    return {'title': title, 'body': '\n'.join(lines) + '\n', 'labels': labels, 'armed': armed,
            'findings': [finding['fingerprint']], 'severity': finding['severity'], 'kind': 'code-change'}


def render_dependencies(manifest, ecosystem, findings, policy, part=None):
    severity = schema.severity_max(f['severity'] for f in findings)
    directory = manifest.rsplit('/', 1)[0] if '/' in manifest else '.'
    title = _title(f'fix(deps): update {len(findings)} vulnerable {ecosystem} packages in {directory} ({severity})')
    lock = LOCK_UPDATE.get(ecosystem)
    fixable = lock and manifest.rsplit('/', 1)[-1] in lock['lockfiles']
    table = ['| Package | Severity | Current | Fixed in | Scope | Advisories |', '| --- | --- | --- | --- | --- | --- |']
    for f in findings:
        c = f['component']
        ids = ', '.join(f['identifiers']['ghsa'][:3] + f['identifiers']['cve'][:3]) or '-'
        table.append(f'| {_code(c["name"])} | {f["severity"]} | {", ".join(c["versions"]) or "?"} | '
                     f'{c["fixedVersion"]} | {c["scope"]}, {c["relationship"]} | {ids} |')
    if fixable:
        packages = [f'{f["component"]["name"]}@{f["component"]["fixedVersion"]}' for f in findings]
        handoff = [
            f'This is a dependency update. It is produced by the deterministic {_code(lock["pipeline"])} Tekton '
            f'pipeline (scripts disabled, lockfile only), not by code generation: run it with '
            f'{_code("directory=" + directory)}, {_code("mode=update")} and the packages '
            f'{", ".join(_code(p) for p in packages)}.',
        ]
        marker = [f'<!-- ai-sdlc-lock-update: {json.dumps({"pipeline": lock["pipeline"], "directory": directory, "mode": "update", "packages": packages}, separators=(",", ":"))} -->']
    else:
        handoff = [f'No deterministic update task exists yet for {ecosystem} manifests such as {_code(manifest)}; '
                   'a maintainer chooses how to apply the update.']
        marker = []
    lines = [
        f'{len(findings)} {ecosystem} package(s) resolved by {_code(manifest)} have published security advisories '
        f'(highest severity {severity}). Update each to at least its fixed version.',
        '',
        *handoff,
        '',
        '## Context',
        '',
        *table,
        '',
        '## Acceptance criteria',
        '',
        *[f'- {item}' for item in acceptance_dependencies(manifest, findings)],
        '',
        '## References',
        '',
        *_references(findings),
        '',
        *marker,
        *_markers(findings),
    ]
    labels = list(policy['labels']['base']) + [policy['labels']['dependency']]
    return {'title': title, 'body': '\n'.join(lines) + '\n', 'labels': labels, 'armed': False,
            'findings': [f['fingerprint'] for f in findings], 'severity': severity, 'kind': 'dependency-update',
            'lockUpdate': json.loads(marker[0][len('<!-- ai-sdlc-lock-update: '):-4]) if marker else None}


def render_unfixable(manifest, ecosystem, findings, policy):
    severity = schema.severity_max(f['severity'] for f in findings)
    directory = manifest.rsplit('/', 1)[0] if '/' in manifest else '.'
    title = _title(f'security: assess {len(findings)} {ecosystem} packages without a fixed version in {directory}')
    lines = [
        f'{len(findings)} package(s) resolved by {_code(manifest)} have advisories without a published fixed '
        f'version (highest severity {severity}). A maintainer decides whether to replace, remove or accept them.',
        '',
        '## Context',
        '',
        *[f'- {_code(f["component"]["name"])}: {f["severity"]}; ' + '; '.join(f['evidence'][:2]) for f in findings],
        '',
        '## Acceptance criteria',
        '',
        f'- Each listed package is replaced, removed from {_code(manifest)}, or recorded as an accepted risk with a reason.',
        '',
        '## References',
        '',
        *_references(findings),
        '',
        *_markers(findings),
    ]
    labels = list(policy['labels']['base']) + [policy['labels']['triage']]
    return {'title': title, 'body': '\n'.join(lines) + '\n', 'labels': labels, 'armed': False,
            'findings': [f['fingerprint'] for f in findings], 'severity': severity, 'kind': 'human-triage'}


def existing_fingerprints(issues, login=None):
    """Fingerprint -> issue number from issue bodies (open and closed).

    With a login, only issues authored by that account count."""
    found = {}
    for issue in issues:
        if not isinstance(issue, dict) or 'pull_request' in issue or type(issue.get('number')) is not int:
            continue
        if login is not None and ((issue.get('user') or {}).get('login') or '').lower() != login.lower():
            continue
        for fp in MARKER.findall(issue.get('body') or ''):
            found.setdefault(fp, {'number': issue['number'], 'state': issue.get('state')})
    return found


def plan(document, existing_issues, policy):
    """The issues to create, in priority order, and what was skipped and why."""
    schema.validate_document(document)
    known = existing_fingerprints(existing_issues, policy['intakeLogin'])
    warnings = [] if policy['intakeLogin'] else [
        'intakeLogin is not set: de-duplication markers are trusted from any issue author']
    skipped, code, deps = [], [], {}
    for finding in document['findings']:
        if finding['fingerprint'] in known:
            skipped.append({'finding': finding['id'], 'reason': f'already filed as #{known[finding["fingerprint"]]["number"]}'
                                                                f' ({known[finding["fingerprint"]]["state"]})'})
        elif not schema.severity_at_least(finding['severity'], policy['minSeverity']):
            skipped.append({'finding': finding['id'], 'reason': f'severity {finding["severity"]} below policy minimum'})
        elif finding['category'] == 'code':
            code.append(finding)
        else:
            c = finding['component']
            deps.setdefault((c['ecosystem'], c['manifest'] or 'unknown-manifest', c['fixedVersion'] is not None),
                            []).append(finding)
    issues = [render_code(f, policy) for f in code]
    for (ecosystem, manifest, fixable), members in sorted(deps.items()):
        members.sort(key=lambda f: (-schema.SEVERITY_RANK[f['severity']], f['component']['name']))
        if not fixable:
            issues.append(render_unfixable(manifest, ecosystem, members, policy))
            continue
        size = policy['dependencyGroupSize']
        for start in range(0, len(members), size):
            issues.append(render_dependencies(manifest, ecosystem, members[start:start + size], policy))
    issues.sort(key=lambda i: (not i['armed'], -schema.SEVERITY_RANK[i['severity']], i['kind'], i['title']))
    deferred = issues[policy['maxIssuesPerRun']:]
    issues = issues[:policy['maxIssuesPerRun']]
    for issue in issues:
        if len(issue['body']) > MAX_BODY:
            raise PolicyError('Rendered issue body exceeds the bound')
    return {'schema': PLAN_SCHEMA, 'repository': document['repository'], 'issues': issues,
            'deferred': [{'title': i['title'], 'findings': i['findings']} for i in deferred], 'skipped': skipped,
            'warnings': warnings}


def list_intake_issues(client, repository, policy):
    """Issues that may carry markers, open or closed: those filed by the intake account
    (even if a label was removed), or without one, those carrying the intake label."""
    label = policy['labels']['base'][-1] if policy['labels']['base'] else None
    query = 'state=all&per_page=100'
    if policy['intakeLogin']:
        query += f'&creator={policy["intakeLogin"]}'
    elif label:
        query += f'&labels={label}'
    return client.get_pages(f'/repos/{repository}/issues?{query}', max_pages=20)


def create(client, repository, issue_plan):
    """File the planned issues. Returns the created issue numbers."""
    created = []
    for issue in issue_plan['issues']:
        status, value = client.post_json(f'/repos/{repository}/issues',
                                         {'title': issue['title'], 'body': issue['body'], 'labels': issue['labels']})
        if status != 201 or not isinstance(value, dict) or type(value.get('number')) is not int:
            raise RuntimeError('Issue creation failed')
        created.append({'number': value['number'], 'title': issue['title'], 'findings': issue['findings']})
    return created
