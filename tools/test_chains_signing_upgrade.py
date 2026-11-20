"""Fail-closed guards of the publish account credential step of Chains signing."""
import copy
import json
import pathlib
import tempfile
import unittest
from unittest.mock import Mock, patch

import build_foundation_state as foundation
import chains_signing_upgrade as signing

UID = '00000000-0000-4000-8000-000000000001'


def record(desired=None, **changes):
    value = {'version': 'ai-sdlc.build-foundation/v1', 'status': 'prepared', 'manifestSHA256': 'm',
             'resources': {signing.KEY: {'uid': UID, 'nonce': 'n', 'desiredSHA256': desired or signing.sha(
                 foundation.body(signing.original_publish()))}}}
    value.update(changes)
    return value


class Cluster:
    def __init__(self, account):
        self.account = account
        self.calls = []

    def __call__(self, argv):
        self.calls.append(argv)
        if argv[0] == 'get':
            return json.dumps(self.account)
        if argv[0] == 'patch':
            ops = json.loads(argv[argv.index('-p') + 1])
            if ops[0] != {'op': 'test', 'path': '/metadata/resourceVersion', 'value': self.account['metadata']['resourceVersion']}:
                raise RuntimeError('precondition failed')
            self.account['imagePullSecrets'] = ops[1]['value']
            self.account['metadata']['resourceVersion'] = str(int(self.account['metadata']['resourceVersion']) + 1)
            return ''
        raise AssertionError(argv)


def live_account(**extra):
    account = signing.original_publish()
    account['metadata'].update(uid=UID, resourceVersion='7', annotations={foundation.ANNOTATION: 'n'})
    account.update(extra)
    return account


class Step(unittest.TestCase):
    def setUp(self):
        self.dir = pathlib.Path(tempfile.mkdtemp())

    def step(self, value, cluster):
        (self.dir / 'build-foundation.json').write_text(json.dumps(value))
        return signing.PublishAccountStep(self.dir, cluster)

    def test_the_reviewed_blueprint_is_exactly_the_credential_delta(self):
        blueprint = json.loads(signing.BLUEPRINT.read_text())
        target = signing.target_publish(blueprint)
        self.assertEqual(target['imagePullSecrets'], [{'name': 'registry-auth'}])
        self.assertEqual({k: v for k, v in target.items() if k != 'imagePullSecrets'}, signing.original_publish())
        for change in (lambda a: a.__setitem__('imagePullSecrets', [{'name': 'other'}]),
                       lambda a: a.__setitem__('automountServiceAccountToken', True),
                       lambda a: a['secrets'].append({'name': 'extra'})):
            bad = copy.deepcopy(blueprint)
            change([i for i in bad['items'] if i['kind'] == 'ServiceAccount' and i['metadata']['name'] == 'publish'][0])
            with self.subTest(change=change), self.assertRaisesRegex(ValueError, 'credential delta'):
                signing.target_publish(bad)

    def test_original_account_is_journaled_patched_with_precondition_and_recorded(self):
        cluster = Cluster(live_account())
        step = self.step(record(), cluster)
        self.assertEqual(step.phase(), 'original')
        self.assertEqual(step.run(), 'admitted')
        patches = [c for c in cluster.calls if c[0] == 'patch']
        self.assertEqual(len(patches), 1)
        saved = json.loads((self.dir / 'build-foundation.json').read_text())
        self.assertEqual(saved['resources'][signing.KEY]['desiredSHA256'], step.target_sha)
        self.assertTrue(saved['chainsSigning']['completed'])
        self.assertNotEqual(saved['manifestSHA256'], 'm')
        self.assertEqual(cluster.account['imagePullSecrets'], [{'name': 'registry-auth'}])

    def test_a_crash_after_the_patch_resumes_without_a_second_patch(self):
        cluster = Cluster(live_account(imagePullSecrets=[{'name': 'registry-auth'}]))
        value = record(chainsSigning={'version': signing.VERSION, 'completed': False})
        step = self.step(value, cluster)
        step.record['chainsSigning'].update({'from': step.original_sha, 'to': step.target_sha})
        step.save()
        step = signing.PublishAccountStep(self.dir, cluster)
        self.assertEqual(step.phase(), 'applied')
        self.assertEqual(step.run(), 'admitted')
        self.assertFalse([c for c in cluster.calls if c[0] == 'patch'])

    def test_replaced_drifted_or_unjournaled_accounts_are_refused(self):
        replaced = live_account()
        replaced['metadata']['uid'] = '00000000-0000-4000-8000-000000000002'
        with self.assertRaisesRegex(ValueError, 'identity or custody'):
            self.step(record(), Cluster(replaced)).run()
        renonced = live_account()
        renonced['metadata']['annotations'][foundation.ANNOTATION] = 'other'
        with self.assertRaisesRegex(ValueError, 'identity or custody'):
            self.step(record(), Cluster(renonced)).run()
        drifted = live_account(automountServiceAccountToken=True)
        with self.assertRaisesRegex(ValueError, 'without an original operation'):
            self.step(record(), Cluster(drifted)).run()
        admitted = live_account(imagePullSecrets=[{'name': 'registry-auth'}])
        with self.assertRaisesRegex(ValueError, 'without an original operation'):
            self.step(record(), Cluster(admitted)).run()
        with self.assertRaisesRegex(ValueError, 'both reviewed states'):
            self.step(record(), Cluster(drifted)).phase()

    def test_only_a_prepared_original_foundation_is_selectable(self):
        for change in ({'status': 'installing'}, {'version': 'x'}, {'resources': {}}):
            with self.subTest(change=change), self.assertRaisesRegex(ValueError, 'prepared build foundation'):
                self.step(record(**change), Cluster(live_account()))

    def test_a_concurrent_change_fails_the_precondition(self):
        cluster = Cluster(live_account())
        step = self.step(record(), cluster)
        original = cluster.__call__

        def racing(argv):
            if argv[0] == 'patch':
                cluster.account['metadata']['resourceVersion'] = '8'
            return original(argv)
        step.call = racing
        with self.assertRaisesRegex(RuntimeError, 'precondition'):
            step.run()


class Combined(unittest.TestCase):
    def test_registry_then_account_order_and_status(self):
        order = []
        with patch.object(signing.registry_boundary_upgrade, 'RegistryBoundaryUpgrade') as registry, \
                patch.object(signing, 'PublishAccountStep') as account:
            registry.return_value.run.side_effect = lambda: order.append('registry')
            account.return_value.run.side_effect = lambda: order.append('account')
            registry.return_value.status.return_value = {'phase': 'admitted'}
            account.return_value.phase.return_value = 'admitted'
            result = signing.ChainsSigningUpgrade(Mock(), pathlib.Path('/x'), Mock()).run()
        self.assertEqual(order, ['registry', 'account'])
        self.assertEqual((result['registry']['phase'], result['publishAccount']), ('admitted', 'admitted'))


if __name__ == '__main__':
    unittest.main()
