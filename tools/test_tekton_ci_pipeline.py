"""Render and validate the reviewed Tekton task catalog and its run tool."""
import ipaddress
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

import yaml

import tekton_ci_install as tekton
import tekton_ci_run as runner

PROJECT = tekton.DESIRED / 'projects/quarkus-super-heroes.json'
SHA = 'ae00455c01a7a50d89ae8f1db99483abd27abe25'
FETCH_TASKS = {'source-checkout', 'maven-dependencies', 'npm-lock-update'}


def documents():
    docs = []
    for name in tekton.CI_FILES:
        docs.extend(d for d in yaml.safe_load_all((tekton.CI_DIR / name).read_text()) if d)
    return docs


def by_kind(kind):
    return {d['metadata']['name']: d for d in documents() if d['kind'] == kind}


def containers(task):
    template = task['spec'].get('stepTemplate', {})
    for container in task['spec']['steps']:
        merged = dict(template, **container)
        merged['securityContext'] = dict(template.get('securityContext', {}), **container.get('securityContext', {}))
        yield 'step', merged
    for sidecar in task['spec'].get('sidecars', []):
        yield 'sidecar', sidecar


def step(task, name):
    return next(s for s in by_kind('Task')[task]['spec']['steps'] if s['name'] == name)


def execute(text, env, path_prefix=''):
    """Run a reviewed script with the interpreter its shebang names."""
    interpreter = 'bash' if text.startswith('#!/bin/bash') else 'sh'
    return subprocess.run([interpreter, '-c', text], env=dict(env, PATH=path_prefix + '/usr/local/bin:/usr/bin:/bin'),
                          capture_output=True, text=True, timeout=60)


