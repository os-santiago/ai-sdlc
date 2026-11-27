"""Deterministic report fetchers: one per provider, each producing one bounded envelope.

An envelope (ai-sdlc.vulnerability-report/v1) records where a report came
from and holds it unchanged:

    {"schema": "ai-sdlc.vulnerability-report/v1", "provider": "dependabot",
     "repository": "owner/name", "name": "dependabot-alerts",
     "format": "dependabot-alerts", "fetchedAt": "...", "data": [...]}

GitHub reads use a read-only credential supplied as a file (a mounted Secret
in Tekton) or, for a local dry run only, the GitHub CLI's active login. The
credential is sent only to api.github.com, never logged, and never written to
an artifact. Redirects are refused, so it cannot follow a response elsewhere.
"""
import datetime
import json
from pathlib import Path
import re
import urllib.error
import urllib.parse
import urllib.request

import vuln_schema as schema

ENVELOPE = 'ai-sdlc.vulnerability-report/v1'
INDEX = 'ai-sdlc.repository-index/v1'
API = 'https://api.github.com'
MAX_PAGE_BYTES = 8 * 1024 * 1024
MAX_PAGES = 30
MAX_REPORT_BYTES = 16 * 1024 * 1024
MAX_INDEX_PATHS = 50_000
TOKEN = re.compile(r'[A-Za-z0-9_]{20,512}')
MODULE_MARKERS = ('pom.xml', 'package.json', 'build.gradle', 'build.gradle.kts', 'go.mod', 'pyproject.toml',
                  'setup.py', 'Cargo.toml', 'Gemfile')


class FetchError(RuntimeError):
    """A provider read failed or exceeded a bound. Messages never carry response bodies."""


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def read_token(path):
    """A credential from a file. Its value is validated and never echoed."""
    try:
        token = Path(path).read_text().strip()
    except OSError:
        raise FetchError('The credential file is not readable') from None
    if not TOKEN.fullmatch(token):
        raise FetchError('The credential file does not hold a GitHub token')
    return token


def gh_token():
    """The GitHub CLI's active credential, for local dry runs only."""
    import github_client
    try:
        return github_client.from_gh()
    except ValueError as error:
        raise FetchError(str(error)) from None


class GitHub:
    """A minimal bounded GitHub REST reader and writer."""

    def __init__(self, token, opener=None, user_agent='ai-sdlc-vulnerability-intake'):
        if token is not None and not TOKEN.fullmatch(token):
            raise FetchError('Invalid GitHub credential format')
        self._token = token
        self._opener = opener or urllib.request.build_opener(_NoRedirect())
        self._agent = user_agent

    def _request(self, method, path, body=None, accept='application/vnd.github+json'):
        if not path.startswith('/') or '//' in path or '..' in path:
            raise FetchError('Invalid API path')
        headers = {'Accept': accept, 'X-GitHub-Api-Version': '2022-11-28', 'User-Agent': self._agent}
        if self._token:
            headers['Authorization'] = 'Bearer ' + self._token
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers['Content-Type'] = 'application/json'
        req = urllib.request.Request(API + path, data=data, headers=headers, method=method)
        try:
            with self._opener.open(req, timeout=30) as response:
                raw = response.read(MAX_PAGE_BYTES + 1)
                link = response.headers.get('Link', '') if response.headers else ''
                status = response.status
        except urllib.error.HTTPError as error:
            error.close()
            raise FetchError(f'GitHub {method} {path.split("?")[0]} failed with HTTP {error.code}') from None
        except (OSError, ValueError):
            raise FetchError(f'GitHub {method} {path.split("?")[0]} failed') from None
        if len(raw) > MAX_PAGE_BYTES:
            raise FetchError('GitHub response exceeded the size bound')
        return status, raw, link

    def get_json(self, path, accept='application/vnd.github+json'):
        _, raw, _ = self._request('GET', path, accept=accept)
        try:
            return json.loads(raw)
        except ValueError:
            raise FetchError('GitHub returned invalid JSON') from None

    def get_pages(self, path, max_pages=MAX_PAGES):
        """Follow rel="next" links on api.github.com only; bounded pages and bytes."""
        items, total, page = [], 0, 0
        while path:
            page += 1
            if page > max_pages:
                raise FetchError('GitHub listing exceeded the page bound')
            _, raw, link = self._request('GET', path)
            total += len(raw)
            if total > MAX_REPORT_BYTES:
                raise FetchError('GitHub listing exceeded the size bound')
            try:
                value = json.loads(raw)
            except ValueError:
                raise FetchError('GitHub returned invalid JSON') from None
            if not isinstance(value, list):
                raise FetchError('GitHub listing is not an array')
            items.extend(value)
            path = next_page(link)
        return items

    def post_json(self, path, body):
        status, raw, _ = self._request('POST', path, body=body)
        try:
            return status, json.loads(raw)
        except ValueError:
            raise FetchError('GitHub returned invalid JSON') from None


