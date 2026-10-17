// scripts/postmortem.test.js — node --test coverage for the canonical
// postmortem lifecycle (issue #191, spec/postmortem-lifecycle.md).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');

const pm = require('./postmortem');

const REPO = 'o/r';
const PARTS = { component: 'verify', class: 'post-merge-verify-failure', location: 'npm test' };
const HASH = pm.computeSignatureHash(PARTS);

function issue(number, hash, extra = {}) {
  return { number, title: `postmortem ${number}`, body: `body\n${pm.signatureMarker(hash)}`, url: `u/${number}`, labels: [], ...extra };
}

function recurrenceComment(hash, occ, createdAt) {
  return { body: `recurrence\n${pm.recurrenceMarker(hash, occ)}`, created_at: createdAt };
}

/** In-memory client exposing the same call surface the gh adapter uses. */
function fakeClient({ open = [], closed = [], comments = {} } = {}) {
  const calls = { comments: [], reopens: [], creates: [], ensured: [] };
  return {
    calls,
    listIssues({ state }) { return state === 'open' ? open : closed; },
    listComments({ issueNumber }) { return comments[issueNumber] || []; },
    addComment({ issueNumber, body }) { calls.comments.push({ issueNumber, body }); },
    reopenIssue({ issueNumber }) { calls.reopens.push(issueNumber); },
    ensureLabel({ label }) { calls.ensured.push(label); },
    createIssue({ title, body, labels }) {
      calls.creates.push({ title, body, labels });
      return { number: 900, url: 'u/900' };
    },
  };
}

const opts = (over = {}) => ({
  repo: REPO,
  signatureHash: HASH,
  signatureText: 'verify post-merge-verify-failure npm test',
  component: PARTS.component,
  class: PARTS.class,
  location: PARTS.location,
  title: 'verify: post-merge checks failed on deadbeef',
  body: 'Post-merge verify commands failed.',
  occurrenceId: 'verify-deadbeef',
  runUrl: 'https://example/run/1',
  reopenMin: 2,
  dedupWindowDays: 90,
  now: new Date('2026-10-14T00:00:00Z'),
  ...over,
});

describe('signature normalization (spec § Canonical Identity)', () => {
  it('is stable across volatile tokens (SHA, timestamp, casing, whitespace)', () => {
    const a = pm.computeSignatureHash({ component: 'Verify', class: 'post-merge-verify-failure', location: 'npm test' });
    const b = pm.computeSignatureHash({ component: '  verify  ', class: 'POST-MERGE-VERIFY-FAILURE', location: 'npm   test' });
    assert.strictEqual(a, b);
    // Volatile tokens at the tail collapse away entirely — the same failing
    // command on a different commit still hits the canonical signature.
    const c = pm.computeSignatureHash({
      component: 'verify', class: 'post-merge-verify-failure',
      location: 'npm test @ 5f8f9d0c9b1a2f3e4d5c6b7a8f9e0d1c2b3a4f5e 2026-10-14T18:30:00Z',
    });
    assert.strictEqual(a, c);
  });

  it('keeps genuinely different failures distinct', () => {
    assert.notStrictEqual(
      pm.computeSignatureHash({ ...PARTS, location: 'bash tools/validate-repo.sh' }),
      HASH);
    assert.notStrictEqual(
      pm.computeSignatureHash({ ...PARTS, class: 'engine-provider-error' }),
      HASH);
  });

  it('accepts a raw signature string and a 16-hex digest', () => {
    assert.strictEqual(pm.computeSignatureHash('x'), pm.computeSignatureHash('x'));
    assert.match(HASH, /^[0-9a-f]{16}$/);
  });
});

describe('canonical identity + markers', () => {
  it('extracts the hash from the marker, meta marker, and title bracket', () => {
    assert.strictEqual(pm.issueSignatureOf(issue(1, HASH)), HASH);
    const meta = { body: `x <!-- ai-sdlc-postmortem-meta: {"sigHash":"${HASH}"} -->` };
    assert.strictEqual(pm.issueSignatureOf(meta), HASH);
    assert.strictEqual(pm.issueSignatureOf({ title: `boom [${HASH}]`, body: '' }), HASH);
    assert.strictEqual(pm.issueSignatureOf({ title: 'plain', body: 'none' }), null);
  });

  it('finds the lowest-numbered matching issue as canonical', () => {
    const c = pm.findCanonical([issue(9, HASH), issue(3, HASH), issue(5, 'other')], HASH);
    assert.strictEqual(c.number, 3);
    assert.strictEqual(pm.findCanonical([issue(5, 'other')], HASH), null);
  });
});

describe('handleOccurrence — open canonical', () => {
  it('comments on a recurring signature, never reopens, never files', () => {
    const client = fakeClient({ open: [issue(7, HASH)] });
    const r = pm.handleOccurrence(opts(), client);
    assert.strictEqual(r.action, 'commented');
    assert.strictEqual(r.issueNumber, 7);
    assert.strictEqual(client.calls.comments.length, 1);
    assert.strictEqual(client.calls.reopens.length, 0);
    assert.strictEqual(client.calls.creates.length, 0);
    assert.ok(client.calls.comments[0].body.includes(`sig=${HASH}`));
    assert.ok(client.calls.comments[0].body.includes('occ=verify-deadbeef'));
  });

  it('skips an occurrence id already recorded (dedup of re-runs)', () => {
    const client = fakeClient({
      open: [issue(7, HASH)],
      comments: { 7: [recurrenceComment(HASH, 'verify-deadbeef', '2026-10-13T00:00:00Z')] },
    });
    const r = pm.handleOccurrence(opts(), client);
    assert.strictEqual(r.action, 'duplicate-occurrence');
    assert.strictEqual(client.calls.comments.length, 0);
  });
});

