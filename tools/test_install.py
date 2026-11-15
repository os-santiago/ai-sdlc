import importlib.util
import json
from pathlib import Path
import tempfile
import subprocess
import os
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('installer', Path(__file__).with_name('install.py'))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class InstallerBoundaryTests(unittest.TestCase):
    def test_standalone_bootstrap_ref_mismatch_preserves_cached_checkout(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / 'source'
            source.mkdir()
            def git(*args):
                return subprocess.check_output(['git', '-C', str(source), *args], text=True).strip()
            git('init', '-q')
            git('config', 'user.name', 'Installer test')
            git('config', 'user.email', 'installer@example.invalid')
            git('remote', 'add', 'origin', 'https://github.com/os-santiago/ai-sdlc.git')
            (source / 'tools').mkdir()
            (source / 'tools/install.py').write_text('raise SystemExit("must not execute")\n')
            git('add', '.')
            git('commit', '-qm', 'first')
            requested = git('rev-parse', 'HEAD')
            (source / 'second').write_text('committed\n')
            git('add', '.')
            git('commit', '-qm', 'second')
            current = git('rev-parse', 'HEAD')
            (source / 'second').write_text('local edits\n')
            script = root / 'install.sh'
            script.write_bytes((installer.ROOT / 'install.sh').read_bytes())
            result = subprocess.run(['bash', str(script), 'plan'], text=True, capture_output=True,
                                    env=dict(os.environ, AI_SDLC_SOURCE_DIR=str(source), AI_SDLC_INSTALL_REF=requested))
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('differs from AI_SDLC_INSTALL_REF', result.stderr)
            self.assertEqual(git('rev-parse', 'HEAD'), current)
            self.assertEqual((source / 'second').read_text(), 'local edits\n')

    def test_stale_or_failed_creation_marker_cannot_adopt_a_cluster(self):
        with tempfile.TemporaryDirectory() as tmp:
            marker = Path(tmp) / 'ownership.json'
            for value in (None, {'cluster': 'ai-sdlc', 'provider': 'podman'},
                          {'cluster': 'ai-sdlc', 'provider': 'podman', 'clusterUID': 'old-uid'}):
                if value is not None: marker.write_text(json.dumps(value))
                with self.assertRaisesRegex(ValueError, 'identity'):
                    installer.check_ownership(marker, 'ai-sdlc', 'podman', 'new-uid')
            marker.write_text(json.dumps({'cluster': 'ai-sdlc', 'provider': 'podman', 'clusterUID': 'new-uid'}))
            installer.check_ownership(marker, 'ai-sdlc', 'podman', 'new-uid')

    def lock(self):
        return {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'},
                'components': {key: {'digest': 'sha256:' + 'a' * 64} for key in installer.COMPONENTS},
                'migrator': {'digest': 'sha256:' + 'b' * 64}}

    def test_missing_mutable_or_foreign_images_fail_before_install(self):
        for invalid in ('mutable', 'missing', 'foreign', 'namespace', 'component_name', 'migrator'):
            lock = self.lock()
            if invalid == 'migrator': lock['migrator']['digest'] = 'latest'
            if invalid == 'mutable': lock['components']['worker']['digest'] = 'latest'
            if invalid == 'missing': del lock['components']['worker']
            if invalid == 'foreign': lock['image']['repository'] = 'example.org/untrusted'
            if invalid == 'namespace': lock['namespace'] = {'name': 'unrelated'}
            if invalid == 'component_name': lock['components']['worker']['name'] = 'unrelated'
            with tempfile.TemporaryDirectory() as tmp:
                path = Path(tmp) / 'images.json'
                path.write_text(json.dumps(lock))
                with self.assertRaises(ValueError): installer.image_lock(path)

    def test_plan_never_creates_cluster_or_applies_resources(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'images.json'
            path.write_text(json.dumps(self.lock()))
            with patch.object(installer, 'tools', return_value={'helm': 'helm'}), patch.object(installer, 'run') as command:
                self.assertEqual(installer.main(['plan', '--images', str(path)]), 0)
                self.assertEqual(command.call_count, 1)
                self.assertEqual(command.call_args.args[0][:2], ['helm', 'template'])

    def test_invalid_target_and_timeout_never_run_tools(self):
        for args in (['local', '--cluster', '../existing'], ['local', '--timeout', '0m']):
            with patch.object(installer, 'tools') as tools:
                with self.assertRaises(ValueError): installer.main(args)
                tools.assert_not_called()

    def test_corrupt_download_never_becomes_executable(self):
        with tempfile.TemporaryDirectory() as tmp:
            with patch.object(installer.platform, 'system', return_value='Linux'), patch.object(installer.platform, 'machine', return_value='x86_64'), patch.object(installer.urllib.request, 'urlopen') as request:
                request.return_value.__enter__.return_value.read.return_value = b'corrupt'
                with self.assertRaisesRegex(ValueError, 'integrity'):
                    installer.tools(Path(tmp))
                self.assertEqual(list(Path(tmp).iterdir()), [])


if __name__ == '__main__':
    unittest.main()
