"""Negative controls for administrative browser installation authority."""
import copy
import json
from pathlib import Path
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock,patch

import browser_platform_install
import browser_runtime
import browser_runtime_upgrade
import browser_upgrade_chart
import browser_upgrade_targets
import browser_schema
import install
import prepared_resources


class TrustTests(unittest.TestCase):
    def test_only_reviewed_verifier_can_change_without_renewing_window(self):
        original={'verifierImage':'old','publisherImage':'publisher','approvedAt':10,'expiresAt':20,'registryCA':'ca','signerPublicKey':'key','scope':'kind-test-production'}
        approved=dict(original,verifierImage='new')
        browser_upgrade_targets.validate_trust(original,approved,'new')
        for field,value in [('publisherImage','other'),('approvedAt',11),('expiresAt',30),('registryCA','other'),('signerPublicKey','other'),('scope','other'),('verifierImage','unpublished')]:
            with self.subTest(field=field),self.assertRaises(ValueError):
                browser_upgrade_targets.validate_trust(original,dict(approved,**{field:value}),'new')
        for value in (None,{},dict(approved,extra='unsafe')):
            with self.assertRaises(ValueError):browser_upgrade_targets.validate_trust(original,value,'new')


class EffectGuards(unittest.TestCase):
    def test_paused_inventory_cannot_adopt_producers_or_new_generations(self):
        desired=[{'apiVersion':'apps/v1','kind':'Deployment','metadata':{'name':'component-'+str(i),'namespace':'ai-sdlc-runtime'},
            'spec':{'replicas':0,'template':{'metadata':{'labels':{}},'spec':{'containers':[{'name':'component','image':'fixed'}]}}}} for i in range(10)]
        actual=copy.deepcopy(desired);ids={}
        for i,obj in enumerate(actual):
            obj['metadata'].update(uid='00000000-0000-4000-8000-'+str(i).zfill(12),generation=2)
            ids[prepared_resources.key(obj)]=obj['metadata']['uid']
        pod={'apiVersion':'v1','kind':'Pod','metadata':{'name':'database','uid':'00000000-0000-4000-8000-111111111111'}}
        objects=actual+[pod];description={'documents':desired}
        check=lambda values,previous=None:browser_runtime_upgrade.paused_inventory(values,description,ids,pod['metadata']['uid'],previous)
        original=check(objects);self.assertEqual(check(objects,original),original)
        for change in ('replicas','uid','generation','template','pod','extra','status','deleting'):
            changed=copy.deepcopy(objects)
            if change=='replicas':changed[0]['spec']['replicas']=1
            elif change=='uid':changed[0]['metadata']['uid']='00000000-0000-4000-8000-222222222222'
            elif change=='generation':changed[0]['metadata']['generation']=3
            elif change=='template':changed[0]['spec']['template']['spec']['containers'][0]['image']='other'
            elif change=='pod':changed.append(copy.deepcopy(pod))
            elif change=='extra':changed.append({'apiVersion':'v1','kind':'Secret','metadata':{'name':'unexpected'}})
            elif change=='status':changed[0]['status']={'replicas':1}
            elif change=='deleting':changed[0]['metadata']['deletionTimestamp']='2026-11-17T12:00:00Z'
            with self.subTest(change=change),self.assertRaises(ValueError):check(changed,original)

    def item(self):
        item=object.__new__(browser_runtime_upgrade.BrowserUpgrade)
        obj={'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'ai-sdlc-browser-profile','namespace':'ai-sdlc-runtime'},'data':{'profile.json':'{}'}}
        item.record={'expiresAt':time.time()+60,'completed':'migrated','mode':'browser-evidence','transition':None,
            'resourceUIDs':{prepared_resources.key(obj):None},'targets':{'staged':{'documents':[obj]}}}
        item.config=Mock();item.config.read.return_value=dict(obj,metadata=dict(obj['metadata'],uid='foreign'))
        item.unchanged=Mock();item.observe=Mock();item.retained=Mock();item.begin=Mock()
        return item

    def test_identical_object_appearing_after_prepare_prevents_intent(self):
        item=self.item()
        with patch.object(browser_runtime_upgrade,'BrowserPreflight'),patch.object(browser_runtime_upgrade,'advance') as advance:
            with self.assertRaises(ValueError):item.run()
        advance.assert_not_called();self.assertIsNone(item.record['transition'])

    def test_identical_object_before_helm_prevents_rpc(self):
        item=self.item();command=Mock()
        def apply(phase,guarded):return guarded(['helm','upgrade'])
        with patch.object(browser_runtime_upgrade.ReviewerImageUpgrade,'apply',side_effect=apply):
            with self.assertRaises(ValueError):item.apply('staged',command)
        command.assert_not_called()

    def test_runtime_deadline_is_rechecked_after_first_read(self):
        guard=Mock();response=SimpleNamespace(stdout=json.dumps({'metadata':{'uid':'cluster'}}))
        with patch.object(browser_runtime.time,'time',side_effect=[0,11]),patch.object(browser_runtime.subprocess,'run',return_value=response) as command:
            with self.assertRaises(ValueError):browser_runtime.install(['kubectl'],'podman','fixture','cluster',guard=guard,expires_at=10)
        self.assertEqual(command.call_count,1)
        self.assertEqual(command.call_args.args[0],['kubectl','get','namespace','kube-system','-o','json'])
        self.assertEqual(guard.call_count,2)

    def test_expiry_during_guard_prevents_effect_and_caps_timeout(self):
        item=object.__new__(browser_platform_install.Platform)
        item.expires_at=10;item.guard=Mock();item.config=Mock()
        with patch.object(browser_platform_install.time,'time',return_value=11):
            with self.assertRaises(ValueError):item.effect(['kubectl','create'],timeout=20)
        item.config.base_run.assert_not_called()
        with patch.object(browser_platform_install.time,'time',return_value=7):
            item.effect(['kubectl','create'],timeout=20)
        self.assertEqual(item.config.base_run.call_args.kwargs['timeout'],3)

    def test_namespace_replacement_prevents_scope_create(self):
        item=object.__new__(browser_platform_install.Platform)
        item.expires_at=time.time()+60;item.outer_guard=Mock();item.config=Mock()
        cluster='11111111-1111-1111-1111-111111111111';production='22222222-2222-2222-2222-222222222222';original='33333333-3333-3333-3333-333333333333';replacement='44444444-4444-4444-4444-444444444444'
        item.target={'clusterUID':cluster,'namespaceUID':production}
        item.record={'resources':{'v1/Namespace//ai-sdlc-browser':{'uid':original}}}
        item.config.read.side_effect=[{'metadata':{'uid':cluster}},{'metadata':{'uid':production}},{'metadata':{'uid':replacement}}]
        with self.assertRaisesRegex(ValueError,'Original browser namespace changed'):item.guard()
        self.assertEqual(item.config.read.call_count,3)
        item.config.base_run.assert_not_called()


class SchemaTests(unittest.TestCase):
    def test_index_order_is_not_oid_custody_but_flags_and_multiplicity_are(self):
        expected=copy.deepcopy(browser_schema.EXPECTED['deployment_browser_intents'])
        changed=copy.deepcopy(expected);changed['indexes'].reverse()
        self.assertEqual(browser_schema.canonical(expected),browser_schema.canonical(changed))
        changed['indexes'][0][0]=False
        self.assertNotEqual(browser_schema.canonical(expected),browser_schema.canonical(changed))
        changed=copy.deepcopy(expected);changed['indexes'].pop()
        self.assertNotEqual(browser_schema.canonical(expected),browser_schema.canonical(changed))


if __name__=='__main__':unittest.main()
