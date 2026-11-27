#!/usr/bin/env python3
"""Create a reviewed Maven CI PipelineRun for one module at one immutable commit.

The run targets the owned kind cluster that `./install.sh local --tekton-ci`
prepared. Inputs are validated before any cluster access; the PipelineRun
references only the installed Pipeline (Tekton refuses inline specifications).
See docs/operations/tekton-ci.md.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

import tekton_ci_install

ROOT = Path(__file__).resolve().parents[1]
PROJECTS = tekton_ci_install.DESIRED / 'projects'
SERVICES = ('none', 'postgres')
CACHE_CLAIM = 'ai-sdlc-ci-maven-cache'
SERVICE_ACCOUNT = 'ai-sdlc-ci-runner'
REPOSITORY = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9_.-]{1,100}')
REVISION = re.compile(r'[0-9a-f]{40}')
MODULE = re.compile(r'[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*')
MIRROR = re.compile(r'https?://[A-Za-z0-9.-]+(?::[0-9]{1,5})?(?:/[A-Za-z0-9._~/-]*)?')
MAVEN_ARG = re.compile(r'-[A-Za-z][A-Za-z0-9_.:=,!*/@+-]{0,199}')


def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=kwargs.pop('check', True), text=True, **kwargs)


def validate_repository(value):
    if not REPOSITORY.fullmatch(value) or any(part in ('.', '..') for part in value.split('/')):
        raise ValueError('Repository must be owner/name')
    return value


def validate_module(value):
    if value != '.' and (not MODULE.fullmatch(value) or any(p in ('.', '..') for p in value.split('/'))):
        raise ValueError('Module must be a relative directory without dot segments')
    return value


def validate_services(values):
    if not values or any(v not in SERVICES for v in values) or len(set(values)) != len(values) or ('none' in values and len(values) > 1):
        raise ValueError('Services must be none or a subset of: ' + ', '.join(SERVICES[1:]))
    return ','.join(values)


def validate_maven_args(values):
    # Operator-reviewed switches only: no goals, files or shell syntax. Each
    # value reaches Maven as one argv entry, never through a shell.
    if len(values) > 32 or any(not MAVEN_ARG.fullmatch(v) for v in values):
        raise ValueError('Maven arguments must be at most 32 simple -D/-P style switches')
    if any(v.startswith(('-s', '-gs', '-f', '-Dmaven.repo.local', '--settings', '--file')) for v in values):
        raise ValueError('Maven arguments must not change settings, POM or local repository')
    return list(values)


def load_project(path):
    project = json.loads(Path(path).read_text())
    if set(project) != {'version', 'repository', 'baseBranch', 'modules'} or project['version'] != 1:
        raise ValueError('Unsupported Tekton CI project catalog')
    validate_repository(project['repository'])
    if not re.fullmatch(r'[A-Za-z0-9._/-]{1,100}', project['baseBranch']):
        raise ValueError('Invalid base branch')
    if not isinstance(project['modules'], dict) or not project['modules']:
        raise ValueError('A project catalog lists at least one module')
    for name, module in project['modules'].items():
        validate_module(name)
        if (set(module) != {'status', 'services', 'mavenArgs', 'note'}
                or module['status'] not in ('supported', 'unverified', 'pending')
                or not isinstance(module['services'], list) or not isinstance(module['mavenArgs'], list)
                or not isinstance(module['note'], str)):
            raise ValueError('Invalid module entry: ' + name)
        validate_maven_args(module['mavenArgs'])
        if module['status'] != 'pending':
            validate_services(module['services'])
    return project


def label(value):
    value = re.sub(r'[^A-Za-z0-9_.-]', '.', value)[:63].strip('._-')
    return value or 'none'


def render(repository, revision, module, services, maven_args, warm_cache=False, mirror_url=''):
    validate_repository(repository)
    if not REVISION.fullmatch(revision):
        raise ValueError('Revision must be a full lowercase 40-character commit SHA')
    validate_module(module)
    if mirror_url and not MIRROR.fullmatch(mirror_url):
        raise ValueError('Invalid Maven mirror URL')
    cache = ({'name': 'maven-cache-writable', 'persistentVolumeClaim': {'claimName': CACHE_CLAIM}} if warm_cache else
             {'name': 'maven-cache', 'persistentVolumeClaim': {'claimName': CACHE_CLAIM, 'readOnly': True}})
    return {
        'apiVersion': 'tekton.dev/v1', 'kind': 'PipelineRun',
        'metadata': {
            'generateName': 'maven-ci-', 'namespace': tekton_ci_install.CI_NAMESPACE,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc', 'ai-sdlc.io/repository': label(repository),
                       'ai-sdlc.io/revision': revision, 'ai-sdlc.io/module': label(module),
                       'ai-sdlc.io/cache': 'warm' if warm_cache else 'read-only'},
            'annotations': {'ai-sdlc.io/repository': repository, 'ai-sdlc.io/module': module}},
        'spec': {
            'pipelineRef': {'name': tekton_ci_install.PIPELINE},
            'params': [{'name': 'repository', 'value': repository},
                       {'name': 'revision', 'value': revision},
                       {'name': 'module', 'value': module},
                       {'name': 'services', 'value': validate_services(services)},
                       {'name': 'maven-args', 'value': validate_maven_args(maven_args)},
                       {'name': 'maven-mirror-url', 'value': mirror_url}],
            'timeouts': {'pipeline': '1h0m0s'},
            'taskRunTemplate': {
                'serviceAccountName': SERVICE_ACCOUNT,
                'podTemplate': {
                    'automountServiceAccountToken': False,
                    'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532,
                                        'fsGroup': 65532, 'seccompProfile': {'type': 'RuntimeDefault'}}}},
            'workspaces': [
                {'name': 'source', 'volumeClaimTemplate': {'spec': {
                    'accessModes': ['ReadWriteOnce'], 'resources': {'requests': {'storage': '4Gi'}}}}},
                cache]}}


def base_head(repository, branch):
    """Read the public base-branch head without credentials."""
    result = run(['git', 'ls-remote', '--', 'https://github.com/' + repository + '.git', 'refs/heads/' + branch],
                 capture_output=True, timeout=60, env=dict(os.environ, GIT_TERMINAL_PROMPT='0'))
    lines = result.stdout.split()
    if len(lines) != 2 or not REVISION.fullmatch(lines[0]):
        raise ValueError('Cannot read the base branch head')
    return lines[0]


def summarize(pipeline_run):
    status = pipeline_run.get('status', {})
    condition = next((c for c in status.get('conditions', []) if c.get('type') == 'Succeeded'), {})
    results = {r.get('name'): r.get('value') for r in status.get('results', [])}
    summary = None
    if isinstance(results.get('summary'), str):
        try:
            summary = json.loads(results['summary'])
        except ValueError:
            summary = None
    meta = pipeline_run.get('metadata', {})
    return {'pipelineRun': meta.get('namespace', '') + '/' + meta.get('name', ''), 'uid': meta.get('uid'),
            'succeeded': {'True': True, 'False': False}.get(condition.get('status')),
            'reason': condition.get('reason'), 'commit': results.get('commit'), 'summary': summary}


def wait(kubectl, name, seconds, poll=10, sleep=time.sleep, clock=time.monotonic):
    deadline = clock() + seconds
    while True:
        result = run(kubectl + ['get', 'pipelinerun.tekton.dev', name, '-n', tekton_ci_install.CI_NAMESPACE, '-o', 'json'],
                     capture_output=True, check=False, timeout=60)
        if result.returncode == 0:
            observed = summarize(json.loads(result.stdout))
            if observed['succeeded'] is not None:
                return observed
        if clock() >= deadline:
            raise ValueError('PipelineRun did not finish within the wait bound: ' + name)
        sleep(poll)


def connect(cluster):
    """Kubectl for the owned cluster; refuses a cluster whose identity differs from its record."""
    import install
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state')) / 'ai-sdlc' / cluster
    marker = json.loads((state / 'ownership.json').read_text())
    kubectl = [install.tools(install.tool_cache(), ['kubectl'])['kubectl'], '--kubeconfig', state / 'kubeconfig',
               '--context', 'kind-' + cluster]
    uid = run(kubectl + ['get', 'namespace', 'kube-system', '-o', 'jsonpath={.metadata.uid}'], capture_output=True).stdout.strip()
    install.check_ownership(state / 'ownership.json', cluster, marker.get('provider'), uid)
    return kubectl


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', default='ai-sdlc')
    parser.add_argument('--project', type=Path, help='Module catalog, for example ' + str((PROJECTS / 'quarkus-super-heroes.json').relative_to(ROOT)))
    parser.add_argument('--repository', help='owner/name; defaults to the catalog repository')
    parser.add_argument('--revision', required=True, help='Full 40-character commit SHA, for example a pull-request head')
    parser.add_argument('--module', default='.')
    parser.add_argument('--services', help='Comma-separated services; defaults to the catalog entry or none')
    parser.add_argument('--maven-arg', action='append', default=None, help='Extra Maven switch; replaces the catalog arguments')
    parser.add_argument('--maven-mirror-url', default='')
    parser.add_argument('--warm-cache', action='store_true', help='Write the shared Maven cache; only for the current base-branch head')
    parser.add_argument('--allow-unverified', action='store_true', help='Run a catalog module whose status is unverified')
    parser.add_argument('--wait', type=int, default=0, metavar='SECONDS', help='Wait up to SECONDS for completion (0: do not wait)')
    args = parser.parse_args(argv)
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}', args.cluster) or not 0 <= args.wait <= 7200:
        raise ValueError('Invalid cluster name or wait bound')
    project = load_project(args.project) if args.project else None
    repository = args.repository or (project or {}).get('repository')
    if not repository:
        raise ValueError('Name --repository or a --project catalog')
    if project and repository != project['repository']:
        raise ValueError('Repository differs from the project catalog')
    entry = (project or {}).get('modules', {}).get(args.module) if project else None
    if project and entry is None:
        raise ValueError('Module is not in the project catalog')
    if entry and entry['status'] == 'pending':
        raise ValueError('Module is pending: ' + entry['note'])
    if entry and entry['status'] == 'unverified' and not args.allow_unverified:
        raise ValueError('Module is unverified; pass --allow-unverified to run it: ' + entry['note'])
    services = args.services.split(',') if args.services else (entry['services'] if entry else ['none'])
    maven_args = args.maven_arg if args.maven_arg is not None else (entry['mavenArgs'] if entry else [])
    pipeline_run = render(repository, args.revision, args.module, services, maven_args,
                          warm_cache=args.warm_cache, mirror_url=args.maven_mirror_url)
    if args.warm_cache:
        if project is None:
            raise ValueError('A warm-cache run needs the project catalog to name its base branch')
        if base_head(repository, project['baseBranch']) != args.revision:
            raise ValueError('A warm-cache run must use the current base-branch head')
    kubectl = connect(args.cluster)
    created = json.loads(run(kubectl + ['create', '-o', 'json', '-f', '-'], input=json.dumps(pipeline_run),
                             capture_output=True).stdout)
    name = created['metadata']['name']
    print('Created ' + tekton_ci_install.CI_NAMESPACE + '/' + name, file=sys.stderr)
    if not args.wait:
        print(json.dumps(summarize(created), indent=2))
        return 0
    observed = wait(kubectl, name, args.wait)
    print(json.dumps(observed, indent=2))
    return 0 if observed['succeeded'] else 1


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print('Tekton CI run failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
