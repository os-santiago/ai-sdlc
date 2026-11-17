"""Install only rendered deployer RBAC into owned fixture namespaces."""
import json
from pathlib import Path
import subprocess
import yaml


def install(run,kubectl,helm,root,out,namespace,prefix,target,profile,create,api_cidrs=None):
    inner={'repository':'os-santiago/saferus','environment':'ai-sdlc-production','autonomy':'auto-merge-low','freeze':True}
    outer=dict(inner,autonomy='auto-deploy')
    build={'version':'ai-sdlc.build-installation/v1','inner':inner,'outer':outer,'profile':{'clusterUID':target['clusterUID'],'namespaceUID':'fixture-builds','publisherImage':'quay.io/os-santiago/ai-sdlc@sha256:'+'b'*64,'deniedTargets':[{'host':'10.96.0.1','port':443}]}}
    trust={'version':'ai-sdlc.saferus-provenance/v1','scope':'kind-test-production','repository':inner['repository'],'environment':'ai-sdlc-production','clusterUID':target['clusterUID'],'buildNamespaceUID':'fixture-builds','publisherImage':build['profile']['publisherImage'],'verifierImage':'quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64,'registryCA':'fixture','signerPublicKey':'fixture'}
    values={'fullnameOverride':prefix,'namespace':{'name':namespace},'runtime':{'mode':'verification','paused':True},'authority':{'policyJSON':json.dumps(inner)},'verifier':{'digest':'sha256:'+'a'*64},'review':{'enabled':True,'configJSON':'{}'},'publication':{'enabled':True},'merge':{'enabled':True,'configJSON':'{}'},'build':{'enabled':True,'digest':'sha256:'+'b'*64,'configJSON':json.dumps(build),'outerPolicyJSON':json.dumps(outer)},'provenance':{'enabled':True,'trustJSON':json.dumps(trust),'registryCredentialSecret':prefix+'-provenance-registry','credentialRevision':'c'*64},'candidate':{'apiServerCIDRs':['10.96.0.1/32']},'release':{'enabled':True,'digest':'sha256:'+'d'*64,'targetJSON':json.dumps(target),'browserProfileJSON':json.dumps(profile)}}
    if api_cidrs is not None:values['candidate']['apiServerCIDRs']=api_cidrs
    path=out/'rbac-values.json';path.write_text(json.dumps(values))
    raw=run([helm,'template',prefix,root/'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime','-n',namespace,'-f',root/'runtime/k8s/deploy/local/values.yaml','-f',path,'--show-only','templates/release.yaml','--show-only','templates/browser.yaml'])
    permitted={'ServiceAccount','Role','RoleBinding','ClusterRole','ClusterRoleBinding'}
    if api_cidrs is not None:permitted.add('NetworkPolicy')
    for obj in yaml.safe_load_all(raw):
        if obj and obj['kind'] in permitted:create(obj)
    return run(kubectl+['create','token',prefix+'-deployer','-n',namespace,'--duration=10m']).decode().strip()


def verify(kubectl,kubeconfig,out,token,node):
    config=yaml.safe_load(kubeconfig.read_text());config['users'][0]['user']={'token':token}
    private=out/'restricted-kubeconfig';private.write_text(yaml.safe_dump(config));private.chmod(0o600)
    command=[str(kubectl),'--kubeconfig',str(private),'--request-timeout=10s','auth','can-i']
    checks=[('create','pods','ai-sdlc-browser',True),('delete','pods','ai-sdlc-browser',True),('get','pods/log','ai-sdlc-browser',True),('get','configmaps/approved-console-ca','ai-sdlc-browser',True),('list','networkpolicies','ai-sdlc-browser',True),('get','nodes/'+node,None,True),('get','runtimeclasses/ai-sdlc-browser',None,True),('get','networkpolicies/ai-sdlc-browser-console','ai-sdlc-production',True),('patch','deployments/saferus-console','ai-sdlc-production',True),('create','pods','ai-sdlc-production',False),('create','pods/exec','ai-sdlc-browser',False),('create','pods/attach','ai-sdlc-browser',False),('get','secrets','ai-sdlc-browser',False),('create','serviceaccounts/token','ai-sdlc-browser',False),('patch','networkpolicies','ai-sdlc-browser',False),('patch','runtimeclasses/ai-sdlc-browser',None,False),('get','nodes/foreign',None,False),('list','nodes',None,False),('delete','namespaces/ai-sdlc-browser',None,False)]
    for verb,resource,namespace,allowed in checks:
        subresources={'pods/log':('pods','log'),'pods/exec':('pods','exec'),'pods/attach':('pods','attach'),'serviceaccounts/token':('serviceaccounts','token')}
        arguments=[resource]
        if resource in subresources:
            parent,child=subresources[resource];arguments=[parent,'--subresource='+child]
        result=subprocess.run(command+[verb,*arguments,*(['-n',namespace] if namespace else [])],capture_output=True,timeout=15)
        if (result.returncode,result.stdout.strip())!=((0,b'yes') if allowed else (1,b'no')):raise ValueError('actual browser fixture RBAC differs: '+verb+' '+resource+' expected='+str(allowed)+' observed='+('yes' if result.returncode==0 and result.stdout.strip()==b'yes' else 'no' if result.returncode==1 and result.stdout.strip()==b'no' else 'error'))
    return {'actualServiceAccount':True,'permissionChecks':len(checks),'createDeleteScope':'ai-sdlc-browser','arbitraryExecDenied':True,'credentialsAndPolicyWritesDenied':True}
