import copy
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock,patch
import uuid

import release_runtime_install as release

class ReleaseInstallTests(unittest.TestCase):
    def test_publication_requires_exact_revision_digest_and_closed_fields(self):
        lock={'image':{'tag':'a'*40}}
        selected={'version':'ai-sdlc.release-images/v1','revision':'a'*40,'deployerDigest':'sha256:'+'b'*64}
        with tempfile.TemporaryDirectory() as tmp:
            target,images=Path(tmp)/'target',Path(tmp)/'images';target.write_text('{}')
            browser=Path(tmp)/'browser';browser.write_text('{}')
            images.write_text(json.dumps(selected));self.assertEqual(release.inputs(target,images,lock,browser)['images'],selected)
            for key,value in [('revision','c'*40),('deployerDigest','latest'),('version','v2'),('url','https://foreign.invalid')]:
                images.write_text(json.dumps(dict(selected,**{key:value})))
                with self.assertRaises(ValueError):release.inputs(target,images,lock,browser)

    def fixture(self):
        target={'namespace':'ai-sdlc-production','deployment':'saferus-console','service':'saferus-console','pullSecret':'ai-sdlc-release-reader'}
        objects={}
        for kind,name,field in [('namespace','kube-system','clusterUID'),('namespace','ai-sdlc-production','namespaceUID'),('secret','ai-sdlc-release-reader','pullSecretUID'),('secret','saferus-console-tls','tlsSecretUID')]:
            uid=str(uuid.uuid4());target[field]=uid
            obj={'metadata':{'name':name,'uid':uid}}
            if kind=='secret':obj.update(type='kubernetes.io/dockerconfigjson' if field=='pullSecretUID' else 'kubernetes.io/tls',immutable=True,data={'private':'fixture-must-not-be-in-compiler'})
            objects[(kind,name)]=obj
        for kind in ('deployment','service'):objects[(kind,'saferus-console')]={'kind':kind,'metadata':{'name':'saferus-console'}}
        lock={'image':{'repository':'quay.io/os-santiago/ai-sdlc'},'migrator':{'digest':'sha256:'+'a'*64}}
        approved={'target':target,'browser':{'version':'fixture-public-profile'},'images':{'deployerDigest':'sha256:'+'b'*64}}
        cfg=Mock()
        def run(args,**kwargs):
            self.assertEqual(args[1],'get');self.assertTrue(kwargs['capture_output']);return SimpleNamespace(stdout=json.dumps(objects[(args[2],args[3])]))
        compiled={'version':'ai-sdlc.release-installation/v1','target':target,'targetSHA256':'c'*64,'trustSHA256':'d'*64}
        return objects,lock,approved,cfg,run,compiled

    def test_configuration_is_read_only_and_compiler_receives_no_private_secret(self):
        objects,lock,approved,cfg,run,compiled=self.fixture()
        with patch.object(release.build_runtime_install,'component_call',return_value=compiled) as compiler, patch.object(release.browser_profile_install,'configure',return_value='{}') as browser:
            actual=release.configure(run,['kubectl'],'podman',lock,approved,{},cfg)
        browser.assert_called_once();self.assertEqual(actual['browserProfileJSON'],'{}')
        self.assertTrue(actual['enabled']);self.assertEqual(json.loads(actual['targetJSON']),approved['target'])
        payload=compiler.call_args.args[-1]
        self.assertEqual(set(payload),{'target','trust','deployment','service'})
        self.assertNotIn('fixture-must-not-be-in-compiler',json.dumps(payload))
        cfg.bind_source.assert_called_once();self.assertNotIn('fixture-must-not-be-in-compiler',str(cfg.bind_source.call_args))

    def test_replaced_or_mutable_credentials_and_foreign_scope_rejected_before_compiler(self):
        for fault in ('uid','mutable','type','owner','deleting','scope'):
            objects,lock,approved,cfg,run,compiled=self.fixture();secret=objects[('secret','ai-sdlc-release-reader')]
            if fault=='uid':secret['metadata']['uid']=str(uuid.uuid4())
            elif fault=='mutable':secret['immutable']=False
            elif fault=='type':secret['type']='Opaque'
            elif fault=='owner':secret['metadata']['ownerReferences']=[{'uid':'foreign'}]
            elif fault=='deleting':secret['metadata']['deletionTimestamp']='now'
            else:approved['target']['namespace']='foreign'
            with self.subTest(fault=fault),patch.object(release.build_runtime_install,'component_call') as compiler:
                with self.assertRaises(ValueError):release.configure(run,['kubectl'],'podman',lock,approved,{},cfg)
                compiler.assert_not_called();cfg.bind_source.assert_not_called()

    def test_compiler_cannot_replace_approved_target(self):
        objects,lock,approved,cfg,run,compiled=self.fixture();compiled=copy.deepcopy(compiled);compiled['target']['namespaceUID']=str(uuid.uuid4())
        with patch.object(release.build_runtime_install,'component_call',return_value=compiled):
            with self.assertRaises(ValueError):release.configure(run,['kubectl'],'podman',lock,approved,{},cfg)
        cfg.bind_source.assert_not_called()
