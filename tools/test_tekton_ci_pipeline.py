"""Render and validate the reviewed Tekton CI desired state and its run tool."""
import ipaddress
import json
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from unittest.mock import Mock

import yaml

import tekton_ci_install as tekton
import tekton_ci_run as runner

PROJECT = tekton.DESIRED / 'projects/quarkus-super-heroes.json'
SHA = 'ae00455c01a7a50d89ae8f1db99483abd27abe25'


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


def script(task, step):
    return next(s for s in task['spec']['steps'] if s['name'] == step)['script']


class ManifestTests(unittest.TestCase):
    def test_inventory_and_namespace_scope(self):
        kinds = sorted((d['kind'], d['metadata']['name']) for d in documents())
        self.assertEqual(kinds, sorted([
            ('Namespace', 'ai-sdlc-ci'), ('ServiceAccount', 'ai-sdlc-ci-runner'),
            ('PersistentVolumeClaim', 'ai-sdlc-ci-maven-cache'), ('NetworkPolicy', 'ai-sdlc-ci-default-deny'),
            ('NetworkPolicy', 'ai-sdlc-ci-dns'), ('NetworkPolicy', 'ai-sdlc-ci-public-https'),
            ('Task', 'ai-sdlc-git-clone'), ('Task', 'ai-sdlc-maven-verify'), ('Pipeline', 'ai-sdlc-maven-ci')]))
        for doc in documents():
            if doc['kind'] != 'Namespace':
                self.assertEqual(doc['metadata']['namespace'], 'ai-sdlc-ci', doc['metadata']['name'])
        namespace = by_kind('Namespace')['ai-sdlc-ci']['metadata']['labels']
        self.assertEqual(namespace['pod-security.kubernetes.io/enforce'], 'restricted')
        self.assertIs(by_kind('ServiceAccount')['ai-sdlc-ci-runner']['automountServiceAccountToken'], False)

    def test_every_container_is_pinned_non_root_and_unprivileged(self):
        for task in by_kind('Task').values():
            for role, container in containers(task):
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

    def test_tasks_mount_no_host_paths_secrets_or_projected_tokens(self):
        for task in by_kind('Task').values():
            for volume in task['spec'].get('volumes', []):
                self.assertEqual(set(volume) - {'name'}, {'emptyDir'}, task['metadata']['name'])
                self.assertIn('sizeLimit', volume['emptyDir'])

    def test_parameters_never_reach_script_text(self):
        # Parameters enter through env; script substitution would let a value
        # inject shell syntax.
        for task in by_kind('Task').values():
            for _, container in containers(task):
                self.assertNotRegex(container.get('script', ''), r'\$\(params\.', task['metadata']['name'])

    def test_pipeline_references_resolve_to_reviewed_tasks(self):
        tasks = by_kind('Task')
        pipeline = by_kind('Pipeline')['ai-sdlc-maven-ci']['spec']
        params = {p['name'] for p in pipeline['params']}
        workspaces = {w['name'] for w in pipeline['workspaces']}
        for entry in pipeline['tasks']:
            self.assertEqual(set(entry['taskRef']), {'name'}, 'installed tasks only, no resolvers or bundles')
            task = tasks[entry['taskRef']['name']]['spec']
            declared = {p['name']: p for p in task['params']}
            self.assertLessEqual({p['name'] for p in entry['params']}, set(declared))
            required = {name for name, p in declared.items() if 'default' not in p}
            self.assertLessEqual(required, {p['name'] for p in entry['params']})
            for param in entry['params']:
                for reference in re.findall(r'\$\(params\.([a-z-]+)', json.dumps(param['value'])):
                    self.assertIn(reference, params)
            self.assertEqual({w['name'] for w in entry['workspaces']}, {w['name'] for w in task['workspaces']})
            self.assertLessEqual({w['workspace'] for w in entry['workspaces']}, workspaces)
        verify = tasks['ai-sdlc-maven-verify']['spec']
        cache = {w['name']: w for w in verify['workspaces']}
        self.assertIs(cache['maven-cache']['readOnly'], True)
        self.assertIs(cache['maven-cache']['optional'], True)
        self.assertIs(cache['maven-cache-writable']['optional'], True)
        maven = next(s for s in verify['steps'] if s['name'] == 'maven')
        self.assertEqual(maven['onError'], 'continue', 'the summary step must still publish a result')
        self.assertEqual({r['name'] for r in by_kind('Pipeline')['ai-sdlc-maven-ci']['spec']['results']}, {'commit', 'summary'})

    def test_network_policy_denies_by_default_and_allows_only_dns_and_public_https(self):
        policies = by_kind('NetworkPolicy')
        deny = policies['ai-sdlc-ci-default-deny']['spec']
        self.assertEqual(deny, {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']})
        for task in by_kind('Task').values():
            self.assertEqual(task['metadata']['labels']['ai-sdlc.io/ci-egress'], 'public-https')
        ports = set()
        for name in ('ai-sdlc-ci-dns', 'ai-sdlc-ci-public-https'):
            spec = policies[name]['spec']
            self.assertEqual(spec['policyTypes'], ['Egress'])
            self.assertNotIn('ingress', spec)
            # Tekton copies the Task label below to each task pod.
            self.assertEqual(spec['podSelector'], {'matchLabels': {'ai-sdlc.io/ci-egress': 'public-https'}})
            for rule in spec['egress']:
                ports |= {(p['protocol'], p['port']) for p in rule['ports']}
        self.assertEqual(ports, {('UDP', 53), ('TCP', 53), ('TCP', 443)})
        https = policies['ai-sdlc-ci-public-https']['spec']['egress'][0]['to'][0]['ipBlock']
        excluded = [ipaddress.ip_network(c) for c in https['except']]
        for address in ('10.96.0.1', '10.244.0.5', '172.18.0.2', '192.168.1.1', '169.254.169.254', '127.0.0.1', '100.64.0.1'):
            self.assertTrue(any(ipaddress.ip_address(address) in n for n in excluded), address)
        self.assertEqual(https['cidr'], '0.0.0.0/0')

    def test_sidecar_starts_only_when_requested_and_binds_loopback(self):
        verify = by_kind('Task')['ai-sdlc-maven-verify']
        postgres = verify['spec']['sidecars'][0]
        self.assertIn('listen_addresses=127.0.0.1', postgres['script'])
        self.assertIn('exec sleep', postgres['script'])
        self.assertIn('readinessProbe', postgres)
        values = {e['name']: e['value'] for e in postgres['env']}
        self.assertEqual(values['POSTGRES_HOST_AUTH_METHOD'], 'trust')
        self.assertFalse(any('PASSWORD' in name for name in values))


class ScriptTests(unittest.TestCase):
    """Run the reviewed shell scripts locally against their input checks and report parsing."""

    def sh(self, text, env):
        return subprocess.run(['sh', '-c', text], env=dict(env, PATH='/usr/bin:/bin'), capture_output=True, text=True, timeout=30)

    def test_clone_refuses_mutable_or_malformed_inputs_before_network(self):
        text = script(by_kind('Task')['ai-sdlc-git-clone'], 'clone')
        with tempfile.TemporaryDirectory() as directory:
            base = {'DESTINATION': directory + '/src', 'RESULT': directory + '/result', 'HOME': directory}
            for repository, revision in (('os-santiago/x', 'main'), ('os-santiago/x', SHA[:12]),
                                         ('os-santiago/x', SHA.upper()), ('../x', SHA), ('a/b;id', SHA),
                                         ('a/..', SHA), ('https://evil/x', SHA)):
                result = self.sh(text, dict(base, REPOSITORY=repository, REVISION=revision))
                self.assertEqual(result.returncode, 2, (repository, revision, result.stderr))
                self.assertFalse(Path(directory, 'src').exists())

    def test_maven_step_refuses_unsafe_module_services_and_mirror(self):
        text = script(by_kind('Task')['ai-sdlc-maven-verify'], 'maven')
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, 'source/app').mkdir(parents=True)
            Path(directory, 'source/app/pom.xml').write_text('<project/>')
            base = {'SOURCE': directory + '/source', 'HOME': directory + '/home', 'MAVEN_MIRROR_URL': '',
                    'CACHE_READONLY_BOUND': 'false', 'CACHE_WRITABLE_BOUND': 'false', 'CACHE_READONLY': '', 'CACHE_WRITABLE': ''}
            for module, services, mirror in (('../app', 'none', ''), ('app/../..', 'none', ''), ('/etc', 'none', ''),
                                             ('app', 'redis', ''), ('app', 'postgres;id', ''), ('missing', 'none', ''),
                                             ('app', 'none', 'http://x/<y>'), ('app', 'none', 'file:///etc')):
                result = self.sh(text, dict(base, MODULE=module, SERVICES=services, MAVEN_MIRROR_URL=mirror))
                self.assertEqual(result.returncode, 2, (module, services, mirror, result.stderr))

    def test_summary_sums_surefire_and_failsafe_reports_and_propagates_failure(self):
        text = script(by_kind('Task')['ai-sdlc-maven-verify'], 'summarize')
        with tempfile.TemporaryDirectory() as directory:
            reports = Path(directory, 'source/app/target')
            for kind, attributes in (('surefire', 'tests="70" errors="0" skipped="1" failures="2"'),
                                     ('failsafe', 'tests="5" errors="1" skipped="0" failures="0"')):
                Path(reports, kind + '-reports').mkdir(parents=True)
                Path(reports, kind + '-reports/TEST-a.xml').write_text(
                    '<?xml version="1.0"?>\n<testsuite xmlns:xsi="x" name="a" time="1.2" ' + attributes + '>\n</testsuite>\n')
            Path(reports, 'surefire-reports/TEST-broken.xml').write_text('not xml')
            code = Path(directory, 'exit')
            env = {'SOURCE': directory + '/source', 'MODULE': 'app', 'MAVEN_EXIT_CODE_PATH': str(code),
                   'RESULT': directory + '/result'}
            code.write_text('1')
            result = self.sh(text, env)
            self.assertEqual(result.returncode, 1)
            summary = json.loads(Path(directory, 'result').read_text())
            self.assertEqual(summary, {'module': 'app', 'mavenExitCode': 1, 'reports': 2, 'tests': 75,
                                       'failures': 2, 'errors': 1, 'skipped': 1})
            code.write_text('0')
            self.assertEqual(self.sh(text, env).returncode, 0)
            code.unlink()
            self.assertEqual(self.sh(text, env).returncode, 1, 'a missing exit code is a failure')
            self.assertEqual(json.loads(Path(directory, 'result').read_text())['mavenExitCode'], 255)
            env['MODULE'] = 'a"b'
            code.write_text('0')
            self.sh(text, env)
            self.assertEqual(json.loads(Path(directory, 'result').read_text())['module'], 'invalid')


    def gate(self, curl_exit_codes):
        text = script(by_kind('Task')['ai-sdlc-maven-verify'], 'egress-gate')
        with tempfile.TemporaryDirectory() as directory:
            codes = Path(directory, 'codes')
            codes.write_text('\n'.join(map(str, curl_exit_codes)) + '\n')
            # Stubs: curl replays the next exit code (repeating the last), sleep returns at once.
            Path(directory, 'curl').write_text('#!/bin/sh\nc=$(head -n1 "%s")\n[ "$(wc -l < "%s")" -gt 1 ] && sed -i 1d "%s"\nexit "$c"\n' % (codes, codes, codes))
            Path(directory, 'sleep').write_text('#!/bin/sh\nexit 0\n')
            for stub in ('curl', 'sleep'):
                Path(directory, stub).chmod(0o755)
            env = {'KUBERNETES_SERVICE_HOST': '10.96.0.1', 'KUBERNETES_SERVICE_PORT': '443'}
            return subprocess.run(['sh', '-c', text], env=dict(env, PATH=directory + ':/usr/bin:/bin'),
                                  capture_output=True, text=True, timeout=60)

    def test_egress_gate_requires_repeated_denial_before_maven_runs(self):
        steps = [s['name'] for s in by_kind('Task')['ai-sdlc-maven-verify']['spec']['steps']]
        self.assertEqual(steps, ['egress-gate', 'maven', 'summarize'])
        gate = by_kind('Task')['ai-sdlc-maven-verify']['spec']['steps'][0]
        self.assertNotIn('onError', gate, 'a failed gate must stop the task before Maven')
        self.assertEqual(self.gate([28, 28, 28]).returncode, 0)
        self.assertEqual(self.gate([0, 0, 7, 28, 7]).returncode, 0, 'enforcement may start late')
        self.assertEqual(self.gate([28, 28, 0, 28, 28, 0]).returncode, 1, 'a success resets the denial count')
        self.assertEqual(self.gate([0]).returncode, 1)
        self.assertEqual(self.gate([6]).returncode, 1, 'a resolution or tool failure is not a denial')
        text = script(by_kind('Task')['ai-sdlc-maven-verify'], 'egress-gate')
        self.assertEqual(subprocess.run(['sh', '-c', text], env={'PATH': '/usr/bin:/bin'}, capture_output=True,
                                        timeout=10).returncode, 1, 'no address to probe is not a denial')


class RunToolTests(unittest.TestCase):
    def test_rendered_run_binds_reviewed_pipeline_inputs(self):
        pipeline = by_kind('Pipeline')['ai-sdlc-maven-ci']['spec']
        run = runner.render('os-santiago/quarkus-super-heroes', SHA, 'rest-villains', ['postgres'], ['-Dtest=!UIResourceTests'])
        spec = run['spec']
        self.assertEqual(spec['pipelineRef'], {'name': 'ai-sdlc-maven-ci'})
        self.assertNotIn('pipelineSpec', spec)
        self.assertLessEqual({p['name'] for p in spec['params']}, {p['name'] for p in pipeline['params']})
        self.assertLessEqual({w['name'] for w in spec['workspaces']}, {w['name'] for w in pipeline['workspaces']})
        params = {p['name']: p['value'] for p in spec['params']}
        self.assertEqual(params['revision'], SHA)
        self.assertEqual(params['services'], 'postgres')
        self.assertEqual(params['maven-args'], ['-Dtest=!UIResourceTests'])
        cache = next(w for w in spec['workspaces'] if w['name'].startswith('maven-cache'))
        self.assertEqual(cache, {'name': 'maven-cache', 'persistentVolumeClaim': {'claimName': 'ai-sdlc-ci-maven-cache', 'readOnly': True}})
        template = spec['taskRunTemplate']
        self.assertEqual(template['serviceAccountName'], 'ai-sdlc-ci-runner')
        self.assertIs(template['podTemplate']['automountServiceAccountToken'], False)
        self.assertIs(template['podTemplate']['securityContext']['runAsNonRoot'], True)
        self.assertEqual(run['metadata']['labels']['ai-sdlc.io/repository'], 'os-santiago.quarkus-super-heroes')
        warm = runner.render('os-santiago/quarkus-super-heroes', SHA, 'rest-villains', ['postgres'], [], warm_cache=True)
        self.assertEqual(warm['spec']['workspaces'][1]['name'], 'maven-cache-writable')

    def test_render_rejects_mutable_or_unsafe_inputs(self):
        good = ('os-santiago/x', SHA, 'app', ['none'], [])
        cases = [
            dict(revision='main'), dict(revision=SHA[:39]), dict(revision=SHA.upper()),
            dict(repository='x'), dict(repository='a/..'), dict(module='../x'), dict(module='/abs'),
            dict(services=['redis']), dict(services=['none', 'postgres']), dict(services=[]),
            dict(maven_args=['clean']), dict(maven_args=['-s', '/x']), dict(maven_args=['-Dmaven.repo.local=/x']),
            dict(maven_args=['-Dx=$(id)']), dict(maven_args=['-Da=b c']), dict(maven_args=['-D'] * 40),
        ]
        names = ('repository', 'revision', 'module', 'services', 'maven_args')
        for case in cases:
            values = dict(zip(names, good), **case)
            with self.assertRaises(ValueError, msg=case):
                runner.render(*(values[n] for n in names))
        with self.assertRaises(ValueError):
            runner.render(*good, mirror_url='file:///etc')

    def test_project_catalog_is_valid_and_only_supported_modules_run_by_default(self):
        project = runner.load_project(PROJECT)
        self.assertEqual(project['repository'], 'os-santiago/quarkus-super-heroes')
        supported = {n for n, m in project['modules'].items() if m['status'] == 'supported'}
        self.assertEqual(supported, {'rest-villains'})
        for module in project['modules'].values():
            if module['status'] != 'pending':
                self.assertLessEqual(set(module['services']), set(runner.SERVICES))
        with self.assertRaisesRegex(ValueError, 'pending'):
            runner.main(['--project', str(PROJECT), '--revision', SHA, '--module', 'rest-fights'])
        with self.assertRaisesRegex(ValueError, 'unverified'):
            runner.main(['--project', str(PROJECT), '--revision', SHA, '--module', 'rest-heroes'])
        with self.assertRaisesRegex(ValueError, 'differs'):
            runner.main(['--project', str(PROJECT), '--repository', 'a/b', '--revision', SHA, '--module', 'rest-villains'])

    def test_warm_cache_requires_current_base_branch_head(self):
        original = runner.base_head
        try:
            runner.base_head = Mock(return_value='0' * 40)
            with self.assertRaisesRegex(ValueError, 'base-branch head'):
                runner.main(['--project', str(PROJECT), '--revision', SHA, '--module', 'rest-villains', '--warm-cache'])
        finally:
            runner.base_head = original

    def test_wait_reports_terminal_result_and_bounds_polling(self):
        done = {'metadata': {'name': 'maven-ci-x', 'namespace': 'ai-sdlc-ci', 'uid': 'u'},
                'status': {'conditions': [{'type': 'Succeeded', 'status': 'True', 'reason': 'Succeeded'}],
                           'results': [{'name': 'commit', 'value': SHA},
                                       {'name': 'summary', 'value': '{"tests":74,"failures":0}'}]}}
        running = {'metadata': {'name': 'maven-ci-x'}, 'status': {'conditions': [{'type': 'Succeeded', 'status': 'Unknown'}]}}
        original = runner.run
        try:
            runner.run = Mock(side_effect=[Mock(returncode=0, stdout=json.dumps(running)),
                                           Mock(returncode=1, stdout=''),
                                           Mock(returncode=0, stdout=json.dumps(done))])
            observed = runner.wait(['kubectl'], 'maven-ci-x', 100, sleep=Mock(), clock=Mock(return_value=0))
            self.assertEqual((observed['succeeded'], observed['commit'], observed['summary']['tests']), (True, SHA, 74))
            runner.run = Mock(return_value=Mock(returncode=0, stdout=json.dumps(running)))
            with self.assertRaisesRegex(ValueError, 'wait bound'):
                runner.wait(['kubectl'], 'maven-ci-x', 5, sleep=Mock(), clock=Mock(side_effect=[0, 1, 10]))
        finally:
            runner.run = original


if __name__ == '__main__':
    unittest.main()
