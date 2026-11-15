'use strict';
const assert = require('node:assert/strict');
const test = require('node:test');
const { rootMountReadOnly } = require('./offline_probe.js');

test('an EACCES write to a root-owned directory does not establish a read-only mount', () => {
  // UID 1000 cannot write /forbidden-probe on this rw mount either.
  const rw = '40 30 0:12 / / rw,relatime - overlay overlay rw\n';
  assert.equal(rootMountReadOnly(rw), false);
  assert.equal(rootMountReadOnly(rw.replace('rw,relatime', 'ro,relatime')), true);
});

test('read-only submounts and superblock options do not mask a writable root', () => {
  assert.equal(rootMountReadOnly('40 30 0:12 / / rw - overlay overlay ro\n41 40 0:13 / /etc ro - tmpfs tmpfs ro'), false);
  assert.equal(rootMountReadOnly('41 40 0:13 / /etc ro - tmpfs tmpfs ro'), false);
  assert.equal(rootMountReadOnly(''), false);
});
