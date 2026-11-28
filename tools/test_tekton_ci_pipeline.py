"""Render and validate the reviewed Tekton task catalog and its run tool."""
import hashlib
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
FETCH_TASKS = {'source-checkout', 'maven-dependencies', 'npm-lock-update', 'dependency-audit'}


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


def go_marshal_string(value):
    """Encode a string exactly as Go's encoding/json does (HTML escaping on)."""
    out = ['"']
    for ch in value:
        if ch == '"':
            out.append('\\"')
        elif ch == '\\':
            out.append('\\\\')
        elif ch == '\n':
            out.append('\\n')
        elif ch == '\r':
            out.append('\\r')
        elif ch == '\t':
            out.append('\\t')
        elif ord(ch) < 0x20 or ch in '<>&\u2028\u2029':
            out.append('\\u%04x' % ord(ch))
        else:
            out.append(ch)
    return ''.join(out) + '"'


def termination_message(summary):
    """The step's termination message bytes as Tekton writes them for one result."""
    return ('[{"key":"summary","value":' + go_marshal_string(summary) + ',"type":1},'
            '{"key":"StartedAt","value":"2026-11-27T21:18:36.996Z","type":3}]').encode()


class GoMarshalEmulationTests(unittest.TestCase):
    def test_emulation_matches_known_go_output(self):
        # Reference encodings of encoding/json.Marshal for these strings.
        self.assertEqual(go_marshal_string('a"b\\c<d>&e\n'), '"a\\"b\\\\c\\u003cd\\u003e\\u0026e\\n"')
        self.assertEqual(go_marshal_string('\u2028\u00e9'), '"\\u2028\u00e9"')


