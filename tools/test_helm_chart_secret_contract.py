"""The documented Helm chart Secret contract matches the templates.

Two independent checks: the rendered chart in each covered capability
combination, and a scan of the template source for every Secret reference.
"""
import copy
import json
from pathlib import Path
import re
import subprocess
import unittest

import yaml

import install
# Module imports, so unittest does not collect those test classes again here.
import test_helm
import test_release_helm
from test_helm import CHART, HELM

ROOT = Path(__file__).resolve().parents[1]
CONTRACT = ROOT / 'docs/operations/helm-chart-secret-contract.md'
UPSTREAM_SECRET = 'fixture-model-upstream'
WORKLOADS = ('Deployment', 'StatefulSet', 'Job', 'CronJob')

# Every templated Secret name expression in the chart source and the contract
# row it renders. A literal name must appear in the contract table itself.
TEMPLATED_NAMES = {
    '{{ printf "%s-%s" $.Values.authority.databaseSecretPrefix $component.name }}': 'ai-sdlc-database-{role}',
    '{{ printf "%s-%s" $.Values.authority.identitySecretPrefix $component.name }}': 'ai-sdlc-identity-{role}',
    '{{ printf "%s-verifier" .Values.authority.databaseSecretPrefix }}': 'ai-sdlc-database-{role}',
    '{{ printf "%s-verifier" .Values.authority.identitySecretPrefix }}': 'ai-sdlc-identity-{role}',
    '{{ .Values.authority.databaseSecretPrefix }}-build-controller': 'ai-sdlc-database-{role}',
    '{{ .Values.authority.identitySecretPrefix }}-build-controller': 'ai-sdlc-identity-{role}',
    '{{ .Values.authority.databaseSecretPrefix }}-deployer': 'ai-sdlc-database-{role}',
    '{{ .Values.authority.identitySecretPrefix }}-deployer': 'ai-sdlc-identity-{role}',
    '{{ .Values.authority.databaseSecretPrefix }}-verifier': 'ai-sdlc-database-{role}',
    '{{ .Values.authority.identitySecretPrefix }}-verifier': 'ai-sdlc-identity-{role}',
    '{{ include "ai-sdlc-k8s-runtime.databaseSecretName" . }}': 'ai-sdlc-postgres-app',
    '{{ .Values.migrator.credentialsSecret }}': 'ai-sdlc-database-roles',
    '{{ $.Values.intake.credentialSecret }}': 'ai-sdlc-github-client',
    '{{ $.Values.modelGateway.upstream.credentialSecret | quote }}': '{modelGateway.upstream.credentialSecret}',
    '{{ .Values.provenance.registryCredentialSecret }}': 'ai-sdlc-provenance-registry',
    '{{ .Values.console.databaseSecret }}': 'ai-sdlc-flow-console-database',
}


def contract_table(marker):
    text = CONTRACT.read_text()
    match = re.search(r'<!-- %s:begin -->\n(.*?)<!-- %s:end -->' % (marker, marker), text, re.S)
    if match is None:
        raise AssertionError('missing contract table ' + marker)
    rows = []
    for line in match.group(1).splitlines()[2:]:
        cells = [cell.strip() for cell in line.strip().strip('|').split('|')]
        rows.append(cells)
    return rows


def mounted_contract():
    """Secret name pattern -> (keys the chart or runtime reads, keys only the installer keeps).

    The Keys cell lists the read keys, then optionally "; installer also keeps"
    and the keys the installer stores for its own consistency checks."""
    result = {}
    for cells in contract_table('secret-contract'):
        name = re.match(r'`([^`]+)`', cells[0]).group(1)
        chart, _, kept = cells[1].partition(';')
        keys = re.findall(r'`([^`]+)`', chart)
        if name in result or not keys or len(cells) != 6:
            raise AssertionError('malformed contract row: ' + cells[0])
        result[name] = (set(keys), set(re.findall(r'`([^`]+)`', kept)))
    return result


def api_contract():
    return {(re.match(r'`([^`]+)`', cells[0]).group(1), re.match(r'`([^`]+)`', cells[1]).group(1))
            for cells in contract_table('secret-api-contract')}


def pattern(name):
    roles = '|'.join(re.escape(role) for role in install.ALL_ROLES)
    expression = re.escape(name).replace(re.escape('{role}'), '(%s)' % roles)
    expression = expression.replace(re.escape('{modelGateway.upstream.credentialSecret}'), re.escape(UPSTREAM_SECRET))
    return re.compile(expression + r'\Z')


