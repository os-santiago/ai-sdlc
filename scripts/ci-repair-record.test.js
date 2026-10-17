// scripts/ci-repair-record.test.js — tests for the ci-repair attempt
// record extraction (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { outcomeFor, buildComment, main } = require('./ci-repair-record');

const env = (over = {}) => ({
  PR: '9', ATTEMPT: '1', MAX_REPAIRS: '3', HEAD_SHA: 'c'.repeat(40),
  STALL_SIG: '', FAILING: 'ci', BASE_FAULT: 'false', ENGINE_CODE: '0',
  PUSH_OUTCOME: 'pushed', COMMIT: 'd'.repeat(40),
  RUN_URL: 'https://github.com/x/y/actions/runs/1', ...over,
});

describe('outcomeFor', () => {
  it('maps the outcome table', () => {
    assert.equal(outcomeFor(env({ BASE_FAULT: 'true' })).outcome, 'base-contract-error');
    assert.equal(outcomeFor(env()).outcome, 'pushed');
    assert.equal(outcomeFor(env({ PUSH_OUTCOME: '' })).outcome, 'commit-failed');
    assert.equal(outcomeFor(env({ ENGINE_CODE: '10' })).outcome, 'no-changes');
    assert.equal(outcomeFor(env({ ENGINE_CODE: '2' })).outcome, 'engine-exit-2');
    assert.equal(outcomeFor(env({ ENGINE_CODE: '' })).outcome, 'engine-exit-not-run');
  });
});

describe('buildComment', () => {
  it('stamps the attempt marker with optional sig', () => {
    const c = buildComment({ e: env({ STALL_SIG: 'abcd' }), outcome: 'pushed', baseFault: false, failure: {}, runbook: {}, stall: null });
    assert.match(c, /<!-- ai-sdlc:ci-repair attempt=1 sha=c+ sig=abcd -->/);
    assert.match(c, /outcome: `pushed` → d{40}/);
  });

  it('base-fault appends the escalation diagnosis', () => {
    const c = buildComment({ e: env({ BASE_FAULT: 'true' }), outcome: 'base-contract-error', baseFault: true, failure: {}, runbook: {}, stall: null });
    assert.match(c, /scc:config-error.*base.*ref/);
  });
});

describe('main', () => {
  it('writes manifest + comment and emits outcome', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'crr-'));
    fs.writeFileSync(path.join(dir, 'failure.json'), JSON.stringify({ signature: 's1', rootCause: 'ci: boom', checks: ['ci'], baseFault: false }));
    fs.writeFileSync(path.join(dir, 'scc-manifest.json'), JSON.stringify({ tool_calls_total: 4, iterations: 2 }));
    fs.writeFileSync(path.join(dir, 'stall.json'), JSON.stringify({ class: 'transport', route: 'retry', terminal: false, reason: 'r' }));
    fs.writeFileSync(path.join(dir, 'runbook.json'), JSON.stringify({ hit: true, entry: 'rb-1' }));
    const gho = path.join(dir, 'out.txt');
    const r = main(['--dir', dir], { env: { ...env(), GITHUB_OUTPUT: gho } });
    const m = JSON.parse(fs.readFileSync(path.join(dir, 'ci-repair-manifest.json'), 'utf8'));
    assert.equal(m.outcome, 'pushed');
    assert.equal(m.engineToolCalls, 4);
    assert.equal(m.engineStall.class, 'transport');
    assert.equal(m.failure.runbookHit, 'rb-1');
    assert.match(fs.readFileSync(path.join(dir, 'attempt-comment.md'), 'utf8'), /runbook: `rb-1`/);
    assert.match(fs.readFileSync(gho, 'utf8'), /outcome=pushed/);
  });

  it('tolerates absent input files', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'crr-'));
    const r = main(['--dir', dir], { env: env({ ENGINE_CODE: '10' }) });
    assert.equal(r.outcome, 'no-changes');
    assert.deepEqual(r.manifest.failingChecks, []);
  });
});
