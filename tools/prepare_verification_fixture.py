#!/usr/bin/env python3
"""Prepare only a synthetic candidate for the pinned verifier conformance test."""
import argparse
import os
import tempfile
from pathlib import Path
import subprocess

SOURCE = '0622cdaeb59a11a27bd5f3d7e18183c401a5e412'
UTILS = 'quarkus-app/src/main/resources/META-INF/resources/js/utils.js'


def prepare(source_repository, target):
    env = {key: value for key, value in os.environ.items() if not key.startswith('GIT_')}
    target = Path(target).resolve()
    target.mkdir(mode=0o700, parents=True, exist_ok=False)
    result = subprocess.run(['git', '-C', str(source_repository), 'show', SOURCE + ':' + UTILS],
                            capture_output=True, check=True, timeout=15, env=env)
    if not 1 <= len(result.stdout) <= 32768:
        raise ValueError('Unexpected pinned source size')
    path = target / UTILS
    path.parent.mkdir(parents=True)
    path.write_bytes(result.stdout)
    patch = Path(__file__).resolve().parent / 'fixtures/homedir-locale.patch'
    # Apply outside any parent repository, with ambient Git overrides removed.
    with tempfile.TemporaryDirectory(prefix='ai-sdlc-verifier-patch-') as directory:
        isolated = Path(directory)
        original = isolated / UTILS
        original.parent.mkdir(parents=True)
        original.write_bytes(result.stdout)
        env['GIT_CEILING_DIRECTORIES'] = str(isolated.parent)
        for arguments in (['--check'], []):
            subprocess.run(['git', 'apply', *arguments, str(patch)], cwd=isolated,
                           env=env, check=True, timeout=15)
        for name in (UTILS, 'tests/js/utils-locale-fallback.test.js'):
            generated = isolated / name
            if not generated.is_file() or not 1 <= generated.stat().st_size <= 32768:
                raise ValueError('Missing or oversized prepared candidate file')
            destination = target / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(generated.read_bytes())
    return target


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-repository', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    print(prepare(args.source_repository, args.output))