class ManifestTests(unittest.TestCase):
    def test_inventory_and_namespace_scope(self):
        kinds = sorted((d['kind'], d['metadata']['name']) for d in documents())
        self.assertEqual(kinds, sorted([
            ('Namespace', 'ai-sdlc-ci'), ('ServiceAccount', 'ai-sdlc-ci-runner'),
            ('ServiceAccount', 'default'), ('NetworkPolicy', 'ai-sdlc-ci-default-deny'), ('ResourceQuota', 'ai-sdlc-ci-candidate-pods'), ('ResourceQuota', 'ai-sdlc-ci-fetch-pods'),
            ('PriorityClass', 'ai-sdlc-ci-candidate'),
            ('ValidatingAdmissionPolicy', 'ai-sdlc-ci-pipelineruns'), ('ValidatingAdmissionPolicyBinding', 'ai-sdlc-ci-pipelineruns'),
            ('ValidatingAdmissionPolicy', 'ai-sdlc-ci-pipelinerun-labels'),
            ('ValidatingAdmissionPolicyBinding', 'ai-sdlc-ci-pipelinerun-labels'),
            ('ValidatingAdmissionPolicy', 'ai-sdlc-ci-candidate-configmaps'),
            ('ValidatingAdmissionPolicyBinding', 'ai-sdlc-ci-candidate-configmaps'),
            ('NetworkPolicy', 'ai-sdlc-ci-fetch-dns'), ('NetworkPolicy', 'ai-sdlc-ci-fetch-https'),
            ('NetworkPolicy', 'ai-sdlc-ci-dependency-proxy'),
            ('Task', 'source-checkout'), ('Task', 'apply-candidate'), ('Task', 'maven-build-test'), ('Task', 'maven-dependencies'),
            ('Task', 'npm-lock-update'), ('Task', 'dependency-audit')] + [('Pipeline', p) for p in tekton.PIPELINES]))
        for doc in documents():
            if doc['kind'] not in ('Namespace', 'ValidatingAdmissionPolicy', 'ValidatingAdmissionPolicyBinding', 'PriorityClass'):
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
        self.assertIn("['accessModes', 'resources']", rules, 'source claims allow no selector, class, data source or volume')
        self.assertIn("['automountServiceAccountToken', 'securityContext', 'runtimeClassName', 'priorityClassName']", rules, 'pod template allowlist')
        self.assertIn("['name', 'computeResources']", rules, 'step and sidecar overrides')
        self.assertIn('readOnly == true', rules)
        namespace = by_kind('Namespace')['ai-sdlc-ci']['metadata']['labels']
        self.assertEqual(namespace['pod-security.kubernetes.io/enforce'], 'restricted')
        self.assertIs(by_kind('ServiceAccount')['ai-sdlc-ci-runner']['automountServiceAccountToken'], False)

    def test_egress_label_cannot_change_on_update(self):
        policy = by_kind('ValidatingAdmissionPolicy')['ai-sdlc-ci-pipelinerun-labels']['spec']
        self.assertEqual(policy['matchConstraints']['resourceRules'][0]['operations'], ['UPDATE'])
        self.assertEqual([v['expression'] for v in policy['validations']], ['variables.egress == variables.oldEgress'])
        binding = by_kind('ValidatingAdmissionPolicyBinding')['ai-sdlc-ci-pipelinerun-labels']['spec']
        self.assertEqual((binding['policyName'], binding['validationActions']), ('ai-sdlc-ci-pipelinerun-labels', ['Deny']))

    def test_configmaps_in_the_ci_namespace_are_only_immutable_candidates(self):
        policy = by_kind('ValidatingAdmissionPolicy')['ai-sdlc-ci-candidate-configmaps']['spec']
        self.assertEqual(policy['matchConstraints']['resourceRules'], [{'apiGroups': [''], 'apiVersions': ['*'],
                                                                        'operations': ['CREATE', 'UPDATE'], 'resources': ['configmaps']}])
        self.assertEqual({c['name'] for c in policy['matchConditions']}, {'not-root-ca', 'not-cluster-admin'})
        rules = ' '.join(v['expression'] for v in policy['validations'])
        for required in ('^ai-sdlc-candidate-[0-9a-f]{16}-[0-9a-z]{6}$', 'object.immutable == true', '!has(object.binaryData)',
                         'object.data.size() == 2', "'ai-sdlc.io/candidate'"):
            self.assertIn(required, rules)
        binding = by_kind('ValidatingAdmissionPolicyBinding')['ai-sdlc-ci-candidate-configmaps']['spec']
        self.assertEqual((binding['policyName'], binding['validationActions']), ('ai-sdlc-ci-candidate-configmaps', ['Deny']))
        self.assertEqual(binding['matchResources'], {'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'ai-sdlc-ci'}}})

    def test_candidate_and_fetch_pods_fit_beside_the_measured_node_baseline(self):
        quotas = by_kind('ResourceQuota')
        candidate, fetch = quotas['ai-sdlc-ci-candidate-pods']['spec'], quotas['ai-sdlc-ci-fetch-pods']['spec']
        self.assertEqual(candidate['scopeSelector']['matchExpressions'],
                         [{'scopeName': 'PriorityClass', 'operator': 'In', 'values': ['ai-sdlc-ci-candidate']}])
        self.assertEqual(fetch['scopeSelector']['matchExpressions'],
                         [{'scopeName': 'PriorityClass', 'operator': 'NotIn', 'values': ['ai-sdlc-ci-candidate']}])
        priority = by_kind('PriorityClass')['ai-sdlc-ci-candidate']
        self.assertEqual((priority['value'], priority['preemptionPolicy'], priority['globalDefault']), (0, 'Never', False))
        pods = json.loads((tekton.ROOT / 'runtime/k8s/deploy/local/offline-runtime/kubelet-pods.json').read_text())['podPidsLimit']
        node_limit, baseline_peak, fetch_peak = 2048, 1100, 150  # measured on the existing kind nodes and a Maven pod with tests
        total = baseline_peak + int(candidate['hard']['pods']) * pods + int(fetch['hard']['pods']) * fetch_peak
        self.assertLessEqual(total, node_limit, total)
        self.assertIn('podPidsLimit: %d' % pods, (tekton.ROOT / 'runtime/k8s/deploy/local/kind.yaml').read_text())
        rules = ' '.join(v['expression'] for v in by_kind('ValidatingAdmissionPolicy')['ai-sdlc-ci-pipelineruns']['spec']['validations'])
        self.assertIn("s.podTemplate.priorityClassName == 'ai-sdlc-ci-candidate'", rules, 'build-test must use the candidate quota')
        # Only build-test may claim the single candidate slot: a checkout or
        # npm task with the class would hold it without running candidate code.
        flat = ' '.join(' '.join(v['expression'] for v in by_kind('ValidatingAdmissionPolicy')['ai-sdlc-ci-pipelineruns']['spec']['validations']).split())
        self.assertIn("(s.podTemplate.priorityClassName == 'ai-sdlc-ci-candidate' && object.spec.pipelineRef.name == 'ai-sdlc-maven-build-test' "
                      "&& s.pipelineTaskName == 'build-test')", flat)

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
                declared = {p['name']: p for p in task.get('params', [])}
                self.assertLessEqual({p['name'] for p in entry.get('params', [])}, set(declared))
                required = {name for name, p in declared.items() if 'default' not in p}
                self.assertLessEqual(required, {p['name'] for p in entry.get('params', [])})
                for param in entry.get('params', []):
                    for reference in re.findall(r'\$\(params\.([a-z-]+)', json.dumps(param['value'])):
                        self.assertIn(reference, params)
                bound = {w['name'] for w in entry['workspaces']}
                self.assertLessEqual({w['name'] for w in task['workspaces'] if not w.get('optional')}, bound)
                self.assertLessEqual(bound, {w['name'] for w in task['workspaces']})
                for w in entry['workspaces']:
                    # Tekton refuses an optional pipeline workspace bound to a required task workspace.
                    if next(p for p in spec['workspaces'] if p['name'] == w['workspace']).get('optional'):
                        self.assertTrue(next(t for t in task['workspaces'] if t['name'] == w['name']).get('optional'), w)
                self.assertLessEqual({w['workspace'] for w in entry['workspaces']}, workspaces)
            candidate = any(w['name'] == 'candidate' for w in spec['workspaces'])
            self.assertEqual({r['name'] for r in spec['results']}, {'commit', 'summary'} | ({'tree', 'candidate-sha256'} if candidate else set()))
            # npm resolves URL specs and lockfile hosts from the diff under the
            # fetch egress; it takes no candidate until proxy-only egress.
            self.assertEqual(candidate, pipeline['metadata']['name'] == 'ai-sdlc-maven-build-test', pipeline['metadata']['name'])
            if candidate:
                apply = next(t for t in spec['tasks'] if t['name'] == 'apply')
                self.assertEqual(apply['when'], [{'input': '$(workspaces.candidate.bound)', 'operator': 'in', 'values': ['true']}])
                self.assertEqual(next(w for w in spec['workspaces'] if w['name'] == 'candidate'), {'name': 'candidate', 'optional': True})
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

    def apply_candidate(self, diff, digest=None):
        text = step('apply-candidate', 'apply')['script']
        directory = tempfile.mkdtemp()
        source = Path(directory, 'source')
        source.mkdir()
        git = lambda *a: subprocess.run(['git', '-C', str(source), *a], check=True, capture_output=True, text=True,
                                        env={'HOME': directory, 'GIT_CONFIG_NOSYSTEM': '1', 'PATH': '/usr/bin:/bin'})
        git('init', '-q')
        source.joinpath('A.java').write_text('class A {}\n')
        git('add', '-A')
        git('-c', 'user.name=t', '-c', 'user.email=t@e', 'commit', '-qm', 'base')
        candidate = Path(directory, 'candidate')
        candidate.mkdir()
        candidate.joinpath('candidate.diff').write_bytes(diff)
        candidate.joinpath('candidate.sha256').write_text(digest or hashlib.sha256(diff).hexdigest())
        result = execute(text, {'SOURCE': str(source), 'CANDIDATE': str(candidate), 'RESULT': directory + '/tree',
                                'DIGEST_RESULT': directory + '/digest', 'HOME': directory, 'GIT_CONFIG_NOSYSTEM': '1'})
        tree = Path(directory, 'tree').read_text() if Path(directory, 'tree').exists() else None
        return result, tree, source

    def test_apply_candidate_verifies_digest_and_paths_and_publishes_the_tree(self):
        good = b'--- a/A.java\n+++ b/A.java\n@@ -1 +1 @@\n-class A {}\n+class A { int x; }\n'
        result, tree, source = self.apply_candidate(good)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertRegex(tree, '^[0-9a-f]{40}$')
        self.assertEqual(source.joinpath('A.java').read_text(), 'class A { int x; }\n')
        self.assertEqual(Path(source.parent, 'digest').read_text(), hashlib.sha256(good).hexdigest())
        cases = {
            'digest mismatch': (good, '0' * 64, 'candidate diff digest mismatch'),
            'parent path': (b'--- a/../x\n+++ b/../x\n@@ -0,0 +1 @@\n+x\n', None, 'outside the tree'),
            'git metadata': (b'--- /dev/null\n+++ b/.git/hooks/pre-commit\n@@ -0,0 +1 @@\n+x\n', None, 'outside the tree'),
            'binary': (b'diff --git a/b.bin b/b.bin\nGIT binary patch\nliteral 1\nIcmZ?d00001\n\n', None, 'binary candidate changes'),
            'symlink': (b'diff --git a/l b/l\nnew file mode 120000\nindex 0000000..e69de29\n--- /dev/null\n+++ b/l\n@@ -0,0 +1 @@\n+/etc/passwd\n\\ No newline at end of file\n',
                        None, 'symbolic link or submodule'),
            'symlink index line': (b'diff --git a/l b/l\nindex 0000000..e69de29 120000\n--- a/l\n+++ b/l\n@@ -1 +1 @@\n-a\n+b\n', None, 'symbolic link or submodule'),
            'submodule': (b'diff --git a/m b/m\nnew file mode 160000\nindex 0000000..1111111\n--- /dev/null\n+++ b/m\n@@ -0,0 +1 @@\n+Subproject commit 1111111111111111111111111111111111111111\n',
                          None, 'symbolic link or submodule'),
            'does not apply': (b'--- a/A.java\n+++ b/A.java\n@@ -1 +1 @@\n-class B {}\n+class C {}\n', None, 'does not apply'),
            'size bound': (b'x' * (786432 + 1), None, 'exceeds 768 KiB'),
            'symlink deletion': (b'diff --git a/l b/l\ndeleted file mode 120000\nindex e69de29..0000000\n--- a/l\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n',
                                 None, 'adds, changes or deletes a symbolic link'),
        }
        for name, (diff, digest, message) in cases.items():
            result = self.apply_candidate(diff, digest)[0]
            self.assertEqual(result.returncode, 2, (name, result.stderr))
            self.assertIn(message, result.stderr, name)

    def test_apply_candidate_accepts_a_staged_rename(self):
        diff = (b'diff --git a/A.java b/Moved.java\nsimilarity index 100%\nrename from A.java\nrename to Moved.java\n')
        result, tree, source = self.apply_candidate(diff)
        self.assertEqual(result.returncode, 0, result.stderr)
        listed = subprocess.run(['git', '-C', str(source), 'ls-tree', '--name-only', tree], capture_output=True, text=True, check=True).stdout.split()
        self.assertEqual(listed, ['Moved.java'])
        delete_add = (b'diff --git a/A.java b/A.java\ndeleted file mode 100644\nindex 1111111..0000000\n--- a/A.java\n+++ /dev/null\n'
                      b'@@ -1 +0,0 @@\n-class A {}\n'
                      b'diff --git a/Moved.java b/Moved.java\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/Moved.java\n'
                      b'@@ -0,0 +1 @@\n+class A {}\n')
        result, tree, source = self.apply_candidate(delete_add)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_apply_candidate_tree_includes_files_a_gitignore_in_the_diff_hides(self):
        diff = (b'diff --git a/.gitignore b/.gitignore\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/.gitignore\n'
                b'@@ -0,0 +1 @@\n+Helper.java\n'
                b'diff --git a/Helper.java b/Helper.java\nnew file mode 100644\nindex 0000000..2222222\n--- /dev/null\n+++ b/Helper.java\n'
                b'@@ -0,0 +1 @@\n+class Helper {}\n')
        result, tree, source = self.apply_candidate(diff)
        self.assertEqual(result.returncode, 0, result.stderr)
        listed = subprocess.run(['git', '-C', str(source), 'ls-tree', '--name-only', tree], capture_output=True, text=True, check=True).stdout.split()
        self.assertEqual(sorted(listed), ['.gitignore', 'A.java', 'Helper.java'], 'the tree must describe what the build compiles')

    def test_apply_candidate_refuses_a_work_tree_the_index_does_not_describe(self):
        diff = b'--- a/A.java\n+++ b/A.java\n@@ -1 +1 @@\n-class A {}\n+class A { int x; }\n'
        text = step('apply-candidate', 'apply')['script'].replace('cd "$SOURCE"', 'cd "$SOURCE"; echo stray > Stray.java')
        directory = tempfile.mkdtemp()
        source = Path(directory, 'source')
        source.mkdir()
        env = {'HOME': directory, 'GIT_CONFIG_NOSYSTEM': '1', 'PATH': '/usr/bin:/bin'}
        for args in (['init', '-q'], ['add', '-A'], ['-c', 'user.name=t', '-c', 'user.email=t@e', 'commit', '-qm', 'base', '--allow-empty']):
            subprocess.run(['git', '-C', str(source), *args], check=True, capture_output=True, env=env)
        source.joinpath('A.java').write_text('class A {}\n')
        subprocess.run(['git', '-C', str(source), 'add', 'A.java'], check=True, env=env)
        subprocess.run(['git', '-C', str(source), '-c', 'user.name=t', '-c', 'user.email=t@e', 'commit', '-qm', 'a'], check=True, env=env)
        candidate = Path(directory, 'candidate')
        candidate.mkdir()
        candidate.joinpath('candidate.diff').write_bytes(diff)
        candidate.joinpath('candidate.sha256').write_text(hashlib.sha256(diff).hexdigest())
        result = execute(text, {'SOURCE': str(source), 'CANDIDATE': str(candidate), 'RESULT': directory + '/tree',
                                'DIGEST_RESULT': directory + '/digest', 'HOME': directory, 'GIT_CONFIG_NOSYSTEM': '1'})
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn('differs from the applied index', result.stderr)

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
                ' unreachable) echo "bash: connect: Network is unreachable" >&2; exit 1;;'
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
        self.assertEqual(self.gate(['unreachable', 'unreachable', 'unreachable']).returncode, 0, 'the gVisor sandbox has no network')
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
                '[ERROR] ' + directory + '/source/app/src/main/java/A.java:[12,5] cannot find symbol\n'
                '[ERROR] \tCannot access central in offline mode and the artifact a:b:jar:1 has not been downloaded from it before.\n')
            result = execute(text, env)
            self.assertEqual(result.returncode, 0, 'Tekton publishes no results from a failed step; the verdict is mavenExitCode')
            summary = json.loads(Path(directory, 'result').read_text())
            self.assertEqual(summary, {'module': 'app', 'mavenExitCode': 1, 'reports': 2, 'tests': 75, 'failures': 2,
                                       'errors': 1, 'skipped': 1,
                                       'failingTests': ['org.a.ATests.breaks', 'org.a.ATests.quo"te', 'org.b.BIT.it'],
                                       'compilerErrors': ['A.java:[12,5] cannot find symbol'],
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

    def test_summary_fits_the_kubelet_budget_after_go_encoding(self):
        # Names decoded from &lt; and generic compiler errors are full of <, >
        # and &, which Go's json.Marshal writes as 6 bytes each.
        text = step('maven-build-test', 'summarize')['script']
        with tempfile.TemporaryDirectory() as directory:
            reports = Path(directory, 'source/app/target/surefire-reports')
            reports.mkdir(parents=True)
            name = '&lt;' * 40 + '&amp;&gt;'
            cases = ''.join('<testcase name="%s%d" classname="%s">\n<failure/>\n</testcase>\n' % (name, i, name) for i in range(10))
            Path(reports, 'TEST-a.xml').write_text('<testsuite tests="10" failures="10">\n' + cases + '</testsuite>\n')
            log = Path(directory, 'maven.log')
            log.write_text(''.join('[ERROR] /x/A%d.java:[1,1] incompatible types: List<Map<String,Set<T>>> & <? extends X>\n' % i for i in range(9)))
            Path(directory, 'exit').write_text('1')
            for task in ('maven-build-test', 'maven-dependencies'):
                result = execute(step(task, 'summarize')['script'].replace('/tmp/maven.log', str(log)),
                                 {'SOURCE': directory + '/source', 'MODULE': 'app', 'MAVEN_EXIT_CODE_PATH': directory + '/exit',
                                  'RESULT': directory + '/result'})
                self.assertEqual(result.returncode, 0, result.stderr)
                raw = Path(directory, 'result').read_text()
                self.assertLessEqual(len(termination_message(raw)), 1755, task)
                summary = json.loads(raw)
                self.assertTrue(summary['truncated'])
                self.assertIn('<', ''.join(summary['failingTests'] + summary['compilerErrors']) or '<')

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

    def npm(self, directory, stub, packages=('vite',), npmrc=True, lock=None):
        """Run the update step against a stub npm, with /tmp mapped into the test directory."""
        text = step('npm-lock-update', 'update')['script'].replace('/tmp/', directory + '/tmp/')
        Path(directory, 'tmp').mkdir(exist_ok=True)
        web = Path(directory, 'source/web')
        web.mkdir(parents=True, exist_ok=True)
        web.joinpath('package.json').write_text('{"name":"w"}')
        override = lock
        lock = {'lockfileVersion': 3, 'packages': {
            '': {'name': 'w'},
            'node_modules/vite': {'version': '6.0.0', 'resolved': 'https://registry.npmjs.org/vite/-/vite-6.0.0.tgz'},
            'node_modules/old': {'version': '1.0.0', 'resolved': 'https://registry.npmjs.org/old/-/old-1.0.0.tgz'},
            'node_modules/same': {'version': '1.0.0', 'resolved': 'https://registry.npmjs.org/same/-/same-1.0.0.tgz'}}}
        web.joinpath('package-lock.json').write_text(json.dumps(override or lock, indent=2))
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

    def test_npm_summary_bounds_hosts_and_fits_after_go_encoding(self):
        if not shutil.which('node'):
            self.skipTest('node is not installed')
        with tempfile.TemporaryDirectory() as directory:
            # The stub rewrites the lockfile with 60 packages on long, HTML-heavy
            # hosts and versions.
            stub = ('#!/bin/sh\nnode -e \'const f="package-lock.json",j=JSON.parse(require("fs").readFileSync(f));'
                    'j.packages["node_modules/vite"]={version:"6.0.0-"+"x".repeat(80)+"b",resolved:"https://"+"a".repeat(63)+".new.example/v.tgz"};'
                    'for(let i=0;i<60;i++){j.packages["node_modules/p"+i]={version:"1.0.0-<&>".repeat(20)+i,'
                    'resolved:"https://"+"h".repeat(200)+i+".example/p.tgz"};}'
                    'require("fs").writeFileSync(f,JSON.stringify(j,null,2))\'\n')
            result = self.npm(directory, stub)
            self.assertEqual(result.returncode, 0, result.stderr)
            raw = Path(directory, 'result').read_text()
            self.assertLessEqual(len(termination_message(raw)), 3072, 'four containers keep 3072 bytes each')
            summary = json.loads(raw)
            self.assertTrue(summary['truncated'])
            self.assertLessEqual(len(summary['resolvedHosts']), 10)
            self.assertTrue(all(len(h) <= 64 for h in summary['resolvedHosts'] + summary['newResolvedHosts']))
            self.assertTrue(all(len(c['to'] or '') <= 64 for c in summary['changes']))

    def test_npm_changes_are_compared_before_values_are_bounded(self):
        if not shutil.which('node'):
            self.skipTest('node is not installed')
        long = '1.0.0-' + 'x' * 80
        host = 'a' * 63
        lock = {'lockfileVersion': 3, 'packages': {'': {'name': 'w'}, 'node_modules/p': {
            'version': long + 'a', 'resolved': 'https://' + host + '.old.example/p.tgz', 'integrity': 'sha512-same'}}}
        with tempfile.TemporaryDirectory() as directory:
            stub = ('#!/bin/sh\nnode -e \'const f="package-lock.json",j=JSON.parse(require("fs").readFileSync(f));'
                    'j.packages["node_modules/p"].version=j.packages["node_modules/p"].version.slice(0,-1)+"b";'
                    'j.packages["node_modules/p"].resolved=j.packages["node_modules/p"].resolved.replace(".old.",".new.");'
                    'require("fs").writeFileSync(f,JSON.stringify(j,null,2))\'\n')
            result = self.npm(directory, stub, lock=lock)
            self.assertEqual(result.returncode, 0, result.stderr)
            summary = json.loads(Path(directory, 'result').read_text())
        self.assertEqual(summary['changed'], 1, 'versions that differ after the published bound still count')
        self.assertEqual(len(summary['newResolvedHosts']), 1, 'hosts that share the published prefix still count as new')
        self.assertTrue(all(len(v) <= 64 for c in summary['changes'] for v in (c['from'], c['to'])))

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


    def audit(self, directory, ecosystem, report, code):
        text = step('dependency-audit', 'summarize')['script']
        Path(directory, 'tmp/audit').mkdir(parents=True, exist_ok=True)
        if report is not None:
            Path(directory, 'tmp/audit/report.json').write_text(json.dumps(report))
        if code is not None:
            Path(directory, 'tmp/audit/exit-code').write_text(str(code))
        result = subprocess.run([shutil.which('node'), '-e', text.replace('/tmp/', directory + '/tmp/')],
                                env={'ECOSYSTEM': ecosystem, 'TARGET': 'web', 'RESULT': directory + '/result'},
                                capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(Path(directory, 'result').read_text())

    def test_osv_resolves_natively_through_the_reviewed_registry(self):
        text = step('dependency-audit', 'osv-scanner')['script']
        self.assertIn('--data-source=native', text)
        self.assertIn('--maven-registry="$registry"', text)
        self.assertIn('https://repo.maven.apache.org/maven2', text)

    def test_audit_bounds_oversized_reports_and_reports_coverage(self):
        if not shutil.which('node'):
            self.skipTest('node is not installed')
        text = step('dependency-audit', 'summarize')['script']
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, 'tmp/audit').mkdir(parents=True)
            with open(Path(directory, 'tmp/audit/report.json'), 'wb') as report:
                report.truncate(17 * 1024 * 1024)
            Path(directory, 'tmp/audit/exit-code').write_text('1')
            subprocess.run([shutil.which('node'), '-e', text.replace('/tmp/', directory + '/tmp/')],
                           env={'ECOSYSTEM': 'maven', 'TARGET': 'm', 'RESULT': directory + '/result'}, check=True, timeout=30)
            summary = json.loads(Path(directory, 'result').read_text())
        self.assertEqual((summary['complete'], summary['reason']), (False, 'report larger than 16 MiB'))
        osv = {'results': [{'packages': [{'package': {'name': 'g:a', 'version': '1'}, 'groups': [{'ids': ['X'], 'max_severity': '7'}]}]}]}
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, 'tmp/audit').mkdir(parents=True)
            Path(directory, 'tmp/audit/stderr.txt').write_text('failed resolving org.example:parent:1.0\n')
            summary = self.audit(directory, 'maven', osv, 1)
        self.assertEqual(summary['coverage'], {'resolution': 'native', 'testScope': False, 'resolutionMessages': 1})
        self.assertFalse(summary['complete'], 'a resolution failure leaves the audit incomplete')
        self.assertEqual(summary['reason'], 'dependency resolution reported errors')

    def test_audit_summarizes_npm_and_osv_reports_within_bounds(self):
        if not shutil.which('node'):
            self.skipTest('node is not installed')
        npm = {'metadata': {'dependencies': {'total': 1500}}, 'vulnerabilities': {
            'axios': {'severity': 'high', 'range': '<1.21.1', 'fixAvailable': True,
                      'via': [{'source': 1, 'url': 'https://github.com/advisories/GHSA-aaaa'}]},
            'lodash': {'severity': 'low', 'range': '<4.17.21', 'fixAvailable': False, 'via': ['axios']}}}
        with tempfile.TemporaryDirectory() as directory:
            summary = self.audit(directory, 'npm', npm, 1)
        self.assertEqual((summary['complete'], summary['toolExitCode'], summary['packages'], summary['vulnerable']), (True, 1, 1500, 2))
        self.assertEqual(summary['bySeverity'], {'critical': 0, 'high': 1, 'moderate': 0, 'low': 1, 'unknown': 0})
        self.assertEqual(summary['findings'][0], {'package': 'axios', 'version': '<1.21.1', 'severity': 'high',
                                                  'ids': ['https://github.com/advisories/GHSA-aaaa'], 'fixAvailable': True})
        osv = {'results': [{'source': {'path': '/x/pom.xml'}, 'packages': [
            {'package': {'name': 'g:a%d' % i, 'version': '1.0'},
             'groups': [{'ids': ['GHSA-%d' % i], 'aliases': ['CVE-%d' % i, 'GHSA-%d' % i], 'max_severity': str(i % 10 + 0.5)}]}
            for i in range(60)]}]}
        with tempfile.TemporaryDirectory() as directory:
            summary = self.audit(directory, 'maven', osv, 1)
        self.assertEqual((summary['complete'], summary['packages'], summary['vulnerable'], summary['truncated']), (True, None, 60, True))
        self.assertLessEqual(len(summary['findings']), 20)
        self.assertEqual(summary['findings'][0]['severity'], 'critical')
        self.assertLessEqual(len(json.dumps(json.dumps(summary, separators=(',', ':')))), 1600, 'measured JSON-escaped against the per-container budget')
        with tempfile.TemporaryDirectory() as directory:
            broken = self.audit(directory, 'maven', None, 2)
        self.assertEqual((broken['complete'], broken['toolExitCode'], broken['findings']), (False, 2, []))

    def test_audit_validation_refuses_unsafe_targets_and_registries(self):
        text = step('dependency-audit', 'validate')['script']
        def check(ecosystem, target, registry='', maven=''):
            return subprocess.run(['sh', '-c', text], env={'ECOSYSTEM': ecosystem, 'TARGET': target, 'REGISTRY_URL': registry,
                                                           'MAVEN_REGISTRY_URL': maven, 'PATH': '/usr/bin:/bin'},
                                  capture_output=True, timeout=10).returncode
        for case in (('pip', 'web'), ('npm', '.'), ('npm', '../x'), ('maven', 'a/../b'), ('npm', '/abs'),
                     ('npm', 'evil\n;rm -rf'), ('maven', 'rest-villains\n../x'),
                     ('npm', 'web', 'https://evil.example/'), ('npm', 'web', 'http://10.96.0.12:8081/\nx'),
                     ('maven', 'm', '', 'https://deps.dev/maven2'), ('maven', 'm', '', 'http://evil:8080/maven2')):
            self.assertEqual(check(*case), 2, case)
        for case in (('maven', 'rest-villains'), ('npm', 'web', 'https://registry.npmjs.org/'),
                     ('npm', 'web', 'http://10.96.0.12:8081/'), ('maven', 'm', '', 'http://10.96.0.12:8080/maven2')):
            self.assertEqual(check(*case), 0, case)


class RunToolTests(unittest.TestCase):
    def test_build_test_run_is_read_only_and_can_be_sandboxed(self):
        pipeline = by_kind('Pipeline')['ai-sdlc-maven-build-test']['spec']
        run = runner.maven_build_test('os-santiago/quarkus-super-heroes', SHA, 'rest-villains', ['postgres'], ['-Dtest=!UIResourceTests'],
                                      runtime_class='ai-sdlc-offline')
        spec = run['spec']
        self.assertEqual(spec['pipelineRef'], {'name': 'ai-sdlc-maven-build-test'})
        self.assertNotIn('pipelineSpec', spec)
        self.assertEqual({p['name'] for p in spec['params']}, {p['name'] for p in pipeline['params']})
        self.assertEqual({w['name'] for w in spec['workspaces']}, {w['name'] for w in pipeline['workspaces'] if not w.get('optional')})
        cache = spec['workspaces'][1]
        self.assertEqual(cache, {'name': 'maven-cache', 'persistentVolumeClaim': {
            'claimName': tekton.cache_claim('os-santiago/quarkus-super-heroes'), 'readOnly': True}})
        template = spec['taskRunTemplate']
        self.assertEqual(template['serviceAccountName'], 'ai-sdlc-ci-runner')
        self.assertIs(template['podTemplate']['automountServiceAccountToken'], False)
        self.assertEqual(spec['taskRunSpecs'], [{'pipelineTaskName': 'build-test', 'podTemplate': dict(
            template['podTemplate'], runtimeClassName='ai-sdlc-offline', priorityClassName='ai-sdlc-ci-candidate')}])
        for name, task in runner.SANDBOXED_TASKS.items():
            self.assertIn(task, {t['name'] for t in by_kind('Pipeline')[name]['spec']['tasks']})
        self.assertNotIn('taskRunSpecs', runner.maven_build_test('a/b', SHA, '.', ['none'], [])['spec'], 'sandbox is opt-in')
        self.assertNotIn('taskRunSpecs', runner.maven_dependencies('a/b', SHA, '.', ['none'], [])['spec'],
                         'the offline gVisor class would cut the fetch task off')
        self.assertEqual(run['metadata']['labels']['ai-sdlc.io/repository'], 'os-santiago.quarkus-super-heroes')

    def test_audit_run_binds_its_pipeline_inputs_and_passes_when_complete(self):
        run = runner.dependency_audit('a/b', SHA, 'maven', 'rest-villains')
        pipeline = by_kind('Pipeline')['ai-sdlc-dependency-audit']['spec']
        self.assertEqual({p['name'] for p in run['spec']['params']}, {p['name'] for p in pipeline['params']})
        self.assertNotIn('taskRunSpecs', run['spec'])
        for bad in (('pip', 'x', ''), ('npm', '.', ''), ('maven', 'x', 'https://r.example/'), ('npm', 'x', 'https://evil.example/')):
            with self.assertRaises(ValueError):
                runner.dependency_audit('a/b', SHA, *bad)
        with self.assertRaises(ValueError):
            runner.dependency_audit('a/b', SHA, 'maven', 'x', '', 'https://deps.dev/maven2')
        base = ['dependency-audit', '--project', str(PROJECT), '--revision', SHA]
        with self.assertRaisesRegex(ValueError, 'Maven module'):
            runner.main(base + ['--ecosystem', 'maven', '--directory', 'ui-super-heroes/src/main/webui'])
        with self.assertRaisesRegex(ValueError, 'npm directory'):
            runner.main(base + ['--ecosystem', 'npm', '--directory', 'rest-villains'])
        # Without a catalog the target is only validated, never looked up.
        args = runner.argparse.Namespace(pipeline='dependency-audit', repository='a/b', revision=SHA, ecosystem='maven',
                                         directory='m', registry_url='', maven_mirror_url='', sandbox=False, runtime_class='ai-sdlc-offline')
        self.assertEqual(runner.render_from_args(args, None)['spec']['pipelineRef'], {'name': 'ai-sdlc-dependency-audit'})
        observed = {'succeeded': True, 'commit': SHA, 'summary': {'complete': True, 'vulnerable': 3}}
        self.assertTrue(runner.passed(observed, SHA), 'findings are output, not failure')
        self.assertFalse(runner.passed(dict(observed, summary={'complete': False}), SHA))

    def test_dependencies_and_npm_runs_bind_their_pipeline_inputs(self):
        deps = runner.maven_dependencies('a/b', SHA, 'm', ['postgres'], [], mirror_url='http://dependency-proxy.ai-sdlc-dependencies.svc/maven2')
        self.assertEqual(deps['spec']['workspaces'][1], {'name': 'maven-cache', 'persistentVolumeClaim': {'claimName': tekton.cache_claim('a/b')}})
        npm = runner.npm_lock_update('a/b', SHA, 'ui/web', 'update', ['vite', '@scope/pkg'])
        for run, name in ((deps, 'ai-sdlc-maven-dependencies'), (npm, 'ai-sdlc-npm-lock-update')):
            pipeline = by_kind('Pipeline')[name]['spec']
            self.assertEqual({p['name'] for p in run['spec']['params']}, {p['name'] for p in pipeline['params']})
            self.assertEqual({w['name'] for w in run['spec']['workspaces']}, {w['name'] for w in pipeline['workspaces'] if not w.get('optional')})
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

    def test_sandboxed_runs_require_the_raised_task_limit(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory, 'ai-sdlc/c')
            state.mkdir(parents=True)
            state.joinpath('ownership.json').write_text('{"provider": "podman"}')
            with patch.dict('os.environ', {'XDG_STATE_HOME': directory}):
                for shown, ok in (('1024', True), ('infinity', True), ('629145', True), ('307', False), ('', False)):
                    run = Mock(return_value=Mock(stdout=shown + '\n'))
                    if ok:
                        runner.sandbox_ready('c', 1024, run=run)
                    else:
                        with self.assertRaisesRegex(ValueError, 're-run ./install.sh sandbox'):
                            runner.sandbox_ready('c', 1024, run=run)
                self.assertEqual(run.call_args.args[0][:3], ['podman', 'exec', 'c-control-plane'])

    def test_every_ready_node_must_have_the_raised_task_limit(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory, 'ai-sdlc/c')
            state.mkdir(parents=True)
            state.joinpath('ownership.json').write_text('{"provider": "podman"}')
            with patch.dict('os.environ', {'XDG_STATE_HOME': directory}):
                def run(args, **_kwargs):
                    if args[:2] == ['kubectl', 'get']:
                        return Mock(stdout='node/c-worker\nnode/c-worker2\n')
                    return Mock(stdout=shown[args[2]] + '\n')
                shown = {'c-worker': '3072', 'c-worker2': '3072'}
                runner.sandbox_ready('c', 768, run=run, kubectl=['kubectl'])
                shown['c-worker2'] = '307'
                with self.assertRaisesRegex(ValueError, 'c-worker2 still caps'):
                    runner.sandbox_ready('c', 768, run=run, kubectl=['kubectl'])
                foreign = Mock(return_value=Mock(stdout='node/other-worker\n'))
                with self.assertRaisesRegex(ValueError, 'not a node of this kind cluster'):
                    runner.sandbox_ready('c', 768, run=foreign, kubectl=['kubectl'])
                # An install from before the label: the single node.
                old = Mock(return_value=Mock(stdout='3072\n'))
                old.side_effect = lambda args, **_k: Mock(stdout='' if args[0] == 'kubectl' else '3072\n')
                runner.sandbox_ready('c', 768, run=old, kubectl=['kubectl'])
                self.assertEqual(old.call_args.args[0][:3], ['podman', 'exec', 'c-control-plane'])

    def test_admission_requires_the_sandbox_for_build_test(self):
        rules = ' '.join(v['expression'] for v in by_kind('ValidatingAdmissionPolicy')['ai-sdlc-ci-pipelineruns']['spec']['validations'])
        self.assertIn("s.pipelineTaskName == 'build-test'", rules)
        self.assertIn("s.podTemplate.runtimeClassName == 'ai-sdlc-offline'", rules)

    def test_a_run_passes_only_with_success_the_requested_commit_and_exit_code_zero(self):
        ok = {'succeeded': True, 'commit': SHA, 'summary': {'mavenExitCode': 0}}
        self.assertTrue(runner.passed(ok, SHA))
        self.assertTrue(runner.passed(dict(ok, summary={'npmExitCode': 0}), SHA))
        for bad in (dict(ok, succeeded=False), dict(ok, commit='0' * 40), dict(ok, commit=None),
                    dict(ok, summary={'mavenExitCode': 1}), dict(ok, summary=None), dict(ok, summary={'mavenExitCode': '0'})):
            self.assertFalse(runner.passed(bad, SHA), bad)


if __name__ == '__main__':
    unittest.main()
