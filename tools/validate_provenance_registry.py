#!/usr/bin/env python3
"""Validate pull-only provenance against a disposable, digest-pinned native Zot.

Requires Linux, Podman and Python's standard library. Never connects to Kubernetes
or installs trust. Synthetic history is emitted by the Go cryptographic fixture.
"""
import argparse
import base64
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import ssl
import subprocess
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
IMAGE = 'quay.io/konflux-ci/zot@sha256:92cb058816f30d14d3b64d85f8ddd6c4c681fbf9263af04085cd520c0bc7db42'
LABEL = 'ai-sdlc.io/validation-run'
REPO = 'ai-sdlc-fixtures/saferus'
PREFIX = '/v2/' + REPO + '/'


class FixtureError(Exception):
    pass


class Run:
    def __init__(self, output):
        self.output = output
        self.private = output / 'private'
        self.identity = uuid.uuid4().hex
        self.names = []
        self.report = {'version': 1, 'scope': 'disposable-native-registry-fixture',
                       'status': 'failed', 'registryImage': IMAGE, 'checks': [],
                       'liveRegistryModified': False, 'releaseAuthorized': False,
                       'operationalTrustInstalled': False, 'cleanup': []}
        self.deadline = time.monotonic() + 600

    def call(self, args, limit=60):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise FixtureError('Fixture deadline exceeded')
        deadline = time.monotonic() + min(limit, remaining)
        process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        captured = bytearray()
        try:
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                while selector.get_map():
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise FixtureError('Container operation exceeded deadline')
                    for key, _ in selector.select(min(remaining, .2)):
                        chunk = os.read(key.fileobj.fileno(), 65536)
                        if not chunk:
                            selector.unregister(key.fileobj)
                            continue
                        captured.extend(chunk)
                        if len(captured) > 4 * 1024 * 1024:
                            raise FixtureError('Container output exceeded limit')
            process.wait(timeout=max(.01, deadline - time.monotonic()))
            if process.returncode:
                # Never include output: fixture credentials may be present.
                raise FixtureError('Container or fixture operation failed')
            return captured.decode()
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)
            process.stdout.close()

    def go_test(self, name, address=None):
        container = 'ai-sdlc-zot-' + self.identity + '-' + str(len(self.names))
        self.names.append(container)
        cache = Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/validation/go'
        for child in ('mod', 'build'):
            (cache / child).mkdir(parents=True, exist_ok=True)
        lock = json.loads((ROOT / 'tools/runtime-validation.lock.json').read_text())
        self.report['goImage'] = lock['go']
        self.report['stage'] = name
        userns = ['--userns=keep-id', '--user', str(os.getuid()) + ':' + str(os.getgid())] if os.getuid() != 0 else []
        args = ['podman', 'run', '--rm', *userns, '--name', container, '--label', LABEL + '=' + self.identity,
                '--network=host', '-v', str(ROOT) + ':/src:ro,z',
                '-v', str(self.private) + ':/fixture:rw,z',
                '-v', str(cache / 'mod') + ':/go/pkg/mod:z',
                '-v', str(cache / 'build') + ':/go-cache:z',
                '-w', '/src/runtime/k8s', '-e', 'GOTOOLCHAIN=local', '-e', 'GOCACHE=/go-cache',
                '-e', 'AI_SDLC_NATIVE_ZOT_FIXTURE=/fixture']
        if address:
            args += ['-e', 'AI_SDLC_NATIVE_ZOT_ADDRESS=' + address]
        args += [lock['go'], 'go', 'test', '-buildvcs=false', '-json', '-count=1',
                 './internal/provenance', '-run', '^' + name + '$']
        events = self.call(args, 240)
        passed = False
        for line in events.splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if event.get('Test') == name and event.get('Action') == 'pass':
                passed = True
        if not passed:
            raise FixtureError('Required native Go fixture did not pass')
        self.report['checks'].append({'name': name, 'status': 'passed'})

    def check(self, name, actual, expected):
        self.report['checks'].append({'name': name, 'actual': actual, 'expected': expected})
        if actual != expected:
            raise FixtureError('Native registry check failed: ' + name)

    def cleanup(self):
        for name in reversed(self.names):
            entry = {'container': name, 'status': 'failed'}
            self.report['cleanup'].append(entry)
            try:
                inspected = subprocess.run(['podman', 'container', 'inspect', name],
                                           capture_output=True, timeout=10, check=False)
                if inspected.returncode == 0:
                    data = json.loads(inspected.stdout)[0]
                    if (data.get('Config', {}).get('Labels', {}).get(LABEL) != self.identity
                            or not re.fullmatch('[a-f0-9]{64}', data.get('Id', ''))):
                        entry['status'] = 'refused-unowned'
                        continue
                    subprocess.run(['podman', 'rm', '-f', '-v', data['Id']],
                                   capture_output=True, timeout=20, check=True)
                # A successful list proves absence; daemon failure does not.
                listed = subprocess.run(['podman', 'ps', '-a', '--format', '{{.Names}}'],
                                        capture_output=True, text=True, timeout=10, check=True)
                if name in listed.stdout.splitlines():
                    raise FixtureError('Owned container remains')
                entry['status'] = 'absent'
            except (OSError, ValueError, KeyError, IndexError, subprocess.SubprocessError, FixtureError):
                pass
        self.report['cleanupComplete'] = all(x['status'] == 'absent' for x in self.report['cleanup'])
        # This directory was exclusively created by this invocation. Retain only
        # public, sanitized evidence; destroy generated passwords and private key.
        try:
            shutil.rmtree(self.private)
        except OSError:
            self.report['cleanupComplete'] = False
            self.report['privateFixtureCleanupFailed'] = True

    def execute(self):
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.private.mkdir(mode=0o700)
        try:
            self.report['source'] = {
                'commit': self.call(['git', '-C', str(ROOT), 'rev-parse', 'HEAD']).strip(),
                'dirty': bool(self.call(['git', '-C', str(ROOT), 'status', '--porcelain']))}
            self.go_test('TestEmitNativeZotProvenanceFixture')
            credentials = json.loads((self.private / 'credentials.json').read_text())
            routes = {k: base64.b64decode(v, validate=True)
                      for k, v in json.loads((self.private / 'routes.json').read_text()).items()}
            uid, gid = (10000, 10000) if os.getuid() == 0 else (os.getuid(), os.getgid())
            self.private.chmod(0o750)
            if os.getuid() == 0:
                os.chown(self.private, 0, gid)
            for path in self.private.iterdir():
                path.chmod(0o640)
                if os.getuid() == 0:
                    os.chown(path, 0, gid)
            container = 'ai-sdlc-zot-' + self.identity + '-registry'
            self.names.append(container)
            userns = ['--userns=keep-id'] if os.getuid() != 0 else []
            self.report['stage'] = 'native-registry-acl'
            self.call(['podman', 'run', '-d', '--name', container, '--label', LABEL + '=' + self.identity, *userns,
                       '--user', str(uid) + ':' + str(gid), '--read-only', '--cap-drop=ALL',
                       '--security-opt=no-new-privileges', '--tmpfs', '/var/lib/zot:rw,size=128m,mode=1777',
                       '-p', '127.0.0.1::5000', '-v', str(self.private) + ':/cfg:ro,z', IMAGE, '/cfg/config.json'])
            endpoint = self.call(['podman', 'port', container, '5000/tcp']).strip()
            if not re.fullmatch(r'127\.0\.0\.1:[0-9]+', endpoint):
                raise FixtureError('Unexpected registry binding')
            port = int(endpoint.rsplit(':', 1)[1])
            context = ssl.create_default_context(cafile=str(self.private / 'ca.crt'))
            context.minimum_version = ssl.TLSVersion.TLSv1_3

            def request(method, path, user='ai-sdlc-publisher', body=b'', media='application/octet-stream'):
                connection = http.client.HTTPSConnection('127.0.0.1', port, context=context, timeout=5)
                headers = {'Content-Type': media}
                if user:
                    headers['Authorization'] = 'Basic ' + base64.b64encode((user + ':' + credentials[user]).encode()).decode()
                try:
                    connection.request(method, path, body, headers)
                    response = connection.getresponse()
                    raw = response.read((1 << 20) + 1)
                    if len(raw) > 1 << 20:
                        raise FixtureError('Registry response exceeded limit')
                    return response.status, {k.lower(): v for k, v in response.getheaders()}, raw
                finally:
                    connection.close()

            ready = time.monotonic() + 20
            while True:
                try:
                    if request('GET', '/v2/')[0] == 200:
                        break
                except (OSError, http.client.HTTPException):
                    pass
                if time.monotonic() >= ready:
                    raise FixtureError('Registry did not become ready with verified TLS')
                time.sleep(.1)

            def upload(repo, raw, user):
                status, headers, _ = request('POST', '/v2/' + repo + '/blobs/uploads/', user)
                self.check('start-blob-upload', status, 202)
                location = headers.get('location', '')
                if not location.startswith('/v2/' + repo + '/blobs/uploads/') or '#' in location:
                    raise FixtureError('Unexpected upload location')
                location += ('&' if '?' in location else '?') + 'digest=sha256:' + hashlib.sha256(raw).hexdigest()
                self.check('store-blob', request('PUT', location, user, raw)[0], 201)

            # Native registry validates real referenced blobs before accepting
            # each image/signature/attestation manifest. No HTTP mock is used.
            for path, raw in routes.items():
                if path.startswith(PREFIX + 'blobs/'):
                    upload(REPO, raw, 'ai-sdlc-publisher')
            manifests = {p: raw for p, raw in routes.items() if p.startswith(PREFIX + 'manifests/')}
            for path, raw in manifests.items():
                self.check('store-manifest', request('PUT', path, body=raw, media=json.loads(raw)['mediaType'])[0], 201)
            image_path, image = next((p, raw) for p, raw in manifests.items() if '/manifests/sha256:' in p)
            image_manifest = json.loads(image)
            # Other repository genuinely exists, so denial cannot hide a 404.
            for descriptor in [image_manifest['config']] + image_manifest['layers']:
                upload('other/fixture', routes[PREFIX + 'blobs/' + descriptor['digest']], 'fixture-admin')
            self.check('seed-other-repository', request('PUT', '/v2/other/fixture/manifests/fixture',
                       'fixture-admin', image, image_manifest['mediaType'])[0], 201)
            verifier = 'ai-sdlc-verifier'
            self.check('verifier-read-before-denials', request('GET', image_path, verifier)[0], 200)
            self.check('verifier-cannot-upload', request('POST', PREFIX + 'blobs/uploads/', verifier)[0], 403)
            self.check('verifier-cannot-write-manifest', request('PUT', PREFIX + 'manifests/denied', verifier,
                       image, image_manifest['mediaType'])[0], 403)
            self.check('verifier-cannot-delete', request('DELETE', image_path, verifier)[0], 403)
            self.check('verifier-cannot-read-other-repository', request('GET', '/v2/other/fixture/manifests/fixture', verifier)[0], 403)
            self.check('publisher-cannot-write-other-repository', request('POST', '/v2/other/fixture/blobs/uploads/')[0], 403)
            self.check('publisher-cannot-delete', request('DELETE', image_path)[0], 403)
            self.check('anonymous-cannot-read', request('GET', image_path, None)[0], 401)
            self.check('verifier-read-after-denials', request('GET', image_path, verifier)[0], 200)
            self.go_test('TestActualNativeZotRetainedProvenance', endpoint)
            proof = json.loads((self.private / 'verified-proof.json').read_text())
            (self.output / 'verified-proof.json').write_text(json.dumps(proof, indent=2) + '\n')
            self.report.update(status='passed', tlsMinimum='TLS1.3', registryReads=5,
                               artifactDigest=image_path.rsplit('/', 1)[1])
        except (OSError, ValueError, StopIteration, subprocess.SubprocessError, FixtureError) as error:
            self.report['failureClass'] = type(error).__name__
            if isinstance(error, FixtureError):
                self.report['failureBoundary'] = str(error)
        finally:
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            self.cleanup()
            if not self.report['cleanupComplete']:
                self.report['status'] = 'failed'
            (self.output / 'report.json').write_text(json.dumps(self.report, indent=2) + '\n')
        return self.report['status'] == 'passed'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True, help='New private evidence directory')
    args = parser.parse_args()
    output = args.output.absolute()
    if ':' in str(output) or ':' in str(ROOT):
        parser.error('Container paths cannot contain colons')
    def interrupted(signum, frame):
        raise FixtureError('Fixture interrupted')
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    passed = Run(output).execute()
    print(('PASS' if passed else 'FAIL') + ': ' + str(output / 'report.json'))
    return 0 if passed else 1


if __name__ == '__main__':
    raise SystemExit(main())
