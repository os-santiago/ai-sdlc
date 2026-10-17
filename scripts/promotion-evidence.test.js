// scripts/promotion-evidence.test.js — unit + mocked-history integration
// tests for the trust-ladder promotion gate (issue #201).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  evaluatePromotionEvidence,
  normalizeRecord,
  rollupPassed,
  fetchMergedPrs,
  main,
} = require('./promotion-evidence');

// newest-first merge record shorthand
const rec = (number, { labels = [], checksPassed = true, escalated } = {}) => ({
  number,
  mergedAt: `2026-01-${String(Math.min(number, 28)).padStart(2, '0')}T00:00:00Z`,
  labels,
  ...(checksPassed !== undefined ? { checksPassed } : {}),
  ...(escalated !== undefined ? { escalated } : {}),
});

const clean = (n, start = 1) => Array.from({ length: n }, (_, i) => rec(start + i));

// --- AC: empty history → false ------------------------------------------------
describe('evaluatePromotionEvidence — empty history', () => {
  it('returns false on an empty merge list', () => {
    const r = evaluatePromotionEvidence([]);
    assert.equal(r.satisfied, false);
    assert.equal(r.examined, 0);
    assert.equal(r.consecutiveClean, 0);
    assert.ok(r.reasons.some((x) => /empty window/.test(x)));
  });

  it('returns false on a non-array input', () => {
    assert.equal(evaluatePromotionEvidence(null).satisfied, false);
    assert.equal(evaluatePromotionEvidence(undefined).satisfied, false);
  });
});

// --- AC: N = 0 ---------------------------------------------------------------
describe('evaluatePromotionEvidence — threshold 0', () => {
  it('threshold 0 reduces the gate to zero escalations in a non-empty window', () => {
    const r = evaluatePromotionEvidence(clean(2), { threshold: 0 });
    assert.equal(r.satisfied, true);
  });

  it('threshold 0 still fails on an escalation in the window', () => {
    const r = evaluatePromotionEvidence(
      [rec(1), rec(2, { labels: ['needs-human'] })],
      { threshold: 0 },
    );
    assert.equal(r.satisfied, false);
  });

  it('threshold 0 with empty history is still false — no evidence, no promotion', () => {
    assert.equal(evaluatePromotionEvidence([], { threshold: 0 }).satisfied, false);
  });
});

// --- AC: last N merges clean → true -------------------------------------------
describe('evaluatePromotionEvidence — clean streak', () => {
  it('returns true when the last N merges are clean', () => {
    const r = evaluatePromotionEvidence(clean(5), { threshold: 3, window: 30 });
    assert.equal(r.satisfied, true);
    assert.equal(r.consecutiveClean, 5);
  });

  it('returns true when the streak exactly meets the threshold', () => {
    const r = evaluatePromotionEvidence(clean(3), { threshold: 3 });
    assert.equal(r.satisfied, true);
  });

  it('returns false when fewer records exist than the threshold', () => {
    const r = evaluatePromotionEvidence(clean(2), { threshold: 3 });
    assert.equal(r.satisfied, false);
    assert.equal(r.consecutiveClean, 2);
  });

  it('a merge with failing checks breaks the streak', () => {
    const r = evaluatePromotionEvidence(
      [...clean(2, 1), rec(3, { checksPassed: false }), ...clean(3, 4)],
      { threshold: 3 },
    );
    assert.equal(r.satisfied, false);
    assert.equal(r.consecutiveClean, 2);
    assert.equal(r.escalations.length, 0); // not an escalation — just not clean
  });

  it('a non-clean merge older than the streak does not fail the gate', () => {
    // [clean, clean, clean, FAIL, clean] — streak 3 meets threshold;
    // the failed merge inside the window is not an escalation label.
    const r = evaluatePromotionEvidence(
      [...clean(3, 1), rec(4, { checksPassed: false }), rec(5)],
      { threshold: 3 },
    );
    assert.equal(r.satisfied, true);
    assert.equal(r.consecutiveClean, 3);
  });
});

// --- AC: escalation label in window → false ------------------------------------
describe('evaluatePromotionEvidence — escalations', () => {
  it('returns false when any record in the window carries the escalation label', () => {
    const r = evaluatePromotionEvidence(
      [...clean(4, 1), rec(5, { labels: ['needs-human'] }), ...clean(3, 6)],
      { threshold: 3, window: 30 },
    );
    assert.equal(r.satisfied, false);
    assert.equal(r.escalations.length, 1);
    assert.equal(r.escalations[0].number, 5);
  });

  it('an escalation older than the window does not fail the gate', () => {
    const r = evaluatePromotionEvidence(
      [...clean(3, 1), rec(4, { labels: ['needs-human'] })],
      { threshold: 3, window: 3 },
    );
    assert.equal(r.satisfied, true);
    assert.equal(r.examined, 3);
  });

  it('honors a custom escalation-label binding', () => {
    const r = evaluatePromotionEvidence(
      [rec(1), rec(2, { labels: ['human-hold'] })],
      { threshold: 1, escalationLabels: ['human-hold'] },
    );
    assert.equal(r.satisfied, false);
    assert.equal(r.escalations[0].labels[0], 'human-hold');
  });

  it('a record flagged escalated via the ledger (no label) fails the gate', () => {
    const r = evaluatePromotionEvidence(
      [rec(1), rec(2, { escalated: true })],
      { threshold: 1 },
    );
    assert.equal(r.satisfied, false);
  });

  it('external escalation records scoped to the window fail the gate', () => {
    const r = evaluatePromotionEvidence(clean(5), {
      threshold: 3,
      escalations: [{ number: 77, labels: [{ name: 'needs-human' }] }],
    });
    assert.equal(r.satisfied, false);
    assert.equal(r.escalations.length, 1);
  });
});

