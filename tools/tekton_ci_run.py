#!/usr/bin/env python3
"""Create a PipelineRun of one reviewed Tekton catalog pipeline and read its bounded results.

Pipelines (installed by `./install.sh local --tekton-ci`, docs/operations/tekton-ci.md):

  maven-build-test    offline build and tests of one module at a commit; the
                      pull-request CI and the build/test tool
  maven-dependencies  fill the shared Maven cache; current base-branch head only
  npm-lock-update     update an npm lockfile with scripts disabled
  dependency-audit    known vulnerabilities of an npm lockfile or Maven module

Inputs are validated before any cluster access, and each PipelineRun
references an installed pipeline (Tekton refuses inline specifications).
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
SERVICES = ('none', 'postgres', 'mongodb')
SERVICE_ACCOUNT = 'ai-sdlc-ci-runner'
SANDBOX = 'ai-sdlc-offline'
REPOSITORY = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9_.-]{1,100}')
REVISION = re.compile(r'[0-9a-f]{40}')
PATH = re.compile(r'[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*')
MIRROR = re.compile(r'https?://[A-Za-z0-9.-]+(?::[0-9]{1,5})?(?:/[A-Za-z0-9._~/-]*)?')
MAVEN_ARG = re.compile(r'-[A-Za-z][A-Za-z0-9_.:=,!*/@+-]{0,199}')
# npm update takes package names only and stays within package.json ranges.
NPM_PACKAGE = re.compile(r'(?:@[a-z0-9][a-z0-9._-]{0,100}/)?[a-z0-9][a-z0-9._-]{0,100}')
# The pipeline task that runs untrusted candidate code offline can get the
# sandbox runtime class (--sandbox). The installed ai-sdlc-offline class
# configures gVisor with network=none, so fetch tasks (trusted base inputs, or
# npm with scripts disabled) always use the default runtime. Opt-in for now:
# with the pinned runsc build, Tekton task pods failed to start their
# sub-containers ("creating root file system: input/output error") although a
# plain four-container pod started; see docs/operations/tekton-ci.md.
SANDBOXED_TASKS = {'ai-sdlc-maven-build-test': 'build-test'}
POD_SECURITY = {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532, 'fsGroup': 65532,
                'seccompProfile': {'type': 'RuntimeDefault'}}


def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=kwargs.pop('check', True), text=True, **kwargs)


def validate_repository(value):
    if not REPOSITORY.fullmatch(value) or any(part in ('.', '..') for part in value.split('/')):
        raise ValueError('Repository must be owner/name')
    return value


def validate_revision(value):
    if not REVISION.fullmatch(value):
        raise ValueError('Revision must be a full lowercase 40-character commit SHA')
    return value


def validate_path(value, what='Module', allow_root=True):
    if (value == '.' and allow_root) or (PATH.fullmatch(value) and not any(p in ('.', '..') for p in value.split('/'))):
        return value
    raise ValueError(what + ' must be a relative directory without dot segments')


def validate_services(values):
    if not values or any(v not in SERVICES for v in values) or len(set(values)) != len(values) or ('none' in values and len(values) > 1):
        raise ValueError('Services must be none or a subset of: ' + ', '.join(SERVICES[1:]))
    return ','.join(values)


def validate_maven_args(values):
    # Operator-reviewed switches only: no goals, files or shell syntax. Each
    # value reaches Maven as one argv entry, never through a shell.
    if len(values) > 32 or any(not MAVEN_ARG.fullmatch(v) for v in values):
        raise ValueError('Maven arguments must be at most 32 simple -D/-P style switches')
    if any(v.startswith(('-s', '-gs', '-f', '-o', '-Dmaven.repo.local', '--settings', '--file', '--offline')) for v in values):
        raise ValueError('Maven arguments must not change settings, POM, offline mode or local repository')
    return list(values)


def validate_packages(mode, packages):
    if mode not in ('update', 'audit-fix'):
        raise ValueError('npm mode must be update or audit-fix')
    if len(packages) > 16 or any(not NPM_PACKAGE.fullmatch(p) for p in packages):
        raise ValueError('npm packages must be at most 16 package names (npm update takes no versions)')
    if (mode == 'update') != bool(packages):
        raise ValueError('update needs packages; audit-fix takes none')
    return list(packages)


def load_project(path):
    project = json.loads(Path(path).read_text())
    if set(project) - {'npmDirectories'} != {'version', 'repository', 'baseBranch', 'modules'} or project['version'] != 1:
        raise ValueError('Unsupported Tekton CI project catalog')
    validate_repository(project['repository'])
    if not re.fullmatch(r'[A-Za-z0-9._/-]{1,100}', project['baseBranch']):
        raise ValueError('Invalid base branch')
    if not isinstance(project['modules'], dict) or not project['modules']:
        raise ValueError('A project catalog lists at least one module')
    for name, module in project['modules'].items():
        validate_path(name)
        if (set(module) != {'status', 'services', 'mavenArgs', 'note'}
                or module['status'] not in ('supported', 'unverified', 'pending')
                or not isinstance(module['services'], list) or not isinstance(module['mavenArgs'], list)
                or not isinstance(module['note'], str)):
            raise ValueError('Invalid module entry: ' + name)
        validate_maven_args(module['mavenArgs'])
        if module['status'] != 'pending':
            validate_services(module['services'])
    directories = project.get('npmDirectories', [])
    if not isinstance(directories, list) or len(directories) > 16:
        raise ValueError('npmDirectories lists at most 16 directories')
    for directory in directories:
        validate_path(directory, 'npm directory', allow_root=False)
    return project


def label(value):
    value = re.sub(r'[^A-Za-z0-9_.-]', '.', value)[:63].strip('._-')
    return value or 'none'


def pipeline_run(pipeline, repository, revision, params, workspaces, target, runtime_class=None):
    """Render a PipelineRun of an installed pipeline with restricted, token-free pods."""
    if pipeline not in tekton_ci_install.PIPELINES:
        raise ValueError('Pipeline is not in the reviewed catalog')
    pod = {'automountServiceAccountToken': False, 'securityContext': POD_SECURITY}
    spec = {
        'pipelineRef': {'name': pipeline},
        'params': [{'name': 'repository', 'value': validate_repository(repository)},
                   {'name': 'revision', 'value': validate_revision(revision)}] + params,
        'timeouts': {'pipeline': '1h0m0s'},
        'taskRunTemplate': {'serviceAccountName': SERVICE_ACCOUNT, 'podTemplate': pod},
        'workspaces': [{'name': 'source', 'volumeClaimTemplate': {'spec': {
            'accessModes': ['ReadWriteOnce'], 'resources': {'requests': {'storage': '4Gi'}}}}}] + workspaces}
    if runtime_class and pipeline in SANDBOXED_TASKS:
        if not re.fullmatch(r'[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?', runtime_class):
            raise ValueError('Invalid runtime class name')
        spec['taskRunSpecs'] = [{'pipelineTaskName': SANDBOXED_TASKS[pipeline],
                                 'podTemplate': dict(pod, runtimeClassName=runtime_class)}]
    return {'apiVersion': 'tekton.dev/v1', 'kind': 'PipelineRun',
            'metadata': {'generateName': pipeline.removeprefix('ai-sdlc-') + '-', 'namespace': tekton_ci_install.CI_NAMESPACE,
                         'labels': {'app.kubernetes.io/part-of': 'ai-sdlc', 'ai-sdlc.io/pipeline': pipeline,
                                    'ai-sdlc.io/repository': label(repository), 'ai-sdlc.io/revision': revision,
                                    'ai-sdlc.io/target': label(target)},
                         'annotations': {'ai-sdlc.io/repository': repository, 'ai-sdlc.io/target': target}},
            'spec': spec}


def maven_build_test(repository, revision, module, services, maven_args, runtime_class=None):
    return pipeline_run('ai-sdlc-maven-build-test', repository, revision,
                        [{'name': 'module', 'value': validate_path(module)},
                         {'name': 'services', 'value': validate_services(services)},
                         {'name': 'maven-args', 'value': validate_maven_args(maven_args)}],
                        [{'name': 'maven-cache', 'persistentVolumeClaim': {'claimName': tekton_ci_install.cache_claim(repository.lower()), 'readOnly': True}}],
                        module, runtime_class)


def maven_dependencies(repository, revision, module, services, maven_args, mirror_url=''):
    if mirror_url and not MIRROR.fullmatch(mirror_url):
        raise ValueError('Invalid Maven mirror URL')
    return pipeline_run('ai-sdlc-maven-dependencies', repository, revision,
                        [{'name': 'module', 'value': validate_path(module)},
                         {'name': 'services', 'value': validate_services(services)},
                         {'name': 'maven-args', 'value': validate_maven_args(maven_args)},
                         {'name': 'maven-mirror-url', 'value': mirror_url}],
                        [{'name': 'maven-cache', 'persistentVolumeClaim': {'claimName': tekton_ci_install.cache_claim(repository.lower())}}],
                        module)


def npm_lock_update(repository, revision, directory, mode, packages, registry_url=''):
    if registry_url and not MIRROR.fullmatch(registry_url):
        raise ValueError('Invalid npm registry URL')
    return pipeline_run('ai-sdlc-npm-lock-update', repository, revision,
                        [{'name': 'directory', 'value': validate_path(directory, 'npm directory', allow_root=False)},
                         {'name': 'mode', 'value': mode},
                         {'name': 'packages', 'value': validate_packages(mode, packages)},
                         {'name': 'registry-url', 'value': registry_url}],
                        [], directory)


# Registries an audit may send dependency lists to: the public ones, or the
# in-cluster dependency proxy by cluster IP.
AUDIT_NPM_REGISTRY = re.compile(r'https://registry\.npmjs\.org/|http://[0-9]{1,3}(?:\.[0-9]{1,3}){3}:8081/')
AUDIT_MAVEN_REGISTRY = re.compile(r'https://repo\.maven\.apache\.org/maven2|http://[0-9]{1,3}(?:\.[0-9]{1,3}){3}:8080/maven2')


def dependency_audit(repository, revision, ecosystem, target, registry_url='', maven_registry_url=''):
    if ecosystem not in ('npm', 'maven'):
        raise ValueError('ecosystem must be npm or maven')
    if registry_url and (ecosystem != 'npm' or not AUDIT_NPM_REGISTRY.fullmatch(registry_url)):
        raise ValueError('registry-url must be the public npm registry or the dependency proxy')
    if maven_registry_url and (ecosystem != 'maven' or not AUDIT_MAVEN_REGISTRY.fullmatch(maven_registry_url)):
        raise ValueError('maven-registry-url must be Maven Central or the dependency proxy')
    return pipeline_run('ai-sdlc-dependency-audit', repository, revision,
                        [{'name': 'ecosystem', 'value': ecosystem},
                         {'name': 'target', 'value': validate_path(target, 'Audit target', allow_root=ecosystem == 'maven')},
                         {'name': 'registry-url', 'value': registry_url},
                         {'name': 'maven-registry-url', 'value': maven_registry_url}],
                        [], target)


def base_head(repository, branch):
    """Read the public base-branch head without credentials."""
    result = run(['git', 'ls-remote', '--', 'https://github.com/' + repository + '.git', 'refs/heads/' + branch],
                 capture_output=True, timeout=60, env=dict(os.environ, GIT_TERMINAL_PROMPT='0'))
    lines = result.stdout.split()
    if len(lines) != 2 or not REVISION.fullmatch(lines[0]):
        raise ValueError('Cannot read the base branch head')
    return lines[0]


def passed(observed, revision):
    """A run passes only when the pipeline succeeded, checked out the requested
    commit and its tool reported exit code 0. The tasks publish a summary even
    when the tool fails, so the pipeline's own verdict is not enough."""
    summary = observed.get('summary') or {}
    if 'complete' in summary:
        # An audit passes when it completed; findings are its output, not a failure.
        return observed.get('succeeded') is True and observed.get('commit') == revision and summary.get('complete') is True
    code = summary.get('mavenExitCode', summary.get('npmExitCode'))
    return observed.get('succeeded') is True and observed.get('commit') == revision and type(code) is int and code == 0


