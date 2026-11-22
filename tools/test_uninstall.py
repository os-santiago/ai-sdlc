import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import install
import uninstall

UID = 'b0b0b0b0-1111-2222-3333-444444444444'
NAMESPACE = install.NAMESPACE
HELM = {'app.kubernetes.io/instance': 'ai-sdlc', 'app.kubernetes.io/managed-by': 'Helm',
        'app.kubernetes.io/part-of': 'ai-sdlc'}


def resource(kind, name, namespace='', labels=None, annotations=None, uid=None,
             data=None, spec=None):
    item = {'apiVersion': 'v1', 'kind': kind,
            'metadata': {'name': name, 'namespace': namespace,
                         'labels': labels or {}, 'annotations': annotations or {},
                         'uid': uid or name + '-uid'}}
    if data is not None:
        item['data'] = data
    if spec is not None:
        item['spec'] = spec
    return item


MANIFEST = """---
# Source: ai-sdlc-k8s-runtime/templates/namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: ai-sdlc-runtime
---
# Source: ai-sdlc-k8s-runtime/templates/configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ai-sdlc-config
  namespace: ai-sdlc-runtime
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ai-sdlc-authority
  namespace: ai-sdlc-runtime
  annotations:
    ai-sdlc.io/cluster-uid: "ignored; live annotation binds"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ai-sdlc-worker
  namespace: ai-sdlc-runtime
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: ai-sdlc-postgres
  namespace: ai-sdlc-runtime
---
apiVersion: batch/v1
kind: Job
metadata:
  name: ai-sdlc-migrate-1
  namespace: ai-sdlc-runtime
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ai-sdlc-deployer
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ai-sdlc-deployer
"""


class Api:
    """A bounded kubectl/helm double: every call is recorded, deletes are explicit."""

    def __init__(self, uid=UID, objects=None, manifest=MANIFEST):
        self.uid = uid
        self.objects = dict(objects or {})
        self.manifest = manifest
        self.deleted = []
        self.calls = []

    def __call__(self, args, **kwargs):
        args = [str(a) for a in args]
        self.calls.append(args)
        if args[0] == 'helm':
            if self.manifest is None:
                return subprocess.CompletedProcess(args, 1, '', 'Error: release: not found')
            return subprocess.CompletedProcess(args, 0, self.manifest, '')
        rest = args[1:]
        while rest and rest[0].startswith('--'):
            flag = rest.pop(0)
            if '=' not in flag:
                rest.pop(0)
        operation, rest = rest[0], rest[1:]
        if operation == 'get':
            return self.get(args, rest)
        if operation == 'delete':
            return self.delete(args, rest)
        raise AssertionError('unexpected invocation: ' + ' '.join(args))

    def parse(self, rest):
        name = rest[1] if len(rest) > 1 and not rest[1].startswith('-') else None
        namespace, selector, index = '', {}, 1 if name is None else 2
        while index < len(rest):
            flag = rest[index]
            if flag in ('-n', '--namespace'):
                namespace = rest[index + 1]
                index += 2
            elif flag == '-l':
                for pair in rest[index + 1].split(','):
                    key, _, value = pair.partition('=')
                    selector[key] = value
                index += 2
            elif flag == '-o':
                index += 2
            else:
                index += 1
        return rest[0], name, namespace, selector

    def get(self, args, rest):
        kind, name, namespace, selector = self.parse(rest)
        kind = {'configmap': 'configmaps', 'namespace': 'namespaces',
                'secret': 'secrets'}.get(kind, kind)
        if kind == 'namespaces' and name == 'kube-system':
            return subprocess.CompletedProcess(args, 0, self.uid, '')
        if name is not None:
            item = self.objects.get((kind, name, namespace))
            out = json.dumps(item) if item is not None else ''
            return subprocess.CompletedProcess(args, 0, out, '')
        items = [item for (k, _, ns), item in self.objects.items()
                 if k == kind and ns == namespace
                 and all(item.get('metadata', {}).get('labels', {}).get(key) == value
                         for key, value in selector.items())]
        return subprocess.CompletedProcess(args, 0, json.dumps({'kind': 'List', 'items': items}), '')

    def delete(self, args, rest):
        kind, name, namespace, _ = self.parse(rest)
        self.deleted.append((kind, name, namespace))
        self.objects.pop((kind, name, namespace), None)
        return subprocess.CompletedProcess(args, 0, '', '')


