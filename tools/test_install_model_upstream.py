import json
import tempfile
import unittest
from pathlib import Path

import candidate_install

UPSTREAM = {'adapter': 'openai', 'url': 'http://10.89.0.1:18090/v1/chat/completions', 'model': 'claude-sonnet',
            'cidr': '10.89.0.1/32', 'port': 18090, 'seconds': 300, 'directAnswers': False,
            'callSeconds': 330, 'reviewCallSeconds': 170, 'credentialSecret': ''}


def values():
    profile = {'version': 'ai-sdlc.executor-profile/v1', 'maxSeconds': 600, 'maxSteps': 24, 'maxTokens': 4096}
    model = {'Limits': {'Calls': 16, 'InputBytes': 2097152, 'OutputBytes': 4194304}, 'ResponseLimit': 262144}
    return {'candidate': {'configJSON': json.dumps({'profile': profile, 'model': model})},
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
        limits = json.loads(result['candidate']['configJSON'])['model']['Limits']
        self.assertEqual(limits, {'Calls': 24, 'InputBytes': 4194304, 'OutputBytes': 24 * 262144})

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
        installed = {'version': 'ai-sdlc.candidate-runtime/v1', 'profile': {'version': 'ai-sdlc.executor-profile/v1', 'maxSteps': 24, 'maxSeconds': 600, 'maxTokens': 4096}}
        configmap = json.dumps({'data': {'config.json': json.dumps(installed)}})
        desired = {'candidate': {'configJSON': json.dumps({'profile': {'maxSteps': 24, 'maxSeconds': 600, 'maxTokens': 4096, 'callSeconds': 330}})}}
        with patch.object(install, 'run', return_value=type('R', (), {'stdout': configmap})()):
            with self.assertRaisesRegex(ValueError, 'model service differs'):
                candidate_install.require_unchanged_tool_budget(['kubectl'], desired)
            desired['candidate']['configJSON'] = json.dumps({'profile': {'maxSteps': 24, 'maxSeconds': 600, 'maxTokens': 4096}})
            candidate_install.require_unchanged_tool_budget(['kubectl'], desired)
            desired['candidate']['configJSON'] = json.dumps({'profile': {'maxSteps': 24, 'maxSeconds': 600, 'maxTokens': 4096}, 'model': {'Limits': {'Calls': 24}}})
            with self.assertRaisesRegex(ValueError, 'model budget differs'):
                candidate_install.require_unchanged_tool_budget(['kubectl'], desired)

    def test_upstream_calls_follow_the_selected_step_budget(self):
        config = json.loads(values()['candidate']['configJSON'])
        config['profile'].update(maxSteps=32, maxSeconds=900)
        selected = dict(values(), candidate={'configJSON': json.dumps(config)})
        result = json.loads(candidate_install.apply_model_upstream(selected, write(UPSTREAM))['candidate']['configJSON'])
        self.assertEqual((result['profile']['maxSteps'], result['profile']['maxSeconds'], result['profile']['callSeconds']), (32, 900, 330))
        self.assertEqual(result['model']['Limits'], {'Calls': 32, 'InputBytes': 4194304, 'OutputBytes': 32 * 262144})

    def test_the_benchmark_answer_envelope_follows_the_answer_bound(self):
        # Default: the envelope reviewed before the option existed.
        default = candidate_install.apply_model_upstream(values(), write(UPSTREAM))['modelGateway']
        self.assertEqual(default['benchmark'], {'promptTokens': 16384, 'completionTokens': 4096})
        config = json.loads(values()['candidate']['configJSON'])
        config['profile']['maxTokens'] = 8192
        selected = dict(values(), candidate={'configJSON': json.dumps(config)})
        result = candidate_install.apply_model_upstream(selected, write(UPSTREAM))
        # The derived per-call timeout must cover the longest admitted answer.
        self.assertEqual(result['modelGateway']['benchmark'], {'promptTokens': 16384, 'completionTokens': 8192})
        # Reviewers keep their short-verdict envelope.
        self.assertEqual(result['modelGateway']['reviewBenchmark'], default['reviewBenchmark'])
        limits = json.loads(result['candidate']['configJSON'])['model']
        # Each call still reserves the 256 KiB response limit: 8192 tokens fit
        # within it at up to 32 bytes per token, and 32 calls within 8 MiB.
        self.assertGreaterEqual(limits['ResponseLimit'], 8192 * 32)
        self.assertLessEqual(limits['Limits']['OutputBytes'], 8 * 1024 * 1024)


if __name__ == '__main__':
    unittest.main()
