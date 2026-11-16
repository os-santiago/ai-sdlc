"""Actual native Zot checks for cold administrative installation only.

The marker is explicitly not a software build, signature, provenance or release.
No workflow record is created. Normal CA and hostname verification is mandatory.
"""
import base64
import hashlib
import http.client
import io
import json
import socket
import ssl
import subprocess
import time
from types import SimpleNamespace

HOST = 'registry-service.kind-registry'
REPO = 'ai-sdlc-fixtures/saferus'
PREFIX = '/v2/' + REPO + '/'
MEDIA = 'application/vnd.oci.image.manifest.v1+json'


class DeadlineReader(io.RawIOBase):
    """Bound headers and body even when the server continuously drips bytes."""
    def __init__(self,sock,end):
        self.sock,self.end=sock,end
        self.raw=sock.makefile('rb',buffering=0)

    def readable(self):
        return True

    def readinto(self,buffer):
        remaining=self.end-time.monotonic()
        if remaining<=0:raise TimeoutError('Registry response deadline exceeded')
        self.sock.settimeout(min(5,remaining))
        return self.raw.readinto(buffer)

    def close(self):
        try:self.raw.close()
        finally:super().close()


def check_matrix(request, nonce, require_existing=False):
    """Seed one deterministic administrative marker, then verify least privilege."""
    checks=[]
    def expect(name, response, status):
        if response[0]!=status:
            raise ValueError('Registry conformance failed: '+name)
        checks.append(name)
        return response
    config=b'{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}'
    config_digest='sha256:'+hashlib.sha256(config).hexdigest()
    manifest=json.dumps({'schemaVersion':2,'mediaType':MEDIA,'config':{
        'mediaType':'application/vnd.oci.image.config.v1+json','digest':config_digest,'size':len(config)},
        'layers':[],'annotations':{'ai-sdlc.io/purpose':'administrative-registry-probe',
            'ai-sdlc.io/creation-nonce':nonce}},sort_keys=True,separators=(',',':')).encode()
    artifact_digest='sha256:'+hashlib.sha256(manifest).hexdigest()
    path=PREFIX+'manifests/install-probe-'+nonce
    prior=request('GET',path,'publisher')
    if prior[0]==404:
        if require_existing:
            raise ValueError('Original administrative marker is absent; refusing to recreate persistence evidence')
        blob=request('HEAD',PREFIX+'blobs/'+config_digest,'publisher')
        if blob[0]==404:
            _,headers,_=expect('publisher-create-upload',request('POST',PREFIX+'blobs/uploads/','publisher'),202)
            location=headers.get('location','')
            if not location.startswith(PREFIX+'blobs/uploads/') or '#' in location or any(c in location for c in '\r\n'):
                raise ValueError('Registry upload location escaped its repository')
            expect('publisher-upload-blob',request('PUT',location+('&' if '?' in location else '?')+'digest='+config_digest,
                'publisher',config),201)
        else:
            expect('retained-config-blob',blob,200)
        expect('publisher-create-marker',request('PUT',path,'publisher',manifest,MEDIA),201)
    elif prior[0]!=200 or prior[2]!=manifest:
        raise ValueError('Administrative marker changed; refusing overwrite')
    expect('publisher-update-same-marker',request('PUT',path,'publisher',manifest,MEDIA),201)
    response=expect('reader-read-before',request('GET',path,'reader'),200)
    if response[2]!=manifest:raise ValueError('Administrative marker bytes differ')
    expect('reader-no-upload',request('POST',PREFIX+'blobs/uploads/','reader'),403)
    expect('reader-no-manifest',request('PUT',PREFIX+'manifests/denied-'+nonce,'reader',b'{}',MEDIA),403)
    # Never delete retained content even if authorization is accidentally broad.
    absent='sha256:'+hashlib.sha256(('absent-administrative-marker:'+nonce).encode()).hexdigest()
    absent_path=PREFIX+'manifests/'+absent
    expect('deletion-target-absent',request('GET',absent_path,'publisher'),404)
    expect('reader-no-delete',request('DELETE',absent_path,'reader'),403)
    expect('publisher-no-delete',request('DELETE',absent_path,'publisher'),403)
    expect('reader-no-other-repository',request('GET','/v2/other/fixture/manifests/absent','reader'),403)
    expect('publisher-no-other-repository',request('POST','/v2/other/fixture/blobs/uploads/','publisher'),403)
    expect('anonymous-no-read',request('GET',path,None),401)
    final=expect('reader-read-after',request('GET',path,'reader'),200)
    if final[2]!=manifest:raise ValueError('Administrative marker changed after denials')
    return {'status':'passed','checks':checks,'markerDigest':artifact_digest,
        'scope':'administrative-cold-installation','workflowEvidence':False}


