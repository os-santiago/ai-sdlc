#!/usr/bin/env python3
"""Disposable PostgreSQL + Helm proof of pause_once; never an installed-runtime upgrade.

Only a fresh labelled namespace in an explicitly selected kind test cluster and
an owned tmpfs database are mutated. Production Pause identity checks are covered
separately; this fixture exercises its shared fence transport and pause sequence.
"""
import argparse
import faulthandler
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time
import uuid

import build_runtime_install
import install
import install_images
import install_upgrade
import runtime_upgrade_pause
from upgrade_fence_client import Fence

LABEL='ai-sdlc.upgrade-pause-fixture'
POSTGRES='docker.io/library/postgres@sha256:005c5c37db96c1e6652a79ea31f9886402fba81ac95dc2db897739121dd7f8f2'


def command(args,*,input=None,env=None,timeout=30,check=True):
    result=subprocess.run([str(x) for x in args],input=input,env=env,capture_output=True,text=True,timeout=timeout)
    if check and result.returncode:raise ValueError('Disposable fixture command failed')
    return result


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster',required=True);parser.add_argument('--kubeconfig',type=Path,required=True)
    parser.add_argument('--provider',choices=('podman','docker'),default='podman')
    parser.add_argument('--run',type=int,required=True);parser.add_argument('--revision',required=True)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--exercise-transition',action='store_true')
    parser.add_argument('--exercise-tool-budget-transition',action='store_true')
    args=parser.parse_args(argv)
    # An explicit test name excludes all managed E2E/production installations.
    if not args.cluster.startswith('ai-sdlc-') or not args.cluster.endswith('-test'):
        raise ValueError('A dedicated kind test cluster is required')
    args.output.mkdir(mode=0o700,parents=True,exist_ok=False)
    owner=secrets.token_hex(16);namespace='upgrade-pause-'+owner;db_name=namespace+'-db'
    provider=args.provider;cid=nsuid=None
    binaries=install.tools(Path(os.environ.get('XDG_CACHE_HOME',Path.home()/'.cache'))/'ai-sdlc/tools')
    kube=[binaries['kubectl'],'--kubeconfig',args.kubeconfig,'--context','kind-'+args.cluster,'--request-timeout=10s']
    helm=[binaries['helm'],'--kubeconfig',args.kubeconfig,'--kube-context','kind-'+args.cluster]
    report={'version':1,'status':'running','scope':'disposable PostgreSQL and namespaced Helm producer fixture',
            'autonomousEndToEndValidated':False,'installedRuntimeValidated':False,'cleanup':[],
            'owner':owner,'namespace':namespace,'databaseContainer':db_name,'creationAttempts':[]}
    def write_report():
        (args.output/'report.json').write_text(json.dumps(report,indent=2)+'\n')
    def guard_namespace():
        obj=json.loads(command(kube+['get','namespace',namespace,'-o','json']).stdout)
        meta=obj['metadata']
        if nsuid is None or meta.get('uid')!=nsuid or meta.get('labels',{}).get(LABEL)!=owner or meta.get('deletionTimestamp'):
            raise ValueError('Original fixture namespace custody changed')
    def sql(value,check=True):
        return command([provider,'exec','-i',cid,'psql','-X','-q','-A','-t','-v','ON_ERROR_STOP=1','-v','VERBOSITY=sqlstate','-U','ai_sdlc','-d','ai_sdlc'],input=value,check=check)
    write_report()
    stack_file=None
    try:
        report['stage']='publication';write_report()
        api=install_images.GitHub()
        # Verify signatures; revisions without the cutover sentinel predate signing.
        _,publication=install_images.resolve(api,args.run,args.revision,include_build=True,include_release=True,
                                             verify=install_images.signature_verifier(api,args.revision,allow_unsigned_legacy=True))
        image=install_images.REGISTRY+'@'+publication['images']['migrator']['digest']
        catalog=build_runtime_install.component_call(provider,image,'describe-migrations',{})
        source=install_upgrade.Source(install.ROOT)
        if catalog!=source.catalog(args.revision) or len(catalog)!=34:raise ValueError('Unexpected published migration catalog')
        fixture_catalog=catalog if args.exercise_tool_budget_transition else catalog[:-1]
        report.update(publication=publication['runURL'],migratorImage=image,source=command(['git','rev-parse','HEAD']).stdout.strip(),
                      dirty=bool(command(['git','status','--porcelain']).stdout))
        password=secrets.token_urlsafe(36)
        env=dict(os.environ,POSTGRES_USER='ai_sdlc',POSTGRES_PASSWORD=password,POSTGRES_DB='ai_sdlc')
        report['stage']='database-start';report['creationAttempts'].append('database');write_report()
        cid=command([provider,'run','-d','--name',db_name,'--label',LABEL+'='+owner,
            '--env','POSTGRES_USER','--env','POSTGRES_PASSWORD','--env','POSTGRES_DB',
            '--tmpfs','/var/lib/postgresql/data:rw,size=512m','-p','127.0.0.1::5432',POSTGRES],env=env).stdout.strip()
        report['databaseContainerID']=cid;write_report()
        details=json.loads(command([provider,'container','inspect',cid]).stdout)[0]
        if details['Config']['Labels'].get(LABEL)!=owner or details['Id']!=cid:raise ValueError('Original fixture database identity lost')
        endpoint=details['NetworkSettings']['Ports']['5432/tcp']
        if len(endpoint)!=1 or endpoint[0]['HostIp']!='127.0.0.1':raise ValueError('Fixture database is not loopback-bound')
        port=int(endpoint[0]['HostPort'])
        for _ in range(60):
            if command([provider,'exec',cid,'pg_isready','-h','127.0.0.1','-U','ai_sdlc','-d','ai_sdlc'],check=False).returncode==0:break
            time.sleep(.5)
        else:raise ValueError('Disposable database did not become ready')
        report['stage']='schema';write_report()
        sql('CREATE TABLE public.schema_migrations(filename text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now());')
        for migration in fixture_catalog:
            raw=source.git('show',args.revision+':'+install_upgrade.MIGRATIONS+migration['filename'])
            if hashlib.sha256(raw).hexdigest()!=migration['sha256']:raise ValueError('Original fixture migration changed')
            sql('BEGIN;\n'+raw.decode()+"\nINSERT INTO public.schema_migrations(filename) VALUES('"+migration['filename']+"');COMMIT;")
        plan,work,event=[str(uuid.uuid4()) for _ in range(3)]
        sql(f"INSERT INTO plans(id,repository,correlation_id) VALUES('{plan}','fixture/app','pause-fixture');"
            f"INSERT INTO work_items(plan_id,id,uuid,issue_numbers,acceptance_criteria,status) VALUES('{plan}','retained','{work}','[1]','[\"retain original history\"]','ready');"
            f"INSERT INTO outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id) VALUES('{event}','fixture','retained','fixture','{{\"retain\":true}}','pause-fixture');")
        snapshot="SELECT row_to_json(t)::text FROM (SELECT * FROM work_items ORDER BY uuid) t;SELECT row_to_json(t)::text FROM (SELECT * FROM outbox ORDER BY id) t;"
        before=sql(snapshot).stdout
        ns={'apiVersion':'v1','kind':'Namespace','metadata':{'name':namespace,'labels':{LABEL:owner}}}
        report['stage']='namespace-start';report['creationAttempts'].append('namespace');write_report()
        created=json.loads(command(kube+['create','-f','-','-o','json'],input=json.dumps(ns)).stdout)
        nsuid=created['metadata']['uid'];report['namespaceUID']=nsuid;write_report()
        with tempfile.TemporaryDirectory() as directory:
            chart=Path(directory);(chart/'templates').mkdir()
            (chart/'Chart.yaml').write_text('apiVersion: v2\nname: pause-fixture\nversion: 0.1.0\n')
            (chart/'values.yaml').write_text('replicas: 1\n')
            if args.exercise_transition or args.exercise_tool_budget_transition:
                (chart/'values.yaml').write_text('replicas: 1\nintake: 0\nfrozen: true\ntoolSteps: 16\n')
                (chart/'templates/policy.yaml').write_text('''apiVersion: v1
kind: ConfigMap
metadata: {name: phase-policy}
data:
  frozen: {{ .Values.frozen | quote }}
  toolSteps: {{ .Values.toolSteps | quote }}
''')
            (chart/'templates/producer.yaml').write_text('''apiVersion: apps/v1
kind: Deployment
metadata:
  name: producer
spec:
  replicas: {{ .Values.replicas }}
  selector:
    matchLabels: {app: pause-fixture}
  template:
    metadata:
      labels: {app: pause-fixture}
    spec:
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 1
      containers:
        - name: producer
          image: registry.k8s.io/pause:3.10
          imagePullPolicy: Never
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
            runAsNonRoot: true
            runAsUser: 65534
            seccompProfile: {type: RuntimeDefault}
''')
            if args.exercise_transition or args.exercise_tool_budget_transition:
                producer=(chart/'templates/producer.yaml').read_text()
                (chart/'templates/intake.yaml').write_text(producer.replace('name: producer','name: intake').replace('.Values.replicas','.Values.intake').replace('app: pause-fixture','app: intake-fixture'))
            report['stage']='helm-install';write_report();guard_namespace()
            command(helm+['install','pause-fixture',chart,'-n',namespace,'--wait','--timeout','60s'],timeout=65)
            original=json.loads(command(kube+['get','deployment','producer','-n',namespace,'-o','json']).stdout)
            depuid=original['metadata']['uid']
            originalpods=json.loads(command(kube+['get','pods','-n',namespace,'-o','json']).stdout)['items']
            if len(originalpods)!=1:raise ValueError('Expected one original fixture producer')
            def observe(replicas):
                guard_namespace()
                dep=json.loads(command(kube+['get','deployment','producer','-n',namespace,'-o','json']).stdout)
                pods=json.loads(command(kube+['get','pods','-n',namespace,'-o','json']).stdout)['items']
                if dep['metadata']['uid']!=depuid or dep['spec']['replicas']!=replicas or len(pods)!=replicas:
                    raise ValueError('Original fixture producer has not converged')
                if replicas and pods[0]['metadata']['uid']!=originalpods[0]['metadata']['uid']:
                    raise ValueError('Original fixture producer replaced')
            record={'phase':'planned','expiresAt':time.time()+120}
            def save(value):(args.output/'pause-intent.json').write_text(json.dumps(value,indent=2)+'\n')
            connection=f'postgresql://ai_sdlc:{password}@127.0.0.1:{port}/ai_sdlc?sslmode=disable'
            report['stage']='fenced-pause';write_report()
            stack_file=(args.output/'wait-stack.log').open('x')
            faulthandler.dump_traceback_later(15,repeat=True,file=stack_file)
            with Fence(provider,image,connection,port,fixture_catalog,expires_at=record['expiresAt']) as fence:
                report['trace']=[]
                def trace(name,function):
                    def invoke(*args,**kwargs):
                        def event(state):
                            report['trace'].append({'operation':name,'state':state,'at':round(time.time(),3)})
                            report['trace']=report['trace'][-128:]
                        event('started')
                        try:result=function(*args,**kwargs)
                        except Exception:event('failed');raise
                        event('completed');return result
                    return invoke
                for name in ('write','read','check','command','release'):
                    setattr(fence,name,trace(name,getattr(fence,name)))
                writer=f"SET lock_timeout='300ms';INSERT INTO outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id) VALUES('{uuid.uuid4()}','fixture','blocked','fixture','{{}}','fixture');"
                blocked=sql(writer,check=False)
                if blocked.returncode==0 or '55P03' not in blocked.stderr:raise ValueError('Fixture writer was not blocked by the original fence')
                def apply():
                    guard_namespace()
                    fence.command(helm+['upgrade','pause-fixture',chart,'-n',namespace,'--set','replicas=0','--timeout','30s'],timeout=35)
                runtime_upgrade_pause.pause_once(record,fence,trace('source',lambda:observe(1)),trace('target',lambda:observe(0)),apply,save)
            observe(0)
            if sql(snapshot).stdout!=before:raise ValueError('Original fixture history changed during pause')
            if sql("INSERT INTO outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id) VALUES('"+str(uuid.uuid4())+"','fixture','after','fixture','{}','fixture');",check=False).returncode:
                raise ValueError('Released database remained fenced')
            report.update(status='passed',sourceMigrationCount=len(fixture_catalog),writerBlocked=True,historyUnchanged=True,
                producerUIDRetained=True,originalPodAbsent=True,fenceReleased=True,workflowRowsWrittenByPause=False)
            if args.exercise_transition or args.exercise_tool_budget_transition:
                from diagnostic_upgrade_fixture import exercise
                report['stage']='diagnostic-phase-fixture';write_report()
                exercise(chart=chart,kube=kube,helm=helm,namespace=namespace,guard=guard_namespace,
                    command=command,provider=provider,image=image,connection=connection,port=port,
                    catalog=fixture_catalog,sql=sql,snapshot=snapshot,report=report,save=write_report,
                    tool_budget=args.exercise_tool_budget_transition)
    except Exception as error:
        report.update(status='failed',errorType=type(error).__name__)
        raise
    finally:
        faulthandler.cancel_dump_traceback_later()
        if stack_file is not None:stack_file.close()
        if nsuid is None and 'namespace' in report['creationAttempts']:
            report['cleanup'].append({'kind':'namespace','status':'uncertain-no-original-UID'});report['status']='failed'
        if cid is None and 'database' in report['creationAttempts']:
            report['cleanup'].append({'kind':'database','status':'uncertain-no-original-ID'});report['status']='failed'
        if nsuid is not None:
            try:
                delete={'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':nsuid}}
                command(kube+['delete','--raw','/api/v1/namespaces/'+namespace,'-f','-'],input=json.dumps(delete))
                command(kube+['wait','--for=delete','namespace/'+namespace,'--timeout=60s'],timeout=65)
                report['cleanup'].append({'kind':'namespace','status':'removed'})
            except Exception:report['cleanup'].append({'kind':'namespace','status':'failed'});report['status']='failed'
        if cid is not None:
            try:
                obj=json.loads(command([provider,'container','inspect',cid]).stdout)[0]
                if obj['Id']!=cid or obj['Config']['Labels'].get(LABEL)!=owner:raise ValueError('Foreign fixture container')
                command([provider,'rm','-f',cid])
                report['cleanup'].append({'kind':'database','status':'removed'})
            except Exception:report['cleanup'].append({'kind':'database','status':'failed'});report['status']='failed'
        write_report()
    print(json.dumps({'status':report['status'],'report':str(args.output/'report.json'),'cleanup':report['cleanup']},indent=2))
    return 0 if report['status']=='passed' else 1

if __name__=='__main__':
    try:raise SystemExit(main())
    except (ValueError,OSError,subprocess.SubprocessError):
        raise SystemExit('Disposable pause validation failed; inspect its sanitized report.')
