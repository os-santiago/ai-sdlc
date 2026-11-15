"""A stop request never substitutes for observation of the original writer."""
import copy
import unittest

from verify_workspace_bundle import writer_exit


class WriterExitTests(unittest.TestCase):
    def observation(self):
        return {'metadata': {'uid': 'original-pod'}, 'status': {'containerStatuses': [
            {'name': 'probe', 'containerID': 'containerd://original', 'restartCount': 0,
             'state': {'terminated': {'exitCode': 0}}}]}}

    def test_only_original_clean_exit_permits_export(self):
        original = self.observation()
        self.assertTrue(writer_exit(original, 'original-pod', 'containerd://original'))
        for mutation in ('pod', 'container', 'restart', 'killed', 'missing-exit'):
            with self.subTest(mutation=mutation):
                observed = copy.deepcopy(original)
                worker = observed['status']['containerStatuses'][0]
                if mutation == 'pod':
                    observed['metadata']['uid'] = 'replacement'
                elif mutation == 'container':
                    worker['containerID'] = 'containerd://replacement'
                elif mutation == 'restart':
                    worker['restartCount'] = 1
                elif mutation == 'killed':
                    worker['state']['terminated']['exitCode'] = 137
                else:
                    worker['state']['terminated'] = {}
                with self.assertRaises(ValueError):
                    writer_exit(observed, 'original-pod', 'containerd://original')

    def test_running_writer_requires_more_observation(self):
        observed = self.observation()
        observed['status']['containerStatuses'][0]['state'] = {'running': {}}
        self.assertFalse(writer_exit(observed, 'original-pod', 'containerd://original'))
