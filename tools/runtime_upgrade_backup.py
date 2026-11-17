"""Restore-verify a bounded private backup of the original paused runtime."""
import json
import os
import re
import secrets
import subprocess
import time

import bootstrap_database_forward
import bootstrap_prepare as bootstrap
import build_foundation_install as files
import install_images
import konflux_runtime_install as platform
import prepared_configuration
from runtime_upgrade_pause import Pause
import upgrade_backup_data as data
from upgrade_fence_client import Fence

VERSION='ai-sdlc.runtime-upgrade-backup/v1'
LABEL='ai-sdlc.upgrade-backup-restore'


class Restore:
    def __init__(self,provider,image,directory,record,save):
        if provider not in ('podman','docker') or not re.fullmatch(r'docker\.io/library/postgres(?::[A-Za-z0-9_.-]+)?@sha256:[a-f0-9]{64}',image):
            raise ValueError('Original immutable PostgreSQL image required')
        self.provider,self.image,self.directory,self.record,self.save=provider,image,directory,record,save
        self.owner=secrets.token_hex(16);self.name='ai-sdlc-restore-'+self.owner
        self.user='aisdlc_restore_'+self.owner;self.cid=None

    def identity(self):
        if self.cid is None:raise ValueError('Original restore container acknowledgement missing')
        raw=data.capture([self.provider,'container','inspect',self.cid])
        values=json.loads(raw)
        if (not isinstance(values,list) or len(values)!=1 or values[0].get('Id')!=self.cid
                or values[0].get('Config',{}).get('Labels',{}).get(LABEL)!=self.owner
                or values[0].get('HostConfig',{}).get('NetworkMode')!='none'):
            raise ValueError('Original isolated restore container custody changed')

    def start(self):
        self.record['restore']={'nonce':self.owner,'name':self.name,'administrator':self.user,'image':self.image,'state':'starting'}
        self.save(self.record)
        env=dict(os.environ,POSTGRES_USER=self.user,POSTGRES_PASSWORD=secrets.token_urlsafe(36),POSTGRES_DB='restore_control')
        argv=[self.provider,'run','-d','--name',self.name,'--network','none','--label',LABEL+'='+self.owner,
            '--env','POSTGRES_USER','--env','POSTGRES_PASSWORD','--env','POSTGRES_DB',
            '--tmpfs','/var/lib/postgresql/data:rw,size=512m','--memory=768m','--cpus=1']
        for name in sorted(data.FILES):argv+=['-v',str(self.directory/name)+':/backup/'+name+':ro,z']
        result=subprocess.run(argv+[self.image],env=env,capture_output=True,text=True,timeout=30)
        if result.returncode or not re.fullmatch('[a-f0-9]{64}\n?',result.stdout):
            raise ValueError('Original isolated restore container acknowledgement unavailable')
        self.cid=result.stdout.strip();self.record['restore'].update(containerID=self.cid,state='started');self.save(self.record)
        self.identity()
        deadline=time.monotonic()+60
        while time.monotonic()<deadline:
            ready=subprocess.run([self.provider,'exec',self.cid,'pg_isready','-h','127.0.0.1','-U',self.user,'-d','restore_control'],
                                 capture_output=True,timeout=min(5,max(.001,deadline-time.monotonic())))
            if ready.returncode==0:return
            time.sleep(.5)
        raise ValueError('Original restore database readiness expired')

    def execute(self):
        self.identity();prefix=[self.provider,'exec',self.cid]
        data.verify_archives(self.directory,self.record['archives'])
        data.capture(prefix+['psql','-X','--no-password','-v','ON_ERROR_STOP=1','-U',self.user,'-d','restore_control','-f','/backup/globals.sql'])
        self.identity()
        data.capture(prefix+['pg_restore','--exit-on-error','--create','--no-password','--username='+self.user,'--dbname=restore_control','/backup/database.dump'])
        self.identity()
        actual=data.Database(data.capture,prefix,self.user).snapshot(exclude_restore_role=self.user)
        if actual!=self.record['retainedData']:raise ValueError('Original restored history or roles differ')
        data.verify_archives(self.directory,self.record['archives'])

    def close(self):
        if self.cid is None:
            self.record['restore']['state']='uncertain-no-original-ID';self.save(self.record)
            raise ValueError('Restore container outcome is unknown; preserve original intent')
        self.identity();data.capture([self.provider,'rm','-f',self.cid])
        present=data.capture([self.provider,'ps','-aq','--filter','id='+self.cid])
        if present.strip():raise ValueError('Original isolated restore cleanup incomplete')
        self.record['restore']['state']='removed';self.save(self.record)


