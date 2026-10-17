// scripts/contract-drift.test.js — node --test coverage for the
// declared-vs-observed contract drift sweep (issue #176).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');

const cd = require('./contract-drift');

const REPO = 'o/r';
const SIG = cd.repoSignature(REPO);
const NOW = '2026-10-14T00:00:00Z';
const WINDOW = { startMs: Date.parse('2026-09-14T00:00:00Z'), endMs: Date.parse(NOW) };

// --- fixture builders ----------------------------------------------------------

function contract(over = {}) {
  return {
    spec: '1.0.0',
    autonomy: { level: 'auto-merge-low', merge_requires_checks: true },
    triggers: { issue_label: 'ready-to-implement', skip_labels: ['epic', 'needs-human'] },
    verify: { required_checks: ['contract', 'actionlint'] },
    review: { mode: 'ai' },
    merge: { method: 'squash', skip_labels: ['needs-human', 'hold'] },
    labels: { state_machine: { veto_pairs: [{ winner: 'needs-human', loser: 'ready-to-implement' }] } },
    engine: { name: 'scc' },
    ...over,
  };
}

function pipelinePr(n, over = {}) {
  return {
    number: n, headRefName: `feat/issue-${n}-x`, state: 'MERGED',
    labels: [{ name: 'ai-sdlc' }], mergedAt: '2026-10-01T00:00:00Z',
    createdAt: '2026-09-30T00:00:00Z', ...over,
  };
}

function runs(stages, at = '2026-10-01T12:00:00Z') {
  return stages.map((s) => ({ workflowName: `ai-sdlc-${s}`, conclusion: 'success', createdAt: at }));
}

const ALL_STAGES = ['intake', 'implement', 'review', 'automerge', 'verify', 'ci-repair', 'sweep'];

function model(over = {}) {
  const m = {
    repo: REPO,
    contract: contract(),
    observed: {
      workflowRuns: [],
      pullRequests: [],
      issues: [],
      stages: {},
    },
  };
  return { ...m, ...over, observed: { ...m.observed, ...(over.observed || {}) } };
}

/** A fully consistent model: every expected stage ran, contract honored. */
function cleanModel() {
  return model({
    contract: contract({ autonomy: { level: 'auto-merge-low' } }),
    observed: {
      workflowRuns: runs(['intake', 'implement', 'review', 'automerge', 'verify']),
      pullRequests: [pipelinePr(10, {
        mergeParents: 1,
        statusCheckRollup: [{ name: 'contract', conclusion: 'SUCCESS' }, { name: 'actionlint', conclusion: 'SUCCESS' }],
      })],
      issues: [{ number: 3, labels: [{ name: 'scc:pr-opened' }] }],
      stages: {
        implement: { runs: 1, engines: ['scc'] },
        review: { runs: 1, engines: ['scc'] },
      },
    },
  });
}

/** In-memory client exposing the same call surface the gh adapter uses. */
function fakeClient({ open = [], closed = [], comments = {} } = {}) {
  const calls = { comments: [], reopens: [], closes: [], creates: [], ensured: [] };
  return {
    calls,
    listIssues({ state }) { return state === 'open' ? open : closed; },
    listComments({ issueNumber }) { return comments[issueNumber] || []; },
    addComment({ issueNumber, body }) { calls.comments.push({ issueNumber, body }); },
    reopenIssue({ issueNumber }) { calls.reopens.push(issueNumber); },
    closeIssue({ issueNumber }) { calls.closes.push(issueNumber); },
    ensureLabel({ label }) { calls.ensured.push(label); },
    createIssue({ title, body, labels }) {
      calls.creates.push({ title, body, labels });
      return { number: 900, url: 'u/900' };
    },
  };
}

function driftIssue(number, fingerprint = 'oldfp') {
  return {
    number, title: 'contract drift', labels: [],
    url: `u/${number}`,
    body: `drift\n${cd.signatureMarker(SIG)}\n${cd.metaMarker(REPO, fingerprint)}`,
  };
}

// --- evaluation ---------------------------------------------------------------

