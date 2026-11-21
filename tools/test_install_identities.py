"""Exercise ownership, preservation and preflight before credential mutation."""
import copy
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('installer', Path(__file__).with_name('install.py'))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


def fixture():
    identities = {role: {'keyId': role + '-1', 'role': role, 'privateKey': 'fixture-kept-' + role}
                  for role in installer.ALL_ROLES}
    keys = {role + '-1': {'role': role, 'value': 'fixture-public-' + role} for role in installer.ALL_ROLES}
    policy = {'version': '1', 'repository': 'example/project', 'environment': 'production-test',
              'autonomy': 'shadow', 'freeze': True, 'keys': {k:v for k,v in keys.items() if v['role'] != 'build-controller'}, 'grants': []}
    # Cryptographic validation is exercised by the Go bootstrap tests and the
    # real installer integration; these fixtures isolate Python reconciliation.
    secrets, passwords = {}, {}
    for role in installer.ALL_ROLES:
        secrets['ai-sdlc-identity-' + role] = {'identity.json': json.dumps(identities[role]),
                                             'public-key.json': json.dumps(keys[role + '-1'])}
        password = role.replace('-', '_') + 'x' * 40
        passwords[role] = password
        secrets['ai-sdlc-database-' + role] = {'password': password, 'uri': installer.component_uri(role, password)}
    secrets['ai-sdlc-database-roles'] = {'passwords.json': json.dumps(passwords)}
    material = {'policy': copy.deepcopy(policy), 'identities': copy.deepcopy(identities)}
    material['policy']['keys'] = copy.deepcopy(keys)
    for identity in material['identities'].values():
        identity['privateKey'] = 'fixture-new-' + identity['role']
    resource = {'metadata': {'annotations': {'ai-sdlc.io/cluster-uid': 'owned-uid'}},
                'data': {'policy.json': json.dumps(policy)}}
    return material, secrets, resource


