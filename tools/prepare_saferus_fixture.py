#!/usr/bin/env python3
"""Prepare a synthetic, disposable verifier input; never modify target Git state."""
import argparse
import os
from pathlib import Path
import subprocess

SOURCE = 'd5df3a8aa0a19b8f55dfc29c7bc13a10405a2aaa'
SOURCE_PATH = 'web/static/ttl.js'
TEST_PATH = 'tests/ttl.test.mjs'
BROWSER_PATH = 'tools/verify_browser.py'
FORMATTER = """export function formatTTL(seconds) {
  if (!Number.isSafeInteger(seconds) || seconds < 0) return 'Unavailable';
  if (seconds === 0) return '0 seconds';
  const parts = [];
  for (const [size, unit] of [[86400, 'day'], [3600, 'hour'], [60, 'minute'], [1, 'second']]) {
    const count = Math.floor(seconds / size);
    if (count) parts.push(`${count} ${unit}${count === 1 ? '' : 's'}`);
    seconds %= size;
  }
  return parts.join(' ');
}
"""
TEST = """import test from 'node:test';
import assert from 'node:assert/strict';
import {formatTTL} from '../web/static/ttl.js';
test('synthetic formatter fixture', () => {
  assert.equal(formatTTL(60), '1 minute');
  assert.equal(formatTTL(61), '1 minute 1 second');
  assert.equal(formatTTL(-1), 'Unavailable');
});
"""


def prepare(repository, output):
    env = {k: v for k, v in os.environ.items() if not k.startswith('GIT_')}
    def read(path):
        result = subprocess.run(['git', '--no-replace-objects', '-C', str(repository),
                                 'show', SOURCE + ':' + path], env=env, capture_output=True,
                                check=True, timeout=15)
        if not 0 < len(result.stdout) <= 32768:
            raise ValueError('Pinned source exceeds fixture bound')
        return result.stdout.decode()
    read(SOURCE_PATH); read(TEST_PATH)
    browser = read(BROWSER_PATH)
    old = "expect(page.locator('#ttl')).to_have_text('60 seconds')"
    if browser.count(old) != 1:
        raise ValueError('Pinned browser assertion changed')
    output = Path(output).resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    for name, data in ((SOURCE_PATH, FORMATTER), (TEST_PATH, TEST),
                       (BROWSER_PATH, browser.replace(old, old.replace('60 seconds', '1 minute'), 1))):
        path = output / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(data)
    return output


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-repository', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    print(prepare(args.source_repository, args.output))