def verify(installer):
    installer.fence()
    deployment=installer.read('deployment','registry','kind-registry')
    replicas=json.loads(installer.call(['get','replicasets','-n','kind-registry','-o','json']))['items']
    owned={r['metadata']['uid'] for r in replicas if any(o.get('controller') and o.get('uid')==deployment['metadata']['uid']
        for o in r['metadata'].get('ownerReferences',[]))}
    pods=json.loads(installer.call(['get','pods','-n','kind-registry','-o','json']))['items']
    active=[p for p in pods if not p['metadata'].get('deletionTimestamp') and any(
        o.get('controller') and o.get('uid') in owned for o in p['metadata'].get('ownerReferences',[]))]
    if len(active)!=1 or not any(c.get('type')=='Ready' and c.get('status')=='True' for c in active[0].get('status',{}).get('conditions',[])):
        raise ValueError('Exactly one ready owned registry Pod required')
    pod=active[0];name,uid=pod['metadata']['name'],pod['metadata']['uid']
    bundle=installer.read('configmap','trusted-ca','kind-registry')
    ca=bundle.get('data',{}).get('ca-bundle.crt','') if bundle else ''
    if not ca or len(ca)>1024*1024:raise ValueError('Bounded registry CA bundle required')
    context=ssl.create_default_context(cadata=ca)
    context.minimum_version=ssl.TLSVersion.TLSv1_3
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1',0));port=reservation.getsockname()[1]
    class Connection(http.client.HTTPSConnection):
        def connect(self):
            raw=socket.create_connection(('127.0.0.1',port),timeout=min(5,self.end-time.monotonic()))
            try:
                remaining=self.end-time.monotonic()
                if remaining<=0:raise TimeoutError('Registry TLS deadline exceeded')
                raw.settimeout(min(5,remaining))
                self.sock=context.wrap_socket(raw,server_hostname=HOST)
            except BaseException:
                raw.close();raise
    process=subprocess.Popen(installer.kubectl+['port-forward','-n','kind-registry','pod/'+name,str(port)+':5000',
        '--address','127.0.0.1'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    try:
        ready=time.monotonic()+15
        while True:
            if process.poll() is not None:raise ValueError('Registry port-forward stopped')
            try:
                with socket.create_connection(('127.0.0.1',port),timeout=.2):break
            except OSError:
                if time.monotonic()>ready:raise ValueError('Registry port-forward timed out') from None
                time.sleep(.1)
        def request(method,path,identity,body=b'',media='application/octet-stream'):
            installer.fence()
            current=installer.read('pod',name,'kind-registry')
            if current is None or current['metadata']['uid']!=uid or current['metadata'].get('deletionTimestamp'):
                raise ValueError('Registry Pod changed during conformance')
            headers={'Content-Type':media,'Accept':MEDIA}
            if identity:
                credential=installer.state['custody'][identity]
                headers['Authorization']='Basic '+base64.b64encode((credential['username']+':'+credential['password']).encode()).decode()
            connection=Connection(HOST,context=context,timeout=5)
            connection.end=time.monotonic()+installer.deadline.remaining(15)
            def response_class(sock,*args,**kwargs):
                wrapped=SimpleNamespace(makefile=lambda mode:io.BufferedReader(DeadlineReader(sock,connection.end)))
                return http.client.HTTPResponse(wrapped,*args,**kwargs)
            connection.response_class=response_class
            try:
                connection.request(method,path,body,headers)
                response=connection.getresponse();raw=response.read(1024*1024+1)
                if len(raw)>1024*1024:raise ValueError('Registry response exceeded bound')
                return response.status,{k.lower():v for k,v in response.getheaders()},raw
            finally:
                connection.close()
        report=check_matrix(request,installer.state['creationNonce'],require_existing=bool(installer.state.get('conformance')))
        report.update(tlsHostnameVerified=HOST,tlsMinimum='TLS1.3',podUID=uid,
            observedAt=time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()))
        return report
    finally:
        process.terminate()
        try:process.wait(timeout=5)
        except subprocess.TimeoutExpired:process.kill();process.wait(timeout=5)
