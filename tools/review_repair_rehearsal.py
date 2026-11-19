"""Run the exact published migrator inside an owned offline restore namespace."""
import json
import os
import re
import secrets
import time
from urllib.parse import urlsplit, urlunsplit

import install
from installer_process import capture
import review_repair_upgrade_data as compatibility

LABEL='ai-sdlc.repair-rehearsal'


class Rehearsal:
    def __init__(self, restore, image, connection, passwords, record, save, deadline, guard):
        parsed=urlsplit(connection)
        if (parsed.scheme not in ('postgres','postgresql') or parsed.username!='ai_sdlc' or not parsed.password
                or parsed.path!='/ai_sdlc' or not re.fullmatch(r'quay\.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}',image)
                or set(passwords)!=set(install.ALL_ROLES)
                or any(not re.fullmatch('[A-Za-z0-9_-]{32,128}',v) for v in passwords.values())):
            raise ValueError('Original database credentials and exact published rehearsal helper required')
        self.restore,self.image,self.record,self.save=restore,image,record,save
        # Each guard re-observes the paused runtime; 300s keeps a completed
        # rehearsal from expiring during its own final checks.
        self.guard,self.deadline=guard,min(deadline,time.time()+300)
        self.connection=urlunsplit(('postgresql',parsed.netloc.rsplit('@',1)[0]+'@127.0.0.1:5432','/ai_sdlc','sslmode=disable',''))
        self.material=json.dumps(passwords).encode()
        self.owner=secrets.token_hex(24);self.name='ai-sdlc-repair-rehearsal-'+self.owner;self.cid=None

    def live(self):
        if time.time()>=self.deadline: raise ValueError('Original repair rehearsal window expired')
        self.restore.identity()
        self.guard()

    def command(self,args,**kwargs):
        self.live()
        # installer_process.capture admits at most 120 s per command.
        return capture(args,timeout=min(120,self.deadline-time.time()),**kwargs)

    def identity(self):
        self.restore.identity()
        rows=json.loads(capture([self.restore.provider,'container','inspect',self.cid]))
        if (len(rows)!=1 or rows[0].get('Id')!=self.cid
                or rows[0].get('Config',{}).get('Labels',{}).get(LABEL)!=self.owner
                or rows[0].get('HostConfig',{}).get('NetworkMode')!='container:'+self.restore.cid
                or rows[0].get('Config',{}).get('Image')!=self.image
                or rows[0].get('Config',{}).get('Cmd')!=['migrate']):
            raise ValueError('Original rehearsal helper custody changed')
        return rows[0]

    def run(self, source, catalog):
        if self.record: raise ValueError('Original rehearsal intent cannot be replayed')
        self.live()
        self.record.update(nonce=self.owner,name=self.name,image=self.image,restoreContainerID=self.restore.cid,state='creating')
        self.save()
        env=dict(os.environ,DATABASE_URL=self.connection)
        argv=[self.restore.provider,'create','--name',self.name,'--label',LABEL+'='+self.owner,
              '--network','container:'+self.restore.cid,'-i','--read-only','--cap-drop=ALL',
              '--security-opt=no-new-privileges','--memory=256m','--cpus=1','--log-driver=none',
              '--env','DATABASE_URL','--env','AI_SDLC_DATABASE_CREDENTIALS_PATH=/dev/stdin','--env','AI_SDLC_RUNTIME_MODE=verification']
        for key in ('AI_SDLC_RELEASE','AI_SDLC_PROVENANCE','AI_SDLC_BUILD','AI_SDLC_MERGE','AI_SDLC_PUBLICATION','AI_SDLC_INDEPENDENT_REVIEW'):
            argv+=['--env',key+'=true']
        raw=self.command(argv+[self.image,'migrate'],env=env)
        if not re.fullmatch(b'[a-f0-9]{64}\n?',raw):raise ValueError('Rehearsal helper acknowledgement unavailable')
        self.cid=raw.decode().strip();self.record.update(containerID=self.cid,state='created');self.save()
        before=self.identity()['State']
        if before.get('Status')!='created' or before.get('Running'):raise ValueError('Rehearsal helper already started')
        self.live();self.record['state']='starting';self.save()
        self.command([self.restore.provider,'start','--attach','--interactive',self.cid],input=self.material)
        after=self.identity()['State']
        if after.get('Status')!='exited' or after.get('Running') or after.get('ExitCode')!=0:
            raise ValueError('Rehearsal helper completion unavailable')
        from upgrade_backup_data import Database
        db=Database(capture,[self.restore.provider,'exec',self.restore.cid],self.restore.user)
        expected=compatibility.Compatibility(db).rehearsed(source,catalog,self.restore.user)
        self.live();self.record.update(state='completed',expected=expected);self.save()
        return expected

    def close(self):
        self.material=None;self.connection=None
        if self.cid is None:
            if not self.record:return
            self.record['cleanup']='uncertain-no-original-ID';self.save()
            raise ValueError('Preserve unknown rehearsal create intent')
        self.identity();capture([self.restore.provider,'rm','-f',self.cid])
        if capture([self.restore.provider,'ps','-aq','--filter','id='+self.cid]).strip():
            raise ValueError('Original rehearsal helper cleanup incomplete')
        self.record['cleanup']='removed';self.save()
