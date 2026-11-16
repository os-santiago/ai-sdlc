"""Install separately approved public trust and original reader-only custody.

Caller owns the prepared-runtime lock and exclusion guard. This module cannot
approve an observed signing key, alter registry ACL, activate or advance work.
"""
import base64
import json

import build_foundation_install
import build_runtime_install
import managed_registry_install
import supply_chain_install

VERSION='ai-sdlc.provenance-runtime-custody/v1'


def configure(run,kubectl,provider,state_dir,lock,bundle,approved_trust,verifier_digest,configuration):
    approved=build_foundation_install.bounded_json(approved_trust)
    image=lock['image']['repository']+'@'+lock['migrator']['digest']
    verifier=lock['image']['repository']+'@'+verifier_digest
    compiled=build_runtime_install.component_call(provider,image,'compile-provenance-installation',
        {'trust':approved,'profile':bundle['profile'],'verifierImage':verifier})
    if (set(compiled)!={'version','trust','trustSHA256','profileSHA256','verifierImage'}
            or compiled['version']!='ai-sdlc.provenance-installation/v1'
            or compiled['trust']!=approved or compiled['verifierImage']!=verifier):
        raise ValueError('Provenance compiler changed separately approved input')
    cluster_uid=bundle['profile']['clusterUID']
    original=managed_registry_install.read_record(state_dir/'managed-registry.json')
    if (original.get('version')!=managed_registry_install.VERSION or original.get('clusterUID')!=cluster_uid
            or original.get('replacementProof',{}).get('status')!='passed'
            or original.get('replacementProof',{}).get('workflowEvidence') is not False):
        raise ValueError('Original accepted cold registry custody required')
    managed_registry_install.validate_custody(original.get('custody'))

    def get(kind,name,namespace=None):
        configuration.guard()
        raw=run(kubectl+['get',kind,name,*(['-n',namespace] if namespace else []),
            '--ignore-not-found','-o','json'],capture_output=True,timeout=20).stdout
        if len(raw)>4*1024*1024:raise ValueError('Provenance custody observation exceeded bound')
        return json.loads(raw) if raw.strip() else None

    namespace=get('namespace','kind-registry')
    if namespace is None or namespace['metadata']['uid']!=original['objects']['Namespace/kind-registry']['uid']:
        raise ValueError('Original registry namespace replaced')
    source=get('secret','provenance-registry-reader','kind-registry')
    expected_data={'credential.json':base64.b64encode(json.dumps(original['custody']['reader'],sort_keys=True,separators=(',',':')).encode()).decode()}
    origin=original['objects']['Secret/provenance-registry-reader']
    meta=(source or {}).get('metadata',{})
    if (source is None or meta.get('uid')!=origin['uid'] or meta.get('deletionTimestamp') or meta.get('ownerReferences')
            or meta.get('annotations',{}).get(managed_registry_install.NONCE)!=origin['nonce']
            or source.get('immutable') is not True or source.get('type')!='Opaque' or source.get('data')!=expected_data):
        raise ValueError('Original reader credential source changed')
    source_identity={'namespaceUID':namespace['metadata']['uid'],'secretUID':meta['uid'],
        'credentialSHA256':managed_registry_install.digest(expected_data)}
    observer=managed_registry_install.Installer(state_dir,original,kubectl,configuration.binaries,
        supply_chain_install.Deadline(120),cluster_uid,read_only=True)
    observer.fence(cold=False)
    observer.check_chart_objects()
    source_identity['registryObjectsSHA256']=managed_registry_install.digest(original['chartObjects'])
    source_identity['storage']=observer.storage()
    configuration.bind_source('provenance-registry',source_identity)
    desired={'apiVersion':'v1','kind':'Secret','metadata':{'name':'ai-sdlc-provenance-registry','namespace':'ai-sdlc-runtime',
        'annotations':{'ai-sdlc.io/cluster-uid':cluster_uid,'ai-sdlc.io/reader-source-uid':meta['uid']},
        'labels':{'app.kubernetes.io/part-of':'ai-sdlc'}},'type':'Opaque','immutable':True,'data':expected_data}
    destination=configuration.ensure_secret(desired)
    configuration.guard()
    observer.fence(cold=False)
    observer.check_chart_objects()
    # A source replacement during destination creation cannot become approved
    # by observing it at the end of the operation.
    again=get('secret','provenance-registry-reader','kind-registry')
    if again is None or again['metadata']['uid']!=meta['uid'] or again.get('data')!=expected_data:
        raise ValueError('Reader source changed during installation')
    return {'enabled':True,'trustJSON':json.dumps(approved,sort_keys=True),
        'registryCredentialSecret':destination['metadata']['name'],
        'credentialRevision':source_identity['credentialSHA256']}