describe('evaluateDrift — divergence detection', () => {
  it('reports nothing for a consistent contract + observations', () => {
    const findings = cd.evaluateDrift(cleanModel(), WINDOW);
    assert.deepStrictEqual(findings, []);
  });

  it('flags merges above the declared autonomy ceiling', () => {
    const m = cleanModel();
    m.contract = contract({ autonomy: { level: 'suggest' } });
    const f = cd.evaluateDrift(m, WINDOW);
    const hit = f.find((x) => x.check === 'autonomy-exceeded');
    assert.ok(hit, 'expected autonomy-exceeded finding');
    assert.strictEqual(hit.declared, 'suggest');
    assert.ok(String(hit.observed).includes('#10'));
  });

  it('allows merges at or above auto-merge-low', () => {
    for (const level of ['auto-merge-low', 'auto-merge-all', 'auto-deploy']) {
      const m = cleanModel();
      m.contract = contract({ autonomy: { level } });
      const f = cd.evaluateDrift(m, WINDOW);
      assert.ok(!f.some((x) => x.check === 'autonomy-exceeded'), `level ${level}`);
    }
  });

  it('flags a declared stage engine that never appears in invocations', () => {
    const m = cleanModel();
    m.observed.stages = {
      implement: { runs: 2, engines: ['devin'] },
      review: { runs: 1, engines: ['devin'] },
    };
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(f.some((x) => x.check === 'declared-engine-unused' && x.subject === 'engine:scc'));
    assert.ok(f.filter((x) => x.check === 'stage-engine-mismatch').length === 2);
  });

  it('skips engine checks when no engine evidence exists (partial data)', () => {
    const m = cleanModel();
    delete m.observed.stages;
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(!f.some((x) => ['declared-engine-unused', 'stage-engine-mismatch'].includes(x.check)));
  });

  it('flags expected stages that never ran in the window', () => {
    const m = model({
      observed: {
        workflowRuns: runs(['intake', 'implement']),   // review silent
        pullRequests: [pipelinePr(7)],
        issues: [],
      },
    });
    const f = cd.evaluateDrift(m, WINDOW);
    const stages = f.filter((x) => x.check === 'stage-never-invoked').map((x) => x.subject);
    assert.ok(stages.includes('stage:review'));
    assert.ok(!stages.includes('stage:implement'));
    assert.ok(!stages.includes('stage:intake'));
    // automerge/verify are out of scope: invoked via `uses:` inside callers,
    // they mint no observable runs or PR checks — outcome checks cover them.
    assert.ok(!stages.includes('stage:automerge'));
    assert.ok(!stages.includes('stage:verify'));
  });

  it('counts reusable-call stage evidence from PR check-rollups', () => {
    const m = model({
      observed: {
        workflowRuns: [],   // no ai-sdlc-* runs — `uses:` stages mint none
        pullRequests: [pipelinePr(8, {
          statusCheckRollup: [
            { name: 'intake', conclusion: 'SUCCESS' },
            { name: 'implement', conclusion: 'SUCCESS' },
            { name: 'review / review', conclusion: 'SUCCESS' },
            { name: 'contract', conclusion: 'SUCCESS' },
          ],
        })],
        issues: [],
      },
    });
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(!f.some((x) => x.check === 'stage-never-invoked'));
  });

  it('does not flag stage-never-invoked with no pipeline activity', () => {
    const m = model({ observed: { workflowRuns: [], pullRequests: [], issues: [] } });
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(!f.some((x) => x.check === 'stage-never-invoked'));
  });

  it('flags declared engines missing from the adapter registry', () => {
    const m = cleanModel();
    m.registry = { devin: {} };   // 'scc' no longer certified
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(f.some((x) => x.check === 'engine-unregistered' && x.subject === 'engine:scc'));
  });

  it('flags required checks that never appear in rollups', () => {
    const m = cleanModel();
    m.observed.pullRequests[0].statusCheckRollup = [{ name: 'contract', conclusion: 'SUCCESS' }];
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(f.some((x) => x.check === 'required-check-missing' && x.subject === 'check:actionlint'));
    assert.ok(!f.some((x) => x.subject === 'check:contract'));
  });

  it('skips required-check detection when no rollup data exists', () => {
    const m = cleanModel();
    delete m.observed.pullRequests[0].statusCheckRollup;
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(!f.some((x) => x.check === 'required-check-missing'));
  });

  it('flags merges landed while checks were failing', () => {
    const m = cleanModel();
    m.observed.pullRequests[0].statusCheckRollup = [
      { name: 'contract', conclusion: 'SUCCESS' },
      { name: 'actionlint', conclusion: 'FAILURE' },
    ];
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(f.some((x) => x.check === 'merge-checks-bypassed' && x.subject === 'pr:10'));
  });

  it('flags merge method divergence from merge-commit parent count', () => {
    const m = cleanModel();
    m.observed.pullRequests[0].mergeParents = 2;   // merge commit under squash contract
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(f.some((x) => x.check === 'merge-method-mismatch' && x.subject === 'pr:10'));
    // and the inverse: declared merge + 1 parent
    m.contract = contract({ merge: { method: 'merge', skip_labels: [] } });
    m.observed.pullRequests[0].mergeParents = 1;
    const f2 = cd.evaluateDrift(m, WINDOW);
    assert.ok(f2.some((x) => x.check === 'merge-method-mismatch'));
  });

  it('flags merges that landed under a merge-skip label', () => {
    const m = cleanModel();
    m.observed.pullRequests[0].labels = [{ name: 'ai-sdlc' }, { name: 'needs-human' }];
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(f.some((x) => x.check === 'skip-label-violation' && x.subject === 'pr:10'));
  });

  it('flags issues holding forbidden label pairs (declared ∪ skip_labels×trigger)', () => {
    const m = cleanModel();
    m.observed.issues = [
      { number: 5, labels: [{ name: 'ready-to-implement' }, { name: 'needs-human' }] },
      { number: 6, labels: [{ name: 'ready-to-implement' }, { name: 'epic' }] },  // skip_label veto
      { number: 7, labels: [{ name: 'scc:queued' }] },                            // clean
    ];
    const f = cd.evaluateDrift(m, WINDOW);
    const hits = f.filter((x) => x.check === 'label-veto-violation').map((x) => x.subject);
    assert.ok(hits.includes('issue:5'));
    assert.ok(hits.includes('issue:6'));   // epic veto via triggers.skip_labels
    assert.ok(!hits.includes('issue:7'));
  });

  it('treats engine stage-definition objects like bare ids', () => {
    const m = model({
      contract: contract({ engines: { implement: { engine: 'devin' } } }),
      observed: {
        workflowRuns: runs(['implement']),
        pullRequests: [],
        issues: [],
        stages: { implement: { runs: 1, engines: ['devin'] } },
      },
    });
    assert.strictEqual(cd.contractEngineForStage(m.contract, 'implement'), 'devin');
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(!f.some((x) => x.check === 'stage-engine-mismatch' && x.subject === 'stage:implement'));
    assert.ok(!f.some((x) => x.check === 'declared-engine-unused'));   // scc unpinned but observed nowhere? all-stages unpinned → scc expected
  });
});

