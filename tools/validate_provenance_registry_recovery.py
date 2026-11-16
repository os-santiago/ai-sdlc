#!/usr/bin/env python3
"""Exercise native ACL installation across a restart with retained fixture data.

The directory survives a container restart, like a Pod's emptyDir. This does not
prove Pod replacement safety, Kubernetes projected-volume convergence or a live
installation. No Kubernetes API is used.
"""
import argparse
import http.client
import json
import os
from pathlib import Path
import signal
import time

import validate_provenance_registry as native


class Recovery(native.Run):
    def prepare_configuration(self):
        self.publisher = 'konflux'
        credentials = json.loads((self.private / 'credentials.json').read_text())
        credentials[self.publisher] = credentials.pop('ai-sdlc-publisher')
        (self.private / 'credentials.json').write_text(json.dumps(credentials))
        hashes = (self.private / 'htpasswd').read_text().replace('ai-sdlc-publisher:', 'konflux:')
        self.publisher_hash = next(line for line in hashes.splitlines() if line.startswith('konflux:'))
        config = json.loads((self.private / 'config.json').read_text())
        policies = config['http']['accessControl']['repositories'][native.REPO]['policies']
        policies[0]['users'] = [self.publisher]
        self.approved_config = json.dumps(config)
        self.approved_hashes = hashes
        del config['http']['accessControl']
        # Model independent Kubernetes projected volumes with atomic ..data
        # symlink replacement, not writes through an existing open inode.
        for name, contents in [('config', json.dumps(config)), ('auth', '\n'.join(
                line for line in hashes.splitlines() if not line.startswith('ai-sdlc-verifier:')) + '\n')]:
            directory = self.private / (name + '-volume')
            directory.mkdir(mode=0o755)
            self.project(directory, 'initial', contents)
            target = self.private / ('config.json' if name == 'config' else 'htpasswd')
            target.unlink()
            target.symlink_to(name + '-volume/..data/value')
        storage = self.private / 'storage'
        storage.mkdir(mode=0o700)
        if os.getuid() == 0:
            os.chown(storage, 10000, 10000)
        self.report['scope'] = 'disposable-native-registry-restart-fixture'

    @staticmethod
    def project(directory, generation, contents):
        version = directory / generation
        version.mkdir(mode=0o755)
        value = version / 'value'
        value.write_text(contents)
        value.chmod(0o640)
        if os.getuid() == 0:
            os.chown(value, 0, 10000)
        temporary = directory / '..next'
        temporary.symlink_to(generation)
        temporary.replace(directory / '..data')

    def storage_options(self):
        # A bounded fixture payload only; deletion is owned by Run.cleanup.
        storage = self.private / 'storage'
        storage.chmod(0o700)
        if os.getuid() == 0:
            os.chown(storage, 10000, 10000)
        return ['-v', str(storage) + ':/var/lib/zot:rw,z']

    def after_seed(self, request, image_path, endpoint):
        self.check('new-reader-absent-before-upgrade', request('GET', image_path, 'ai-sdlc-verifier')[0], 401)
        self.check('existing-publisher-read-before-upgrade', request('GET', image_path)[0], 200)
        self.project(self.private / 'config-volume', 'approved', self.approved_config)
        self.restart(request, image_path, endpoint)
        # Enforce ACL before introducing the new credential. Auth implementations
        # may watch htpasswd independently from their configuration loader.
        self.check('acl-loaded-before-new-reader', request('POST', '/v2/other/fixture/blobs/uploads/')[0], 403)
        self.check('publisher-delete-denied-before-new-reader', request('DELETE', image_path)[0], 403)
        self.check('new-reader-still-absent-after-acl', request('GET', image_path, 'ai-sdlc-verifier')[0], 401)
        self.project(self.private / 'auth-volume', 'approved', self.approved_hashes)
        self.restart(request, image_path, endpoint)
        actual = (self.private / 'htpasswd').read_text().splitlines()
        if self.publisher_hash not in actual:
            raise native.FixtureError('Existing publisher credential was changed')
        self.report['recovery'] = {'sameContainer': True, 'newProcessStart': True,
                                   'restartCount': 2, 'aclEnforcedBeforeCredential': True,
                                   'publisherCredentialPreserved': True,
                                   'projectedFilesReplaced': True,
                                   'podReplacementValidated': False,
                                   'kubernetesRestartValidated': False}
        self.check('existing-publisher-read-after-upgrade', request('GET', image_path)[0], 200)
        self.check('existing-publisher-upload-after-upgrade', request('POST', native.PREFIX + 'blobs/uploads/')[0], 202)

    def restart(self, request, image_path, endpoint):
        name = next(name for name in self.names if name.endswith('-registry'))
        before = json.loads(self.call(['podman', 'inspect', name]))[0]
        if before['Config']['Labels'].get(native.LABEL) != self.identity or not before['State']['Running']:
            raise native.FixtureError('Original running fixture identity required')
        # This is an owned administrative fixture restart, never an application
        # stage command. No assumption that this Zot build supports SIGHUP.
        self.call(['podman', 'stop', '--time', '10', before['Id']], 20)
        self.call(['podman', 'start', before['Id']], 20)
        after = json.loads(self.call(['podman', 'inspect', name]))[0]
        retained = (before['Id'] == after['Id'] and after['State']['Running']
                    and before['State']['StartedAt'] != after['State']['StartedAt'])
        if not retained or self.call(['podman', 'port', after['Id'], '5000/tcp']).strip() != endpoint:
            raise native.FixtureError('Restart replaced fixture identity or endpoint')
        deadline = time.monotonic() + 20
        while True:
            try:
                if request('GET', image_path)[0] == 200:
                    break
            except (OSError, http.client.HTTPException):
                pass
            if time.monotonic() >= deadline:
                raise native.FixtureError('Retained registry did not recover')
            time.sleep(.1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    output = args.output.absolute()
    if ':' in str(output) or ':' in str(native.ROOT):
        parser.error('Container paths cannot contain colons')
    def interrupted(signum, frame):
        raise native.FixtureError('Fixture interrupted')
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    passed = Recovery(output).execute()
    print(('PASS' if passed else 'FAIL') + ': ' + str(output / 'report.json'))
    return 0 if passed else 1


if __name__ == '__main__':
    raise SystemExit(main())
