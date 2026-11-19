"""Namespace replacement must fail before fixture authority reaches a new owner."""
import json
import subprocess
import unittest
from unittest.mock import patch

from verify_review_repair_composition import controller


class NamespaceCustodyTests(unittest.TestCase):
    def run_fixture(self, replace_at):
        state = {'uid': 'owned', 'labels': {}, 'posts': 0, 'tokens': 0, 'gets': 0}
        def run(argv, **kwargs):
            args = argv[1:]
            if args[:3] == ['create', '-f', '-']:
                value = json.loads(kwargs['input'])
                if value['kind'] == 'Namespace':
                    state['labels'] = value['metadata']['labels']
                    return subprocess.CompletedProcess(argv, 0, json.dumps({'metadata': dict(uid='owned', labels=state['labels'])}).encode())
                state['posts'] += 1
                if replace_at == 'before-token':
                    state['uid'] = 'replacement'
                return subprocess.CompletedProcess(argv, 0, b'{}')
            if args[:2] == ['get', 'namespace']:
                state['gets'] += 1
                if replace_at == 'before-post':
                    state['uid'] = 'replacement'
                return subprocess.CompletedProcess(argv, 0, json.dumps({'metadata': dict(uid=state['uid'], labels=state['labels'])}).encode())
            if args[:2] == ['create', 'token']:
                state['tokens'] += 1
                return subprocess.CompletedProcess(argv, 0, b'fixture-token')
            if args[0] == 'get':
                return subprocess.CompletedProcess(argv, 0, b'{"items":[]}')
            raise AssertionError(args)
        with patch('verify_review_repair_composition.subprocess.run', side_effect=run), patch('verify_review_repair_composition.cleanup_namespace'):
            with self.assertRaisesRegex(ValueError, 'Original repair fixture namespace changed'):
                with controller(['kubectl'], {'APIServer': 'fixture', 'CA': 'fixture'}, 'reviewers', True):
                    if replace_at == 'before-post' or replace_at == 'before-token':
                        self.fail('replacement reached test workload')
                    state['uid'] = 'replacement'
        return state

    def test_replacement_before_permissions_sends_no_post_or_token(self):
        state = self.run_fixture('before-post')
        self.assertEqual((state['posts'], state['tokens']), (0, 0))

    def test_replacement_after_permissions_sends_no_token(self):
        state = self.run_fixture('before-token')
        self.assertEqual((state['posts'], state['tokens']), (1, 0))

    def test_replacement_after_test_cannot_accept_empty_inventory(self):
        state = self.run_fixture('after-test')
        self.assertEqual(state['tokens'], 1)


if __name__ == '__main__':
    unittest.main()
