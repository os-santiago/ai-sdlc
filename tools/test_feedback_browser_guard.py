"""Failure controls for the disposable Kubernetes guard fixture itself."""
import copy
import json
import subprocess
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import validate_feedback_browser_guard as fixture


class API:
    cluster = '00000000-0000-0000-0000-000000000001'

    def __init__(self, replace_before=None, cleanup_cost=5):
        self.namespace = None
        self.creates, self.deletes, self.timeouts = [], [], []
        self.residue = False
        self.sequence = 1
        self.now = 0
        self.cleanup_cost = cleanup_cost
        self.replace_before = replace_before
        self.cleanup = False

    def __call__(self, args, *, input, timeout, **kwargs):
        value = json.loads(input) if input else None
        command = args[1:]
        if self.cleanup or command[0] in ('delete', 'wait'):
            self.timeouts.append(timeout)
            if self.cleanup_cost > timeout:
                self.now += timeout
                raise subprocess.TimeoutExpired(args, timeout)
            self.now += self.cleanup_cost
        if command[:3] == ['get', 'namespace', 'kube-system']:
            answer = {'metadata': {'uid': self.cluster}}
        elif command[0] == 'create':
            self.creates.append(value['kind'])
            answer = copy.deepcopy(value)
            self.sequence += 1
            answer.setdefault('metadata', {})['uid'] = f'00000000-0000-0000-0000-{self.sequence:012d}'
            if value['kind'] == 'Namespace':
                self.namespace = answer
                self.residue = False
            elif value['kind'] != 'NetworkPolicy':
                self.residue = True
        elif command[:3] == ['get', 'namespace', 'ai-sdlc-browser']:
            answer = copy.deepcopy(self.namespace)
            if (self.replace_before == 'network' and self.creates == ['Namespace']
                    or self.replace_before == 'pod' and self.creates == ['Namespace', 'NetworkPolicy']):
                # Keep all benign labels; replacing only identity must suffice.
                answer['metadata']['uid'] = '00000000-0000-0000-0000-999999999999'
                self.namespace = answer
        elif command[:2] == ['get', 'pods,jobs,cronjobs']:
            answer = {'items': [{}] if self.residue else []}
            if self.residue:
                self.cleanup = True
        elif command[0] == 'delete':
            self.deletes.append(value['preconditions']['uid'])
            if self.deletes[-1] != self.namespace['metadata']['uid']:
                return SimpleNamespace(returncode=1, stdout='', stderr='conflict')
            answer = {}
        elif command[0] == 'wait':
            self.cleanup = False
            self.namespace = None
            answer = {}
        else:
            raise AssertionError(command)
        return SimpleNamespace(returncode=0, stdout=json.dumps(answer), stderr='')


class GuardFixtureTests(unittest.TestCase):
    def run_fixture(self, api):
        source = SimpleNamespace(git=lambda *args: b'fixture-head' if args[0] == 'rev-parse' else b'')
        with patch.object(fixture, 'Source', return_value=source), patch.object(
                fixture.time, 'monotonic', side_effect=lambda: api.now):
            return fixture.verify(['kubectl'], api.cluster, 'fixture@sha256:' + 'a'*64, runner=api)

    def test_replacement_cannot_receive_namespaced_post_or_cleanup(self):
        for stage, allowed in [('network', ['Namespace']), ('pod', ['Namespace', 'NetworkPolicy'])]:
            with self.subTest(stage=stage):
                api = API(replace_before=stage)
                result = self.run_fixture(api)
                self.assertEqual(result['status'], 'failed')
                self.assertEqual(api.creates, allowed)
                self.assertEqual(api.deletes, ['00000000-0000-0000-0000-000000000002'])
                self.assertEqual(result['cleanup'], [])
                self.assertIsNotNone(api.namespace)

    def test_cleanup_budget_is_shared_across_calls_and_cases(self):
        api = API()
        result = self.run_fixture(api)
        self.assertEqual(result['status'], 'passed')
        self.assertEqual(api.timeouts, list(range(70, 25, -5)))
        self.assertEqual(result['cleanupSecondsUsed'], 45)
        self.assertEqual(len(result['cleanup']), 3)

    def test_cleanup_timeout_stops_without_next_namespace(self):
        api = API(cleanup_cost=40)
        result = self.run_fixture(api)
        self.assertEqual(result['status'], 'failed')
        self.assertEqual(result['errorType'], 'TimeoutExpired')
        self.assertEqual(api.timeouts, [70, 30])
        self.assertEqual(result['cleanupSecondsUsed'], 70)
        self.assertEqual(api.creates.count('Namespace'), 1)
        self.assertEqual(result['cleanup'], [])

    def test_namespace_ack_requires_original_name_nonce_and_live_uid(self):
        original = {'metadata': {'name': 'ai-sdlc-browser', 'uid': API.cluster,
                                'labels': {'ai-sdlc.io/feedback-guard-fixture': 'owned'}}}
        for key, changed in [('name', 'foreign'), ('labels', {}), ('deletionTimestamp', 'now'), ('uid', '')]:
            value = copy.deepcopy(original)
            value['metadata'][key] = changed
            with self.subTest(key=key), self.assertRaises(ValueError):
                fixture.namespace_identity(value, 'owned')


if __name__ == '__main__':
    unittest.main()
