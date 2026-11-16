import base64
import copy
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest

import closed_build_provenance as closed
from test_konflux_provenance import statement, EXPECTED, IMAGE


def fixture():
    expected = dict(EXPECTED, IMAGE_URL='registry-service.kind-registry/ai-sdlc-fixtures/saferus',
                    **{'CHAINS-GIT_URL': 'https://github.com/os-santiago/saferus'})
    value = statement()
    value['subject'][0]['name'] = expected['IMAGE_URL']
    value['predicate']['materials'][1]['uri'] = 'git+' + expected['CHAINS-GIT_URL'] + '.git'
    task = value['predicate']['buildConfig']['tasks'][0]
    task['results'] = [{'name': k, 'type': 'string', 'value': v} for k, v in expected.items()]
    steps = [
        {'name': 'custody', 'args': ['--guard-workspace', '--storage-identity='+'e'*64,
          '--node-boot-id=11111111-1111-1111-1111-111111111111', '--workspace-phase=publish', '--authority-expires-at=1794812000000']},
        {'name': 'publish', 'args': ['--recipe=ai-sdlc.saferus-go-static/v1', '--input=/slot/workspace/output',
          '--output='+expected['IMAGE_URL']+':'+'f'*32, '--source-sha='+expected['CHAINS-GIT_COMMIT'],
          '--definition-digest='+expected['DEFINITION_DIGEST'], '--run-uid='+expected['RUN_UID'], '--timeout=4m']}]
    task['steps'] = [{'entryPoint': '/component', 'arguments': s['args'],
                     'environment': {'image': 'oci://'+IMAGE, 'container': s['name']}} for s in copy.deepcopy(steps)]
    return value, expected, steps


class ClosedProvenanceTests(unittest.TestCase):
    def test_exact_closed_identity(self):
        value, expected, steps = fixture()
        closed.bind_statement(value, expected, IMAGE, steps)
        envelope = {'payloadType': 'application/vnd.in-toto+json',
                    'payload': base64.b64encode(json.dumps(value).encode()).decode(), 'signatures': [{'sig': 'fixture-only'}]}
        self.assertEqual(closed.verified_statement(json.dumps(envelope).encode(), expected, IMAGE, steps), value)

    def test_each_closed_execution_value_and_common_identity_is_bound(self):
        for stage, count in [(0, 5), (1, 7)]:
            for index in range(count):
                with self.subTest(stage=stage, index=index):
                    value, expected, steps = fixture()
                    value['predicate']['buildConfig']['tasks'][0]['steps'][stage]['arguments'][index] += '-changed'
                    with self.assertRaises(ValueError): closed.bind_statement(value, expected, IMAGE, steps)
        for case in ('image', 'entrypoint', 'container', 'extra-step', 'missing-guard', 'subject', 'source', 'material', 'builder', 'duplicate-result'):
            with self.subTest(case=case):
                value, expected, steps = fixture(); task = value['predicate']['buildConfig']['tasks'][0]
                if case == 'image': task['steps'][0]['environment']['image'] += 'wrong'
                if case == 'entrypoint': task['steps'][1]['entryPoint'] = '/bin/sh'
                if case == 'container': task['steps'][0]['environment']['container'] = 'publish'
                if case == 'extra-step': task['steps'].append(copy.deepcopy(task['steps'][0]))
                if case == 'missing-guard': task['steps'].pop(0)
                if case == 'subject': value['subject'][0]['digest']['sha256'] = 'f'*64
                if case == 'source': next(r for r in task['results'] if r['name']=='CHAINS-GIT_COMMIT')['value']='f'*40
                if case == 'material': value['predicate']['materials'].pop()
                if case == 'builder': value['predicate']['builder']['id']='other'
                if case == 'duplicate-result': task['results'].append(copy.deepcopy(task['results'][0]))
                with self.assertRaises(ValueError): closed.bind_statement(value, expected, IMAGE, steps)

    @unittest.skipUnless(os.getenv('AI_SDLC_CLOSED_PROVENANCE_FIXTURE'), 'explicit actual fixture evidence required')
    def test_actual_fixture_hashes_and_externally_selected_source(self):
        path = Path(os.environ['AI_SDLC_CLOSED_PROVENANCE_FIXTURE'])
        report = json.loads(path.read_text()); source, tree = report['sourceSHA'], report['treeSHA']
        self.assertEqual(closed.load_fixture(path, source, tree)['sourceSHA'], source)
        with self.assertRaises(ValueError): closed.load_fixture(path, 'f'*40, tree)
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)
            for name in ('report.json','build.json','publish.json','fixture-slot.json','fixture-command.json','profile.json'):
                (target/name).write_bytes((path.parent/name).read_bytes())
            (target/'publish.json').write_bytes((target/'publish.json').read_bytes()+b' ')
            with self.assertRaises(ValueError): closed.load_fixture(target/'report.json',source,tree)

    @unittest.skipUnless(os.getenv('AI_SDLC_CLOSED_ATTESTATION'), 'actual verified envelope required')
    def test_rewritten_local_tree_and_hashes_cannot_reuse_actual_signature(self):
        original = Path(os.environ['AI_SDLC_CLOSED_PROVENANCE_FIXTURE'])
        signed = Path(os.environ['AI_SDLC_CLOSED_ATTESTATION']).read_bytes()
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)
            for name in ('report.json','build.json','publish.json','fixture-slot.json','fixture-command.json','profile.json'):
                (target/name).write_bytes((original.parent/name).read_bytes())
            report = json.loads((target/'report.json').read_text()); plan=report['transport']['plan']
            command = json.loads((target/'fixture-command.json').read_text())
            docs = {phase:json.loads((target/(phase+'.json')).read_text()) for phase in ('build','publish')}
            wrong_tree='f'*40
            report['treeSHA']=plan['treeSHA']=command['treeSHA']=wrong_tree
            docs['build']['spec']['pipelineSpec']['tasks'][0]['taskSpec']['steps'][1]['env'][1]['value']=wrong_tree
            def write():
                for phase in ('build','publish'):
                    raw=closed.go_json(docs[phase],sort_keys=True)
                    (target/(phase+'.json')).write_bytes(raw)
                    plan[phase+'RequestSHA256']=hashlib.sha256(raw).hexdigest()
                (target/'report.json').write_text(json.dumps(report))
                (target/'fixture-command.json').write_text(json.dumps(command))
            write()
            with self.assertRaises(ValueError): closed.load_fixture(target/'report.json',report['sourceSHA'],wrong_tree)
            # Even recomputing ALL mutable local definition/request hashes cannot
            # change the definition in the independently verified signed envelope.
            profile=json.loads((target/'profile.json').read_text());slot=json.loads((target/'fixture-slot.json').read_text())
            changed=closed.definition_digest(docs,profile,slot,plan['definitionSHA256'])
            plan['definitionSHA256']=changed
            report['transport']['results']['DEFINITION_DIGEST']='sha256:'+changed
            for phase in ('build','publish'):
                docs[phase]['metadata']['annotations']['ai-sdlc.definition-sha256']=changed
            docs['publish']['spec']['pipelineSpec']['tasks'][0]['taskSpec']['steps'][1]['args'][4]='--definition-digest=sha256:'+changed
            write()
            forged=closed.load_fixture(target/'report.json',report['sourceSHA'],wrong_tree)
            with self.assertRaises(ValueError):
                closed.verified_statement(signed,forged['results'],forged['publisherImage'],forged['closedSteps'])


if __name__ == '__main__': unittest.main()
