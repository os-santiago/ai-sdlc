"""Identity checks for isolated supply-chain fixtures, not live authority."""
import json
import hashlib
import re
import tarfile
from pathlib import PurePosixPath


def owned_node_exec(run, provider, name, expected_id, arguments, **kwargs):
    observed=run([provider,'inspect',name,'--format','{{.Id}}'],capture_output=True,limit=15).stdout.strip()
    if observed!=expected_id:raise ValueError('Owned kind node was replaced')
    # Never use the mutable name for the operation after checking identity.
    flags=['-i'] if kwargs.get('stdin') is not None else []
    return run([provider,'exec']+flags+[expected_id]+arguments,capture_output=True,limit=60,**kwargs)


def has_owner(resource,kind,uid):
    return any(x.get('kind')==kind and x.get('uid')==uid and x.get('controller') is True
               for x in resource['metadata'].get('ownerReferences',[]))


def run_binding(run,tasks,pods):
    owned=[t for t in tasks if has_owner(t,'PipelineRun',run['metadata']['uid'])]
    if len(owned)>1:raise ValueError('Unexpected additional fixture TaskRun')
    if not owned:return None
    task=owned[0]
    selected=[p for p in pods if has_owner(p,'TaskRun',task['metadata']['uid'])]
    if len(selected)>1:raise ValueError('Unexpected additional fixture Pod')
    if not selected:return None
    pod=selected[0]
    if task.get('status',{}).get('podName') not in (None,'',pod['metadata']['name']):raise ValueError('TaskRun Pod name differs from owner chain')
    if task['metadata']['namespace']!=run['metadata']['namespace'] or pod['metadata']['namespace']!=run['metadata']['namespace']:raise ValueError('Owner chain crosses namespaces')
    return {'taskRunUID':task['metadata']['uid'],'podUID':pod['metadata']['uid'],'pod':pod['metadata']['name']}


def writer_identity(run,tasks,pods,expected,allowed_secrets=()):
    current=run_binding(run,tasks,pods)
    if not expected or current!=expected:raise ValueError('Original writer owner chain changed')
    pod=next(p for p in pods if p['metadata']['uid']==current['podUID'])
    if pod['metadata'].get('deletionTimestamp') or pod['spec'].get('ephemeralContainers'):raise ValueError('Writer was deleted or modified')
    if pod['status'].get('phase') not in ('Succeeded','Failed'):return None
    if pod['status']['phase']!='Succeeded':raise ValueError('Original writer failed')
    statuses=pod['status'].get('containerStatuses',[])
    if {s['name'] for s in statuses}!={c['name'] for c in pod['spec']['containers']}:raise ValueError('Incomplete original writer status')
    for item in statuses+pod['status'].get('initContainerStatuses',[]):
        if item.get('restartCount',0)!=0 or item.get('state',{}).get('terminated',{}).get('exitCode')!=0:raise ValueError('Writer container did not terminate cleanly')
    if pod['spec'].get('automountServiceAccountToken') is not False or any(v.get('projected') or (v.get('secret') and v['secret'].get('secretName') not in allowed_secrets) for v in pod['spec'].get('volumes',[])):raise ValueError('Writer unexpectedly received credentials')
    return {**current,'containers':[{'name':x['name'],'containerID':x['containerID'],'exitCode':0} for x in statuses]}


def removable_results_finalizer(task,run_uids,pods):
    """Only archived, terminal fixture Tasks with no running writer qualify."""
    owned=any(has_owner(task,'PipelineRun',uid) for uid in run_uids if uid)
    terminal=any(c.get('type')=='Succeeded' and c.get('status') in ('True','False') for c in task.get('status',{}).get('conditions',[]))
    writers=[p for p in pods if has_owner(p,'TaskRun',task['metadata']['uid'])]
    stopped=all(p.get('status',{}).get('phase') in ('Succeeded','Failed') for p in writers)
    return owned and terminal and stopped and task['metadata'].get('finalizers')==['results.tekton.dev/taskrun']


def finalizer_patch(task,uid):
    if task['metadata']['uid']!=uid or task['metadata'].get('finalizers')!=['results.tekton.dev/taskrun']:
        raise ValueError('Fixture finalizer identity changed')
    return [{'op':'test','path':'/metadata/uid','value':uid},
            {'op':'test','path':'/metadata/resourceVersion','value':task['metadata']['resourceVersion']},
            {'op':'test','path':'/metadata/finalizers','value':['results.tekton.dev/taskrun']},
            {'op':'remove','path':'/metadata/finalizers'}]


def validate_oci_archive(path,expected_image):
    """Validate bounded OCI blobs without extracting paths onto the host."""
    if path.is_symlink() or not path.is_file() or path.stat().st_size>512<<20:
        raise ValueError('Publisher archive must be a bounded regular OCI file')
    blobs={};metadata={};seen=set();total=0
    with tarfile.open(path,mode='r:') as archive:
        for member in archive:
            name=member.name;total+=member.size
            if len(seen)>=4096 or total>512<<20 or name in seen or PurePosixPath(name).is_absolute() or '..' in PurePosixPath(name).parts or not (member.isfile() or member.isdir()):
                raise ValueError('Unsafe or oversized publisher archive')
            seen.add(name)
            if member.isdir():continue
            with archive.extractfile(member) as stream:
                if name in ('index.json','oci-layout'):
                    if member.size>65536:raise ValueError('Oversized OCI metadata')
                    metadata[name]=json.load(stream)
                elif re.fullmatch(r'blobs/sha256/[a-f0-9]{64}',name):
                    digest=hashlib.sha256();size=0
                    while chunk:=stream.read(65536):digest.update(chunk);size+=len(chunk)
                    if digest.hexdigest()!=name.rsplit('/',1)[1] or size!=member.size:raise ValueError('OCI blob digest mismatch')
                    blobs['sha256:'+digest.hexdigest()]=size
                else:raise ValueError('Unexpected OCI archive entry')
    if metadata.get('oci-layout')!={'imageLayoutVersion':'1.0.0'}:raise ValueError('Not an OCI archive')
    descriptors=metadata.get('index.json',{}).get('manifests',[])
    if len(descriptors)!=1 or descriptors[0].get('digest')!=expected_image.rsplit('@',1)[1] or blobs.get(descriptors[0]['digest'])!=descriptors[0].get('size'):
        raise ValueError('OCI archive does not contain the expected single publisher manifest')
    digest=hashlib.sha256()
    with path.open('rb') as stream:
        while chunk:=stream.read(65536):digest.update(chunk)
    return digest.hexdigest()