class ManifestTests(unittest.TestCase):
    def test_inventory_and_namespace_scope(self):
        kinds = sorted((d['kind'], d['metadata']['name']) for d in documents())
        self.assertEqual(kinds, sorted([
            ('Namespace', 'ai-sdlc-ci'), ('ServiceAccount', 'ai-sdlc-ci-runner'),
            ('ServiceAccount', 'default'), ('NetworkPolicy', 'ai-sdlc-ci-default-deny'),
            ('ValidatingAdmissionPolicy', 'ai-sdlc-ci-pipelineruns'), ('ValidatingAdmissionPolicyBinding', 'ai-sdlc-ci-pipelineruns'),
            ('NetworkPolicy', 'ai-sdlc-ci-fetch-dns'), ('NetworkPolicy', 'ai-sdlc-ci-fetch-https'),
            ('NetworkPolicy', 'ai-sdlc-ci-dependency-proxy'),
            ('Task', 'source-checkout'), ('Task', 'maven-build-test'), ('Task', 'maven-dependencies'),
            ('Task', 'npm-lock-update')] + [('Pipeline', p) for p in tekton.PIPELINES]))
        for doc in documents():
            if doc['kind'] not in ('Namespace', 'ValidatingAdmissionPolicy', 'ValidatingAdmissionPolicyBinding'):
                self.assertEqual(doc['metadata']['namespace'], 'ai-sdlc-ci', doc['metadata']['name'])
        for account in by_kind('ServiceAccount').values():
            self.assertIs(account['automountServiceAccountToken'], False, account['metadata']['name'])
        binding = by_kind('ValidatingAdmissionPolicyBinding')['ai-sdlc-ci-pipelineruns']['spec']
        self.assertEqual((binding['validationActions'], binding['matchResources']['namespaceSelector']),
                         (['Deny'], {'matchLabels': {'kubernetes.io/metadata.name': 'ai-sdlc-ci'}}))
        policy = by_kind('ValidatingAdmissionPolicy')['ai-sdlc-ci-pipelineruns']['spec']
        self.assertEqual(policy['failurePolicy'], 'Fail')
        rules = ' '.join(v['expression'] for v in policy['validations'])
        for pipeline in tekton.PIPELINES:
            self.assertIn(pipeline, rules)
        self.assertIn("'ai-sdlc-ci-runner'", rules)
        self.assertIn("'ai-sdlc.io/ci-egress' in object.metadata.labels", rules, 'labels propagate to pods')
        self.assertIn('s.metadata.labels', rules)
        self.assertIn('request.userInfo.groups', rules, 'cache writers are administrators')
        self.assertIn('dataSourceRef', rules)
        self.assertIn('readOnly == true', rules)
        namespace = by_kind('Namespace')['ai-sdlc-ci']['metadata']['labels']
        self.assertEqual(namespace['pod-security.kubernetes.io/enforce'], 'restricted')
        self.assertIs(by_kind('ServiceAccount')['ai-sdlc-ci-runner']['automountServiceAccountToken'], False)

    def test_every_container_is_pinned_non_root_and_unprivileged(self):
        for task in by_kind('Task').values():
            for _, container in containers(task):
                where = task['metadata']['name'] + '/' + container['name']
                self.assertRegex(container['image'], tekton.PINNED_IMAGE.pattern, where)
                context = container['securityContext']
                self.assertIs(context.get('runAsNonRoot'), True, where)
                self.assertGreater(context.get('runAsUser', 0), 0, where)
                self.assertIs(context.get('allowPrivilegeEscalation'), False, where)
                self.assertIs(context.get('readOnlyRootFilesystem'), True, where)
                self.assertNotIn('privileged', context, where)
                self.assertEqual(context.get('capabilities'), {'drop': ['ALL']}, where)
                self.assertEqual(context.get('seccompProfile'), {'type': 'RuntimeDefault'}, where)
                self.assertIn('limits', container.get('computeResources', {}), where)
                self.assertNotIn('envFrom', container, where)
                for env in container.get('env', []):
                    self.assertNotIn('valueFrom', env, where + ': no Secret or API-derived values')

    def test_tasks_mount_only_bounded_empty_dirs(self):
        for task in by_kind('Task').values():
            for volume in task['spec'].get('volumes', []):
                self.assertEqual(set(volume) - {'name'}, {'emptyDir'}, task['metadata']['name'])
                self.assertIn('sizeLimit', volume['emptyDir'])

    def test_parameters_never_reach_script_text(self):
        # Parameters enter through env or argv; script substitution would let
        # a value inject shell syntax.
        for task in by_kind('Task').values():
            for _, container in containers(task):
                self.assertNotRegex(container.get('script', ''), r'\$\((params|workspaces|results)\.', task['metadata']['name'])

    def test_pipelines_reference_reviewed_tasks_with_declared_inputs(self):
        tasks = by_kind('Task')
        self.assertEqual(set(by_kind('Pipeline')), set(tekton.PIPELINES))
        for pipeline in by_kind('Pipeline').values():
            spec = pipeline['spec']
            params = {p['name'] for p in spec['params']}
            workspaces = {w['name'] for w in spec['workspaces']}
            self.assertEqual(spec['tasks'][0]['taskRef'], {'name': 'source-checkout'})
            for entry in spec['tasks']:
                self.assertEqual(set(entry['taskRef']), {'name'}, 'installed tasks only, no resolvers or bundles')
                task = tasks[entry['taskRef']['name']]['spec']
                declared = {p['name']: p for p in task['params']}
                self.assertLessEqual({p['name'] for p in entry['params']}, set(declared))
                required = {name for name, p in declared.items() if 'default' not in p}
                self.assertLessEqual(required, {p['name'] for p in entry['params']})
                for param in entry['params']:
                    for reference in re.findall(r'\$\(params\.([a-z-]+)', json.dumps(param['value'])):
                        self.assertIn(reference, params)
                self.assertEqual({w['name'] for w in entry['workspaces']},
                                 {w['name'] for w in task['workspaces'] if not w.get('optional')})
                self.assertLessEqual({w['workspace'] for w in entry['workspaces']}, workspaces)
            self.assertEqual({r['name'] for r in spec['results']}, {'commit', 'summary'})
        build = {w['name']: w for w in tasks['maven-build-test']['spec']['workspaces']}
        self.assertIs(build['maven-cache']['readOnly'], True, 'candidate code never writes the shared cache')
        self.assertNotIn('readOnly', {w['name']: w for w in tasks['maven-dependencies']['spec']['workspaces']}['maven-cache'])

    def test_build_test_and_dependencies_share_services_gate_and_summary(self):
        build, deps = by_kind('Task')['maven-build-test']['spec'], by_kind('Task')['maven-dependencies']['spec']
        for key in ('sidecars', 'volumes', 'stepTemplate'):
            self.assertEqual(build[key], deps[key], key)
        for name in ('egress-gate', 'summarize'):
            self.assertEqual(step('maven-build-test', name), step('maven-dependencies', name), name)
        self.assertEqual([s['name'] for s in build['steps']], ['egress-gate', 'maven', 'summarize'])
        self.assertNotIn('onError', build['steps'][0], 'a failed gate stops the task before Maven')
        self.assertEqual(build['steps'][1]['onError'], 'continue', 'the summary step must still publish a result')
        self.assertIn(' -o ', step('maven-build-test', 'maven')['script'], 'build-test is offline')
        self.assertIn('maven.repo.local.tail', step('maven-build-test', 'maven')['script'])
        self.assertIn('-Dmaven.test.failure.ignore=true', step('maven-dependencies', 'maven')['script'])

    def test_network_policy_denies_by_default_and_fetch_tasks_get_dns_and_public_https_only(self):
        policies = by_kind('NetworkPolicy')
        self.assertEqual(policies['ai-sdlc-ci-default-deny']['spec'], {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']})
        for name, task in by_kind('Task').items():
            labels = task['metadata']['labels']
            if name in FETCH_TASKS:
                self.assertEqual(labels.get('ai-sdlc.io/ci-egress'), 'fetch', name)
            else:
                self.assertNotIn('ai-sdlc.io/ci-egress', labels, name + ' runs candidate code with no network')
        ports = set()
        for name in ('ai-sdlc-ci-fetch-dns', 'ai-sdlc-ci-fetch-https'):
            spec = policies[name]['spec']
            self.assertEqual(spec['policyTypes'], ['Egress'])
            self.assertNotIn('ingress', spec)
            self.assertEqual(spec['podSelector'], {'matchLabels': {'ai-sdlc.io/ci-egress': 'fetch'}})
            for rule in spec['egress']:
                ports |= {(p['protocol'], p['port']) for p in rule['ports']}
        self.assertEqual(ports, {('UDP', 53), ('TCP', 53), ('TCP', 443)})
        https = policies['ai-sdlc-ci-fetch-https']['spec']['egress'][0]['to'][0]['ipBlock']
        excluded = [ipaddress.ip_network(c) for c in https['except']]
        for address in ('10.96.0.1', '10.244.0.5', '172.18.0.2', '192.168.1.1', '169.254.169.254', '127.0.0.1', '100.64.0.1'):
            self.assertTrue(any(ipaddress.ip_address(address) in n for n in excluded), address)
        self.assertEqual(https['cidr'], '0.0.0.0/0')
        # The proxy contract: only the proxy pods (Maven, npm) and DNS, and no
        # task carries the label until the proxy is installed.
        proxy = policies['ai-sdlc-ci-dependency-proxy']['spec']
        self.assertEqual(proxy['podSelector'], {'matchLabels': {'ai-sdlc.io/ci-egress': 'dependency-proxy'}})
        self.assertEqual(proxy['egress'][0]['to'], [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'ai-sdlc-dependencies'}},
                                                     'podSelector': {'matchLabels': {'app.kubernetes.io/component': 'dependency-proxy'}}}])
        self.assertEqual({p['port'] for p in proxy['egress'][0]['ports']}, {8080, 8081})
        self.assertEqual(len(proxy['egress']), 1, 'no DNS: callers pass the proxy cluster IP')

    def test_sidecars_start_only_when_requested_and_bind_loopback(self):
        for sidecar in by_kind('Task')['maven-build-test']['spec']['sidecars']:
            self.assertIn('127.0.0.1', sidecar['script'], sidecar['name'])
            self.assertIn('exec sleep', sidecar['script'], sidecar['name'])
            self.assertIn('readinessProbe', sidecar)
            self.assertFalse(any('PASSWORD' in e['name'] for e in sidecar.get('env', [])))


