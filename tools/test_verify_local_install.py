import copy
import subprocess
import sys
import unittest
from pathlib import Path

import verify_local_install as verifier


class VerificationTests(unittest.TestCase):
    def fixture(self):
        names = verifier.install.COMPONENT_NAMES
        lock = {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'}, 'components': {}}
        deployments = []
        for index, (key, name) in enumerate(names.items()):
            digest = 'sha256:' + str(index) * 64
            lock['components'][key] = {'digest': digest}
            deployments.append({'metadata': {'name': 'ai-sdlc-' + name}, 'status': {'readyReplicas': 1},
                                'spec': {'template': {'spec': {'containers': [{'image': lock['image']['repository'] + '@' + digest}]}}}})
        return deployments, lock

    def test_swapped_component_images_are_rejected(self):
        deployments, lock = self.fixture()
        verifier.verify_deployments(deployments, lock)
        swapped = copy.deepcopy(deployments)
        swapped[0]['spec'], swapped[3]['spec'] = swapped[3]['spec'], swapped[0]['spec']
        with self.assertRaisesRegex(ValueError, 'mapping'):
            verifier.verify_deployments(swapped, lock)

    def test_python_optimization_cannot_disable_validation(self):
        result = subprocess.run([sys.executable, '-O', '-c',
                                 'from verify_local_install import require; require(False, "denied")'],
                                cwd=Path(__file__).parent, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('denied', result.stderr)


if __name__ == '__main__': unittest.main()
