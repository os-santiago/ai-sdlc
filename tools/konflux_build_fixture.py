"""Reviewed isolated Homedir definitions; no live AI-SDLC commands."""
import copy
import hashlib
import json
from pathlib import Path

INPUTS = json.loads((Path(__file__).resolve().parents[1]/'runtime/k8s/deploy/konflux/homedir/inputs.json').read_text())
SECURITY = {'runAsNonRoot':True,'runAsUser':1000,'runAsGroup':1000,
            'allowPrivilegeEscalation':False,'readOnlyRootFilesystem':True,'capabilities':{'drop':['ALL']}}
PUBLIC_EGRESS = [{'to':[{'namespaceSelector':{'matchLabels':{'kubernetes.io/metadata.name':'kube-system'}},
                       'podSelector':{'matchLabels':{'k8s-app':'kube-dns'}}}],
                 'ports':[{'protocol':'UDP','port':53},{'protocol':'TCP','port':53}]},
                {'to':[{'ipBlock':{'cidr':'0.0.0.0/0','except':['10.0.0.0/8','172.16.0.0/12','192.168.0.0/16',
                    '127.0.0.0/8','169.254.0.0/16','100.64.0.0/10','224.0.0.0/4','240.0.0.0/4']}}],
                 'ports':[{'protocol':'TCP','port':443}]}]
REGISTRY_EGRESS = {'to':[{'namespaceSelector':{'matchLabels':{'kubernetes.io/metadata.name':'kind-registry'}},
                        'podSelector':{'matchLabels':{'run':'registry'}}}],
                   'ports':[{'protocol':'TCP','port':5000},{'protocol':'TCP','port':443}]}
RESULTS = ('IMAGE_URL','IMAGE_DIGEST','CHAINS-GIT_URL','CHAINS-GIT_COMMIT','DEFINITION_DIGEST','RUN_UID')

def policies(namespace):
    result=[]
    for role in ('build','publish','control'):
        egress=copy.deepcopy(PUBLIC_EGRESS)
        if role=='publish':egress.append(REGISTRY_EGRESS)
        if role=='control':egress=[{}]
        result.append({'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy',
            'metadata':{'name':role,'namespace':namespace},
            'spec':{'podSelector':{'matchLabels':{'ai-sdlc.role':role}},'policyTypes':['Ingress','Egress'],
                    'ingress':[],'egress':egress}})
    return result

def build_pipeline(targets=None):
    script='''#!/bin/bash
set -euo pipefail
umask 022
mkdir -p /work/home /work/cache
export HOME=/work/home
export JAVA_TOOL_OPTIONS=-Duser.home=/work/home
export MAVEN_OPTS='-Xmx1536m -Duser.home=/work/home'
test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
test ! -e /registry-auth
export GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0
git -c core.hooksPath=/dev/null init /work/source
cd /work/source
git -c core.hooksPath=/dev/null -c protocol.file.allow=never fetch --no-tags --depth=1 SOURCE_URL SOURCE_SHA
git -c core.hooksPath=/dev/null checkout --detach FETCH_HEAD
test "$(git rev-parse HEAD)" = SOURCE_SHA
mvn -B -ntp -Dmaven.repo.local=/work/cache -DargLine=-Xmx3g -f quarkus-app/pom.xml verify
'''.replace('SOURCE_URL',INPUTS['sourceURL']).replace('SOURCE_SHA',INPUTS['sourceSHA'])
    if targets:
        checks=[]
        for target in targets:
            host,port=target['host'],target['port']
            import ipaddress
            if ipaddress.ip_address(host).version!=4 or not isinstance(port,int) or not 1<=port<=65535:
                raise ValueError('Invalid network gate target')
            checks.append('count=0\nfor attempt in {1..20}; do\n  set +e\n  timeout 3 bash -c \'exec 3<>/dev/tcp/$1/$2\' probe HOST PORT\n  status=$?\n  set -e\n  if [ "$status" -eq 124 ]; then count=$((count+1)); elif [ "$status" -eq 0 ]; then count=0; else exit 1; fi\n  if [ "$count" -eq 2 ]; then break; fi\n  sleep 1\ndone\ntest "$count" -eq 2\n'.replace('HOST',host).replace('PORT',str(port)))
        script=script.replace('umask 022','umask 022\n'+''.join(checks))
    return {'tasks':[{'name':'maven','taskSpec':{'volumes':[{'name':'artifacts','persistentVolumeClaim':{'claimName':'artifacts'}},
                 {'name':'tmp','emptyDir':{'medium':'Memory','sizeLimit':'512Mi'}}],
            'steps':[{'name':'verify','image':INPUTS['builderImage'],'script':script,
                'securityContext':copy.deepcopy(SECURITY),
                'computeResources':{'requests':{'cpu':'1','memory':'1Gi'},'limits':{'cpu':INPUTS['mavenCPU'],'memory':INPUTS['mavenMemory']}},
                'volumeMounts':[{'name':'artifacts','mountPath':'/work'},{'name':'tmp','mountPath':'/tmp'}]}]}}]}

def publish_pipeline(image,definition,output):
    return {'results':[{'name':key,'value':'$(tasks.publish.results.'+key+')'} for key in RESULTS],
      'tasks':[{'name':'publish','params':[{'name':'run-uid','value':'$(context.pipelineRun.uid)'}],
      'taskSpec':{'params':[{'name':'run-uid','type':'string'}],
        'results':[{'name':key,'type':'string'} for key in RESULTS],
        'volumes':[{'name':'artifacts','persistentVolumeClaim':{'claimName':'artifacts','readOnly':True}},
                   {'name':'registry-auth','secret':{'secretName':'registry-auth','defaultMode':0o400}},
                   {'name':'ca','configMap':{'name':'trusted-ca'}}],
        'steps':[{'name':'publish','image':image,'command':['/component'],
          'args':['--input=/artifacts/source/quarkus-app/target/quarkus-app','--output='+output,
                  '--source-sha='+INPUTS['sourceSHA'],'--definition-digest='+definition,
                  '--run-uid=$(params.run-uid)','--timeout=8m'],
          'securityContext':copy.deepcopy(SECURITY),
          'computeResources':{'requests':{'cpu':'100m','memory':'256Mi'},'limits':{'cpu':'2','memory':'2Gi'}},
          'volumeMounts':[{'name':'artifacts','mountPath':'/artifacts','readOnly':True},
                         {'name':'registry-auth','mountPath':'/registry-auth','readOnly':True},
                         {'name':'ca','mountPath':'/etc/pki/ca-trust','readOnly':True}]}]}}]}

def definition_digest(image,targets=None):
    definition={'inputs':INPUTS,'build':build_pipeline(targets),'publish':publish_pipeline(image,'DEFINITION','OUTPUT')}
    return 'sha256:'+hashlib.sha256(json.dumps(definition,sort_keys=True,separators=(',',':')).encode()).hexdigest()

def pipeline_run(namespace,name,role,pipeline):
    return {'apiVersion':'tekton.dev/v1','kind':'PipelineRun',
      'metadata':{'name':name,'namespace':namespace,'labels':{'ai-sdlc.role':role}},
      'spec':{'timeouts':{'pipeline':'15m'},'taskRunTemplate':{'serviceAccountName':role,
        'podTemplate':{'automountServiceAccountToken':False,'securityContext':{'runAsNonRoot':True,'runAsUser':1000,'runAsGroup':1000,
           'fsGroup':1000,'seccompProfile':{'type':'RuntimeDefault'}}}},'pipelineSpec':pipeline}}
