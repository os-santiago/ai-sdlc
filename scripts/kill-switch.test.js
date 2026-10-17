// scripts/kill-switch.test.js — node --test coverage for the org
// kill-switch (issue #172): engaged / disengaged / unreadable verdicts,
// every signal channel, and the engage/release mutations.
'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const {
  OFF_LABEL,
  envEngaged,
  checkKillSwitch,
  engage,
  release,
} = require('./kill-switch.js');

const REPO = 'acme/app';
const CONTROL = 'acme/ai-sdlc';
const ISSUE = { number: 7, title: 'AI-SDLC autonomy OFF', created_at: '2026-01-01T00:00:00Z', actor: 'ops' };

const clean = () => async () => ({ ok: true, issues: [] });
const openIssue = () => async () => ({ ok: true, issues: [ISSUE] });
const failing = () => async () => ({ ok: false, error: 'http-500' });

// --- envEngaged ---------------------------------------------------------------

test('envEngaged: unset / falsy values stay clear', () => {
  assert.equal(envEngaged({}), null);
  for (const v of ['0', 'false', 'no', 'off', 'clear', 'FALSE', '']) {
    assert.equal(envEngaged({ AI_SDLC_KILL: v }), null, v);
  }
});

test('envEngaged: truthy values engage', () => {
  for (const v of ['1', 'true', 'yes', 'on', 'TRUE', ' Engaged ']) {
    assert.match(envEngaged({ AI_SDLC_KILL: v }), /AI_SDLC_KILL=/, v);
  }
  assert.match(envEngaged({ AI_SDLC_OFF: '1' }), /AI_SDLC_OFF=1/);
});

test('envEngaged: unrecognized non-empty value engages (fail-closed)', () => {
  assert.match(envEngaged({ AI_SDLC_KILL: 'ture' }), /unrecognized value/);
});

// --- check: disengaged ---------------------------------------------------------

test('check: all channels clear → allowed', async () => {
  const v = await checkKillSwitch({ repo: REPO, controlRepo: CONTROL, env: {}, readIssues: clean() });
  assert.equal(v.state, 'clear');
  assert.equal(v.allowed, true);
  assert.deepEqual(v.sources.map((s) => s.state), ['clear', 'clear', 'clear']);
});

test('check: explicit falsy env stays clear', async () => {
  const v = await checkKillSwitch({ repo: REPO, env: { AI_SDLC_KILL: 'false', AI_SDLC_OFF: '0' }, readIssues: clean() });
  assert.equal(v.allowed, true);
});

test('check: control repo defaults to target — single read', async () => {
  let reads = 0;
  const v = await checkKillSwitch({
    repo: REPO,
    env: {},
    readIssues: async () => { reads += 1; return { ok: true, issues: [] }; },
  });
  assert.equal(v.allowed, true);
  assert.equal(reads, 1);
});

// --- check: engaged ------------------------------------------------------------

test('check: env kill engages without touching the network', async () => {
  let reads = 0;
  const v = await checkKillSwitch({
    repo: REPO,
    env: { AI_SDLC_KILL: '1' },
    readIssues: async () => { reads += 1; return { ok: true, issues: [] }; },
  });
  assert.equal(v.state, 'engaged');
  assert.equal(v.allowed, false);
  assert.match(v.reason, /AI_SDLC_KILL=1/);
});

test('check: open ai-sdlc:off issue in control repo engages (fleet kill)', async () => {
  const v = await checkKillSwitch({
    repo: REPO,
    controlRepo: CONTROL,
    env: {},
    readIssues: async (r) => (r === CONTROL ? { ok: true, issues: [ISSUE] } : { ok: true, issues: [] }),
  });
  assert.equal(v.state, 'engaged');
  assert.equal(v.allowed, false);
  assert.match(v.reason, new RegExp(`#${ISSUE.number}.*${CONTROL.replace('/', '\\/')}`));
});

