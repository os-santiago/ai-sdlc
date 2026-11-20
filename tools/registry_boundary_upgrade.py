"""Admit Tekton Chains to the managed registry boundary of a live installation.

The managed registry is otherwise cold-only. Its original network boundary
admitted the runtime, build and production namespaces but not the Tekton Chains
controller, so signatures and attestations could never be attached and
provenance verification refused every build (fail closed).

This closed transition changes exactly one thing: it adds the Chains
controller peer to the registry ingress, through a Helm upgrade of the same
release with the same values. Every other chart object, the recorded resource
UIDs, the storage binding and the private custody must stay exactly as
installed. Intent and outcome are journaled in the registry record.
"""
import copy
import json
import tempfile

import managed_registry_install as registry

VERSION = 'ai-sdlc.registry-boundary-upgrade/v1'
CHAINS_PEER = {'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'tekton-pipelines'}},
               'podSelector': {'matchLabels': {'app.kubernetes.io/component': 'controller',
                                               'app.kubernetes.io/instance': 'default',
                                               'app.kubernetes.io/name': 'controller',
                                               'app.kubernetes.io/part-of': 'tekton-chains'}}}
ORIGINAL_NAMESPACES = ['ai-sdlc-runtime', 'ai-sdlc-builds', 'ai-sdlc-production']


def original_boundary():
    """The boundary every managed registry installed before this change has."""
    return {'podSelector': {'matchLabels': {'run': 'registry'}}, 'policyTypes': ['Ingress', 'Egress'],
            'ingress': [{'from': [{'namespaceSelector': {'matchExpressions': [
                {'key': 'kubernetes.io/metadata.name', 'operator': 'In', 'values': list(ORIGINAL_NAMESPACES)}]}}],
                'ports': [{'protocol': 'TCP', 'port': 5000}]}], 'egress': []}


def target_boundary(rendered):
    """The rendered boundary must be the original plus exactly the Chains peer."""
    expected = original_boundary()
    expected['ingress'][0]['from'].append(copy.deepcopy(CHAINS_PEER))
    if registry.network_spec(rendered) != registry.network_spec(expected):
        raise ValueError('Rendered registry boundary is not the reviewed Chains admission')
    return expected


def live_boundary(installer):
    current = installer.read('networkpolicy', 'registry-boundary', registry.NAMESPACE)
    if current is None:
        raise ValueError('Original registry boundary is absent')
    return registry.network_spec(current.get('spec'))


class RegistryBoundaryUpgrade:
    def __init__(self, installer):
        self.installer = installer
        state = installer.state
        proof = state.get('replacementProof') or {}
        if (state.get('version') != registry.VERSION or state.get('status') != 'verified'
                or proof.get('status') != 'passed' or not isinstance(state.get('chartObjects'), dict)
                or not state.get('storage')):
            raise ValueError('Original accepted managed registry required')

    def rendered_boundary(self):
        policies = [o for o in self.installer.chart_objects() if o['kind'] == 'NetworkPolicy']
        if len(policies) != 1 or policies[0]['metadata']['name'] != 'registry-boundary':
            raise ValueError('Exactly one rendered registry boundary required')
        return target_boundary(policies[0]['spec'])

    def status(self):
        installer = self.installer
        installer.fence(cold=False)
        target = self.rendered_boundary()
        live = live_boundary(installer)
        if live == registry.network_spec(target):
            installer.check_chart_objects()
            phase = 'admitted'
        elif live == registry.network_spec(original_boundary()):
            installer.check_chart_objects(network=original_boundary())
            phase = 'original'
        else:
            raise ValueError('Registry boundary differs from both reviewed states')
        return {'version': VERSION, 'phase': phase, 'journal': copy.deepcopy(installer.state.get('boundaryUpgrade')),
                'storage': installer.storage(), 'workflowEvidence': False}

    def run(self):
        installer = self.installer
        installer.fence(cold=False)
        target = self.rendered_boundary()
        live = live_boundary(installer)
        journal = installer.state.get('boundaryUpgrade')
        expected_journal = {'version': VERSION, 'from': registry.digest(registry.network_spec(original_boundary())),
                            'to': registry.digest(registry.network_spec(target))}
        if journal is not None and {k: journal.get(k) for k in expected_journal} != expected_journal:
            raise ValueError('Original registry boundary operation changed')
        if live == registry.network_spec(target):
            # Completed earlier (or the Helm request succeeded before a crash):
            # only an operation this journal started may be recorded as done.
            if journal is None:
                raise ValueError('Registry boundary already differs without an original operation')
        else:
            if live != registry.network_spec(original_boundary()):
                raise ValueError('Registry boundary differs from the original; refusing')
            installer.check_chart_objects(network=original_boundary())
            physical = installer.storage()
            if journal is None:
                journal = dict(expected_journal, storage=physical, completed=False,
                               sourceChartSHA256=installer.state.get('chartSHA256'), targetChartSHA256=registry.chart_manifest())
                installer.save(dict(installer.state, boundaryUpgrade=journal))
            if journal.get('targetChartSHA256') != registry.chart_manifest():
                raise ValueError('Registry chart changed since the boundary operation began')
            if installer.deadline.remaining(330) < 330:
                raise ValueError('Insufficient registry Helm budget')
            installer.fence(cold=False)
            with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as output:
                json.dump(installer.chart_values(), output)
                output.flush()
                installer.deadline.run([installer.binaries['helm'], 'upgrade', registry.RELEASE, registry.CHART,
                                        '--kubeconfig', installer.kubectl[installer.kubectl.index('--kubeconfig') + 1],
                                        '--namespace', registry.RELEASE_NAMESPACE, '-f', output.name,
                                        '--server-side=false', '--wait=legacy', '--timeout', '5m'], limit=330)
        installer.fence(cold=False)
        installer.check_chart_objects()
        if installer.storage() != journal['storage']:
            raise ValueError('Original registry storage changed during the boundary upgrade')
        if journal.get('targetChartSHA256') != registry.chart_manifest():
            raise ValueError('Registry chart changed during the boundary upgrade')
        if not journal.get('completed'):
            # The installed chart is now the target chart: later installer
            # steps compare the record against the current chart files.
            installer.save(dict(installer.state, chartSHA256=journal['targetChartSHA256'],
                                boundaryUpgrade=dict(journal, completed=True)))
        return self.status()
