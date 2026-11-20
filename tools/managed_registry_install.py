"""Cold-only Helm ownership of the native local registry.

Never converts an operator-owned registry or takes over existing resources.
Private custody precedes Kubernetes effects and remains outside Helm history.
"""
import base64
import copy
import hashlib
import json
from pathlib import Path
import re
import secrets
import tempfile
import time
import yaml

import build_runtime_install
from konflux_runtime_install import write_record
from bootstrap_prepare import normalized_template

VERSION = 'ai-sdlc.managed-registry/v1'
CUSTODY_VERSION = 'ai-sdlc.cold-registry-custody/v1'
NAMESPACE = 'kind-registry'
RELEASE = 'ai-sdlc-registry'
RELEASE_NAMESPACE = 'ai-sdlc-platform'
NONCE = 'ai-sdlc.io/registry-create-nonce'
CHART = Path(__file__).resolve().parents[1]/'runtime/k8s/deploy/helm/ai-sdlc-registry'


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def chart_manifest():
    return digest({str(p.relative_to(CHART)):hashlib.sha256(p.read_bytes()).hexdigest()
        for p in sorted(CHART.rglob('*')) if p.is_file()})


def network_spec(value):
    result=copy.deepcopy(value)
    for key in ('ingress','egress'):
        if result.get(key)==[]:
            result.pop(key)
    return result


def stable_chart_content(desired,current):
    kind=desired['kind']
    if kind in ('Certificate','Bundle'):
        return current.get('spec')==desired.get('spec')
    if kind=='StorageClass':
        return all(current.get(k)==desired.get(k) for k in ('provisioner','reclaimPolicy','volumeBindingMode','parameters','mountOptions','allowVolumeExpansion','allowedTopologies'))
    if kind=='ServiceAccount':
        return current.get('automountServiceAccountToken') is False and not current.get('secrets') and not current.get('imagePullSecrets')
    if kind=='PersistentVolumeClaim':
        spec=copy.deepcopy(current.get('spec',{}));spec.pop('volumeName',None)
        if spec.get('volumeMode')=='Filesystem':spec.pop('volumeMode')
        return spec==desired['spec']
    if kind=='Service':
        spec=current.get('spec',{})
        ports=copy.deepcopy(spec.get('ports',[]))
        for port in ports:
            if port.get('protocol')=='TCP':port.pop('protocol')
        return (spec.get('type')=='ClusterIP' and spec.get('selector')==desired['spec']['selector']
            and ports==desired['spec']['ports'] and not spec.get('externalIPs') and not spec.get('externalName')
            and spec.get('sessionAffinity','None')=='None' and spec.get('internalTrafficPolicy','Cluster')=='Cluster')
    return True


