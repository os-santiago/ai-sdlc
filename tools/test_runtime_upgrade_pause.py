"""Pause safety boundaries: no replay, no authority extension, no E2E claim."""
import copy
import json
from pathlib import Path
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import initial_activation
import runtime_upgrade_pause as pause
import test_initial_activation as activation_fixture
import test_release_helm


class PauseTests(unittest.TestCase):
    def test_real_chart_pauses_all_producers_without_migrating_or_extending_policy(self):
        source=activation_fixture.source_values()
        active=initial_activation.phase_values(source,'active',activation_fixture.compiler(source))
        original=copy.deepcopy(active)
        target=pause.pause_values(active,activation_fixture.compiler(active))
        self.assertEqual(active,original)
        self.assertTrue(target['runtime']['paused']);self.assertTrue(target['runtime']['intakePaused'])
        rendered=test_release_helm.ReleaseChartTests().render(target)
        deployments=[d for d in rendered if d['kind']=='Deployment']
        self.assertEqual(len(deployments),10)
        self.assertEqual({d['spec']['replicas'] for d in deployments},{0})
        self.assertFalse(any(d['kind']=='Job' for d in rendered))
        for section,key in (('authority','policyJSON'),('build','outerPolicyJSON')):
            expected=json.loads(active[section][key]);expected['freeze']=True
            self.assertEqual(json.loads(target[section][key]),expected)
        for key in set(active)-{'runtime','authority','build'}:
            self.assertEqual(target[key],active[key])

    def test_compiler_cannot_extend_authority(self):
        source=activation_fixture.source_values()
        active=initial_activation.phase_values(source,'active',activation_fixture.compiler(source))
        def changed(request):
            value=activation_fixture.compiler(active)(request)
            value['issueMaxAgeSeconds']+=1
            return value
        with self.assertRaises(ValueError):pause.pause_values(active,changed)

    def test_acknowledgement_loss_keeps_intent_and_cannot_replay(self):
        record={'phase':'planned','expiresAt':time.time()+60};saved=[]
        fence=Mock();apply=Mock(side_effect=OSError('ack lost'));observe=Mock()
        with self.assertRaises(OSError):
            pause.pause_once(record,fence,observe,observe,apply,lambda value:saved.append(copy.deepcopy(value)))
        self.assertEqual(saved[-1]['phase'],'pausing');fence.release.assert_not_called()
        with self.assertRaises(ValueError):pause.pause_once(record,fence,observe,observe,apply,Mock())
        self.assertEqual(apply.call_count,1)

    def test_source_drift_or_fence_loss_prevents_helm(self):
        for fault in ('source','fence'):
            with self.subTest(fault=fault):
                record={'phase':'planned','expiresAt':time.time()+60};fence=Mock();source=Mock();apply=Mock();save=Mock()
                if fault=='source':source.side_effect=ValueError('identity changed')
                else:fence.check.side_effect=ValueError('backend gone')
                with self.assertRaises(ValueError):pause.pause_once(record,fence,source,Mock(),apply,save)
                apply.assert_not_called();save.assert_not_called()

    def test_release_failure_cannot_publish_paused(self):
        record={'phase':'planned','expiresAt':time.time()+60};fence=Mock()
        fence.release.side_effect=ValueError('release unknown')
        with self.assertRaises(ValueError):pause.pause_once(record,fence,Mock(),Mock(),Mock(),Mock())
        self.assertEqual(record['phase'],'pausing')

    def test_convergence_reobserves_without_replaying_effect(self):
        record={'phase':'planned','expiresAt':time.time()+60};fence=Mock();apply=Mock()
        target=Mock(side_effect=[ValueError('old pod remains'),None])
        with patch.object(pause.time,'sleep'):
            pause.pause_once(record,fence,Mock(),target,apply,Mock())
        self.assertEqual(target.call_count,2);apply.assert_called_once();fence.release.assert_called_once()
        self.assertEqual(record['phase'],'pausing')

    def fixture(self,path):
        item=object.__new__(pause.Pause);item.config=SimpleNamespace(state_dir=path)
        item.activation_hash='a'*64;item.record={'phase':'paused'};item.target=Mock()
        prior={'activationSHA256':item.activation_hash,'configuration':'active','installationReady':True}
        (path/'installation.json').write_text(json.dumps(prior))
        return item

    def test_report_retry_reobserves_without_helm_and_status_is_read_only(self):
        with tempfile.TemporaryDirectory() as directory:
            item=self.fixture(Path(directory));write=pause.platform.write_record
            def lost(*args):write(*args);raise OSError('ack lost')
            with patch.object(pause.platform,'write_record',side_effect=lost):
                with self.assertRaises(OSError):item.run()
            result=item.run()
            self.assertFalse(result['autonomousEndToEndValidated']);self.assertFalse(result['upgradeAuthorized'])
            report=json.loads((Path(directory)/'installation.json').read_text())
            self.assertFalse(report['installationReady']);self.assertTrue(report['runtimePaused'])
            with patch.object(pause.platform,'write_record') as writer:item.status();writer.assert_not_called()

    def test_incomplete_intent_cannot_publish_or_replay(self):
        with tempfile.TemporaryDirectory() as directory:
            item=self.fixture(Path(directory));item.record['phase']='pausing'
            with patch.object(pause.platform,'write_record') as writer:
                with self.assertRaises(ValueError):item.run()
                writer.assert_not_called();item.target.assert_not_called()

if __name__=='__main__':unittest.main()
