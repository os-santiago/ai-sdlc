#!/usr/bin/env python3
"""Round-trip a pinned source snapshot in a disposable offline Pod; no live work."""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import re
import secrets
import subprocess
import time

import install
import network_probe
import offline_probe
import verify_model_mailbox

VERSION = 'ai-sdlc.workspace-bundle/v1'


def source_bundle(repository, sha):
    if not re.fullmatch('[a-f0-9]{40}', sha):
        raise ValueError('An exact source commit is required')
    git = ['git', '--no-replace-objects', '-C', str(repository)]

    def read(*args):
        return subprocess.run(git + list(args), check=True, capture_output=True, timeout=30).stdout

    if read('rev-parse', sha + '^{commit}').decode().strip() != sha:
        raise ValueError('Source commit mismatch')
    records = read('ls-tree', '-r', '-z', '-l', sha).split(b'\0')
    files = []
    total = 0
    for record in records:
        if not record:
            continue
        metadata, name = record.split(b'\t', 1)
        mode, kind, oid, size = metadata.split()
        if mode not in (b'100644', b'100755') or kind != b'blob':
            raise ValueError('Fixture source must contain regular files only')
        size = int(size)
        total += size
        if size > 4*1024*1024 or total > 16*1024*1024 or len(files) >= 4096:
            raise ValueError('Fixture source exceeds workspace limits')
        data = read('cat-file', 'blob', oid.decode())
        if len(data) != size:
            raise ValueError('Source object size mismatch')
        files.append({'path': name.decode('utf-8'), 'mode': int(mode[-3:], 8),
                      'data': base64.b64encode(data).decode('ascii')})
    files.sort(key=lambda item: item['path'])
    return {'version': VERSION, 'sourceSHA': sha, 'files': files}


def wire(value):
    return json.dumps(value, ensure_ascii=False, separators=(',', ':')).encode()


def identities(bundle):
    result = {}
    for item in bundle['files']:
        if set(item) != {'path', 'mode', 'data'} or item['path'] in result:
            raise ValueError('Invalid exported file identity')
        data = base64.b64decode(item['data'], validate=True)
        result[item['path']] = (item['mode'], len(data), hashlib.sha256(data).hexdigest())
    return result


def writer_exit(observed, pod_uid, container_id):
    if observed['metadata']['uid'] != pod_uid:
        raise ValueError('Workspace writer Pod was replaced')
    worker = next(item for item in observed['status']['containerStatuses'] if item['name'] == 'probe')
    if worker.get('restartCount', 0) != 0 or worker.get('containerID') != container_id:
        raise ValueError('Workspace writer restarted or changed container identity')
    terminated = worker.get('state', {}).get('terminated')
    if terminated is None:
        return False
    if terminated.get('exitCode') != 0:
        raise ValueError('Workspace writer did not terminate cleanly')
    return True