def next_page(link):
    for part in (link or '').split(','):
        match = re.fullmatch(r'\s*<(https://api\.github\.com(/[^>]*))>;\s*rel="next"\s*', part)
        if match:
            return match.group(2)
    return None


def validate_repository(repository):
    if (not isinstance(repository, str) or not schema.REPOSITORY.fullmatch(repository)
            or any(p in ('.', '..') for p in repository.split('/'))):
        raise FetchError('Repository must be owner/name')
    return repository


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')


def envelope(provider, repository, name, fmt, data):
    return {'schema': ENVELOPE, 'provider': provider, 'repository': repository, 'name': name,
            'format': fmt, 'fetchedAt': now(), 'data': data}


def fetch_code_scanning_alerts(client, repository):
    validate_repository(repository)
    alerts = client.get_pages(f'/repos/{repository}/code-scanning/alerts?state=open&per_page=100')
    return envelope('github-code-scanning', repository, 'code-scanning-alerts', 'github-code-scanning-alerts', alerts)


def default_branch(client, repository):
    meta = client.get_json(f'/repos/{repository}')
    branch = meta.get('default_branch') if isinstance(meta, dict) else None
    if not isinstance(branch, str) or not re.fullmatch(r'[A-Za-z0-9._/-]{1,200}', branch):
        raise FetchError('Repository default branch is unavailable')
    return branch


def fetch_code_scanning_sarif(client, repository, ref=None):
    """The SARIF of the latest default-branch analysis per category, reconciled with open alerts.

    Only analyses of one ref are read (the default branch unless ref is
    given), never pull-request merge refs, and only the newest page of them.
    Results are kept only when they carry the number of a currently open
    alert, so dismissed and fixed alerts never re-enter the intake."""
    validate_repository(repository)
    ref = ref or 'refs/heads/' + default_branch(client, repository)
    if not re.fullmatch(r'refs/heads/[A-Za-z0-9._/-]{1,200}', ref):
        raise FetchError('SARIF fetch needs a branch ref')
    analyses = client.get_json(f'/repos/{repository}/code-scanning/analyses?per_page=100'
                               f'&ref={urllib.parse.quote(ref, safe="")}')
    if not isinstance(analyses, list):
        raise FetchError('Code scanning analyses listing is not an array')
    latest = {}
    for analysis in analyses:
        if not isinstance(analysis, dict) or type(analysis.get('id')) is not int:
            raise FetchError('Malformed code scanning analysis')
        if analysis.get('ref') not in (None, ref):
            continue
        category = analysis.get('category') or ''
        if category not in latest and not analysis.get('error'):
            latest[category] = analysis
    open_alerts = client.get_pages(f'/repos/{repository}/code-scanning/alerts?state=open&per_page=100'
                                   f'&ref={urllib.parse.quote(ref, safe="")}')
    open_numbers = {a['number'] for a in open_alerts if isinstance(a, dict) and type(a.get('number')) is int}
    runs = []
    for category in sorted(latest):
        sarif = client.get_json(f'/repos/{repository}/code-scanning/analyses/{latest[category]["id"]}',
                                accept='application/sarif+json')
        if not isinstance(sarif, dict) or sarif.get('version') != '2.1.0':
            raise FetchError('Code scanning analysis is not SARIF 2.1.0')
        for run in sarif.get('runs') or []:
            if isinstance(run, dict):
                run = dict(run, results=[r for r in run.get('results') or [] if isinstance(r, dict)
                                         and (r.get('properties') or {}).get('github/alertNumber') in open_numbers])
                runs.append(run)
    return envelope('github-code-scanning', repository, 'code-scanning-sarif', 'sarif',
                    {'version': '2.1.0', 'runs': runs})


