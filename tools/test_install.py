import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('installer', Path(__file__).with_name('install.py'))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class InstallerBoundaryTests(unittest.TestCase):
    def lock(self):
        return {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'},
                'components': {key: {'digest': 'sha256:' + 'a' * 64} for key in installer.COMPONENTS}}

    def test_missing_mutable_or_foreign_images_fail_before_install(self):
        for invalid in ('mutable', 'missing', 'foreign'):
            lock = self.lock()
            if invalid == 'mutable': lock['components']['worker']['digest'] = 'latest'
            if invalid == 'missing': del lock['components']['worker']
            if invalid == 'foreign': lock['image']['repository'] = 'example.org/untrusted'
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
