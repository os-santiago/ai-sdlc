"""Original-object administrative preparation, never delivery-stage authority.

Caller must hold the installation lock and prove an empty paused runtime before
and after effects. A patch intent is single use; only its exact nonce, original
UID and full intended spec can resolve a lost response. No workflow records.
"""
import copy
import secrets
import re

import bootstrap_prepare
import konflux_runtime_install as platform

ANNOTATION='ai-sdlc.io/release-preparation'


def metadata(obj):
    meta=obj.get('metadata',{})
    bootstrap_prepare.Preparation.identity(obj)
    if (meta.get('name')!='saferus-console' or meta.get('namespace')!='ai-sdlc-production'
            or meta.get('ownerReferences') or meta.get('finalizers')
            or not isinstance(meta.get('generation'),int) or meta['generation']<1
            or not str(meta.get('resourceVersion','')).isdigit()):
        raise ValueError('Original baseline Deployment identity required')
    return meta


def patch_request(before,nonce):
    meta=metadata(before)
    pod=before.get('spec',{}).get('template',{}).get('spec',{})
    containers=pod.get('containers',[])
    if (not isinstance(nonce,str) or not re.fullmatch('[a-f0-9]{48}',nonce)
            or before.get('spec',{}).get('replicas')!=1 or len(containers)!=1
            or containers[0].get('name')!='console'
            or not re.fullmatch(r'localhost/saferus-console@sha256:[a-f0-9]{64}',containers[0].get('image',''))):
        raise ValueError('Original pinned initial console and creation nonce required')
    if (before.get('apiVersion')!='apps/v1' or before.get('kind')!='Deployment'
            or before['spec']['template']['spec'].get('imagePullSecrets')
            or ANNOTATION in meta.get('annotations',{})):
        raise ValueError('Release preparation requires the original unconfigured baseline')
    desired=copy.deepcopy(before['spec']);desired['template']['spec']['imagePullSecrets']=[{'name':'ai-sdlc-release-reader'}]
    annotations=copy.deepcopy(meta.get('annotations',{}));annotations[ANNOTATION]=nonce
    # Compare the full original spec; no concurrent image or security update
    # can be hidden by only testing the original image or resource name.
    request=[{'op':'test','path':'/metadata/uid','value':meta['uid']},
        {'op':'test','path':'/metadata/resourceVersion','value':meta['resourceVersion']},
        {'op':'test','path':'/metadata/generation','value':meta['generation']},
        {'op':'test','path':'/spec','value':before['spec']},
        {'op':'add','path':'/metadata/annotations','value':annotations},
        {'op':'add','path':'/spec/template/spec/imagePullSecrets','value':[{'name':'ai-sdlc-release-reader'}]}]
    return request,desired,annotations


def verify_after(before,current,nonce):
    original=metadata(before);meta=metadata(current)
    _,spec,annotations=patch_request(before,nonce)
    actual_annotations=copy.deepcopy(meta.get('annotations',{}))
    # Deployment controller updates only its revision annotation after creating
    # the new ReplicaSet. It is not part of setup custody or API authority.
    revision='deployment.kubernetes.io/revision'
    for value in (actual_annotations,annotations):
        if revision in value:
            if not str(value[revision]).isdigit() or int(value[revision])<1:
                raise ValueError('Malformed Deployment controller revision')
            value.pop(revision)
    if (meta['uid']!=original['uid'] or meta['generation']!=original['generation']+1
            or meta['resourceVersion']==original['resourceVersion']
            or current['spec']!=spec or actual_annotations!=annotations
            or meta.get('labels',{})!=original.get('labels',{})):
        raise ValueError('Release preparation acknowledgement differs from original intent')


class Patch:
    def __init__(self,path,record,guard,get,send):
        self.path,self.record,self.guard,self.get,self.send=path,record,guard,get,send

    def apply(self,before):
        self.guard()
        intent=self.record.get('deploymentPatch')
        if intent is None:
            current=self.get()
            if current!=before:raise ValueError('Baseline changed before preparation intent')
            nonce=secrets.token_hex(24);request,_,_=patch_request(before,nonce)
            self.record['deploymentPatch']={'before':copy.deepcopy(before),'nonce':nonce,'response':None}
            platform.write_record(self.path,self.record) # before the sole PATCH
            self.guard()
            current=self.send(request)
        else:
            if intent.get('before')!=before:raise ValueError('Original preparation input changed')
            nonce=intent['nonce'];current=self.get() # never a second PATCH
        verify_after(before,current,nonce)
        intent=self.record['deploymentPatch']
        if intent['response'] is not None and intent['response']!=current['spec']:
            raise ValueError('Original prepared specification changed')
        self.guard()
        intent['response']=copy.deepcopy(current['spec'])
        platform.write_record(self.path,self.record)
        return current