def installed_objects():
    """A frozen maintenance install plus unrelated sentinel resources."""
    objects = {
        ('namespaces', NAMESPACE, ''):
            resource('Namespace', NAMESPACE, labels={'app.kubernetes.io/part-of': 'ai-sdlc'}),
        ('configmaps', 'ai-sdlc-config', NAMESPACE):
            resource('ConfigMap', 'ai-sdlc-config', NAMESPACE, labels=dict(HELM),
                     data={'AI_SDLC_RUNTIME_MODE': 'maintenance'}),
        ('configmaps', 'ai-sdlc-authority', NAMESPACE):
            resource('ConfigMap', 'ai-sdlc-authority', NAMESPACE, labels=dict(HELM),
                     annotations={uninstall.UID_ANNOTATION: UID},
                     data={'policy.json': json.dumps({'freeze': True})}),
        ('deployments', 'ai-sdlc-worker', NAMESPACE):
            resource('Deployment', 'ai-sdlc-worker', NAMESPACE, labels=dict(HELM)),
        ('statefulsets', 'ai-sdlc-postgres', NAMESPACE):
            resource('StatefulSet', 'ai-sdlc-postgres', NAMESPACE, labels=dict(HELM),
                     spec={'replicas': 1,
                           'volumeClaimTemplates': [{'metadata': {'name': 'data'}}]}),
        ('jobs', 'ai-sdlc-migrate-1', NAMESPACE):
            resource('Job', 'ai-sdlc-migrate-1', NAMESPACE, labels=dict(HELM)),
        ('persistentvolumeclaims', 'data-ai-sdlc-postgres-0', NAMESPACE):
            resource('PersistentVolumeClaim', 'data-ai-sdlc-postgres-0', NAMESPACE),
        ('secrets', 'ai-sdlc-database-intake', NAMESPACE):
            resource('Secret', 'ai-sdlc-database-intake', NAMESPACE,
                     annotations={uninstall.UID_ANNOTATION: UID}),
        ('secrets', 'ai-sdlc-postgres-app', NAMESPACE):
            resource('Secret', 'ai-sdlc-postgres-app', NAMESPACE,
                     labels={'app.kubernetes.io/part-of': 'ai-sdlc'}),
        ('clusterroles', 'ai-sdlc-deployer', ''):
            resource('ClusterRole', 'ai-sdlc-deployer', labels=dict(HELM)),
        ('clusterrolebindings', 'ai-sdlc-deployer', ''):
            resource('ClusterRoleBinding', 'ai-sdlc-deployer', labels=dict(HELM)),
        ('runtimeclasses', 'ai-sdlc-offline', ''):
            resource('RuntimeClass', 'ai-sdlc-offline',
                     annotations={uninstall.UID_ANNOTATION: UID}),
        ('runtimeclasses', 'unrelated-class', ''):
            resource('RuntimeClass', 'unrelated-class'),
        ('deployments', 'sentinel-worker', NAMESPACE):
            resource('Deployment', 'sentinel-worker', NAMESPACE),
        ('secrets', 'sentinel-secret', NAMESPACE):
            resource('Secret', 'sentinel-secret', NAMESPACE),
        ('persistentvolumeclaims', 'sentinel-data', NAMESPACE):
            resource('PersistentVolumeClaim', 'sentinel-data', NAMESPACE),
    }
    return objects


class UninstallTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.state = self.root / 'ai-sdlc' / 'test-cluster'
        self.state.mkdir(parents=True)
        (self.state / 'kubeconfig').write_text('apiVersion: v1\n')
        self.ownership({'cluster': 'test-cluster', 'provider': 'docker', 'clusterUID': UID})

    def ownership(self, record, name='ownership.json'):
        (self.state / name).write_text(json.dumps(record) + '\n')

    def invoke(self, args, api):
        env = patch.dict(os.environ, {'XDG_STATE_HOME': str(self.root)})
        binaries = {'kubectl': 'kubectl', 'helm': 'helm', 'kind': 'kind'}
        output = io.StringIO()
        with env, patch.object(install, 'tools', return_value=binaries), \
                patch.object(install, 'run', side_effect=api), \
                contextlib.redirect_stdout(output):
            result = uninstall.main(['--cluster', 'test-cluster'] + args)
        self.assertEqual(result, 0)
        return json.loads(output.getvalue())

    def test_plan_is_read_only_and_selects_only_owned_resources(self):
        api = Api(objects=installed_objects())
        result = self.invoke([], api)
        removed = {(item['resource'], item['name']) for item in result['remove']}
        expected = {('deployments', 'ai-sdlc-worker'), ('statefulsets', 'ai-sdlc-postgres'),
                    ('jobs', 'ai-sdlc-migrate-1'), ('configmaps', 'ai-sdlc-config'),
                    ('clusterroles', 'ai-sdlc-deployer'), ('clusterrolebindings', 'ai-sdlc-deployer'),
                    ('runtimeclasses', 'ai-sdlc-offline')}
        self.assertEqual(removed, expected)
        self.assertEqual(result['purge'], [])
        retained = {(item['kind'], item['name']) for item in result['retained']}
        self.assertIn(('PersistentVolumeClaim', 'data-ai-sdlc-postgres-0'), retained)
        self.assertIn(('Secret', 'ai-sdlc-database-intake'), retained)
        self.assertIn(('Secret', 'ai-sdlc-postgres-app'), retained)
        self.assertIn(('ConfigMap', 'ai-sdlc-authority'), retained)
        self.assertIn(('Namespace', NAMESPACE), retained)
        untouched = {(item['kind'], item['name']) for item in result['untouched']}
        self.assertIn(('Deployment', 'sentinel-worker'), untouched)
        self.assertIn(('PersistentVolumeClaim', 'sentinel-data'), untouched)
        self.assertIn(('RuntimeClass', 'unrelated-class'), untouched)
        self.assertTrue(result['intakeStopped'])
        self.assertFalse(result['applied'])
        self.assertEqual(api.deleted, [])

    def test_auxiliary_namespace_deletion_requires_an_empty_scan(self):
        manifest = MANIFEST + """---
apiVersion: v1
kind: Namespace
metadata:
  name: ai-sdlc-executors
"""
        objects = installed_objects()
        objects[('namespaces', 'ai-sdlc-executors', '')] = resource(
            'Namespace', 'ai-sdlc-executors', labels=dict(HELM))
        objects[('secrets', 'leftover', 'ai-sdlc-executors')] = resource(
            'Secret', 'leftover', 'ai-sdlc-executors')
        result = self.invoke([], Api(objects=objects, manifest=manifest))
        retained = {(item['kind'], item['name']) for item in result['retained']}
        self.assertIn(('Namespace', 'ai-sdlc-executors'), retained)
        self.assertNotIn(('namespaces', 'ai-sdlc-executors'),
                         {(item['resource'], item['name']) for item in result['remove']})

        del objects[('secrets', 'leftover', 'ai-sdlc-executors')]
        result = self.invoke([], Api(objects=objects, manifest=manifest))
        self.assertIn(('namespaces', 'ai-sdlc-executors'),
                      {(item['resource'], item['name']) for item in result['remove']})

    def test_refuses_without_ownership_record(self):
        (self.state / 'ownership.json').unlink()
        api = Api(objects=installed_objects())
        env = patch.dict(os.environ, {'XDG_STATE_HOME': str(self.root)})
        binaries = {'kubectl': 'kubectl', 'helm': 'helm', 'kind': 'kind'}
        with env, patch.object(install, 'tools', return_value=binaries), \
                patch.object(install, 'run', side_effect=api):
            with self.assertRaisesRegex(ValueError, 'ownership record'):
                uninstall.main(['--cluster', 'test-cluster'])
        self.assertEqual(api.deleted, [])

    def test_refuses_on_cluster_identity_or_provider_mismatch(self):
        api = Api(uid='different-uid', objects=installed_objects())
        env = patch.dict(os.environ, {'XDG_STATE_HOME': str(self.root)})
        binaries = {'kubectl': 'kubectl', 'helm': 'helm', 'kind': 'kind'}
        with env, patch.object(install, 'tools', return_value=binaries), \
                patch.object(install, 'run', side_effect=api):
            with self.assertRaisesRegex(ValueError, 'identity'):
                uninstall.main(['--cluster', 'test-cluster'])
        api.uid = UID
        with env, patch.object(install, 'tools', return_value=binaries), \
                patch.object(install, 'run', side_effect=api):
            with self.assertRaisesRegex(ValueError, 'provider'):
                uninstall.main(['--cluster', 'test-cluster', '--provider', 'podman'])
        self.assertEqual(api.deleted, [])

    def test_refuses_a_state_dir_owned_by_another_platform(self):
        self.ownership({'version': 1, 'cluster': 'test-cluster', 'provider': 'docker',
                        'clusterUID': UID, 'scope': 'isolated-konflux'}, 'konflux-ownership.json')
        api = Api(objects=installed_objects())
        env = patch.dict(os.environ, {'XDG_STATE_HOME': str(self.root)})
        binaries = {'kubectl': 'kubectl', 'helm': 'helm', 'kind': 'kind'}
        with env, patch.object(install, 'tools', return_value=binaries), \
                patch.object(install, 'run', side_effect=api):
            with self.assertRaisesRegex(ValueError, 'different platform'):
                uninstall.main(['--cluster', 'test-cluster'])
        self.assertEqual(api.deleted, [])

    def test_apply_deletes_only_owned_resources_and_journals(self):
        api = Api(objects=installed_objects())
        result = self.invoke(['--apply'], api)
        self.assertTrue(result['applied'])
        deleted = {(kind, name) for kind, name, _ in api.deleted}
        expected = {('deployments', 'ai-sdlc-worker'), ('statefulsets', 'ai-sdlc-postgres'),
                    ('jobs', 'ai-sdlc-migrate-1'), ('configmaps', 'ai-sdlc-config'),
                    ('clusterroles', 'ai-sdlc-deployer'), ('clusterrolebindings', 'ai-sdlc-deployer'),
                    ('runtimeclasses', 'ai-sdlc-offline')}
        self.assertEqual(deleted, expected)
        for key in [('persistentvolumeclaims', 'data-ai-sdlc-postgres-0', NAMESPACE),
                    ('persistentvolumeclaims', 'sentinel-data', NAMESPACE),
                    ('secrets', 'ai-sdlc-database-intake', NAMESPACE),
                    ('secrets', 'ai-sdlc-postgres-app', NAMESPACE),
                    ('secrets', 'sentinel-secret', NAMESPACE),
                    ('configmaps', 'ai-sdlc-authority', NAMESPACE),
                    ('deployments', 'sentinel-worker', NAMESPACE),
                    ('namespaces', NAMESPACE, ''),
                    ('runtimeclasses', 'unrelated-class', '')]:
            self.assertIn(key, api.objects)
        journal = json.loads((self.state / 'uninstall.json').read_text())
        self.assertEqual(journal['clusterUID'], UID)
        self.assertEqual(journal['purgedData'], [])
        self.assertEqual(set(journal['deleted']),
                         {'Deployment/ai-sdlc-worker', 'StatefulSet/ai-sdlc-postgres',
                          'Job/ai-sdlc-migrate-1', 'ConfigMap/ai-sdlc-config',
                          'ClusterRole/ai-sdlc-deployer', 'ClusterRoleBinding/ai-sdlc-deployer',
                          'RuntimeClass/ai-sdlc-offline'})

    def test_second_apply_is_idempotent(self):
        api = Api(objects=installed_objects())
        self.invoke(['--apply'], api)
        api.deleted.clear()
        result = self.invoke(['--apply'], api)
        self.assertTrue(result['applied'])
        self.assertEqual(result['remove'], [])
        self.assertEqual(result['purge'], [])
        self.assertEqual(result['deleted'], [])
        self.assertEqual(api.deleted, [])

    def test_delete_data_purges_only_named_owned_claims(self):
        api = Api(objects=installed_objects())
        result = self.invoke(['--apply', '--delete-data', 'data-ai-sdlc-postgres-0'], api)
        self.assertIn(('persistentvolumeclaims', 'data-ai-sdlc-postgres-0', NAMESPACE), api.deleted)
        self.assertIn(('persistentvolumeclaims', 'sentinel-data', NAMESPACE), api.objects)
        self.assertIn(('secrets', 'ai-sdlc-database-intake', NAMESPACE), api.objects)
        self.assertIn(('configmaps', 'ai-sdlc-authority', NAMESPACE), api.objects)
        journal = json.loads((self.state / 'uninstall.json').read_text())
        self.assertEqual(journal['purgedData'], ['data-ai-sdlc-postgres-0'])

    def test_delete_data_refuses_unowned_or_foreign_names(self):
        api = Api(objects=installed_objects())
        env = patch.dict(os.environ, {'XDG_STATE_HOME': str(self.root)})
        binaries = {'kubectl': 'kubectl', 'helm': 'helm', 'kind': 'kind'}
        for claim in ('sentinel-data', 'data-ai-sdlc-postgres-9', '../etc'):
            with env, patch.object(install, 'tools', return_value=binaries), \
                    patch.object(install, 'run', side_effect=api):
                with self.assertRaisesRegex(ValueError, 'installer-owned|Invalid'):
                    uninstall.main(['--cluster', 'test-cluster', '--apply',
                                    '--delete-data', claim])
        self.assertEqual(api.deleted, [])

    def test_foreign_uid_annotation_is_ambiguous_and_refuses(self):
        objects = installed_objects()
        objects[('configmaps', 'foreign-install', NAMESPACE)] = resource(
            'ConfigMap', 'foreign-install', NAMESPACE,
            annotations={uninstall.UID_ANNOTATION: 'another-cluster'})
        api = Api(objects=objects)
        env = patch.dict(os.environ, {'XDG_STATE_HOME': str(self.root)})
        binaries = {'kubectl': 'kubectl', 'helm': 'helm', 'kind': 'kind'}
        with env, patch.object(install, 'tools', return_value=binaries), \
                patch.object(install, 'run', side_effect=api):
            with self.assertRaisesRegex(ValueError, 'Ambiguous ownership'):
                uninstall.main(['--cluster', 'test-cluster', '--apply'])
        self.assertEqual(api.deleted, [])

    def test_unbound_release_record_refuses(self):
        objects = installed_objects()
        del objects[('configmaps', 'ai-sdlc-authority', NAMESPACE)]
        api = Api(objects=objects)
        env = patch.dict(os.environ, {'XDG_STATE_HOME': str(self.root)})
        binaries = {'kubectl': 'kubectl', 'helm': 'helm', 'kind': 'kind'}
        with env, patch.object(install, 'tools', return_value=binaries), \
                patch.object(install, 'run', side_effect=api):
            with self.assertRaisesRegex(ValueError, 'cannot be bound'):
                uninstall.main(['--cluster', 'test-cluster', '--apply'])
        self.assertEqual(api.deleted, [])

    def test_apply_refuses_until_intake_is_frozen(self):
        objects = installed_objects()
        objects[('configmaps', 'ai-sdlc-config', NAMESPACE)]['data'] = {
            'AI_SDLC_RUNTIME_MODE': 'candidate'}
        api = Api(objects=objects)
        result = self.invoke([], api)
        self.assertFalse(result['intakeStopped'])
        env = patch.dict(os.environ, {'XDG_STATE_HOME': str(self.root)})
        binaries = {'kubectl': 'kubectl', 'helm': 'helm', 'kind': 'kind'}
        with env, patch.object(install, 'tools', return_value=binaries), \
                patch.object(install, 'run', side_effect=api):
            with self.assertRaisesRegex(ValueError, 'freeze'):
                uninstall.main(['--cluster', 'test-cluster', '--apply'])
        self.assertEqual(api.deleted, [])

    def test_absent_release_and_empty_namespace_uninstall_to_nothing(self):
        api = Api(objects={}, manifest=None)
        result = self.invoke(['--apply'], api)
        self.assertEqual(result['deleted'], [])
        self.assertEqual(api.deleted, [])

    def test_delegation_reaches_the_uninstall_entrypoint(self):
        with patch.object(uninstall, 'main', return_value=0) as delegated:
            install.main(['uninstall', '--cluster', 'test-cluster'])
            delegated.assert_called_once_with(['--cluster', 'test-cluster'])


if __name__ == '__main__':
    unittest.main()
