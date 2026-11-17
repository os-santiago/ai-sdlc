#!/usr/bin/env python3
"""Exercise private backup/restore primitives against two owned disposable DBs."""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time

import install
import install_upgrade
import upgrade_backup_data as backup
from runtime_upgrade_backup import Restore

IMAGE='docker.io/library/postgres@sha256:005c5c37db96c1e6652a79ea31f9886402fba81ac95dc2db897739121dd7f8f2'
LABEL='ai-sdlc.upgrade-backup-fixture'


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--provider',choices=('podman','docker'),default='podman')
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args(argv);args.output.mkdir(mode=0o700,parents=True,exist_ok=False)
    owner=secrets.token_hex(16);containers=[];attempted=[];owned_restore=None
    report={'version':1,'status':'running','owner':owner,'scope':'disposable backup/restore primitives',
            'autonomousEndToEndValidated':False,'installedRuntimeValidated':False,'cleanup':[]}
    def save():(args.output/'report.json').write_text(json.dumps(report,indent=2)+'\n')
    def run(argv,**kwargs):
        result=subprocess.run(list(map(str,argv)),capture_output=True,timeout=30,**kwargs)
        if result.returncode:raise ValueError('Disposable backup fixture command failed')
        return result.stdout
    def start(kind,user,database,archive=None):
        name='ai-sdlc-backup-'+owner+'-'+kind;attempted.append(name);report['attempted']=attempted;save()
        env=dict(os.environ,POSTGRES_USER=user,POSTGRES_PASSWORD=secrets.token_urlsafe(36),POSTGRES_DB=database)
        command=[args.provider,'run','-d','--name',name,'--network','none','--label',LABEL+'='+owner,
            '--env','POSTGRES_USER','--env','POSTGRES_PASSWORD','--env','POSTGRES_DB',
            '--tmpfs','/var/lib/postgresql/data:rw,size=512m','--memory','768m','--cpus','1']
        if archive is not None:command+=['-v',str(archive)+':/backup:ro,z']
        cid=run(command+[IMAGE],env=env).decode().strip()
        if not re.fullmatch('[a-f0-9]{64}',cid):raise ValueError('Original fixture container ID missing')
        containers.append(cid);report['containerIDs']=containers;save()
        for _ in range(60):
            ready=subprocess.run([args.provider,'exec',cid,'pg_isready','-h','127.0.0.1','-U',user,'-d',database],capture_output=True,timeout=10)
            if ready.returncode==0:break
            time.sleep(.5)
        else:raise ValueError('Disposable database not ready')
        return cid
    try:
        source=install_upgrade.Source(install.ROOT);revision=source.git('rev-parse','HEAD').decode().strip()
        catalog=source.catalog(revision)
        report['source']=revision;report['dirty']=bool(source.git('status','--porcelain'));save()
        cid=start('source','ai_sdlc','ai_sdlc');prefix=[args.provider,'exec',cid]
        database=backup.Database(backup.capture,prefix)
        def sql(statement):return database.sql(statement)
        sql('CREATE TABLE schema_migrations(filename text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now());')
        for item in catalog[:33]:
            raw=source.git('show',revision+':'+install_upgrade.MIGRATIONS+item['filename'])
            if backup.digest(raw)!=item['sha256']:raise ValueError('Fixture migration changed')
            sql('BEGIN;'+raw.decode()+"\nINSERT INTO schema_migrations(filename) VALUES('"+item['filename']+"');COMMIT;")
        sql("CREATE ROLE aisdlc_backup_fixture NOLOGIN;GRANT SELECT ON schema_migrations TO aisdlc_backup_fixture;"
            "INSERT INTO plans(id,repository,correlation_id) VALUES('00000000-0000-0000-0000-000000000001','fixture/app','retained');"
            "INSERT INTO work_items(plan_id,id,uuid,issue_numbers,acceptance_criteria,status) VALUES('00000000-0000-0000-0000-000000000001','retained','00000000-0000-0000-0000-000000000002','[1]','[\"keep data\"]','ready');"
            "INSERT INTO outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id) VALUES('00000000-0000-0000-0000-000000000003','fixture','retained','fixture','{\"nested\":{\"value\":true}}','retained');")
        report['stage']='export';save()
        artifacts,expected=database.export()
        directory=args.output/'private-archives';directory.mkdir(mode=0o700)
        manifest=backup.write_archives(directory,artifacts);backup.verify_archives(directory,manifest)
        report['archiveManifest']=manifest;report['stage']='restore';save()
        restoration={'archives':manifest,'retainedData':expected}
        def save_restore(record):report['restore']=record['restore'];save()
        owned_restore=Restore(args.provider,IMAGE,directory,restoration,save_restore)
        owned_restore.start();owned_restore.execute()
        restorer=owned_restore.user
        restore_prefix=[args.provider,'exec',owned_restore.cid]
        restored_db=backup.Database(backup.capture,restore_prefix,restorer)
        observed=restored_db.snapshot(exclude_restore_role=restorer)
        if observed!=expected:
            report['differingProofs']=[name for name in sorted(set(observed)|set(expected)) if observed.get(name)!=expected.get(name)]
            raise ValueError('Restored retained data or roles differ')
        if database.snapshot()!=expected:raise ValueError('Source fixture changed during restoration')
        restored_db.sql('GRANT aisdlc_backup_fixture TO '+restorer+';')
        if restored_db.snapshot(exclude_restore_role=restorer).get('@membership')==expected['@membership']:
            raise ValueError('Unexpected restored membership was ignored')
        report['changedMembershipRejected']=True
        restored_db.sql('REVOKE SELECT ON schema_migrations FROM aisdlc_backup_fixture;')
        after_acl=restored_db.snapshot(exclude_restore_role=restorer)
        if after_acl['@schema']==expected['@schema']:raise ValueError('Changed restored ACL was ignored')
        report['changedACLRejected']=True
        restored_db.sql('ALTER TABLE execution_requests DISABLE ROW LEVEL SECURITY;')
        if restored_db.snapshot(exclude_restore_role=restorer)['@schema']==after_acl['@schema']:
            raise ValueError('Changed restored row-level security was ignored')
        report['changedRLSRejected']=True
        restored_db.sql('REVOKE aisdlc_backup_fixture FROM '+restorer+';')
        restored_db.sql("UPDATE work_items SET acceptance_criteria='[\"changed fixture row\"]';")
        if restored_db.snapshot(exclude_restore_role=restorer).get('work_items')==expected['work_items']:
            raise ValueError('Changed restored data was ignored')
        report['changedRetainedRowRejected']=True
        # Corruption and overly broad permissions must invalidate a copied archive.
        original=(directory/'globals.sql').read_bytes()
        (directory/'globals.sql').write_bytes(original+b'\n-- altered\n')
        try:backup.verify_archives(directory,manifest)
        except ValueError:report['corruptArchiveRejected']=True
        else:raise ValueError('Changed private archive accepted')
        (directory/'globals.sql').write_bytes(original);os.chmod(directory/'globals.sql',0o644)
        try:backup.verify_archives(directory,manifest)
        except ValueError:report['publicArchiveRejected']=True
        else:raise ValueError('Public archive accepted')
        os.chmod(directory/'globals.sql',0o600);backup.verify_archives(directory,manifest)
        report.update(status='passed',retainedDataAndRolesMatch=True,originalSourceUnchanged=True,
            proofCount=len(expected),backupRestoredToFreshIsolatedDatabase=True,rollbackAuthorized=False)
    except Exception as error:
        report.update(status='failed',errorType=type(error).__name__);raise
    finally:
        if owned_restore is not None:
            try:owned_restore.close();report['cleanup'].append({'kind':'restore','status':'removed'})
            except Exception:report['cleanup'].append({'kind':'restore','status':'failed'});report['status']='failed'
        for cid in containers:
            try:
                obj=json.loads(run([args.provider,'container','inspect',cid]))[0]
                if obj['Id']!=cid or obj['Config']['Labels'].get(LABEL)!=owner:raise ValueError('Foreign fixture container')
                run([args.provider,'rm','-f',cid]);report['cleanup'].append({'id':cid,'status':'removed'})
            except Exception:report['cleanup'].append({'id':cid,'status':'failed'});report['status']='failed'
        if len(attempted)!=len(containers):report['cleanup'].append({'status':'uncertain-no-original-ID'});report['status']='failed'
        save()
    print(json.dumps({'status':report['status'],'report':str(args.output/'report.json')}))
    return 0 if report['status']=='passed' else 1

if __name__=='__main__':
    try:raise SystemExit(main())
    except (ValueError,OSError,subprocess.SubprocessError):raise SystemExit('Disposable backup validation failed; inspect its sanitized report.')
