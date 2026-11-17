#!/usr/bin/env python3
"""Published 0033->0034 migration compatibility on an owned disposable database."""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
import uuid

import install
import install_images
import install_upgrade
from installer_process import capture
import upgrade_backup_data as data
from runtime_upgrade_backup import Restore
from upgrade_migrator import Migrator
from diagnostic_upgrade_data import Compatibility

PG='docker.io/library/postgres@sha256:005c5c37db96c1e6652a79ea31f9886402fba81ac95dc2db897739121dd7f8f2'
LABEL='ai-sdlc.upgrade-migration-fixture'


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--provider',choices=('podman','docker'),default='podman')
    for name in ('source','target'):
        parser.add_argument('--'+name+'-run',type=int,required=True);parser.add_argument('--'+name+'-revision',required=True)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--exercise-tool-budget-history',action='store_true')
    args=parser.parse_args(argv);args.output.mkdir(mode=0o700,parents=True,exist_ok=False)
    owner=secrets.token_hex(16);name='ai-sdlc-migration-'+owner;cid=None;restorer=None;attempted=False
    report={'version':1,'status':'running','owner':owner,'databaseContainer':name,'cleanup':[],
            'scope':'published migrator compatibility on disposable PostgreSQL',
            'autonomousEndToEndValidated':False,'installedRuntimeValidated':False,'migrations':[]}
    def save():(args.output/'report.json').write_text(json.dumps(report,indent=2)+'\n')
    try:
        source=install_upgrade.Source(install.ROOT)
        plan=install_upgrade.plan(source,install_images.GitHub(),args.source_run,args.source_revision,args.target_run,args.target_revision)
        if (len(plan['source']['migrations'])!=33 or len(plan['target']['migrations'])!=34
                or [v['filename'] for v in plan['pendingMigrations']]!=['0034_execution_failures.sql']):
            raise ValueError('Only the reviewed diagnostic migration bridge is covered')
        report.update(sourcePublication=plan['source']['publication'],targetPublication=plan['target']['publication'],
            source=source.git('rev-parse','HEAD').decode().strip(),dirty=bool(source.git('status','--porcelain')),stage='database-start');save()
        admin=secrets.token_urlsafe(36);env=dict(os.environ,POSTGRES_USER='ai_sdlc',POSTGRES_PASSWORD=admin,POSTGRES_DB='ai_sdlc')
        attempted=True
        cid=capture([args.provider,'run','-d','--name',name,'--label',LABEL+'='+owner,'--env','POSTGRES_USER',
            '--env','POSTGRES_PASSWORD','--env','POSTGRES_DB','--env','POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256','--tmpfs','/var/lib/postgresql/data:rw,size=512m',
            '-p','127.0.0.1::5432',PG],env=env).decode().strip()
        report['databaseContainerID']=cid;save()
        observed=json.loads(capture([args.provider,'container','inspect',cid]))[0]
        if observed['Id']!=cid or observed['Config']['Labels'].get(LABEL)!=owner:raise ValueError('Original fixture database changed')
        ports=observed['NetworkSettings']['Ports']['5432/tcp']
        if len(ports)!=1 or ports[0]['HostIp']!='127.0.0.1':raise ValueError('Original fixture loopback endpoint required')
        port=int(ports[0]['HostPort']);connection=f'postgresql://ai_sdlc:{admin}@127.0.0.1:{port}/ai_sdlc?sslmode=disable'
        for _ in range(60):
            result=subprocess.run([args.provider,'exec',cid,'pg_isready','-h','127.0.0.1','-U','ai_sdlc','-d','ai_sdlc'],capture_output=True,timeout=5)
            if result.returncode==0:break
            time.sleep(.5)
        else:raise ValueError('Fixture database did not become ready')
        passwords={role:secrets.token_urlsafe(36) for role in install.ALL_ROLES}
        prefix=[args.provider,'exec',cid];database=data.Database(capture,prefix)
        def migrate(which):
            image=install_images.REGISTRY+'@'+plan[which]['images']['migrator']['digest']
            intent={'publication':plan[which]['publication']};report['migrations'].append(intent);save()
            helper=Migrator(args.provider,image,connection,port,passwords,intent,lambda record:save())
            try:helper.run()
            finally:helper.close()
        report['stage']='source-migration';save();migrate('source')
        plan_id,work,event=[str(uuid.uuid4()) for _ in range(3)]
        database.sql(f"INSERT INTO plans(id,repository,correlation_id) VALUES('{plan_id}','fixture/app','retained');"
            f"INSERT INTO work_items(plan_id,id,uuid,issue_numbers,acceptance_criteria,status) VALUES('{plan_id}','retained','{work}','[1]','[\"keep original history\"]','failed');"
            f"INSERT INTO outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id) VALUES('{event}','fixture','retained','fixture','{{\"retain\":true}}','retained');")
        compatibility=Compatibility(database)
        original=compatibility.source(plan['source']['migrations'])
        grants=original['permissions'];role_attributes=original['roles']
        permissions=compatibility.permissions
        report['stage']='backup-restore';save()
        artifacts,proof=database.export();directory=args.output/'private-backup';directory.mkdir(mode=0o700)
        manifest=data.write_archives(directory,artifacts);restore_record={'archives':manifest,'retainedData':proof}
        def restore_save(record):report['restore']=record['restore'];save()
        restorer=Restore(args.provider,PG,directory,restore_record,restore_save)
        restorer.start();restorer.execute();restorer.close();restorer=None
        report['backupRestoredBeforeMigration']=True
        report['stage']='target-migration';save();migrate('target')
        report['stage']='retained-history-comparison';save()
        report['compatibility']=compatibility.verify(original)
        report['stage']='original-credential-comparison';save()
        for component,password in dict(passwords,administrator=admin).items():
            role='ai_sdlc' if component=='administrator' else 'aisdlc_'+component.replace('-','_')
            actual=capture([args.provider,'exec','--env','PGPASSWORD',cid,'psql','-X','-q','-A','-t','--no-password',
                '-h','127.0.0.1','-U',role,'-d','ai_sdlc','-c','SELECT current_user;'],env=dict(os.environ,PGPASSWORD=password)).decode().strip()
            if actual!=role:raise ValueError('Original component credential no longer authenticates')
        report['stage']='diagnostic-privilege-comparison';save()
        privileges=json.loads(database.sql("SELECT json_build_object('select',has_table_privilege('aisdlc_worker','execution_failures','SELECT'),'insert',has_column_privilege('aisdlc_worker','execution_failures','request_id','INSERT'),'update',has_table_privilege('aisdlc_worker','execution_failures','UPDATE'),'delete',has_table_privilege('aisdlc_worker','execution_failures','DELETE'),'timestamp',has_column_privilege('aisdlc_worker','execution_failures','observed_at','INSERT'));"))
        if privileges!={'select':True,'insert':True,'update':False,'delete':False,'timestamp':False}:
            raise ValueError('Diagnostic privileges differ from the reviewed boundary')
        for component in install.ALL_ROLES:
            if component=='worker':continue
            role='aisdlc_'+component.replace('-','_')
            denied=database.sql(f"SELECT NOT (has_table_privilege('{role}','execution_failures','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege('{role}','execution_failures','SELECT,INSERT,UPDATE,REFERENCES'));")
            if denied!='t\n':raise ValueError('Unrelated component received diagnostic privileges')
        public=database.sql("SELECT NOT EXISTS(SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) a WHERE c.oid='execution_failures'::regclass AND a.grantee=0);")
        if public!='t\n':raise ValueError('PUBLIC received diagnostic privileges')
        report['stage']='negative-compatibility-controls';save()
        # These mutations are confined to the owned disposable fixture. Each
        # control is restored before another comparison or cleanup is attempted.
        try:
            database.sql('GRANT SELECT ON plans TO PUBLIC;')
            if permissions()==grants:raise ValueError('ACL comparator missed a changed privilege')
        finally:database.sql('REVOKE SELECT ON plans FROM PUBLIC;')
        if permissions()!=grants:raise ValueError('ACL control failed to restore fixture')
        try:
            database.sql('ALTER DEFAULT PRIVILEGES FOR ROLE ai_sdlc IN SCHEMA public GRANT SELECT ON TABLES TO aisdlc_verifier;')
            if permissions()==grants:raise ValueError('ACL comparator missed changed default privileges')
        finally:database.sql('ALTER DEFAULT PRIVILEGES FOR ROLE ai_sdlc IN SCHEMA public REVOKE SELECT ON TABLES FROM aisdlc_verifier;')
        if permissions()!=grants:raise ValueError('Default ACL control failed to restore fixture')
        if permissions('GRANT USAGE ON TYPE plans TO aisdlc_verifier;')==grants:
            raise ValueError('ACL comparator missed changed type privileges')
        if permissions()!=grants:raise ValueError('Type ACL control failed to roll back fixture')
        try:
            database.sql('ALTER ROLE aisdlc_worker CREATEDB;')
            if compatibility.roles()==role_attributes:
                raise ValueError('Role comparator missed added authority')
        finally:database.sql('ALTER ROLE aisdlc_worker NOCREATEDB;')
        if compatibility.roles()!=role_attributes:
            raise ValueError('Role control failed to restore fixture')
        # A wrong password must fail on the same endpoint where the original
        # credential just succeeded. Neither is embedded in argv or reports.
        auth=[args.provider,'exec','--env','PGPASSWORD',cid,'psql','-X','-q','-A','-t','--no-password',
              '-h','127.0.0.1','-U','aisdlc_worker','-d','ai_sdlc','-c','SELECT current_user;']
        try:capture(auth,env=dict(os.environ,PGPASSWORD=secrets.token_urlsafe(36)))
        except ValueError:pass
        else:raise ValueError('Fixture did not enforce original password')
        if capture(auth,env=dict(os.environ,PGPASSWORD=passwords['worker']))!=b'aisdlc_worker\n':
            raise ValueError('Original credential unavailable after negative control')
        try:
            database.sql("UPDATE work_items SET status='ready';")
            try:compatibility.quiet()
            except ValueError:pass
            else:raise ValueError('Queued work was allowed to resume')
        finally:database.sql("UPDATE work_items SET status='failed';")
        queued='fixture-'+secrets.token_hex(16)
        try:
            database.sql("INSERT INTO authorized_outbox(producer,event_id,target,operation,receipt,payload) VALUES('worker','"+queued+"','orchestrator','fixture','{}',decode('00','hex'));")
            try:compatibility.quiet()
            except ValueError:pass
            else:raise ValueError('Pending authenticated delivery was allowed to resume')
        finally:database.sql("DELETE FROM authorized_outbox WHERE producer='worker' AND event_id='"+queued+"';")
        compatibility.quiet()
        compatibility.verify(original)
        report.update(status='passed',sourceMigrations=33,targetMigrations=34,originalRowsUnchanged=True,
            originalSchemaAndPrivilegesUnchanged=True,originalTrackingUnchanged=True,originalCredentialsAuthenticate=True,
            diagnosticTableEmpty=True,diagnosticPrivilegesBounded=True,unrelatedDiagnosticRolesDenied=True,
            changedACLDetected=True,changedDefaultACLDetected=True,changedTypeACLDetected=True,changedRoleCapabilityDetected=True,wrongPasswordRejected=True,
            queuedWorkRejected=True,pendingDeliveryRejected=True,
            upgradeAuthorized=False,rollbackAuthorized=False)
        if args.exercise_tool_budget_history:
            from tool_budget_fixture import exercise
            exercise(database,args.provider,PG,args.output/'private-tool-budget-backup',
                     plan['target']['migrations'],plan_id,work,report,save)
    except Exception as error:report.update(status='failed',errorType=type(error).__name__);raise
    finally:
        if restorer is not None:
            try:restorer.close()
            except Exception:report['cleanup'].append({'kind':'restore','status':'failed'});report['status']='failed'
        if cid is not None:
            try:
                obj=json.loads(capture([args.provider,'container','inspect',cid]))[0]
                if obj['Id']!=cid or obj['Config']['Labels'].get(LABEL)!=owner:raise ValueError('Foreign fixture container')
                capture([args.provider,'rm','-f',cid]);report['cleanup'].append({'kind':'database','status':'removed'})
            except Exception:report['cleanup'].append({'kind':'database','status':'failed'});report['status']='failed'
        elif attempted:report['cleanup'].append({'kind':'database','status':'uncertain-no-original-ID'});report['status']='failed'
        save()
    print(json.dumps({'status':report['status'],'report':str(args.output/'report.json')}));return 0 if report['status']=='passed' else 1

if __name__=='__main__':
    try:raise SystemExit(main())
    except (ValueError,OSError,subprocess.SubprocessError):raise SystemExit('Disposable migration validation failed; inspect its sanitized report.')
