// scripts/ci-repair-prompt.test.js — tests for the repair-prompt +
// failure-signature extraction (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { normLine, analyzeFailures, buildRepairPrompt, main } = require('./ci-repair-prompt');

const env = (over = {}) => ({
  MAX_BYTES: '12000', HEAD_REF: 'fix/issue-9-x', ATTEMPT: '1', MAX_REPAIRS: '3', ...over,
});

const FAILING = [{ kind: 'check', id: '11', name: 'ci', conclusion: 'failure', app: 'github-actions', url: 'https://x', output: { title: '', summary: '', text: '' } }];

const dirWith = (files) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'crp-'));
  for (const [n, d] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(dir, n)), { recursive: true });
    fs.writeFileSync(path.join(dir, n), typeof d === 'string' ? d : JSON.stringify(d));
  }
  return dir;
};

describe('normLine', () => {
  it('collapses hashes and numbers for a stable signature surface', () => {
    assert.equal(normLine('##[error]fail at abc1234 run 42'), 'fail at H run N');
    assert.equal(normLine(''), '');
  });
});

describe('analyzeFailures', () => {
  it('first error line becomes root cause + signature part', () => {
    const dir = dirWith({ 'logs/11.log': 'ok\n##[error] boom happened\nmore\n' });
    const r = analyzeFailures({ dir, failing: FAILING, cap: 12000 });
    assert.equal(r.rootCause, 'ci: boom happened');
    assert.match(r.signature, /^[0-9a-f]{16}$/);
    assert.equal(r.baseFaultChecks.length, 0);
  });

  it('scc:config-error in log or check output → baseFaultChecks', () => {
    const dir = dirWith({ 'logs/11.log': 'noise\nscc:config-error: bad yaml\n' });
    const r = analyzeFailures({ dir, failing: FAILING, cap: 12000 });
    assert.deepEqual(r.baseFaultChecks, ['ci']);
  });

  it('no log file → falls back to bounded check output', () => {
    const dir = dirWith({});
    const f = [{ ...FAILING[0], output: { title: '', summary: '', text: 'x'.repeat(100) + ' err' } }];
    const r = analyzeFailures({ dir, failing: f, cap: 10 });
    assert.equal(r.details[0].body.length, 10);
  });
});

describe('buildRepairPrompt', () => {
  it('fences sanitized log sections inside the prompt', () => {
    const esc = (s) => `[${s}]`;
    const prompt = buildRepairPrompt({
      pr: { number: 9, title: 't' }, pending: ['e2e'], esc,
      details: [{ check: FAILING[0], body: 'log-body' }],
      env: env(),
    });
    assert.match(prompt, /branch fix\/issue-9-x\).*attempt 1 of 3/);
    assert.match(prompt, /<untrusted-input source="pr-ci-logs">/);
    assert.match(prompt, /### check "\[ci\]" — failure/);
    assert.match(prompt, /Still running \(not yet concluded\): \[e2e\]/);
  });
});

describe('main', () => {
  it('writes failure.json + repair-prompt.md and the three outputs', () => {
    const dir = dirWith({
      'pr.json': { number: 9, title: 't' },
      'failing.json': FAILING,
      'pending.json': [],
      'logs/11.log': 'boom error here\n',
    });
    const gho = path.join(dir, 'out.txt');
    const r = main(['--dir', dir, '--sanitizer', path.join(dir, 'none.js')], { env: { ...env(), GITHUB_OUTPUT: gho } });
    const failure = JSON.parse(fs.readFileSync(path.join(dir, 'failure.json'), 'utf8'));
    assert.equal(failure.signature, r.signature);
    assert.equal(failure.baseFault, false);
    assert.match(fs.readFileSync(path.join(dir, 'repair-prompt.md'), 'utf8'), /boom error here/);
    assert.match(fs.readFileSync(gho, 'utf8'), /base_fault=false\nsanitize_verdict=clean/);
  });
});
