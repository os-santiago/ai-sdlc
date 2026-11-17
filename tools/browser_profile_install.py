"""Validate an explicitly selected public browser scope; never creates/adopts it."""
import json
import re
import build_foundation_install as files
import build_runtime_install


def configure(run,kubectl,provider,lock,profile,target,configuration):
    if (profile.get('version')!='ai-sdlc.saferus-browser-profile/v1'
            or not re.fullmatch('[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?',profile.get('nodeName',''))
            or profile.get('clusterUID')!=target.get('clusterUID')):
        raise ValueError('Original browser profile and target required')
    def get(kind,name,namespace=None):
        configuration.guard()
        raw=run(kubectl+['get',kind,*([name] if name else []),*(['-n',namespace] if namespace else []),'-o','json'],capture_output=True,timeout=20).stdout
        if len(raw)>128*1024:raise ValueError('Browser public observation exceeded bound')
        result=json.loads(raw);configuration.guard();return result
    observations={key:get(kind,name,ns) for key,kind,name,ns in (
        ('cluster','namespace','kube-system',None),('namespace','namespace','ai-sdlc-browser',None),
        ('node','node',profile['nodeName'],None),('runtimeClass','runtimeclass','ai-sdlc-browser',None),
        ('ca','configmap','approved-console-ca','ai-sdlc-browser'),
        ('policies','networkpolicies',None,'ai-sdlc-browser'),
        ('consoleIngress','networkpolicy','ai-sdlc-browser-console','ai-sdlc-production'))}
    output=build_runtime_install.component_call(provider,lock['image']['repository']+'@'+lock['migrator']['digest'],
        'compile-browser-installation',dict(observations,target=target,profile=profile))
    if (set(output)!={'version','profile','profileSHA256','targetSHA256'}
            or output['version']!='ai-sdlc.browser-installation/v1' or output['profile']!=profile
            or not re.fullmatch('[a-f0-9]{64}',output['profileSHA256'])
            or not re.fullmatch('[a-f0-9]{64}',output['targetSHA256'])):
        raise ValueError('Browser compiler changed approved public scope')
    configuration.bind_source('browser-original-profile',output)
    return json.dumps(profile,sort_keys=True)
