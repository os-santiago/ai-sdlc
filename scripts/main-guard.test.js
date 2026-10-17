// scripts/main-guard.test.js — unit + mocked-API tests for the main-guard
// sentinel (issue #189). Ports the Hermes pr_merger MAIN_GUARD semantics:
// union verdict across gate workflows, freshness window, fail-open on
// stale signal, deduplicated postmortem filing, transition-only comments.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  parseCsvList,
  checkMainHealth,
  findSentinelIssue,
  filePostmortem,
  mainGuardCheck,
  main,
} = require('./main-guard');

const NOW = Date.parse('2026-10-17T12:00:00Z');
const H = 3600000;

const run = (workflowName, conclusion, ageH = 1, status = 'completed', databaseId = 11) => ({
  workflowName,
  conclusion,
  status,
  databaseId,
  headSha: 'a'.repeat(40),
  createdAt: new Date(NOW - ageH * H - 60000).toISOString(),
  updatedAt: new Date(NOW - ageH * H).toISOString(),
});
const issue = (number, title = 'fix(ci): main red — sentinel halt on o/r') => ({ number, title });

// Records every gh invocation; serves canned responses per argv prefix.
function mockGh({ runs = [], issues = [], comments = '', issueCreate = 'https://github.com/o/r/issues/42', failIssueCreate = 0 } = {}) {
  const calls = [];
  let createAttempts = 0;
  const exec = (argv) => {
    calls.push(argv);
    const key = argv.slice(0, 2).join(' ');
    if (key === 'run list') return JSON.stringify(runs);
    if (key === 'issue list') return JSON.stringify(issues);
    if (key === 'issue create') {
      createAttempts += 1;
      if (createAttempts <= failIssueCreate) throw new Error('label not found');
      return issueCreate;
    }
    if (key === 'issue close') return '';
    if (argv[0] === 'api' && String(argv[2] || '').includes('/comments')) return comments;
    if (key === 'pr comment') return '';
    throw new Error(`unexpected gh call: ${argv.join(' ')}`);
  };
  return { calls, exec };
}

const callsOf = (calls, prefix) => calls.filter((c) => c.slice(0, 2).join(' ') === prefix);

// --- checkMainHealth — union verdict + freshness window -----------------------
describe('checkMainHealth', () => {
  it('red when ANY gate workflow\'s newest in-window run failed (union)', () => {
    const { exec } = mockGh({ runs: [run('build', 'success'), run('ci', 'failure')] });
    const h = checkMainHealth('o/r', 'main', { exec, now: NOW, workflows: ['ci', 'build'] });
    assert.equal(h.status, 'red');
    assert.equal(h.run.workflowName, 'ci');
    assert.equal(h.gates.build.verdict, 'green');
  });

  it('cancelled and timed_out count as red', () => {
    const { exec } = mockGh({ runs: [run('ci', 'timed_out'), run('ci', 'success', 2)] });
    const h = checkMainHealth('o/r', 'main', { exec, now: NOW, workflows: ['ci'] });
    assert.equal(h.status, 'red');
    const { exec: e2 } = mockGh({ runs: [run('ci', 'cancelled')] });
    assert.equal(checkMainHealth('o/r', 'main', { exec: e2, now: NOW }).status, 'red');
  });

  it('green when every gate is green-or-unknown', () => {
    const { exec } = mockGh({ runs: [run('ci', 'success'), run('build', 'success')] });
    const h = checkMainHealth('o/r', 'main', { exec, now: NOW, workflows: ['ci', 'build'] });
    assert.equal(h.status, 'green');
  });

  it('pending/in-progress runs never count as signal', () => {
    const { exec } = mockGh({
      runs: [run('ci', null, 0, 'in_progress'), run('ci', 'success', 3)],
    });
    const h = checkMainHealth('o/r', 'main', { exec, now: NOW, workflows: ['ci'] });
    assert.equal(h.status, 'green'); // newest COMPLETED is the 3h-old green
  });

  it('gate run older than the window is stale → verdict unknown for that gate', () => {
    const { exec } = mockGh({ runs: [run('ci', 'failure', 100)] });
    const h = checkMainHealth('o/r', 'main', { exec, now: NOW, workflows: ['ci'], maxAgeH: 72 });
    // gate stale → falls back to newest completed across ALL workflows —
    // which is the same stale run → unknown (fail-open), NOT red.
    assert.equal(h.status, 'unknown');
    assert.equal(h.fallback, true);
    assert.equal(h.gates.ci.stale, true);
  });

  it('all gates unknown → falls back to newest completed run of any workflow', () => {
    const { exec } = mockGh({ runs: [run('unrelated-wf', 'failure', 1)] });
    const h = checkMainHealth('o/r', 'main', { exec, now: NOW, workflows: ['ci'] });
    assert.equal(h.status, 'red');
    assert.equal(h.fallback, true);
  });

  it('no runs at all → unknown', () => {
    const { exec } = mockGh({ runs: [] });
    assert.equal(checkMainHealth('o/r', 'main', { exec, now: NOW }).status, 'unknown');
  });

  it('API failure → unknown (fail-open), never throws', () => {
    const exec = () => { throw new Error('HTTP 502'); };
    const h = checkMainHealth('o/r', 'main', { exec, now: NOW });
    assert.equal(h.status, 'unknown');
    assert.match(h.error, /502/);
  });

  it('unsafe base ref → unknown', () => {
    const { exec } = mockGh();
    assert.equal(checkMainHealth('o/r', 'main; rm -rf /', { exec }).status, 'unknown');
  });
});