def fetch_dependabot_alerts(client, repository):
    validate_repository(repository)
    alerts = client.get_pages(f'/repos/{repository}/dependabot/alerts?state=open&per_page=100')
    return envelope('dependabot', repository, 'dependabot-alerts', 'dependabot-alerts', alerts)


def fetch_file(path, repository, name=None):
    """A report produced by a scanner task or uploaded by an operator.

    JSON content is kept as JSON (SARIF and other known formats are parsed
    deterministically later); anything else is kept as bounded text."""
    validate_repository(repository)
    source = Path(path)
    try:
        size = source.stat().st_size
        if size > MAX_REPORT_BYTES:
            raise FetchError('Report file exceeds the size bound')
        raw = source.read_bytes()[:MAX_REPORT_BYTES + 1]
    except OSError:
        raise FetchError('Report file is not readable') from None
    text = raw.decode('utf-8', errors='replace')
    label = re.sub(r'[^A-Za-z0-9_.-]', '-', name or source.name)[:96] or 'report'
    try:
        return envelope('file', repository, label, 'json', json.loads(text))
    except ValueError:
        return envelope('file', repository, label, 'text', text)


def repository_index(client, repository, revision=None):
    """Paths and module roots of one commit, for cheap correlation without a checkout."""
    validate_repository(repository)
    if revision is None:
        branch = default_branch(client, repository)
        commit = client.get_json(f'/repos/{repository}/commits/{urllib.parse.quote(branch, safe="")}')
        revision = commit.get('sha') if isinstance(commit, dict) else None
    if not isinstance(revision, str) or not schema.REVISION.fullmatch(revision):
        raise FetchError('Revision must be a full commit SHA')
    tree = client.get_json(f'/repos/{repository}/git/trees/{revision}?recursive=1')
    if not isinstance(tree, dict) or not isinstance(tree.get('tree'), list):
        raise FetchError('Repository tree is malformed')
    return build_index(repository, revision, tree)


def build_index(repository, revision, tree):
    paths = sorted(e['path'] for e in tree['tree']
                   if isinstance(e, dict) and e.get('type') == 'blob' and isinstance(e.get('path'), str)
                   and schema.PATH.fullmatch(e['path']))
    if len(paths) > MAX_INDEX_PATHS:
        raise FetchError('Repository tree exceeds the path bound')
    modules = sorted({p.rsplit('/', 1)[0] for p in paths
                      if '/' in p and p.rsplit('/', 1)[1] in MODULE_MARKERS and 'node_modules/' not in p})
    return {'schema': INDEX, 'repository': repository, 'revision': revision, 'truncated': bool(tree.get('truncated')),
            'modules': modules, 'paths': paths}


def validate_index(index):
    if (not isinstance(index, dict) or index.get('schema') != INDEX or not isinstance(index.get('paths'), list)
            or not isinstance(index.get('modules'), list) or len(index['paths']) > MAX_INDEX_PATHS
            or not schema.REVISION.fullmatch(index.get('revision') or '')):
        raise FetchError('Invalid repository index')
    return index
