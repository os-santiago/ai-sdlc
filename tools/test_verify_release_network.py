import copy
import json
import os
from pathlib import Path
import shutil
import tempfile
import subprocess
import unittest
import verify_release_network as probe

class NetworkProofTests(unittest.TestCase):
    def test_tls_exec_and_malformed_failures_are_not_isolation(self):
        for code,text in ((1,'probe:denied'),(2,'probe:denied'),(0,'probe:denied'),(3,'probe:invalid'),(3,'probe:denied\nextra')):
            with self.subTest(code=code,text=text),self.assertRaises(ValueError):
                probe.outcome(subprocess.CompletedProcess([],code,text))
        self.assertFalse(probe.outcome(subprocess.CompletedProcess([],3,'probe:denied\n')))
        self.assertTrue(probe.outcome(subprocess.CompletedProcess([],0,'probe:allowed\n')))

    def test_replaced_terminating_or_foreign_objects_refused(self):
        original={'metadata':{'uid':'original','annotations':{'ai-sdlc.io/network-probe-nonce':'owner'}}}
        self.assertIs(probe.original(original,'original','owner'),original)
        for value,uid,nonce in ((original,'replacement','owner'),(original,'original','foreign'),
                ({'metadata':{'uid':'original','deletionTimestamp':'now'}},'original',None),(None,'original',None)):
            with self.subTest(value=value),self.assertRaises(ValueError):probe.original(value,uid,nonce)

    def test_same_uid_mutated_labels_or_execution_cannot_run_probe(self):
        expected=probe.pod('ns','client','registry/image@sha256:'+'a'*64,{'component':'deployer'},'nonce')
        current=copy.deepcopy(expected);current['metadata']['uid']='original'
        changes=[lambda x:x['metadata']['labels'].update(component='worker'),
            lambda x:x['spec']['containers'][0].update(image='untrusted/image'),
            lambda x:x['spec']['containers'][0].update(command=['sh','-c','echo probe:denied']),
            lambda x:x['spec'].update(automountServiceAccountToken=True),
            lambda x:x['spec'].update(ephemeralContainers=[{'name':'other'}])]
        for change in changes:
            altered=copy.deepcopy(current);change(altered);calls=[]
            with self.subTest(change=change),self.assertRaises(ValueError):
                probe.execute_probe(lambda:altered,expected,'original','nonce',lambda:calls.append('exec'))
            self.assertEqual(calls,[])

    def test_cleanup_continues_after_refused_resource_and_preserves_failure(self):
        removed=[];report={'cleanup':[]}
        entries=[('Pod','first','ns','uid1'),('Pod','replaced','ns','uid2'),('Pod','last','ns','uid3')]
        def remove(entry):
            if entry[1]=='replaced':raise ValueError('UID differs')
            removed.append(entry[1])
        with self.assertRaises(ValueError):probe.cleanup_all(entries,remove,report,lambda:None)
        self.assertEqual(removed,['last','first'])
        self.assertFalse(report['cleanupComplete'])
        self.assertEqual([r['removed'] for r in report['cleanup']],[True,False,True])

    def test_report_write_failure_cannot_skip_owned_cleanup(self):
        removed=[];report={'cleanup':[]}
        def fail():raise OSError('disk unavailable')
        with self.assertRaises(ValueError):probe.cleanup_all([('Pod','one','ns','uid')],lambda e:removed.append(e),report,fail)
        self.assertEqual(len(removed),1)
        self.assertFalse(report['cleanupComplete'])

class ActualNodeClientTests(unittest.TestCase):
    @unittest.skipUnless(os.environ.get('AI_SDLC_TEST_NODE') or shutil.which('node'),'Node required for live TLS client fixture')
    def test_real_tls_success_bad_certificate_and_malformed_response(self):
        node=os.environ.get('AI_SDLC_TEST_NODE') or shutil.which('node')
        with tempfile.TemporaryDirectory(prefix='release-client-') as directory:
            path=Path(directory)
            subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','1',
                '-subj','/CN=saferus-console','-addext','subjectAltName=DNS:saferus-console',
                '-keyout',str(path/'key.pem'),'-out',str(path/'cert.pem')],check=True,capture_output=True,timeout=15)
            (path/'key.pem').chmod(0o644) # Disposable test key only; never an application credential.
            path.chmod(0o755)
            (path/'client.js').write_text(probe.CLIENT.replace('process.argv[1]','process.argv[2]'))
            (path/'test.js').write_text(r"""
const fs=require('fs'),https=require('https'),{spawn}=require('child_process');
const cert=fs.readFileSync('cert.pem','utf8');let status=204;
const server=https.createServer({key:fs.readFileSync('key.pem'),cert},(q,s)=>{s.statusCode=status;s.end(status===204?'':'unexpected')});
function attempt(options){return new Promise((resolve,reject)=>{const c=spawn(process.execPath,['client.js',JSON.stringify(options)]);let out='';c.stdout.on('data',b=>out+=b);c.on('error',reject);c.on('exit',code=>resolve([code,out.trim()]));});}
server.listen(0,'127.0.0.1',async()=>{try{
 const base={host:'127.0.0.1',port:server.address().port,tls:true,ca:cert};
 const good=await attempt(base);const badCA=await attempt({...base,ca:undefined});
 status=500;const badStatus=await attempt(base);
 if(JSON.stringify(good)!==JSON.stringify([0,'probe:allowed'])||badCA[0]!==2||badStatus[0]!==2)throw Error('TLS classification differs');
 server.close(async()=>{const refused=await attempt(base);if(JSON.stringify(refused)!==JSON.stringify([3,'probe:denied']))process.exit(2);console.log('tls-client:passed');});
}catch(e){server.close();process.exit(2);}});
setTimeout(()=>process.exit(2),15000).unref();
""")
            result=subprocess.run([node,'test.js'],cwd=path,text=True,capture_output=True,timeout=25)
            self.assertEqual((result.returncode,result.stdout.strip()),(0,'tls-client:passed'),result.stderr)

if __name__=='__main__':unittest.main()
