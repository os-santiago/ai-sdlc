#!/usr/bin/env python3
"""Exercise node-file custody only inside an owned offline disposable container."""
import argparse
import json
from pathlib import Path
import re
import secrets
import subprocess

import install
import konflux_runtime_install as platform
import registry_node_files as files


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--provider',choices=('podman','docker'),required=True)
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args()
    args.output.mkdir(parents=True,mode=0o700,exist_ok=False)
    lock=json.loads((install.ROOT/'runtime/k8s/deploy/konflux/upstream.lock.json').read_text())
    nonce=secrets.token_hex(24);name='ai-sdlc-node-trust-'+nonce[:16];cid=None
    report={'version':1,'scope':'isolated-node-file-fixture','workflowEvidence':False,'status':'running','checks':[]}
    state={'nonce':nonce}
    def save():platform.write_record(args.output/'journal.json',state)
    def record():platform.write_record(args.output/'report.json',report)
    record()
    def run(argv,**kwargs):
        return subprocess.run(argv,capture_output=True,text=True,timeout=30,check=True,**kwargs)
    try:
        cid=run([args.provider,'create','--name',name,'--network=none','--label','ai-sdlc.node-trust-fixture='+nonce,
            '--entrypoint','/bin/sleep',lock['nodeImage'],'240']).stdout.strip()
        if not re.fullmatch('[a-f0-9]{64}',cid):raise ValueError('Original fixture container ID unavailable')
        run([args.provider,'start',cid])
        calls=[];lost={'script':None}
        def node(command,**kwargs):
            calls.append(command)
            result=run([args.provider,'exec',*(['-i'] if 'input' in kwargs else []),cid,*command],**kwargs)
            if len(command)>2 and command[2]==lost['script']:
                lost['script']=None
                raise TimeoutError('Injected lost reply after actual fixture effect')
            return result
        journal=files.Files(node,state,save)
        journal.directory(True)
        lost['script']=files.CREATE
        try:journal.ensure('ca.crt','synthetic public certificate bytes\n',True)
        except TimeoutError:pass
        else:raise ValueError('Lost create response was not injected')
        journal.ensure('ca.crt','synthetic public certificate bytes\n',True)
        report['checks'].append('lost-create-reply-retains-original-file')
        journal.ensure('hosts.toml','server = "https://registry-service.kind-registry"\n',True)
        before_identity,before=journal.read('/etc/hosts')
        lost['script']=files.APPEND
        try:journal.hosts('10.96.250.17',True)
        except TimeoutError:pass
        else:raise ValueError('Lost append response was not injected')
        journal.hosts('10.96.250.17',True)
        report['checks'].append('lost-hosts-reply-does-not-append-again')
        journal.directory(False)
        journal.ensure('ca.crt','synthetic public certificate bytes\n',False)
        journal.ensure('hosts.toml','server = "https://registry-service.kind-registry"\n',False)
        journal.hosts('10.96.250.17',False)
        identity,after=journal.read('/etc/hosts')
        if identity!=before_identity or not after.startswith(before) or sum(c[2]==files.APPEND for c in calls if len(c)>2)!=1:
            raise ValueError('Host file identity or prefix was not retained')
        report['checks'].append('read-only-reapply-preserves-files-and-hosts-inode')
        # Only the original disposable container is mutated for negative cases.
        node(['rm','--',files.ROOT+'/ca.crt'])
        try:journal.ensure('ca.crt','synthetic public certificate bytes\n',True)
        except subprocess.CalledProcessError:pass
        else:raise ValueError('Deleted file was recreated')
        report['checks'].append('deleted-trust-file-never-recreated')
        node(['ln','-s','/etc/hosts',files.ROOT+'/ca.crt'])
        try:journal.ensure('ca.crt','synthetic public certificate bytes\n',True)
        except subprocess.CalledProcessError:pass
        else:raise ValueError('Symlink trust file was accepted')
        report['checks'].append('symlink-substitution-rejected')
        node(['bash','-c','printf "# fixture drift\\n" >> /etc/hosts'])
        try:journal.hosts('10.96.250.17',True)
        except ValueError:pass
        else:raise ValueError('Host drift was relearned')
        report['checks'].append('hosts-drift-rejected')
        report['status']='passed'
    except BaseException as e:
        report['status']='failed';report['failureType']=type(e).__name__;raise
    finally:
        try:
            found=subprocess.run([args.provider,'inspect',name],capture_output=True,text=True,timeout=15,check=False)
            if found.returncode==0:
                actual=json.loads(found.stdout)[0];original=actual.get('Id','')
                if (not re.fullmatch('[a-f0-9]{64}',original) or cid is not None and cid!=original
                        or actual.get('Config',{}).get('Labels',{}).get('ai-sdlc.node-trust-fixture')!=nonce):
                    raise ValueError('Fixture cleanup original identity differs')
                run([args.provider,'rm','-f','-v',original])
            else:
                names=run([args.provider,'ps','-a','--format','{{.Names}}']).stdout.splitlines()
                if name in names:raise ValueError('Fixture container absence unproven')
            report['originalContainerRemoved']=True
        except BaseException as e:
            report['status']='failed';report['cleanupFailureType']=type(e).__name__;raise
        finally:record()
    print(json.dumps(report,indent=2))
    return 0


if __name__=='__main__':raise SystemExit(main())
