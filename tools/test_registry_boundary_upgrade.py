"""Fail-closed guards of the registry boundary admission of Tekton Chains."""
import copy
import os
import unittest
from unittest.mock import Mock, patch

import yaml

import managed_registry_install as registry
import registry_boundary_upgrade as boundary
import runtime_upgrade_pause

HELM = os.environ.get('AI_SDLC_TEST_HELM', 'helm')


def accepted_state(**changes):
    state = {'version': registry.VERSION, 'status': 'verified', 'replacementProof': {'status': 'passed'},
             'chartObjects': {'NetworkPolicy/registry-boundary': 'uid'}, 'storage': {'pvcUID': 'p'},
             'clusterUID': 'c', 'creationNonce': 'n', 'custody': {}}
    state.update(changes)
    return state


class FakeInstaller:
    def __init__(self, live, state=None):
        self.state = state or accepted_state()
        self.live = copy.deepcopy(live)
        self.saved, self.checks, self.helm = [], [], []
        self.binaries, self.kubectl = {'helm': 'helm'}, ['kubectl', '--kubeconfig', 'k']
        self.deadline = Mock()
        self.deadline.remaining.return_value = 600
        self.deadline.run.side_effect = self.apply
        self.physical = {'pvcUID': 'p'}

    def apply(self, argv, **kwargs):
        self.helm.append(argv)
        self.live = boundary.target_boundary(self.rendered()['spec'])

    def rendered(self):
        spec = boundary.original_boundary()
        spec['ingress'][0]['from'].append(copy.deepcopy(boundary.CHAINS_PEER))
        return {'kind': 'NetworkPolicy', 'metadata': {'name': 'registry-boundary'}, 'spec': spec}

    def chart_objects(self):
        return [self.rendered()]

    def chart_values(self):
        return {'clusterUID': 'c'}

    def fence(self, cold=True):
        assert cold is False

    def read(self, kind, name, namespace=None):
        return {'spec': copy.deepcopy(self.live)}

    def check_chart_objects(self, capture=False, network=None):
        self.checks.append(network)
        expected = network if network is not None else self.rendered()['spec']
        if registry.network_spec(self.live) != registry.network_spec(expected):
            raise ValueError('Registry network boundary changed')

    def storage(self):
        return self.physical

    def save(self, state):
        self.saved.append(copy.deepcopy(state))
        self.state = state


class Boundary(unittest.TestCase):
    def test_the_real_chart_renders_exactly_the_reviewed_admission(self):
        installer = registry.Installer.__new__(registry.Installer)
        installer.state = accepted_state(clusterUID='f962cf24-77b8-473b-94c2-c30a0aa2e6d5',
                                         creationNonce='a' * 48, custody={'version': 'x'})
        installer.binaries = {'helm': HELM}
        installer.deadline = Mock(run=lambda argv, **kw: __import__('subprocess').run(
            [str(a) for a in argv], input=kw.get('input'), capture_output=True, text=True, check=True))
        with patch.object(registry, 'digest', return_value='b' * 64):
            policies = [o for o in installer.chart_objects() if o['kind'] == 'NetworkPolicy']
        self.assertEqual(len(policies), 1)
        target = boundary.target_boundary(policies[0]['spec'])
        self.assertEqual(target['ingress'][0]['from'][-1], boundary.CHAINS_PEER)
        self.assertEqual(target['ingress'][0]['ports'], [{'protocol': 'TCP', 'port': 5000}])

    def test_any_other_boundary_change_is_refused(self):
        for change in (lambda s: s['ingress'][0]['ports'].append({'protocol': 'TCP', 'port': 5001}),
                       lambda s: s['ingress'][0]['from'][1]['podSelector']['matchLabels'].pop('app.kubernetes.io/name'),
                       lambda s: s['ingress'][0]['from'].append({'namespaceSelector': {}}),
                       lambda s: s.__setitem__('egress', [{}]),
                       lambda s: s['ingress'][0]['from'][0]['namespaceSelector']['matchExpressions'][0]['values'].append('x')):
            spec = FakeInstaller(boundary.original_boundary()).rendered()['spec']
            change(spec)
            with self.subTest(spec=spec), self.assertRaisesRegex(ValueError, 'reviewed Chains admission'):
                boundary.target_boundary(spec)


