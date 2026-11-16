"""Bounded public registry trust on an original kind node; no credentials."""
import hashlib
import re

HOST='registry-service.kind-registry'
ROOT='/etc/containerd/certs.d/'+HOST

READ=r'''set -euo pipefail
p="$1"
test ! -L "$p"; test -f "$p"; test "$(realpath -e "$p")" = "$p"
test "$(stat -c %u "$p")" = 0
test "$(stat -c %h "$p")" = 1
mode="$(stat -c %a "$p")"; test "$((8#$mode & 8#22))" = 0
test "$(stat -c %s "$p")" -le 65536
stat -c '%d:%i' "$p"
cat -- "$p"
'''
DIRECTORY=r'''set -euo pipefail
p="$1"; nonce="$2"; first="$3"
for parent in /etc/containerd /etc/containerd/certs.d; do
  if [ ! -e "$parent" ]; then test "$first" = true; mkdir -m 0755 "$parent"; fi
  test ! -L "$parent"; test -d "$parent"; test "$(realpath -e "$parent")" = "$parent"
  test "$(stat -c %u "$parent")" = 0
  mode="$(stat -c %a "$parent")"; test "$((8#$mode & 8#22))" = 0
done
if [ "$first" = true ]; then
  mkdir -m 0755 "$p"
  (set -o noclobber; printf '%s' "$nonce" > "$p/.ai-sdlc-owner")
fi
test ! -L "$p"; test -d "$p"; test "$(realpath -e "$p")" = "$p"
test "$(stat -c %u "$p")" = 0
mode="$(stat -c %a "$p")"; test "$((8#$mode & 8#22))" = 0
test ! -L "$p/.ai-sdlc-owner"; test -f "$p/.ai-sdlc-owner"
test "$(stat -c %u "$p/.ai-sdlc-owner")" = 0
mode="$(stat -c %a "$p/.ai-sdlc-owner")"; test "$((8#$mode & 8#22))" = 0
test "$(stat -c %s "$p/.ai-sdlc-owner")" = 48
test "$(cat "$p/.ai-sdlc-owner")" = "$nonce"
stat -c '%d:%i' "$p"
'''
CREATE=r'''set -euo pipefail
p="$1"; wanted="$2"
test ! -e "$p"; test ! -L "$p"
umask 022
(set -o noclobber; cat > "$p")
test "$(sha256sum "$p" | cut -d ' ' -f1)" = "$wanted"
'''
APPEND=r'''set -euo pipefail
p=/etc/hosts; identity="$1"; before="$2"; after="$3"
test ! -L "$p"; test -f "$p"
exec 9<> "$p"; flock -x -w 5 9
test "$(stat -c '%d:%i' "$p")" = "$identity"
test "$(sha256sum "$p" | cut -d ' ' -f1)" = "$before"
cat >> "$p"
test "$(stat -c '%d:%i' "$p")" = "$identity"
test "$(sha256sum "$p" | cut -d ' ' -f1)" = "$after"
'''


def sha(value):return hashlib.sha256(value.encode()).hexdigest()


class Files:
    def __init__(self,node,state,save):self.node,self.state,self.save=node,state,save

    def read(self,path):
        value=self.node(['bash','-c',READ,'read',path]).stdout
        identity,content=value.split('\n',1)
        if not re.fullmatch(r'[0-9]+:[0-9]+',identity) or len(content.encode())>65536:
            raise ValueError('Original bounded node file unavailable')
        return identity,content

    def directory(self,mutate):
        first='directoryIntent' not in self.state
        if first:
            if not mutate:raise ValueError('Registry node directory is not installed')
            self.state['directoryIntent']={'nonce':self.state['nonce'],'identity':None};self.save()
        entry=self.state['directoryIntent']
        if entry['nonce']!=self.state['nonce'] or not re.fullmatch('[a-f0-9]{48}',entry['nonce']):
            raise ValueError('Registry node directory nonce changed')
        observed=self.node(['bash','-c',DIRECTORY,'directory',ROOT,entry['nonce'],str(first).lower()]).stdout.strip()
        if not re.fullmatch(r'[0-9]+:[0-9]+',observed) or entry['identity'] not in (None,observed):
            raise ValueError('Original registry node directory replaced')
        if entry['identity'] is None:
            if not mutate:raise ValueError('Registry node directory creation is unresolved')
            entry['identity']=observed;self.save()

    def ensure(self,name,content,mutate):
        if name not in ('ca.crt','hosts.toml') or not 0<len(content.encode())<=65536:
            raise ValueError('Unsupported node trust file')
        path=ROOT+'/'+name
        entries=self.state.setdefault('files',{})
        first=name not in entries
        if first:
            if not mutate:raise ValueError('Registry node file is not installed')
            entries[name]={'sha256':sha(content),'identity':None};self.save()
            self.node(['bash','-c',CREATE,'create',path,sha(content)],input=content)
        entry=entries[name]
        if entry['sha256']!=sha(content):raise ValueError('Registry node desired file changed')
        identity,actual=self.read(path)
        if actual!=content or entry['identity'] not in (None,identity):
            raise ValueError('Original registry node trust file changed')
        if entry['identity'] is None:
            if not mutate:raise ValueError('Registry node file creation is unresolved')
            entry['identity']=identity;self.save()

    def hosts(self,address,mutate):
        identity,content=self.read('/etc/hosts')
        entry=self.state.get('hosts')
        if entry is None:
            if not mutate:raise ValueError('Registry node resolution is not installed')
            for line in content.splitlines():
                if HOST in line.partition('#')[0].lower().split()[1:]:
                    raise ValueError('Registry host already exists without original installation custody')
            line=('' if content.endswith('\n') else '\n')+address+' '+HOST+'\n'
            entry={'identity':identity,'beforeSHA256':sha(content),'afterSHA256':sha(content+line),'line':line,'complete':False}
            self.state['hosts']=entry;self.save()
        expected_line=('' if entry['line'].startswith(address) else '\n')+address+' '+HOST+'\n'
        if entry['identity']!=identity or entry['line']!=expected_line:
            raise ValueError('Original node hosts identity or desired entry changed')
        if sha(content)==entry['afterSHA256']:
            if not entry['complete']:
                if not mutate:raise ValueError('Node hosts write is unresolved')
                entry['complete']=True;self.save()
            return
        if not mutate or entry['complete'] or sha(content)!=entry['beforeSHA256']:
            raise ValueError('Original node hosts content changed')
        self.node(['bash','-c',APPEND,'append',identity,entry['beforeSHA256'],entry['afterSHA256']],input=entry['line'])
        current,after=self.read('/etc/hosts')
        if current!=identity or sha(after)!=entry['afterSHA256']:raise ValueError('Node hosts append did not retain identity')
        entry['complete']=True;self.save()