// --- window handling -----------------------------------------------------------

describe('observed-model windowing', () => {
  it('ignores out-of-window merges and runs', () => {
    const m = model({
      contract: contract({ autonomy: { level: 'suggest' } }),
      observed: {
        workflowRuns: runs(ALL_STAGES, '2026-01-01T00:00:00Z'),   // stale
        pullRequests: [pipelinePr(4, {
          mergedAt: '2026-01-05T00:00:00Z',
          createdAt: '2026-01-01T00:00:00Z', closedAt: '2026-01-05T00:00:00Z',
        })],
        issues: [],
      },
    });
    const f = cd.evaluateDrift(m, WINDOW);
    assert.ok(!f.some((x) => x.check === 'autonomy-exceeded'));
    assert.ok(!f.some((x) => x.check === 'stage-never-invoked'));
  });

  it('derives stage run counts from workflow names', () => {
    const n = cd.normalizeObserved(model({
      observed: { workflowRuns: runs(['intake', 'implement', 'implement', 'ci-repair']) },
    }), WINDOW);
    assert.strictEqual(n.stages.intake.runs, 1);
    assert.strictEqual(n.stages.implement.runs, 2);
    assert.strictEqual(n.stages['ci-repair'].runs, 1);
  });
});

// --- canonical dedup lifecycle ---------------------------------------------------

