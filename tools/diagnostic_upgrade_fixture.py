"""Real Helm phase/recovery proof using inert Pods in an owned test namespace."""
import json
import time

from diagnostic_runtime_upgrade import advance
from runtime_upgrade_pause import pause_once
from upgrade_fence_client import Fence


def exercise(*, chart, kube, helm, namespace, guard, command, provider, image,
             connection, port, catalog, sql, snapshot, report, save):
    before = sql(snapshot).stdout
    objects = json.loads(command(kube + ['get', 'deployments', '-n', namespace, '-o', 'json']).stdout)['items']
    identities = {obj['metadata']['name']: obj['metadata']['uid'] for obj in objects}
    if set(identities) != {'producer', 'intake'}:
        raise ValueError('Closed original phase fixture required')
    counts = {'source': (0, 0, True), 'staged': (0, 0, True), 'warming': (1, 0, True),
              'armed': (1, 0, False), 'active': (1, 1, False)}
    def observe(phase):
        guard()
        phase = 'source' if phase == 'migrated' else phase
        producer, intake, frozen = counts[phase]
        expected = {'producer': producer, 'intake': intake}
        deployments = json.loads(command(kube + ['get', 'deployments', '-n', namespace, '-o', 'json']).stdout)['items']
        if {d['metadata']['name']: d['metadata']['uid'] for d in deployments} != identities:
            raise ValueError('Original fixture controllers changed')
        for deployment in deployments:
            wanted = expected[deployment['metadata']['name']]
            if (deployment['spec']['replicas'] != wanted
                    or deployment['status'].get('observedGeneration', 0) < deployment['metadata']['generation']
                    or deployment['status'].get('readyReplicas', 0) != wanted):
                raise ValueError('Original fixture replicas have not converged')
        pods = json.loads(command(kube + ['get', 'pods', '-n', namespace, '-o', 'json']).stdout)['items']
        if len(pods) != producer + intake or any(p['metadata'].get('deletionTimestamp') for p in pods):
            raise ValueError('Old fixture Pods remain')
        policy = json.loads(command(kube + ['get', 'configmap', 'phase-policy', '-n', namespace, '-o', 'json']).stdout)
        if policy['data']['frozen'] != str(frozen).lower():
            raise ValueError('Fixture freeze differs from intended phase')
    sends = []
    def apply(phase, execute=None):
        guard()
        producer, intake, frozen = counts[phase]
        argv = helm + ['upgrade', 'pause-fixture', chart, '-n', namespace, '--set', 'replicas=' + str(producer),
                      '--set', 'intake=' + str(intake), '--set', 'frozen=' + str(frozen).lower(), '--wait', '--timeout', '30s']
        (execute or (lambda args, **kw: command(args, **kw)))(argv, timeout=35)
        sends.append(phase)
    record = {'completed': 'migrated', 'transition': None, 'expiresAt': time.time() + 180}
    def persist(value):
        report['transitionFixtureIntent'] = value.copy()
        save()
    advance(record, 'staged', observe, apply, persist)
    def lost(phase):
        apply(phase)
        raise OSError('Simulated acknowledgement loss after actual Helm convergence')
    try:
        advance(record, 'warming', observe, lost, persist)
    except OSError:
        pass
    else:
        raise ValueError('Expected acknowledgement-loss boundary was not exercised')
    if record['completed'] != 'staged' or record['transition']['to'] != 'warming':
        raise ValueError('Original incomplete transition intent was not retained')
    advance(record, 'warming', observe, apply, persist)
    if sends.count('warming') != 1:
        raise ValueError('Acknowledgement loss replayed Helm')
    advance(record, 'armed', observe, apply, persist)
    recovery = {'phase': 'planned', 'expiresAt': time.time() + 120}
    with Fence(provider, image, connection, port, catalog, expires_at=recovery['expiresAt']) as fence:
        pause_once(recovery, fence, lambda: observe('armed'), lambda: observe('source'),
                   lambda: apply('source', fence.command), lambda value: save())
    observe('source')
    record = {'completed': 'migrated', 'transition': None, 'expiresAt': time.time() + 180}
    for phase in ('staged', 'warming', 'armed', 'active'):
        advance(record, phase, observe, apply, persist)
    if sends[-1] != 'active' or sends.count('active') != 1 or sql(snapshot).stdout != before:
        raise ValueError('Fixture activation order or retained history changed')
    report.update(phaseSequenceVerified=True, lostHelmAcknowledgementNotReplayed=True,
                  originalRecoveryPauseVerified=True, recoveryFenceReleased=True,
                  originalControllerUIDsRetained=True, intakeOpenedLast=True,
                  phaseFixtureScope='inert Helm producers; not the installed runtime CLI')
    save()
