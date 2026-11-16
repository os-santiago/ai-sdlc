#!/usr/bin/env python3
"""Exercise the normal node initializer against a disposable offline container."""
import argparse
import json
from pathlib import Path
import re
import secrets
import subprocess
import time

import build_foundation_node
import install


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--provider',choices=['podman','docker'],default='podman')
    parser.add_argument('--migrator-image',required=True)
    parser.add_argument('--output',required=True,type=Path)
    args=parser.parse_args()
    if args.output.exists():raise ValueError('New evidence path required')
    nonce=secrets.token_hex(20);name='ai-sdlc-foundation-node-fixture-'+nonce;cid=None
    image=json.loads((Path(__file__).resolve().parents[1]/'runtime/k8s/deploy/konflux/upstream.lock.json').read_text())['nodeImage']
    report={'status':'failed','scope':'isolated administrative initializer; no Kubernetes or workflow state',
            'migratorImage':args.migrator_image,'network':'none','autonomousE2E':False,'checks':[],'cleanupComplete':False}
    deadline=time.monotonic()+240
    def run(argv,**kwargs):
        kwargs['timeout']=min(kwargs.get('timeout',30),max(.1,deadline-time.monotonic()))
        return install.run(argv,**kwargs)
    def call(argv,**kwargs):return run([args.provider,*argv],capture_output=True,**kwargs)
    try:
        security=['--security-opt','apparmor=unconfined'] if args.provider=='docker' else []
        cid=call(['run','-d','--name',name,'--label','ai-sdlc.foundation-node-fixture='+nonce,'--network=none',
                  '--cap-add=SYS_ADMIN',*security,'--entrypoint','sleep',image,'300']).stdout.strip()
        if not re.fullmatch(r'[a-f0-9]{64}',cid):raise ValueError('Original fixture identity unavailable')
        def node(argv):return call(['exec',cid,*argv])
        boot=node(['cat','/proc/sys/kernel/random/boot_id']).stdout.strip()
        state={'storageIdentitySHA256':secrets.token_hex(32),'nodeBootID':boot}
        saved=[]
        def save():saved.append(json.loads(json.dumps(state)))
        build_foundation_node.initialize(run,args.provider,cid,args.migrator_image,state,save)
        root=state['mountIntent']['target'];original_mount=state['mountIntent']['id']
        node(['touch',root+'/workspace/preserved'])
        build_foundation_node.initialize(run,args.provider,cid,args.migrator_image,state,save)
        node(['test','-f',root+'/workspace/preserved'])
        if state['mountIntent']['id']!=original_mount:raise ValueError('Reapply replaced original mount')
        report['checks'].append('normal-published-migrator-initializes-and-preserves-original-mount-and-data')
        state['mountIntent']['id']='99999999'
        try:build_foundation_node.initialize(run,args.provider,cid,args.migrator_image,state,save)
        except (ValueError,subprocess.CalledProcessError):pass
        else:raise ValueError('Changed mount identity accepted')
        state['mountIntent']['id']=original_mount
        report['checks'].append('changed-original-mount-id-denied')
        node(['umount',root])
        try:build_foundation_node.initialize(run,args.provider,cid,args.migrator_image,state,save)
        except (ValueError,subprocess.CalledProcessError):pass
        else:raise ValueError('Missing original mount recreated')
        report['checks'].append('absent-original-mount-not-recreated')
        if not saved or 'mountIntent' not in saved[0] or saved[0]['mountIntent']['id'] is not None:
            raise ValueError('Durable mount intent did not precede initialization')
        report['checks'].append('mount-intent-precedes-effect')
        report['initializer']=state['initializer'];report['status']='passed'
    except BaseException as error:
        report['failureType']=type(error).__name__
        if isinstance(error,subprocess.CalledProcessError):report['boundedDiagnostic']=(error.stderr or '')[-2048:]
        raise
    finally:
        # Explicit ownership recovery is administrative fixture cleanup only.
        found=call(['inspect',name],check=False)
        if found.returncode==0:
            original=json.loads(found.stdout)[0]
            if (original.get('Config',{}).get('Labels',{}).get('ai-sdlc.foundation-node-fixture')!=nonce
                    or not re.fullmatch(r'[a-f0-9]{64}',original.get('Id','')) or cid is not None and original['Id']!=cid):
                raise ValueError('Fixture cleanup identity changed')
            call(['rm','-f','-v',original['Id']])
        elif name in call(['ps','-a','--format','{{.Names}}']).stdout.splitlines():
            raise ValueError('Fixture absence not proven')
        report['cleanupComplete']=True
        args.output.write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps({'status':report['status'],'checks':report['checks'],'cleanupComplete':True,'autonomousE2E':False}))

if __name__=='__main__':main()
