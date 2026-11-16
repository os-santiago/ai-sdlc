#!/usr/bin/env python3
"""Exercise the custody guard in an owned disposable container, never live work."""
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
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary', type=pathlib.Path, required=True)
    p.add_argument('--prepare-test', type=pathlib.Path, required=True)
    p.add_argument('--output', type=pathlib.Path, required=True)
    args = p.parse_args()
    for f in (args.binary, args.prepare_test):
        if f.is_symlink() or not f.is_file() or not 0 < f.stat().st_size < 128 << 20:
            raise ValueError('Explicit bounded fixture binaries required')
    if args.output.exists():
        raise ValueError('Use a new report path')
    args.output.parent.mkdir(parents=True, exist_ok=True)
    owner = secrets.token_hex(16)
    container_name = 'ai-sdlc-guard-' + owner
    identity = hashlib.sha256(secrets.token_bytes(32)).hexdigest()
    image = json.loads((ROOT / 'tools/runtime-validation.lock.json').read_text())['go']
    report = {'version': 1, 'status': 'failed', 'scope': 'isolated local tmpfs guard',
              'autonomousE2E': False, 'network': 'none', 'checks': [], 'cleanupComplete': False}
    cid = None
    deadline = time.monotonic() + 180

    def call(argv, check=True):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Guard fixture deadline exceeded')
        return subprocess.run(['podman', *argv], capture_output=True, text=True,
                              timeout=min(30, remaining), check=check)

    def execute(argv, uid='0:0', check=True):
        return call(['exec', '--user', uid, cid, *argv], check=check)

    def guard(phase='build', override=None, extra=(), accepted=True):
        values = {'--storage-identity': identity, '--node-boot-id': boot,
                  '--workspace-phase': phase}
        values.update(override or {})
        result = execute(['/component', '--guard-workspace',
                          *[k + '=' + v for k, v in values.items()], *extra],
                         uid='1000:1000', check=False)
        if (result.returncode == 0) != accepted:
            raise ValueError('Guard positive/negative control failed')
        if accepted:
            proof = json.loads(result.stdout)
            if proof.get('storageIdentitySHA256') != identity or proof.get('phase') != phase:
                raise ValueError('Guard proof identity differs')
        return result

    try:
        cid = call(['run', '-d', '--name', container_name, '--network=none', '--read-only',
                    '--label', 'ai-sdlc.guard-fixture=' + owner,
                    '--cap-add', 'SYS_ADMIN',
                    '--tmpfs', '/slot:rw,size=4294967296,mode=0755',
                    '-v', str(args.binary.resolve()) + ':/component:ro,z',
                    '-v', str(args.prepare_test.resolve()) + ':/prepare.test:ro,z',
                    '--env', 'AI_SDLC_PREPARE_GUARD_FIXTURE=1',
                    '--env', 'AI_SDLC_GUARD_FIXTURE_ID=' + identity,
                    image, 'sleep', '240']).stdout.strip()
        if not re.fullmatch('[a-f0-9]{64}', cid):
            raise ValueError('Container identity unavailable')
        actual = json.loads(call(['inspect', cid]).stdout)[0]
        if actual['Config']['Labels'].get('ai-sdlc.guard-fixture') != owner:
            raise ValueError('Fixture ownership differs')
        execute(['mount', '-o', 'remount,size=4294967296,nr_inodes=32768', '/slot'])
        report['fixtureSetupCapability'] = 'SYS_ADMIN; guard effective capabilities verified zero'
        execute(['/prepare.test', '-test.run=^TestPrepareWorkspaceGuardFixture$'])
        boot = execute(['cat', '/proc/sys/kernel/random/boot_id']).stdout.strip()
        status = execute(['cat', '/proc/self/status'], uid='1000:1000').stdout
        if not re.search(r'^Uid:\s+1000\s+1000\s+1000\s+1000$', status, re.M) or not re.search(r'^CapEff:\s+0+$', status, re.M):
            raise ValueError('Guard was not tested as unprivileged UID 1000')
        guard(); report['checks'].append('bounded-empty-build-workspace')
        guard(override={'--storage-identity': '0' * 64}, accepted=False)
        guard(override={'--node-boot-id': '11111111-1111-1111-1111-111111111111'}, accepted=False)
        guard(extra=('--auth=/does-not-exist',), accepted=False)
        report['checks'].append('wrong-identity-boot-and-publisher-arguments-denied')
        for command in (['touch', '/slot/unauthorized'], ['rm', '/slot/custody.json']):
            if execute(command, uid='1000:1000', check=False).returncode == 0:
                raise ValueError('Writer altered root custody')
        guard(); report['checks'].append('writer-cannot-replace-custody')
        execute(['touch', '/slot/workspace/source'], uid='1000:1000')
        guard(accepted=False)
        guard(phase='publish', accepted=False)
        execute(['mount', '-o', 'remount,ro', '/slot'])
        guard(phase='publish')
        execute(['mount', '-o', 'remount,rw', '/slot'])
        report['checks'].append('nonempty-build-denied-readonly-publish-inspection-accepted')
        execute(['rm', '/slot/workspace/source'], uid='1000:1000')
        execute(['chmod', '0644', '/slot/custody.json']); guard(accepted=False)
        execute(['chmod', '0444', '/slot/custody.json']); guard()
        execute(['ln', '/slot/custody.json', '/slot/second-link']); guard(accepted=False)
        execute(['rm', '/slot/second-link']); guard()
        execute(['mv', '/slot/custody.json', '/slot/original'])
        execute(['ln', '-s', 'original', '/slot/custody.json']); guard(accepted=False)
        execute(['rm', '/slot/custody.json']); execute(['mv', '/slot/original', '/slot/custody.json']); guard()
        execute(['chmod', '0777', '/slot']); guard(accepted=False)
        execute(['chmod', '0555', '/slot']); guard()
        report['checks'].append('mode-hardlink-symlink-and-root-permission-controls')
        execute(['mount', '-o', 'remount,size=2147483648', '/slot']); guard(accepted=False)
        execute(['mount', '-o', 'remount,size=4294967296', '/slot']); guard()
        execute(['mount', '-o', 'remount,nr_inodes=65536', '/slot']); guard(accepted=False)
        execute(['mount', '-o', 'remount,nr_inodes=32768', '/slot']); guard()
        report['checks'].append('actual-capacity-and-inode-limits-enforced')
        report['status'] = 'passed'
    finally:
        if not cid:
            # Recover only the fixture's own allocation after a lost CLI reply.
            # This administrative fixture is not an autonomous build controller.
            found = subprocess.run(['podman', 'inspect', container_name],
                                   capture_output=True, text=True, timeout=10, check=False)
            if found.returncode == 0:
                candidate = json.loads(found.stdout)[0]
                if (candidate['Config']['Labels'].get('ai-sdlc.guard-fixture') == owner
                        and re.fullmatch('[a-f0-9]{64}', candidate.get('Id', ''))):
                    cid = candidate['Id']
        if cid:
            actual = json.loads(subprocess.run(['podman', 'inspect', cid], check=True,
                               capture_output=True, text=True, timeout=10).stdout)[0]
            if actual['Id'] != cid or actual['Config']['Labels'].get('ai-sdlc.guard-fixture') != owner:
                raise ValueError('Refusing cleanup of replaced fixture')
            subprocess.run(['podman', 'rm', '-f', '-v', cid], check=True,
                           capture_output=True, timeout=20)
            report['cleanupComplete'] = True
        args.output.write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
