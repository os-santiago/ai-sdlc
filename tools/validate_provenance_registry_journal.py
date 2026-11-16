#!/usr/bin/env python3
"""Exercise durable administrative restart recovery against real fixture objects.

Uses only the owned administrative test cluster, inheriting the native registry
proof, denial matrix and actual Pod replacement rejection. Lost replies are
injected after real Kubernetes/containerd effects; no workflow rows are used.
"""
import argparse
import hashlib
import json
from pathlib import Path
import signal
import time

from konflux_runtime_install import write_record
from provenance_registry_state import RegistryJournal, digest
import validate_provenance_registry as native
from validate_provenance_registry_kubernetes import KubernetesRecovery


class Interrupted(Exception):
    pass


class JournalRecovery(KubernetesRecovery):
    def __init__(self, output):
        super().__init__(output)
        self.phase = 0

    def prepare_configuration(self):
        super().prepare_configuration()
        self.report['scope'] = 'isolated-kubernetes-registry-journal-recovery-fixture'

    def restart(self, request, image_path, endpoint):
        self.guard()
        self.phase += 1
        original = self.read('secret', 'registry-fixture')
        desired = self.fixture_files()
        # This fixture projects config and auth from one Secret, so each
        # transition owns a separate journal. The live layout uses two objects.
        path = self.private / ('upgrade-'+str(self.phase)+'.json')
        inputs = self.private / ('upgrade-inputs-'+str(self.phase)+'.json')
        write_record(inputs, {'original': original, 'desired': desired})
        state = {'version': 'ai-sdlc.registry-upgrade-journal/v1', 'changes': {}, 'restarts': {}}
        journal = RegistryJournal(path, state, self.guard)
        writes = 0

        def replace(value):
            nonlocal writes
            writes += 1
            self.kube(['replace', '-f', '-'], value)
            raise Interrupted('lost actual Kubernetes update acknowledgement')

        try:
            journal.change_data(original, desired, lambda: self.read('secret', 'registry-fixture'), replace)
        except Interrupted:
            pass
        else:
            raise native.FixtureError('Expected actual lost update acknowledgement')
        # Discard both in-memory objects and reload durable inputs and intent.
        del journal, state, original, desired
        saved = json.loads(inputs.read_text())
        journal = RegistryJournal(path, json.loads(path.read_text()), self.guard)
        journal.change_data(saved['original'], saved['desired'], lambda: self.read('secret', 'registry-fixture'), replace)
        if writes != 1:
            raise native.FixtureError('Lost update reply repeated a Kubernetes write')
        wanted = [hashlib.sha256((self.private/name).read_bytes()).hexdigest() for name in ('config.json', 'htpasswd')]
        base = '/var/lib/kubelet/pods/'+self.pod_uid+'/volumes/kubernetes.io~secret/config/'

        def projected():
            raw = self.call(['podman', 'exec', self.node_id, 'sha256sum', base+'config.json', base+'htpasswd'])
            return [line.split()[0] for line in raw.splitlines()]

        deadline = time.monotonic() + 130
        while projected() != wanted:
            self.guard()
            if time.monotonic() >= deadline:
                raise native.FixtureError('Actual journal fixture projection did not converge')
            time.sleep(1)
        observed_original = self.guard()['status']['containerStatuses'][0]['containerID']
        actual_signals = 0
        injected = False

        def observe():
            pod = self.guard()
            status = pod['status'].get('containerStatuses', [{}])[0]
            cid = status.get('containerID', '')
            tasks = self.call(['podman', 'exec', self.node_id, 'ctr', '-n', 'k8s.io', 'tasks', 'list'])
            running = any(line.split()[0] == cid.removeprefix('containerd://') and line.split()[-1] == 'RUNNING'
                          for line in tasks.splitlines() if len(line.split()) >= 3)
            return {'podUID': pod['metadata']['uid'], 'containerID': cid,
                    'restartCount': status.get('restartCount', 0), 'volumeIdentity': self.volume_identity(),
                    'configurationSHA256': digest(projected()), 'ready': status.get('ready', False), 'running': running}

        def stop(cid):
            nonlocal injected, actual_signals
            if cid != observed_original:
                raise native.FixtureError('Journal tried to signal a replacement task')
            # Phase two interrupts after durable signal intent but before the
            # effect. Recovery must freshly observe and target the original ID.
            if self.phase == 2 and not injected:
                injected = True
                raise Interrupted('interruption before original task signal')
            actual_signals += 1
            self.call(['podman', 'exec', self.node_id, 'ctr', '-n', 'k8s.io', 'tasks', 'kill', '--signal', 'SIGTERM', cid.split('://')[1]])
            if self.phase == 1 and not injected:
                injected = True
                raise Interrupted('lost actual original task signal acknowledgement')

        deadline = time.monotonic() + 100
        while True:
            try:
                if journal.restart_original('registry', digest(wanted), observe, stop):
                    break
            except Interrupted:
                journal = RegistryJournal(path, json.loads(path.read_text()), self.guard)
            if time.monotonic() >= deadline:
                raise native.FixtureError('Journal did not reconcile the original registry restart')
            time.sleep(1)
        if actual_signals != 1 or not injected:
            raise native.FixtureError('Journal repeated the signal effect or missed interruption')
        final = json.loads(path.read_text())['restarts']['registry']
        self.report.setdefault('journalRecovery', []).append({
            'phase': self.phase, 'actualKubernetesWrites': writes, 'actualTaskSignals': actual_signals,
            'recordedSignalAttempts': final['attempts'], 'lostUpdateAcknowledgementRecovered': True,
            'lostSignalAcknowledgementRecovered': self.phase == 1,
            'interruptedBeforeSignalRecovered': self.phase == 2,
            'freshOriginalTaskObservationRequired': True})

    def after_verification(self, request, image_path, endpoint):
        self.negative_configuration_changes()
        # The parent's replacement negative calls restart() a third time. Its
        # initial guard must reject the new Pod before any journal/API mutation.
        super().after_verification(request, image_path, endpoint)
        if self.phase != 2:
            raise native.FixtureError('Replacement crossed the original custody fence')
        self.report['scope'] = 'isolated-kubernetes-registry-journal-recovery-fixture'

    def negative_configuration_changes(self):
        def create(name, data):
            return json.loads(self.kube(['create', '-f', '-', '-o', 'json'], {'apiVersion': 'v1',
                'kind': 'ConfigMap', 'metadata': {'name': name, 'namespace': self.namespace}, 'data': data}))
        before = create('journal-cas', {'value': 'original'})
        state = {'version': 'ai-sdlc.registry-upgrade-journal/v1', 'changes': {}, 'restarts': {}}
        journal = RegistryJournal(self.private/'cas-journal.json', state, self.guard)
        read = lambda: self.read('configmap', 'journal-cas')
        def conflict(value):
            outsider = read()
            outsider['data'] = {'value': 'foreign-writer'}
            self.kube(['replace', '-f', '-'], outsider)
            self.kube(['replace', '-f', '-'], value)  # Now-stale resourceVersion.
        try:
            journal.change_data(before, {'value': 'desired'}, read, conflict)
        except native.FixtureError:
            pass
        else:
            raise native.FixtureError('Actual stale Kubernetes update succeeded')
        observed = read()
        if observed['data'] != {'value': 'foreign-writer'} or observed['metadata']['resourceVersion'] == before['metadata']['resourceVersion']:
            raise native.FixtureError('Foreign Kubernetes update was not preserved')
        def forbidden(_):
            raise native.FixtureError('Conflict reconciliation attempted another write')
        try:
            journal.change_data(before, {'value': 'desired'}, read, forbidden)
        except ValueError:
            pass
        else:
            raise native.FixtureError('Conflict was adopted into the upgrade journal')

        create('fixture-freeze', {'frozen': 'true'})
        before = create('journal-freeze', {'value': 'original'})
        def fence():
            self.guard()
            if self.read('configmap', 'fixture-freeze')['data'] != {'frozen': 'true'}:
                raise ValueError('Fixture freeze was revoked')
        state = {'version': 'ai-sdlc.registry-upgrade-journal/v1', 'changes': {}, 'restarts': {}}
        journal = RegistryJournal(self.private/'freeze-journal.json', state, fence)
        save = journal.save
        def revoke(candidate):
            save(candidate)
            policy = self.read('configmap', 'fixture-freeze')
            policy['data'] = {'frozen': 'false'}
            self.kube(['replace', '-f', '-'], policy)
        journal.save = revoke
        try:
            journal.change_data(before, {'value': 'desired'}, lambda: self.read('configmap', 'journal-freeze'), forbidden)
        except ValueError:
            pass
        else:
            raise native.FixtureError('Revoked fixture freeze allowed a configuration write')
        observed = self.read('configmap', 'journal-freeze')
        if observed['data'] != before['data'] or observed['metadata']['resourceVersion'] != before['metadata']['resourceVersion']:
            raise native.FixtureError('Revoked fixture freeze changed the target')
        self.report['actualCASConflictPreservedForeignData'] = True
        self.report['actualFixtureFreezeRevocationPreventedWrite'] = True
        self.report['operationalFreezePolicyValidated'] = False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=Path)
    output = parser.parse_args().output.absolute()
    if ':' in str(output) or ':' in str(native.ROOT):
        parser.error('Container paths cannot contain colons')
    def interrupted(signum, frame):
        raise native.FixtureError('Fixture interrupted')
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    passed = JournalRecovery(output).execute()
    print(('PASS' if passed else 'FAIL') + ': ' + str(output/'report.json'))
    return 0 if passed else 1


if __name__ == '__main__':
    raise SystemExit(main())
