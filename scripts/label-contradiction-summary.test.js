// scripts/label-contradiction-summary.test.js — summary-render tests for
// the sweep label-repair step (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { renderSummary, main } = require('./label-contradiction-summary');

describe('renderSummary', () => {
  it('renders a sweep record with repairs', () => {
    const lines = renderSummary({
      scanned: 12, contradictions: 2, repaired: 1,
      repairs: [
        { issue: 5, removed: ['ready-to-implement'], vetoes: ['needs-human'] },
        { issue: 9, removed: ['ready-to-implement'], vetoes: ['needs-refinement'], escalated: true },
      ],
    });
    assert.equal(lines[0], '### ai-sdlc label-contradiction repair');
    assert.match(lines[1], /issues scanned: 12 · contradictions: 2 · repaired: 1/);
    assert.match(lines[2], /- #5: removed `ready-to-implement` — vetoed by `needs-human`$/);
    assert.match(lines[3], /escalated needs-human \(label-drift-loop\)$/);
  });

  it('renders a single-issue record (no repairs array)', () => {
    const lines = renderSummary({
      contradictions: 1, issue: 3, removed: ['ready-to-implement'], vetoes: ['wontfix'], repaired: 1,
    });
    assert.match(lines[1], /issues scanned: 1/);
    assert.match(lines[2], /- #3: removed `ready-to-implement` — vetoed by `wontfix`$/);
  });

  it('renders an error field and a clean empty result', () => {
    const lines = renderSummary({ scanned: 4, contradictions: 0, repairs: [] });
    assert.equal(lines.length, 2);
    const err = renderSummary({ scanned: 1, contradictions: 1, repairs: [{ issue: 7, removed: [], vetoes: [], error: 'boom' }] });
    assert.match(err[2], /ERROR: boom$/);
  });
});

describe('main', () => {
  it('appends to $GITHUB_STEP_SUMMARY when set', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lcs-'));
    const repairs = path.join(dir, 'repairs.json');
    const summary = path.join(dir, 'summary.md');
    fs.writeFileSync(repairs, JSON.stringify({ scanned: 2, contradictions: 0 }));
    main(['--repairs', repairs], { env: { GITHUB_STEP_SUMMARY: summary } });
    assert.match(fs.readFileSync(summary, 'utf8'), /issues scanned: 2 · contradictions: 0 · repaired: 0/);
  });
});