class Run(unittest.TestCase):
    def test_original_boundary_is_journaled_upgraded_and_verified(self):
        installer = FakeInstaller(boundary.original_boundary())
        result = boundary.RegistryBoundaryUpgrade(installer).run()
        self.assertEqual(result['phase'], 'admitted')
        self.assertEqual(len(installer.helm), 1)
        self.assertIn('upgrade', installer.helm[0])
        self.assertNotIn('--install', installer.helm[0])
        self.assertFalse(installer.saved[0]['boundaryUpgrade']['completed'])  # intent before the effect
        self.assertTrue(installer.state['boundaryUpgrade']['completed'])
        self.assertEqual(installer.state['chartSHA256'], registry.chart_manifest())
        self.assertEqual(installer.checks[0], boundary.original_boundary())  # everything else exact first

    def test_a_crash_after_helm_resumes_without_a_second_upgrade(self):
        installer = FakeInstaller(boundary.original_boundary())
        installer.save(dict(installer.state, boundaryUpgrade={
            'version': boundary.VERSION, 'from': registry.digest(registry.network_spec(boundary.original_boundary())),
            'to': registry.digest(registry.network_spec(boundary.target_boundary(installer.rendered()['spec']))),
            'storage': {'pvcUID': 'p'}, 'completed': False, 'targetChartSHA256': registry.chart_manifest()}))
        installer.live = boundary.target_boundary(installer.rendered()['spec'])
        boundary.RegistryBoundaryUpgrade(installer).run()
        self.assertEqual(installer.helm, [])
        self.assertTrue(installer.state['boundaryUpgrade']['completed'])

    def test_unknown_or_unjournaled_boundaries_and_storage_changes_are_refused(self):
        drifted = boundary.original_boundary()
        drifted['ingress'][0]['ports'][0]['port'] = 5001
        with self.assertRaisesRegex(ValueError, 'differs from the original'):
            boundary.RegistryBoundaryUpgrade(FakeInstaller(drifted)).run()
        admitted = FakeInstaller(boundary.original_boundary())
        admitted.live = boundary.target_boundary(admitted.rendered()['spec'])
        with self.assertRaisesRegex(ValueError, 'without an original operation'):
            boundary.RegistryBoundaryUpgrade(admitted).run()
        moved = FakeInstaller(boundary.original_boundary())
        moved.deadline.run.side_effect = lambda argv, **kw: (moved.apply(argv), setattr(moved, 'physical', {'pvcUID': 'q'}))
        with self.assertRaisesRegex(ValueError, 'storage changed'):
            boundary.RegistryBoundaryUpgrade(moved).run()

    def test_only_an_accepted_managed_registry_is_selectable(self):
        for change in ({'status': 'installed'}, {'replacementProof': {}}, {'version': 'x'}, {'chartObjects': None}, {'storage': None}):
            with self.subTest(change=change), self.assertRaisesRegex(ValueError, 'accepted managed registry'):
                boundary.RegistryBoundaryUpgrade(FakeInstaller(boundary.original_boundary(), accepted_state(**change)))

    def test_status_reports_both_reviewed_states_only(self):
        installer = FakeInstaller(boundary.original_boundary())
        self.assertEqual(boundary.RegistryBoundaryUpgrade(installer).status()['phase'], 'original')
        installer.live = boundary.target_boundary(installer.rendered()['spec'])
        self.assertEqual(boundary.RegistryBoundaryUpgrade(installer).status()['phase'], 'admitted')
        installer.live = {'podSelector': {}}
        with self.assertRaisesRegex(ValueError, 'both reviewed states'):
            boundary.RegistryBoundaryUpgrade(installer).status()


class Cli(unittest.TestCase):
    def test_chains_signing_takes_no_publication_or_approval_inputs(self):
        for extra in (['--provenance-trust', 'p'], ['--release-target', 'r'], ['--target-run', '1'],
                      ['--target-revision', 'a' * 40], ['--model-upstream', 'u']):
            for command in ('chains-signing', 'chains-signing-status'):
                with self.subTest(command=command, extra=extra), \
                        patch.object(runtime_upgrade_pause.platform, 'lock_state') as lock, self.assertRaises(ValueError):
                    runtime_upgrade_pause.main([command, '--cluster', 'c', *extra])
                lock.assert_not_called()


if __name__ == '__main__':
    unittest.main()
