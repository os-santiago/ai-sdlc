// scripts/review-prompt.test.js — tests for the review-prompt extraction
// (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { buildReviewPrompt, main } = require('./review-prompt');

const dirWith = (files) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rp-'));
  for (const [n, d] of Object.entries(files)) {
    fs.writeFileSync(path.join(dir, n), JSON.stringify(d));
  }
  return dir;
};

describe('buildReviewPrompt', () => {
  it('fences the diff + metadata and embeds the verdict contract', () => {
    const prompt = buildReviewPrompt({
      pr: { number: 5 }, meta: { files: ['a.js'], lines: 12 },
      body: 'stat\n+diff', title: 't', base: 'main', head: 'fix/x',
      env: { TIER: 'high', WHY: 'w', MODEL: 'm' },
    });
    assert.match(prompt, /bounded code review/);
    assert.match(prompt, /<untrusted-input source="pr-diff">\nPR #5: t\nBase → head: main → fix\/x/);
    assert.match(prompt, /pr:risk-high — w/);
    assert.match(prompt, /Files changed: 1 · diff lines: 12/);
    assert.match(prompt, /ai-sdlc-review-verdict\.json/);
    assert.match(prompt, /do NOT edit \.github\/workflows\//i);
  });
});

describe('main', () => {
  it('sanitizes, writes review-prompt.md, emits sanitize outputs', () => {
    const dir = dirWith({
      'pr.json': { number: 5, title: 't', baseRefName: 'main', headRefName: 'h' },
      'files.json': { files: ['a.js'], lines: 3, dirs: ['.'] },
    });
    fs.writeFileSync(path.join(dir, 'diff.patch'), '+x');
    fs.writeFileSync(path.join(dir, 'diff-stat.txt'), 'a.js | 1 +');
    const gho = path.join(dir, 'out.txt');
    const r = main(['--dir', dir, '--sanitizer', path.join(dir, 'none.js')], {
      env: { TIER: 'low', WHY: 'w', MODEL: 'm', GITHUB_OUTPUT: gho },
    });
    assert.equal(r.verdict, 'clean');
    assert.match(fs.readFileSync(path.join(dir, 'review-prompt.md'), 'utf8'), /PR #5: t/);
    assert.match(fs.readFileSync(gho, 'utf8'), /sanitize_verdict=clean\nsanitize_reasons=none/);
  });
});
