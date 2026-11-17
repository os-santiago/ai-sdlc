import tomllib
import unittest

import browser_runtime as browser
import offline_runtime as offline


class BrowserRuntime(unittest.TestCase):
    def test_separate_handler_preserves_existing_isolation_and_reapplies(self):
        base = '''version = 2
[plugins."io.containerd.grpc.v1.cri".containerd]
default_runtime_name = "runc"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
runtime_type = "io.containerd.runc.v2"
'''
        original = offline.node_configuration(base, (offline.MANIFESTS/'containerd.toml').read_text())
        fragment = (browser.MANIFESTS/'containerd.toml').read_text()
        desired = browser.node_configuration(original, fragment)
        self.assertTrue(desired.startswith(original))
        self.assertEqual(browser.node_configuration(desired, fragment), desired)
        self.assertEqual(offline.node_configuration(desired, (offline.MANIFESTS/'containerd.toml').read_text()), desired)
        for invalid in (base, original.replace('default_runtime_name = "runc"', 'default_runtime_name = "ai-sdlc-browser"'),
                        original.replace('pod_annotations = []', 'pod_annotations = ["*"]'),
                        desired.replace('# BEGIN AI-SDLC BROWSER RUNTIME v1', '# foreign'),
                        desired.replace('ai-sdlc-browser.toml', 'untrusted.toml'),
                        'imports = ["/override.toml"]\n'+original):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                browser.node_configuration(invalid, fragment)

    def test_browser_cannot_override_network_or_host_access(self):
        browser_config = tomllib.loads((browser.MANIFESTS/'runsc.toml').read_text())
        offline_config = tomllib.loads((offline.MANIFESTS/'runsc.toml').read_text())
        self.assertEqual(browser_config['runsc_config']['network'], 'sandbox')
        self.assertEqual(offline_config['runsc_config']['network'], 'none')
        browser_config['runsc_config']['network'] = 'none'
        self.assertEqual(browser_config, offline_config)
        handler = tomllib.loads((browser.MANIFESTS/'containerd.toml').read_text())['plugins']['io.containerd.grpc.v1.cri']['containerd']['runtimes']
        self.assertEqual(set(handler), {'ai-sdlc-browser'})
        self.assertEqual(handler['ai-sdlc-browser']['pod_annotations'], [])
        self.assertEqual(handler['ai-sdlc-browser']['container_annotations'], [])


if __name__ == '__main__':
    unittest.main()
