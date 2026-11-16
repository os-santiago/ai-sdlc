import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import build_foundation_install as foundation
import install

class FoundationInstallTests(unittest.TestCase):
    def args(self,command='local'):
        return [command,'--cluster','owned','--images',str(install.LOCAL/'images.lock.json'),
                '--publisher-image','quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64,'--controller-digest','sha256:'+'b'*64]

    def test_approved_input_cannot_alias_mutable_candidate_or_hardlink(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);state=root/'ai-sdlc/owned';state.mkdir(parents=True)
            candidate=state/'build-trust-candidate.json';candidate.write_text('{"approved":"old"}')
            alias=root/'approved.json';os.link(candidate,alias)
            with patch.dict(os.environ,{'XDG_STATE_HOME':directory}),patch.object(install,'tools',return_value={}),patch.object(foundation.platform,'lock_state') as lock:
                for source in (candidate,alias):
                    with self.assertRaisesRegex(ValueError,'separate immutable input'):
                        foundation.main(self.args()+['--approved-trust',str(source)])
                lock.assert_not_called()
            self.assertEqual(json.loads(candidate.read_text()),{'approved':'old'})

    def test_plan_does_not_touch_cluster_tools_or_credentials(self):
        import contextlib,io
        with patch.object(install,'tools') as tools,contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(foundation.main(self.args('plan')),0)
        tools.assert_not_called();self.assertFalse(json.loads(output.getvalue())['deliveryActivated'])

    def test_installer_runner_accepts_explicit_nonthrowing_cleanup_probe(self):
        with patch.object(install.subprocess,'run',return_value=subprocess.CompletedProcess([],1,'')) as run:
            result=install.run(['podman','inspect','owned'],check=False,capture_output=True)
        self.assertEqual(result.returncode,1);self.assertFalse(run.call_args.kwargs['check'])

if __name__=='__main__':unittest.main()
