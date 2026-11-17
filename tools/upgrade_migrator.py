"""Own one invocation of the published migrator; the caller supplies upgrade gates."""
import json
import os
import re
import secrets
from urllib.parse import urlsplit

import install
from installer_process import capture

LABEL='ai-sdlc.upgrade-migrator'


class Migrator:
    def __init__(self,provider,image,connection,port,passwords,record,save):
        parsed=urlsplit(connection)
        if (provider not in ('podman','docker') or not re.fullmatch(r'quay\.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}',image)
                or parsed.scheme!='postgresql' or parsed.hostname!='127.0.0.1' or parsed.port!=port
                or type(port) is not int or not 1024<=port<=65535 or parsed.username!='ai_sdlc'
                or not parsed.password or parsed.path!='/ai_sdlc' or parsed.query!='sslmode=disable' or parsed.fragment):
            raise ValueError('Reviewed migrator and original forwarded database required')
        if (not isinstance(passwords,dict) or set(passwords)!=set(install.ALL_ROLES)
                or any(not isinstance(value,str) or not re.fullmatch('[A-Za-z0-9_-]{32,128}',value) for value in passwords.values())
                or len(set(passwords.values()))!=len(passwords)):
            raise ValueError('Original distinct component credentials required')
        self.provider,self.image,self.connection=provider,image,connection
        self.material=json.dumps(passwords).encode();self.record,self.save=record,save
        self.owner=secrets.token_hex(24);self.name='ai-sdlc-upgrade-migrator-'+self.owner;self.cid=None

    def identity(self):
        if self.cid is None:raise ValueError('Original migration container acknowledgement missing')
        values=json.loads(capture([self.provider,'container','inspect',self.cid]))
        if (not isinstance(values,list) or len(values)!=1 or values[0].get('Id')!=self.cid
                or values[0].get('Config',{}).get('Labels',{}).get(LABEL)!=self.owner
                or values[0].get('HostConfig',{}).get('NetworkMode')!='host'
                or values[0].get('Config',{}).get('Cmd')!=['migrate']
                or values[0].get('Config',{}).get('Image')!=self.image):
            raise ValueError('Original migrator container custody changed')
        return values[0]

    def run(self):
        if any(key in self.record for key in ('state','nonce','name','containerID','cleanup')) or self.cid is not None:
            raise ValueError('Original migration intent cannot be replayed')
        self.record.update(nonce=self.owner,name=self.name,image=self.image,state='creating')
        self.save(self.record)
        env=dict(os.environ,DATABASE_URL=self.connection)
        argv=[self.provider,'create','--name',self.name,'--label',LABEL+'='+self.owner,'--network','host','-i',
              '--read-only','--cap-drop=ALL','--security-opt=no-new-privileges','--memory=256m','--cpus=1',
              '--log-driver=none','--env','DATABASE_URL','--env','AI_SDLC_DATABASE_CREDENTIALS_PATH=/dev/stdin',
              '--env','AI_SDLC_RUNTIME_MODE=verification']
        for key in ('AI_SDLC_RELEASE','AI_SDLC_PROVENANCE','AI_SDLC_BUILD','AI_SDLC_MERGE','AI_SDLC_PUBLICATION','AI_SDLC_INDEPENDENT_REVIEW'):
            argv+=['--env',key+'=true']
        raw=capture(argv+[self.image,'migrate'],env=env)
        if not re.fullmatch(b'[a-f0-9]{64}\n?',raw):raise ValueError('Original migrator create acknowledgement unavailable')
        self.cid=raw.decode().strip();self.record.update(containerID=self.cid,state='created');self.save(self.record)
        before=self.identity()
        if before.get('State',{}).get('Status')!='created' or before['State'].get('Running'):
            raise ValueError('Original migrator was already started')
        self.record['state']='starting';self.save(self.record)
        # DB URI is inherited by name, never embedded in argv. The original
        # component role document travels only through its bounded private stdin.
        capture([self.provider,'start','--attach','--interactive',self.cid],input=self.material,timeout=120)
        after=self.identity()['State']
        if after.get('Status')!='exited' or after.get('Running') or after.get('ExitCode')!=0:
            raise ValueError('Original migrator completion unavailable')
        self.record['state']='completed';self.save(self.record)

    def close(self):
        self.material=None;self.connection=None
        if self.cid is None:
            self.record['cleanup']='uncertain-no-original-ID';self.save(self.record)
            raise ValueError('Preserve original migration creation intent')
        self.identity();capture([self.provider,'rm','-f',self.cid])
        if capture([self.provider,'ps','-aq','--filter','id='+self.cid]).strip():
            raise ValueError('Original migration container cleanup incomplete')
        self.record['cleanup']='removed';self.save(self.record)
