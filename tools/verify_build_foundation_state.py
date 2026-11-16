#!/usr/bin/env python3
"""Validate administrative intent recovery against a disposable real namespace."""
import argparse
import json
from pathlib import Path
import secrets
import subprocess
import tempfile

import build_foundation_state as journal
import install
import konflux_runtime_install as platform


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster',default='ai-sdlc-konflux-reviewed')
    parser.add_argument('--output',required=True,type=Path)
    args=parser.parse_args()
    if args.output.exists():raise ValueError('New evidence path required')
    state_dir=Path.home()/'.local/state/ai-sdlc'/args.cluster
    owner=json.loads((state_dir/'konflux-ownership.json').read_text())
    tools=Path.home()/'.cache/ai-sdlc/tools'
    kubectl=[str(tools/'kubectl-1.35.8'),'--kubeconfig',str(state_dir/'kubeconfig'),'--context','kind-'+args.cluster]
    def call(argv,**kw):return install.run(kubectl+argv,capture_output=True,timeout=45,**kw)
    actual=json.loads(call(['get','namespace','kube-system','-o','json']).stdout)['metadata']['uid']
    platform.verify_cluster(install.run,kubectl,state_dir,args.cluster,owner['provider'],actual)
    name='ai-sdlc-foundation-qa-'+secrets.token_hex(8);uid=None;creates=0;lost=False
    report={'status':'failed','scope':'disposable Kubernetes administrative journal','autonomousE2E':False,'cleanupComplete':False}
    with tempfile.TemporaryDirectory(prefix='ai-sdlc-foundation-state-') as directory:
        state={'resources':{}};path=Path(directory)/'state.json'
        def get(obj):
            raw=call(['get',obj['kind'],obj['metadata']['name'],*(['-n',obj['metadata']['namespace']] if 'namespace' in obj['metadata'] else []),'--ignore-not-found','-o','json']).stdout
            return json.loads(raw) if raw.strip() else None
        def create(obj):
            nonlocal creates
            creates+=1
            result=json.loads(call(['create','-f','-','-o','json'],input=json.dumps(obj)).stdout)
            if lost:raise TimeoutError('fixture lost create ACK')
            return result
        resources=journal.Resources(path,state,get,create)
        ns={'apiVersion':'v1','kind':'Namespace','metadata':{'name':name,'labels':{'pod-security.kubernetes.io/enforce':'restricted'}}}
        try:
            namespace=resources.ensure(ns);uid=namespace['metadata']['uid']
            obj={'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'intent','namespace':name},'data':{'public':'original'}}
            lost=True
            try:resources.ensure(obj)
            except TimeoutError:pass
            else:raise ValueError('Lost reply injection did not fire')
            lost=False
            original=resources.ensure(obj)
            resources.ensure(obj)
            if creates!=2:raise ValueError('Recovered resource was recreated')
            report['lostCreateAckRecoveredOriginalUID']=original['metadata']['uid']
            call(['patch','configmap','intent','-n',name,'--type=merge','-p','{"data":{"public":"drift"}}'])
            try:resources.ensure(obj)
            except ValueError:pass
            else:raise ValueError('Effective desired state drift accepted')
            report['driftRejected']=True
            call(['delete','--raw','/api/v1/namespaces/'+name+'/configmaps/intent','-f','-'],input=json.dumps({'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':original['metadata']['uid']}}))
            try:resources.ensure(obj)
            except ValueError:pass
            else:raise ValueError('Absent original resource recreated')
            if creates!=2:raise ValueError('Uncertain resource caused another create')
            report['absenceQuarantined']=True;report['createCalls']=creates;report['status']='passed'
        finally:
            # Recover namespace ACK only for this fixture's persisted nonce.
            if uid is None and journal.identity(ns) in state['resources']:
                observed=get(ns);intent=state['resources'][journal.identity(ns)]
                if observed is not None and observed['metadata'].get('annotations',{}).get(journal.ANNOTATION)==intent['nonce']:
                    uid=observed['metadata']['uid']
            if uid:
                call(['delete','--raw','/api/v1/namespaces/'+name,'-f','-'],input=json.dumps({'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':uid}}))
                call(['wait','--for=delete','namespace/'+name,'--timeout=30s'])
            elif get(ns) is not None:raise ValueError('Fixture namespace cleanup cannot be proven')
            report['cleanupComplete']=True;args.output.write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps(report))

if __name__=='__main__':main()
