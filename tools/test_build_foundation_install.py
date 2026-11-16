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

    def test_build_image_file_matches_explicit_plan_without_tools(self):
        import contextlib,io
        lock=install.image_lock(install.LOCAL/'images.lock.json')
        selected={'version':'ai-sdlc.build-images/v1','revision':lock['image']['tag'],
                  'publisherImage':'quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64,'controllerDigest':'sha256:'+'b'*64}
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'build-images.json';path.write_text(json.dumps(selected))
            with patch.object(install,'tools') as tools,contextlib.redirect_stdout(io.StringIO()) as explicit:
                foundation.main(self.args('plan'))
            with patch.object(install,'tools') as tools,contextlib.redirect_stdout(io.StringIO()) as document:
                foundation.main(self.args('plan')[:5]+['--build-images',str(path)])
            tools.assert_not_called()
            self.assertEqual(json.loads(explicit.getvalue()),json.loads(document.getvalue()))
            for changes in ({'revision':'0'*40},{'publisherImage':'quay.io/other/image@sha256:'+'a'*64},
                            {'controllerDigest':True},{'unknown':'field'},{'version':'other'}):
                path.write_text(json.dumps(dict(selected,**changes)))
                with patch.object(install,'tools') as tools,self.assertRaises(ValueError):
                    foundation.main(self.args('plan')[:5]+['--build-images',str(path)])
                tools.assert_not_called()
            path.write_text(json.dumps(selected))
            with self.assertRaisesRegex(ValueError,'never a mixture'):
                foundation.main(self.args('plan')+['--build-images',str(path)])
            with self.assertRaises(ValueError):
                foundation.main(self.args('plan')[:5])

    def test_installer_runner_accepts_explicit_nonthrowing_cleanup_probe(self):
        with patch.object(install.subprocess,'run',return_value=subprocess.CompletedProcess([],1,'')) as run:
            result=install.run(['podman','inspect','owned'],check=False,capture_output=True)
        self.assertEqual(result.returncode,1);self.assertFalse(run.call_args.kwargs['check'])

    def test_observer_update_preserves_creation_and_compiler_identity(self):
        import contextlib,copy,io
        original=install.image_lock(install.LOCAL/'images.lock.json')
        updated=copy.deepcopy(original)
        updated['image']['tag']='e'*40
        updated['migrator']['digest']='sha256:'+'e'*64
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'observer-images.json';path.write_text(json.dumps(updated))
            with patch.object(install,'tools') as tools,contextlib.redirect_stdout(io.StringIO()) as before:
                foundation.main(self.args('plan'))
            with patch.object(install,'tools') as tools,contextlib.redirect_stdout(io.StringIO()) as after:
                foundation.main(self.args('plan')+['--observer-images',str(path)])
            tools.assert_not_called()
            old,new=json.loads(before.getvalue()),json.loads(after.getvalue())
            self.assertEqual(old['desired'],new['desired'])
            self.assertEqual(old['physicalStorage'],new['physicalStorage'])
            self.assertEqual(old['resources'],new['resources'])
            self.assertFalse(new['networkReadinessValidated'])
            self.assertEqual(new['profileObserver'],foundation.observer_identity(updated))
            original_image=old['desired']['migratorImage'];new_image=new['profileObserver']['image']
            for operation in ('observe-build-admission','observe-build-trust','observe-build-profile'):
                self.assertEqual(foundation.observation_image(operation,original_image,new_image),new_image)
            self.assertEqual(foundation.observation_image('build-admission',original_image,new_image),original_image)
            for operation in ('prepare-build-slot','compile-build-installation','unknown'):
                with self.assertRaises(ValueError):foundation.observation_image(operation,original_image,new_image)
            state=dict(old['desired'],profileObserver=new['profileObserver'],resources={'original':'retained'})
            before_state=copy.deepcopy(state)
            with self.assertRaisesRegex(ValueError,'implicit observer replacement'):
                foundation.select_observer(state,old['profileObserver'],False)
            self.assertEqual(foundation.select_observer(state,new['profileObserver'],True),new['profileObserver'])
            self.assertEqual(state,before_state)
            with patch.object(install,'tools') as tools,self.assertRaisesRegex(ValueError,'Status cannot'):
                foundation.main(self.args('status')+['--observer-images',str(path)])
            tools.assert_not_called()

if __name__=='__main__':unittest.main()
