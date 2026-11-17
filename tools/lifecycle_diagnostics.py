"""Finite fixture observations; never logs Pod specs, messages or credentials."""
import json
import re

UUID = re.compile(r'^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$')
REASONS = {'ContainerCreating','PodInitializing','ErrImagePull','ImagePullBackOff',
    'CrashLoopBackOff','CreateContainerConfigError','CreateContainerError','RunContainerError',
    'StartError','Completed','Error','OOMKilled','Unschedulable','SchedulingGated',
    'ContainersNotReady','ContainersNotInitialized','PodCompleted','SandboxChanged'}


def summary(pod, namespace, name):
    metadata = pod.get('metadata', {})
    if (metadata.get('namespace') != namespace or metadata.get('name') != name
            or not UUID.fullmatch(str(metadata.get('uid', '')))):
        raise ValueError('Fixture Pod observation identity differs')
    status = pod.get('status', {})
    phase = status.get('phase')
    result = {'podUID': metadata['uid'], 'phase': phase if phase in
        ('Pending','Running','Succeeded','Failed','Unknown') else 'unknown',
        'deleting': metadata.get('deletionTimestamp') is not None,
        'conditions': [], 'containers': []}
    for condition in status.get('conditions', [])[:16]:
        kind = condition.get('type')
        if kind not in ('PodScheduled','Ready','ContainersReady','Initialized','PodReadyToStartContainers'):
            continue
        value = condition.get('status')
        reason = condition.get('reason')
        result['conditions'].append({'type':kind,'status':value if value in ('True','False','Unknown') else 'unknown',
                                    'reason':reason if reason in REASONS else 'other'})
    for container in status.get('containerStatuses', [])[:8]:
        name = container.get('name')
        if name not in ('worker','mailbox'):
            continue
        state = container.get('state', {})
        kind = next((v for v in ('waiting','terminated','running') if v in state), 'unknown')
        fields = state.get(kind, {})
        reason = fields.get('reason')
        code, restarts = fields.get('exitCode'), container.get('restartCount')
        result['containers'].append({'name':name,'state':kind,
            'reason':reason if reason in REASONS else 'other',
            'exitCode':code if type(code) is int and 0 <= code <= 255 else None,
            'restartCount':restarts if type(restarts) is int and 0 <= restarts <= 10000 else None,
            'ready':container.get('ready') is True})
    return result


def observe(command, namespace, namespace_uid, name):
    current = json.loads(command(['get','namespace',namespace,'-o','json'], timeout=1))
    if current.get('metadata',{}).get('uid') != namespace_uid:
        raise ValueError('Original fixture namespace changed')
    raw = command(['-n',namespace,'get','pod',name,'--ignore-not-found','-o','json'], timeout=1)
    if len(raw) > 1024*1024:
        raise ValueError('Fixture Pod observation bound')
    if not raw.strip():
        return None
    result = summary(json.loads(raw), namespace, name)
    current = json.loads(command(['get','namespace',namespace,'-o','json'], timeout=1))
    if current.get('metadata',{}).get('uid') != namespace_uid:
        raise ValueError('Original fixture namespace changed')
    return result
