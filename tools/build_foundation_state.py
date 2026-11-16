"""Durable original-object ownership for administrative build foundation setup.

This journal never authorizes workflow execution or adopts pre-existing resources.
It records create intent before POST; after a lost reply only the same nonce and
exact desired object may close that intent. Absence after intent is quarantined.
"""
import copy
import hashlib
import json
import re
import secrets

from konflux_runtime_install import write_record

ANNOTATION = 'ai-sdlc.io/build-foundation'
UID = re.compile(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}')


def canonical(value):
    return json.dumps(value,sort_keys=True,separators=(',',':')).encode()


def identity(obj):
    return '/'.join((obj['apiVersion'],obj['kind'],obj['metadata'].get('namespace',''),obj['metadata']['name']))


def body(obj):
    """Remove only API bookkeeping and explicit harmless core default fields."""
    value=copy.deepcopy(obj)
    meta=value.pop('metadata',{})
    value.pop('status',None)
    if value['kind'] in ('PersistentVolume','PersistentVolumeClaim'):
        spec=value['spec']
        if spec.get('volumeMode')=='Filesystem':spec.pop('volumeMode')
        if value['kind']=='PersistentVolume':
            if spec.get('storageClassName')=='':spec.pop('storageClassName')
            reference=spec.get('claimRef',{})
            reference.pop('resourceVersion',None)
            if reference.get('kind')=='PersistentVolumeClaim':reference.pop('kind')
    if value['apiVersion']=='admissionregistration.k8s.io/v1':
        for key in ('matchConstraints','matchResources'):
            match=value.get('spec',{}).get(key)
            if isinstance(match,dict):
                match.setdefault('matchPolicy','Equivalent')
                match.setdefault('namespaceSelector',{})
                match.setdefault('objectSelector',{})
                for rule in match.get('resourceRules',[]):rule.setdefault('scope','*')
    if value['kind']=='NetworkPolicy' and value['spec'].get('ingress')==[]:
        value['spec'].pop('ingress')
    if value['kind']=='ConfigMap' and value.get('data')=={}:value.pop('data')
    if value['kind']=='Namespace':value.pop('spec',None)
    # Kubernetes adds its name label to namespaces. All caller-selected labels
    # remain part of the exact desired state; arbitrary new selectors are drift.
    labels=dict(meta.get('labels',{}))
    if value['kind']=='Namespace' and labels.get('kubernetes.io/metadata.name')==meta.get('name'):
        labels.pop('kubernetes.io/metadata.name')
    value['labels']=labels
    return value


class Resources:
    def __init__(self,path,state,get,create):
        self.path,self.state,self.get,self.create=path,state,get,create

    def save(self):
        write_record(self.path,self.state)

    def ensure(self,desired):
        desired=copy.deepcopy(desired)
        key=identity(desired)
        entries=self.state['resources']
        expected=hashlib.sha256(canonical(body(desired))).hexdigest()
        intent=entries.get(key)
        observed=self.get(desired)
        if intent is None:
            if observed is not None:
                raise ValueError('Refusing an existing unowned build foundation resource')
            intent={'nonce':secrets.token_hex(24),'desiredSHA256':expected,'uid':None}
            entries[key]=intent
            self.save()
            desired['metadata'].setdefault('annotations',{})[ANNOTATION]=intent['nonce']
            observed=self.create(desired)
        elif intent.get('desiredSHA256')!=expected:
            raise ValueError('Build foundation desired state changed; automatic replacement is forbidden')
        elif observed is None:
            raise ValueError('Build foundation create outcome or original resource absence is unresolved')
        meta=observed.get('metadata',{})
        uid=meta.get('uid','')
        if (not UID.fullmatch(uid) or meta.get('deletionTimestamp') is not None
                or identity(observed)!=key or meta.get('annotations',{}).get(ANNOTATION)!=intent['nonce']
                or (intent['uid'] is not None and intent['uid']!=uid)
                or body(observed)!=body(desired)):
            raise ValueError('Build foundation original identity or effective desired state differs')
        if intent['uid'] is None:
            intent['uid']=uid
            self.save()
        return observed
