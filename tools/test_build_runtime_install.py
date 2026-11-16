"""Verify approved desired state, read-only preflight and secret-safe cleanup."""
import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import build_runtime_install as build
import install_config
import install
import konflux_runtime_install

IMAGE = 'quay.io/os-santiago/ai-sdlc@sha256:' + 'a'*64
LOCK = {'image': {'repository': 'quay.io/os-santiago/ai-sdlc', 'tag': 'b'*40}, 'migrator': {'digest': 'sha256:'+'a'*64}}


class BuildInstallTests(unittest.TestCase):
    def test_bootstrap_diagnostics_expose_only_finite_boundary_categories(self):
        for operation,stderr,expected in (
            ('inspect-empty-bootstrap','timestamp initial database inspection denied: history/outbox\n','boundary: history'),
            ('inspect-empty-bootstrap','initial database inspection denied: primary-authority','boundary: primary-authority'),
            ('inspect-empty-bootstrap','initial database inspection denied: private-sentinel','preflight rejected'),
            ('inspect-empty-bootstrap','driver failed postgres://private-sentinel:secret-sentinel@host/database','preflight rejected'),
            ('compile-build-installation','initial database inspection denied: connection','preflight rejected'),
        ):
            def run(args,**kwargs):
                if args[1]=='run':return subprocess.CompletedProcess(args,1,'',stderr)
                if args[1]=='inspect':return subprocess.CompletedProcess(args,1,'','')
                return subprocess.CompletedProcess(args,0,'','')
            with self.subTest(operation=operation,expected=expected),patch.object(build.subprocess,'run',side_effect=run):
                with self.assertRaisesRegex(ValueError,expected) as caught:
                    build.component_call('podman',IMAGE,operation,{})
                self.assertNotIn('private-sentinel',str(caught.exception))
                self.assertNotIn('secret-sentinel',str(caught.exception))

    def bundle(self):
        return {'version':'ai-sdlc.build-desired/v1','revision':'b'*40,'controllerDigest':'sha256:'+'c'*64,
                'profile':{'clusterUID':'original','publisherImage':IMAGE},'slots':[{'original':'slot'}],'issueMaxAgeSeconds':60}

    def test_bundle_is_bounded_and_bound_to_reviewed_revision(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'build.json';path.write_text(json.dumps(self.bundle()))
            self.assertEqual(build.load_bundle(path,LOCK),self.bundle())
            for key,value in [('version','v2'),('revision','d'*40),('controllerDigest','latest'),('slots',[]),('issueMaxAgeSeconds',True),('issueMaxAgeSeconds',301),('unknown',1)]:
                changed=self.bundle();changed[key]=value;path.write_text(json.dumps(changed))
                with self.assertRaises(ValueError):build.load_bundle(path,LOCK)
            changed=self.bundle();changed['profile']['publisherImage']='localhost/ai-sdlc-artifact-builder@sha256:'+'a'*64
            path.write_text(json.dumps(changed))
            with self.assertRaises(ValueError):build.load_bundle(path,LOCK)
            path.write_text(' '*131073)
            with self.assertRaises(ValueError):build.load_bundle(path,LOCK)
            alias=Path(directory)/'alias';alias.symlink_to(path)
            with self.assertRaises(ValueError):build.load_bundle(alias,LOCK)

    def test_private_inputs_only_stdin_and_lost_reply_cleanup_uses_original_identity(self):
        calls=[];nonce='e'*48;cid='f'*64
        def run(args,**kwargs):
            calls.append((args,kwargs))
            if args[1]=='run': raise subprocess.TimeoutExpired(args,150)
            if args[1]=='inspect':return subprocess.CompletedProcess(args,0,json.dumps([{'Id':cid,'Config':{'Labels':{'ai-sdlc.build-install':nonce}}}]))
            return subprocess.CompletedProcess(args,0,'')
        with patch.object(build.secrets,'token_hex',return_value=nonce),patch.object(build.subprocess,'run',side_effect=run):
            with self.assertRaises(subprocess.TimeoutExpired):build.component_call('podman',IMAGE,'observe-build-profile',{'private':'synthetic-secret'},'1'*64)
        command,options=calls[0]
        self.assertNotIn('synthetic-secret',' '.join(command));self.assertIn('synthetic-secret',options['input'])
        self.assertIn('--read-only',command);self.assertIn('--cap-drop=ALL',command)
        self.assertEqual(command[command.index('--network')+1],'container:'+'1'*64)
        self.assertEqual(calls[-1][0],['podman','rm','-f','-v',cid])

    def test_cleanup_refuses_another_container_and_does_not_print_private_error(self):
        for changed in ('nonce','id'):
            cid='f'*64;nonce='e'*48
            record={'Id':cid,'Config':{'Labels':{'ai-sdlc.build-install':nonce}}}
            if changed=='nonce':record['Config']['Labels']['ai-sdlc.build-install']='other'
            else:record['Id']='name'
            def run(args,**kwargs):
                if args[1]=='run':return subprocess.CompletedProcess(args,1,'private-output','private-error')
                if args[1]=='inspect':return subprocess.CompletedProcess(args,0,json.dumps([record]))
                self.fail('must not remove an unproven container')
            with patch.object(build.secrets,'token_hex',return_value=nonce),patch.object(build.subprocess,'run',side_effect=run):
                with self.assertRaisesRegex(ValueError,'Refusing cleanup'):build.component_call('podman',IMAGE,'compile-build-installation',{})

    def test_preflight_only_observes_and_rejects_profile_drift(self):
        bundle=self.bundle();bundle['profile'].update(namespaceUID='ns',publisherImage=IMAGE,deniedTargets=[])
        calls=[]
        def run(args,**kwargs):
            calls.append(args)
            if args[0]=='podman':value=[{'Id':'1'*64,'Config':{'Labels':{'io.x-k8s.kind.cluster':'owned'}}}]
            elif args[1:3]==['config','view']:value={'clusters':[{'cluster':{'certificate-authority-data':'ca'}}],'users':[{'user':{'client-certificate-data':'cert','client-key-data':'private'}}]}
            else:value={'data':{'trust.json':'{}'}}
            return subprocess.CompletedProcess(args,0,json.dumps(value))
        with patch.object(build,'component_call',return_value=bundle['profile']) as component:
            build.preflight(run,['kubectl'],'podman','owned',LOCK,bundle)
            self.assertEqual(component.call_args.args[2],'observe-build-profile')
            self.assertEqual(component.call_args.args[-1],'1'*64)
        with patch.object(build,'component_call',return_value=dict(bundle['profile'],clusterUID='changed')):
            with self.assertRaisesRegex(ValueError,'differs'):build.preflight(run,['kubectl'],'podman','owned',LOCK,bundle)
        self.assertFalse(any(set(c)&{'apply','create','delete','patch','exec'} for c in calls))

    def test_frozen_enabled_build_cannot_be_removed_or_have_cleanup_privileges_revoked(self):
        def run(args, **kwargs):
            return subprocess.CompletedProcess(args,0,json.dumps({'data':{'AI_SDLC_RUNTIME_MODE':'maintenance','AI_SDLC_BUILD':'true'}}))
        with self.assertRaisesRegex(ValueError,'containment-preserving'):
            konflux_runtime_install.require_frozen(run,['kubectl'])

    def test_status_config_does_not_require_build_bundle_or_activate_capability(self):
        base={'apiVersion':'ai-sdlc.io/local-install/v1','kind':'LocalInstallation','cluster':'owned','provider':'podman','repository':'os-santiago/saferus','environment':'ai-sdlc-production','profile':'verification','images':str(install.LOCAL/'images.lock.json'),'platform':'konflux','buildBundle':'missing-build.json','independentReview':True,'publication':True,'merge':True}
        class ReachedTools(Exception): pass
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'install.json';path.write_text(json.dumps(base))
            with patch.object(install,'tools',side_effect=ReachedTools), patch.object(build,'load_bundle') as load:
                with self.assertRaises(ReachedTools):install.main(['status','--config',str(path)])
                load.assert_not_called()

    def test_configuration_requires_all_reviewed_capabilities_and_resolves_relative_bundle(self):
        base={'apiVersion':'ai-sdlc.io/local-install/v1','kind':'LocalInstallation','cluster':'owned','provider':'podman','repository':'os-santiago/saferus','environment':'ai-sdlc-production','profile':'verification','images':'images.json','platform':'konflux','buildBundle':'build.json','independentReview':True,'publication':True,'merge':True}
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'install.json';path.write_text(json.dumps(base))
            self.assertEqual(install_config.load(path)[0]['build_bundle'],Path(directory)/'build.json')
            for key,value in [('platform','kind'),('profile','maintenance'),('repository','os-santiago/homedir'),('environment','real-production'),('merge',False),('publication',False),('independentReview',False)]:
                changed=dict(base);changed[key]=value;path.write_text(json.dumps(changed))
                with self.assertRaises(ValueError):install_config.load(path)

if __name__=='__main__':unittest.main()
