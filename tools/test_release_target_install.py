import base64
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
import uuid
from unittest.mock import Mock,patch

import release_target_install as target

class TargetInstallTests(unittest.TestCase):
    def test_unprepared_runtime_is_rejected_before_observation_or_writes(self):
        value=object.__new__(target.Preparation);value.configuration=Mock(record={'phase':'preparing'});value.get=Mock()
        with self.assertRaises(ValueError):value.prepare()
        value.configuration.guard.assert_not_called();value.get.assert_not_called()

    def test_status_cannot_record_intent_for_incomplete_preparation(self):
        for record in (None,{}, {'deploymentPatch':None},{'preparedTargetSHA256':'a'*64,'deploymentPatch':{'response':None}}):
            value=object.__new__(target.Preparation);value.guard=Mock();value.record=record;value.get=Mock()
            with patch.object(target.platform,'write_record') as write:
                with self.assertRaises(ValueError):value.prepare(read_only=True)
                write.assert_not_called();value.get.assert_not_called()

    def test_original_reader_copy_never_grants_a_new_registry(self):
        value=object.__new__(target.Preparation);value.guard=Mock();value.state=Path('/unused');value.owner={'clusterUID':'original-cluster'};value.kubectl=[];value.binaries={}
        credential={'username':'provenance','password':'a'*48}
        secret={'metadata':{'uid':'original-reader'},'type':'Opaque','immutable':True,'data':{'credential.json':base64.b64encode(json.dumps(credential).encode()).decode()}}
        value.get=Mock(return_value=secret)
        with patch.object(target.managed_registry_install,'read_record',return_value={'clusterUID':'original-cluster'}),patch.object(target.managed_registry_install,'Installer') as registry:
            uid,desired=value.source_credentials()
            registry.return_value.fence.assert_called_once_with(cold=False);registry.return_value.check_chart_objects.assert_called_once()
        self.assertEqual(uid,'original-reader');self.assertTrue(desired['immutable']);self.assertEqual(desired['metadata']['namespace'],'ai-sdlc-production')
        config=json.loads(base64.b64decode(desired['data']['.dockerconfigjson']))
        self.assertEqual(set(config['auths']),{'registry-service.kind-registry'})
        self.assertEqual(base64.b64decode(config['auths']['registry-service.kind-registry']['auth']).decode(),'provenance:'+credential['password'])

    def test_public_compiler_preflight_rejects_changed_baseline_and_non_hashes(self):
        value=object.__new__(target.Preparation);value.lock={'image':{'repository':'quay.io/os-santiago/ai-sdlc'},'migrator':{'digest':'sha256:'+'a'*64}};value.provider='podman';value.source={'baselineImage':'baseline'}
        good={'version':'ai-sdlc.release-specs/v1','baselineImage':'baseline','deploymentSpecSHA256':'a'*64,'serviceSpecSHA256':'b'*64}
        with patch.object(target.build_runtime_install,'component_call',return_value=good):self.assertEqual(value.describe({},{}),good)
        for key,data in [('baselineImage','other'),('deploymentSpecSHA256','bad'),('unknown','field')]:
            with patch.object(target.build_runtime_install,'component_call',return_value=dict(good,**{key:data})):
                with self.assertRaises(ValueError):value.describe({},{})

    def test_private_custody_directory_rejects_shared_or_symlink(self):
        with tempfile.TemporaryDirectory() as tmp:
            p=Path(tmp);target.private_directory(p)
            link=p/'link';link.symlink_to(p)
            with self.assertRaises(ValueError):target.private_directory(link)
            p.chmod(0o755)
            with self.assertRaises(ValueError):target.private_directory(p)


class NamespaceFenceTests(unittest.TestCase):
    def test_namespace_replaced_during_compiler_preflight_copies_no_credential(self):
        value=object.__new__(target.Preparation)
        cluster,namespace=str(uuid.uuid4()),str(uuid.uuid4())
        value.owner={'clusterUID':cluster,'namespaceUID':namespace};value.intent={'nonce':'a'*48}
        current={'metadata':{'uid':namespace,'annotations':{'saferus.io/creation-nonce':'a'*48,'saferus.io/cluster-uid':cluster}}}
        value.configuration=SimpleNamespace(record={'phase':'configured-paused'},guard=lambda **kw:{'clusterUID':cluster})
        value.record=None;value.ca='original-ca';value.source={'baselineImage':'original-baseline'};value.run=Mock()
        deployment={'spec':{'template':{'spec':{'containers':[{'image':'original-baseline'}]}}}}
        value.original=Mock(side_effect=lambda kind,name: {'type':'kubernetes.io/tls'} if kind=='Secret' else deployment)
        value.get=Mock(side_effect=lambda kind,name,namespace=None:current if kind=='namespace' else {'data':{'ca.crt':'original-ca'}})
        def compiler(*args):current['metadata']['uid']=str(uuid.uuid4());return {}
        value.describe=Mock(side_effect=compiler)
        with patch.object(target.platform,'write_record') as write:
            with self.assertRaisesRegex(ValueError,'namespace changed'):value.prepare()
            write.assert_not_called();value.run.assert_not_called()
    def test_namespace_deletion_and_changed_nonce_are_rejected(self):
        value=object.__new__(target.Preparation);cluster,namespace=str(uuid.uuid4()),str(uuid.uuid4())
        value.owner={'clusterUID':cluster,'namespaceUID':namespace};value.intent={'nonce':'a'*48}
        for fault in ('deleted','nonce','cluster'):
            current={'metadata':{'uid':namespace,'annotations':{'saferus.io/creation-nonce':'a'*48,'saferus.io/cluster-uid':cluster}}}
            if fault=='deleted':current['metadata']['deletionTimestamp']='now'
            else:current['metadata']['annotations']['saferus.io/'+('creation-nonce' if fault=='nonce' else 'cluster-uid')]='foreign'
            value.get=Mock(return_value=current)
            with self.assertRaises(ValueError):value.namespace()
