"""Explicit approval inputs change dates, never existing work or identity."""
import copy
import json
from pathlib import Path
import ssl
import subprocess
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

import approval_window
import approval_window_targets
import bootstrap_prepare
import test_initial_activation
import test_tool_budget_upgrade
import tool_budget_targets


class ApprovalTargets(unittest.TestCase):
    def fixture(self):
        source, render = test_tool_budget_upgrade.TargetTests().source()
        compile_build = test_initial_activation.compiler(source['values'])
        source = tool_budget_targets.targets(source, render, compile_build)['active']
        now = 1800000000
        approved = {}
        for section, key in (('provenance', 'trustJSON'), ('release', 'targetJSON')):
            original = json.loads(source['values'][section][key])
            original.update(approvedAt=(now-3600)*1000, expiresAt=(now-1)*1000)
            source['values'][section][key] = json.dumps(original, sort_keys=True)
            approved[section] = dict(original, approvedAt=(now-10)*1000, expiresAt=(now+7200)*1000)
        source['documents'] = render(source['values'])
        return source, render, compile_build, approved, now

    def test_expired_source_gets_only_explicit_dates_and_intake_opens_last(self):
        source, render, compile_build, approved, now = self.fixture()
        unchanged = copy.deepcopy(source)
        result = approval_window_targets.targets(source, approved, render, compile_build, now)
        restored = copy.deepcopy(result['active']['values'])
        for section, key in (('provenance', 'trustJSON'), ('release', 'targetJSON')):
            self.assertEqual(json.loads(restored[section][key]), approved[section])
            restored[section][key] = source['values'][section][key]
        self.assertEqual(render(restored), source['documents'])
        self.assertEqual(source, unchanged)
        for phase in ('paused', 'staged', 'warming', 'armed', 'active'):
            self.assertEqual(result[phase]['values']['candidate'], source['values']['candidate'])
            self.assertEqual(result[phase]['images'], source['images'])
            intake = next(d for d in result[phase]['documents'] if d['kind']=='Deployment' and d['metadata']['name']=='ai-sdlc-intake')
            self.assertEqual(intake['spec']['replicas'], int(phase == 'active'))

    def test_other_fields_and_invalid_windows_are_rejected(self):
        source, render, compile_build, approved, now = self.fixture()
        for section in ('provenance', 'release'):
            for fault in ('field', 'scope', 'past', 'future', 'long', 'same', 'boolean'):
                bad = copy.deepcopy(approved)
                doc = bad[section]
                if fault == 'field': doc['unexpected'] = True
                elif fault == 'scope': doc['repository'] = 'another/repository'
                elif fault == 'past': doc['expiresAt'] = (now-1)*1000
                elif fault == 'future': doc['approvedAt'] = (now+1)*1000
                elif fault == 'long': doc['expiresAt'] = (now+86401)*1000
                elif fault == 'same': doc['approvedAt'] = (now-3600)*1000
                else: doc['approvedAt'] = True
                with self.subTest(section=section, fault=fault), self.assertRaises(ValueError):
                    approval_window_targets.targets(source, bad, render, compile_build, now)

    def test_administrative_expired_observer_does_not_change_ordinary_readiness(self):
        source, _, _, _, _ = self.fixture()
        observer = bootstrap_prepare.Preparation(None, [], 'podman', 'fixture', None, source['images'])
        observer.use_expired_source_render(source['documents'])
        self.assertTrue(observer.expired_source)
        self.assertEqual(set(observer.activation_replicas.values()), {1})
        observer.use_activation_render(source['documents'], 'active')
        self.assertFalse(observer.expired_source)


