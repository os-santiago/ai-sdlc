import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import dependency_proxy_install

spec = importlib.util.spec_from_file_location('installer', Path(__file__).with_name('install.py'))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class DependencyProxyInstallTests(unittest.TestCase):
    def test_selection_is_strict_and_canonical(self):
        self.assertEqual(dependency_proxy_install.parse('maven'), ('maven',))
        self.assertEqual(dependency_proxy_install.parse('npm,maven'), ('maven', 'npm'))
        for bad in ('', 'pip', 'maven,maven', 'maven,', ' maven', 'MAVEN', None):
            with self.assertRaises(ValueError, msg=bad):
                dependency_proxy_install.parse(bad)
        self.assertEqual(dependency_proxy_install.values(('npm',)),
                         {'enabled': True, 'maven': {'enabled': False}, 'npm': {'enabled': True}})

    def test_plan_renders_the_proxy_only_when_selected(self):
        lock = {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'},
                'components': {key: {'digest': 'sha256:' + 'a' * 64} for key in installer.COMPONENTS},
                'migrator': {'digest': 'sha256:' + 'b' * 64}}
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'images.json'
            path.write_text(json.dumps(lock))
            rendered = []
            def run(command, **kwargs):
                values = [str(command[i + 1]) for i, arg in enumerate(command) if arg == '-f']
                rendered.append(json.loads(Path(values[-1]).read_text()))
                return subprocess.CompletedProcess(command, 0, '')
            with patch.object(installer, 'tools', return_value={'helm': 'helm'}), patch.object(installer, 'run', side_effect=run), \
                 patch('builtins.print'):
                self.assertEqual(installer.main(['plan', '--images', str(path)]), 0)
                default = rendered[-1]
                self.assertEqual(installer.main(['plan', '--images', str(path), '--dependency-proxy', 'maven,npm']), 0)
                self.assertEqual(rendered[-1], dict(default, dependencyProxy={'enabled': True, 'maven': {'enabled': True}, 'npm': {'enabled': True}}))
            self.assertNotIn('dependencyProxy', default)
        for args in (['status', '--dependency-proxy', 'maven'], ['plan', '--dependency-proxy', 'gradle'],
                     ['local', '--configure-prepared', '--dependency-proxy', 'maven']):
            with self.subTest(args=args), patch.object(installer, 'tools') as tools:
                with self.assertRaises((ValueError, SystemExit)):
                    installer.main(args)
                tools.assert_not_called()


if __name__ == '__main__':
    unittest.main()