def verify(kubectl, mailbox_image, worker_image, bundle):
    namespace = 'ai-sdlc-workspacecheck-' + secrets.token_hex(6)
    namespace_uid = None
    payload = wire(bundle)
    digest = hashlib.sha256(payload).hexdigest()
    started = time.monotonic()

    def command(args, value=None, expected=0, timeout=75):
        data = wire(value) if isinstance(value, dict) else value
        result = subprocess.run([str(part) for part in kubectl] + args,
                                input=data, capture_output=True, timeout=timeout)
        allowed = expected if isinstance(expected, tuple) else (expected,)
        if result.returncode not in allowed:
            raise ValueError('Workspace fixture command failed: ' + ' '.join(args[:5])
                             + '; exit=' + str(result.returncode) + '; stdoutBytes=' + str(len(result.stdout)) + '; stderr=' + result.stderr[:300].decode(errors='replace'))
        return result.stdout

    def node(code):
        return command(['-n', namespace, 'exec', 'executor', '-c', 'probe', '--', 'node', '-e', code])

    def helper(mode, raw=None, expected=0, uid=None, expected_digest=None):
        args = ['-n', namespace, 'exec', '-i', 'executor', '-c', 'mailbox', '--',
                '/workspace-transfer', mode, '--pod-uid', uid or pod_uid,
                '--source-sha', bundle['sourceSHA']]
        if mode == 'import':
            args += ['--digest', expected_digest or hashlib.sha256(raw).hexdigest()]
        return command(args, raw if raw is not None else b'', expected)

    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-workspace-conformance',
                       'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        namespace_uid = created['metadata']['uid']
        pod = verify_model_mailbox.fixture(namespace, mailbox_image, worker_image)
        pod['spec']['containers'][0]['command'] = [
            'node', '-e', "const fs=require('fs');setInterval(()=>{if(fs.existsSync('/workspaces/fixture-stop'))process.exit(0)},100)"]
        pod['spec']['containers'][1]['volumeMounts'].append({'name': 'workspace', 'mountPath': '/workspaces'})
        command(['create', '-f', '-'], pod)
        command(['-n', namespace, 'wait', '--for=condition=Ready', 'pod/executor', '--timeout=90s'], timeout=100)
        observed = json.loads(command(['-n', namespace, 'get', 'pod', 'executor', '-o', 'json']))
        pod_uid = observed['metadata']['uid']
        images = {item['name']: item['imageID'] for item in observed['status']['containerStatuses']}
        worker_container_id = next(item['containerID'] for item in observed['status']['containerStatuses']
                                   if item['name'] == 'probe')
        if not worker_container_id:
            raise ValueError('Workspace writer container identity is missing')
        # Existing conformance checks establish the actual gVisor/credential boundary.
        local = offline_probe.checked_local(command(['-n', namespace, 'exec', 'executor', '-c', 'probe', '--',
                                                     'node', '-e', offline_probe.SCRIPT, 'local']))
        helper('import', b'', expected=1, uid='00000000-0000-0000-0000-000000000000', expected_digest=digest)
        helper('import', payload, expected=1, expected_digest='0'*64)
        for path in ('../outside', '/outside', 'a/../b', '.git/config', 'a/.GIT/config'):
            invalid = {'version': VERSION, 'sourceSHA': bundle['sourceSHA'],
                       'files': [{'path': path, 'mode': 420, 'data': 'eA=='}]}
            helper('import', wire(invalid), expected=1)
        invalid = {'version': VERSION, 'sourceSHA': bundle['sourceSHA'],
                   'files': [{'path': 'a', 'mode': 420, 'data': ''}, {'path': 'a/b', 'mode': 420, 'data': ''}]}
        helper('import', wire(invalid), expected=1)
        if node("console.log(require('fs').existsSync('/workspaces/repository'))").strip() != b'false':
            raise ValueError('Invalid input created workspace before validation')
        imported = json.loads(helper('import', payload))
        if imported != {'imported': True, 'files': len(bundle['files']), 'sourceSHA': bundle['sourceSHA'], 'bundleSHA256': digest}:
            raise ValueError('Import receipt mismatch')
        worker_files = json.loads(node("""const fs=require('fs'),path=require('path'),crypto=require('crypto');
const root='/workspaces/repository',out={};
function walk(rel){for(const name of fs.readdirSync(path.join(root,rel))){const r=rel?rel+'/'+name:name,p=path.join(root,r),st=fs.lstatSync(p);if(st.isDirectory())walk(r);else if(st.isFile()){const b=fs.readFileSync(p);out[r]=[(st.mode&64)?493:420,b.length,crypto.createHash('sha256').update(b).digest('hex')]}else throw Error('unexpected source node')}}
walk('');console.log(JSON.stringify(out));"""))
        if {name: tuple(value) for name, value in worker_files.items()} != identities(bundle):
            raise ValueError('Worker cannot read the exact imported source tree')
        helper('import', payload, expected=1)
        initial_export = json.loads(helper('export'))
        if identities(initial_export) != identities(bundle):
            raise ValueError('Initial export differs: observed files=' + str(len(initial_export['files'])) + ', expected=' + str(len(bundle['files'])))
        hostile = [
            "fs.symlinkSync('/etc/passwd',p)",
            "fs.symlinkSync('/workspaces/repository',p)",
            "fs.writeFileSync('/workspaces/outside','private fixture');fs.linkSync('/workspaces/outside',p)",
            "require('child_process').execFileSync('mkfifo',[p])",
            "fs.writeFileSync(p,'');fs.truncateSync(p,4*1024*1024+1)",
        ]
        for index, setup in enumerate(hostile):
            node("const fs=require('fs'),p='/workspaces/repository/unsafe-fixture';" + setup)
            if index == 3:
                # gVisor denies host FIFOs and creates a synthetic FIFO in the
                # writer's filesystem. It is absent from the helper's mount.
                # Verify containment and exact regular-file bytes, not a false
                # claim that the helper rejected a node it cannot observe.
                if node("console.log(require('fs').lstatSync('/workspaces/repository/unsafe-fixture').isFIFO())").strip() != b'true':
                    raise ValueError('FIFO positive control did not execute')
                fifo_view = json.loads(helper('export'))
                if identities(fifo_view) != identities(bundle):
                    raise ValueError('Executor-local FIFO changed the exported regular-file tree')
            else:
                helper('export', expected=1)
            node("require('fs').unlinkSync('/workspaces/repository/unsafe-fixture')")
        # Deterministic component mutations in a new fixture-only directory.
        # No Homedir source file or live issue is modified by this test.
        fixture_path = '.ai-sdlc-transfer-fixture-' + secrets.token_hex(6)
        seeds = {'modified.txt': 'before\n', 'deleted.txt': 'remove\n', 'script.sh': '#!/bin/sh\nexit 0\n'}
        node("const fs=require('fs'),p='/workspaces/repository/'+" + json.dumps(fixture_path)
             + ";fs.mkdirSync(p);for(const [name,body] of Object.entries(" + json.dumps(seeds)
             + ")){fs.writeFileSync(p+'/'+name,body,{mode:0o644})}")
        seeded = dict(bundle, files=list(bundle['files']) + [
            {'path': fixture_path + '/' + name, 'mode': 420, 'data': base64.b64encode(body.encode()).decode()}
            for name, body in seeds.items()])
        if identities(json.loads(helper('export'))) != identities(seeded):
            raise ValueError('Helper cannot observe regular files created by worker')
        node("const fs=require('fs'),p='/workspaces/repository/'+" + json.dumps(fixture_path)
             + ";fs.writeFileSync(p+'/created.txt','created\\n');fs.writeFileSync(p+'/modified.txt','after\\n');"
             + "fs.unlinkSync(p+'/deleted.txt');fs.chmodSync(p+'/script.sh',0o755)")
        expected_candidate = dict(bundle, files=list(bundle['files']) + [
            {'path': fixture_path + '/created.txt', 'mode': 420, 'data': base64.b64encode(b'created\n').decode()},
            {'path': fixture_path + '/modified.txt', 'mode': 420, 'data': base64.b64encode(b'after\n').decode()},
            {'path': fixture_path + '/script.sh', 'mode': 493, 'data': base64.b64encode(seeds['script.sh'].encode()).decode()}])
        # Stop the only workspace writer and observe its container's actual exit.
        # PID 1 can observe the marker and exit before the signaling exec has
        # returned. That exec can then report SIGKILL (137). Neither status is
        # proof of writer exit: require the original container's clean observed
        # termination below before exporting any candidate bytes.
        command(['-n', namespace, 'exec', 'executor', '-c', 'probe', '--', 'node', '-e',
                 "require('fs').writeFileSync('/workspaces/fixture-stop','stop')"], expected=(0, 137))
        deadline = time.monotonic() + 15
        while True:
            observed = json.loads(command(['-n', namespace, 'get', 'pod', 'executor', '-o', 'json']))
            if writer_exit(observed, pod_uid, worker_container_id):
                break
            if time.monotonic() >= deadline:
                raise ValueError('Workspace writer still running')
            time.sleep(.1)
        exported = json.loads(helper('export'))
        if set(exported) != {'version', 'sourceSHA', 'files'} or exported['version'] != VERSION or exported['sourceSHA'] != bundle['sourceSHA']:
            raise ValueError('Export envelope mismatch')
        if identities(exported) != identities(expected_candidate):
            raise ValueError('External comparison differs from the exact synthetic candidate')
        report = dict(local, scope='isolated-component-fixture', sourceSHA=bundle['sourceSHA'],
                      sourceBundleSHA256=digest, sourceFiles=len(bundle['files']),
                      sourceBytes=sum(value[1] for value in identities(bundle).values()),
                      filesAndModesIndependentlyEqual=True, workerReadExactSource=True,
                      syntheticCandidateOnly=True, syntheticCandidateOperations=['create', 'modify', 'delete', 'owner-executable-bit'],
                      writerTerminationObserved=True,
                      invalidImportsLeftNoWorkspace=True, existingWorkspaceRejected=True,
                      hostileExportNodesRejected=len(hostile)-1, executorLocalFIFOExcludedFromBundle=True, podUID=pod_uid,
                      observedContainerImageIDs=images, autonomousExecutionEnabled=False,
                      durationSeconds=round(time.monotonic()-started, 3))
    finally:
        if namespace_uid:
            network_probe.cleanup_namespace(command, namespace, namespace_uid)
    report['temporaryResourcesRemoved'] = True
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kubectl', required=True)
    parser.add_argument('--kubeconfig', required=True)
    parser.add_argument('--mailbox-image', required=True)
    parser.add_argument('--source-repository', required=True, type=Path)
    parser.add_argument('--source-sha', required=True)
    args = parser.parse_args()
    bundle = source_bundle(args.source_repository, args.source_sha)
    lock = install.image_lock(Path(__file__).resolve().parents[1] / 'runtime/k8s/deploy/local/images.lock.json')
    worker = lock['image']['repository'] + '@' + lock['components']['worker']['digest']
    print(json.dumps(verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.mailbox_image, worker, bundle), indent=2))


if __name__ == '__main__':
    main()