describe('handleDrift — canonical issue lifecycle', () => {
  const findings = [
    { check: 'autonomy-exceeded', subject: 'autonomy.level', declared: 'suggest', observed: '#1', detail: 'd' },
  ];

  it('files exactly one canonical issue for a new divergence set', () => {
    const client = fakeClient();
    const r = cd.handleDrift({ repo: REPO, findings }, client);
    assert.strictEqual(r.action, 'filed');
    assert.strictEqual(client.calls.creates.length, 1);
    const body = client.calls.creates[0].body;
    assert.ok(body.includes(cd.signatureMarker(SIG)));
    assert.ok(body.includes('ai-sdlc-drift-meta'));
    assert.deepStrictEqual(client.calls.ensured, [cd.DRIFT_LABEL]);
  });

  it('is a no-op when the open canonical already carries the same fingerprint', () => {
    const fp = cd.fingerprintFindings(findings);
    const client = fakeClient({ open: [driftIssue(7, fp)] });
    const r = cd.handleDrift({ repo: REPO, findings }, client);
    assert.strictEqual(r.action, 'unchanged');
    assert.strictEqual(client.calls.creates.length, 0);
    assert.strictEqual(client.calls.comments.length, 0);
  });

  it('comments (never re-files) when the finding set changed on an open canonical', () => {
    const client = fakeClient({ open: [driftIssue(7, 'different-fp')] });
    const r = cd.handleDrift({ repo: REPO, findings }, client);
    assert.strictEqual(r.action, 'updated');
    assert.strictEqual(client.calls.creates.length, 0);
    assert.strictEqual(client.calls.comments.length, 1);
    assert.ok(client.calls.comments[0].body.includes('finding set changed'));
    assert.ok(client.calls.comments[0].body.includes(r.fingerprint));
  });

  it('reopens the closed canonical (comment first) on recurring drift', () => {
    const client = fakeClient({ closed: [driftIssue(3)] });
    const r = cd.handleDrift({ repo: REPO, findings }, client);
    assert.strictEqual(r.action, 'reopened');
    assert.deepStrictEqual(client.calls.reopens, [3]);
    assert.strictEqual(client.calls.comments.length, 1);   // evidence before reopen
    assert.strictEqual(client.calls.creates.length, 0);
  });

  it('closes the open canonical when drift clears', () => {
    const client = fakeClient({ open: [driftIssue(7)] });
    const r = cd.handleDrift({ repo: REPO, findings: [] }, client);
    assert.strictEqual(r.action, 'resolved');
    assert.deepStrictEqual(client.calls.closes, [7]);
    assert.strictEqual(client.calls.comments.length, 1);
  });

  it('reports clean — never files — when there is no drift and no canonical', () => {
    const client = fakeClient();
    const r = cd.handleDrift({ repo: REPO, findings: [] }, client);
    assert.strictEqual(r.action, 'clean');
    assert.strictEqual(client.calls.creates.length, 0);
    assert.strictEqual(client.calls.comments.length, 0);
    assert.strictEqual(client.calls.closes.length, 0);
  });

  it('dry-run mutates nothing', () => {
    const client = fakeClient();
    const r = cd.handleDrift({ repo: REPO, findings, dryRun: true }, client);
    assert.strictEqual(r.action, 'dry_run_file');
    assert.strictEqual(client.calls.creates.length, 0);
    const client2 = fakeClient({ open: [driftIssue(7)] });
    const r2 = cd.handleDrift({ repo: REPO, findings: [], dryRun: true }, client2);
    assert.strictEqual(r2.action, 'dry_run_close');
    assert.strictEqual(client2.calls.closes.length, 0);
  });

  it('stable fingerprint: same finding set → same hash, reordered → same hash', () => {
    const a = [{ check: 'c1', subject: 's1' }, { check: 'c2', subject: 's2' }];
    const b = [{ check: 'c2', subject: 's2' }, { check: 'c1', subject: 's1' }];
    assert.strictEqual(cd.fingerprintFindings(a), cd.fingerprintFindings(b));
    assert.notStrictEqual(cd.fingerprintFindings(a), cd.fingerprintFindings([a[0]]));
  });
});
