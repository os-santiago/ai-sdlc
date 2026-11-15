"""Bind an already cryptographically verified Chains statement to fixture inputs.

This is not an authorization engine or a SLSA-level assessment. The caller must
first verify the envelope using the independently selected public signing key.
"""
import base64
import json
import re


def strict_json(raw):
    if len(raw)>2*1024*1024:raise ValueError('Provenance exceeds size bound')
    def unique(pairs):
        result={}
        for key,value in pairs:
            if key in result:raise ValueError('Duplicate provenance key')
            result[key]=value
        return result
    return json.loads(raw,object_pairs_hook=unique)


def verified_statement(raw,expected,publisher_image):
    envelope=strict_json(raw)
    if not isinstance(envelope,dict) or envelope.get('payloadType')!='application/vnd.in-toto+json' or not envelope.get('signatures'):
        raise ValueError('Missing verified DSSE envelope')
    try:payload=base64.b64decode(envelope['payload'],validate=True)
    except (KeyError,TypeError,ValueError):raise ValueError('Invalid verified payload encoding') from None
    statement=strict_json(payload)
    bind_statement(statement,expected,publisher_image)
    return statement


def bind_statement(statement,expected,publisher_image):
    try:
        if statement['_type']!='https://in-toto.io/Statement/v0.1' or statement['predicateType']!='https://slsa.dev/provenance/v0.2':raise ValueError('Unexpected statement format')
        wanted={'name':expected['IMAGE_URL'],'digest':{'sha256':expected['IMAGE_DIGEST'].removeprefix('sha256:')}}
        if statement['subject']!=[wanted]:raise ValueError('Signed artifact subject differs from expected digest')
        p=statement['predicate']
        if p['buildType']!='tekton.dev/v1/PipelineRun' or p['builder']!={'id':'https://tekton.dev/chains/v2'}:raise ValueError('Untrusted builder or build type')
        tasks=p['buildConfig']['tasks']
        if len(tasks)!=1:raise ValueError('Unexpected task topology')
        task=tasks[0]
        if task['name']!='publish' or task['status']!='Succeeded' or task['serviceAccountName']!='publish':raise ValueError('Unexpected trusted publisher task')
        results={}
        for result in task['results']:
            if result['name'] in results or result.get('type')!='string':raise ValueError('Duplicate or mistyped publisher result')
            results[result['name']]=result['value']
        if results!=expected:raise ValueError('Signed source, definition, run or image result differs from expected input')
        invocation=task['invocation']
        if invocation['parameters']!={'run-uid':expected['RUN_UID']} or invocation['environment']['labels']['tekton.dev/pipelineRunUID']!=expected['RUN_UID']:raise ValueError('Signed run identity differs')
        steps=task['steps']
        if len(steps)!=1 or steps[0]['entryPoint']!='/component' or steps[0]['environment']['image']!='oci://'+publisher_image:raise ValueError('Untrusted publisher image or entry point')
        args=steps[0]['arguments']
        if len(args)!=6 or args[0]!='--input=/artifacts/source/quarkus-app/target/quarkus-app' or not re.fullmatch(re.escape('--output='+expected['IMAGE_URL']+':')+r'[a-f0-9]{12}',args[1]):raise ValueError('Unexpected publisher recipe arguments')
        if args[2:]!=['--source-sha='+expected['CHAINS-GIT_COMMIT'],'--definition-digest='+expected['DEFINITION_DIGEST'],'--run-uid='+expected['RUN_UID'],'--timeout=8m']:raise ValueError('Publisher arguments differ from expected identity')
        repository,digest=publisher_image.rsplit('@sha256:',1)
        materials=p['materials']
        wanted_materials=[{'uri':'oci://'+repository,'digest':{'sha256':digest}},
                          {'uri':'git+'+expected['CHAINS-GIT_URL']+'.git','digest':{'sha1':expected['CHAINS-GIT_COMMIT']}}]
        if len(materials)!=2 or any(m not in materials for m in wanted_materials):raise ValueError('Signed source or publisher material differs')
    except (KeyError,TypeError,IndexError,AttributeError):raise ValueError('Incomplete provenance identity') from None
