"""Deterministic parsers from provider reports to observations.

An observation (ai-sdlc.vulnerability-observation/v1) is one provider result
in a uniform shape: no interpretation beyond what the format states. Known
formats are parsed here without a model:

- SARIF 2.1.0 (CodeQL, and any scanner that emits SARIF: Trivy, OWASP
  Dependency-Check, Semgrep, SonarQube exports, ...);
- the GitHub code scanning alerts API;
- the GitHub Dependabot alerts API.

Anything else becomes a bounded `document` observation that only the
generative triage stage can interpret. Every text field from a report is
untrusted: it is cleaned (vuln_schema.clean_text) and bounded here, and it is
fenced again before any prompt.
"""
import json
import re

import vuln_schema as schema

MAX_OBSERVATIONS = 5000
MAX_DOCUMENT_CHARS = 48_000
CVSS_BANDS = ((9.0, 'critical'), (7.0, 'high'), (4.0, 'medium'), (0.1, 'low'))
# Without a security-severity, SARIF levels describe rule precision, not risk.
LEVEL_SEVERITY = {'error': 'medium', 'warning': 'low', 'note': 'note', 'none': 'note'}
ADVISORY_SEVERITY = {'critical': 'critical', 'high': 'high', 'moderate': 'medium', 'medium': 'medium',
                     'low': 'low'}
CWE_TAG = re.compile(r'(?i)^external/cwe/cwe-0*([1-9][0-9]{0,5})$')
TRIVY_PACKAGE = re.compile(r'Package: (\S+)\s+Installed Version: (\S+)(?:.*?Fixed Version: ([^\s]*))?', re.S)
LOCKFILE_ECOSYSTEM = {'package-lock.json': 'npm', 'npm-shrinkwrap.json': 'npm', 'yarn.lock': 'npm',
                      'pnpm-lock.yaml': 'npm', 'pom.xml': 'maven', 'build.gradle': 'maven', 'go.sum': 'go',
                      'go.mod': 'go', 'requirements.txt': 'pip', 'poetry.lock': 'pip', 'Cargo.lock': 'rust',
                      'Gemfile.lock': 'rubygems'}


class ReportError(ValueError):
    """A report is malformed or exceeds a bound."""


def severity_from_score(score):
    try:
        value = float(score)
    except (TypeError, ValueError):
        return None
    if not 0 <= value <= 10:
        return None
    for floor, name in CVSS_BANDS:
        if value >= floor:
            return name
    return 'note'


def normalize_path(uri):
    """A repository-relative path from a SARIF URI or API path, or None."""
    if not isinstance(uri, str) or not uri:
        return None
    path = re.sub(r'^file://', '', uri)
    for prefix in ('/github/workspace/', '/workspace/source/'):
        if path.startswith(prefix):
            path = path[len(prefix):]
    path = path.lstrip('/')
    while path.startswith('./'):
        path = path[2:]
    path = path.replace('%20', ' ')
    if not schema.PATH.fullmatch(path) or len(path) > 512 or any(p in ('.', '..') for p in path.split('/')):
        return None
    return path


def _ids(pattern, values):
    found = set()
    for value in values:
        if isinstance(value, str):
            found.update(pattern.findall(value.upper() if pattern is not schema.GHSA else value))
    return sorted(found)


def _ghsa(values):
    found = set()
    for value in values:
        if isinstance(value, str):
            for match in re.findall(r'(?i)GHSA(?:-[23456789cfghjmpqrvwx]{4}){3}', value):
                found.add('GHSA' + match[4:].lower())
    return sorted(found)


def cwe_from_tags(tags):
    out = set()
    for tag in tags or []:
        match = CWE_TAG.match(tag) if isinstance(tag, str) else None
        if match:
            out.add(f'CWE-{int(match.group(1))}')
    return sorted(out)


def _line_range(start, end):
    if type(start) is not int or start < 1:
        return []
    end = end if type(end) is int and end >= start else start
    return [{'start': start, 'end': end}]


def _package_ecosystem(path):
    name = (path or '').rsplit('/', 1)[-1]
    return LOCKFILE_ECOSYSTEM.get(name)