def summarize(pipeline_run_object):
    status = pipeline_run_object.get('status', {})
    condition = next((c for c in status.get('conditions', []) if c.get('type') == 'Succeeded'), {})
    results = {r.get('name'): r.get('value') for r in status.get('results', [])}
    summary = None
    if isinstance(results.get('summary'), str) and len(results['summary']) <= 4096:
        try:
            summary = json.loads(results['summary'])
        except ValueError:
            summary = None
    meta = pipeline_run_object.get('metadata', {})
    return {'pipelineRun': meta.get('namespace', '') + '/' + meta.get('name', ''), 'uid': meta.get('uid'),
            'succeeded': {'True': True, 'False': False}.get(condition.get('status')),
            'reason': condition.get('reason'), 'commit': results.get('commit'), 'summary': summary}


def cancel(kubectl, name):
    """Ask Tekton to stop a run; report whether the request was accepted."""
    try:
        result = run(kubectl + ['patch', 'pipelinerun.tekton.dev', name, '-n', tekton_ci_install.CI_NAMESPACE,
                                '--type', 'merge', '-p', '{"spec":{"status":"Cancelled"}}'],
                     capture_output=True, check=False, timeout=60)
    except subprocess.SubprocessError:
        return False
    return result.returncode == 0


def wait(kubectl, name, seconds, poll=10, sleep=time.sleep, clock=time.monotonic):
    deadline = clock() + seconds
    while True:
        try:
            result = run(kubectl + ['get', 'pipelinerun.tekton.dev', name, '-n', tekton_ci_install.CI_NAMESPACE, '-o', 'json'],
                         capture_output=True, check=False, timeout=60)
        except subprocess.TimeoutExpired:
            result = None  # A slow read is retried within the deadline.
        if result is not None and result.returncode == 0:
            observed = summarize(json.loads(result.stdout))
            if observed['succeeded'] is not None:
                return observed
        if clock() >= deadline:
            # Stop the run rather than leave candidate code running unobserved.
            if cancel(kubectl, name):
                raise ValueError('PipelineRun did not finish within the wait bound and was cancelled: ' + name)
            raise ValueError('PipelineRun did not finish within the wait bound; cancellation is NOT confirmed, '
                             'cancel it manually: ' + name)
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


