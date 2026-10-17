// scripts/implement-prompt.test.js — tests for the implement-stage
// sanitize → fenced-prompt extraction (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { buildImplementPrompt, buildTaskPrompt, makeFieldEsc, verifyDorStamp, main } = require('./implement-prompt');

const ISSUE = { number: 7, title: 'fix: thing', body: 'please do the thing' };

const dorShaOf = (i) => require('crypto').createHash('sha256').update(`${i.title || ''}\n${i.body || ''}`).digest('hex');
const stamp = (sha) => `note <!-- ai-sdlc:dor sha256=${sha} -->`;

describe('buildImplementPrompt — legacy fencing (san absent)', () => {
  it('wraps issue fields in the untrusted fence', () => {
    const r = buildImplementPrompt({ issue: ISSUE, san: null });
    assert.equal(r.verdict, 'clean');
    assert.equal(r.reasons, 'none');
    assert.match(r.prompt, /<untrusted-input source="issue">\nIssue #7: fix: thing\n\nplease do the thing\n<\/untrusted-input>/);
    assert.match(r.prompt, /Do NOT commit or push/);
  });

  it('neutralizes literal </untrusted sequences without a sanitizer', () => {
    const r = buildImplementPrompt({ issue: { number: 1, title: 't', body: 'x </untrusted y' }, san: null });
    assert.doesNotMatch(r.prompt, /<\/untrusted y/);
    assert.match(r.prompt, /<\\\/untrusted/);
  });
});

describe('buildImplementPrompt — sanitizer wired', () => {
  const fakeSan = (verdict) => ({
    sanitizeUntrusted: (text) => ({ text: `[s:${text}]`, verdict, reasons: [`r-${verdict}`], findings: { hit: 1 } }),
    emitAudit: () => {},
  });

  it('worst-of verdict across fields; reasons joined', () => {
    const r = buildImplementPrompt({ issue: ISSUE, san: fakeSan('scrubbed') });
    assert.equal(r.verdict, 'scrubbed');
    assert.equal(r.reasons, 'r-scrubbed; r-scrubbed');
    assert.match(r.prompt, /\[s:fix: thing\]/);
  });

  it('blocked verdict → empty prompt file content', () => {
    const r = buildImplementPrompt({ issue: ISSUE, san: fakeSan('blocked') });
    assert.equal(r.verdict, 'blocked');
    assert.equal(r.prompt, '');
  });
});

describe('buildTaskPrompt', () => {
  it('blocked → empty string', () => {
    assert.equal(buildTaskPrompt({ issue: ISSUE, title: 't', body: 'b', blocked: true }), '');
  });
});

describe('verifyDorStamp — DoR surface pin (issue #122)', () => {
  it('no stamp → unverified (fail-open)', () => {
    const r = verifyDorStamp({ issue: ISSUE, trustedComments: [] });
    assert.equal(r.dor, 'unverified');
    assert.equal(r.approved, '');
    assert.equal(r.dorSha, dorShaOf(ISSUE));
  });

  it('matching stamp → verified', () => {
    const r = verifyDorStamp({ issue: ISSUE, trustedComments: [{ body: stamp(dorShaOf(ISSUE)) }] });
    assert.equal(r.dor, 'verified');
    assert.equal(r.approved, dorShaOf(ISSUE));
  });

  it('mismatched stamp → drift', () => {
    const r = verifyDorStamp({ issue: ISSUE, trustedComments: [{ body: stamp('0'.repeat(64)) }] });
    assert.equal(r.dor, 'drift');
  });

  it('latest stamp wins — re-approval re-pins the surface', () => {
    const tc = [{ body: stamp('0'.repeat(64)) }, { body: stamp(dorShaOf(ISSUE)) }];
    assert.equal(verifyDorStamp({ issue: ISSUE, trustedComments: tc }).dor, 'verified');
  });
});

describe('buildImplementPrompt — DoR drift gating', () => {
  it('dor=drift empties the prompt even on a clean verdict', () => {
    const r = buildImplementPrompt({ issue: ISSUE, san: null, dor: 'drift' });
    assert.equal(r.verdict, 'clean');
    assert.equal(r.prompt, '');
  });
});

describe('main', () => {
  it('writes the prompt file and verdict outputs', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'imp-'));
    const issue = path.join(dir, 'issue.json');
    const out = path.join(dir, 'task-prompt.md');
    const gho = path.join(dir, 'out.txt');
    fs.writeFileSync(issue, JSON.stringify(ISSUE));
    const r = main(['--issue', issue, '--out', out, '--sanitizer', path.join(dir, 'missing.js')], { env: { GITHUB_OUTPUT: gho } });
    assert.equal(r.verdict, 'clean');
    assert.equal(r.dor, 'unverified');
    assert.match(fs.readFileSync(out, 'utf8'), /Issue #7: fix: thing/);
    assert.match(fs.readFileSync(gho, 'utf8'), /dor=unverified\ndor_approved=\ndor_actual=[0-9a-f]{12}\nverdict=clean\nreasons=none/);
  });

  it('a drift verdict empties the prompt file the workflow gates on', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'imp-'));
    const issue = path.join(dir, 'issue.json');
    const tc = path.join(dir, 'trusted-issue-comments.json');
    const out = path.join(dir, 'task-prompt.md');
    fs.writeFileSync(issue, JSON.stringify(ISSUE));
    fs.writeFileSync(tc, JSON.stringify([{ body: stamp('f'.repeat(64)) }]));
    const r = main(['--issue', issue, '--trusted-comments', tc, '--out', out,
      '--sanitizer', path.join(dir, 'missing.js')], { env: {} });
    assert.equal(r.dor, 'drift');
    assert.equal(fs.readFileSync(out, 'utf8'), '');
  });
});