// --- mainGuardCheck — the sentinel decision -----------------------------------
describe('mainGuardCheck — red main (halt + postmortem)', () => {
  it('files a deduplicated postmortem issue and halts (AC1)', () => {
    const { calls, exec } = mockGh({ runs: [run('ci', 'failure')] });
    const r = mainGuardCheck({ repo: 'o/r', exec, now: NOW, apply: true });
    assert.equal(r.action, 'halt');
    assert.equal(r.verdict, 'red');
    assert.equal(r.issue, 42);
    assert.equal(r.filed, true);
    const create = callsOf(calls, 'issue create');
    assert.equal(create.length, 1);
    assert.ok(create[0].includes('postmortem'));
    assert.match(create[0][create[0].indexOf('--title') + 1], /main red/);
  });

  it('open sentinel issue → halt without re-filing (dedup)', () => {
    const { calls, exec } = mockGh({ runs: [run('ci', 'failure')], issues: [issue(7)] });
    const r = mainGuardCheck({ repo: 'o/r', exec, now: NOW, apply: true });
    assert.equal(r.action, 'halt');
    assert.equal(r.issue, 7);
    assert.equal(r.dedup, 'sentinel-issue-open');
    assert.equal(callsOf(calls, 'issue create').length, 0);
  });

  it('retries issue create unlabeled when labels are missing', () => {
    const { calls, exec } = mockGh({ runs: [run('ci', 'failure')], failIssueCreate: 1 });
    const r = mainGuardCheck({ repo: 'o/r', exec, now: NOW, apply: true });
    assert.equal(r.filed, true);
    assert.equal(callsOf(calls, 'issue create').length, 2);
    assert.ok(!callsOf(calls, 'issue create')[1].includes('--label'));
  });

  it('without --apply the halt is reported but nothing is filed', () => {
    const { calls, exec } = mockGh({ runs: [run('ci', 'failure')] });
    const r = mainGuardCheck({ repo: 'o/r', exec, now: NOW });
    assert.equal(r.action, 'halt');
    assert.equal(callsOf(calls, 'issue create').length, 0);
  });
});

