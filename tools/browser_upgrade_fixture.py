#!/usr/bin/env python3
"""Exercise the reviewed 34->35 bridge on owned disposable PostgreSQL only.

The source is a successful main publication; the explicit local target image is
fixture evidence, not an approved publication or permission to upgrade a cluster.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import time

import browser_schema
from browser_upgrade_data import Compatibility, TABLES
import install
import install_images
import install_upgrade
from installer_process import capture
from runtime_upgrade_backup import Restore
import upgrade_backup_data as data
from upgrade_fence_client import Fence, HistoryRejected
from upgrade_migrator import Migrator

PG='docker.io/library/postgres@sha256:005c5c37db96c1e6652a79ea31f9886402fba81ac95dc2db897739121dd7f8f2'
LABEL='ai-sdlc.browser-upgrade-fixture'


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--provider',choices=('podman','docker'),default='podman')
    parser.add_argument('--source-run',type=int,required=True)
    parser.add_argument('--source-revision',required=True)
    parser.add_argument('--target-image',required=True)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args(argv)
    if not args.target_image.startswith(install_images.REGISTRY+'@') or not install_images.DIGEST.fullmatch(args.target_image.split('@')[-1]):
        raise ValueError('Explicit immutable fixture target required')
    args.output.mkdir(mode=0o700,parents=True,exist_ok=False)
    nonce=secrets.token_hex(16);cid=None;attempted=False;restorer=None
    report={'version':1,'status':'running','scope':'disposable browser schema34-to35 compatibility',
        'targetOrigin':'explicit local fixture image; not publication approval',
        'workflowEvidence':False,'installedRuntimeValidated':False,'upgradeAuthorized':False,
        'targetImage':args.target_image,'cleanup':[],'migrations':[],'controls':[]}
    def save():(args.output/'report.json').write_text(json.dumps(report,sort_keys=True,indent=2)+'\n')
    try:
        source=install_upgrade.Source(install.ROOT)
        _,publication=install_images.resolve(install_images.GitHub(),args.source_run,args.source_revision,include_build=True,include_release=True)
        before=source.catalog(args.source_revision)
        folder=install.ROOT/'runtime/k8s/internal/store/migrations'
        after=[{'filename':p.name,'sha256':hashlib.sha256(p.read_bytes()).hexdigest()} for p in sorted(folder.glob('*.sql'))]
        if len(before)!=34 or len(after)!=35 or after[:34]!=before or after[-1]['filename']!='0035_deployment_browser.sql':
            raise ValueError('Exact reviewed additive browser prefix required')
        inspected=json.loads(capture([args.provider,'image','inspect',args.target_image]))
        if len(inspected)!=1 or inspected[0].get('Os')!='linux' or inspected[0].get('Architecture')!='amd64' or args.target_image not in inspected[0].get('RepoDigests',[]):
            raise ValueError('Original fixture image identity differs')
        described=json.loads(capture([args.provider,'run','--rm','--network','none','--read-only','--cap-drop=ALL','--security-opt=no-new-privileges',args.target_image,'describe-migrations']))
        if described!=after:raise ValueError('Target embedded catalog differs from reviewed source')
        report.update(sourcePublication=publication['runURL'],sourceRevision=args.source_revision,
            source=source.git('rev-parse','HEAD').decode().strip(),dirty=bool(source.git('status','--porcelain')),
            targetCatalog=after,targetConfigDigest=inspected[0]['Id'],stage='database-start');save()
        admin=secrets.token_urlsafe(36)
        env=dict(os.environ,POSTGRES_USER='ai_sdlc',POSTGRES_PASSWORD=admin,POSTGRES_DB='ai_sdlc')
        attempted=True
        cid=capture([args.provider,'run','-d','--name','ai-sdlc-browser-upgrade-'+nonce,'--label',LABEL+'='+nonce,
            '--env','POSTGRES_USER','--env','POSTGRES_PASSWORD','--env','POSTGRES_DB',
            '--env','POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256','--tmpfs','/var/lib/postgresql/data:rw,size=512m',
            '-p','127.0.0.1::5432',PG],env=env).decode().strip()
        observed=json.loads(capture([args.provider,'container','inspect',cid]))[0]
        if observed['Id']!=cid or observed['Config']['Labels'].get(LABEL)!=nonce:raise ValueError('Fixture custody differs')
        ports=observed['NetworkSettings']['Ports']['5432/tcp']
        if len(ports)!=1 or ports[0]['HostIp']!='127.0.0.1':raise ValueError('Original loopback endpoint required')
        port=int(ports[0]['HostPort']);connection=f'postgresql://ai_sdlc:{admin}@127.0.0.1:{port}/ai_sdlc?sslmode=disable'
        for _ in range(100):
            if subprocess.run([args.provider,'exec',cid,'pg_isready','-h','127.0.0.1','-U','ai_sdlc'],capture_output=True,timeout=5).returncode==0:break
            time.sleep(.1)
        else:raise ValueError('Owned PostgreSQL unavailable')
        database=data.Database(capture,[args.provider,'exec',cid]);passwords={k:secrets.token_urlsafe(36) for k in install.ALL_ROLES}
        def migrate(image):
            intent={};report['migrations'].append(intent);save()
            migrator=Migrator(args.provider,image,connection,port,passwords,intent,lambda _:save())
            try:migrator.run()
            finally:migrator.close()
        report['stage']='source-migration';save()
        migrate(install_images.REGISTRY+'@'+publication['images']['migrator']['digest'])
        database.sql((install.ROOT/'runtime/k8s/internal/store/testdata/browser_upgrade_history.sql').read_text())
        database.sql("INSERT INTO execution_failures(request_id,pod_uid,container_id,exit_code) SELECT request_id,pod_uid,'containerd://'||repeat('a',64),22 FROM execution_lifecycles;")
        compatibility=Compatibility(database);original=compatibility.source(before)
        def fenced(catalog):
            with Fence(args.provider,args.target_image,connection,port,catalog,expires_at=time.time()+90,browser_deployment=True) as fence:
                fence.check()
                database.sql("BEGIN; SET LOCAL lock_timeout='100ms'; DO $$ BEGIN BEGIN UPDATE work_items SET status=status; RAISE EXCEPTION 'writer escaped'; EXCEPTION WHEN lock_not_available THEN NULL; END; END $$; ROLLBACK;")
                fence.check()
                return fence.nonce
        report['sourceFence']=fenced(before);save()
        report['stage']='source-backup-restoration';save()
        artifacts,proof=database.export();directory=args.output/'private-backup';directory.mkdir(mode=0o700)
        manifest=data.write_archives(directory,artifacts);backup={'archives':manifest,'retainedData':proof}
        def restore_save(record):report['restore']=record['restore'];save()
        restorer=Restore(args.provider,PG,directory,backup,restore_save)
        restorer.start();restorer.execute();restorer.close();restorer=None
        report['backupRestoredBeforeMigration']=True
        report['stage']='target-migration';save();migrate(args.target_image)
        report['compatibility']=compatibility.verify(original,after)
        for component,password in passwords.items():
            role='aisdlc_'+component.replace('-','_')
            current=capture([args.provider,'exec','--env','PGPASSWORD',cid,'psql','-X','-qAt','--no-password',
                '-h','127.0.0.1','-U',role,'-d','ai_sdlc','-c','SELECT current_user;'],env=dict(os.environ,PGPASSWORD=password))
            if current.decode().strip()!=role:raise ValueError('Original component credentials changed')
        report['originalCredentialsAuthenticate']=True
        report['targetFence']=fenced(after)
        # Generic failures never count as a meaningful historical denial.
        database.sql("INSERT INTO review_effects(run_id,kind,token,resource_name) SELECT id,'export',gen_random_uuid(),'browser-fixture' FROM review_runs;")
        try:
            try:
                with Fence(args.provider,args.target_image,connection,port,after,expires_at=time.time()+90,browser_deployment=True):pass
            except HistoryRejected:report['controls'].append('unacknowledged-review-effect-typed-rejection')
            else:raise ValueError('Unknown effect did not reject browser maintenance')
        finally:database.sql('DELETE FROM review_effects;')
        report['restoredFence']=fenced(after)
        # Rollback-only SQL captures a VALID changed observation. Transport,
        # syntax and parsing failures occur outside each rejection assertion.
        queries={table:browser_schema.QUERY.replace('public.execution_failures','public.'+table) for table in TABLES}
        class Observations:
            def __init__(self,values):self.values=values
            def sql(self,query):
                if query not in self.values:raise AssertionError('Unexpected negative oracle query')
                return self.values[query]
        for table in TABLES:
            for label,statement in [('rls',f'ALTER TABLE {table} DISABLE ROW LEVEL SECURITY;'),
                                    ('acl',f'GRANT TRUNCATE ON {table} TO aisdlc_worker;')]:
                # Every actual DDL/query and JSON parse occurs outside the
                # expected rejection; a query error cannot make this pass.
                observed={q:json.loads(database.sql('BEGIN;'+statement+q+'ROLLBACK;')) for q in queries.values()}
                if browser_schema.canonical(observed[queries[table]])==browser_schema.canonical(browser_schema.EXPECTED[table]):
                    raise ValueError('DDL negative did not change metadata')
                try:browser_schema.verify(Observations({q:json.dumps(v) for q,v in observed.items()}))
                except ValueError as e:
                    if str(e)!='Browser table structure, row security or privileges changed':raise
                else:raise ValueError('Browser metadata verifier accepted drift')
                browser_schema.verify(database)
                report['controls'].append(table+'-'+label)
        database.sql("UPDATE execution_failures SET exit_code=23;")
        try:
            try:compatibility.verify(original,after)
            except ValueError as e:
                if str(e)!='Original workflow rows or database authority changed':raise
                report['controls'].append('original-diagnostic-row-drift')
            else:raise ValueError('Original diagnostic change accepted')
        finally:database.sql('UPDATE execution_failures SET exit_code=22;')
        compatibility.verify(original,after)
        # Authority drift on an OLD table or role must also remain detectable.
        for label,change,undo in (
            ('original-table-acl','GRANT SELECT ON plans TO PUBLIC;','REVOKE SELECT ON plans FROM PUBLIC;'),
            ('original-role-capability','ALTER ROLE aisdlc_worker CREATEDB;','ALTER ROLE aisdlc_worker NOCREATEDB;')):
            database.sql(change)
            try:
                try:compatibility.verify(original,after)
                except ValueError as e:
                    if str(e)!='Original schema, privileges, role identities or tracking changed':raise
                    report['controls'].append(label)
                else:raise ValueError('Original authority drift accepted')
            finally:database.sql(undo)
            compatibility.verify(original,after)
        artifacts,proof=database.export();target_directory=args.output/'private-target-backup';target_directory.mkdir(mode=0o700)
        target_backup={'archives':data.write_archives(target_directory,artifacts),'retainedData':proof}
        def target_save(record):report['targetRestore']=record['restore'];save()
        restorer=Restore(args.provider,PG,target_directory,target_backup,target_save)
        restorer.start();restorer.execute()
        browser_schema.verify(data.Database(capture,[args.provider,'exec',restorer.cid]))
        restorer.close();restorer=None
        report['targetRestorationMetadataVerified']=True
        report.update(status='passed',stage='complete',originalHistoryRetained=True,browserTablesEmpty=True)
    except Exception as error:report.update(status='failed',errorType=type(error).__name__);raise
    finally:
        if restorer is not None:
            try:restorer.close()
            except Exception:report['cleanup'].append({'kind':'restore','status':'failed'});report['status']='failed'
        if cid is not None:
            try:
                obj=json.loads(capture([args.provider,'container','inspect',cid]))[0]
                if obj['Id']!=cid or obj['Config']['Labels'].get(LABEL)!=nonce:raise ValueError('Foreign fixture container')
                capture([args.provider,'rm','-f','-v',cid]);report['cleanup'].append({'kind':'database','status':'removed'})
            except Exception:report['cleanup'].append({'kind':'database','status':'failed'});report['status']='failed'
        elif attempted:report['cleanup'].append({'kind':'database','status':'uncertain-no-original-ID'});report['status']='failed'
        save()
    print(json.dumps({'status':report['status'],'report':str(args.output/'report.json')}))
    return 0 if report['status']=='passed' else 1

if __name__=='__main__':
    try:raise SystemExit(main())
    except (ValueError,OSError,subprocess.SubprocessError):raise SystemExit('Disposable browser migration failed; inspect sanitized report.')
