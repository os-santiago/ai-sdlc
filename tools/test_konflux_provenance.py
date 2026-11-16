import base64
import copy
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import konflux_provenance as provenance

EXPECTED={'IMAGE_URL':'registry-service.kind-registry/ai-sdlc-fixtures/homedir','IMAGE_DIGEST':'sha256:'+'a'*64,
          'CHAINS-GIT_URL':'https://github.com/os-santiago/homedir','CHAINS-GIT_COMMIT':'b'*40,
          'DEFINITION_DIGEST':'sha256:'+'c'*64,'RUN_UID':'run-123'}
IMAGE='localhost/ai-sdlc-artifact-builder@sha256:'+'d'*64

def statement():
    task={'name':'publish','status':'Succeeded','serviceAccountName':'publish',
          'results':[{'name':k,'type':'string','value':v} for k,v in EXPECTED.items()],
          'invocation':{'parameters':{'run-uid':'run-123'},'environment':{'labels':{'tekton.dev/pipelineRunUID':'run-123'}}},
          'steps':[{'entryPoint':'/component','environment':{'image':'oci://'+IMAGE},'arguments':[
            '--input=/artifacts/source/quarkus-app/target/quarkus-app','--output='+EXPECTED['IMAGE_URL']+':123456abcdef',
            '--source-sha='+EXPECTED['CHAINS-GIT_COMMIT'],'--definition-digest='+EXPECTED['DEFINITION_DIGEST'],
            '--run-uid=run-123','--timeout=8m']}]}
    return {'_type':'https://in-toto.io/Statement/v0.1','predicateType':'https://slsa.dev/provenance/v0.2',
            'subject':[{'name':EXPECTED['IMAGE_URL'],'digest':{'sha256':'a'*64}}],
            'predicate':{'buildType':'tekton.dev/v1/PipelineRun','builder':{'id':'https://tekton.dev/chains/v2'},
              'buildConfig':{'tasks':[task]},'materials':[{'uri':'oci://localhost/ai-sdlc-artifact-builder','digest':{'sha256':'d'*64}},
                {'uri':'git+https://github.com/os-santiago/homedir.git','digest':{'sha1':'b'*40}}]}}

