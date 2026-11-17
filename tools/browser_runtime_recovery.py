"""Bounded restoration of the original node configuration, never installation."""
import json
import subprocess
import time

import offline_runtime


class Recovery:
    def __init__(self,provider,node,cid,cluster,stage,before,desired):
        self.provider,self.node,self.cid,self.cluster=provider,node,cid,cluster
        self.stage,self.before,self.desired=stage,before,desired
        self.deadline=None

    def _command(self,argv,value=None,timeout=30):
        if self.deadline is None:self.deadline=time.monotonic()+90
        remaining=self.deadline-time.monotonic()
        if remaining<=0:raise ValueError('Original browser recovery budget expired')
        return subprocess.run(argv,input=value,text=True,capture_output=True,
            check=True,timeout=min(timeout,remaining)).stdout

    def _run(self,argv,value=None,timeout=30):
        fresh=json.loads(self._command([self.provider,'inspect',self.node]))[0]
        if fresh['Id']!=self.cid or fresh.get('Config',{}).get('Labels',{}).get('io.x-k8s.kind.cluster')!=self.cluster:
            raise ValueError('Original browser recovery node replaced')
        return self._command([self.provider,'exec','-i',self.cid]+argv,value,timeout)

    def _current(self):
        self._run(['test','-f',offline_runtime.NODE_CONFIG])
        self._run(['test','!','-L',offline_runtime.NODE_CONFIG])
        return self._run(['cat',offline_runtime.NODE_CONFIG])

    def restore(self):
        current=self._current()
        if current not in (self.before,self.desired):raise ValueError('Foreign containerd configuration prevents restoration')
        if current==self.before:return
        self._run(['test','!','-L',self.stage])
        self._run(['tee',self.stage],self.before)
        if self._current()!=current:raise ValueError('Containerd configuration changed during restoration')
        self._run(['mv','-T',self.stage,offline_runtime.NODE_CONFIG])
        self._run(['systemctl','restart','containerd'],timeout=60)
        if self._current()!=self.before:raise ValueError('Original containerd restoration not observed')

    def cleanup(self):
        self._run(['rm','-f','--',self.stage])
