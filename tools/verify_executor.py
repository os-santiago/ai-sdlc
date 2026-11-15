#!/usr/bin/env python3
"""Real SCC launcher fixture with synthetic model replies; never advances live work."""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import secrets
import struct
import subprocess
import time
import uuid

import network_probe
import offline_probe
from verify_workspace_bundle import identities, source_bundle, wire

FIXTURE_PATH = 'AI_SDLC_EXECUTOR_FIXTURE.txt'
FIXTURE_BODY = b'Created by the isolated SCC executor conformance fixture.\n'
VARIANTS = ('normal', 'source-token-override', 'source-profile-override',
            'source-stream-override', 'persistent-child', 'source-tree-tamper',
            'profile-digest-tamper', 'policy-digest-tamper', 'scc-timeout')


def tree_digest(bundle):
    digest = hashlib.sha256(b'ai-sdlc.workspace-tree/v1\0')
    digest.update(bytes.fromhex(bundle['sourceSHA']))
    digest.update(struct.pack('>I', len(bundle['files'])))
    for item in sorted(bundle['files'], key=lambda value: value['path'].encode()):
        path, data = item['path'].encode(), base64.b64decode(item['data'], validate=True)
        digest.update(struct.pack('>I', len(path)) + path)
        digest.update(struct.pack('>IQ', item['mode'], len(data)))
        digest.update(hashlib.sha256(data).digest())
    return digest.hexdigest()


