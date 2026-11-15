from types import SimpleNamespace
import unittest
from unittest.mock import Mock

from network_probe import cleanup_namespace, connection_result, pod


class NetworkConformanceTests(unittest.TestCase):
    def test_cleanup_uses_atomic_uid_precondition_and_never_retries_conflict(self):
        call = Mock(side_effect=ValueError('Conflict: replacement namespace has a different UID'))
        with self.assertRaises(ValueError):
            cleanup_namespace(call, 'owned-probe', 'original-uid')
        call.assert_called_once_with(
            ['delete', '--raw', '/api/v1/namespaces/owned-probe', '-f', '-'],
            {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': 'original-uid'}})

    def test_transport_or_exec_failure_is_never_a_successful_denial(self):
        for code, output in [(1, ''), (3, ''), (0, ''), (2, 'probe:allowed'), (0, 'probe:denied')]:
            with self.assertRaises(ValueError):
                connection_result(SimpleNamespace(returncode=code, stdout=output))
        self.assertFalse(connection_result(SimpleNamespace(returncode=3, stdout='probe:denied\n')))
        self.assertTrue(connection_result(SimpleNamespace(returncode=0, stdout='probe:allowed\n')))

    def test_probe_workloads_have_no_runtime_identity_or_host_access(self):
        for name in ('client', 'server-a', 'server-b'):
            spec = pod('disposable-test', name, 'pinned-image')['spec']
            self.assertFalse(spec['automountServiceAccountToken'])
            self.assertNotIn('volumes', spec)
            self.assertNotIn('hostNetwork', spec)
            self.assertNotIn('hostPID', spec)
            container = spec['containers'][0]
            self.assertNotIn('env', container)
            self.assertTrue(container['securityContext']['readOnlyRootFilesystem'])
            self.assertFalse(container['securityContext']['allowPrivilegeEscalation'])


if __name__ == '__main__':
    unittest.main()
