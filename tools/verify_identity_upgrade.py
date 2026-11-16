"""Test additive identities on real Kubernetes in an owned disposable namespace.

No database workflow records or delivery components are created. Private
bootstrap output and Secret contents remain in captured pipes and memory.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import secrets
import subprocess

import install


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--migrator-image',required=True)
    parser.add_argument('--report',type=Path,required=True)
    args=parser.parse_args()
    if not re.fullmatch(r'quay.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}',args.migrator_image):
        raise ValueError('Imported immutable migrator fixture required')
    state=Path.home()/'.local/state/ai-sdlc/ai-sdlc-identities-test'
    marker=json.loads((state/'ownership.json').read_text())
    if marker.get('cluster')!='ai-sdlc-identities-test' or marker.get('provider')!='podman':
        raise ValueError('Dedicated owned identity fixture cluster required')
    k=[str(install.tools(Path.home()/'.cache/ai-sdlc/tools')['kubectl']),'--kubeconfig',str(state/'kubeconfig')]
    def run(command,value=None):
        result=subprocess.run(command,input=json.dumps(value) if value is not None else None,text=True,capture_output=True,timeout=90)
        if result.returncode:
            raise ValueError('Identity fixture command failed without disclosing private output')
        return json.loads(result.stdout) if result.stdout.strip() else None
    uid=run(k+['get','namespace','kube-system','-o','json'])['metadata']['uid']
    if uid!=marker.get('clusterUID'):
        raise ValueError('Fixture cluster identity changed')
    namespace='ai-sdlc-identity-upgrade-'+secrets.token_hex(6)
    created=run(k+['create','-f','-','-o','json'],{'apiVersion':'v1','kind':'Namespace','metadata':{'name':namespace}})
    original_namespace=install.NAMESPACE
    install.NAMESPACE=namespace
    report={'version':1,'scope':'isolated-identity-upgrade-fixture','status':'failed','workflowAdvanced':False,'namespaceRemoved':False}
    try:
        old=install.image_lock(install.LOCAL/'images.lock.json')
        public=install.configure_identities(k,'podman',old,uid,'os-santiago/saferus','ai-sdlc-production',freeze=True)
        initial=json.loads(public['authority']['policyJSON'])
        if any(v['role']=='build-controller' for v in initial['keys'].values()):
            raise ValueError('Fixture requires a historical eight-identity image')
        run(k+['create','-f','-','-o','json'],{'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'ai-sdlc-authority','namespace':namespace,'annotations':{'ai-sdlc.io/cluster-uid':uid}},'data':{'policy.json':public['authority']['policyJSON']}})
        def snapshot():
            resources=run(k+['get','secrets','-n',namespace,'-o','json'])['items']
            return {r['metadata']['name']:(r['metadata']['uid'],hashlib.sha256(json.dumps(r['data'],sort_keys=True).encode()).hexdigest()) for r in resources}
        before=snapshot()
        lock=json.loads(json.dumps(old));lock['migrator']['digest']=args.migrator_image.split('@')[1]
        extend=install.extend_owned_role_secret
        class Interrupted(Exception):pass
        def interrupted(*_):raise Interrupted()
        install.extend_owned_role_secret=interrupted
        try:
            install.configure_identities(k,'podman',lock,uid,'os-santiago/saferus','ai-sdlc-production',require_build=True)
            raise ValueError('Expected interrupted additive upgrade')
        except Interrupted:
            pass
        finally:
            install.extend_owned_role_secret=extend
        partial=snapshot()
        if any(partial.get(n)!=v for n,v in before.items()) or len(partial)!=len(before)+2:
            raise ValueError('Interrupted upgrade altered existing custody')
        upgraded=install.configure_identities(k,'podman',lock,uid,'os-santiago/saferus','ai-sdlc-production',require_build=True)
        after=snapshot()
        if json.loads(upgraded['authority']['policyJSON'])!=initial:
            raise ValueError('Upgrade changed inner policy')
        if any(after.get(n)!=v for n,v in partial.items() if n!='ai-sdlc-database-roles'):
            raise ValueError('Upgrade rotated an existing identity or component password')
        if after['ai-sdlc-database-roles'][0]!=before['ai-sdlc-database-roles'][0]:
            raise ValueError('Upgrade replaced migrator Secret identity')
        repeated=install.configure_identities(k,'podman',lock,uid,'os-santiago/saferus','ai-sdlc-production',require_build=True)
        if snapshot()!=after or repeated!=upgraded:
            raise ValueError('Reapplication changed custody')
        report.update(status='passed',oldIdentitiesPreserved=True,innerPolicyPreserved=True,interruptedUpgradeRecovered=True,newIdentityPreserved=True,reapplicationPreserved=True,migratorSecretUIDPreserved=True)
    finally:
        install.NAMESPACE=original_namespace
        run(k+['delete','--raw','/api/v1/namespaces/'+namespace,'-f','-'],{'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':created['metadata']['uid']}})
        result=subprocess.run(k+['wait','--for=delete','namespace/'+namespace,'--timeout=60s'],capture_output=True,timeout=65)
        report['namespaceRemoved']=result.returncode==0
        args.report.parent.mkdir(parents=True,exist_ok=True)
        args.report.write_text(json.dumps(report,indent=2)+'\n')
    if not report['namespaceRemoved']:raise ValueError('Fixture namespace cleanup incomplete')
    print(json.dumps(report))


if __name__=='__main__':main()