test('check: open ai-sdlc:off issue in target repo engages (per-repo kill)', async () => {
  const v = await checkKillSwitch({
    repo: REPO,
    controlRepo: CONTROL,
    env: {},
    readIssues: async (r) => (r === REPO ? { ok: true, issues: [ISSUE] } : { ok: true, issues: [] }),
  });
  assert.equal(v.state, 'engaged');
  assert.equal(v.allowed, false);
  assert.match(v.reason, /repo-issue|issue #7/);
});

test('check: AI_SDLC_CONTROL_REPO env is honored when no flag passed', async () => {
  const seen = [];
  const v = await checkKillSwitch({
    repo: REPO,
    env: { AI_SDLC_CONTROL_REPO: CONTROL },
    readIssues: async (r) => { seen.push(r); return { ok: true, issues: [] }; },
  });
  assert.equal(v.allowed, true);
  assert.deepEqual(seen.sort(), [CONTROL, REPO].sort());
});

// --- check: unreadable → fail-closed -------------------------------------------

test('check: unreadable control repo → autonomy OFF', async () => {
  const v = await checkKillSwitch({
    repo: REPO,
    controlRepo: CONTROL,
    env: {},
    readIssues: async (r) => (r === CONTROL ? { ok: false, error: 'boom' } : { ok: true, issues: [] }),
  });
  assert.equal(v.state, 'unreadable');
  assert.equal(v.allowed, false);
  assert.match(v.reason, /unreadable/);
});

test('check: unreadable target repo → autonomy OFF', async () => {
  const v = await checkKillSwitch({ repo: REPO, env: {}, readIssues: failing() });
  assert.equal(v.state, 'unreadable');
  assert.equal(v.allowed, false);
});

test('check: engaged beats unreadable when both fire', async () => {
  const v = await checkKillSwitch({
    repo: REPO,
    controlRepo: CONTROL,
    env: {},
    readIssues: async (r) => (r === CONTROL ? { ok: true, issues: [ISSUE] } : { ok: false, error: 'boom' }),
  });
  assert.equal(v.state, 'engaged');
  assert.equal(v.allowed, false);
});

test('check: invalid repo shape → unreadable (never allows)', async () => {
  const v = await checkKillSwitch({ repo: 'not-a-repo', env: {}, readIssues: clean() });
  assert.equal(v.state, 'unreadable');
  assert.equal(v.allowed, false);
});

// --- engage --------------------------------------------------------------------

test('engage: creates the control issue when none is open', async () => {
  const calls = [];
  const gh = (args) => {
    calls.push(args);
    if (args[0] === 'issue' && args[1] === 'create') return 'https://github.com/acme/ai-sdlc/issues/9\n';
    return '';
  };
  const res = await engage({ repo: CONTROL, actor: 'ops', reason: 'incident-42', gh, readIssues: clean() });
  assert.equal(res.action, 'engaged');
  assert.equal(res.issue, 9);
  assert.equal(res.reused, false);
  assert.equal(res.actor, 'ops');
  assert.ok(res.at);
  const created = calls.find((c) => c[0] === 'issue' && c[1] === 'create');
  assert.ok(created, 'issue create called');
  assert.ok(created.includes('--label') && created.includes(OFF_LABEL));
  assert.ok(created.join(' ').includes('ops'));
});

test('engage: comments on the existing open issue instead of duplicating', async () => {
  const calls = [];
  const gh = (args) => { calls.push(args); return ''; };
  const res = await engage({ repo: CONTROL, actor: 'ops', gh, readIssues: openIssue() });
  assert.equal(res.action, 'engaged');
  assert.equal(res.reused, true);
  assert.equal(res.issue, ISSUE.number);
  assert.ok(calls.some((c) => c[0] === 'issue' && c[1] === 'comment' && c[2] === String(ISSUE.number)));
  assert.ok(!calls.some((c) => c[0] === 'issue' && c[1] === 'create'));
});

test('engage: fails loudly when the control repo cannot be read', async () => {
  await assert.rejects(
    engage({ repo: CONTROL, actor: 'ops', gh: () => '', readIssues: failing() }),
    /cannot read/);
});

// --- release -------------------------------------------------------------------

test('release: comments + closes every open off-issue', async () => {
  const calls = [];
  const two = async () => ({ ok: true, issues: [ISSUE, { ...ISSUE, number: 8 }] });
  const gh = (args) => { calls.push(args); return ''; };
  const res = await release({ repo: CONTROL, actor: 'ops', reason: 'resolved', gh, readIssues: two });
  assert.equal(res.action, 'released');
  assert.deepEqual(res.closed, [7, 8]);
  assert.equal(calls.filter((c) => c[1] === 'close').length, 2);
  assert.equal(calls.filter((c) => c[1] === 'comment').length, 2);
});

test('release: no open off-issue → closed empty, still audited', async () => {
  const res = await release({ repo: CONTROL, actor: 'ops', gh: () => '', readIssues: clean() });
  assert.equal(res.action, 'released');
  assert.deepEqual(res.closed, []);
});