class IdentityInstallationTests(unittest.TestCase):
    def configure(self, material, secrets, resource, freeze=None, **options):
        lock = {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'},
                'migrator': {'digest': 'sha256:' + 'a' * 64}}
        with patch.object(installer, 'identity_material', return_value=material), \
             patch.object(installer, 'owned_secret', side_effect=lambda k, name, uid: secrets.get(name)), \
             patch.object(installer, 'read_resource', return_value=resource), \
             patch.object(installer, 'run') as run, \
             patch.object(installer, 'create_owned_secret') as create:
            try:
                result = installer.configure_identities(['kubectl'], 'podman', lock, 'owned-uid',
                                                        'example/project', 'production-test', freeze=freeze, **options)
            finally:
                create.assert_not_called()
            self.assertEqual(run.call_count, 1)
            private_input = json.loads(run.call_args.kwargs['input'])
            for role in installer.ALL_ROLES:
                self.assertEqual(private_input['identities'][role]['privateKey'], 'fixture-kept-' + role)
            self.assertNotIn('privateKey', json.dumps(result))
            self.assertNotIn('password', json.dumps(result))
            return result

    def test_a_project_manifest_is_compiled_bound_and_emitted(self):
        material, secrets, resource = fixture()
        manifest = {'version': 'ai-sdlc.project/v1', 'repository': 'example/project'}
        lock = {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'}, 'migrator': {'digest': 'sha256:' + 'a' * 64}}
        calls = []
        def run(args, **kwargs):
            calls.append(args[-1])
            if args[-1] == 'compile-project-installation':
                return SimpleNamespace(stdout=json.dumps({'version': 'ai-sdlc.project-installation/v1', 'manifestJSON': '{"m":1}',
                                                          'manifestSHA256': 'f' * 64, 'verifierQuota': {'requests.cpu': '810m'}}))
            policy = json.loads(kwargs['input'])['policy']
            self.assertEqual(policy['projectSHA256'], 'f' * 64)
            return SimpleNamespace(stdout='')
        with patch.object(installer, 'identity_material', return_value=material), \
             patch.object(installer, 'owned_secret', side_effect=lambda k, name, uid: secrets.get(name)), \
             patch.object(installer, 'read_resource', return_value=resource), \
             patch.object(installer, 'run', side_effect=run), \
             patch.object(installer, 'create_owned_secret'):
            result = installer.configure_identities(['kubectl'], 'podman', lock, 'owned-uid', 'example/project', 'production-test',
                                                    project_manifest=manifest)
        self.assertEqual(calls, ['compile-project-installation', 'validate-bootstrap'])
        self.assertEqual(json.loads(result['authority']['policyJSON'])['projectSHA256'], 'f' * 64)
        self.assertEqual(result['project'], {'manifestJSON': '{"m":1}', 'verifierQuota': {'requests.cpu': '810m'}})
        bound = json.loads(resource['data']['policy.json'])
        bound['projectSHA256'] = 'f' * 64
        resource['data']['policy.json'] = json.dumps(bound)
        with self.assertRaises(ValueError):
            self.configure(material, secrets, resource)

    def test_reapplication_preserves_identities_passwords_and_policy(self):
        material, secrets, resource = fixture()
        result = self.configure(material, secrets, resource)
        self.assertEqual(json.loads(result['authority']['policyJSON']), json.loads(resource['data']['policy.json']))

    def test_scope_key_or_password_conflict_prevents_creation(self):
        for conflict in ('scope', 'key', 'password'):
            with self.subTest(conflict=conflict):
                material, secrets, resource = fixture()
                if conflict in ('scope', 'key'):
                    policy = json.loads(resource['data']['policy.json'])
                    if conflict == 'scope': policy['repository'] = 'other/project'
                    else: policy['keys']['worker-1']['value'] = 'changed-public-key'
                    resource['data']['policy.json'] = json.dumps(policy)
                else:
                    secrets['ai-sdlc-database-roles']['passwords.json'] = '{}'
                with self.assertRaises(ValueError):
                    self.configure(material, secrets, resource)

    def test_additive_builder_upgrade_preserves_inner_policy_and_old_credentials(self):
        material, secrets, resource = fixture()
        before = copy.deepcopy(secrets)
        for name in ('ai-sdlc-identity-build-controller', 'ai-sdlc-database-build-controller'):
            del secrets[name]
        passwords = json.loads(secrets['ai-sdlc-database-roles']['passwords.json'])
        del passwords['build-controller']
        secrets['ai-sdlc-database-roles']['passwords.json'] = json.dumps(passwords)
        lock = {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'}, 'migrator': {'digest': 'sha256:' + 'a'*64}}
        with patch.object(installer, 'identity_material', return_value=material), \
             patch.object(installer, 'owned_secret', side_effect=lambda k,n,u: secrets.get(n)), \
             patch.object(installer, 'read_resource', return_value=resource), \
             patch.object(installer, 'run') as run, \
             patch.object(installer, 'create_owned_secret') as create, \
             patch.object(installer, 'extend_owned_role_secret') as extend:
            result = installer.configure_identities([], 'podman', lock, 'owned-uid', 'example/project', 'production-test')
        self.assertEqual(json.loads(result['authority']['policyJSON']), json.loads(resource['data']['policy.json']))
        self.assertEqual([c.args[1] for c in create.call_args_list], ['ai-sdlc-identity-build-controller', 'ai-sdlc-database-build-controller'])
        upgraded = json.loads(extend.call_args.args[3]['passwords.json'])
        self.assertEqual({k:v for k,v in upgraded.items() if k != 'build-controller'}, passwords)
        validated = json.loads(run.call_args.kwargs['input'])
        for role in installer.ALL_ROLES:
            if role != 'build-controller':
                self.assertEqual(validated['identities'][role], json.loads(before['ai-sdlc-identity-'+role]['identity.json']))

    def test_migrator_addition_uses_resource_version_and_rejects_changed_owner(self):
        import base64
        previous = {'passwords.json': '{"fixture":"old"}'}
        resource = {'apiVersion':'v1','kind':'Secret','type':'Opaque',
                    'metadata':{'uid':'secret-uid','resourceVersion':'23','annotations':{'ai-sdlc.io/cluster-uid':'owned-uid'}},
                    'data':{k:base64.b64encode(v.encode()).decode() for k,v in previous.items()}}
        with patch.object(installer, 'read_resource', return_value=resource), patch.object(installer, 'run') as run:
            installer.extend_owned_role_secret([], 'owned-uid', previous, {'passwords.json':'{"fixture":"old","build":"new"}'})
            sent=json.loads(run.call_args.kwargs['input'])
            self.assertEqual(sent['metadata']['resourceVersion'],'23')
            self.assertEqual(sent['metadata']['uid'],'secret-uid')
            self.assertEqual(run.call_args.args[0], ['replace','-f','-'])
            run.reset_mock()
            with self.assertRaisesRegex(ValueError, 'changed'):
                installer.extend_owned_role_secret([], 'owned-uid', previous, {})
            run.assert_not_called()

    def test_old_pinned_image_remains_inner_only_and_cannot_enable_build(self):
        material, secrets, resource = fixture()
        del material['identities']['build-controller']
        del material['policy']['keys']['build-controller-1']
        for name in ('ai-sdlc-identity-build-controller','ai-sdlc-database-build-controller'):
            del secrets[name]
        old=json.loads(secrets['ai-sdlc-database-roles']['passwords.json'])
        del old['build-controller']
        secrets['ai-sdlc-database-roles']['passwords.json']=json.dumps(old)
        lock={'image':{'repository':'quay.io/os-santiago/ai-sdlc'},'migrator':{'digest':'sha256:'+'a'*64}}
        with patch.object(installer,'identity_material',return_value=material), \
             patch.object(installer,'owned_secret',side_effect=lambda k,n,u:secrets.get(n)), \
             patch.object(installer,'read_resource',return_value=resource), \
             patch.object(installer,'run'), patch.object(installer,'create_owned_secret') as create:
            result=installer.configure_identities([], 'podman', lock, 'owned-uid','example/project','production-test')
            self.assertEqual(json.loads(result['authority']['policyJSON']),json.loads(resource['data']['policy.json']))
            with self.assertRaisesRegex(ValueError,'required component'):
                installer.configure_identities([], 'podman', lock, 'owned-uid','example/project','production-test',require_build=True)
            create.assert_not_called()

    def test_foreign_secret_is_not_adopted(self):
        resource = {'type': 'Opaque', 'metadata': {'annotations': {'ai-sdlc.io/cluster-uid': 'other'}}}
        with patch.object(installer, 'read_resource', return_value=resource):
            with self.assertRaisesRegex(ValueError, 'ownership'):
                installer.owned_secret(['kubectl'], 'ai-sdlc-identity-worker', 'owned-uid')

    def test_profile_changes_only_freeze_and_never_replaces_keys(self):
        material, secrets, resource = fixture()
        original = json.loads(resource['data']['policy.json'])
        result = self.configure(material, secrets, resource, freeze=False)
        expected = dict(original, freeze=False)
        self.assertEqual(json.loads(result['authority']['policyJSON']), expected)

    def test_verification_upgrade_restores_only_reviewed_four_grants(self):
        material, secrets, resource = fixture()
        material['policy']['grants'] = [{'operation': name} for name in
            ('admit-issue', 'plan-admission', 'execute-work', 'verify-artifact', 'release-artifact')]
        old = json.loads(resource['data']['policy.json'])
        old.update(autonomy='auto-pr', grants=material['policy']['grants'][:3])
        resource['data']['policy.json'] = json.dumps(old)
        result = self.configure(material, secrets, resource, freeze=False, candidate=True, verification=True)
        policy = json.loads(result['authority']['policyJSON'])
        self.assertEqual({g['operation'] for g in policy['grants']},
                         {'admit-issue', 'plan-admission', 'execute-work', 'verify-artifact'})
        material, secrets, _ = fixture()
        material['policy']['grants'] = policy['grants']
        resource['data']['policy.json'] = json.dumps(policy)
        result = self.configure(material, secrets, resource, freeze=False, candidate=True)
        self.assertEqual(len(json.loads(result['authority']['policyJSON'])['grants']), 3)

    def test_shadow_has_no_ambient_credential_fallback(self):
        with patch.object(installer, 'owned_secret', return_value=None), patch.object(installer, 'configure_github_client') as configure:
            with self.assertRaisesRegex(ValueError, 'explicitly imported'):
                installer.shadow_intake_config([], 'owned-uid', 'example/project')
            configure.assert_not_called()

    def test_private_bootstrap_output_cannot_enter_container_logs(self):
        with patch.object(installer, 'run', return_value=SimpleNamespace(stdout='{}')) as run:
            installer.identity_material('podman', 'pinned-image', 'example/project', 'production-test')
        self.assertIn('--log-driver=none', run.call_args.args[0])
        self.assertIn('--network=none', run.call_args.args[0])
        self.assertTrue(run.call_args.kwargs['capture_output'])

    def test_shadow_rejects_concurrent_credential_change(self):
        with patch.object(installer, 'owned_secret', side_effect=[{'credential.json': '{}'}, {'credential.json': 'changed'}]), patch.object(installer, 'configure_github_client'):
            with self.assertRaisesRegex(ValueError, 'changed while deriving'):
                installer.shadow_intake_config([], 'owned-uid', 'example/project')


if __name__ == '__main__':
    unittest.main()