describe('mainGuardCheck — green main (cleared)', () => {
  it('green + open sentinel issue → cleared and issue closed with notice (AC2)', () => {
    const { calls, exec } = mockGh({ runs: [run('ci', 'success')], issues: [issue(9)] });
    const r = mainGuardCheck({ repo: 'o/r', exec, now: NOW, apply: true });
    assert.equal(r.action, 'cleared');
    assert.equal(r.issue, 9);
    const close = callsOf(calls, 'issue close');
    assert.equal(close.length, 1);
    assert.match(close[0][close[0].indexOf('--comment') + 1], /cleared/);
  });

  it('steady green without a latch → silent ok, no mutations (AC2: no new postmortem)', () => {
    const { calls, exec } = mockGh({ runs: [run('ci', 'success')] });
    const r = mainGuardCheck({ repo: 'o/r', exec, now: NOW, apply: true });
    assert.equal(r.action, 'ok');
    assert.equal(callsOf(calls, 'issue create').length, 0);
    assert.equal(callsOf(calls, 'issue close').length, 0);
  });

  it('red→green→red cycle: one issue per episode, comments only on transitions', () => {
    // Episode 1: red files issue #42 and comments once on the PR.
    const state = { open: [], next: 5, commented: 0 };
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      const key = argv.slice(0, 2).join(' ');
      if (key === 'run list') return JSON.stringify(state.runs);
      if (key === 'issue list') return JSON.stringify(state.open);
      if (key === 'issue create') {
        const n = state.next++;
        state.open.push(issue(n));
        return `https://github.com/o/r/issues/${n}`;
      }
      if (key === 'issue close') { state.open = []; return ''; }
      if (argv[0] === 'api') return state.commented ? `<!-- ai-sdlc:main-guard-halt issue=${state.open[0] ? state.open[0].number : 0} -->` : '';
      if (key === 'pr comment') { state.commented += 1; return ''; }
      throw new Error(`unexpected gh call: ${argv.join(' ')}`);
    };
    // red → halt + file + PR comment
    state.runs = [run('ci', 'failure')];
    let r = mainGuardCheck({ repo: 'o/r', pr: 3, exec, now: NOW, apply: true });
    assert.equal(r.action, 'halt'); assert.equal(r.issue, 5);
    assert.equal(state.commented, 1);
    // red again (same episode) → halt, no new issue, no new comment
    state.commented = 1; // simulate the posted marker comment visible in API
    r = mainGuardCheck({ repo: 'o/r', pr: 3, exec, now: NOW + H, apply: true });
    assert.equal(r.action, 'halt'); assert.equal(r.issue, 5);
    assert.equal(r.dedup, 'sentinel-issue-open');
    assert.equal(r.prComment.skipped, 'dedup');
    assert.equal(callsOf(calls, 'issue create').length, 1);
    // green → cleared + close (transition comment on the issue)
    state.runs = [run('ci', 'success')];
    r = mainGuardCheck({ repo: 'o/r', pr: 3, exec, now: NOW + 2 * H, apply: true });
    assert.equal(r.action, 'cleared'); assert.equal(r.closed, true);
    assert.equal(callsOf(calls, 'issue close').length, 1);
    // green again → silent ok (no transition → no comment)
    r = mainGuardCheck({ repo: 'o/r', pr: 3, exec, now: NOW + 3 * H, apply: true });
    assert.equal(r.action, 'ok');
    assert.equal(callsOf(calls, 'issue close').length, 1);
    assert.equal(callsOf(calls, 'issue create').length, 1);
    // red again (new episode) → halt + NEW issue + new PR comment
    state.commented = 0; state.runs = [run('ci', 'failure')];
    r = mainGuardCheck({ repo: 'o/r', pr: 3, exec, now: NOW + 4 * H, apply: true });
    assert.equal(r.action, 'halt'); assert.equal(r.issue, 6);
    assert.equal(state.commented, 1);
    assert.equal(callsOf(calls, 'issue create').length, 2);
  });
});

describe('mainGuardCheck — stale/unknown and disable (fail-open)', () => {
  it('no gate signal inside the window → ok, never a latch (AC3)', () => {
    const { calls, exec } = mockGh({ runs: [run('ci', 'failure', 200)] }); // ancient
    const r = mainGuardCheck({ repo: 'o/r', exec, now: NOW, apply: true, maxAgeH: 72 });
    assert.equal(r.action, 'ok');
    assert.equal(r.verdict, 'unknown');
    assert.match(r.reason, /fail-open/);
    assert.equal(callsOf(calls, 'issue create').length, 0);
  });

  it('API outage → ok (fail-open), no mutations', () => {
    const exec = (argv) => {
      if (argv.slice(0, 2).join(' ') === 'run list') throw new Error('HTTP 503');
      if (argv.slice(0, 2).join(' ') === 'issue list') return '[]';
      throw new Error(`unexpected: ${argv.join(' ')}`);
    };
    const r = mainGuardCheck({ repo: 'o/r', exec, now: NOW, apply: true });
    assert.equal(r.action, 'ok');
    assert.equal(r.verdict, 'unknown');
  });

  it('disabled → action disabled and zero gh calls (AC4)', () => {
    const { calls, exec } = mockGh();
    const r = mainGuardCheck({ repo: 'o/r', exec, enabled: false });
    assert.equal(r.action, 'disabled');
    assert.equal(calls.length, 0);
  });
});

// --- CLI ----------------------------------------------------------------------
describe('cli', () => {
  it('parses csv gate workflows', () => {
    assert.deepEqual(parseCsvList('ci, Build ,'), ['ci', 'build']);
  });

  it('end-to-end: red → halt, writes --json and --github-output', () => {
    const { exec } = mockGh({ runs: [run('ci', 'failure')] });
    const outFile = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'mg-')), 'out.txt');
    process.env.GITHUB_OUTPUT = outFile;
    try {
      let out = '';
      const r = main(
        ['--repo', 'o/r', '--gate-workflows', 'ci,build', '--apply', '--json', '--github-output'],
        { exec, stdout: (s) => { out = s; } },
      );
      assert.equal(r.action, 'halt');
      assert.equal(JSON.parse(out).action, 'halt');
      const written = fs.readFileSync(outFile, 'utf8');
      assert.match(written, /main_guard=halt/);
      assert.match(written, /main_guard_verdict=red/);
      assert.match(written, /main_guard_issue=42/);
      assert.match(written, /main_guard_workflow=ci/);
    } finally {
      delete process.env.GITHUB_OUTPUT;
    }
  });

  it('requires --repo', () => {
    assert.throws(() => main([], { exec: () => '' }), /--repo/);
  });
});
