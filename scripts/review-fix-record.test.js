// scripts/review-fix-record.test.js — tests for the review-fix attempt
// record extraction (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { outcomeFor, buildComment, main } = require('./review-fix-record');

const env = (over = {}) => ({
  PR: '5', ATTEMPT: '1', MAX_FIX: '3', HEAD_SHA: 'e'.repeat(40),
  ENGINE_CODE: '0', PUSH_OUTCOME: 'pushed', COMMIT: 'f'.repeat(40),
  SANITIZE: 'clean', RUN_URL: 'https://x/run', ...over,
});

describe('outcomeFor', () => {
  it('maps the outcome table', () => {
    assert.equal(outcomeFor(env({ SANITIZE: 'blocked' })).outcome, 'injection-quarantined');
    assert.equal(outcomeFor(env()).outcome, 'pushed');
    assert.equal(outcomeFor(env({ PUSH_OUTCOME: '' })).outcome, 'commit-failed');
    assert.equal(outcomeFor(env({ ENGINE_CODE: '10' })).outcome, 'no-changes');
    assert.equal(outcomeFor(env({ ENGINE_CODE: '7' })).outcome, 'engine-exit-7');
    assert.equal(outcomeFor(env({ ENGINE_CODE: '' })).outcome, 'engine-exit-not-run');
  });
});

describe('buildComment', () => {
  it('stamps the outcome marker; pushed adds the re-review note', () => {
    const c = buildComment({ e: env(), outcome: 'pushed' });
    assert.match(c, /<!-- ai-sdlc:review-fix-outcome attempt=1 sha=e+ -->/);
    assert.match(c, /re-triggers `pull_request: synchronize`/);
    const c2 = buildComment({ e: env(), outcome: 'no-changes' });
    assert.doesNotMatch(c2, /re-triggers/);
  });
});

describe('main', () => {
  it('writes manifest + comment and emits outcome', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rfr-'));
    const gho = path.join(dir, 'out.txt');
    const r = main(['--dir', dir], { env: { ...env(), GITHUB_OUTPUT: gho } });
    const m = JSON.parse(fs.readFileSync(path.join(dir, 'review-fix-manifest.json'), 'utf8'));
    assert.equal(m.outcome, 'pushed');
    assert.equal(m.pr, 5);
    assert.match(fs.readFileSync(path.join(dir, 'attempt-comment.md'), 'utf8'), /attempt 1\/3.*finished/);
    assert.match(fs.readFileSync(gho, 'utf8'), /outcome=pushed/);
  });
});