def verify(kubectl, provider, render_image, mailbox_image, worker_image, bundle, variant="normal"):
    if variant not in VARIANTS:
        raise ValueError('Unsupported executor fixture variant')
    rejected_profile = variant in ("source-token-override", "source-profile-override", "source-stream-override")
    preflight_reject = variant in ('profile-digest-tamper', 'policy-digest-tamper')
    tree_reject = variant == 'source-tree-tamper'
    timeout_case = variant == 'scc-timeout'
    if rejected_profile:
        if ".sc-agent.json" in identities(bundle):
            raise ValueError("Config override fixture requires source without .sc-agent.json")
        config = {"model": {"maxTokens": 1000000}}
        if variant == "source-profile-override":
            config = {"activeProfile": "hostile", "profiles": {"hostile": {"maxTokens": 1000000}}}
        elif variant == "source-stream-override":
            config = {"model": {"stream": True}}
        bundle = dict(bundle, files=list(bundle["files"]) + [{"path": ".sc-agent.json", "mode": 420,
                      "data": base64.b64encode(wire(config)).decode()}])
    namespace = 'ai-sdlc-executorcheck-' + secrets.token_hex(6)
    namespace_uid = None
    started = time.monotonic()
    if FIXTURE_PATH in identities(bundle):
        raise ValueError('Source already contains the fixture file')
    adev = Path(__file__).resolve().parents[1].joinpath(
        'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/files/adev-profile.yaml').read_bytes()
    profile = {'version': 'ai-sdlc.executor-profile/v1', 'model': 'fixture-model',
               'maxSteps': 4, 'maxSeconds': 45, 'maxTokens': 2048,
               'aDevSHA256': hashlib.sha256(adev).hexdigest()}
    if timeout_case:
        profile['maxSeconds'] = 10
    profile_raw = wire(profile)
    assignment = {'version': 'ai-sdlc.executor-assignment/v1', 'workItemId': str(uuid.uuid4()),
                  'attempt': 1, 'repository': 'os-santiago/homedir', 'sourceSHA': bundle['sourceSHA'],
                  'treeSHA256': tree_digest(bundle), 'profileSHA256': hashlib.sha256(profile_raw).hexdigest(),
                  'authorityRevision': hashlib.sha256(b'isolated synthetic component fixture').hexdigest(),
                  'issueNumbers': [1], 'title': 'Isolated executor conformance fixture',
                  'body': 'Synthetic component test, not a real issue. Create ' + FIXTURE_PATH,
                  'acceptanceCriteria': ['Create ' + FIXTURE_PATH + ' containing exactly: ' + FIXTURE_BODY.decode(),
                                         'Preserve every original source file.'],
                  'expiresAt': int((time.time() + 300) * 1000)}
    assignment_raw = wire(assignment)
    assignment_digest = hashlib.sha256(assignment_raw).hexdigest()
    render = subprocess.run([provider, 'run', '--rm', '--network', 'none', '--pull', 'never',
                             '--entrypoint', '/executor-render', '-i', render_image],
                            input=wire({'namespace': namespace, 'workerImage': worker_image,
                                        'mailboxImage': mailbox_image, 'assignment': assignment_raw.decode(),
                                        'profile': profile_raw.decode(), 'adevProfile': adev.decode()}),
                            capture_output=True, timeout=15)
    if render.returncode:
        raise ValueError('Fixed workload compiler rejected fixture')
    workload = json.loads(render.stdout)
    if variant == 'profile-digest-tamper':
        workload['configMap']['data']['profile.json'] = wire(dict(profile, maxTokens=2049)).decode()
    elif variant == 'policy-digest-tamper':
        workload['configMap']['data']['adev-profile.yaml'] += '\n# tampered after digest binding\n'
    imported_bundle = bundle
    if tree_reject:
        imported_bundle = dict(bundle, files=list(bundle['files']) + [
            {'path': 'AI_SDLC_TAMPER.txt', 'mode': 420, 'data': base64.b64encode(b'tampered source\n').decode()}])
    pod_name = workload['pod']['metadata']['name']
    attempt = assignment['workItemId'] + '-1'

    def command(args, value=None, expected=0, timeout=75):
        data = wire(value) if isinstance(value, dict) else value
        result = subprocess.run([str(part) for part in kubectl] + args,
                                input=data, capture_output=True, timeout=timeout)
        if result.returncode != expected:
            raise ValueError('Executor fixture command failed: ' + ' '.join(args[:5])
                             + '; exit=' + str(result.returncode) + '; stderr='
                             + result.stderr[:300].decode(errors='replace'))
        return result.stdout

    def helper(binary, args, value=None, expected=0):
        return command(['-n', namespace, 'exec', '-i', pod_name, '-c', 'mailbox', '--', binary] + args,
                       value, expected)

    def observe():
        pod = json.loads(command(['-n', namespace, 'get', 'pod', pod_name, '-o', 'json'], timeout=10))
        if pod['metadata']['uid'] != pod_uid:
            raise ValueError('Executor Pod was replaced')
        return pod

    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-executor-conformance',
                       'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        namespace_uid = created['metadata']['uid']
        command(['create', '-f', '-'], workload['configMap'])
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], workload['pod']))
        pod_uid = created['metadata']['uid']
        if preflight_reject:
            deadline = time.monotonic() + 90
            while True:
                pod = observe()
                states = {item['name']: item for item in pod.get('status', {}).get('containerStatuses', [])}
                if ('running' in states.get('mailbox', {}).get('state', {})
                        and 'terminated' in states.get('worker', {}).get('state', {})):
                    break
                if time.monotonic() >= deadline:
                    raise ValueError('Bad policy was not rejected within startup bound')
                time.sleep(.1)
            if states['worker']['state']['terminated']['exitCode'] != 1:
                raise ValueError('Bad profile did not fail closed')
            helper('/model-mailbox', ['read', '--attempt', attempt, '--pod-uid', pod_uid], expected=3)
            logs = command(['-n', namespace, 'logs', pod_name, '-c', 'mailbox'])
            if b'model request published sequence=' in logs:
                raise ValueError('Bad policy reached model publication')
            report = dict(scope='isolated-component-fixture', syntheticIssueOnly=True,
                          liveWorkflowAdvanced=False, variant=variant, modelCalls=0,
                          reviewedInputDigestRejected=True, workerTerminatedObserved=True,
                          workerExitCode=1, observedPodUID=pod_uid,
                          observedContainerImageIDs={name: item['imageID'] for name, item in states.items()},
                          sourceImported=False, candidateExported=False,
                          assignmentSHA256=assignment_digest, fixedTemplateUsed=True,
                          tamperedInputAfterCompilation=True)
        else:
            command(['-n', namespace, 'wait', '--for=condition=Ready', 'pod/' + pod_name, '--timeout=90s'], timeout=100)
            pod = observe()
            image_ids = {item['name']: item['imageID'] for item in pod['status']['containerStatuses']}
            # Read-only conformance before the exclusive start marker. After start,
            # the fixture never execs a command into the worker container.
            local = offline_probe.checked_local(command(['-n', namespace, 'exec', pod_name, '-c', 'worker', '--',
                                                         'node', '-e', offline_probe.SCRIPT, 'local']))
            helper('/model-mailbox', ['read', '--attempt', attempt, '--pod-uid', pod_uid], expected=3)
            payload = wire(imported_bundle)
            helper('/workspace-transfer', ['import', '--pod-uid', pod_uid, '--source-sha', bundle['sourceSHA'],
                                          '--digest', hashlib.sha256(payload).hexdigest()], payload)
            helper('/executor-control', ['start', '00000000-0000-0000-0000-000000000000', assignment_digest,
                                         assignment['treeSHA256']], expected=1)
            helper('/executor-control', ['start', pod_uid, assignment_digest, assignment['treeSHA256']])
            helper('/executor-control', ['start', pod_uid, assignment_digest, assignment['treeSHA256']], expected=1)
            seen = set()
            wrote = False
            execution_started = time.monotonic()
            deadline = execution_started + (profile['maxSeconds'] + 15 if timeout_case else 60)
            while True:
                pod = observe()
                if time.monotonic() >= deadline:
                    raise ValueError('Launcher fixture exceeded external deadline')
                state = next(item['state'] for item in pod['status']['containerStatuses'] if item['name'] == 'worker')
                if 'terminated' in state:
                    exit_code = state['terminated']['exitCode']
                    expected_exit = 124 if timeout_case else (1 if tree_reject or rejected_profile else 0)
                    if exit_code != expected_exit:
                        raise ValueError('SCC launcher exited ' + str(state['terminated']['exitCode'])
                                         + '; model calls=' + str(len(seen)))
                    break
                result = subprocess.run([str(part) for part in kubectl] + ['-n', namespace, 'exec', pod_name,
                                        '-c', 'mailbox', '--', '/model-mailbox', 'read', '--attempt', attempt,
                                        '--pod-uid', pod_uid], capture_output=True, timeout=10)
                if result.returncode == 0:
                    request = json.loads(result.stdout)
                    if rejected_profile or tree_reject:
                        raise ValueError('Source config escaped reviewed request constraints')
                    if request['sequence'] not in seen:
                        seen.add(request['sequence'])
                        body = json.loads(base64.b64decode(request['payload']))
                        if body['model'] != profile['model'] or body.get('stream') is not False:
                            raise ValueError('SCC escaped the fixed model/stream configuration')
                        if timeout_case:
                            # Hold the real request without replying. The launcher,
                            # not this harness timeout, must terminate SCC with 124.
                            if len(seen) != 1:
                                raise ValueError('Blocked SCC unexpectedly retried')
                            continue
                        if not wrote:
                            tools = {item['function']['name']: item['function'] for item in body.get('tools', [])}
                            if 'write_file' not in tools:
                                raise ValueError('Pinned SCC did not expose write_file; observed=' + ','.join(tools))
                            parameters = tools['write_file']['parameters']
                            if not {'path', 'content'} <= set(parameters.get('properties', {})):
                                raise ValueError('Pinned write_file schema differs: ' + json.dumps(parameters))
                            message = {'role': 'assistant', 'content': None, 'tool_calls': [{
                                'id': 'fixture-write', 'type': 'function', 'function': {'name': 'write_file',
                                'arguments': json.dumps({'path': FIXTURE_PATH, 'content': FIXTURE_BODY.decode()})}}]}
                            if variant == 'persistent-child':
                                if 'run_shell' not in tools:
                                    raise ValueError('Pinned SCC did not expose run_shell')
                                child_command = r'''(sleep 10; printf leaked > AI_SDLC_CHILD_LEAK.txt) >/dev/null 2>&1 </dev/null & child=$!; kill -0 "$child" && printf 'spawned\n' > AI_SDLC_CHILD_STARTED.txt'''
                                message['tool_calls'].append({'id': 'fixture-child', 'type': 'function',
                                    'function': {'name': 'run_shell', 'arguments': json.dumps({'command': child_command})}})
                            reason = 'tool_calls'
                            wrote = True
                        else:
                            message = {'role': 'assistant', 'content': 'Created the requested conformance fixture file and preserved the original source.'}
                            reason = 'stop'
                        reply = {'id': 'fixture-' + str(request['sequence']), 'object': 'chat.completion',
                                 'model': profile['model'], 'choices': [{'index': 0, 'finish_reason': reason, 'message': message}]}
                        helper('/model-mailbox', ['respond', '--attempt', attempt, '--pod-uid', pod_uid],
                               dict(request, kind='response', status=200, payload=base64.b64encode(wire(reply)).decode()))
                elif result.returncode != 3:
                    raise ValueError('Mailbox read failed')
                if time.monotonic() >= deadline:
                    raise ValueError('Launcher fixture exceeded external deadline')
                time.sleep(.1)
            logs = command(['-n', namespace, 'logs', pod_name, '-c', 'mailbox'])
            if logs.count(b'model request published sequence=') != len(seen):
                raise ValueError('Published request log does not match external observations')
            if rejected_profile:
                if seen or b'reviewed model request rejected' not in logs:
                    raise ValueError('Actual SCC request rejection was not observed')
            elif tree_reject:
                if seen:
                    raise ValueError('Source tree mismatch reached model')
            elif timeout_case:
                if len(seen) != 1 or wrote:
                    raise ValueError('Timeout did not interrupt an actual pending model call')
            elif not wrote or len(seen) < 2:
                raise ValueError('SCC tool loop was not exercised')
            if variant == 'persistent-child':
                # The child has an independent process group. Only observed PID-1
                # container termination is sufficient before waiting for its deadline.
                time.sleep(11)
                observe()
            exported = json.loads(helper('/workspace-transfer', ['export', '--pod-uid', pod_uid,
                                                                 '--source-sha', bundle['sourceSHA']]))
            expected = dict(bundle, files=list(bundle['files']) + [
                {'path': FIXTURE_PATH, 'mode': 420, 'data': base64.b64encode(FIXTURE_BODY).decode()}])
            if rejected_profile or tree_reject or timeout_case:
                expected = imported_bundle
            elif variant == 'persistent-child':
                expected['files'].append({'path': 'AI_SDLC_CHILD_STARTED.txt', 'mode': 420,
                                          'data': base64.b64encode(b'spawned\n').decode()})
            if identities(exported) != identities(expected):
                actual_names, expected_names = set(identities(exported)), set(identities(expected))
                raise ValueError('Actual SCC candidate differs; added=' + str(sorted(actual_names - expected_names))
                                 + '; missing=' + str(sorted(expected_names - actual_names)))
            report = dict(local, scope='isolated-component-fixture', syntheticModelReplies=bool(wrote),
                          syntheticIssueOnly=True, liveWorkflowAdvanced=False, originalSourceFilesPreserved=True,
                          sourceSHA=bundle['sourceSHA'], sourceFiles=len(bundle['files']), sourceTreeSHA256=assignment['treeSHA256'],
                          modelCalls=len(seen), workerTerminatedObserved=True, workerExitCode=exit_code,
                          variant=variant, reviewedRequestOverrideRejected=rejected_profile,
                          sourceIdentityRejected=tree_reject, launcherTimeoutObserved=timeout_case,
                          executionObservedSeconds=round(time.monotonic() - execution_started, 3),
                          persistentChildContained=variant == 'persistent-child',
                          candidateIndependentlyCompared=True, createdFixtureFile=None if rejected_profile or tree_reject or timeout_case else FIXTURE_PATH,
                          observedPodUID=pod_uid, observedContainerImageIDs=image_ids,
                          assignmentSHA256=assignment_digest, fixedTemplateUsed=True,
                          wrongPodStartRejected=True, repeatedStartRejected=True)
    finally:
        if namespace_uid:
            network_probe.cleanup_namespace(command, namespace, namespace_uid)
    report.update(temporaryResourcesRemoved=True, durationSeconds=round(time.monotonic() - started, 3))
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kubectl', required=True)
    parser.add_argument('--kubeconfig', required=True)
    parser.add_argument('--provider', choices=('docker', 'podman'), required=True)
    parser.add_argument('--render-image', required=True)
    parser.add_argument('--mailbox-image', required=True, help='immutable image reference available in kind')
    parser.add_argument('--worker-image', required=True, help='immutable image reference available in kind')
    parser.add_argument('--source-repository', type=Path, required=True)
    parser.add_argument('--source-sha', required=True)
    parser.add_argument('--variant', choices=VARIANTS, default='normal')
    args = parser.parse_args()
    print(json.dumps(verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.provider,
                           args.render_image, args.mailbox_image, args.worker_image,
                           source_bundle(args.source_repository, args.source_sha), args.variant), indent=2))


if __name__ == '__main__':
    main()