class ScriptTests(unittest.TestCase):
    """Run the reviewed scripts locally against input checks, stubs and synthetic reports."""

    def test_checkout_refuses_mutable_or_malformed_inputs_before_network(self):
        text = step('source-checkout', 'clone')['script']
        with tempfile.TemporaryDirectory() as directory:
            base = {'DESTINATION': directory + '/src', 'RESULT': directory + '/result', 'HOME': directory}
            for repository, revision in (('os-santiago/x', 'main'), ('os-santiago/x', SHA[:12]),
                                         ('os-santiago/x', SHA.upper()), ('../x', SHA), ('a/b;id', SHA),
                                         ('a/..', SHA), ('https://evil/x', SHA), ('os-santiago/x', SHA + '\nextra'),
                                         ('os-santiago/x\n../../y', SHA)):
                result = execute(text, dict(base, REPOSITORY=repository, REVISION=revision))
                self.assertEqual(result.returncode, 2, (repository, revision, result.stderr))
                self.assertFalse(Path(directory, 'src').exists())

    def test_maven_steps_refuse_unsafe_module_services_and_mirror(self):
        for task in ('maven-build-test', 'maven-dependencies'):
            text = step(task, 'maven')['script']
            with tempfile.TemporaryDirectory() as directory:
                Path(directory, 'source/app').mkdir(parents=True)
                Path(directory, 'source/app/pom.xml').write_text('<project/>')
                base = {'SOURCE': directory + '/source', 'HOME': directory + '/home', 'CACHE': directory + '/cache',
                        'MAVEN_MIRROR_URL': ''}
                cases = [('../app', 'none', ''), ('app/../..', 'none', ''), ('/etc', 'none', ''), ('app', 'redis', ''),
                         ('app\n../x', 'none', ''), ('app', 'none\npostgres', ''),
                         ('app', 'postgres;id', ''), ('missing', 'none', '')]
                if task == 'maven-dependencies':
                    cases += [('app', 'none', 'http://x/<y>'), ('app', 'none', 'file:///etc')]
                for module, services, mirror in cases:
                    result = execute(text, dict(base, MODULE=module, SERVICES=services, MAVEN_MIRROR_URL=mirror))
                    self.assertEqual(result.returncode, 2, (task, module, services, mirror, result.stderr))

    def gate(self, outcomes):
        """Replay connect outcomes: 'timeout' (exit 124), 'refused', 'open' or 'other'."""
        text = step('maven-build-test', 'egress-gate')['script']
        with tempfile.TemporaryDirectory() as directory:
            plan = Path(directory, 'plan')
            plan.write_text('\n'.join(outcomes) + '\n')
            # Stubs: timeout replays the next outcome (repeating the last), sleep returns at once.
            Path(directory, 'timeout').write_text(
                '#!/bin/sh\no=$(head -n1 "%s")\n[ "$(wc -l < "%s")" -gt 1 ] && sed -i 1d "%s"\n'
                'case "$o" in timeout) exit 124;; refused) echo "bash: connect: Connection refused" >&2; exit 1;;'
                ' open) exit 0;; *) echo "bash: unknown host" >&2; exit 1;; esac\n' % (plan, plan, plan))
            Path(directory, 'sleep').write_text('#!/bin/sh\nexit 0\n')
            for stub in ('timeout', 'sleep'):
                Path(directory, stub).chmod(0o755)
            return execute(text, {'KUBERNETES_SERVICE_HOST': '10.96.0.1', 'KUBERNETES_SERVICE_PORT': '443'}, directory + ':')

    def test_egress_gate_requires_repeated_denial(self):
        self.assertEqual(self.gate(['timeout', 'timeout', 'timeout']).returncode, 0)
        self.assertEqual(self.gate(['open', 'open', 'refused', 'timeout', 'refused']).returncode, 0, 'enforcement may start late')
        self.assertEqual(self.gate(['timeout', 'timeout', 'open', 'timeout', 'timeout', 'open']).returncode, 1,
                         'an established connection resets the count, however slow the server is')
        self.assertEqual(self.gate(['open']).returncode, 1)
        self.assertEqual(self.gate(['other']).returncode, 1, 'a resolution or tool failure is not a denial')
        text = step('maven-build-test', 'egress-gate')['script']
        self.assertEqual(execute(text, {}).returncode, 1, 'no address to probe is not a denial')

    def test_summary_reports_counts_failing_tests_compiler_errors_and_missing_dependencies(self):
        text = step('maven-build-test', 'summarize')['script']
        with tempfile.TemporaryDirectory() as directory:
            reports = Path(directory, 'source/app/target')
            Path(reports, 'surefire-reports').mkdir(parents=True)
            Path(reports, 'failsafe-reports').mkdir(parents=True)
            Path(reports, 'surefire-reports/TEST-a.xml').write_text(
                '<?xml version="1.0"?>\n<testsuite xmlns:xsi="x" name="a" time="1.2" tests="70" errors="0" skipped="1" failures="2">\n'
                '<testcase name="ok" classname="org.a.ATests" time="0.1"/>\n'
                '<testcase name="breaks" classname="org.a.ATests" time="0.1">\n<failure message="x" type="y">trace</failure>\n</testcase>\n'
                '<testcase name="quo&quot;te" classname="org.a.ATests" time="0.1">\n<failure message="x"/>\n</testcase>\n</testsuite>\n')
            Path(reports, 'failsafe-reports/TEST-b.xml').write_text(
                '<testsuite name="b" tests="5" errors="1" skipped="0" failures="0">\n'
                '<testcase name="it" classname="org.b.BIT" time="2">\n<error message="boom"/>\n</testcase>\n</testsuite>\n')
            Path(reports, 'surefire-reports/TEST-broken.xml').write_text('not xml')
            log = Path(directory, 'maven.log')
            code = Path(directory, 'exit')
            env = {'SOURCE': directory + '/source', 'MODULE': 'app', 'MAVEN_EXIT_CODE_PATH': str(code),
                   'RESULT': directory + '/result'}
            code.write_text('1')
            text = text.replace('/tmp/maven.log', str(log))
            log.write_text(
                '[INFO] x\n[ERROR] ' + directory + '/source/app/src/main/java/A.java:[12,5] cannot find symbol\n'
                '[ERROR] \tCannot access central in offline mode and the artifact a:b:jar:1 has not been downloaded from it before.\n')
            result = execute(text, env)
            self.assertEqual(result.returncode, 0, 'Tekton publishes no results from a failed step; the verdict is mavenExitCode')
            summary = json.loads(Path(directory, 'result').read_text())
            self.assertEqual(summary, {'module': 'app', 'mavenExitCode': 1, 'reports': 2, 'tests': 75, 'failures': 2,
                                       'errors': 1, 'skipped': 1,
                                       'failingTests': ['org.a.ATests.breaks', 'org.a.ATests.quo"te', 'org.b.BIT.it'],
                                       'compilerErrors': ['app/src/main/java/A.java:[12,5] cannot find symbol'],
                                       'missingDependencies': True, 'truncated': False})
            log.write_text('[INFO] ok\n')
            code.write_text('0')
            self.assertEqual(execute(text, env).returncode, 0)
            self.assertFalse(json.loads(Path(directory, 'result').read_text())['missingDependencies'])
            code.unlink()
            self.assertEqual(execute(text, env).returncode, 0)
            self.assertEqual(json.loads(Path(directory, 'result').read_text())['mavenExitCode'], 255)
            env['MODULE'] = 'a"b'
            code.write_text('0')
            execute(text, env)
            self.assertEqual(json.loads(Path(directory, 'result').read_text())['module'], 'invalid')

    def test_summary_stays_within_the_result_bound(self):
        text = step('maven-build-test', 'summarize')['script']
        with tempfile.TemporaryDirectory() as directory:
            reports = Path(directory, 'source/app/target/surefire-reports')
            reports.mkdir(parents=True)
            cases = ''.join('<testcase name="%s" classname="%s">\n<failure/>\n</testcase>\n' % ('n' * 300 + str(i), 'c' * 300)
                            for i in range(50))
            Path(reports, 'TEST-a.xml').write_text('<testsuite tests="50" failures="50">\n' + cases + '</testsuite>\n')
            log = Path(directory, 'maven.log')
            log.write_text(''.join('[ERROR] /x/A%d.java:[1,1] %s\n' % (i, 'e' * 400) for i in range(50)))
            Path(directory, 'exit').write_text('1')
            execute(text.replace('/tmp/maven.log', str(log)),
                    {'SOURCE': directory + '/source', 'MODULE': 'app', 'MAVEN_EXIT_CODE_PATH': directory + '/exit',
                     'RESULT': directory + '/result'})
            raw = Path(directory, 'result').read_text()
            self.assertLessEqual(len(json.dumps(raw)), 1602, 'the kubelet keeps 12288 / 7 containers = 1755 bytes per step')
            summary = json.loads(raw)
            self.assertTrue(summary['truncated'])
            self.assertGreater(len(summary['failingTests']), 0, 'lists are trimmed, not dropped, while some entries fit')

    def test_summary_escaping_cannot_exceed_the_result_bound(self):
        # Names made of characters that double when escaped, plus non-ASCII and
        # control characters: escaping happens after the length bound.
        text = step('maven-build-test', 'summarize')['script']
        hostile = ('\\"' * 200) + '\u00e9\u202e\t'
        with tempfile.TemporaryDirectory() as directory:
            reports = Path(directory, 'source/app/target/surefire-reports')
            reports.mkdir(parents=True)
            cases = ''.join('<testcase name="%s%d" classname="%s">\n<failure/>\n</testcase>\n' % (hostile, i, hostile) for i in range(50))
            Path(reports, 'TEST-a.xml').write_text('<testsuite tests="50" failures="50">\n' + cases + '</testsuite>\n')
            log = Path(directory, 'maven.log')
            log.write_text(''.join('[ERROR] /x/A%d.java:[1,1] %s\n' % (i, hostile) for i in range(50)))
            Path(directory, 'exit').write_text('1')
            execute(text.replace('/tmp/maven.log', str(log)),
                    {'SOURCE': directory + '/source', 'MODULE': 'app', 'MAVEN_EXIT_CODE_PATH': directory + '/exit',
                     'RESULT': directory + '/result'})
            raw = Path(directory, 'result').read_bytes()
            self.assertLessEqual(len(json.dumps(raw.decode())), 1602, 'measured JSON-escaped, as Tekton stores it')
            summary = json.loads(raw)
            self.assertTrue(all(len(name) <= 100 and name.isascii() for name in summary['failingTests']))
            self.assertEqual(summary['mavenExitCode'], 1)

    def test_npm_lock_update_refuses_unsafe_inputs(self):
        text = step('npm-lock-update', 'update')['script']
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, 'source/web').mkdir(parents=True)
            Path(directory, 'tmp').mkdir()
            base = {'SOURCE': directory + '/source', 'OUTPUT': directory + '/out', 'RESULT': directory + '/r',
                    'HOME': directory, 'REGISTRY_URL': ''}
            for directory_value, mode, packages, registry in (
                    ('../web', 'update', ['vite'], ''), ('/web', 'update', ['vite'], ''), ('web', 'install', ['vite'], ''),
                    ('web', 'update', [], ''), ('web', 'audit-fix', ['vite'], ''), ('web', 'update', ['vite;id'], ''),
                    ('web', 'update', ['../x'], ''), ('web', 'update', ['git+https://x/y'], ''),
                    ('web', 'update', ['vite'], 'file:///x'), ('web', 'update', ['vite'], ''), ('web', 'update', ['vite@6.3.5'], ''),
                    ('web\nother', 'update', ['vite'], ''),
                    ('web', 'update', ['a'] * 17, '')):
                env = dict(base, DIRECTORY=directory_value, MODE=mode, REGISTRY_URL=registry, PATH='/usr/bin:/bin')
                result = subprocess.run(['sh', '-c', text.replace('/tmp/', directory + '/tmp/'), 'npm-lock-update'] + packages, env=env,
                                        capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 2, (directory_value, mode, packages, registry, result.stderr))

    def npm(self, directory, stub, packages=('vite',), npmrc=True):
        """Run the update step against a stub npm, with /tmp mapped into the test directory."""
        text = step('npm-lock-update', 'update')['script'].replace('/tmp/', directory + '/tmp/')
        Path(directory, 'tmp').mkdir(exist_ok=True)
        web = Path(directory, 'source/web')
        web.mkdir(parents=True, exist_ok=True)
        web.joinpath('package.json').write_text('{"name":"w"}')
        lock = {'lockfileVersion': 3, 'packages': {
            '': {'name': 'w'},
            'node_modules/vite': {'version': '6.0.0', 'resolved': 'https://registry.npmjs.org/vite/-/vite-6.0.0.tgz'},
            'node_modules/old': {'version': '1.0.0', 'resolved': 'https://registry.npmjs.org/old/-/old-1.0.0.tgz'},
            'node_modules/same': {'version': '1.0.0', 'resolved': 'https://registry.npmjs.org/same/-/same-1.0.0.tgz'}}}
        web.joinpath('package-lock.json').write_text(json.dumps(lock, indent=2))
        if npmrc:
            web.joinpath('.npmrc').write_text('registry=https://evil.example/\n')
        stubs = Path(directory, 'bin')
        stubs.mkdir(exist_ok=True)
        stubs.joinpath('npm').write_text(stub)
        stubs.joinpath('npm').chmod(0o755)
        env = {'SOURCE': directory + '/source', 'OUTPUT': directory + '/out', 'RESULT': directory + '/result',
               'HOME': directory, 'DIRECTORY': 'web', 'MODE': 'update', 'REGISTRY_URL': '',
               'PATH': str(stubs) + ':' + str(Path(shutil.which('node')).parent) + ':/usr/bin:/bin'}
        return subprocess.run(['sh', '-c', text, 'npm-lock-update', *packages], env=env, capture_output=True, text=True, timeout=60)

    def test_npm_lock_update_reports_bounded_changes_without_project_config(self):
        if not shutil.which('node'):
            self.skipTest('node is not installed')
        with tempfile.TemporaryDirectory() as directory:
            # The stub records its argv, working directory listing and registry,
            # then rewrites the lockfile as an update would.
            stub = ('#!/bin/sh\necho "$@" > "%s/argv"\nls -A > "%s/listing"\necho "$NPM_CONFIG_REGISTRY $NPM_CONFIG_GIT $NPM_CONFIG_USERCONFIG $NPM_CONFIG_GLOBALCONFIG" > "%s/config"\n'
                    'node -e \'const f="package-lock.json",j=JSON.parse(require("fs").readFileSync(f));'
                    'j.packages["node_modules/same"].resolved="https://mirror.example/same-1.0.0.tgz";'
                    'j.packages["node_modules/vite"]={version:"6.3.5",resolved:"https://registry.npmjs.org/vite/-/vite-6.3.5.tgz"};'
                    'delete j.packages["node_modules/old"];j.packages["node_modules/new"]={version:"2.0.0",resolved:"https://other.example/new.tgz"};'
                    'require("fs").writeFileSync(f,JSON.stringify(j,null,2))\'\n') % (directory, directory, directory)
            result = self.npm(directory, stub)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(Path(directory, 'argv').read_text().split(),
                             ['update', 'vite', '--package-lock-only', '--ignore-scripts', '--no-audit'])
            self.assertEqual(Path(directory, 'listing').read_text().split(), ['package-lock.json', 'package.json'],
                             'npm runs on copies; the project .npmrc is not there to read')
            self.assertEqual(Path(directory, 'config').read_text().split(),
                             ['https://registry.npmjs.org/', 'false', directory + '/tmp/empty-user-npmrc', directory + '/tmp/empty-global-npmrc'])
            # npm refuses to load one file as both user and global config.
            self.assertEqual(Path(directory, 'tmp/empty-user-npmrc').read_text(), '')
            self.assertEqual(Path(directory, 'tmp/empty-global-npmrc').read_text(), '')
            summary = json.loads(Path(directory, 'result').read_text())
            self.assertEqual(summary['changes'], [{'path': 'new', 'from': None, 'to': '2.0.0'},
                                                  {'path': 'old', 'from': '1.0.0', 'to': None},
                                                  {'path': 'same', 'from': '1.0.0', 'to': '1.0.0', 'resolutionChanged': True},
                                                  {'path': 'vite', 'from': '6.0.0', 'to': '6.3.5'}])
            self.assertEqual((summary['changed'], summary['truncated'], summary['npmExitCode']), (4, False, 0))
            self.assertEqual(summary['resolvedHosts'], ['mirror.example', 'other.example', 'registry.npmjs.org'])
            self.assertEqual(summary['newResolvedHosts'], ['mirror.example', 'other.example'])
            diff = Path(directory, 'out/package-lock.diff').read_bytes()
            self.assertEqual(summary['diffBytes'], len(diff))
            self.assertRegex(summary['diffSHA256'], '^[0-9a-f]{64}$')
            self.assertEqual(json.loads(Path(directory, 'source/web/package-lock.json').read_text())['packages']['node_modules/vite']['version'],
                             '6.0.0', 'the checkout itself is not modified')

    def test_npm_failure_still_publishes_its_summary(self):
        if not shutil.which('node'):
            self.skipTest('node is not installed')
        with tempfile.TemporaryDirectory() as directory:
            result = self.npm(directory, '#!/bin/sh\necho "npm error E404" >&2\nexit 1\n', npmrc=False)
            self.assertEqual(result.returncode, 0, 'Tekton publishes no results from a failed step; the verdict is npmExitCode')
            summary = json.loads(Path(directory, 'result').read_text())
            self.assertEqual((summary['npmExitCode'], summary['changed']), (1, 0))

    def test_npm_egress_gate_requires_repeated_denial(self):
        if not shutil.which('node'):
            self.skipTest('node is not installed')
        text = step('npm-lock-update', 'egress-gate')['script']
        node = shutil.which('node')
        refused = subprocess.run([node, '-e', text], env={'KUBERNETES_SERVICE_HOST': '127.0.0.1', 'KUBERNETES_SERVICE_PORT': '1'},
                                 capture_output=True, text=True, timeout=30)
        self.assertEqual(refused.returncode, 0, refused.stderr)
        self.assertEqual(subprocess.run([node, '-e', text], env={}, capture_output=True, timeout=30).returncode, 1,
                         'no address to probe is not a denial')
        self.assertTrue(text.startswith('#!/usr/local/bin/node'))


