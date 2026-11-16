import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import registry_node_files as files
import registry_node_install as install


class RegistryNodeTests(unittest.TestCase):
    def test_only_reviewed_containerd_default_or_exact_path(self):
        prefix='version = 4\n[plugins."io.containerd.cri.v1.images".registry]\n'
        for value in ('','/etc/containerd/certs.d','/etc/containerd/certs.d:/etc/docker/certs.d'):
            self.assertEqual(install.registry_path(prefix+'config_path = '+json.dumps(value),'2.3.4'),'/etc/containerd/certs.d')
        for value in ('/other','/etc/containerd/certs.d:/other'):
            with self.assertRaises(ValueError):install.registry_path(prefix+'config_path = '+json.dumps(value),'2.3.4')
        for extra in ('\n[plugins."io.containerd.cri.v1.images".registry.mirrors.x]\nendpoint = ["http://other"]',
                      '\n[plugins."io.containerd.cri.v1.images".registry.configs.x]\nusername = "other"'):
            with self.assertRaises(ValueError):install.registry_path(prefix+'config_path = ""'+extra,'2.3.4')
        with self.assertRaises(ValueError):install.registry_path(prefix+'config_path = ""','2.3.5')

    def fixture(self):
        state={'nonce':'a'*48};contents={'/etc/hosts':('1:2','127.0.0.1 localhost\n')};calls=[];saved=[]
        def node(args,**kwargs):
            calls.append(args)
            script=args[2]
            if script==files.DIRECTORY:return subprocess.CompletedProcess(args,0,'1:3\n')
            if script==files.CREATE:
                path=args[4]
                self.assertIn(path.rsplit('/',1)[1],saved[-1]['files'])
                if path in contents:raise ValueError('exclusive create rejected')
                contents[path]=('1:4',kwargs['input'])
                return subprocess.CompletedProcess(args,0,'')
            if script==files.APPEND:
                self.assertIn('hosts',saved[-1]);identity,old=contents['/etc/hosts']
                self.assertEqual(args[4:6],[identity,files.sha(old)])
                contents['/etc/hosts']=(identity,old+kwargs['input'])
                return subprocess.CompletedProcess(args,0,'')
            if script==files.READ:
                identity,content=contents[args[4]]
                return subprocess.CompletedProcess(args,0,identity+'\n'+content)
            raise AssertionError('unknown operation')
        def save():saved.append(json.loads(json.dumps(state)))
        return files.Files(node,state,save),contents,calls,state

    def test_original_identities_and_hosts_bytes_survive_reapply(self):
        journal,contents,calls,state=self.fixture()
        for _ in range(2):
            journal.directory(True);journal.ensure('ca.crt','public',True);journal.hosts('10.96.1.2',True)
        self.assertEqual(contents['/etc/hosts'],('1:2','127.0.0.1 localhost\n10.96.1.2 '+files.HOST+'\n'))
        self.assertEqual(sum(a[2]==files.CREATE for a in calls),1)
        self.assertEqual(sum(a[2]==files.APPEND for a in calls),1)
        journal.directory(False);journal.ensure('ca.crt','public',False);journal.hosts('10.96.1.2',False)

    def test_lost_create_reply_binds_only_exact_existing_file(self):
        journal,contents,calls,state=self.fixture()
        journal.directory(True)
        state['files']={'ca.crt':{'sha256':files.sha('public'),'identity':None}}
        with self.assertRaises(KeyError):journal.ensure('ca.crt','public',True)
        self.assertFalse(any(a[2]==files.CREATE for a in calls))
        contents[files.ROOT+'/ca.crt']=('1:4','wrong')
        with self.assertRaises(ValueError):journal.ensure('ca.crt','public',True)
        contents[files.ROOT+'/ca.crt']=('1:4','public')
        journal.ensure('ca.crt','public',True)
        self.assertEqual(state['files']['ca.crt']['identity'],'1:4')
        contents[files.ROOT+'/ca.crt']=('1:5','public')
        with self.assertRaises(ValueError):journal.ensure('ca.crt','public',True)

    def test_hosts_drift_alias_and_lost_ack_do_not_duplicate(self):
        journal,contents,calls,state=self.fixture()
        contents['/etc/hosts']=('1:2','10.0.0.1 '+files.HOST.upper()+'\n')
        with self.assertRaises(ValueError):journal.hosts('10.96.1.2',True)
        self.assertNotIn('hosts',state)
        contents['/etc/hosts']=('1:2','127.0.0.1 localhost')
        journal.hosts('10.96.1.2',True)
        state['hosts']['complete']=False
        journal.hosts('10.96.1.2',True)
        self.assertEqual(sum(a[2]==files.APPEND for a in calls),1)
        contents['/etc/hosts']=('1:2','127.0.0.1 localhost')
        with self.assertRaises(ValueError):journal.hosts('10.96.1.2',True)


if __name__=='__main__':unittest.main()