def require_runtime_class(kubectl, name):
    if run(kubectl + ['get', 'runtimeclass', name, '-o', 'name'], capture_output=True, check=False).returncode != 0:
        raise ValueError('RuntimeClass ' + name + ' is absent; install it with ./install.sh sandbox')


def module_entry(project, module, allow_unverified):
    entry = project['modules'].get(module)
    if entry is None:
        raise ValueError('Module is not in the project catalog')
    if entry['status'] == 'pending':
        raise ValueError('Module is pending: ' + entry['note'])
    if entry['status'] == 'unverified' and not allow_unverified:
        raise ValueError('Module is unverified; pass --allow-unverified to run it: ' + entry['note'])
    return entry


def render_from_args(args, project):
    repository = args.repository or (project or {}).get('repository')
    if not repository:
        raise ValueError('Name --repository or a --project catalog')
    if project and repository != project['repository']:
        raise ValueError('Repository differs from the project catalog')
    runtime_class = args.runtime_class if args.sandbox else None
    if args.pipeline == 'dependency-audit':
        declared = (project.get('npmDirectories', []) if args.ecosystem == 'npm' else list(project['modules'])) if project else []
        if project and args.directory not in declared:
            raise ValueError('Audit target is not a catalog ' + ('npm directory' if args.ecosystem == 'npm' else 'Maven module'))
        return dependency_audit(repository, args.revision, args.ecosystem, args.directory, args.registry_url,
                                args.maven_mirror_url)
    if args.pipeline == 'npm-lock-update':
        if project and args.directory not in project.get('npmDirectories', []):
            raise ValueError('npm directory is not in the project catalog')
        return npm_lock_update(repository, args.revision, args.directory, args.mode, args.package or [],
                               args.registry_url)
    entry = module_entry(project, args.module, args.allow_unverified) if project else None
    services = args.services.split(',') if args.services else (entry['services'] if entry else ['none'])
    maven_args = args.maven_arg if args.maven_arg is not None else (entry['mavenArgs'] if entry else [])
    if args.pipeline == 'maven-build-test':
        return maven_build_test(repository, args.revision, args.module, services, maven_args, runtime_class)
    return maven_dependencies(repository, args.revision, args.module, services, maven_args, args.maven_mirror_url)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('pipeline', choices=['maven-build-test', 'maven-dependencies', 'npm-lock-update', 'dependency-audit'])
    parser.add_argument('--cluster', default='ai-sdlc')
    parser.add_argument('--project', type=Path, help='Catalog, for example ' + str((PROJECTS / 'quarkus-super-heroes.json').relative_to(ROOT)))
    parser.add_argument('--repository', help='owner/name; defaults to the catalog repository')
    parser.add_argument('--revision', help='Full 40-character commit SHA; maven-dependencies defaults to the base-branch head')
    parser.add_argument('--module', default='.')
    parser.add_argument('--services', help='Comma-separated services; defaults to the catalog entry or none')
    parser.add_argument('--maven-arg', action='append', default=None, help='Extra Maven switch; replaces the catalog arguments')
    parser.add_argument('--maven-mirror-url', default='', help='maven-dependencies: Maven proxy URL; dependency-audit (maven): Maven registry, Central or the proxy')
    parser.add_argument('--directory', help='npm-lock-update: directory with package-lock.json; dependency-audit: the target directory or module')
    parser.add_argument('--ecosystem', choices=['npm', 'maven'], help='dependency-audit: npm or maven')
    parser.add_argument('--mode', default='update', choices=['update', 'audit-fix'])
    parser.add_argument('--package', action='append', help='npm-lock-update update mode: a package name (npm update takes no versions)')
    parser.add_argument('--registry-url', default='', help='npm-lock-update: registry or proxy URL')
    parser.add_argument('--allow-unverified', action='store_true', help='Run a catalog module whose status is unverified')
    parser.add_argument('--sandbox', action='store_true', help='maven-build-test: run build-test under --runtime-class (gVisor)')
    parser.add_argument('--runtime-class', default=SANDBOX, help='RuntimeClass for --sandbox (default ai-sdlc-offline)')
    parser.add_argument('--wait', type=int, default=0, metavar='SECONDS', help='Wait up to SECONDS for completion (0: do not wait)')
    args = parser.parse_args(argv)
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}', args.cluster) or not 0 <= args.wait <= 7200:
        raise ValueError('Invalid cluster name or wait bound')
    project = load_project(args.project) if args.project else None
    if args.pipeline == 'maven-dependencies':
        # The only pipeline that writes the shared cache: trusted inputs only.
        if project is None:
            raise ValueError('maven-dependencies needs the project catalog to name its base branch')
        head = base_head(project['repository'], project['baseBranch'])
        args.revision = args.revision or head
        if args.revision != head:
            raise ValueError('maven-dependencies must use the current base-branch head')
    if not args.revision:
        raise ValueError('--revision is required')
    if args.pipeline in ('npm-lock-update', 'dependency-audit') and not args.directory:
        raise ValueError('--directory is required')
    if args.pipeline == 'dependency-audit' and not args.ecosystem:
        raise ValueError('--ecosystem is required')
    rendered = render_from_args(args, project)
    kubectl = connect(args.cluster)
    if 'taskRunSpecs' in rendered['spec']:
        require_runtime_class(kubectl, args.runtime_class)
    result = run(kubectl + ['create', '-o', 'json', '-f', '-'], input=json.dumps(rendered), capture_output=True, check=False)
    if result.returncode != 0:
        # The rendered run carries no credential; show the API's reason (for
        # example an admission denial), bounded to one line.
        lines = result.stderr.strip().splitlines()
        raise ValueError('PipelineRun creation refused: ' + (lines[-1][:300] if lines else 'no reason given'))
    created = json.loads(result.stdout)
    name = created['metadata']['name']
    print('Created ' + tekton_ci_install.CI_NAMESPACE + '/' + name, file=sys.stderr)
    if not args.wait:
        print(json.dumps(summarize(created), indent=2))
        return 0
    observed = wait(kubectl, name, args.wait)
    observed['passed'] = passed(observed, args.revision)
    print(json.dumps(observed, indent=2))
    return 0 if observed['passed'] else 1


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print('Tekton CI run failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
