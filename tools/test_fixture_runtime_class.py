"""Fixture controllers may only read the one fixture RuntimeClass, and only this run's grant is removed."""
import unittest

import fixture_runtime_class


class FixtureRuntimeClassTests(unittest.TestCase):
    def test_grant_reads_only_the_named_class(self):
        calls = []
        fixture_runtime_class.grant(lambda args, value=None: calls.append((args, value)), 'abc123', [('ai-sdlc-verifiers', 'verifier')])
        [(args, value)] = calls
        self.assertEqual(args, ['create', '-f', '-'])
        role, binding = value['items']
        self.assertEqual(role['kind'], 'ClusterRole')
        self.assertEqual(role['rules'], [{'apiGroups': ['node.k8s.io'], 'resources': ['runtimeclasses'],
                                          'resourceNames': ['ai-sdlc-offline'], 'verbs': ['get']}])
        self.assertEqual(binding['roleRef']['name'], role['metadata']['name'])
        self.assertEqual(binding['subjects'], [{'kind': 'ServiceAccount', 'name': 'verifier', 'namespace': 'ai-sdlc-verifiers'}])
        for item in value['items']:
            self.assertEqual(item['metadata']['labels'], {fixture_runtime_class.LABEL: 'abc123'})

    def test_revoke_selects_only_this_run(self):
        calls = []
        fixture_runtime_class.revoke(lambda args, value=None: calls.append(args), 'abc123')
        self.assertEqual(calls, [['delete', 'clusterrolebinding,clusterrole', '-l', fixture_runtime_class.LABEL + '=abc123',
                                  '--ignore-not-found', '--wait=true']])

    def test_grant_requires_a_subject_and_fresh_nonces_differ(self):
        with self.assertRaises(ValueError):
            fixture_runtime_class.objects('abc123', [])
        self.assertNotEqual(fixture_runtime_class.new_nonce(), fixture_runtime_class.new_nonce())


if __name__ == '__main__':
    unittest.main()
