#!/usr/bin/env python3
"""Validate the production custody installer in an owned disposable container."""
import argparse
import hashlib
import json
import pathlib
import re
import secrets
import subprocess
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--provider', choices=('podman', 'docker'), default='podman')
    parser.add_argument('--binary', type=pathlib.Path, required=True)
    parser.add_argument('--output', type=pathlib.Path, required=True)
    args = parser.parse_args()
    if args.binary.is_symlink() or not args.binary.is_file() or not 0 < args.binary.stat().st_size < 128 << 20:
        raise ValueError('Explicit bounded production binary required')
    if args.output.exists():
        raise ValueError('Use a new evidence file')
    args.output.parent.mkdir(parents=True, exist_ok=True)
    owner = secrets.token_hex(16)
    name = 'ai-sdlc-custody-install-' + owner
    identity = hashlib.sha256(secrets.token_bytes(32)).hexdigest()
    root = '/var/local/ai-sdlc/build-slots/' + identity
    image = json.loads((ROOT / 'tools/runtime-validation.lock.json').read_text())['go']
    report = {'version': 1, 'status': 'failed', 'scope': 'isolated production installer',
              'autonomousE2E': False, 'workloadExecuted': False, 'network': 'none',
              'checks': [], 'cleanupComplete': False}
    deadline = time.monotonic() + 180
    cid = None

    def call(argv, **options):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Custody fixture deadline exceeded')
        try:
            return subprocess.run([args.provider, *argv], capture_output=True, text=True,
                                  timeout=min(30, remaining), **options)
        except subprocess.CalledProcessError as error:
            # This fixture carries no credentials. Preserve bounded diagnostics
            # for provider/kernel differences without losing cleanup evidence.
            report['operationError'] = (error.stderr or '')[-2048:]
            raise

    def execute(argv, user='0:0', **options):
        return call(['exec', '-i', '--user', user, cid, *argv], **options)

    def install(value=None, accepted=True, user='0:0'):
        value = value if value is not None else {'storageIdentitySHA256': identity, 'nodeBootID': boot}
        result = execute(['/installer', 'prepare-build-slot'], user=user,
                         input=json.dumps(value), check=False)
        if (result.returncode == 0) != accepted:
            raise ValueError('Production custody installer control failed')
        if accepted:
            proof = json.loads(result.stdout)
            if proof.get('storageIdentitySHA256') != identity or proof.get('phase') != 'installed':
                raise ValueError('Installation proof mismatch')

    try:
        setup_security = ['--security-opt', 'apparmor=unconfined'] if args.provider == 'docker' else []
        cid = call(['run', '-d', '--name', name, '--network=none', '--read-only',
                    '--label', 'ai-sdlc.install-fixture=' + owner, '--cap-add', 'SYS_ADMIN',
                    *setup_security,
                    '--tmpfs', root + ':rw,size=4294967296,mode=0755',
                    '-v', str(args.binary.resolve()) + ':/installer:ro,z',
                    image, 'sleep', '240'], check=True).stdout.strip()
        if not re.fullmatch('[a-f0-9]{64}', cid):
            cid = None
            raise ValueError('Original container identity unavailable')
        actual = json.loads(call(['inspect', cid], check=True).stdout)[0]
        if actual['Config']['Labels'].get('ai-sdlc.install-fixture') != owner:
            raise ValueError('Fixture ownership mismatch')
        boot = execute(['cat', '/proc/sys/kernel/random/boot_id'], check=True).stdout.strip()
        execute(['mount', '-o', 'remount,size=4294967296,nr_inodes=32768', root], check=True)
        install(user='1000:1000', accepted=False)
        install({'storageIdentitySHA256': '../escape', 'nodeBootID': boot}, accepted=False)
        install({'storageIdentitySHA256': identity, 'nodeBootID': '11111111-1111-1111-1111-111111111111'}, accepted=False)
        install({'storageIdentitySHA256': identity, 'nodeBootID': boot, 'repair': True}, accepted=False)
        report['checks'].append('nonroot-traversal-wrong-boot-unknown-option-denied')
        install()
        before = execute(['cat', root + '/custody.json'], check=True).stdout
        execute(['touch', root + '/workspace/preserved'], user='1000:1000', check=True)
        install()
        execute(['test', '-f', root + '/workspace/preserved'], check=True)
        if execute(['cat', root + '/custody.json'], check=True).stdout != before:
            raise ValueError('Reapply replaced custody')
        report['checks'].append('initialization-and-reapply-preserve-consumed-workspace')
        execute(['mount', '-o', 'remount,size=2147483648', root], check=True)
        install(accepted=False)
        execute(['mount', '-o', 'remount,size=4294967296', root], check=True)
        execute(['mount', '-o', 'remount,nr_inodes=65536', root], check=True)
        install(accepted=False)
        execute(['mount', '-o', 'remount,nr_inodes=32768', root], check=True)
        install()
        report['checks'].append('changed-storage-bounds-denied')
        execute(['chmod', '0755', root], check=True)
        install(accepted=False)
        execute(['chmod', '0555', root], check=True)
        execute(['mv', root + '/custody.json', root + '/original'], check=True)
        execute(['ln', '-s', 'original', root + '/custody.json'], check=True)
        install(accepted=False)
        execute(['test', '-f', root + '/workspace/preserved'], check=True)
        report['checks'].append('partial-state-and-symlink-denied-without-erasing-data')
        report['status'] = 'passed'
    finally:
        try:
            # Cleanup has an independent budget. Recover lost allocation ACK only
            # by our unguessable name, nonce label and original container ID.
            if not cid:
                found = subprocess.run([args.provider, 'inspect', name], capture_output=True,
                                       text=True, timeout=10, check=False)
                if found.returncode == 0:
                    candidate = json.loads(found.stdout)[0]
                    if candidate.get('Config', {}).get('Labels', {}).get('ai-sdlc.install-fixture') == owner:
                        cid = candidate.get('Id')
            if cid and re.fullmatch('[a-f0-9]{64}', cid):
                actual = json.loads(subprocess.run([args.provider, 'inspect', cid], capture_output=True,
                                    text=True, check=True, timeout=10).stdout)[0]
                if actual['Id'] != cid or actual['Config']['Labels'].get('ai-sdlc.install-fixture') != owner:
                    raise ValueError('Refusing cleanup of replaced fixture')
                subprocess.run([args.provider, 'rm', '-f', '-v', cid], capture_output=True, check=True, timeout=20)
                report['cleanupComplete'] = True
        except Exception as error:
            report['status'] = 'failed'
            report['cleanupError'] = type(error).__name__
            raise
        finally:
            args.output.write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
