#!/usr/bin/env python3
"""Inspect unschedulable synthetic Tekton Pods and test mandatory Pod admission."""
import argparse
import base64
import copy
import json
from pathlib import Path
import secrets
import subprocess
import time

import konflux_fixture_guard as guard


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--compiled', type=Path, required=True)
    p.add_argument('--output', type=Path, required=True)
    p.add_argument('--cluster', default='ai-sdlc-konflux-reviewed')
    p.add_argument('--inspect-only', action='store_true')
    args = p.parse_args()
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    state, tools = guard.installation_paths(args.cluster)
    owner = json.loads((state / 'konflux-ownership.json').read_text())
    if owner['cluster'] != args.cluster or owner['scope'] != 'isolated-konflux':
        raise ValueError('Owned isolated Konflux fixture required')
    kubectl = [str(tools / 'kubectl-1.35.8'), '--kubeconfig', str(state / 'kubeconfig')]
    namespace = 'ai-sdlc-builds'; nonce = secrets.token_hex(12)
    policy_name = 'ai-sdlc-build-pods-fixture-' + nonce
    uid = None; owned = []; run_uids = []
    deadline = time.monotonic() + 240
    report = {'version': 1, 'status': 'failed', 'scope': 'nonexecuting synthetic Pod admission fixture',
              'autonomousE2E': False, 'workloadExecuted': False, 'accepted': [], 'rejected': [],
              'cleanupComplete': False, 'preAdmissionDenials': []}

    def persist_report():
        temporary=args.output / 'report.pending.json'
        temporary.write_text(json.dumps(report,indent=2)+'\n')
        temporary.replace(args.output / 'report.json')

    def call(argv, value=None, check=True, actor=False):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Admission fixture deadline exceeded')
        prefix = kubectl + (['--as=system:serviceaccount:tekton-pipelines:tekton-pipelines-controller'] if actor else [])
        return subprocess.run(prefix + argv, input=json.dumps(value) if value is not None else None,
                              capture_output=True, text=True, check=check, timeout=min(30, remaining))

    def get(kind, name=None, ns=namespace):
        return json.loads(call(['get', kind] + ([name] if name else []) + (['-n', ns] if ns else []) + ['-o', 'json']).stdout)

    def create(value):
        return json.loads(call(['create', '-f', '-', '-o', 'json'], value).stdout)

    def pods_stopped():
        pods = get('pods')['items']
        for pod in pods:
            spec = pod['spec']; status = pod.get('status', {})
            if (spec.get('nodeSelector', {}).get('kubernetes.io/hostname') != 'fixture-node'
                    or spec.get('nodeName', '') not in ('', 'fixture-node') or status.get('containerStatuses') or status.get('initContainerStatuses')):
                raise ValueError('Synthetic Pod was scheduled or executed')
        return pods

    def dryrun(pod, expected, label):
        value = copy.deepcopy(pod)
        value.pop('status', None)
        for key in ('uid', 'resourceVersion', 'creationTimestamp', 'managedFields', 'deletionTimestamp'):
            value['metadata'].pop(key, None)
        value['metadata']['name'] = 'admission-' + label
        result = call(['create', '--dry-run=server', '-f', '-', '-o', 'json'], value, check=False, actor=True)
        if expected and result.returncode:
            raise ValueError('Valid Pod rejected: ' + result.stderr[-3000:])
        native_denial = (label.endswith('-host') and 'violates PodSecurity' in result.stderr) or (label.endswith('-runtimeclass') and 'RuntimeClass' in result.stderr and 'not found' in result.stderr)
        if not expected and native_denial: report['preAdmissionDenials'].append(label)
        if not expected and (result.returncode == 0 or ('Closed build Pod requirement' not in result.stderr and not native_denial)):
            raise ValueError('Negative control did not reach mandatory policy: ' + label + ' ' + result.stderr[-1000:])
        report['accepted' if expected else 'rejected'].append(label)

    try:
        if get('namespace', 'kube-system', None)['metadata']['uid'] != owner['clusterUID']:
            raise ValueError('Cluster UID changed')
        if any(n['metadata'].get('labels', {}).get('kubernetes.io/hostname') == 'fixture-node' for n in get('nodes', ns=None)['items']):
            raise ValueError('Fixture node selector could schedule')
        if any(n['metadata']['name'] == namespace for n in get('namespaces', ns=None)['items']):
            raise ValueError('Refusing existing build namespace')
        ns = create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
                     'labels': {'ai-sdlc.admission-fixture': nonce, 'pod-security.kubernetes.io/enforce': 'restricted'}}})
        uid = ns['metadata']['uid']
        report.update({'namespaceUID':uid,'ownerNonce':nonce,'namespace':namespace})
        persist_report()
        create({'apiVersion': 'v1', 'kind': 'LimitRange', 'metadata': {'name': 'build-budget', 'namespace': namespace}, 'spec': {'limits': [{'type': 'Container', 'default': {'cpu': '100m', 'memory': '256Mi', 'ephemeral-storage': '64Mi'}, 'defaultRequest': {'cpu': '100m', 'memory': '64Mi', 'ephemeral-storage': '64Mi'}}]}})
        # Deliberately synthetic credential, never usable against the real registry.
        dummy = base64.b64encode(b'{"auths":{"registry-service.kind-registry":{"auth":"Zml4dHVyZTpmaXh0dXJl"}}}').decode()
        create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'registry-auth', 'namespace': namespace},
                'type': 'kubernetes.io/dockerconfigjson', 'data': {'.dockerconfigjson': dummy}})
        for file, kind in [('mutation.json', 'mutatingadmissionpolicies'), ('mutation-binding.json','mutatingadmissionpolicybindings')]:
            value=json.loads((args.compiled / file).read_text()); value['metadata']['name']=policy_name
            value['metadata'].setdefault('labels',{})['ai-sdlc.admission-fixture']=nonce
            if file == 'mutation-binding.json': value['spec']['policyName']=policy_name
            obj=create(value);owned.append((kind,obj['metadata']['uid']))
        time.sleep(2)
        for phase in ('build', 'publish'):
            sa = {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': phase, 'namespace': namespace}, 'automountServiceAccountToken': False}
            if phase == 'publish': sa['secrets'] = [{'name': 'registry-auth'}]
            create(sa)
            raw = (args.compiled / (phase + '.json')).read_bytes()
            if len(raw) > 32768: raise ValueError('Compiled fixture exceeds limit')
            value = json.loads(raw)
            template = value['spec']['taskRunTemplate']['podTemplate']
            if (value['metadata']['namespace'] != namespace or value['kind'] != 'PipelineRun'
                    or template['nodeSelector'] != {'kubernetes.io/hostname': 'fixture-node'}):
                raise ValueError('Fixture must remain bound to an absent node')
            run = create(value); run_uids.append(run['metadata']['uid'])
            report['runUIDs']=list(run_uids);persist_report()
        until = time.monotonic() + 60
        while time.monotonic() < until:
            pods = pods_stopped()
            if len(pods) == 2: break
            time.sleep(1)
        else: raise ValueError('Tekton did not construct both pending Pods')
        if get('pvc')['items']: raise ValueError('Fixture unexpectedly provisioned storage')
        for pod in pods:
            phase = pod['metadata']['labels']['ai-sdlc.role']
            (args.output / (phase + '-pod.json')).write_text(json.dumps(pod, indent=2) + '\n')
        report['podShapeObserved'] = True
        if not args.inspect_only:
            for file, kind in [('policy.json', 'validatingadmissionpolicies'), ('binding.json', 'validatingadmissionpolicybindings')]:
                value = json.loads((args.compiled / file).read_text())
                value['metadata']['name'] = policy_name
                value['metadata'].setdefault('labels', {})['ai-sdlc.admission-fixture'] = nonce
                if file == 'binding.json': value['spec']['policyName'] = policy_name
                obj = create(value); owned.append((kind, obj['metadata']['uid']))
            time.sleep(2)
            observed = get('validatingadmissionpolicy', policy_name, None)
            warnings = observed.get('status', {}).get('typeChecking', {}).get('expressionWarnings', [])
            report['expressionWarnings'] = warnings
            if warnings: raise ValueError('Admission policy type checking warnings')
            for pod in pods:
                phase = pod['metadata']['labels']['ai-sdlc.role']
                dryrun(pod, True, phase)
                for label in ('image', 'argument', 'sidecar', 'secret', 'token', 'host', 'fsgroup', 'environment', 'grace', 'scheduler', 'runtimeclass', 'overshadow', 'limits', 'node'):
                    bad = copy.deepcopy(pod)
                    if label == 'image': bad['spec']['containers'][1]['image'] = 'example.invalid/untrusted:latest'
                    if label == 'argument': bad['spec']['containers'][0]['args'].append('--untrusted')
                    if label == 'sidecar':
                        extra = copy.deepcopy(bad['spec']['containers'][0]); extra['name'] = 'injected'; bad['spec']['containers'].append(extra)
                    if label == 'secret': bad['spec']['volumes'].append({'name': 'foreign', 'secret': {'secretName': 'foreign'}})
                    if label == 'token': bad['spec']['volumes'].append({'name': 'token', 'projected': {'sources': [{'serviceAccountToken': {'path': 'token'}}]}})
                    if label == 'host': bad['spec']['hostPID'] = True
                    if label == 'fsgroup': bad['spec']['securityContext']['fsGroup'] = 1000
                    if label == 'environment': bad['spec']['containers'][1].setdefault('env', []).append({'name': 'TOKEN', 'valueFrom': {'secretKeyRef': {'name': 'foreign', 'key': 'token'}}})
                    if label == 'grace': bad['spec']['terminationGracePeriodSeconds']=86400
                    if label == 'scheduler': bad['spec']['schedulerName']='foreign'
                    if label == 'runtimeclass': bad['spec']['runtimeClassName']='foreign'
                    if label == 'overshadow': bad['spec']['containers'][1]['volumeMounts'].append({'name':'tekton-internal-workspace','mountPath':'/slot/workspace'})
                    if label == 'limits': bad['spec']['containers'][1]['resources']['limits']['memory']='8Gi'
                    if label == 'node': bad['spec']['nodeName']='fixture-node'
                    dryrun(bad, False, phase + '-' + label)
            for pod in pods:
                name=pod['metadata']['name']; phase=pod['metadata']['labels']['ai-sdlc.role']
                # Binding only to a proven absent node: no kubelet can execute this fixture.
                call(['create','--raw','/api/v1/namespaces/'+namespace+'/pods/'+name+'/binding','-f','-'],
                     {'apiVersion':'v1','kind':'Binding','metadata':{'name':name,'namespace':namespace},'target':{'apiVersion':'v1','kind':'Node','name':'fixture-node'}})
                bound=get('pod',name);bound['metadata'].setdefault('annotations',{})['tekton.dev/ready']='READY'
                result=call(['replace','--dry-run=server','-f','-','-o','json'],bound,check=False,actor=True)
                if result.returncode: raise ValueError('Bound READY update rejected: '+result.stderr[-2000:])
                report['accepted'].append(phase+'-bound-ready-update')
                ephemeral=copy.deepcopy(bound)
                entry=copy.deepcopy(bound['spec']['containers'][0])
                for field in ('resources','volumeMounts','terminationMessagePath','terminationMessagePolicy'):
                    entry.pop(field,None)
                entry['name']='foreign-ephemeral';entry['targetContainerName']='step-custody'
                ephemeral['spec']['ephemeralContainers']=[entry]
                result=call(['replace','--raw','/api/v1/namespaces/'+namespace+'/pods/'+name+'/ephemeralcontainers?dryRun=All','-f','-'],ephemeral,check=False,actor=True)
                if result.returncode==0 or 'Closed build Pod requirement' not in result.stderr:
                    raise ValueError('Ephemeral subresource did not reach mandatory denial: '+result.stderr[-1500:])
                report['rejected'].append(phase+'-ephemeral-update')
            binding_kind='mutatingadmissionpolicybindings'
            binding_uid=next(resource_uid for kind,resource_uid in owned if kind==binding_kind)
            call(['delete','--raw','/apis/admissionregistration.k8s.io/v1/'+binding_kind+'/'+policy_name,'-f','-'],
                 {'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':binding_uid}})
            owned.remove((binding_kind,binding_uid))
            time.sleep(2)
            for pod in pods:
                phase=pod['metadata']['labels']['ai-sdlc.role']
                for label in ('deadline-without-mutator','storage-without-mutator'):
                    bad=copy.deepcopy(pod)
                    if label.startswith('deadline'):bad['spec']['activeDeadlineSeconds']=5400
                    else:
                        for volume in bad['spec']['volumes']:
                            if 'emptyDir' in volume:volume['emptyDir']={}
                    dryrun(bad,False,phase+'-'+label)
            report['mandatoryPolicyVerified'] = True
        pods_stopped()
        report['status'] = 'passed'
    except Exception as exc:
        report['failure'] = str(exc)[-4000:]
        raise
    finally:
        persist_report()
        deadline = time.monotonic() + 120
        try:
            for kind, resource_uid in reversed(owned):
                call(['delete', '--raw', '/apis/admissionregistration.k8s.io/v1/' + kind + '/' + policy_name, '-f', '-'],
                     {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': resource_uid}})
            if uid:
                current = get('namespace', namespace, None)
                if current['metadata']['uid'] != uid: raise ValueError('Namespace replaced before cleanup')
                pods_stopped()
                # No container ever ran, so there are no execution logs/results to preserve.
                task_uids = [t['metadata']['uid'] for t in get('taskruns')['items']]
                report['taskUIDs']=task_uids;persist_report()
                for attempt in range(3):
                    current=call(['get','namespace',namespace,'--ignore-not-found','-o','json']).stdout
                    if not current.strip():break
                    if json.loads(current)['metadata']['uid'] != uid:raise ValueError('Cleanup namespace replaced')
                    try:
                        guard.cleanup_archived_namespace(lambda argv, value=None: call(argv, value).stdout,
                                                         namespace, uid, task_uids, run_uids)
                        break
                    except subprocess.CalledProcessError as exc:
                        # Re-enter the guard to re-read all ownership/state and build a new
                        # CAS; never retry the stale patch itself or remove arbitrary finalizers.
                        if 'patch' not in exc.cmd or 'taskrun' not in exc.cmd or attempt==2:raise
                        report['guardedCleanupRetries']=attempt+1;persist_report()
                report['cleanupComplete'] = True
        except Exception as exc:
            report['status']='failed';report['cleanupFailure']=type(exc).__name__
            raise
        finally:
            persist_report()
    print(json.dumps(report))


if __name__ == '__main__':
    main()
