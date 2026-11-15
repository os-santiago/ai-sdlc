import json
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import github_client as client

TOKEN = 'fixture_credential_value_not_a_real_token'
REPO = 'example/project'


class GitHubClientTests(unittest.TestCase):
    def metadata(self, path):
        return ({'id': 42, 'login': 'fixture'} if path == '/user' else
                {'full_name': REPO, 'permissions': {'push': True}, 'archived': False})

    def credential(self):
        with patch.object(client, 'request', side_effect=lambda token, path: self.metadata(path)):
            return client.inspect(TOKEN, REPO)

    def api(self, current=None):
        return SimpleNamespace(owned_secret=Mock(return_value=current),
            create_owned_secret=Mock(), read_resource=Mock(), run=Mock())

    def test_cli_failure_is_sanitized_and_bounded(self):
        for result in (SimpleNamespace(returncode=1, stdout=TOKEN, stderr=TOKEN),
                       subprocess.TimeoutExpired(['gh'], 20, output=TOKEN)):
            with patch.object(client.subprocess, 'run') as run:
                if isinstance(result, Exception): run.side_effect = result
                else: run.return_value = result
                with self.assertRaises(ValueError) as error: client.from_gh()
                self.assertNotIn(TOKEN, str(error.exception))
                self.assertEqual(run.call_args.kwargs['timeout'], 20)
                self.assertTrue(run.call_args.kwargs['capture_output'])

    def test_scope_and_token_rejected_before_requests(self):
        with patch.object(client, 'request') as request:
            for token, repo in [(TOKEN, '../project'), (TOKEN, 'example/project?x=1'),
                                (TOKEN + '\nheader', REPO), ('', REPO)]:
                with self.assertRaises(ValueError): client.inspect(token, repo)
            request.assert_not_called()

    def test_wrong_repo_archived_and_read_only_credentials_fail(self):
        for repo in ({'full_name': 'other/project', 'permissions': {'push': True}, 'archived': False},
                     {'full_name': REPO, 'permissions': {'push': False}, 'archived': False},
                     {'full_name': REPO, 'permissions': {'push': True}, 'archived': True}):
            with patch.object(client, 'request', side_effect=[self.metadata('/user'), repo]):
                with self.assertRaises(ValueError): client.inspect(TOKEN, REPO)

    def test_response_errors_do_not_disclose_body_or_token(self):
        with patch.object(client.urllib.request, 'build_opener') as build:
            build.return_value.open.side_effect = OSError(TOKEN)
            with self.assertRaises(ValueError) as error: client.inspect(TOKEN, REPO)
            self.assertNotIn(TOKEN, str(error.exception))
            self.assertEqual(build.return_value.open.call_args.kwargs['timeout'], 20)
        self.assertIsNone(client.NoRedirect().redirect_request(None, None, 302, '', {}, 'https://other.invalid'))

    def test_create_is_validated_and_preserves_existing_credential(self):
        credential = self.credential()
        api = self.api()
        with patch.object(client, 'from_gh', return_value=TOKEN), patch.object(client, 'inspect', return_value=credential):
            report = client.configure(api, ['kubectl'], 'cluster-id', REPO)
        self.assertNotIn(TOKEN, json.dumps(report))
        self.assertEqual(api.create_owned_secret.call_args.args[2], {'credential.json': json.dumps(credential, sort_keys=True)})
        api = self.api({'credential.json': json.dumps(credential)})
        with patch.object(client, 'from_gh') as gh, patch.object(client, 'inspect', return_value=credential):
            client.configure(api, ['kubectl'], 'cluster-id', REPO)
            gh.assert_not_called()
        api.create_owned_secret.assert_not_called()
        api.run.assert_not_called()

    def test_scope_mismatch_prevents_refresh_and_revoked_token_is_not_replaced(self):
        credential = self.credential()
        api = self.api({'credential.json': json.dumps(credential)})
        with patch.object(client, 'from_gh') as gh:
            with self.assertRaises(ValueError): client.configure(api, [], 'cluster-id', 'other/repo', True)
            gh.assert_not_called()
        with patch.object(client, 'inspect', side_effect=ValueError('revoked')), patch.object(client, 'from_gh') as gh:
            with self.assertRaises(ValueError): client.configure(api, [], 'cluster-id', REPO)
            gh.assert_not_called()
        api.run.assert_not_called()

    def test_explicit_refresh_preserves_uid_and_resource_version(self):
        credential = self.credential()
        api = self.api({'credential.json': json.dumps(credential)})
        api.read_resource.return_value = {'apiVersion': 'v1', 'kind': 'Secret', 'type': 'Opaque',
            'metadata': {'name': 'ai-sdlc-github-client', 'uid': 'existing-uid', 'resourceVersion': '5',
                         'annotations': {'ai-sdlc.io/cluster-uid': 'cluster-id'}}, 'data': {'old': 'value'}}
        with patch.object(client, 'from_gh', return_value=TOKEN), patch.object(client, 'inspect', return_value=credential):
            client.configure(api, [], 'cluster-id', REPO, True)
        updated = json.loads(api.run.call_args.kwargs['input'])
        self.assertEqual(updated['metadata']['resourceVersion'], '5')
        self.assertEqual(updated['metadata']['uid'], 'existing-uid')
        self.assertNotIn('data', updated)
        self.assertEqual(api.run.call_args.args[0], ['replace', '-f', '-'])


if __name__ == '__main__': unittest.main()
