"""Administrative custody initialization on the original owned kind node."""
import hashlib
import json
from pathlib import Path
import re
import secrets
import subprocess
import tempfile

MOUNT = r'''set -euo pipefail
slot="$1"; source="$2"; first="$3"; expected_id="$4"
for parent in /var/local /var/local/ai-sdlc /var/local/ai-sdlc/build-slots; do
  if [ ! -e "$parent" ]; then mkdir -m 0755 "$parent"; fi
  test ! -L "$parent"; test -d "$parent"; test "$(realpath -e "$parent")" = "$parent"
  test "$(stat -c %u "$parent")" = 0
  mode="$(stat -c %a "$parent")"; test "$(( 8#$mode & 8#22 ))" = 0
done
if mountpoint -q "$slot"; then
  test ! -L "$slot"; test "$(realpath -e "$slot")" = "$slot"
  test "$(findmnt -n -M "$slot" -o SOURCE)" = "$source"
  if [ -n "$expected_id" ]; then test "$(findmnt -n -M "$slot" -o ID)" = "$expected_id"; fi
else
  test "$first" = true
  test ! -e "$slot"
  mkdir -m 0755 "$slot"
  mount -t tmpfs -o size=4294967296,nr_inodes=32768,mode=0755,uid=0,gid=0 "$source" "$slot"
fi
findmnt -J -M "$slot" -o ID,SOURCE,TARGET,FSTYPE,OPTIONS
'''


def initialize(run, provider, node_id, image, state, save):
    if (provider not in ('podman','docker') or not re.fullmatch(r'[a-f0-9]{64}',node_id)
            or not re.fullmatch(r'quay\.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}',image)):
        raise ValueError('Original node and published immutable migrator required')
    identity=state['storageIdentitySHA256'];boot=state['nodeBootID']
    if not re.fullmatch(r'[a-f0-9]{64}',identity):raise ValueError('Invalid physical storage identity')
    def node(argv,**kwargs):
        return run([provider,'exec',*(['-i'] if 'input' in kwargs else []),node_id,*argv],capture_output=True,timeout=45,**kwargs)
    # Intent precedes any mount; reapply cannot recreate an absent original mount.
    first='mountIntent' not in state
    if first:
        state['mountIntent']={'source':'ai-sdlc-build-'+identity,'target':'/var/local/ai-sdlc/build-slots/'+identity,'id':None}
        save()
    intent=state['mountIntent']
    mount=json.loads(node(['bash','-c',MOUNT,'mount',intent['target'],intent['source'],str(first).lower(),str(intent['id'] or '')]).stdout)['filesystems'][0]
    if mount['source']!=intent['source'] or mount['target']!=intent['target'] or mount['fstype']!='tmpfs':
        raise ValueError('Original dedicated tmpfs is unavailable')
    mount_id=str(mount['id'])
    if intent['id'] is not None and intent['id']!=mount_id:raise ValueError('Physical mount identity changed')
    intent['id']=mount_id;save()
    nonce=secrets.token_hex(20);name='ai-sdlc-foundation-image-'+nonce;cid=None;remote=None
    try:
        cid=run([provider,'create','--name',name,'--network=none','--read-only','--label','ai-sdlc.foundation-extract='+nonce,image],capture_output=True,timeout=45).stdout.strip()
        if not re.fullmatch(r'[a-f0-9]{64}',cid):raise ValueError('Original image extraction container unavailable')
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-foundation-image-') as directory:
            binary=Path(directory)/'component'
            run([provider,'cp',cid+':/component',str(binary)],capture_output=True,timeout=30)
            if binary.is_symlink() or not binary.is_file() or not 0<binary.stat().st_size<128<<20:raise ValueError('Invalid production migrator binary')
            digest=hashlib.sha256(binary.read_bytes()).hexdigest()
            remote=node(['mktemp','-d','/tmp/ai-sdlc-foundation.XXXXXXXXXXXX']).stdout.strip()
            if not re.fullmatch(r'/tmp/ai-sdlc-foundation\.[A-Za-z0-9]{12}',remote):raise ValueError('Invalid private node staging directory')
            run([provider,'cp',str(binary),node_id+':'+remote+'/component'],capture_output=True,timeout=30)
            if node(['sha256sum',remote+'/component']).stdout.split()[0]!=digest:raise ValueError('Copied migrator image identity differs')
            result=json.loads(node([remote+'/component','prepare-build-slot'],input=json.dumps({'storageIdentitySHA256':identity,'nodeBootID':boot})).stdout)
            state['initializer']={'image':image,'binarySHA256':digest,'result':result};save()
    finally:
        try:
            if remote and re.fullmatch(r'/tmp/ai-sdlc-foundation\.[A-Za-z0-9]{12}',remote):node(['rm','-rf','--',remote])
        finally:
            found=run([provider,'inspect',name],capture_output=True,timeout=15,check=False)
            if found.returncode==0:
                actual=json.loads(found.stdout)[0];original=actual.get('Id','')
                if (not re.fullmatch(r'[a-f0-9]{64}',original) or actual.get('Config',{}).get('Labels',{}).get('ai-sdlc.foundation-extract')!=nonce
                        or cid is not None and cid!=original):raise ValueError('Extraction cleanup identity differs')
                run([provider,'rm','-f','-v',original],capture_output=True,timeout=30)
            else:
                names=run([provider,'ps','-a','--format','{{.Names}}'],capture_output=True,timeout=15).stdout.splitlines()
                if name in names:raise ValueError('Extraction container absence is unproven')
