"""Run the actual transport test inside the rendered deployer network boundary."""
import base64
import copy
import hashlib
import json
import shutil
import subprocess
import time

import fixture_images
import browser_image
import bootstrap_prepare
from release_target_oci import bounded_output


def secret_custody(current,original):
    def selected(obj):
        meta=obj['metadata']
        if meta.get('deletionTimestamp') or meta.get('ownerReferences'):raise ValueError('Fixture input Secret custody changed')
        return {key:obj.get(key) for key in ('apiVersion','kind','data','immutable','type')}|{'metadata':{key:meta.get(key) for key in ('name','namespace','uid','labels','annotations')}}
    if selected(current)!=selected(original):raise ValueError('Fixture input Secret replaced or modified')


def pod_custody(current,original,desired,node):
    meta=current['metadata'];expected=desired['metadata']
    if (meta.get('uid')!=original['metadata']['uid'] or meta.get('name')!=expected['name'] or meta.get('namespace')!=expected['namespace']
            or meta.get('labels')!=expected.get('labels') or meta.get('annotations')!=expected.get('annotations')
            or meta.get('deletionTimestamp') or meta.get('ownerReferences')
            or bootstrap_prepare.runtime_pod_template(current,node)!=bootstrap_prepare.runtime_pod_template(desired,node)):
        raise ValueError('Fixture client Pod identity or authority changed')


