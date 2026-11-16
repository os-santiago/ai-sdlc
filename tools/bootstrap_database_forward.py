"""Bounded administrative access to the original database through Kubernetes.

Native policy need not admit non-Pod traffic from an auxiliary node-network
container. Keep the inspector unprivileged and use the authenticated API's
port-forward to its original Pod; this never creates a Kubernetes workload.
"""
from contextlib import contextmanager
import json
import os
import re
import selectors
import shutil
import subprocess
import time
from urllib.parse import urlsplit, urlunsplit


@contextmanager
def original_database(run,kubectl,uri,pod_uid):
    try:
        parsed=urlsplit(uri)
        valid=(parsed.scheme=='postgresql' and parsed.port==5432 and parsed.hostname is not None
            and parsed.username=='ai_sdlc' and bool(parsed.password) and parsed.path=='/ai_sdlc'
            and parsed.query=='sslmode=disable' and not parsed.fragment)
    except ValueError:
        valid=False
    if not valid:raise ValueError('Original administrative database URI required')
    timeout=shutil.which('timeout')
    if timeout is None:raise ValueError('Bounded database observation requires the Linux timeout utility')
    def original():
        raw=run(kubectl+['get','pod','ai-sdlc-postgres-0','-n','ai-sdlc-runtime','-o','json'],capture_output=True,timeout=10).stdout
        if len(raw)>4*1024*1024:raise ValueError('Database Pod observation exceeded bound')
        pod=json.loads(raw);meta=pod.get('metadata',{})
        if (meta.get('uid')!=pod_uid or meta.get('deletionTimestamp')
                or not any(c.get('type')=='Ready' and c.get('status')=='True' for c in pod.get('status',{}).get('conditions',[]))):
            raise ValueError('Original database Pod changed during read-only observation')
    original()
    # timeout bounds the forwarding process even if the supervising installer
    # disappears. Neither argv nor output contains a database credential.
    process=subprocess.Popen([timeout,'--kill-after=5s','75s',*kubectl,'port-forward',
        '-n','ai-sdlc-runtime','pod/ai-sdlc-postgres-0',':5432','--address','127.0.0.1'],
        stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,bufsize=0)
    try:
        end=time.monotonic()+15;received=b'';port=None
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout,selectors.EVENT_READ)
            while port is None:
                if process.poll() is not None:raise ValueError('Original database forwarding stopped')
                remaining=end-time.monotonic()
                if remaining<=0:raise ValueError('Original database forwarding readiness timed out')
                if not selector.select(min(remaining,.5)):continue
                chunk=os.read(process.stdout.fileno(),1025-len(received))
                if not chunk:raise ValueError('Original database forwarding produced no endpoint')
                received+=chunk
                if len(received)>1024:raise ValueError('Database forwarding output exceeded bound')
                for line in received.splitlines():
                    match=re.fullmatch(rb'Forwarding from 127\.0\.0\.1:([0-9]{1,5}) -> 5432',line)
                    if match and 1024<=int(match[1])<=65535:port=int(match[1])
        original()
        if process.poll() is not None:raise ValueError('Original database forwarding stopped')
        private=urlunsplit((parsed.scheme,parsed.netloc.rsplit('@',1)[0]+'@127.0.0.1:'+str(port),parsed.path,parsed.query,''))
        yield private,port
        original()
        if process.poll() is not None:raise ValueError('Original database forwarding stopped before observation completed')
    finally:
        process.terminate()
        try:process.wait(timeout=5)
        except subprocess.TimeoutExpired:process.kill();process.wait(timeout=5)
        process.stdout.close()