def observation(**fields):
    """Fill defaults and validate the shape of one observation."""
    base = {'schema': schema.OBSERVATION_SCHEMA, 'kind': 'finding', 'url': None, 'revision': None,
            'ruleId': None, 'title': '', 'message': '', 'severity': 'unknown', 'cwe': [], 'cve': [],
            'ghsa': [], 'path': None, 'lines': [], 'package': None, 'text': None}
    base.update(fields)
    base['id'] = f'{base["provider"]}:{base["providerId"]}'
    if base['provider'] not in schema.PROVIDERS or not schema.PROVIDER_ID.fullmatch(base['providerId']):
        raise ReportError('observation identity is invalid')
    if not schema.RULE.fullmatch(base['tool'] or ''):
        base['tool'] = 'unknown'
    if base['ruleId'] is not None and not schema.RULE.fullmatch(base['ruleId']):
        base['ruleId'] = None
    if base['url'] is not None and not schema.URL.fullmatch(base['url']):
        base['url'] = None
    if base['revision'] is not None and not schema.REVISION.fullmatch(base['revision']):
        base['revision'] = None
    base['title'] = schema.clean_text(base['title'], schema.MAX_TITLE)
    base['message'] = schema.clean_text(base['message'])
    if base['severity'] not in schema.SEVERITIES:
        base['severity'] = 'unknown'
    base['category'] = 'dependency' if base['package'] else 'code'
    return base


def alert_html_url(api_url):
    """The web URL of a GitHub code scanning alert from its API URL."""
    match = re.fullmatch(r'https://api\.github\.com/repos/([^/]+/[^/]+)/code-scanning/alerts/([0-9]+)',
                         api_url if isinstance(api_url, str) else '')
    return f'https://github.com/{match.group(1)}/security/code-scanning/{match.group(2)}' if match else None


def _rule_lookup(run):
    """Map (component index or None, rule index) and rule id to rule objects."""
    tool = run.get('tool') or {}
    driver = tool.get('driver') or {}
    components = [driver] + list(tool.get('extensions') or [])
    by_id, by_index = {}, {}
    for position, component in enumerate(components):
        for index, rule in enumerate(component.get('rules') or []):
            if isinstance(rule, dict):
                by_index[(position, index)] = rule
                if isinstance(rule.get('id'), str):
                    by_id.setdefault(rule['id'], rule)
    return by_id, by_index


def _sarif_rule(result, by_id, by_index):
    ref = result.get('rule') if isinstance(result.get('rule'), dict) else {}
    index = ref.get('index', result.get('ruleIndex'))
    component = ref.get('toolComponent', {}).get('index') if isinstance(ref.get('toolComponent'), dict) else None
    position = 0 if component is None else component + 1
    if type(index) is int and (position, index) in by_index:
        return by_index[(position, index)]
    rule_id = result.get('ruleId') or ref.get('id')
    return by_id.get(rule_id, {})


def parse_sarif(document, *, provider='sarif'):
    """Observations from one SARIF 2.1.0 log."""
    if not isinstance(document, dict) or document.get('version') != '2.1.0' or not isinstance(document.get('runs'), list):
        raise ReportError('Not a SARIF 2.1.0 log')
    out = []
    for run_index, run in enumerate(document['runs']):
        if not isinstance(run, dict):
            raise ReportError('SARIF run must be an object')
        tool_name = ((run.get('tool') or {}).get('driver') or {}).get('name') or 'unknown'
        revision = None
        for vcp in run.get('versionControlProvenance') or []:
            if isinstance(vcp, dict) and isinstance(vcp.get('revisionId'), str):
                revision = vcp['revisionId']
        by_id, by_index = _rule_lookup(run)
        for result_index, result in enumerate(run.get('results') or []):
            if not isinstance(result, dict):
                raise ReportError('SARIF result must be an object')
            if result.get('suppressions') or result.get('baselineState') == 'absent':
                continue
            rule = _sarif_rule(result, by_id, by_index)
            props = rule.get('properties') if isinstance(rule.get('properties'), dict) else {}
            result_props = result.get('properties') if isinstance(result.get('properties'), dict) else {}
            rule_id = result.get('ruleId') or rule.get('id')
            tags = list(props.get('tags') or []) + list(result_props.get('tags') or [])
            level = result.get('level') or (rule.get('defaultConfiguration') or {}).get('level') or 'warning'
            severity = (severity_from_score(result_props.get('security-severity'))
                        or severity_from_score(props.get('security-severity'))
                        or LEVEL_SEVERITY.get(level, 'unknown'))
            location = ((result.get('locations') or [{}])[0] or {}).get('physicalLocation') or {}
            path = normalize_path((location.get('artifactLocation') or {}).get('uri'))
            region = location.get('region') or {}
            message = (result.get('message') or {}).get('text') or ''
            short = (rule.get('shortDescription') or {}).get('text') or rule.get('name') or rule_id or ''
            texts = [rule_id or '', message, short] + [t for t in tags if isinstance(t, str)]
            alert_number = result_props.get('github/alertNumber')
            if type(alert_number) is int and alert_number > 0:
                obs_provider, provider_id = 'github-code-scanning', str(alert_number)
            else:
                fingerprints = result.get('partialFingerprints') or {}
                stable = fingerprints.get('primaryLocationLineHash') or result.get('guid') or result.get('correlationGuid')
                obs_provider = provider
                provider_id = (f'{tool_name}:{stable}' if isinstance(stable, str) and stable
                               else f'{tool_name}:run{run_index}-result{result_index}')
                provider_id = re.sub(r'[^A-Za-z0-9_.:/#-]', '-', provider_id)[:128]
            package = None
            match = TRIVY_PACKAGE.search(message)
            ecosystem = _package_ecosystem(path)
            if match and ecosystem and schema.PACKAGE.fullmatch(match.group(1)):
                version = match.group(2) if schema.VERSION.fullmatch(match.group(2)) else None
                fixed = (match.group(3) or '').split(',')[0].strip() or None
                package = {'ecosystem': ecosystem, 'name': match.group(1), 'versions': [version] if version else [],
                           'manifest': path, 'fixedVersion': fixed if fixed and schema.VERSION.fullmatch(fixed) else None,
                           'vulnerableRange': None, 'scope': 'unknown', 'relationship': 'unknown'}
            out.append(observation(
                provider=obs_provider, providerId=provider_id, tool=tool_name, ruleId=rule_id,
                url=alert_html_url(result_props.get('github/alertUrl')),
                revision=revision, title=short, message=message, severity=severity,
                cwe=cwe_from_tags(tags), cve=_ids(schema.CVE, texts), ghsa=_ghsa(texts),
                path=None if package else path,
                lines=[] if package else _line_range(region.get('startLine'), region.get('endLine')),
                package=package))
            if len(out) > MAX_OBSERVATIONS:
                raise ReportError('Too many SARIF results')
    return out


