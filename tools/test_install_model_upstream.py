import json
import tempfile
import unittest
from pathlib import Path

import candidate_install

UPSTREAM = {'adapter': 'openai', 'url': 'http://10.89.0.1:18090/v1/chat/completions', 'model': 'claude-sonnet',
            'cidr': '10.89.0.1/32', 'port': 18090, 'seconds': 300, 'directAnswers': False,
            'callSeconds': 330, 'reviewCallSeconds': 170, 'credentialSecret': ''}


def values():
    profile = {'version': 'ai-sdlc.executor-profile/v1', 'maxSeconds': 600}
    return {'candidate': {'configJSON': json.dumps({'profile': profile})},
            'review': {'configJSON': json.dumps({'profile': dict(profile, maxSeconds=180)})}}


def write(document):
    file = tempfile.NamedTemporaryFile('w', suffix='.json', delete=False)
    file.write(document if isinstance(document, str) else json.dumps(document))
    file.close()
    return Path(file.name)


class ModelUpstreamInstallTests(unittest.TestCase):
    def test_a_reviewed_upstream_sets_the_gateway_and_call_windows(self):
        result = candidate_install.apply_model_upstream(values(), write(UPSTREAM))
        self.assertEqual(result['modelGateway']['adapter'], 'openai')
        self.assertEqual(result['modelGateway']['upstream']['cidr'], '10.89.0.1/32')
        self.assertEqual(json.loads(result['candidate']['configJSON'])['profile']['callSeconds'], 330)
        self.assertEqual(json.loads(result['review']['configJSON'])['profile']['callSeconds'], 170)

    def test_unsafe_or_ambiguous_documents_are_refused(self):
        for document in (dict(UPSTREAM, url='http://8.8.8.8:18090/v1/chat/completions', cidr='8.8.8.8/32'),
                         dict(UPSTREAM, credentialSecret='model-service'),
                         dict(UPSTREAM, callSeconds=700),
                         dict(UPSTREAM, extra=True),
                         '{"adapter":"openai","adapter":"openai"}'):
            with self.assertRaises(ValueError):
                candidate_install.apply_model_upstream(values(), write(document))


    def test_reconciliation_cannot_switch_the_model_service(self):
        from unittest.mock import patch
        import install
        installed = {'version': 'ai-sdlc.candidate-runtime/v1', 'profile': {'version': 'ai-sdlc.executor-profile/v1', 'maxSteps': 24}}
        configmap = json.dumps({'data': {'config.json': json.dumps(installed)}})
        desired = {'candidate': {'configJSON': json.dumps({'profile': {'maxSteps': 24, 'callSeconds': 330}})}}
        with patch.object(install, 'run', return_value=type('R', (), {'stdout': configmap})()):
            with self.assertRaisesRegex(ValueError, 'model service differs'):
                candidate_install.require_unchanged_tool_budget(['kubectl'], desired)
            desired['candidate']['configJSON'] = json.dumps({'profile': {'maxSteps': 24}})
            candidate_install.require_unchanged_tool_budget(['kubectl'], desired)


if __name__ == '__main__':
    unittest.main()
