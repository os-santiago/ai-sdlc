#!/usr/bin/env python3
"""Disposable gVisor/SCC mailbox fixture. Never injects or advances live work."""
import argparse
import base64
import concurrent.futures
import copy
import json
from pathlib import Path
import secrets
import subprocess
import time

import install
import network_probe
import offline_probe

BODY = {'model': 'fixture-model', 'stream': False,
        'messages': [{'role': 'user', 'content': 'read-only fixture'}]}
REPLY = {'id': 'fixture', 'object': 'chat.completion', 'model': 'fixture-model',
         'choices': [{'index': 0, 'finish_reason': 'stop',
                      'message': {'role': 'assistant', 'content': 'A loopback-only mailbox carries bounded model data while provider credentials remain outside the executor. No changes are required for this read-only question.'}}]}
HTTP = """const body=process.argv[1]==='oversize-fixture'?'x'.repeat(128*1024+1):process.argv[1]; fetch('http://127.0.0.1:8091/v1/chat/completions',
{method:'POST',headers:{'Content-Type':'application/json'},body,signal:AbortSignal.timeout(20000)})
.then(async r=>console.log(JSON.stringify({status:r.status,body:await r.json()})))
.catch(()=>process.exit(2));"""


def fixture(namespace, mailbox_image, worker_image):
    value = offline_probe.pod(namespace, 'executor', worker_image, 'ai-sdlc-offline')
    spec = value['spec']
    spec['volumes'] += [
        {'name': 'bridge', 'emptyDir': {'sizeLimit': '16Mi'}},
        {'name': 'pod-identity', 'downwardAPI': {'items': [
            {'path': 'uid', 'fieldRef': {'fieldPath': 'metadata.uid'}}]}}]
    client = spec['containers'][0]
    client['volumeMounts'].append({'name': 'bridge', 'mountPath': '/bridge'})
    mailbox = copy.deepcopy(client)
    mailbox.update(name='mailbox', image=mailbox_image, imagePullPolicy='Never',
                   command=['/model-mailbox', 'serve', '--attempt', 'fixture-461',
                            '--pod-uid', '$(POD_UID)'],
                   env=[{'name': 'POD_UID', 'valueFrom': {'fieldRef': {'fieldPath': 'metadata.uid'}}}],
                   volumeMounts=[{'name': 'bridge', 'mountPath': '/bridge'},
                                 {'name': 'pod-identity', 'mountPath': '/var/run/ai-sdlc/pod', 'readOnly': True}])
    spec['containers'].append(mailbox)
    return value


