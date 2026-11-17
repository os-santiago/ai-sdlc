import copy
import json
from types import SimpleNamespace
import unittest
from unittest.mock import Mock,patch
import browser_profile_install as browser


class BrowserProfileInstallationTests(unittest.TestCase):
    def fixture(self):
        profile={'version':'ai-sdlc.saferus-browser-profile/v1','nodeName':'original-node','clusterUID':'original-cluster'}
        target={'clusterUID':'original-cluster'}
        lock={'image':{'repository':'quay.io/os-santiago/ai-sdlc'},'migrator':{'digest':'sha256:'+'a'*64}}
        config=Mock()
        calls=[]
        def run(args,**kwargs):
            calls.append(args)
            self.assertEqual(args[1],'get');self.assertNotIn('secret',args)
            return SimpleNamespace(stdout=json.dumps({'kind':'PublicObservation','metadata':{'uid':'fixture'}}))
        compiled={'version':'ai-sdlc.browser-installation/v1','profile':profile,'profileSHA256':'b'*64,'targetSHA256':'c'*64}
        return profile,target,lock,config,run,compiled,calls

    def test_only_original_public_scope_reaches_offline_compiler(self):
        profile,target,lock,config,run,compiled,calls=self.fixture()
        with patch.object(browser.build_runtime_install,'component_call',return_value=compiled) as compiler:
            result=browser.configure(run,['kubectl'],'podman',lock,profile,target,config)
        self.assertEqual(json.loads(result),profile)
        self.assertEqual(len(calls),7)
        self.assertEqual(config.guard.call_count,14)
        payload=compiler.call_args.args[-1]
        self.assertEqual(set(payload),{'profile','target','cluster','namespace','node','runtimeClass','ca','policies','consoleIngress'})
        config.bind_source.assert_called_once_with('browser-original-profile',compiled)

    def test_scope_cannot_choose_external_argument_or_cluster(self):
        for change in ({'nodeName':'--server=https://foreign.invalid'},{'nodeName':'../node'},{'clusterUID':'replacement'},{'version':'foreign'}):
            profile,target,lock,config,run,compiled,calls=self.fixture();profile.update(change)
            with patch.object(browser.build_runtime_install,'component_call') as compiler:
                with self.assertRaises(ValueError):browser.configure(run,['kubectl'],'podman',lock,profile,target,config)
                compiler.assert_not_called();self.assertEqual(calls,[]);config.bind_source.assert_not_called()

    def test_compiler_cannot_change_profile_or_expand_response(self):
        for change in ('profile','version','hash','extra'):
            profile,target,lock,config,run,compiled,calls=self.fixture();compiled=copy.deepcopy(compiled)
            if change=='profile':compiled['profile']['nodeName']='replacement'
            if change=='version':compiled['version']='foreign'
            if change=='hash':compiled['profileSHA256']='alias'
            if change=='extra':compiled['private']='unexpected'
            with patch.object(browser.build_runtime_install,'component_call',return_value=compiled):
                with self.assertRaises(ValueError):browser.configure(run,['kubectl'],'podman',lock,profile,target,config)
            config.bind_source.assert_not_called()

    def test_failed_observation_never_becomes_compiler_success(self):
        profile,target,lock,config,run,compiled,calls=self.fixture()
        def failed(*args,**kwargs):raise ValueError('read failed')
        with patch.object(browser.build_runtime_install,'component_call') as compiler:
            with self.assertRaises(ValueError):browser.configure(failed,['kubectl'],'podman',lock,profile,target,config)
            compiler.assert_not_called();config.bind_source.assert_not_called()
