"""Prepare fixed browser infrastructure with original-resource create custody."""
import hashlib
import json
import time
import yaml

import bootstrap_prepare as bootstrap
import browser_image
import browser_profile_install
import browser_runtime
import build_foundation_install as files
import build_foundation_state
import install
import install_images
import konflux_runtime_install as platform
from release_target_oci import bounded_output

VERSION='ai-sdlc.browser-platform/v1'


class Platform:
    def __init__(self,config,target,image,helper,guard,expires_at):
        if not image.startswith(install_images.REGISTRY+'@') or not install_images.DIGEST.fullmatch(image.split('@')[-1]):
            raise ValueError('Immutable published browser image required')
        self.config,self.target,self.image,self.helper=config,target,image,helper
        self.outer_guard,self.expires_at=guard,expires_at
        self.path=config.state_dir/'browser-platform.json'
        self.record=files.bounded_json(self.path,1024*1024) if self.path.exists() or self.path.is_symlink() else None
        self.identity={'targetSHA256':bootstrap.digest(target),'image':image,'clusterUID':target['clusterUID']}
        if self.record is not None and (self.record.get('version')!=VERSION or self.record.get('identity')!=self.identity):
            raise ValueError('Original browser platform custody changed')

    def save(self):platform.write_record(self.path,self.record)

    def guard(self):
        if time.time()>=self.expires_at:raise ValueError('Original browser installation window expired')
        self.outer_guard()
        for name,uid in (('kube-system',self.target['clusterUID']),('ai-sdlc-production',self.target['namespaceUID'])):
            obj=self.config.read('namespace',name,None)
            if obj is None or bootstrap.Preparation.identity(obj)!=uid:raise ValueError('Original browser target scope changed')
        if self.record is not None:
            original=self.record['resources'].get('v1/Namespace//ai-sdlc-browser')
            if original is not None and original['uid'] is not None:
                namespace=self.config.read('namespace','ai-sdlc-browser',None)
                if namespace is None or bootstrap.Preparation.identity(namespace)!=original['uid']:
                    raise ValueError('Original browser namespace changed before installation effect')
        if time.time()>=self.expires_at:raise ValueError('Original browser installation window expired during observation')

    def effect(self,argv,*,timeout,**kwargs):
        self.guard()
        remaining=self.expires_at-time.time()
        if remaining<=0:raise ValueError('Original browser installation expired before effect')
        return self.config.base_run(argv,timeout=min(timeout,remaining),**kwargs)

    def node(self):
        self.guard()
        raw=self.config.base_run([self.config.provider,'inspect',self.config.cluster+'-control-plane'],capture_output=True,timeout=15).stdout
        node=json.loads(raw)[0]
        expected=self.config.original['original']['nodeID']
        if node['Id']!=expected or node['Config']['Labels'].get('io.x-k8s.kind.cluster')!=self.config.cluster:
            raise ValueError('Original browser node container changed')
        return expected

    def runtime(self):
        self.guard()
        prior=self.record.get('runtime')
        if prior is None:
            # An unrecorded existing class is not adopted into this operation.
            if self.config.read('runtimeclass','ai-sdlc-browser',None) is not None:
                raise ValueError('Browser class exists without this installation custody')
            self.record['runtime']={'state':'creating'};self.save()
            self.guard()
            observed=browser_runtime.install(self.config.kubectl,self.config.provider,self.config.cluster,self.target['clusterUID'],guard=self.guard,expires_at=self.expires_at)
            self.record['runtime']={'state':'complete','observed':observed};self.save()
        elif prior.get('state')!='complete':
            raise ValueError('Original browser runtime effect unresolved; no replay')
        observed=browser_runtime.install(self.config.kubectl,self.config.provider,self.config.cluster,self.target['clusterUID'],read_only=True,guard=self.guard,expires_at=self.expires_at)
        original=self.record['runtime']['observed']
        for key in ('nodeName','nodeUID','runtimeClassUID','configurationSHA256','clusterUID','nodeConfigurationAfterSHA256'):
            if observed[key]!=original[key]:raise ValueError('Original browser runtime changed')
        return observed

    def ensure(self,desired):
        def get(obj):
            self.guard();result=self.config.read(obj['kind'],obj['metadata']['name'],obj['metadata'].get('namespace'));self.guard();return result
        def create(obj):
            self.guard()
            raw=self.effect(self.config.kubectl+['create','-f','-','-o','json'],input=json.dumps(obj),capture_output=True,timeout=20).stdout
            if len(raw)>128*1024:raise ValueError('Browser creation acknowledgement too large')
            self.guard();return json.loads(raw)
        return build_foundation_state.Resources(self.path,self.record,get,create).ensure(desired)

    def bind_source(self,name,value):
        if name!='browser-original-profile':raise ValueError('Unexpected browser compilation output')
        previous=self.record.get('compiled')
        if previous is not None and previous!=value:raise ValueError('Original browser compilation changed')
        self.record['compiled']=value;self.save()

    def run(self):
        self.guard()
        if self.record is None:
            self.record={'version':VERSION,'identity':self.identity,'resources':{},'runtime':None,'compiled':None};self.save()
        runtime=self.runtime()
        cid=self.node()
        def image_present():
            if self.node()!=cid:raise ValueError('Original image node changed')
            raw=bounded_output([self.config.provider,'exec',cid,'ctr','-n','k8s.io','images','ls','name=='+self.image],128*1024).decode()
            rows=[line.split() for line in raw.splitlines() if line.split() and line.split()[0]==self.image]
            if rows and (len(rows)!=1 or len(rows[0])<3 or rows[0][2]!=self.image.split('@')[1]):
                raise ValueError('Cached browser image reference points to foreign content')
            return bool(rows)
        if not image_present():
            # Pulling immutable public content grants no workflow capability.
            self.effect([self.config.provider,'exec',cid,'ctr','-n','k8s.io','images','pull','--platform','linux/amd64',self.image],capture_output=True,timeout=180)
        if not image_present():raise ValueError('Original browser image reference unavailable')
        def blob(digest,limit):
            if self.node()!=cid:raise ValueError('Original image node changed')
            result=bounded_output([self.config.provider,'exec',cid,'ctr','-n','k8s.io','content','get',digest],limit)
            if self.node()!=cid:raise ValueError('Original image node changed')
            return result
        reference=self.image.split('@')[1]
        graph=browser_image.verify(reference,reference,blob)
        if self.record.get('image') not in (None,graph):raise ValueError('Original browser image graph changed')
        self.record['image']=graph;self.save()
        objects={}
        for desired in yaml.safe_load_all((install.ROOT/'runtime/k8s/deploy/local/browser-platform/scope.yaml').read_text()):
            obj=self.ensure(desired);objects[obj['metadata']['name']]=obj
        ca=self.ensure({'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'approved-console-ca','namespace':'ai-sdlc-browser'},'immutable':True,'data':{'ca.crt':self.target['tlsCA']}})
        profile={'version':'ai-sdlc.saferus-browser-profile/v1','nodeName':runtime['nodeName'],'nodeUID':runtime['nodeUID'],
            'clusterUID':self.target['clusterUID'],'namespaceUID':objects['ai-sdlc-browser']['metadata']['uid'],
            'runtimeClassUID':runtime['runtimeClassUID'],'caConfigMapUID':ca['metadata']['uid'],
            'denyPolicyUID':objects['deny-all']['metadata']['uid'],'egressPolicyUID':objects['console-egress']['metadata']['uid'],
            'consoleIngressPolicyUID':objects['ai-sdlc-browser-console']['metadata']['uid'],
            # This owned containerd image record targets the published root
            # digest (which may be an index), also reported by CRI as imageID.
            # The hash-checked graph separately binds its unique amd64 child.
            'image':self.image,'runtimeDigest':graph['referenceDigest'],'caSHA256':hashlib.sha256(self.target['tlsCA'].encode()).hexdigest(),
            'runtimeConfigurationSHA256':runtime['configurationSHA256'],'baselineExpectation':'baseline-seconds','candidateExpectation':'readable-duration'}
        lock={'image':{'repository':install_images.REGISTRY},'migrator':{'digest':self.helper.split('@')[1]}}
        browser_profile_install.configure(self.config.base_run,self.config.kubectl,self.config.provider,lock,profile,self.target,self)
        self.guard();return profile