class Client:
    def __init__(self,args,node,cid,nonce,out,run,guard,create,get,binary_sha256):
        self.args,self.node,self.cid,self.nonce,self.out=args,node,cid,nonce,out
        self.run,self.guard,self.create,self.get=run,guard,create,get
        self.image_id=None;self.image=None
        self.binary_sha256=binary_sha256

    def node_guard(self):
        self.guard()
        obj=json.loads(self.run([self.args.provider,'inspect',self.node]))[0]
        if obj['Id']!=self.cid:raise ValueError('Fixture test image node replaced')

    def prepare(self):
        self.node_guard()
        context=self.out/'client-image';context.mkdir(mode=0o700)
        shutil.copyfile(self.args.binary,context/'transport.test');(context/'transport.test').chmod(0o755)
        if hashlib.sha256((context/'transport.test').read_bytes()).hexdigest()!=self.binary_sha256:raise ValueError('Fixture test binary changed before image build')
        (context/'Containerfile').write_text('FROM scratch\nCOPY transport.test /test\nUSER 1001\nENTRYPOINT ["/test"]\n')
        tag='localhost/ai-sdlc-browser-transport:'+self.nonce
        self.run([self.args.provider,'build','--network=none','--label','ai-sdlc.browser-fixture='+self.nonce,'-t',tag,'-f',context/'Containerfile',context],limit=120)
        image=json.loads(self.run([self.args.provider,'image','inspect',tag]))[0]
        if image['Config']['Labels'].get('ai-sdlc.browser-fixture')!=self.nonce:raise ValueError('Fixture test image ownership differs')
        self.image_id=image['Id']
        archive=context/'client.tar'
        self.run([self.args.provider,'save','--output',archive,tag],limit=60)
        self.node_guard()
        with archive.open('rb') as stream:
            result=subprocess.run([self.args.provider,'exec','-i',self.cid,'ctr','-n','k8s.io','images','import','--local','--digests','-'],stdin=stream,capture_output=True,timeout=60)
        if result.returncode:raise ValueError('Fixture test image import failed')
        self.node_guard()
        self.image=fixture_images.register_digest(self.args.provider,self.node,tag)
        self.node_guard()
        def blob(digest,limit):
            self.node_guard()
            raw=bounded_output([self.args.provider,'exec',self.cid,'ctr','-n','k8s.io','content','get',digest],limit)
            self.node_guard();return raw
        graph=browser_image.verify(self.image.split('@')[-1],self.image.split('@')[-1],blob)
        host_digest=self.image_id if self.image_id.startswith('sha256:') else 'sha256:'+self.image_id
        if graph['configurationDigest']!=host_digest or hashlib.sha256((context/'transport.test').read_bytes()).hexdigest()!=self.binary_sha256:
            raise ValueError('Imported fixture image is not the original binary/configuration')

    def execute(self,kubectl,namespace,prefix,mode,private):
        self.node_guard()
        name='transport-'+mode
        body=json.dumps(private).encode()
        if len(body)>256*1024:raise ValueError('Fixture private input exceeds bound')
        desired_secret={'apiVersion':'v1','kind':'Secret','metadata':{'name':name,'namespace':namespace},'immutable':True,'type':'Opaque',
            'data':{'private.json':base64.b64encode(body).decode()}}
        secret=self.create(desired_secret)
        expected_secret=copy.deepcopy(desired_secret)
        expected_secret['metadata']['uid']=bootstrap_prepare.Preparation.identity(secret)
        secret_custody(secret,expected_secret)
        labels={'app.kubernetes.io/name':'ai-sdlc-k8s-runtime','app.kubernetes.io/instance':prefix,'app.kubernetes.io/component':'deployer'}
        desired={'apiVersion':'v1','kind':'Pod','metadata':{'name':name,'namespace':namespace,'labels':labels},'spec':{
            'nodeName':self.node,'enableServiceLinks':False,'serviceAccountName':prefix+'-deployer','automountServiceAccountToken':False,'restartPolicy':'Never','activeDeadlineSeconds':55,'terminationGracePeriodSeconds':1,
            'securityContext':{'runAsNonRoot':True,'runAsUser':1001,'runAsGroup':1001,'fsGroup':1001,'seccompProfile':{'type':'RuntimeDefault'}},
            'containers':[{'name':'test','image':self.image,'imagePullPolicy':'IfNotPresent','args':['-test.v','-test.timeout=45s','-test.run=^TestRealBrowserDeploymentTransport$'],
                'env':[{'name':'AI_SDLC_BROWSER_TRANSPORT_FIXTURE','value':'/input/private.json'}],
                'terminationMessagePath':'/evidence/result.json','terminationMessagePolicy':'File',
                'securityContext':{'readOnlyRootFilesystem':True,'allowPrivilegeEscalation':False,'capabilities':{'drop':['ALL']}},
                'resources':{'requests':{'cpu':'50m','memory':'64Mi'},'limits':{'cpu':'500m','memory':'256Mi'}},
                'volumeMounts':[{'name':'input','mountPath':'/input','readOnly':True},{'name':'evidence','mountPath':'/evidence'}]}],
            'volumes':[{'name':'input','secret':{'secretName':name,'defaultMode':288}},{'name':'evidence','emptyDir':{'medium':'Memory','sizeLimit':'16Mi'}}]}}
        secret_custody(self.get('secret',name,namespace),secret)
        pod=self.create(desired)
        pod_custody(pod,pod,desired,self.node)
        until=time.monotonic()+65
        while True:
            current=self.get('pod',name,namespace)
            pod_custody(current,pod,desired,self.node)
            statuses=current.get('status',{}).get('containerStatuses',[])
            if len(statuses)==1 and statuses[0].get('state',{}).get('terminated') is not None:break
            if time.monotonic()>=until:raise ValueError('Fixture client Pod exceeded bound')
            time.sleep(.25)
        status=statuses[0];termination=status['state']['terminated']
        secret_custody(self.get('secret',name,namespace),secret)
        pod_custody(self.get('pod',name,namespace),pod,desired,self.node)
        logs=self.run(kubectl+['logs',name,'-n',namespace,'-c','test','--limit-bytes=65536'])
        pod_custody(self.get('pod',name,namespace),pod,desired,self.node)
        secret_custody(self.get('secret',name,namespace),secret)
        (self.out/(mode+'.log')).write_bytes(logs)
        message=termination.get('message','')
        if len(message.encode())>4096:raise ValueError('Fixture public result exceeds bound')
        result=json.loads(message)
        (self.out/(mode+'.json')).write_text(json.dumps(result,indent=2)+'\n')
        if termination['exitCode']!=0 or status['restartCount']!=0 or status['imageID'].split('@')[-1]!=self.image.split('@')[-1] or result.get('status')!='passed':
            raise ValueError('Real in-cluster browser transport failed: '+mode)
        return result

    def close(self):
        if self.image_id is None:return None
        image=json.loads(self.run([self.args.provider,'image','inspect',self.image_id]))[0]
        if image['Id']!=self.image_id or image['Config']['Labels'].get('ai-sdlc.browser-fixture')!=self.nonce:
            raise ValueError('Fixture host image replaced')
        self.run([self.args.provider,'rmi',self.image_id],limit=30)
        return {'kind':'fixture-host-image','status':'removed','nodeImageCacheRetained':True}