class ApprovalOrdering(unittest.TestCase):
    def test_registry_ca_must_cover_the_entire_new_window(self):
        with tempfile.TemporaryDirectory() as directory:
            cert = Path(directory) / 'ca.pem'
            subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                '-keyout', str(Path(directory)/'key.pem'), '-out', str(cert), '-days', '1',
                '-subj', '/CN=approval-window-fixture', '-addext', 'basicConstraints=critical,CA:TRUE',
                '-addext', 'keyUsage=critical,keyCertSign,cRLSign'],
                check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
            raw = cert.read_text()
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        context.load_verify_locations(cadata=raw)
        info = context.get_ca_certs()[0]
        start, end = (ssl.cert_time_to_seconds(info[k]) for k in ('notBefore', 'notAfter'))
        trust = {'registryCA': raw, 'approvedAt': int(start*1000), 'expiresAt': int(end*1000)}
        approval_window_targets.registry_ca_window(trust, start+1)
        for now, changed in ((start-1, trust), (end, trust),
                             (start+1, dict(trust, expiresAt=int(end*1000)+1)),
                             (start+1, dict(trust, approvedAt=int(start*1000)-1))):
            with self.assertRaises(ValueError): approval_window_targets.registry_ca_window(changed, now)

    def test_residual_execution_resource_stops_before_any_intent(self):
        item = object.__new__(approval_window.ApprovalWindow)
        item.config = Mock()
        item.pause = Mock(); item.pause.record = {'resourceUIDs': {}, 'sourceObserved': {}}
        item.record = None
        item.approvals = {'provided': True}
        item.unchanged, item.save, item.apply = Mock(), Mock(), Mock()
        item.describe = Mock(return_value={'documents': [], 'images': {}, 'values': {
            'provenance': {'trustJSON': '{"expiresAt":1}'}, 'release': {'targetJSON': '{"expiresAt":1}'}}})
        with patch.object(approval_window.prepared_resources, 'observe'), \
             patch.object(approval_window.bootstrap, 'Preparation') as preparation:
            observer = preparation.return_value
            observer.observe.return_value = ({}, 'private')
            observer.get.side_effect = [
                {'items': [{'metadata': {'name': 'ai-sdlc-executors'}}]},
                {'items': [{'metadata': {'name': 'unresolved-executor'}}]}]
            with self.assertRaises(ValueError): item.begin()
            observer.use_expired_source_render.assert_called_once()
        item.save.assert_not_called()
        item.apply.assert_not_called()

    def test_execution_inventory_is_rechecked_under_the_first_fence(self):
        item = object.__new__(approval_window.ApprovalWindow)
        item.observe = Mock(side_effect=ValueError('Unresolved original executor'))
        with patch.object(approval_window.ToolBudgetUpgrade, 'capture_first_pause') as capture:
            with self.assertRaises(ValueError): item.capture_first_pause(Mock())
            capture.assert_not_called()

    def test_existing_intent_does_not_refresh_its_window(self):
        item = object.__new__(approval_window.ApprovalWindow)
        item.record = {'expiresAt': time.time()-1, 'completed': 'ready', 'transition': None}
        item.approvals = {'provenance': {'expiresAt': 9999999999999}}
        before = copy.deepcopy(item.record)
        item.save = Mock()
        item.begin()
        self.assertEqual(item.record, before)
        item.save.assert_not_called()
        with self.assertRaises(ValueError): item.deadline()

    def test_fresh_inputs_required_before_any_effect(self):
        item = object.__new__(approval_window.ApprovalWindow)
        item.record, item.approvals = None, None
        item.observe, item.save, item.apply = Mock(), Mock(), Mock()
        with self.assertRaises(ValueError): item.begin()
        item.observe.assert_not_called()
        item.save.assert_not_called()
        item.apply.assert_not_called()

    def test_new_release_preflight_retains_original_secret_and_source_bindings(self):
        item = Mock()
        original = {'target': {'old': True}, 'sources': {'original': 'identity'}, 'privateSecrets': {'original': 'secret'}}
        item.pause.activation.original = original
        item.record = {'completed': 'paused'}
        item.observe.return_value = ({'original': 'observed'}, None)
        target = {'values': {'reviewed': 'approval'}}
        preflight = approval_window.ApprovedReleasePreflight(item, target)
        self.assertEqual(preflight.original['sources'], original['sources'])
        self.assertEqual(preflight.original['privateSecrets'], original['privateSecrets'])
        self.assertEqual(preflight.original['target'], target)
        self.assertNotEqual(original['target'], target)
        preflight.guard()
        item.observe.assert_called_once_with('paused')


if __name__ == '__main__':
    unittest.main()
