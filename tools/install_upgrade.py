"""Plan published runtime differences without changing an installation."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import zipfile

import install_images

MIGRATIONS = 'runtime/k8s/internal/store/migrations/'
NAME = re.compile(r'[0-9]{4}_[a-z0-9_]+\.sql')
MAX_FILES = 512
MAX_SQL_BYTES = 4 * 1024 * 1024


class Source:
    """Read immutable Git objects; never fetch, checkout, or read working files."""
    def __init__(self, root):
        self.root = Path(root)
        self.deadline = None

    def git(self, *args):
        if self.deadline is None:
            self.deadline = time.monotonic() + 60
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Source inspection deadline exceeded')
        # -C does not override inherited GIT_DIR, object/index locations or
        # injected Git configuration. Keep publication authentication in the
        # parent process; isolate only this immutable-object reader.
        env = {key: value for key, value in os.environ.items() if not key.startswith('GIT_')}
        env.update(GIT_NO_REPLACE_OBJECTS='1', GIT_GRAFT_FILE=os.devnull,
                   GIT_NO_LAZY_FETCH='1', GIT_TERMINAL_PROMPT='0')
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(['git', '--no-replace-objects', '-C', str(self.root), *args],
                                    stdout=output, stderr=subprocess.DEVNULL, env=env,
                                    timeout=min(10, remaining), check=False,
                                    preexec_fn=install_images.bound_output)
            if result.returncode or output.tell() >= install_images.MAX_RESPONSE:
                raise ValueError('Exact source object unavailable or oversized')
            output.seek(0)
            return output.read(install_images.MAX_RESPONSE + 1)

    def catalog(self, revision):
        if not isinstance(revision, str) or not install_images.SHA.fullmatch(revision):
            raise ValueError('Full immutable source revision required')
        if self.git('cat-file', '-t', revision) != b'commit\n':
            raise ValueError('Source revision must identify a commit')
        raw = self.git('ls-tree', '-rz', revision, '--', MIGRATIONS)
        entries = raw.split(b'\0')
        if entries[-1] != b'' or not 1 <= len(entries)-1 <= MAX_FILES:
            raise ValueError('Bounded nonempty migration inventory required')
        result = []
        total = 0
        for entry in entries[:-1]:
            header, path = entry.decode('ascii').split('\t', 1)
            mode, kind, blob = header.split(' ')
            name = path.removeprefix(MIGRATIONS)
            if (not path.startswith(MIGRATIONS) or not NAME.fullmatch(name)
                    or mode != '100644' or kind != 'blob' or not install_images.SHA.fullmatch(blob)):
                raise ValueError('Migration must be a canonical regular SQL blob')
            content = self.git('cat-file', 'blob', blob)
            total += len(content)
            if not content or len(content) > 256 * 1024 or total > MAX_SQL_BYTES:
                raise ValueError('Migration source exceeds bound')
            result.append({'filename': name, 'sha256': hashlib.sha256(content).hexdigest()})
        if [x['filename'] for x in result] != sorted({x['filename'] for x in result}):
            raise ValueError('Migration inventory must be unique and ordered')
        return result


def pending_migrations(source, target):
    if len(target) < len(source) or target[:len(source)] != source:
        raise ValueError('Target changes, removes or inserts before an existing migration')
    return target[len(source):]


def plan(source, api, source_run, source_revision, target_run, target_revision):
    for run, revision in ((source_run, source_revision), (target_run, target_revision)):
        if type(run) is not int or not 0 < run <= 2**63-1 or not isinstance(revision, str) or not install_images.SHA.fullmatch(revision):
            raise ValueError('Explicit publication run and full source revision required')
    # Resolve each identity from the authenticated API, not a caller-edited lock.
    # This chain authenticates publication metadata; it is not SLSA attestation.
    _, before = install_images.resolve(api, source_run, source_revision, include_build=True, include_release=True)
    _, after = install_images.resolve(api, target_run, target_revision, include_build=True, include_release=True)
    if source_revision != target_revision:
        source.git('merge-base', '--is-ancestor', source_revision, target_revision)
    old = source.catalog(source_revision)
    new = source.catalog(target_revision)
    pending = pending_migrations(old, new)
    return {
        'version': 'ai-sdlc.upgrade-plan/v1',
        'source': {'revision': source_revision, 'publication': before['runURL'], 'images': before['images'], 'migrations': old},
        'target': {'revision': target_revision, 'publication': after['runURL'], 'images': after['images'], 'migrations': new},
        'changedComponents': sorted(k for k in before['images'] if before['images'][k]['digest'] != after['images'][k]['digest']),
        'migrationPrefixPreserved': True,
        'pendingMigrations': pending,
        'unverifiedGates': ['installed-source-identity', 'database-migration-inventory', 'database-schema-integrity',
                            'configuration-and-consumer-compatibility', 'runtime-owned-drain-and-containment',
                            'backup-restoration', 'target-artifact-pull-and-trust', 'bounded-rollout-and-recovery'],
        'historicalSQLHashesVerified': False,
        'upgradeAuthorized': False,
        'rollbackAuthorized': False,
        'workflowEvidence': False,
        'scope': 'Published source differences only; pending means absent from the source catalog, not observed absent from a database.',
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-run', type=int, required=True)
    parser.add_argument('--source-revision', required=True)
    parser.add_argument('--target-run', type=int, required=True)
    parser.add_argument('--target-revision', required=True)
    args = parser.parse_args(argv)
    try:
        result = plan(Source(Path(__file__).resolve().parents[1]), install_images.GitHub(),
                      args.source_run, args.source_revision, args.target_run, args.target_revision)
    except (ValueError, OSError, subprocess.SubprocessError, zipfile.BadZipFile):
        parser.exit(1, 'Upgrade planning failed; no installation was changed.\n')
    print(json.dumps(result, indent=2))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