class RunToolTests(unittest.TestCase):
    def test_build_test_run_is_read_only_and_can_be_sandboxed(self):
        pipeline = by_kind('Pipeline')['ai-sdlc-maven-build-test']['spec']
        run = runner.maven_build_test('os-santiago/quarkus-super-heroes', SHA, 'rest-villains', ['postgres'], ['-Dtest=!UIResourceTests'],
                                      runtime_class='ai-sdlc-offline')
        spec = run['spec']
        self.assertEqual(spec['pipelineRef'], {'name': 'ai-sdlc-maven-build-test'})
        self.assertNotIn('pipelineSpec', spec)
        self.assertEqual({p['name'] for p in spec['params']}, {p['name'] for p in pipeline['params']})
        self.assertEqual({w['name'] for w in spec['workspaces']}, {w['name'] for w in pipeline['workspaces']})
        cache = spec['workspaces'][1]
        self.assertEqual(cache, {'name': 'maven-cache', 'persistentVolumeClaim': {
            'claimName': tekton.cache_claim('os-santiago/quarkus-super-heroes'), 'readOnly': True}})
        template = spec['taskRunTemplate']
        self.assertEqual(template['serviceAccountName'], 'ai-sdlc-ci-runner')
        self.assertIs(template['podTemplate']['automountServiceAccountToken'], False)
        self.assertEqual(spec['taskRunSpecs'], [{'pipelineTaskName': 'build-test', 'podTemplate': dict(
            template['podTemplate'], runtimeClassName='ai-sdlc-offline')}])
        for name, task in runner.SANDBOXED_TASKS.items():
            self.assertIn(task, {t['name'] for t in by_kind('Pipeline')[name]['spec']['tasks']})
        self.assertNotIn('taskRunSpecs', runner.maven_build_test('a/b', SHA, '.', ['none'], [])['spec'], 'sandbox is opt-in')
        self.assertNotIn('taskRunSpecs', runner.maven_dependencies('a/b', SHA, '.', ['none'], [])['spec'],
                         'the offline gVisor class would cut the fetch task off')
        self.assertEqual(run['metadata']['labels']['ai-sdlc.io/repository'], 'os-santiago.quarkus-super-heroes')

    def test_dependencies_and_npm_runs_bind_their_pipeline_inputs(self):
        deps = runner.maven_dependencies('a/b', SHA, 'm', ['postgres'], [], mirror_url='http://dependency-proxy.ai-sdlc-dependencies.svc/maven2')
        self.assertEqual(deps['spec']['workspaces'][1], {'name': 'maven-cache', 'persistentVolumeClaim': {'claimName': tekton.cache_claim('a/b')}})
        npm = runner.npm_lock_update('a/b', SHA, 'ui/web', 'update', ['vite', '@scope/pkg'])
        for run, name in ((deps, 'ai-sdlc-maven-dependencies'), (npm, 'ai-sdlc-npm-lock-update')):
            pipeline = by_kind('Pipeline')[name]['spec']
            self.assertEqual({p['name'] for p in run['spec']['params']}, {p['name'] for p in pipeline['params']})
            self.assertEqual({w['name'] for w in run['spec']['workspaces']}, {w['name'] for w in pipeline['workspaces']})
        self.assertEqual({p['name']: p['value'] for p in npm['spec']['params']}['packages'], ['vite', '@scope/pkg'])
        with self.assertRaisesRegex(ValueError, 'names'):
            runner.npm_lock_update('a/b', SHA, 'ui/web', 'update', ['vite@6.3.5'])

    def test_render_rejects_mutable_or_unsafe_inputs(self):
        good = ('os-santiago/x', SHA, 'app', ['none'], [])
        cases = [
            dict(revision='main'), dict(revision=SHA[:39]), dict(revision=SHA.upper()),
            dict(repository='x'), dict(repository='a/..'), dict(module='../x'), dict(module='/abs'),
            dict(services=['redis']), dict(services=['none', 'postgres']), dict(services=[]),
            dict(maven_args=['clean']), dict(maven_args=['-s', '/x']), dict(maven_args=['-Dmaven.repo.local=/x']),
            dict(maven_args=['-o']), dict(maven_args=['-Dx=$(id)']), dict(maven_args=['-Da=b c']), dict(maven_args=['-D'] * 40),
        ]
        names = ('repository', 'revision', 'module', 'services', 'maven_args')
        for case in cases:
            values = dict(zip(names, good), **case)
            with self.assertRaises(ValueError, msg=case):
                runner.maven_build_test(*(values[n] for n in names))
        with self.assertRaises(ValueError):
            runner.maven_dependencies(*good, mirror_url='file:///etc')
        with self.assertRaises(ValueError):
            runner.maven_build_test(*good, runtime_class='Bad Class')
        for directory, mode, packages in (('.', 'update', ['vite']), ('w', 'update', []), ('w', 'audit-fix', ['vite']),
                                          ('w', 'update', ['vite; rm']), ('w', 'install', ['vite'])):
            with self.assertRaises(ValueError, msg=(directory, mode, packages)):
                runner.npm_lock_update('a/b', SHA, directory, mode, packages)

    def test_catalog_gates_modules_directories_and_repository(self):
        project = runner.load_project(PROJECT)
        self.assertEqual(project['repository'], 'os-santiago/quarkus-super-heroes')
        self.assertEqual({n for n, m in project['modules'].items() if m['status'] == 'supported'}, {'rest-villains'})
        self.assertEqual(project['npmDirectories'], ['ui-super-heroes/src/main/webui'])
        base = ['maven-build-test', '--project', str(PROJECT), '--revision', SHA]
        with self.assertRaisesRegex(ValueError, 'pending'):
            runner.main(base + ['--module', 'rest-fights'])
        with self.assertRaisesRegex(ValueError, 'unverified'):
            runner.main(base + ['--module', 'rest-heroes'])
        with self.assertRaisesRegex(ValueError, 'differs'):
            runner.main(base + ['--repository', 'a/b', '--module', 'rest-villains'])
        with self.assertRaisesRegex(ValueError, 'not in the project catalog'):
            runner.main(['npm-lock-update', '--project', str(PROJECT), '--revision', SHA, '--directory', 'other', '--package', 'vite'])

    def test_dependencies_only_from_the_current_base_branch_head(self):
        with patch.object(runner, 'base_head', return_value='0' * 40):
            with self.assertRaisesRegex(ValueError, 'base-branch head'):
                runner.main(['maven-dependencies', '--project', str(PROJECT), '--revision', SHA, '--module', 'rest-villains'])
        with self.assertRaisesRegex(ValueError, 'catalog'):
            runner.main(['maven-dependencies', '--repository', 'a/b', '--revision', SHA])

    def test_wait_reports_terminal_result_and_bounds_polling(self):
        done = {'metadata': {'name': 'x', 'namespace': 'ai-sdlc-ci', 'uid': 'u'},
                'status': {'conditions': [{'type': 'Succeeded', 'status': 'True', 'reason': 'Succeeded'}],
                           'results': [{'name': 'commit', 'value': SHA},
                                       {'name': 'summary', 'value': '{"tests":94,"failures":0}'}]}}
        running = {'metadata': {'name': 'x'}, 'status': {'conditions': [{'type': 'Succeeded', 'status': 'Unknown'}]}}
        with patch.object(runner, 'run', side_effect=[Mock(returncode=0, stdout=json.dumps(running)),
                                                     Mock(returncode=1, stdout=''),
                                                     Mock(returncode=0, stdout=json.dumps(done))]):
            observed = runner.wait(['kubectl'], 'x', 100, sleep=Mock(), clock=Mock(return_value=0))
        self.assertEqual((observed['succeeded'], observed['commit'], observed['summary']['tests']), (True, SHA, 94))
        with patch.object(runner, 'run', return_value=Mock(returncode=0, stdout=json.dumps(running))) as calls, \
             self.assertRaisesRegex(ValueError, 'cancelled'):
            runner.wait(['kubectl'], 'x', 5, sleep=Mock(), clock=Mock(side_effect=[0, 1, 10]))
        cancel = calls.call_args.args[0]
        self.assertEqual(cancel[1:3], ['patch', 'pipelinerun.tekton.dev'])
        self.assertIn('{"spec":{"status":"Cancelled"}}', cancel)

    def test_wait_survives_slow_reads_and_reports_unconfirmed_cancellation(self):
        running = {'metadata': {'name': 'x'}, 'status': {'conditions': [{'type': 'Succeeded', 'status': 'Unknown'}]}}
        responses = [subprocess.TimeoutExpired('kubectl', 60), Mock(returncode=0, stdout=json.dumps(running)), Mock(returncode=1, stdout='')]
        with patch.object(runner, 'run', side_effect=responses), \
             self.assertRaisesRegex(ValueError, 'NOT confirmed.*x'):
            runner.wait(['kubectl'], 'x', 5, sleep=Mock(), clock=Mock(side_effect=[0, 1, 10]))
        with patch.object(runner, 'run', side_effect=subprocess.TimeoutExpired('kubectl', 60)):
            self.assertFalse(runner.cancel(['kubectl'], 'x'))

    def test_cache_claim_ignores_repository_case(self):
        upper = runner.maven_build_test('OS-Santiago/Quarkus-Super-Heroes', SHA, 'm', ['none'], [])
        self.assertEqual(upper['spec']['workspaces'][1]['persistentVolumeClaim']['claimName'],
                         tekton.cache_claim('os-santiago/quarkus-super-heroes'))

    def test_a_run_passes_only_with_success_the_requested_commit_and_exit_code_zero(self):
        ok = {'succeeded': True, 'commit': SHA, 'summary': {'mavenExitCode': 0}}
        self.assertTrue(runner.passed(ok, SHA))
        self.assertTrue(runner.passed(dict(ok, summary={'npmExitCode': 0}), SHA))
        for bad in (dict(ok, succeeded=False), dict(ok, commit='0' * 40), dict(ok, commit=None),
                    dict(ok, summary={'mavenExitCode': 1}), dict(ok, summary=None), dict(ok, summary={'mavenExitCode': '0'})):
            self.assertFalse(runner.passed(bad, SHA), bad)


if __name__ == '__main__':
    unittest.main()
