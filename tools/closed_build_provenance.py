"""Bind isolated closed-build evidence to externally selected source and recipe.

This validates a local fixture evidence bundle, not release authority. Operational
verification must obtain expectations from the authenticated built-record journal.
"""
import copy
import hashlib
import json
import re

import konflux_provenance as provenance


def go_json(value, sort_keys=False):
    # Archived structs preserve Go field order; map templates sort their keys.
    return (json.dumps(value, separators=(',', ':'), ensure_ascii=False, sort_keys=sort_keys)
            .replace('&', r'\u0026').replace('<', r'\u003c').replace('>', r'\u003e')
            .replace('\u2028', r'\u2028').replace('\u2029', r'\u2029').encode())


def load_fixture(report_path, source, tree):
    if any(not re.fullmatch(r'[a-f0-9]{40}', v) for v in (source, tree)):
        raise ValueError('Exact externally selected source and tree required')
    report = provenance.strict_json(report_path.read_bytes())
    if (report.get('status') != 'passed' or report.get('workloadExecuted') is not True
            or report.get('syntheticAuthority') is not True or report.get('autonomousE2E') is not False
            or report.get('releaseAuthorized') is not False or report.get('cleanupComplete') is not True
            or report.get('sourceSHA') != source or report.get('treeSHA') != tree):
        raise ValueError('Unexpected closed fixture evidence scope or source')
    transport = report['transport']; plan = transport['plan']; results = transport['results']
    if (transport.get('status') != 'passed' or transport.get('releaseAuthorized') is not False
            or plan['sourceSHA'] != source or plan['treeSHA'] != tree
            or results['CHAINS-GIT_COMMIT'] != source or results['CHAINS-GIT_URL'] != 'https://github.com/os-santiago/saferus'
            or results['RUN_UID'] != transport['publishRunUID']
            or results['DEFINITION_DIGEST'] != 'sha256:' + plan['definitionSHA256']
            or plan['publisherImage'] != report['publisherImage']):
        raise ValueError('Closed fixture plan and result identity differ')
    documents = {}
    for phase in ('build', 'publish'):
        raw = (report_path.parent / (phase + '.json')).read_bytes()
        if len(raw) > 32768 or hashlib.sha256(raw).hexdigest() != plan[phase + 'RequestSHA256']:
            raise ValueError('Archived request bytes differ from observed plan')
        documents[phase] = provenance.strict_json(raw)
    slot = provenance.strict_json((report_path.parent / 'fixture-slot.json').read_bytes())
    profile = provenance.strict_json((report_path.parent / 'profile.json').read_bytes())
    command = provenance.strict_json((report_path.parent / 'fixture-command.json').read_bytes())
    if command['sourceSHA'] != source or command['treeSHA'] != tree:
        raise ValueError('Archived command source differs')
    build_steps = documents['build']['spec']['pipelineSpec']['tasks'][0]['taskSpec']['steps']
    if (len(build_steps) != 2 or build_steps[1]['name'] != 'verify'
            or build_steps[1]['env'] != [{'name':'AI_SDLC_SOURCE_SHA','value':source},
                                       {'name':'AI_SDLC_SOURCE_TREE','value':tree}]):
        raise ValueError('Compiled build source or tree differs from external expectation')
    # Recompute the compiler's acyclic definition, whose hash is signed as a
    # publisher result AND argument. This binds the build's tree/environment,
    # both exact requests, profile and slot through the signed SHA-256 value.
    if definition_digest(documents,profile,slot,plan['definitionSHA256']) != plan['definitionSHA256']:
        raise ValueError('Closed build definition hash does not bind the evidence bundle')
    for value in (slot['storageIdentitySHA256'], plan['definitionSHA256']):
        if not re.fullmatch(r'[a-f0-9]{64}', value):
            raise ValueError('Malformed closed recipe digest')
    if not re.fullmatch(r'[a-f0-9-]{36}', slot['nodeBootID']) or type(command['expiresAt']) is not int:
        raise ValueError('Malformed closed recipe custody')
    image = report['publisherImage']
    if not re.fullmatch(r'localhost/ai-sdlc-artifact-builder@sha256:[a-f0-9]{64}', image):
        raise ValueError('Unexpected closed publisher image')
    output = plan['output']
    if not re.fullmatch(r'registry-service.kind-registry/ai-sdlc-fixtures/saferus:[a-f0-9]{32}', output):
        raise ValueError('Unexpected closed publisher destination')
    uid = results['RUN_UID']
    if not re.fullmatch(r'[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}', uid):
        raise ValueError('Unexpected original publisher identity')
    expected_steps = [
        {'name': 'custody', 'image': image, 'command': ['/component'], 'args': [
            '--guard-workspace', '--storage-identity=' + slot['storageIdentitySHA256'],
            '--node-boot-id=' + slot['nodeBootID'], '--workspace-phase=publish',
            '--authority-expires-at=' + str(command['expiresAt'])]},
        {'name': 'publish', 'image': image, 'command': ['/component'], 'args': [
            '--recipe=ai-sdlc.saferus-go-static/v1', '--input=/slot/workspace/output', '--output=' + output,
            '--source-sha=' + source, '--definition-digest=' + results['DEFINITION_DIGEST'],
            '--run-uid=' + uid, '--timeout=4m']}]
    steps = documents['publish']['spec']['pipelineSpec']['tasks'][0]['taskSpec']['steps']
    if len(steps) != 2:
        raise ValueError('Unexpected compiled publisher topology')
    for actual, expected in zip(steps, expected_steps):
        for key in ('name', 'image', 'command', 'args'):
            got = actual[key]
            if key == 'args':
                got = [x.replace('$(params.run-uid)', uid) for x in got]
            if got != expected[key]:
                raise ValueError('Compiled publisher differs from closed recipe')
    return {'results': results, 'publisherImage': image, 'publishRunUID': uid,
            'definitionDigest': results['DEFINITION_DIGEST'], 'sourceSHA': source,
            'closedSteps': expected_steps, 'scope': 'isolated closed transport fixture'}