def read_record(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 65536:
        raise ValueError('Original protected registry record required')
    return json.loads(path.read_text())


def validate_custody(value):
    if not isinstance(value,dict) or set(value) != {'version','publisher','reader','htpasswd','dockerConfig'} or value['version'] != CUSTODY_VERSION:
        raise ValueError('Invalid private registry custody')
    for role,username in [('publisher','konflux'),('reader','ai-sdlc-verifier')]:
        credential=value[role]
        if (not isinstance(credential,dict) or set(credential) != {'username','password'} or credential['username'] != username
                or not re.fullmatch('[a-f0-9]{64}',credential.get('password',''))):
            raise ValueError('Invalid private registry credential')
    if value['publisher']['password'] == value['reader']['password']:
        raise ValueError('Registry identities must have separate custody')
    if not re.fullmatch(r'konflux:\$2[aby]\$12\$[./A-Za-z0-9]{53}\nai-sdlc-verifier:\$2[aby]\$12\$[./A-Za-z0-9]{53}\n',value['htpasswd']):
        raise ValueError('Reviewed registry password hashes required')
    publisher=value['publisher']
    expected={'auths':{'registry-service.kind-registry':dict(publisher,auth=base64.b64encode(
        (publisher['username']+':'+publisher['password']).encode()).decode())}}
    if value['dockerConfig'] != expected:
        raise ValueError('Publisher registry scope differs')


def prepare(state_dir, cluster, provider, lock, existing_cluster):
    """Generate once, before the cluster or any registry object is created."""
    path=state_dir/'managed-registry.json'
    image=lock['image']['repository']+'@'+lock['migrator']['digest']
    manifest=chart_manifest()
    expected={'version':VERSION,'cluster':cluster,'provider':provider,'migratorImage':image,'chartSHA256':manifest}
    if path.exists() or path.is_symlink():
        state=read_record(path)
        if any(state.get(k)!=v for k,v in expected.items()) or not isinstance(state.get('objects'),dict):
            raise ValueError('Managed registry installation input changed')
    else:
        if existing_cluster or (state_dir/'konflux-ownership.json').exists():
            raise ValueError('Managed registry requires a new installation; operator ownership cannot be converted')
        custody=build_runtime_install.component_call(provider,image,'create-registry-custody',{'version':CUSTODY_VERSION})
        validate_custody(custody)
        state=dict(expected,creationNonce=secrets.token_hex(24),custody=custody,objects={})
        write_record(path,state)
    validate_custody(state.get('custody'))
    if not re.fullmatch('[a-f0-9]{48}',state.get('creationNonce','')):
        raise ValueError('Managed registry creation identity invalid')
    return state


class Installer:
    def __init__(self, state_dir, state, kubectl, binaries, deadline, cluster_uid, read_only=False):
        self.path=state_dir/'managed-registry.json'
        self.read_only=read_only
        self.state,self.kubectl,self.binaries,self.deadline=state,kubectl,binaries,deadline
        if state.get('clusterUID',cluster_uid)!=cluster_uid:
            raise ValueError('Original managed registry cluster changed')
        candidate=dict(state,clusterUID=cluster_uid)
        if not read_only:write_record(self.path,candidate)
        self.state=candidate

    def save(self,candidate):
        if self.read_only:raise ValueError('Registry observer cannot mutate custody')
        write_record(self.path,candidate)
        self.state=candidate

    def call(self,argv,value=None,limit=30):
        result=self.deadline.run(self.kubectl+argv,input=json.dumps(value) if value is not None else None,
            capture_output=True,limit=limit)
        if len(result.stdout)>4*1024*1024:
            raise ValueError('Registry installation observation exceeded its bound')
        return result.stdout

    def read(self,kind,name,namespace=None):
        raw=self.call(['get',kind,name,*(['-n',namespace] if namespace else []),'--ignore-not-found','-o','json'])
        return json.loads(raw) if raw.strip() else None

    def fence(self, cold=True):
        if cold:self.require_cold()
        cluster=self.read('namespace','kube-system')
        if cluster is None or cluster['metadata']['uid']!=self.state['clusterUID']:
            raise ValueError('Original registry cluster identity changed')
        parent=self.read('konflux','konflux')
        if parent is None or parent.get('spec',{}).get('internalRegistry',{}).get('enabled') is not False:
            raise ValueError('Konflux must explicitly disable its internal registry')
        if self.read('konfluxinternalregistry','konflux-internal-registry') is not None:
            raise ValueError('Operator registry controller still exists; refusing ownership conflict')
        if self.state.get('parentUID') is not None and parent['metadata']['uid']!=self.state['parentUID']:
            raise ValueError('Original Konflux parent was replaced')
        return parent['metadata']['uid']

    def require_cold(self):
        if self.read_only:raise ValueError('Registry observer cannot mutate installation')
        for namespace in ('ai-sdlc-runtime','ai-sdlc-builds'):
            if self.read('namespace',namespace) is not None:
                raise ValueError('Managed registry mutation is cold-only, before runtime or build installation; use status')

    def chart_values(self):
        return {'clusterUID':self.state['clusterUID'],'creationNonce':self.state['creationNonce'],
            'credentialRevision':digest(self.state['custody'])}

    def chart_objects(self):
        # Render without private data: credentials are never Helm input.
        result=self.deadline.run([self.binaries['helm'],'template',RELEASE,CHART,'--namespace',RELEASE_NAMESPACE,
            '-f','-'],input=json.dumps(self.chart_values()),capture_output=True,limit=30)
        return [x for x in yaml.safe_load_all(result.stdout) if x and x['kind']!='Namespace']

    def check_chart_objects(self, capture=False, network=None):
        # network: the expected NetworkPolicy spec while a reviewed boundary
        # transition is in progress; every other object stays exact.
        expected=self.chart_objects()
        recorded=self.state.get('chartObjects')
        observed={}
        for desired in expected:
            kind,name=desired['kind'],desired['metadata']['name']
            key=kind+'/'+name
            current=self.read(kind,name,desired['metadata'].get('namespace'))
            if recorded is None and not capture:
                if current is not None:
                    raise ValueError('Registry chart resource exists without original intent; refusing adoption')
                continue
            if current is None:
                raise ValueError('Original chart resource is absent; refusing recreation')
            meta=current['metadata'];annotations=meta.get('annotations',{})
            if (meta.get('deletionTimestamp') or meta.get('ownerReferences')
                    or annotations.get('ai-sdlc.io/creation-nonce')!=self.state['creationNonce']
                    or annotations.get('meta.helm.sh/release-name')!=RELEASE
                    or annotations.get('meta.helm.sh/release-namespace')!=RELEASE_NAMESPACE
                    or recorded is not None and recorded.get(key)!=meta['uid']):
                raise ValueError('Original Helm resource ownership changed')
            if kind=='ConfigMap' and current.get('data')!=desired.get('data'):
                raise ValueError('Native registry configuration changed')
            if kind=='Deployment' and (current['spec'].get('replicas')!=1 or
                    normalized_template(current['spec']['template'])!=normalized_template(desired['spec']['template'])):
                raise ValueError('Registry execution template changed')
            if kind=='NetworkPolicy' and network_spec(current.get('spec'))!=network_spec(network if network is not None else desired.get('spec')):
                raise ValueError('Registry network boundary changed')
            if not stable_chart_content(desired,current):
                raise ValueError('Registry trust, endpoint, identity or storage configuration changed')
            observed[key]=meta['uid']
        if recorded is not None and set(observed)!=set(recorded):
            raise ValueError('Original registry chart inventory changed')
        if capture:
            self.save(dict(self.state,chartObjects=observed))
        if self.state.get('storage'):
            self.storage()
        return observed

    def storage(self):
        pvc=self.read('pvc','registry-data',NAMESPACE)
        if pvc is None or not pvc.get('spec',{}).get('volumeName'):
            raise ValueError('Registry storage is not bound')
        pv=self.read('pv',pvc['spec']['volumeName'])
        if (pvc.get('status',{}).get('phase')!='Bound' or pv is None
                or pv.get('spec',{}).get('persistentVolumeReclaimPolicy')!='Retain'
                or pv['spec'].get('claimRef',{}).get('uid')!=pvc['metadata']['uid']):
            raise ValueError('Retained registry storage is not bound')
        physical={'pvcUID':pvc['metadata']['uid'],'pvUID':pv['metadata']['uid'],'volume':pvc['spec']['volumeName']}
        if self.state.get('storage',physical)!=physical:
            raise ValueError('Original registry storage changed')
        return physical

    def ensure(self,desired):
        """Closed namespace/Secret create protocol, never adoption or rotation."""
        self.fence()
        desired=copy.deepcopy(desired)
        kind,name=desired['kind'],desired['metadata']['name']
        namespace=desired['metadata'].get('namespace')
        if kind not in ('Namespace','Secret'):
            raise ValueError('Unsupported registry custody object')
        key=kind+'/'+name
        wanted=digest(desired)
        original=self.state['objects'].get(key)
        current=self.read(kind,name,namespace)
        first=original is None
        if first:
            if current is not None:
                raise ValueError('Registry resource exists without original custody; refusing adoption')
            original={'nonce':secrets.token_hex(24),'sha256':wanted,'uid':None}
            candidate=copy.deepcopy(self.state);candidate['objects'][key]=original
            self.save(candidate)
            desired['metadata'].setdefault('annotations',{})[NONCE]=original['nonce']
            self.fence()
            current=json.loads(self.call(['create','-f','-','-o','json'],desired))
        elif original.get('sha256')!=wanted or current is None:
            raise ValueError('Original registry create outcome or input changed; refusing recreation')
        meta=current.get('metadata',{})
        uid=meta.get('uid','')
        if (not re.fullmatch(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}',uid)
                or meta.get('deletionTimestamp') or meta.get('ownerReferences')
                or meta.get('annotations',{}).get(NONCE)!=original['nonce']
                or original['uid'] is not None and original['uid']!=uid
                or any(meta.get('labels',{}).get(k)!=v for k,v in desired['metadata'].get('labels',{}).items())
                or any(meta.get('annotations',{}).get(k)!=v for k,v in desired['metadata'].get('annotations',{}).items())
                or kind=='Secret' and (current.get('data')!=desired['data'] or current.get('type')!=desired['type'] or current.get('immutable') is not True)):
            raise ValueError('Original registry resource or private custody changed')
        if original['uid'] is None:
            candidate=copy.deepcopy(self.state);candidate['objects'][key]['uid']=uid;self.save(candidate)
        self.fence()
        return current

    def install(self):
        parent_uid=self.fence()
        if self.state.get('parentUID') is None:
            self.save(dict(self.state,parentUID=parent_uid))
        self.ensure({'apiVersion':'v1','kind':'Namespace','metadata':{'name':NAMESPACE,
            'annotations':{'ai-sdlc.io/cluster-uid':self.state['clusterUID'],'ai-sdlc.io/creation-nonce':self.state['creationNonce'],
                'meta.helm.sh/release-name':RELEASE,'meta.helm.sh/release-namespace':RELEASE_NAMESPACE},
            'labels':{'app.kubernetes.io/part-of':'ai-sdlc','app.kubernetes.io/managed-by':'Helm',
                **{'pod-security.kubernetes.io/'+key:'restricted' for key in ('enforce','audit','warn')}}}})
        custody=self.state['custody']
        def secret(name,kind,values):
            return self.ensure({'apiVersion':'v1','kind':'Secret','metadata':{'name':name,'namespace':NAMESPACE,
                'annotations':{'ai-sdlc.io/cluster-uid':self.state['clusterUID']},'labels':{'app.kubernetes.io/part-of':'ai-sdlc'}},
                'immutable':True,'type':kind,'data':{k:base64.b64encode(v.encode()).decode() for k,v in values.items()}})
        secret('zot-htpasswd','Opaque',{'htpasswd':custody['htpasswd']})
        secret('internal-registry-client-auth','kubernetes.io/dockerconfigjson',{
            '.dockerconfigjson':json.dumps(custody['dockerConfig'],sort_keys=True,separators=(',',':'))})
        secret('provenance-registry-reader','Opaque',{'credential.json':json.dumps(custody['reader'],sort_keys=True,separators=(',',':'))})
        values=self.chart_values()
        self.fence()
        if self.state.get('chartIntent') and not self.state.get('chartObjects'):
            # A failed Helm request cannot be retried by overwriting a partially
            # created release. Recover only its complete original inventory.
            self.check_chart_objects(capture=True)
        else:
            self.check_chart_objects()
        self.save(dict(self.state,chartIntent=True))
        if self.deadline.remaining(330)<330:
            raise ValueError('Insufficient managed registry Helm budget')
        with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as output:
            json.dump(values,output);output.flush()
            self.deadline.run([self.binaries['helm'],'upgrade','--install',RELEASE,CHART,
                '--kubeconfig',self.kubectl[self.kubectl.index('--kubeconfig')+1],
                '--namespace',RELEASE_NAMESPACE,'-f',output.name,'--server-side=false','--wait=legacy','--timeout','5m'],limit=330)
        self.fence()
        self.check_chart_objects(capture=True)
        namespace=self.read('namespace',NAMESPACE)
        if namespace['metadata']['uid']!=self.state['objects']['Namespace/'+NAMESPACE]['uid']:
            raise ValueError('Original registry namespace changed')
        physical=self.storage()
        self.save(dict(self.state,storage=physical,status='installed'))
        import managed_registry_probe
        conformance=managed_registry_probe.verify(self)
        self.save(dict(self.state,status='verified',conformance=conformance))
        return {'version':VERSION,'status':'installed','owner':'Helm','publisherCustodyPreserved':True,
            'readOnlyCredentialSeparate':True,'retainedPVC':True,'nativeACLConformance':conformance,
            'deliveryActivated':False,'liveWorkflowAdvanced':False}

    def verify_recovery(self):
        """Cold acceptance only: reapply, replace the owned Pod, retain marker."""
        import managed_registry_probe
        self.require_cold()
        first=self.install()['nativeACLConformance']
        pods=json.loads(self.call(['get','pods','-n',NAMESPACE,'-o','json']))['items']
        original=next((p for p in pods if p['metadata']['uid']==first['podUID']),None)
        if original is None:
            raise ValueError('Original acceptance Pod changed before intent')
        if self.state.get('replacementProof'):
            proof=self.state['replacementProof']
            if proof['markerDigest']!=first['markerDigest']:
                raise ValueError('Retained acceptance marker changed')
            return dict(proof,currentConformance=first)
        intent=self.state.get('replacementIntent')
        if intent is None:
            intent={'name':original['metadata']['name'],'uid':original['metadata']['uid'],
                'markerDigest':first['markerDigest'],'storage':self.storage()}
            self.save(dict(self.state,replacementIntent=intent))
        self.fence();self.check_chart_objects()
        previous=self.read('pod',intent['name'],NAMESPACE)
        if previous is not None and previous['metadata']['uid']==intent['uid'] and not previous['metadata'].get('deletionTimestamp'):
            self.call(['delete','--raw','/api/v1/namespaces/'+NAMESPACE+'/pods/'+intent['name'],'-f','-'],
                {'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':intent['uid']}})
        until=time.monotonic()+self.deadline.remaining(180)
        while True:
            self.fence()
            observed=json.loads(self.call(['get','pods','-n',NAMESPACE,'-o','json']))['items']
            ready=[p for p in observed if p['metadata']['uid']!=intent['uid'] and not p['metadata'].get('deletionTimestamp')
                and any(c.get('type')=='Ready' and c.get('status')=='True' for c in p.get('status',{}).get('conditions',[]))]
            if len(ready)==1 and not any(p['metadata']['uid']==intent['uid'] for p in observed):break
            if time.monotonic()>=until:raise ValueError('Replacement registry Pod did not become ready')
            time.sleep(1)
        self.check_chart_objects()
        if self.storage()!=intent['storage']:raise ValueError('Acceptance storage identity changed')
        # Reconcile the existing parent through its ordinary public declaration;
        # no CR fields or operator implementation are patched.
        parent=self.read('konflux','konflux')
        self.fence()
        self.call(['annotate','konflux','konflux','--overwrite','--resource-version='+parent['metadata']['resourceVersion'],
            'ai-sdlc.io/registry-acceptance='+self.state['creationNonce']])
        # Observe through more than one asynchronous reconciliation opportunity.
        until=time.monotonic()+self.deadline.remaining(10)
        while time.monotonic()<until:
            self.fence();self.check_chart_objects();time.sleep(.5)
        final=managed_registry_probe.verify(self)
        if final['markerDigest']!=intent['markerDigest'] or final['podUID']==intent['uid']:
            raise ValueError('Administrative marker did not survive Pod replacement')
        proof={'status':'passed','scope':'cold-registry-kubernetes-acceptance','originalPodUID':intent['uid'],
            'replacementPodUID':final['podUID'],'markerDigest':final['markerDigest'],
            'storagePreserved':True,'configurationStableAfterParentAnnotation':True,
            'currentConformance':final,'workflowEvidence':False}
        self.save(dict(self.state,replacementProof=proof))
        return proof