class Backup:
    def __init__(self,config):
        self.config=config;self.pause=Pause(config);self.pause.status()
        self.pause_hash=bootstrap.digest(self.pause.record)
        self.path=config.state_dir/'runtime-upgrade-backup.json'
        self.directory=config.state_dir/'runtime-upgrade-backup'
        self.record=files.bounded_json(self.path,2*1024*1024) if self.path.exists() or self.path.is_symlink() else None
        if self.record is not None and (self.record.get('version')!=VERSION or self.record.get('pauseSHA256')!=self.pause_hash):
            raise ValueError('Original paused backup custody changed')

    def save(self,record):platform.write_record(self.path,record);self.record=record

    def guard(self):
        if bootstrap.digest(files.bounded_json(self.pause.path,16*1024*1024))!=self.pause_hash:
            raise ValueError('Original runtime pause changed during backup')
        return self.pause.target()

    def source(self,operation):
        observed=self.guard();target=self.pause.record['target'];config=self.config
        observer=bootstrap.Preparation(config.base_run,config.kubectl,config.provider,config.cluster,config.state_dir,target['images'])
        observer.use_configured_render(target['documents']);again,connection=observer.observe(paused=True)
        if observed!=again:raise ValueError('Original paused source changed during backup')
        plan=self.pause.record['plan'];helper=install_images.REGISTRY+'@'+plan['target']['images']['migrator']['digest']
        with bootstrap_database_forward.original_database(config.base_run,config.kubectl,connection,observed['resources']['pod'],timeout_seconds=240) as (forwarded,port):
            with Fence(config.provider,helper,forwarded,port,plan['source']['migrations'],expires_at=time.time()+120) as fence:
                self.guard();fence.check()
                prefix=config.kubectl+['exec','ai-sdlc-postgres-0','-n',platform.NAMESPACE,'--']
                result=operation(data.Database(fence.command,prefix))
                self.guard();fence.check()
        self.guard();return result

    def status(self):
        if self.record is None or self.record.get('phase')!='verified' or self.record.get('restore',{}).get('state')!='removed':
            raise ValueError('Backup remains incomplete; preserve original files and restore intent')
        proof=self.record.get('retainedData',{})
        if (not {'@schema','@roles','@membership','@role-settings'}.issubset(proof)
                or type(self.record.get('verifiedAt')) is not int or self.record['verifiedAt']<=0):
            raise ValueError('Complete original restoration proof required')
        for name,value in proof.items():
            field='bytes' if name=='@schema' else 'rows'
            if (not isinstance(value,dict) or set(value)!={field,'sha256'} or type(value[field]) is not int
                    or value[field]<0 or not isinstance(value['sha256'],str) or not re.fullmatch('[a-f0-9]{64}',value['sha256'])):
                raise ValueError('Malformed original restoration proof')
        self.guard();data.verify_archives(self.directory,self.record['archives'])
        return {'version':VERSION,'phase':'verified','scope':'recorded-backup-restoration','verifiedAt':self.record['verifiedAt'],
                'backupRestoredToFreshIsolatedDatabase':True,
                'retainedDataAndRolesVerified':True,'schemaAndSecurityDefinitionsVerified':True,'upgradeAuthorized':False,'rollbackAuthorized':False,
                'workflowEvidence':False,'autonomousEndToEndValidated':False}

    def run(self):
        if self.record is not None:return self.status()
        self.guard()
        if self.directory.exists() or self.directory.is_symlink():raise ValueError('Unrecorded backup directory cannot be adopted')
        stores=[obj for obj in self.pause.record['target']['documents']
                if obj['kind']=='StatefulSet' and obj['metadata']['name']=='ai-sdlc-postgres']
        if len(stores)!=1:raise ValueError('Original PostgreSQL StatefulSet required')
        containers=stores[0]['spec']['template']['spec']['containers']
        if len(containers)!=1 or containers[0]['name']!='postgres':raise ValueError('Original PostgreSQL container required')
        image=containers[0]['image']
        self.record={'version':VERSION,'pauseSHA256':self.pause_hash,'phase':'capturing','createdAt':int(time.time()*1000),
                     'postgresImage':image,'archives':{},'retainedData':{}}
        self.save(self.record);self.directory.mkdir(mode=0o700)
        def export(database):
            artifacts,proof=database.export()
            self.record.update(archives=data.write_archives(self.directory,artifacts),retainedData=proof)
            self.save(self.record)
        self.source(export)
        self.record['phase']='captured';self.save(self.record)
        restore=Restore(self.config.provider,image,self.directory,self.record,self.save)
        self.record['phase']='restoring';self.save(self.record)
        try:restore.start();restore.execute()
        finally:restore.close()
        if self.source(lambda database:database.snapshot())!=self.record['retainedData']:
            raise ValueError('Original retained database changed since the restored backup')
        self.record.update(phase='verified',verifiedAt=int(time.time()*1000));self.save(self.record)
        return self.status()
