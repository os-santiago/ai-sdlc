"""Enable Tekton Chains to attach build signatures in a live installation.

Provenance verification requires the signature and attestation that Tekton
Chains attaches to each published image. Two cold-installed resources kept
Chains from doing so, and provenance (correctly) refused every build:

1. the managed registry boundary did not admit the Chains controller; and
2. Chains pushes with the credentials listed as imagePullSecrets of the build
   run's service account, and the publish service account listed its registry
   credential only for Tekton step credentials.

This closed transition applies exactly those two reviewed deltas, in that
order. The registry step is registry_boundary_upgrade. The service account
step adds imagePullSecrets [registry-auth] to the original publish service
account (same UID and custody nonce), with a resourceVersion precondition, and
moves the build foundation record to the reviewed blueprint.
"""
import copy
import hashlib
import json

import build_foundation_install as files
import build_foundation_state as foundation
import install
from konflux_runtime_install import write_record
import registry_boundary_upgrade

VERSION = 'ai-sdlc.chains-signing-upgrade/v1'
BLUEPRINT = install.ROOT / 'runtime/k8s/deploy/konflux/build-foundation.json'
KEY = 'v1/ServiceAccount/ai-sdlc-builds/publish'
CREDENTIAL = [{'name': 'registry-auth'}]


def original_publish():
    return {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': 'publish', 'namespace': 'ai-sdlc-builds'},
            'automountServiceAccountToken': False, 'secrets': copy.deepcopy(CREDENTIAL)}


def sha(value):
    return hashlib.sha256(foundation.canonical(value)).hexdigest()


def target_publish(blueprint):
    """The reviewed blueprint's publish account must be the original plus
    exactly imagePullSecrets [registry-auth]; nothing else in it may differ."""
    accounts = [i for i in blueprint.get('items', []) if i.get('kind') == 'ServiceAccount'
                and i.get('metadata', {}).get('name') == 'publish']
    expected = dict(original_publish(), imagePullSecrets=copy.deepcopy(CREDENTIAL))
    if len(accounts) != 1 or accounts[0] != expected:
        raise ValueError('Reviewed publish account is not the Chains credential delta')
    return expected


class PublishAccountStep:
    def __init__(self, state_dir, kubectl_call):
        self.path = state_dir / 'build-foundation.json'
        self.call = kubectl_call
        self.record = files.bounded_json(self.path)
        entry = self.record.get('resources', {}).get(KEY)
        if (self.record.get('version') != 'ai-sdlc.build-foundation/v1' or self.record.get('status') != 'prepared'
                or not isinstance(entry, dict) or not foundation.UID.fullmatch(str(entry.get('uid')))):
            raise ValueError('Original prepared build foundation required')
        self.blueprint = files.bounded_json(BLUEPRINT)
        self.target = target_publish(self.blueprint)
        self.original_sha, self.target_sha = sha(foundation.body(original_publish())), sha(foundation.body(self.target))

    def live(self):
        raw = self.call(['get', 'serviceaccount', 'publish', '-n', 'ai-sdlc-builds', '-o', 'json'])
        current = json.loads(raw)
        entry = self.record['resources'][KEY]
        meta = current.get('metadata', {})
        if (meta.get('uid') != entry['uid'] or meta.get('deletionTimestamp') or meta.get('ownerReferences')
                or meta.get('annotations', {}).get(foundation.ANNOTATION) != entry['nonce']):
            raise ValueError('Original publish account identity or custody changed')
        return current, sha(foundation.body(current))

    def phase(self):
        _, observed = self.live()
        entry = self.record['resources'][KEY]
        if observed == self.target_sha and entry['desiredSHA256'] == self.target_sha:
            return 'admitted'
        if observed == self.target_sha:
            return 'applied'
        if observed == self.original_sha and entry['desiredSHA256'] == self.original_sha:
            return 'original'
        raise ValueError('Publish account differs from both reviewed states')

    def save(self):
        write_record(self.path, self.record)

    def run(self):
        entry = self.record['resources'][KEY]
        journal = self.record.get('chainsSigning')
        expected = {'version': VERSION, 'from': self.original_sha, 'to': self.target_sha}
        if journal is not None and {k: journal.get(k) for k in expected} != expected:
            raise ValueError('Original publish account operation changed')
        current, observed = self.live()
        if observed == self.original_sha:
            if entry['desiredSHA256'] != self.original_sha:
                raise ValueError('Publish account record differs from the original')
            if journal is None:
                journal = dict(expected, completed=False)
                self.record['chainsSigning'] = journal
                self.save()
            patch = [{'op': 'test', 'path': '/metadata/resourceVersion', 'value': current['metadata']['resourceVersion']},
                     {'op': 'add', 'path': '/imagePullSecrets', 'value': copy.deepcopy(CREDENTIAL)}]
            self.call(['patch', 'serviceaccount', 'publish', '-n', 'ai-sdlc-builds', '--type=json', '-p', json.dumps(patch)])
            current, observed = self.live()
        elif observed != self.target_sha or journal is None:
            raise ValueError('Publish account already differs without an original operation')
        if observed != self.target_sha:
            raise ValueError('Publish account did not reach the reviewed state')
        if not journal.get('completed'):
            entry['desiredSHA256'] = self.target_sha
            self.record['manifestSHA256'] = hashlib.sha256(foundation.canonical(self.blueprint)).hexdigest()
            self.record['chainsSigning'] = dict(journal, completed=True)
            self.save()
        return self.phase()


class ChainsSigningUpgrade:
    def __init__(self, registry_installer, state_dir, kubectl_call):
        self.registry = registry_boundary_upgrade.RegistryBoundaryUpgrade(registry_installer)
        self.account = PublishAccountStep(state_dir, kubectl_call)

    def status(self):
        return {'version': VERSION, 'registry': self.registry.status(), 'publishAccount': self.account.phase(),
                'workflowEvidence': False}

    def run(self):
        self.registry.run()
        self.account.run()
        return self.status()