// --- Mixed scenario ------------------------------------------------------------
describe('evaluatePromotionEvidence — mixed', () => {
  it('streak counts only the leading clean run, not total clean merges', () => {
    // clean, FAIL-checks, clean x5 → newest clean streak is 1
    const r = evaluatePromotionEvidence(
      [rec(1), rec(2, { checksPassed: false }), ...clean(5, 3)],
      { threshold: 3 },
    );
    assert.equal(r.consecutiveClean, 1);
    assert.equal(r.cleanMerges, 6);
    assert.equal(r.satisfied, false);
  });
});

// --- Normalization -------------------------------------------------------------
describe('normalizeRecord / rollupPassed', () => {
  it('labels normalize from strings or {name} objects', () => {
    const r = normalizeRecord({ labels: ['bug', { name: 'needs-human' }] });
    assert.deepEqual(r.labels, ['bug', 'needs-human']);
    assert.equal(r.escalated, true);
  });

  it('check runs with SUCCESS/NEUTRAL/SKIPPED pass', () => {
    assert.equal(rollupPassed([
      { status: 'COMPLETED', conclusion: 'SUCCESS' },
      { status: 'COMPLETED', conclusion: 'NEUTRAL' },
      { status: 'COMPLETED', conclusion: 'SKIPPED' },
    ]), true);
  });

  it('status contexts with state SUCCESS pass', () => {
    assert.equal(rollupPassed([{ state: 'SUCCESS' }, { state: 'NEUTRAL' }]), true);
  });

  it('any failing, pending or unknown check is not clean (fail-closed)', () => {
    assert.equal(rollupPassed([{ conclusion: 'FAILURE' }]), false);
    assert.equal(rollupPassed([{ status: 'IN_PROGRESS' }]), false);
    assert.equal(rollupPassed([{ state: 'PENDING' }]), false);
    assert.equal(rollupPassed([]), false);
    assert.equal(rollupPassed(undefined), false);
  });

  it('a record with no check information is not clean', () => {
    assert.equal(normalizeRecord({ number: 1, labels: [] }).checksPassed, false);
    assert.equal(
      evaluatePromotionEvidence([{ number: 1, labels: [] }], { threshold: 1 }).satisfied,
      false,
    );
  });
});

// --- Mocked-history integration ------------------------------------------------
describe('integration — mocked gh history', () => {
  const ghList = (prs) => (argv) => {
    assert.equal(argv[0], 'pr');
    assert.equal(argv[1], 'list');
    return JSON.stringify(prs);
  };

  const ghPr = (number, labels = [], conclusion = 'SUCCESS') => ({
    number,
    title: `pr ${number}`,
    mergedAt: '2026-01-01T00:00:00Z',
    labels: labels.map((name) => ({ name })),
    statusCheckRollup: [{ status: 'COMPLETED', conclusion }],
  });

  it('all-green mocked history satisfies the gate', () => {
    const exec = ghList([ghPr(30), ghPr(29), ghPr(28), ghPr(27)]);
    const r = main(['--repo', 'o/n', '--threshold', '3'], { exec, stdout: () => {} });
    assert.equal(r.satisfied, true);
    assert.equal(r.examined, 4);
    assert.equal(r.consecutiveClean, 4);
  });

  it('escalation label inside mocked history fails the gate', () => {
    const exec = ghList([ghPr(30), ghPr(29, ['needs-human']), ghPr(28), ghPr(27)]);
    const r = main(['--repo', 'o/n', '--threshold', '3'], { exec, stdout: () => {} });
    assert.equal(r.satisfied, false);
    assert.equal(r.escalations.length, 1);
    assert.equal(r.escalations[0].number, 29);
  });

  it('a failing check in mocked history breaks the streak', () => {
    const exec = ghList([ghPr(30), ghPr(29, [], 'FAILURE'), ghPr(28)]);
    const r = main(['--repo', 'o/n', '--threshold', '2'], { exec, stdout: () => {} });
    assert.equal(r.satisfied, false);
    assert.equal(r.consecutiveClean, 1);
  });

  it('--merges file replays a ledger history', () => {
    const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'prom-ev-')), 'merges.json');
    fs.writeFileSync(f, JSON.stringify([
      rec(1), rec(2), rec(3, { labels: ['needs-human'] }),
    ]));
    const r = main(['--merges', f, '--threshold', '1'], { stdout: () => {} });
    assert.equal(r.satisfied, false);
    assert.equal(r.examined, 3);
  });

  it('--merges file with clean history satisfies the gate', () => {
    const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'prom-ev-')), 'merges.json');
    fs.writeFileSync(f, JSON.stringify(clean(4)));
    const r = main(['--merges', f, '--threshold', '3'], { stdout: () => {} });
    assert.equal(r.satisfied, true);
  });

  it('fetchMergedPrs degrades to labels-only when statusCheckRollup fails', () => {
    let calls = 0;
    const exec = (argv) => {
      calls += 1;
      if (argv[argv.length - 1].includes('statusCheckRollup')) throw new Error('field unavailable');
      return JSON.stringify([{ number: 9, mergedAt: 'x', labels: [] }]);
    };
    const prs = fetchMergedPrs('o/n', 10, exec);
    assert.equal(calls, 2);
    assert.equal(prs[0].number, 9);
    // unknown check state → not clean → gate fails closed
    assert.equal(evaluatePromotionEvidence(prs, { threshold: 1 }).satisfied, false);
  });
});