class ProvenanceTests(unittest.TestCase):
    def test_exact_identity_and_duplicate_json_keys(self):
        provenance.bind_statement(statement(),EXPECTED,IMAGE)
        with self.assertRaises(ValueError):provenance.strict_json(b'{"subject":[],"subject":[]}')
        with self.assertRaises(ValueError):provenance.verified_statement(b'{}',EXPECTED,IMAGE)

    def test_changed_subject_source_definition_run_builder_and_duplicates(self):
        for case in ('digest','source','definition','run','builder','image','material','result-duplicate','extra-step','extra-argument'):
            with self.subTest(case=case):
                value=statement();p=value['predicate'];task=p['buildConfig']['tasks'][0]
                if case=='digest':value['subject'][0]['digest']['sha256']='f'*64
                if case in ('source','definition','run'):
                    key={'source':'CHAINS-GIT_COMMIT','definition':'DEFINITION_DIGEST','run':'RUN_UID'}[case]
                    next(x for x in task['results'] if x['name']==key)['value']='false'
                if case=='builder':p['builder']['id']='https://untrusted.example'
                if case=='image':task['steps'][0]['environment']['image']='oci://wrong@sha256:'+'f'*64
                if case=='material':p['materials'][1]['digest']['sha1']='f'*40
                if case=='result-duplicate':task['results'].append(copy.deepcopy(task['results'][0]))
                if case=='extra-step':task['steps'].append(copy.deepcopy(task['steps'][0]))
                if case=='extra-argument':task['steps'][0]['arguments'].append('--extra=bad')
                with self.assertRaises(ValueError):provenance.bind_statement(value,EXPECTED,IMAGE)

    def test_saferus_source_destination_and_recipe_are_bound(self):
        expected = dict(EXPECTED, IMAGE_URL='registry-service.kind-registry/ai-sdlc-fixtures/saferus',
                        **{'CHAINS-GIT_URL':'https://github.com/os-santiago/saferus'})
        value = statement()
        value['subject'][0]['name'] = expected['IMAGE_URL']
        task = value['predicate']['buildConfig']['tasks'][0]
        task['results'] = [{'name':k,'type':'string','value':v} for k,v in expected.items()]
        task['steps'][0]['arguments'] = ['--recipe=ai-sdlc.saferus-go-static/v1','--input=/artifacts/output',
            '--output='+expected['IMAGE_URL']+':123456abcdef','--source-sha='+expected['CHAINS-GIT_COMMIT'],
            '--definition-digest='+expected['DEFINITION_DIGEST'],'--run-uid=run-123','--timeout=8m']
        value['predicate']['materials'][1]['uri'] = 'git+'+expected['CHAINS-GIT_URL']+'.git'
        provenance.bind_statement(value, expected, IMAGE)
        for mode in ('recipe','input','source','destination'):
            changed = copy.deepcopy(value); trusted = dict(expected)
            args = changed['predicate']['buildConfig']['tasks'][0]['steps'][0]['arguments']
            if mode == 'recipe': args[0] = '--recipe=ai-sdlc.quarkus-jvm/v1'
            if mode == 'input': args[1] = '--input=/artifacts/source/quarkus-app/target/quarkus-app'
            if mode == 'source': trusted['CHAINS-GIT_URL'] = EXPECTED['CHAINS-GIT_URL']
            if mode == 'destination': trusted['IMAGE_URL'] = EXPECTED['IMAGE_URL']
            with self.assertRaises(ValueError): provenance.bind_statement(changed,trusted,IMAGE)

    @unittest.skipUnless(shutil.which('openssl'),'OpenSSL required for actual synthetic signature negatives')
    def test_valid_synthetic_signature_cannot_authorize_false_source_or_digest(self):
        # These are newly generated test keys, never the actual Chains private
        # key. Cryptographic validity and input truth are separate assertions.
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);key=root/'key';pub=root/'pub';message=root/'pae';sig=root/'sig'
            def run(args):return subprocess.run(['openssl']+args,capture_output=True,timeout=5)
            self.assertEqual(run(['ecparam','-name','prime256v1','-genkey','-noout','-out',str(key)]).returncode,0)
            self.assertEqual(run(['pkey','-in',str(key),'-pubout','-out',str(pub)]).returncode,0)
            for case in ('source','digest'):
                value=statement()
                if case=='digest':value['subject'][0]['digest']['sha256']='f'*64
                else:next(r for r in value['predicate']['buildConfig']['tasks'][0]['results'] if r['name']=='CHAINS-GIT_COMMIT')['value']='f'*40
                payload=json.dumps(value,separators=(',',':')).encode();kind=b'application/vnd.in-toto+json'
                pae=b'DSSEv1 '+str(len(kind)).encode()+b' '+kind+b' '+str(len(payload)).encode()+b' '+payload;message.write_bytes(pae)
                self.assertEqual(run(['dgst','-sha256','-sign',str(key),'-out',str(sig),str(message)]).returncode,0)
                self.assertEqual(run(['dgst','-sha256','-verify',str(pub),'-signature',str(sig),str(message)]).returncode,0)
                envelope=json.dumps({'payloadType':kind.decode(),'payload':base64.b64encode(payload).decode(),'signatures':[{'sig':base64.b64encode(sig.read_bytes()).decode()}]}).encode()
                with self.assertRaises(ValueError):provenance.verified_statement(envelope,EXPECTED,IMAGE)
                message.write_bytes(pae+b'tampered')
                self.assertNotEqual(run(['dgst','-sha256','-verify',str(pub),'-signature',str(sig),str(message)]).returncode,0)

if __name__=='__main__':unittest.main()
