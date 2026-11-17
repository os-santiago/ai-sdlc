import json
import subprocess
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import browser_runtime_recovery as recovery
import offline_runtime


class RecoveryAuthority(unittest.TestCase):
    def fixture(self, fault=None):
        item=recovery.Recovery('podman','owned-control-plane','original','owned','/etc/containerd/config.toml.browser-owned','before','desired')
        files={offline_runtime.NODE_CONFIG:'desired'};calls=[]
        def execute(argv,**kw):
            calls.append(argv)
            if argv[1]=='inspect':
                obj={'Id':'replacement' if fault=='id' else 'original','Config':{'Labels':{'io.x-k8s.kind.cluster':'foreign' if fault=='label' else 'owned'}}}
                return SimpleNamespace(stdout=json.dumps([obj]))
            command=argv[4:]
            if command[0]=='test':return SimpleNamespace(stdout='')
            if command[0]=='cat':return SimpleNamespace(stdout='foreign' if fault=='configuration' else files[command[1]])
            if command[0]=='tee':files[command[1]]=kw['input']
            elif command[0]=='mv':files[command[-1]]=files.pop(command[-2])
            elif command[0]=='rm':files.pop(command[-1],None)
            elif command!=['systemctl','restart','containerd']:raise AssertionError(command)
            return SimpleNamespace(stdout='')
        return item,files,calls,execute

    def test_foreign_identity_or_configuration_prevents_restoration(self):
        for fault in ('id','label','configuration'):
            with self.subTest(fault=fault):
                item,files,calls,execute=self.fixture(fault)
                with patch.object(recovery.subprocess,'run',side_effect=execute),self.assertRaises(ValueError):item.restore()
                self.assertFalse(any(any(x in call for x in ('tee','mv','systemctl')) for call in calls))
                self.assertEqual(files[offline_runtime.NODE_CONFIG],'desired')

    def test_recovery_and_cleanup_share_one_deadline(self):
        item,files,calls,execute=self.fixture()
        with patch.object(recovery.subprocess,'run',side_effect=execute):
            item.restore();deadline=item.deadline;item.cleanup()
            self.assertEqual(item.deadline,deadline)
            self.assertEqual(files[offline_runtime.NODE_CONFIG],'before')
            self.assertNotIn(item.stage,files)
            item.deadline=time.monotonic()-1;count=len(calls)
            with self.assertRaises(ValueError):item.restore()
            with self.assertRaises(ValueError):item.cleanup()
            self.assertEqual(len(calls),count)

    def test_already_original_configuration_needs_no_restart(self):
        item,files,calls,execute=self.fixture();files[offline_runtime.NODE_CONFIG]='before'
        with patch.object(recovery.subprocess,'run',side_effect=execute):item.restore()
        self.assertFalse(any(any(x in call for x in ('tee','mv','systemctl')) for call in calls))


if __name__=='__main__':unittest.main()
