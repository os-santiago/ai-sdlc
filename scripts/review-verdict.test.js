// scripts/review-verdict.test.js — tests for the verdict normalization
// extraction (issue #127): word mapping, findings bound, adversarial merge,
// external blocking.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { normalizeFindings, verdictFor, normalizeVerdict, main } = require('./review-verdict');

const env = (over = {}) => ({
  FINDINGS_MAX: '20', PR: '5', HEAD_SHA: 'e'.repeat(40), ENGINE_CODE: '0',
  ENGINE_NAME: 'scc', MODEL: 'm', TIER: 'low', WHY: 'docs',
  RUN_URL: 'https://x/run', RUNNER_TEMP: os.tmpdir(), ...over,
});

const dirWith = (files) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rv-'));
  for (const [n, d] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(dir, n)), { recursive: true });
    fs.writeFileSync(path.join(dir, n), typeof d === 'string' ? d : JSON.stringify(d));
  }
  return dir;
};

describe('verdictFor', () => {
  it('maps raw verdict words', () => {
    assert.equal(verdictFor('request_changes', true), 'blocked');
    assert.equal(verdictFor('approve', true), 'clean');
    assert.equal(verdictFor('lgtm', true), 'clean');
    assert.equal(verdictFor('???', true), 'inconclusive');
    assert.equal(verdictFor('approve', false), 'inconclusive');
  });
});

describe('normalizeFindings', () => {
  it('normalizes severity, caps counts and bounds total bytes', () => {
    const f = normalizeFindings([
      { severity: 'BLOCKER', path: 'a', line: 'x', summary: 's' },
      { severity: 'weird', path: 'b', line: 4, summary: 't' },
    ], 20);
    assert.equal(f[0].severity, 'blocker');
    assert.equal(f[0].line, 0);
    assert.equal(f[1].severity, 'minor');
    const huge = Array.from({ length: 60 }, (_, i) => ({ severity: 'minor', path: 'p', line: 0, summary: 'x'.repeat(300) }));
    const bounded = normalizeFindings(huge, 60);
    assert.ok(JSON.stringify(bounded).length <= 8000);
  });
});

describe('normalizeVerdict', () => {
  it('non-minor finding blocks even on a clean ai verdict', () => {
    const dir = dirWith({
      'external.json': { blocking: false },
      'verdict.raw.json': { verdict: 'approve', findings: [{ severity: 'major', path: 'a.js', line: 1, summary: 'bug' }] },
    });
    const r = normalizeVerdict({ dir, env: env() });
    assert.equal(r.verdict, 'blocked');
    assert.equal(r.aiVerdict, 'clean');
  });

  it('external blocking forces blocked', () => {
    const dir = dirWith({
      'external.json': { blocking: true },
      'verdict.raw.json': { verdict: 'approve', findings: [] },
    });
    assert.equal(normalizeVerdict({ dir, env: env() }).verdict, 'blocked');
  });

  it('missing raw verdict → inconclusive recorded, ext can still block', () => {
    const dir = dirWith({ 'external.json': { blocking: false } });
    const r = normalizeVerdict({ dir, env: env() });
    assert.equal(r.verdict, 'inconclusive');
    assert.equal(r.parseOk, false);
  });

  it('adversarial plan recorded when the pass did not run', () => {
    const dir = dirWith({
      'external.json': { blocking: false },
      'verdict.raw.json': { verdict: 'approve', findings: [] },
      'adversarial-plan.json': { run: false, reason: 'tier-below-min', tier: 'low', attacks: [] },
    });
    const r = normalizeVerdict({ dir, env: env() });
    assert.equal(r.advOut, 'skip:none:0-attacks');
    assert.equal(r.record.adversarial.reason, 'tier-below-min');
  });
});

describe('main', () => {
  it('writes verdict.json + outputs', () => {
    const dir = dirWith({
      'external.json': { blocking: false },
      'verdict.raw.json': { verdict: 'approve', summary: 'looks fine', findings: [] },
    });
    const gho = path.join(dir, 'out.txt');
    const r = main(['--dir', dir], { env: { ...env(), GITHUB_OUTPUT: gho } });
    const rec = JSON.parse(fs.readFileSync(path.join(dir, 'verdict.json'), 'utf8'));
    assert.equal(rec.verdict, 'clean');
    assert.equal(rec.engine, 'scc');
    assert.match(fs.readFileSync(gho, 'utf8'), /verdict=clean\nfindings_json=\[\]/);
  });
});
