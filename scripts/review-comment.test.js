// scripts/review-comment.test.js — renderer tests for the review verdict
// comment (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { renderVerdictComment, main } = require('./review-comment');

const rec = (over = {}) => ({
  head_sha: 'e'.repeat(40), verdict: 'blocked', risk_tier: 'high', risk_why: 'w',
  findings: [{ severity: 'blocker', path: 'a.js', line: 3, summary: 'bug | here' }],
  external: { reviewer: 'coderabbit', blocking: true, blocking_checks: ['CodeRabbit'], changes_requested_by: ['coderabbitai[bot]'], inline_comments: 2 },
  adversarial: { ran: true, verdict: 'blocked', attacks: 4, probes_resisted: 3 },
  engine: 'scc', model: 'm', engine_exit: '0', run: 'https://x', summary: 'sum', ...over,
});

describe('renderVerdictComment', () => {
  it('stamps the canonical marker and renders all sections', () => {
    const c = renderVerdictComment({ record: rec(), env: {} });
    assert.match(c, /<!-- ai-sdlc:review sha=e+ verdict=blocked -->/);
    assert.match(c, /`coderabbit` — blocking: true \(CodeRabbit\), CHANGES_REQUESTED by coderabbitai\[bot\], inline comments: 2/);
    assert.match(c, /adversarial pass: `blocked` — 4 probes embedded, resisted: 3/);
    assert.match(c, /\| blocker \| `a\.js:3` \| bug \\| here \|/);
    assert.match(c, /```json\n\{\n  "verdict": "blocked"/);
    assert.match(c, /bounded fix pass is dispatched/);
  });

  it('clean verdict drops the fix-pass notice; no ext → AI-only line', () => {
    const c = renderVerdictComment({ record: rec({ verdict: 'clean', findings: [], external: {}, adversarial: null }), env: {} });
    assert.doesNotMatch(c, /bounded fix pass/);
    assert.match(c, /external reviewer: not configured — AI-only path/);
    assert.match(c, /adversarial pass: not run/);
  });

  it('COUNTS env overrides the findings line', () => {
    const c = renderVerdictComment({ record: rec(), env: { COUNTS: 'blocker: 1 · major: 0 · minor: 0' } });
    assert.match(c, /- findings: blocker: 1 · major: 0 · minor: 0/);
  });
});

describe('main', () => {
  it('writes verdict-comment.md from verdict.json', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rc-'));
    fs.writeFileSync(path.join(dir, 'verdict.json'), JSON.stringify(rec()));
    const body = main(['--dir', dir], { env: {} });
    assert.equal(fs.readFileSync(path.join(dir, 'verdict-comment.md'), 'utf8'), body);
  });
});