def parse_code_scanning_alerts(alerts):
    """Observations from GET /repos/{owner}/{repo}/code-scanning/alerts (open alerts only)."""
    if not isinstance(alerts, list):
        raise ReportError('Code scanning alerts must be a JSON array')
    out = []
    for alert in alerts:
        if not isinstance(alert, dict) or type(alert.get('number')) is not int:
            raise ReportError('Malformed code scanning alert')
        if alert.get('state') != 'open':
            continue
        rule = alert.get('rule') or {}
        instance = alert.get('most_recent_instance') or {}
        location = instance.get('location') or {}
        severity = (rule.get('security_severity_level') or
                    LEVEL_SEVERITY.get(rule.get('severity'), 'unknown'))
        tags = rule.get('tags') or []
        message = (instance.get('message') or {}).get('text') or ''
        texts = [rule.get('id') or '', message] + [t for t in tags if isinstance(t, str)]
        out.append(observation(
            provider='github-code-scanning', providerId=str(alert['number']),
            tool=(alert.get('tool') or {}).get('name') or 'unknown', ruleId=rule.get('id'),
            url=alert.get('html_url'), revision=instance.get('commit_sha'),
            title=rule.get('description') or rule.get('name') or '', message=message, severity=severity,
            cwe=cwe_from_tags(tags), cve=_ids(schema.CVE, texts), ghsa=_ghsa(texts),
            path=normalize_path(location.get('path')),
            lines=_line_range(location.get('start_line'), location.get('end_line'))))
        if len(out) > MAX_OBSERVATIONS:
            raise ReportError('Too many alerts')
    return out


