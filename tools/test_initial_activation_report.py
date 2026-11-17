import copy
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock,patch
import initial_activation as activation

class ActivationReportTests(unittest.TestCase):
    def fixture(self,path):
        a=object.__new__(activation.Activation)
        a.state=path;a.config=SimpleNamespace(cluster='original',provider='podman');a.original_hash='a'*64
        a.record={'completed':'active','transition':None,'expiresAt':1}
        images={'image':{'repository':'quay.io/os-santiago/ai-sdlc','tag':'b'*40}}
        observation={'clusterUID':'original-cluster','namespaceUID':'original-namespace'}
        a.original={'target':{'images':images,'observed':observation}}
        description={'images':images,'values':{'authority':{'policyJSON':json.dumps({'repository':'os-santiago/saferus','environment':'ai-sdlc-production'})}}}
        description['values'].update(release={'digest':'sha256:'+'d'*64},build={'digest':'sha256:'+'e'*64,
            'configJSON':json.dumps({'profile':{'publisherImage':'quay.io/os-santiago/ai-sdlc@sha256:'+'f'*64}})})
        a.original['target']['values']=copy.deepcopy(description['values'])
        a.describe=Mock(return_value=description);a.observe=Mock(return_value=observation)
        a.begin=Mock();a.resolve=Mock();a.apply=Mock();a.save=Mock()
        prior={'configuration':'prepared-paused','cluster':'original','platform':'konflux','repository':'os-santiago/saferus',
            'environment':'ai-sdlc-production','images':images}
        (path/'installation.json').write_text(json.dumps(prior))
        (path/'prepared-configuration.json').write_text('original preparation')
        (path/'initial-activation.json').write_text('original activation')
        return a

    def test_ready_snapshot_after_fresh_observation_preserves_authority_files(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory);a=self.fixture(path);before=copy.deepcopy(a.record);a.publish_report()
            report=json.loads((path/'installation.json').read_text())
            self.assertTrue(report['installationReady']);self.assertEqual(report['namespace'],'ai-sdlc-runtime')
            self.assertEqual(report['clusterUID'],'original-cluster');self.assertEqual(report['images'],a.original['target']['images'])
            self.assertEqual(report['deployerImage'],'quay.io/os-santiago/ai-sdlc@sha256:'+'d'*64)
            self.assertEqual(report['publisherImage'],'quay.io/os-santiago/ai-sdlc@sha256:'+'f'*64)
            self.assertGreater(report['observedAt'],0);self.assertFalse(report['autonomousEndToEndValidated'])
            self.assertEqual(a.record,before);a.observe.assert_called_once_with('active')
            self.assertEqual((path/'prepared-configuration.json').read_text(),'original preparation')
            self.assertEqual((path/'initial-activation.json').read_text(),'original activation')

    def test_paused_partial_not_ready_replaced_identity_or_foreign_report_cannot_publish(self):
        for fault in ('prepared','warming','armed','transition','not-ready','identity','scope','images'):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as directory:
                path=Path(directory);a=self.fixture(path)
                if fault in activation.PHASES:a.record['completed']=fault
                elif fault=='transition':a.record['transition']={'from':'armed','to':'active'}
                elif fault=='not-ready':a.observe.side_effect=ValueError('not ready')
                elif fault=='identity':a.observe.return_value={'clusterUID':'replacement','namespaceUID':'original-namespace'}
                elif fault=='scope':
                    prior=json.loads((path/'installation.json').read_text());prior['repository']='foreign/repository';(path/'installation.json').write_text(json.dumps(prior))
                elif fault=='images':a.describe.return_value=copy.deepcopy(a.describe.return_value);a.describe.return_value['images']['image']['tag']='c'*40
                before=(path/'installation.json').read_bytes()
                with self.assertRaises(ValueError):a.publish_report()
                self.assertEqual((path/'installation.json').read_bytes(),before)

    def test_report_failure_retry_only_reobserves_active_without_replaying_helm(self):
        for after_write in (False,True):
            with self.subTest(after_write=after_write),tempfile.TemporaryDirectory() as directory:
                path=Path(directory);a=self.fixture(path);write=activation.platform.write_record
                def fail(*args,**kwargs):
                    if after_write:write(*args,**kwargs)
                    raise OSError('report write acknowledgement lost')
                with patch.object(activation.platform,'write_record',side_effect=fail):
                    with self.assertRaises(OSError):a.activate()
                self.assertEqual(a.record['completed'],'active');a.apply.assert_not_called();a.save.assert_not_called()
                a.activate();a.apply.assert_not_called();a.save.assert_not_called()
                self.assertTrue(json.loads((path/'installation.json').read_text())['installationReady'])
                for call in a.observe.call_args_list:self.assertEqual(call.args,('active',));self.assertFalse(call.kwargs.get('empty',False))

    def test_status_never_repairs_or_writes_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory);a=self.fixture(path);before=(path/'installation.json').read_bytes()
            with patch.object(activation.platform,'write_record') as write:a.activate(read_only=True);write.assert_not_called()
            self.assertEqual((path/'installation.json').read_bytes(),before)
            a.apply.assert_not_called();a.save.assert_not_called()

if __name__=='__main__':unittest.main()
