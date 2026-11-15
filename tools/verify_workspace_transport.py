#!/usr/bin/env python3
"""Exercise fixed Go source/start/export helpers in disposable kind/SCC/gVisor."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

from verify_executor import verify
from verify_pod_transport import TransportFixture
from verify_workspace_bundle import source_bundle, wire


class WorkspaceTransportFixture(TransportFixture):
    manages_workspace = True

    def __init__(self, kubectl, test_binary):
        super().__init__(kubectl, test_binary, database_required=False)
        self.source_payload = None
        self.candidate = None

    def run(self, scenario, config=None, replacement_uid=None):
        if scenario != 'normal':
            return super().run(scenario, config, replacement_uid)
        if not self.source_payload or len(self.source_payload)>32*1024*1024:
            raise ValueError('Bounded workspace source required')
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-workspace-control-') as directory:
            root = Path(directory)
            source, output, control = root/'source.json', root/'candidate.json', root/'input.json'
            payload = {'Config':config or self.config, 'Binding':self.binding,
                       'SourceFile':str(source), 'OutputFile':str(output)}
            for path, data in ((source,self.source_payload),(control,wire(payload))):
                fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
                with os.fdopen(fd,'wb') as stream:stream.write(data)
            result=subprocess.run([self.test_binary,'-test.run','^TestRealWorkspaceLifecycleFixture$','-test.v'],
                                  env=dict(os.environ,AI_SDLC_WORKSPACE_TRANSPORT_FIXTURE=str(control)),
                                  capture_output=True,timeout=60)
            if result.returncode:
                raise ValueError('Actual workspace lifecycle fixture failed: '+result.stdout.decode(errors='replace')[:1500])
            with output.open('rb') as stream:raw=stream.read(32*1024*1024+1)
            if len(raw)>32*1024*1024:raise ValueError('Candidate export bound')
            self.candidate=json.loads(raw)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl','kubeconfig','render-image','mailbox-image','worker-image','test-binary','source-repository','source-sha'):
        parser.add_argument('--'+name,required=True)
    parser.add_argument('--provider',choices=('docker','podman'),required=True)
    args=parser.parse_args()
    kubectl=[args.kubectl,'--kubeconfig',args.kubeconfig]
    fixture=WorkspaceTransportFixture(kubectl,args.test_binary)
    report=verify(kubectl,args.provider,args.render_image,args.mailbox_image,args.worker_image,
                  source_bundle(Path(args.source_repository),args.source_sha),transport=fixture)
    print(json.dumps(report,indent=2))


if __name__=='__main__':
    main()
