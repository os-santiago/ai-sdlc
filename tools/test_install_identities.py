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
                  for role in installer.ROLES}
    keys = {role + '-1': {'role': role, 'value': 'fixture-public-' + role} for role in installer.ROLES}
    policy = {'version': '1', 'repository': 'example/project', 'environment': 'production-test',
              'autonomy': 'shadow', 'freeze': True, 'keys': keys, 'grants': []}
    # Cryptographic validation is exercised by the Go bootstrap tests and the
    # real installer integration; these fixtures isolate Python reconciliation.
    secrets, passwords = {}, {}
    for role in installer.ROLES:
        secrets['ai-sdlc-identity-' + role] = {'identity.json': json.dumps(identities[role]),
                                             'public-key.json': json.dumps(keys[role + '-1'])}
        password = role.replace('-', '_') + 'x' * 40
        passwords[role] = password
        secrets['ai-sdlc-database-' + role] = {'password': password, 'uri': installer.component_uri(role, password)}
    secrets['ai-sdlc-database-roles'] = {'passwords.json': json.dumps(passwords)}
    material = {'policy': copy.deepcopy(policy), 'identities': copy.deepcopy(identities)}
    for identity in material['identities'].values():
        identity['privateKey'] = 'fixture-new-' + identity['role']
    resource = {'metadata': {'annotations': {'ai-sdlc.io/cluster-uid': 'owned-uid'}},
                'data': {'policy.json': json.dumps(policy)}}
    return material, secrets, resource


class IdentityInstallationTests(unittest.TestCase):
    def configure(self, material, secrets, resource, freeze=None):
        lock = {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'},
                'migrator': {'digest': 'sha256:' + 'a' * 64}}
        with patch.object(installer, 'identity_material', return_value=material), \
             patch.object(installer, 'owned_secret', side_effect=lambda k, name, uid: secrets.get(name)), \
             patch.object(installer, 'read_resource', return_value=resource), \
             patch.object(installer, 'run') as run, \
             patch.object(installer, 'create_owned_secret') as create:
            try:
                result = installer.configure_identities(['kubectl'], 'podman', lock, 'owned-uid',
                                                        'example/project', 'production-test', freeze=freeze)
            finally:
                create.assert_not_called()
            self.assertEqual(run.call_count, 1)
            private_input = json.loads(run.call_args.kwargs['input'])
            for role in installer.ROLES:
                self.assertEqual(private_input['identities'][role]['privateKey'], 'fixture-kept-' + role)
            self.assertNotIn('privateKey', json.dumps(result))
            self.assertNotIn('password', json.dumps(result))
            return result

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