def verified_statement(raw, expected, image, steps):
    envelope = provenance.strict_json(raw)
    import base64
    if not isinstance(envelope, dict) or envelope.get('payloadType') != 'application/vnd.in-toto+json' or not envelope.get('signatures'):
        raise ValueError('Missing cryptographically verified DSSE envelope')
    try:
        statement = provenance.strict_json(base64.b64decode(envelope['payload'], validate=True))
    except (KeyError, TypeError, ValueError):
        raise ValueError('Invalid verified envelope') from None
    bind_statement(statement, expected, image, steps)
    return statement


def bind_statement(statement, expected, image, steps):
    task = provenance.bind_common(statement, expected, image)
    try:
        if len(task['steps']) != 2 or len(steps) != 2:
            raise ValueError('Unexpected closed signed task topology')
        for actual, wanted in zip(task['steps'], steps):
            if (actual['entryPoint'] != '/component' or actual['arguments'] != wanted['args']
                    or actual['environment']['image'] != 'oci://' + image
                    or actual['environment']['container'] != wanted['name']):
                raise ValueError('Signed closed step differs from expected recipe')
    except (KeyError, TypeError, IndexError, AttributeError):
        raise ValueError('Incomplete closed provenance identity') from None


def definition_digest(documents,profile,slot,definition):
    templates = copy.deepcopy(documents)
    for phase in ('build','publish'):
        annotations = templates[phase]['metadata']['annotations']
        if annotations['ai-sdlc.definition-sha256'] != definition:
            raise ValueError('Compiled phase definition differs')
        annotations['ai-sdlc.definition-sha256'] = 'DEFINITION'
    publish_args = templates['publish']['spec']['pipelineSpec']['tasks'][0]['taskSpec']['steps'][1]['args']
    if publish_args[4] != '--definition-digest=sha256:' + definition:
        raise ValueError('Compiled publisher definition differs')
    publish_args[4] = '--definition-digest=sha256:DEFINITION'
    template = {'version':'ai-sdlc.saferus-build-definition/v1',
                'profileSHA256':hashlib.sha256(go_json(profile)).hexdigest(),
                'slotDigest':hashlib.sha256(go_json(slot)).hexdigest(),
                'build':templates['build'],'publish':templates['publish']}
    return hashlib.sha256(go_json(template, sort_keys=True)).hexdigest()

