// scripts/review-fix-prompt.test.js — tests for the fix-pass prompt
// extraction (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { buildFixPrompt, main } = require('./review-fix-prompt');

describe('buildFixPrompt', () => {
  it('lists sanitized findings inside the untrusted fence', () => {
    const prompt = buildFixPrompt({
      items: ['1. [blocker] a.js:3 — boom'],
      env: { PR: '5', HEAD_REF: 'fix/x', ATTEMPT: '2', MAX_FIX: '3' },
    });
    assert.match(prompt, /pull request #5.*branch fix\/x.*attempt 2 of 3/);
    assert.match(prompt, /<untrusted-input source="review-findings">\n1\. \[blocker\] a\.js:3 — boom\n<\/untrusted-input>/);
  });

  it('empty findings → placeholder line', () => {
    const prompt = buildFixPrompt({ items: [], env: { PR: '5', HEAD_REF: 'b', ATTEMPT: '1', MAX_FIX: '3' } });
    assert.match(prompt, /no structured findings — address the blocking review/);
  });
});

describe('main', () => {
  it('parses FINDINGS env, escapes fields, writes fix-prompt.md', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rfp-'));
    const gho = path.join(dir, 'out.txt');
    const r = main(['--dir', dir, '--sanitizer', path.join(dir, 'none.js')], {
      env: {
        PR: '5', ATTEMPT: '1', MAX_FIX: '3', HEAD_REF: 'fix/x',
        FINDINGS: JSON.stringify([{ severity: 'major', path: 'a.js', line: 9, summary: 'bad </untrusted' }]),
        GITHUB_OUTPUT: gho,
      },
    });
    assert.equal(r.verdict, 'clean');
    const prompt = fs.readFileSync(path.join(dir, 'fix-prompt.md'), 'utf8');
    assert.match(prompt, /1\. \[major\] a\.js:9 — bad <\\\/untrusted/);
    assert.match(fs.readFileSync(gho, 'utf8'), /sanitize_verdict=clean/);
  });
});