def verify(kubectl, mailbox_image, worker_image):
    namespace = 'ai-sdlc-mailboxcheck-' + secrets.token_hex(6)
    namespace_uid = None

    def command(args, value=None, expected=0, timeout=30):
        result = subprocess.run([str(part) for part in kubectl] + args,
                                input=json.dumps(value) if value is not None else None,
                                text=True, capture_output=True, timeout=timeout)
        if result.returncode != expected:
            raise ValueError('Mailbox fixture command failed: ' + ' '.join(args[:5])
                             + '; stderr=' + result.stderr[:300])
        return result.stdout

    def node(code, *args, expected=0):
        return command(['-n', namespace, 'exec', 'executor', '-c', 'probe', '--',
                        'node', '-e', code, *args], expected=expected, timeout=45)

    def helper(mode, value=None, expected=0, uid=None):
        return command(['-n', namespace, 'exec', '-i', 'executor', '-c', 'mailbox', '--',
                        '/model-mailbox', mode, '--attempt', 'fixture-461',
                        '--pod-uid', uid or pod_uid], value, expected)

    def pending(wait=True):
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            result = subprocess.run([str(part) for part in kubectl] + [
                '-n', namespace, 'exec', 'executor', '-c', 'mailbox', '--',
                '/model-mailbox', 'read', '--attempt', 'fixture-461', '--pod-uid', pod_uid],
                text=True, capture_output=True, timeout=10)
            if result.returncode == 0:
                return json.loads(result.stdout)
            if result.returncode != 3:
                raise ValueError('Mailbox helper could not read request')
            if not wait:
                return None
            time.sleep(.1)
        raise ValueError('Mailbox fixture request was not published')

    def respond(request):
        response = dict(request, kind='response', status=200,
                        payload=base64.b64encode(json.dumps(REPLY).encode()).decode())
        helper('respond', response)
        return response

    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-mailbox-conformance',
                       'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        namespace_uid = created['metadata']['uid']
        command(['create', '-f', '-'], fixture(namespace, mailbox_image, worker_image))
        command(['-n', namespace, 'wait', '--for=condition=Ready', 'pod/executor', '--timeout=90s'], timeout=100)
        pod = json.loads(command(['-n', namespace, 'get', 'pod', 'executor', '-o', 'json']))
        pod_uid = pod['metadata']['uid']
        observed_images = {item['name']: item['imageID'] for item in pod['status']['containerStatuses']}
        if set(observed_images) != {'probe', 'mailbox'} or not all(observed_images.values()):
            raise ValueError('Fixture image identity missing')
        local = offline_probe.checked_local(node(offline_probe.SCRIPT, 'local'))
        health = json.loads(node("fetch('http://127.0.0.1:8091/healthz').then(r=>r.json()).then(v=>console.log(JSON.stringify(v))).catch(()=>process.exit(2))"))
        if health != {'status': 'mailbox-only'}:
            raise ValueError('Mailbox did not start')
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(node, HTTP, json.dumps(BODY))
            try:
                request = pending()
            except ValueError as error:
                logs = command(['-n', namespace, 'logs', 'executor', '-c', 'mailbox', '--tail=3'])
                raise ValueError('Mailbox failed to publish: ' + future.result(timeout=25) + '; ' + logs[:500]) from error
            if json.loads(base64.b64decode(request['payload'])) != BODY:
                raise ValueError('Mailbox changed request bytes')
            helper('read', expected=1, uid='00000000-0000-0000-0000-000000000000')
            wrong = dict(request, kind='response', status=200, sequence=request['sequence'] + 1,
                         payload=base64.b64encode(json.dumps(REPLY).encode()).decode())
            helper('respond', wrong, expected=1)
            concurrent_result = json.loads(node(HTTP, json.dumps(BODY)))
            if concurrent_result['status'] != 429:
                raise ValueError('Mailbox accepted concurrent request')
            response = respond(request)
            result = json.loads(future.result(timeout=25))
            if result != {'status': 200, 'body': REPLY}:
                raise ValueError('Mailbox changed response')
            helper('respond', response, expected=1)
        oversized = json.loads(node(HTTP, 'oversize-fixture'))
        if oversized['status'] != 413:
            raise ValueError('Mailbox accepted oversized body')

        # Actual pinned SCC, synthetic model reply, disposable empty repository.
        # No GitHub issue, control-plane endpoint or live workflow is involved.
        setup = """const fs=require('fs'),cp=require('child_process');
fs.mkdirSync('/workspaces/home/.sc-agent',{recursive:true});
fs.mkdirSync('/workspaces/fixture',{recursive:true});
fs.mkdirSync('/workspaces/tmp',{recursive:true});
fs.writeFileSync('/workspaces/home/.sc-agent/config.json',JSON.stringify({model:{provider:'openai-compatible',baseUrl:'http://127.0.0.1:8091/v1',model:'fixture-model',stream:false}}));
for(const a of [['init','-q'],['config','user.name','Conformance Fixture'],['config','user.email','fixture@example.invalid'],['commit','-q','--allow-empty','-m','fixture']]) cp.execFileSync('git',['-C','/workspaces/fixture',...a]);
console.log(cp.execFileSync('git',['-C','/workspaces/fixture','rev-parse','HEAD'],{encoding:'utf8'}).trim());"""
        baseline_head = node(setup).strip()
        if len(baseline_head) != 40:
            raise ValueError('SCC fixture baseline missing')
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            scc = """const cp=require('child_process');const r=cp.spawnSync('scc',['chat','-yq','Explain in one sentence why a loopback-only model mailbox separates executor network access from provider credentials. This is a read-only question.','--no-commit','--max-steps','2','--max-seconds','30','--summary-file','/workspaces/scc-manifest.json'],{cwd:'/workspaces/fixture',encoding:'utf8',timeout:35000,killSignal:'SIGKILL',maxBuffer:262144});if(r.error)process.exit(2);console.log(JSON.stringify({exit:r.status,diagnostic:(r.stderr||'').slice(-1500)}));"""
            future = pool.submit(node, scc)
            try:
                request = pending()
            except ValueError as error:
                raise ValueError('SCC fixture did not request model: ' + future.result(timeout=25)) from error
            respond(request)
            deadline = time.monotonic() + 25
            while not future.done():
                extra = pending(wait=False)
                if extra and extra['sequence'] != request['sequence']:
                    body = json.loads(base64.b64decode(extra['payload']))
                    raise ValueError('Unexpected SCC followup: ' + json.dumps(body.get('messages', [])[-1:])[:1200])
                if time.monotonic() >= deadline:
                    raise ValueError('SCC fixture exceeded completion deadline')
                time.sleep(.1)
            scc_result = json.loads(future.result())
            manifest = json.loads(node("console.log(require('fs').readFileSync('/workspaces/scc-manifest.json','utf8'))"))
            if scc_result.get('exit') != 10 or manifest.get('files_changed') != 0:
                raise ValueError('SCC did not complete the read-only fixture')
            actual = json.loads(node("const cp=require('child_process');const git=(...a)=>cp.execFileSync('git',['-C','/workspaces/fixture',...a],{encoding:'utf8'});console.log(JSON.stringify({head:git('rev-parse','HEAD').trim(),status:git('status','--porcelain=v1','-z'),diff:git('diff','HEAD','--'),untracked:git('ls-files','--others','--exclude-standard','-z')}));"))
            if actual != {'head': baseline_head, 'status': '', 'diff': '', 'untracked': ''}:
                raise ValueError('SCC fixture changed Git state despite manifest')
        report = dict(local, scope='isolated-component-fixture', requestResponseMatched=True,
                      wrongPodUIDRejected=True, wrongSequenceRejected=True, concurrentRequestRejected=True,
                      lateResponseRejected=True, oversizedRequestRejected=True, sccNoChangeExit=10,
                      sccGitStateIndependentlyUnchanged=True,
                      observedPodUID=pod_uid, autonomousExecutionEnabled=False,
                      observedContainerImageIDs=observed_images,
                      mailboxImage=mailbox_image, workerImage=worker_image)
    finally:
        if namespace_uid:
            network_probe.cleanup_namespace(command, namespace, namespace_uid)
    report['temporaryResourcesRemoved'] = True
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kubectl', required=True)
    parser.add_argument('--kubeconfig', required=True)
    parser.add_argument('--mailbox-image', required=True, help='local fixture image already loaded into kind')
    args = parser.parse_args()
    lock = install.image_lock(Path(__file__).resolve().parents[1] / 'runtime/k8s/deploy/local/images.lock.json')
    worker = lock['image']['repository'] + '@' + lock['components']['worker']['digest']
    print(json.dumps(verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.mailbox_image, worker), indent=2))


if __name__ == '__main__':
    main()
