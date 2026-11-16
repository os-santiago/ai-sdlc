import json
import unittest
import saferus_build_fixture as fixture

class SaferusBuildRecipeTests(unittest.TestCase):
    def test_fixed_builder_has_no_publish_credential_or_api_token(self):
        build=fixture.build_pipeline([{'host':'10.96.0.1','port':443}])
        step=build['tasks'][0]['taskSpec']['steps'][0]
        script=step['script']
        self.assertIn('export GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly',script)
        self.assertIn('test "$(go env GOVERSION)" = go1.27.4',script)
        self.assertIn('go test -race ./...',script)
        self.assertIn('CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build',script)
        self.assertNotIn('export HOME=',script)
        self.assertNotIn('secret',json.dumps(build['tasks'][0]['taskSpec']['volumes']))
        self.assertTrue(step['securityContext']['readOnlyRootFilesystem'])
        run=fixture.pipeline_run('fixture','build','build',build)
        self.assertFalse(run['spec']['taskRunTemplate']['podTemplate']['automountServiceAccountToken'])

    def test_scratch_publisher_cannot_use_public_egress_or_execute_source(self):
        policy=fixture.policies('fixture')[1]
        self.assertEqual(policy['spec']['egress'],[fixture.PUBLIC_EGRESS[0],fixture.REGISTRY_EGRESS])
        publisher=fixture.publish_pipeline('example/image@sha256:'+'a'*64,'sha256:'+'b'*64,'OUTPUT')
        task=publisher['tasks'][0]['taskSpec'];step=task['steps'][0]
        self.assertEqual(step['command'],['/component'])
        self.assertNotIn('script',step)
        self.assertIn('--recipe=ai-sdlc.saferus-go-static/v1',step['args'])
        self.assertIn('--input=/artifacts/output',step['args'])
        artifacts=next(v for v in task['volumes'] if v['name']=='artifacts')
        self.assertTrue(artifacts['persistentVolumeClaim']['readOnly'])

    def test_network_gate_rejects_untrusted_targets_and_digest_binds_them(self):
        for target in ({'host':'127.0.0.1; evil','port':443},{'host':'::1','port':443},{'host':'10.0.0.1','port':65536}):
            with self.assertRaises(ValueError): fixture.build_pipeline([target])
        image='quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64
        self.assertNotEqual(fixture.definition_digest(image),fixture.definition_digest(image,[{'host':'10.96.0.1','port':443}]))

if __name__=='__main__': unittest.main()