def secret_references(docs):
    """(secret name, key or None for a whole-Secret mount) for every workload."""
    configmaps = {d['metadata']['name']: d.get('data') or {} for d in docs if d['kind'] == 'ConfigMap'}
    found = set()
    for doc in docs:
        if doc['kind'] not in WORKLOADS:
            continue
        workload = doc['spec']['jobTemplate']['spec'] if doc['kind'] == 'CronJob' else doc['spec']
        pod = workload['template']['spec']
        secret_volumes = {}
        for volume in pod.get('volumes', []):
            if 'secret' in volume:
                secret = volume['secret']
                secret_volumes[volume['name']] = secret['secretName']
                items = secret.get('items') or []
                found.update((secret['secretName'], item['key']) for item in items)
                if not items:
                    found.add((secret['secretName'], None))
            for source in (volume.get('projected') or {}).get('sources', []):
                if 'secret' in source:
                    raise AssertionError('projected Secret sources are not covered by the contract test')
        for container in pod.get('containers', []) + pod.get('initContainers', []):
            values = []
            for env in container.get('env', []):
                ref = (env.get('valueFrom') or {}).get('secretKeyRef')
                if ref:
                    found.add((ref['name'], ref['key']))
                elif isinstance(env.get('value'), str):
                    values.append(env['value'])
            for source in container.get('envFrom', []):
                if 'secretRef' in source:
                    raise AssertionError('envFrom Secret references are not covered by the contract test')
                if 'configMapRef' in source:
                    values.extend(v for v in configmaps.get(source['configMapRef']['name'], {}).values() if isinstance(v, str))
            # A file path the container reads from a whole-Secret mount names a key.
            for mount in container.get('volumeMounts', []):
                if mount['name'] in secret_volumes:
                    prefix = mount['mountPath'].rstrip('/') + '/'
                    for value in values:
                        if value.startswith(prefix) and '/' not in value[len(prefix):]:
                            found.add((secret_volumes[mount['name']], value[len(prefix):]))
    return found


BUILT_IN_SECRET_ROLES = {'edit', 'admin', 'cluster-admin'}


def api_references(docs):
    """Secrets read through RBAC. Wildcard resources or API groups, and bindings
    to built-in roles that read Secrets, would hide access from the contract."""
    found = set()
    for doc in docs:
        if doc['kind'] in ('RoleBinding', 'ClusterRoleBinding'):
            role = doc['roleRef']
            if role['kind'] == 'ClusterRole' and role['name'] in BUILT_IN_SECRET_ROLES:
                raise AssertionError('binding to a built-in role that reads Secrets: ' + doc['metadata']['name'])
            continue
        if doc['kind'] not in ('Role', 'ClusterRole'):
            continue
        for rule in doc.get('rules') or []:
            if '*' in rule.get('resources', []) or ('*' in rule.get('apiGroups', []) and rule.get('resources')):
                raise AssertionError('wildcard RBAC rule could read Secrets: ' + doc['metadata']['name'])
            if 'secrets' in rule.get('resources', []):
                names = rule.get('resourceNames')
                if not names or doc['kind'] != 'Role':
                    raise AssertionError('Secret access must be a namespaced rule with resourceNames: ' + doc['metadata']['name'])
                found.update((name, doc['metadata']['namespace']) for name in names)
    return found


class SecretContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        chart = test_helm.ChartTests()
        maximal = test_release_helm.ReleaseChartTests().values()
        upstream = copy.deepcopy(maximal)
        upstream['modelGateway'] = {'adapter': 'openai', 'upstream': {
            'url': 'http://10.0.0.5:8080/v1', 'model': 'fixture', 'cidr': '10.0.0.5/32', 'port': 8080,
            'credentialSecret': UPSTREAM_SECRET}}
        # The optional repository-scoped source read credential (#741).
        source = copy.deepcopy(maximal)
        source['verifier']['sourceCredential'] = True
        cls.renderings = {}
        backup = dict(copy.deepcopy(maximal), backup={'enabled': True})
        console = dict(copy.deepcopy(maximal), console={'enabled': True})
        for name, values in [('maintenance', {}), ('shadow', {'runtime': {'mode': 'shadow'}}),
                             ('maximal', maximal), ('upstream', upstream), ('source-credential', source),
                             ('backup', backup), ('console', console)]:
            result = chart.render_build(values)
            if result.returncode != 0:
                raise AssertionError(name + ': ' + result.stderr)
            cls.renderings[name] = [d for d in yaml.safe_load_all(result.stdout) if d]
        # The chart's own defaults: CloudNativePG generates the database Secret.
        result = subprocess.run([HELM, 'template', 'ai-sdlc', str(CHART), '--namespace', 'ai-sdlc-runtime'],
                                capture_output=True, text=True)
        if result.returncode != 0:
            raise AssertionError('chart defaults: ' + result.stderr)
        cls.renderings['chart-defaults'] = [d for d in yaml.safe_load_all(result.stdout) if d]

    def test_every_rendered_secret_reference_is_documented_with_its_key(self):
        contract = mounted_contract()
        matchers = {name: pattern(name) for name in contract}
        used = set()
        for rendering, docs in self.renderings.items():
            for secret, key in secret_references(docs):
                rows = [name for name, matcher in matchers.items() if matcher.match(secret)]
                self.assertEqual(len(rows), 1, '%s: Secret %s matches %s contract rows' % (rendering, secret, rows))
                used.add(rows[0])
                if key is not None:
                    self.assertIn(key, contract[rows[0]][0], '%s: undocumented key %s of %s' % (rendering, key, secret))
        self.assertEqual(used, set(contract), 'documented Secrets that no rendering uses')

    def test_source_credential_is_read_only_by_the_verifier_controller(self):
        """The read credential is rendered only with its setting, and only into
        the verifier Deployment, never a pod the verifier creates."""
        for rendering, docs in self.renderings.items():
            holders = {doc['metadata']['name'] for doc in docs if doc['kind'] in WORKLOADS
                       and any(secret == 'ai-sdlc-source-reader' for secret, _ in secret_references([doc]))}
            self.assertEqual(holders, {'ai-sdlc-verifier'} if rendering == 'source-credential' else set(), rendering)

    def test_role_secrets_cover_exactly_the_installer_identities(self):
        names = {secret for docs in self.renderings.values() for secret, _ in secret_references(docs)}
        for prefix in ('ai-sdlc-database-', 'ai-sdlc-identity-'):
            roles = {name[len(prefix):] for name in names if name.startswith(prefix) and name != 'ai-sdlc-database-roles'}
            self.assertEqual(roles, set(install.ALL_ROLES), prefix)

    def test_rendered_keys_are_the_documented_keys_of_each_secret(self):
        """A key documented for a Secret is either rendered or read from a whole mount."""
        contract = mounted_contract()
        observed = {}
        whole = set()
        for docs in self.renderings.values():
            for secret, key in secret_references(docs):
                row = next(name for name in contract if pattern(name).match(secret))
                if key is None:
                    whole.add(row)
                else:
                    observed.setdefault(row, set()).add(key)
        for row, (keys, _) in contract.items():
            if row not in whole:
                self.assertEqual(observed.get(row, set()), keys, row)

    def test_secret_api_access_is_documented(self):
        found = set()
        for docs in self.renderings.values():
            found |= api_references(docs)
        self.assertEqual(found, api_contract())

    def test_rbac_scan_refuses_wildcards_and_built_in_secret_roles(self):
        def role(rules):
            return [{'kind': 'Role', 'metadata': {'name': 'r', 'namespace': 'n'}, 'rules': rules}]
        for rules in ([{'apiGroups': [''], 'resources': ['*'], 'verbs': ['get']}],
                      [{'apiGroups': ['*'], 'resources': ['secrets'], 'resourceNames': ['x'], 'verbs': ['get']}],
                      [{'apiGroups': [''], 'resources': ['secrets'], 'verbs': ['get']}]):
            with self.assertRaises(AssertionError):
                api_references(role(rules))
        for name in BUILT_IN_SECRET_ROLES:
            binding = {'kind': 'RoleBinding', 'metadata': {'name': 'b', 'namespace': 'n'},
                       'roleRef': {'kind': 'ClusterRole', 'name': name}}
            with self.assertRaises(AssertionError):
                api_references([binding])
        self.assertEqual(api_references(role([{'apiGroups': [''], 'resources': ['secrets'],
                                               'resourceNames': ['x'], 'verbs': ['get']}])), {('x', 'n')})

    def test_template_source_secret_references_are_documented(self):
        contract = mounted_contract()
        name = r'((?:\{\{.*?\}\}|[^,}\s{])+)'
        reference = re.compile(r'secretKeyRef:\s*\{name:\s*' + name + r',\s*key:|secretName:\s*' + name)
        seen = set()
        for path in sorted((CHART / 'templates').glob('*.yaml')):
            for match in reference.finditer(path.read_text()):
                expression = (match.group(1) or match.group(2)).strip()
                seen.add(expression)
                row = TEMPLATED_NAMES.get(expression, expression)
                self.assertIn(row, contract, '%s: Secret reference %r is not in the contract' % (path.name, expression))
        self.assertEqual(seen & set(TEMPLATED_NAMES), set(TEMPLATED_NAMES), 'stale templated name mapping')

    def test_contract_page_contains_no_secret_values(self):
        text = CONTRACT.read_text()
        self.assertNotRegex(text, r'gh[pousr]_[A-Za-z0-9]{20,}')
        self.assertNotRegex(text, r'-----BEGIN [A-Z ]*PRIVATE KEY-----')
        self.assertNotRegex(text, r'postgresql://[^:\s]+:(?!\$\{?password|<password>)[A-Za-z0-9_-]{16,}@')


if __name__ == '__main__':
    unittest.main()