describe('handleOccurrence — closed canonical', () => {
  const closedAt = '2026-10-01T00:00:00Z';
  const closedIssue = () => issue(12, HASH, { closedAt });

  it('comments below the reopen threshold without reopening', () => {
    const client = fakeClient({ closed: [closedIssue()] });
    const r = pm.handleOccurrence(opts(), client);
    assert.strictEqual(r.action, 'commented');
    assert.strictEqual(r.postCloseCount, 1);
    assert.strictEqual(client.calls.comments.length, 1);
    assert.strictEqual(client.calls.reopens.length, 0);
    assert.strictEqual(client.calls.creates.length, 0);
  });

  it('reopens at the configured threshold — comment first, then reopen', () => {
    const client = fakeClient({
      closed: [closedIssue()],
      comments: { 12: [recurrenceComment(HASH, 'verify-aaa', '2026-10-05T00:00:00Z')] },
    });
    const r = pm.handleOccurrence(opts(), client);
    assert.strictEqual(r.action, 'reopened');
    assert.strictEqual(r.postCloseCount, 2);
    assert.deepStrictEqual(client.calls.reopens, [12]);
    assert.strictEqual(client.calls.comments.length, 1);
    assert.ok(client.calls.comments[0].body.includes('reopen threshold'));
    // No duplicate filing ever.
    assert.strictEqual(client.calls.creates.length, 0);
  });

  it('respects a custom reopen_min (e.g. 1 → first post-close hit reopens)', () => {
    const client = fakeClient({ closed: [closedIssue()] });
    const r = pm.handleOccurrence(opts({ reopenMin: 1 }), client);
    assert.strictEqual(r.action, 'reopened');
    assert.deepStrictEqual(client.calls.reopens, [12]);
  });

  it('ignores pre-close recurrence comments when counting post-close hits', () => {
    const client = fakeClient({
      closed: [closedIssue()],
      comments: { 12: [recurrenceComment(HASH, 'verify-old', '2026-09-01T00:00:00Z')] },
    });
    const r = pm.handleOccurrence(opts(), client);
    assert.strictEqual(r.action, 'commented');
    assert.strictEqual(r.postCloseCount, 1);
    assert.strictEqual(client.calls.reopens.length, 0);
  });

  it('files a new issue when the closed match is outside the dedup window', () => {
    const stale = issue(12, HASH, { closedAt: '2025-01-01T00:00:00Z' });
    const client = fakeClient({ closed: [stale] });
    const r = pm.handleOccurrence(opts(), client);
    assert.strictEqual(r.action, 'filed');
    assert.strictEqual(r.issueNumber, 900);
    assert.strictEqual(client.calls.reopens.length, 0);
    assert.strictEqual(client.calls.comments.length, 0);
  });

  it('dedupes repeated occurrences on a closed canonical too', () => {
    const client = fakeClient({
      closed: [closedIssue()],
      comments: { 12: [recurrenceComment(HASH, 'verify-deadbeef', '2026-10-05T00:00:00Z')] },
    });
    const r = pm.handleOccurrence(opts(), client);
    assert.strictEqual(r.action, 'duplicate-occurrence');
    assert.strictEqual(client.calls.comments.length, 0);
    assert.strictEqual(client.calls.reopens.length, 0);
  });
});

describe('handleOccurrence — new signature', () => {
  it('files a new canonical issue carrying the signature marker', () => {
    const client = fakeClient();
    const r = pm.handleOccurrence(opts(), client);
    assert.strictEqual(r.action, 'filed');
    const created = client.calls.creates[0];
    assert.ok(created.body.includes(pm.signatureMarker(HASH)));
    assert.ok(created.body.includes('ai-sdlc-postmortem-meta'));
    assert.ok(created.labels.includes('postmortem'));
    assert.deepStrictEqual(client.calls.ensured, ['postmortem']);
  });

  it('a different signature does not match the existing canonical', () => {
    const client = fakeClient({ open: [issue(7, HASH)] });
    const other = pm.computeSignatureHash({ ...PARTS, location: 'different' });
    const r = pm.handleOccurrence(opts({ signatureHash: other }), client);
    assert.strictEqual(r.action, 'filed');
  });
});

describe('handleOccurrence — dry run', () => {
  it('plans without mutating', () => {
    const closedAt = '2026-10-01T00:00:00Z';
    const c1 = fakeClient({ open: [issue(7, HASH)] });
    assert.strictEqual(pm.handleOccurrence(opts({ dryRun: true }), c1).action, 'dry_run_comment');
    assert.strictEqual(c1.calls.comments.length, 0);

    const c2 = fakeClient({ closed: [issue(12, HASH, { closedAt })] });
    assert.strictEqual(pm.handleOccurrence(opts({ dryRun: true, reopenMin: 1 }), c2).action, 'dry_run_reopen');
    assert.strictEqual(c2.calls.reopens.length, 0);

    const c3 = fakeClient();
    assert.strictEqual(pm.handleOccurrence(opts({ dryRun: true }), c3).action, 'dry_run_file');
    assert.strictEqual(c3.calls.creates.length, 0);
  });
});
