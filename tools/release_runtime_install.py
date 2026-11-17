"""Configure reviewed release capability without starting a producer.

The target and original reader Secret must already be prepared and independently
approved. This module cannot create/adopt an application or approve observations.
"""
import json
import re

import browser_profile_install
import bootstrap_prepare
import build_foundation_install
import build_runtime_install


def inputs(target_path,images_path,lock,browser_path=None):
    target=build_foundation_install.bounded_json(target_path,128*1024)
    images=build_foundation_install.bounded_json(images_path,4096)
    if (set(images)!={'version','revision','deployerDigest'} or images['version']!='ai-sdlc.release-images/v1'
            or images['revision']!=lock['image']['tag'] or not re.fullmatch(r'sha256:[a-f0-9]{64}',images['deployerDigest'])):
        raise ValueError('Release image must come from the selected runtime publication')
    if browser_path is None:raise ValueError('Explicit installed browser profile required')
    profile=build_foundation_install.bounded_json(browser_path,8192)
    return {'target':target,'images':images,'browser':profile}


def configure(run,kubectl,provider,lock,approved,trust,configuration):
    target=approved['target']
    # Scope is checked before constructing any Kubernetes resource argument.
    if (target.get('namespace')!='ai-sdlc-production' or target.get('deployment')!='saferus-console'
            or target.get('service')!='saferus-console' or target.get('pullSecret')!='ai-sdlc-release-reader'):
        raise ValueError('Closed Saferus release scope required')
    def get(kind,name,namespace=None):
        configuration.guard()
        raw=run(kubectl+['get',kind,name,*(['-n',namespace] if namespace else []),'-o','json'],capture_output=True,timeout=20).stdout
        if len(raw)>512*1024:raise ValueError('Release target observation exceeded bound')
        return json.loads(raw)
    identities={}
    for kind,name,ns,field in (
            ('namespace','kube-system',None,'clusterUID'),
            ('namespace','ai-sdlc-production',None,'namespaceUID'),
            ('secret','ai-sdlc-release-reader','ai-sdlc-production','pullSecretUID'),
            ('secret','saferus-console-tls','ai-sdlc-production','tlsSecretUID')):
        obj=get(kind,name,ns);meta=obj['metadata'];uid=bootstrap_prepare.Preparation.identity(obj)
        if uid!=target.get(field) or meta.get('ownerReferences') or meta.get('finalizers'):
            raise ValueError('Separately approved release identity changed')
        if kind=='secret':
            expected_type='kubernetes.io/dockerconfigjson' if field=='pullSecretUID' else 'kubernetes.io/tls'
            if obj.get('type')!=expected_type or field=='pullSecretUID' and obj.get('immutable') is not True:
                raise ValueError('Approved release credential type or immutability changed')
            # Private material is never sent to the offline compiler or report.
            identities[field]={'uid':uid,'sha256':bootstrap_prepare.digest({'type':obj['type'],'immutable':obj.get('immutable',False),'data':obj.get('data',{})})}
        else:identities[field]={'uid':uid}
    deployment=get('deployment','saferus-console','ai-sdlc-production')
    service=get('service','saferus-console','ai-sdlc-production')
    compiled=build_runtime_install.component_call(provider,lock['image']['repository']+'@'+lock['migrator']['digest'],
        'compile-release-installation',{'target':target,'trust':trust,'deployment':deployment,'service':service})
    if (set(compiled)!={'version','target','targetSHA256','trustSHA256'} or compiled['version']!='ai-sdlc.release-installation/v1'
            or compiled['target']!=target or not re.fullmatch('[a-f0-9]{64}',compiled['targetSHA256'])
            or not re.fullmatch('[a-f0-9]{64}',compiled['trustSHA256'])):
        raise ValueError('Release compiler changed approved input')
    configuration.bind_source('release-original-identities',identities)
    browser=browser_profile_install.configure(run,kubectl,provider,lock,approved['browser'],target,configuration)
    return {'enabled':True,'digest':approved['images']['deployerDigest'],'targetJSON':json.dumps(target,sort_keys=True),'browserProfileJSON':browser}
