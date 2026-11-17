"""Private bounded transport to the reviewed migrator's administrative fence."""
import json
import math
import os
import re
import secrets
import selectors
import subprocess
import time
from urllib.parse import urlsplit

from install_images import decode
import installer_process

LABEL = 'ai-sdlc.upgrade-fence'


class HistoryRejected(ValueError):
    """The compiled helper observed a false history predicate, not an I/O error."""


class Fence:
    def __init__(self, provider, image, connection, port, source_catalog, *, expires_at, contained_review=False):
        if type(contained_review) is not bool:
            raise ValueError('Explicit administrative history protocol required')
        self.protocol = 'ai-sdlc.reviewer-upgrade-fence/v1' if contained_review else 'ai-sdlc.upgrade-fence/v1'
        self.operation = 'hold-reviewer-upgrade-fence' if contained_review else 'hold-upgrade-fence'
        if type(expires_at) not in (int,float) or not math.isfinite(expires_at): raise ValueError('Finite original pause deadline required')
        parsed = urlsplit(connection)
        if (provider not in ('podman', 'docker') or not re.fullmatch(r'quay\.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}', image)
                or parsed.scheme != 'postgresql' or parsed.hostname != '127.0.0.1' or parsed.port != port
                or type(port) is not int or not 1024 <= port <= 65535 or parsed.username != 'ai_sdlc'
                or not parsed.password or parsed.path != '/ai_sdlc' or parsed.query != 'sslmode=disable' or parsed.fragment):
            raise ValueError('Reviewed migrator and original forwarded database required')
        self.provider, self.image = provider, image
        self.material = {'databaseURL': connection, 'database': 'ai_sdlc', 'sourceCatalog': source_catalog}
        self.deadline = time.monotonic() + min(125, expires_at-time.time())
        if self.deadline <= time.monotonic(): raise ValueError('Original pause deadline expired')
        self.owner = secrets.token_hex(24)
        self.name = 'ai-sdlc-upgrade-fence-' + self.owner
        self.process = None
        self.nonce = self.expiry = self.container_id = None
        self.buffer = b''
        self.released = False

    def remaining(self, cap=10):
        available = self.deadline - time.monotonic()
        if available <= 0: raise ValueError('Original administrative fence expired')
        return min(cap, available)

    def live(self):
        self.remaining()
        if self.process is None or self.process.poll() is not None:
            raise ValueError('Original administrative fence process stopped')

    def write(self, value):
        raw = json.dumps(value, separators=(',', ':')).encode()+b'\n'
        if len(raw) > 131072: raise ValueError('Private fence input exceeds bound')
        with selectors.DefaultSelector() as selector:
            selector.register(self.process.stdin, selectors.EVENT_WRITE)
            while raw:
                self.live()
                if not selector.select(self.remaining(.25)): continue
                try: written = os.write(self.process.stdin.fileno(), raw[:16384])
                except BlockingIOError: continue
                if written <= 0: raise ValueError('Private fence input stopped')
                raw = raw[written:]

    def read(self, state):
        with selectors.DefaultSelector() as selector:
            selector.register(self.process.stdout, selectors.EVENT_READ)
            while b'\n' not in self.buffer:
                self.remaining()
                # A final complete response may be buffered after process exit.
                if not selector.select(self.remaining(.25)):
                    self.live(); continue
                part = os.read(self.process.stdout.fileno(), 4097-len(self.buffer))
                if not part: raise ValueError('Administrative fence response missing')
                self.buffer += part
                if len(self.buffer) > 4096: raise ValueError('Fence response exceeds bound')
        line, self.buffer = self.buffer.split(b'\n', 1)
        if self.buffer: raise ValueError('Unsolicited fence response')
        value = decode(line)
        if (state == 'held' and self.nonce is None and isinstance(value, dict)
                and set(value) == {'version','state','reason','workflowRowsWritten','upgradeAuthorized'}
                and value['version'] == self.protocol and value['state'] == 'rejected'
                and value['reason'] == 'history' and value['workflowRowsWritten'] is False
                and value['upgradeAuthorized'] is False):
            raise HistoryRejected('Compiled administrative history predicate rejected')
        if (not isinstance(value, dict) or set(value) != {'version','state','nonce','expiresAt','workflowRowsWritten','upgradeAuthorized'}
                or value['version'] != self.protocol or value['state'] != state
                or value['workflowRowsWritten'] is not False or value['upgradeAuthorized'] is not False
                or not isinstance(value['nonce'],str) or not re.fullmatch('[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}',value['nonce'])
                or type(value['expiresAt']) is not int):
            raise ValueError('Invalid administrative fence acknowledgement')
        if self.nonce is None:
            remaining = value['expiresAt']/1000-time.time()
            if not 0 < remaining <= 120: raise ValueError('Invalid original fence lifetime')
            self.nonce, self.expiry = value['nonce'], value['expiresAt']
            self.deadline = min(self.deadline, time.monotonic()+remaining)
        elif value['nonce'] != self.nonce or value['expiresAt'] != self.expiry:
            raise ValueError('Original administrative fence changed')
        return value

    def check(self):
        if self.released: raise ValueError('Administrative fence already released')
        self.live(); self.write({'operation':'check','nonce':self.nonce}); self.read('held')
        self.live()

    def release(self):
        self.check(); self.write({'operation':'release','nonce':self.nonce}); self.read('released')
        self.released = True
        if self.process.wait(timeout=self.remaining(10)) != 0:
            raise ValueError('Administrative fence completion was not successful')

    def command(self, args, *, timeout=30):
        """Cancel an owned external command when the original fence disappears."""
        self.check()
        raw=installer_process.capture(args,timeout=self.remaining(timeout),monitor=self.live)
        self.check()
        return raw

    def owned(self):
        result = subprocess.run([self.provider,'ps','-aq','--filter','name=^'+self.name+'$'],
                                capture_output=True,text=True,timeout=10,check=False)
        if result.returncode: raise ValueError('Cannot establish fence container absence')
        ids = result.stdout.split()
        if not ids: return None
        if len(ids)!=1 or not re.fullmatch('[a-f0-9]{12,64}',ids[0]): raise ValueError('Unexpected fence container inventory')
        result = subprocess.run([self.provider,'container','inspect',ids[0]],capture_output=True,text=True,timeout=10,check=False)
        if result.returncode or len(result.stdout)>1024*1024: raise ValueError('Fence container identity unavailable')
        values = json.loads(result.stdout)
        if len(values)!=1: raise ValueError('Unexpected fence container observation')
        obj=values[0];identity=obj.get('Id')
        if (not isinstance(identity,str) or not re.fullmatch('[a-f0-9]{64}',identity)
                or obj.get('Config',{}).get('Labels',{}).get(LABEL)!=self.owner
                or self.container_id is not None and identity!=self.container_id):
            raise ValueError('Original fence container custody changed')
        self.container_id=identity
        return identity

    def close(self):
        if self.process is not None:
            for pipe in (self.process.stdin,self.process.stdout):
                if pipe is not None: pipe.close()
            if self.process.poll() is None:
                self.process.terminate()
                try: self.process.wait(timeout=3)
                except subprocess.TimeoutExpired: self.process.kill(); self.process.wait(timeout=3)
        original=self.owned()
        if original is not None:
            result=subprocess.run([self.provider,'rm','-f',original],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15,check=False)
            if result.returncode or self.owned() is not None: raise ValueError('Original fence container cleanup incomplete')

    def __enter__(self):
        # Keep the stopped container until CID/label-bound cleanup. Automatic
        # removal can race ps -> inspect and hide an explicit denial response.
        args=[self.provider,'run','--name',self.name,'-i','--label',LABEL+'='+self.owner,
              '--network','host','--read-only','--cap-drop=ALL','--security-opt=no-new-privileges',
              '--memory=256m','--cpus=1','--log-driver=none',self.image,self.operation]
        try:
            self.process=subprocess.Popen(args,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,bufsize=0)
            os.set_blocking(self.process.stdin.fileno(),False)
            self.write(self.material);self.read('held');self.material=None
            if self.owned() is None: raise ValueError('Original fence container disappeared')
            return self
        except BaseException:
            self.close();raise

    def __exit__(self,kind,value,traceback):
        try:
            if kind is None and not self.released:self.release()
        finally:self.close()
