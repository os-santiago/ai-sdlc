#!/usr/bin/env python3
"""Prepare only a synthetic candidate for the pinned verifier conformance test."""
import argparse
from pathlib import Path
import subprocess

SOURCE = '0622cdaeb59a11a27bd5f3d7e18183c401a5e412'
UTILS = 'quarkus-app/src/main/resources/META-INF/resources/js/utils.js'


def prepare(source_repository, target):
    target = Path(target).resolve()
    target.mkdir(mode=0o700, parents=True, exist_ok=False)
    result = subprocess.run(['git', '-C', str(source_repository), 'show', SOURCE + ':' + UTILS],
                            capture_output=True, check=True, timeout=15)
    if not 1 <= len(result.stdout) <= 32768:
        raise ValueError('Unexpected pinned source size')
    path = target / UTILS
    path.parent.mkdir(parents=True)
    path.write_bytes(result.stdout)
    patch = Path(__file__).resolve().parent / 'fixtures/homedir-locale.patch'
    subprocess.run(['git', 'apply', '--check', str(patch)], cwd=target, check=True, timeout=15)
    subprocess.run(['git', 'apply', str(patch)], cwd=target, check=True, timeout=15)
    return target


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-repository', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    print(prepare(args.source_repository, args.output))
