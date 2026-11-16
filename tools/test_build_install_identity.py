import subprocess
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
import json

import build_install_identity


class InstallerIdentityTests(unittest.TestCase):
    def test_substitution_and_lost_create_reply_keep_original_cleanup(self):
        image = 'sha256:' + 'b' * 64
        cid, nonce = 'a' * 64, 'c' * 32
        for mode in ('matching', 'substitution', 'lost-reply'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                binary = Path(directory) / 'input'
                binary.write_bytes(b'component' if mode != 'substitution' else b'changed')
                calls = []

                def run(argv, **kwargs):
                    calls.append(argv)
                    operation = argv[1]
                    if operation == 'image':
                        value = json.dumps([{'Id': image}])
                    elif operation == 'create':
                        if mode == 'lost-reply':
                            raise subprocess.TimeoutExpired(argv, 30)
                        value = cid
                    elif operation == 'cp':
                        Path(argv[-1]).write_bytes(b'component')
                        value = ''
                    elif operation == 'inspect':
                        value = json.dumps([{'Id': cid, 'Config': {'Labels': {'ai-sdlc.installer-identity': nonce}}}])
                    elif operation == 'rm':
                        self.assertEqual(argv[-1], cid)
                        value = ''
                    else:
                        self.fail('Inspection executed an unexpected operation: ' + operation)
                    return SimpleNamespace(stdout=value, returncode=0)

                with patch.object(build_install_identity.subprocess, 'run', side_effect=run), patch.object(build_install_identity.secrets, 'token_hex', return_value=nonce):
                    if mode == 'matching':
                        proof = build_install_identity.verify('podman', image, binary)
                        self.assertEqual(proof['imageID'], image)
                    else:
                        error = ValueError if mode == 'substitution' else subprocess.TimeoutExpired
                        with self.assertRaises(error):
                            build_install_identity.verify('podman', image, binary)
                self.assertEqual(calls[-1], ['podman', 'rm', '-f', '-v', cid])


if __name__ == '__main__':
    unittest.main()