def parse_dependabot_alerts(alerts):
    """Observations from GET /repos/{owner}/{repo}/dependabot/alerts (open alerts only)."""
    if not isinstance(alerts, list):
        raise ReportError('Dependabot alerts must be a JSON array')
    out = []
    for alert in alerts:
        if not isinstance(alert, dict) or type(alert.get('number')) is not int:
            raise ReportError('Malformed Dependabot alert')
        if alert.get('state') != 'open':
            continue
        dependency = alert.get('dependency') or {}
        package = dependency.get('package') or {}
        advisory = alert.get('security_advisory') or {}
        vulnerability = alert.get('security_vulnerability') or {}
        name = package.get('name')
        ecosystem = (package.get('ecosystem') or '').lower()
        if not isinstance(name, str) or not schema.PACKAGE.fullmatch(name) or not schema.ECOSYSTEM.fullmatch(ecosystem):
            raise ReportError('Dependabot alert has an invalid package')
        version = dependency.get('version_requirement')
        version = version.lstrip('= ') if isinstance(version, str) else None
        fixed = (vulnerability.get('first_patched_version') or {}).get('identifier')
        vrange = vulnerability.get('vulnerable_version_range')
        identifiers = [i.get('value') for i in advisory.get('identifiers') or [] if isinstance(i, dict)]
        texts = [advisory.get('cve_id') or '', advisory.get('ghsa_id') or ''] + [i for i in identifiers if isinstance(i, str)]
        cwes = sorted({f'CWE-{int(c["cwe_id"][4:])}' for c in advisory.get('cwes') or []
                       if isinstance(c, dict) and isinstance(c.get('cwe_id'), str)
                       and re.fullmatch(r'CWE-[0-9]{1,6}', c['cwe_id']) and int(c['cwe_id'][4:]) > 0})
        out.append(observation(
            provider='dependabot', providerId=str(alert['number']), tool='dependabot', url=alert.get('html_url'),
            ruleId=advisory.get('ghsa_id'), title=advisory.get('summary') or '', message=advisory.get('summary') or '',
            severity=ADVISORY_SEVERITY.get(vulnerability.get('severity') or advisory.get('severity'), 'unknown'),
            cwe=cwes, cve=_ids(schema.CVE, texts), ghsa=_ghsa(texts),
            package={'ecosystem': ecosystem, 'name': name,
                     'versions': [version] if version and schema.VERSION.fullmatch(version) else [],
                     'manifest': normalize_path(dependency.get('manifest_path')),
                     'fixedVersion': fixed if isinstance(fixed, str) and schema.VERSION.fullmatch(fixed) else None,
                     'vulnerableRange': vrange if isinstance(vrange, str) and schema.VERSION_RANGE.fullmatch(vrange) else None,
                     'scope': dependency.get('scope') if dependency.get('scope') in schema.SCOPES else 'unknown',
                     'relationship': (dependency.get('relationship')
                                      if dependency.get('relationship') in schema.RELATIONSHIPS else 'unknown')}))
        if len(out) > MAX_OBSERVATIONS:
            raise ReportError('Too many alerts')
    return out


def document_observation(name, text):
    """An unparsed report: bounded text for the generative triage stage only."""
    safe_name = re.sub(r'[^A-Za-z0-9_.-]', '-', name)[:96] or 'report'
    if not isinstance(text, str):
        raise ReportError('Report text must be a string')
    truncated = len(text) > MAX_DOCUMENT_CHARS
    return observation(provider='file', providerId=safe_name, tool='unknown', kind='document',
                       title=f'Unparsed report {safe_name}', message='truncated' if truncated else '',
                       text=text[:MAX_DOCUMENT_CHARS])


PARSERS = {
    'sarif': lambda data, provider: parse_sarif(data, provider=provider),
    'github-code-scanning-alerts': lambda data, provider: parse_code_scanning_alerts(data),
    'dependabot-alerts': lambda data, provider: parse_dependabot_alerts(data),
}


def detect_format(data):
    """Recognize a known format from content alone."""
    if isinstance(data, dict) and data.get('version') == '2.1.0' and isinstance(data.get('runs'), list):
        return 'sarif'
    if isinstance(data, list) and data and all(isinstance(a, dict) for a in data[:5]):
        first = data[0]
        if 'security_advisory' in first and 'dependency' in first:
            return 'dependabot-alerts'
        if 'rule' in first and 'most_recent_instance' in first:
            return 'github-code-scanning-alerts'
    return None


def parse_envelope(envelope):
    """Observations from one fetch artifact (see vuln_fetch.envelope)."""
    if not isinstance(envelope, dict) or envelope.get('schema') != 'ai-sdlc.vulnerability-report/v1':
        raise ReportError('Not a vulnerability report envelope')
    fmt, data, name = envelope.get('format'), envelope.get('data'), envelope.get('name') or 'report'
    if fmt == 'text':
        if isinstance(data, str):
            try:
                parsed = json.loads(data)
            except ValueError:
                parsed = None
            detected = detect_format(parsed)
            if detected:
                return PARSERS[detected](parsed, 'sarif')
        return [document_observation(name, data if isinstance(data, str) else json.dumps(data))]
    if fmt == 'json':
        detected = detect_format(data)
        if detected:
            return PARSERS[detected](data, 'sarif')
        return [document_observation(name, json.dumps(data, sort_keys=True)[:MAX_DOCUMENT_CHARS + 1])]
    if fmt not in PARSERS:
        raise ReportError(f'Unknown report format {fmt!r}')
    return PARSERS[fmt](data, 'sarif')


def merge_observations(groups):
    """Concatenate observation lists, keeping the first of identical ids.

    The code scanning alerts API and the same analysis's SARIF describe the
    same alert; both carry provider github-code-scanning and the alert
    number, so the alert is kept once."""
    seen, out = set(), []
    for group in groups:
        for item in group:
            if item['id'] not in seen:
                seen.add(item['id'])
                out.append(item)
    return out
