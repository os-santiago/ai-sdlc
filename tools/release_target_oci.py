"""Verify the single-platform baseline's original content-addressed graph."""
import hashlib
import json
import re

DIGEST=re.compile(r'sha256:[a-f0-9]{64}')


def verify(reference,runtime,read):
    cache={}
    def blob(digest,expected=None):
        if not isinstance(digest,str) or not DIGEST.fullmatch(digest):raise ValueError('Invalid original OCI digest')
        if expected is not None and (type(expected) is not int or not 1<=expected<=64*1024*1024):raise ValueError('OCI descriptor size exceeds baseline bound')
        if digest not in cache:
            raw=read(digest,expected or 128*1024)
            if not isinstance(raw,bytes) or len(raw)>64*1024*1024 or 'sha256:'+hashlib.sha256(raw).hexdigest()!=digest:raise ValueError('Original OCI content digest changed')
            cache[digest]=raw
        raw=cache[digest]
        if expected is not None and len(raw)!=expected:raise ValueError('Original OCI content size differs')
        return raw
    def document(digest):
        raw=blob(digest)
        if len(raw)>128*1024:raise ValueError('OCI document too large')
        return json.loads(raw)
    ref,run=document(reference),document(runtime)
    def manifest(digest,doc):
        if doc.get('schemaVersion')!=2:raise ValueError('Unsupported OCI schema')
        if 'manifests' not in doc:return digest,doc
        entries=doc['manifests']
        if not isinstance(entries,list) or len(entries)!=1:raise ValueError('Original baseline must have one manifest')
        descriptor=entries[0]
        if 'platform' in descriptor and (descriptor['platform'].get('os')!='linux' or descriptor['platform'].get('architecture')!='amd64'):raise ValueError('OCI descriptor platform differs')
        child=descriptor['digest'];raw=blob(child,descriptor['size']);result=json.loads(raw)
        if result.get('schemaVersion')!=2 or 'manifests' in result:raise ValueError('Nested OCI index rejected')
        return child,result
    ref_manifest,ref_doc=manifest(reference,ref);run_manifest,run_doc=manifest(runtime,run)
    if ref_manifest!=run_manifest or ref_doc!=run_doc:raise ValueError('Runtime image is not the original baseline manifest')
    config=ref_doc['config'];raw=blob(config['digest'],config['size']);platform=json.loads(raw)
    if platform.get('os')!='linux' or platform.get('architecture')!='amd64':raise ValueError('Original baseline platform differs')
    layers=ref_doc['layers']
    if not isinstance(layers,list) or not 1<=len(layers)<=8:raise ValueError('Baseline layer count exceeds bound')
    for layer in layers:blob(layer['digest'],layer['size'])
    return {'version':'ai-sdlc.baseline-oci-binding/v1','referenceDigest':reference,'runtimeDigest':runtime,
        'manifestDigest':ref_manifest,'configurationDigest':config['digest'],
        'content':{key:len(value) for key,value in sorted(cache.items())},'allContentHashesVerified':True}
