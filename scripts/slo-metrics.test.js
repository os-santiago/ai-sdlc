// scripts/slo-metrics.test.js — fixture-driven tests for the agentic SLO
// metrics reporter (issue #194). All computation is pure over the collected
// model; `gh` is exercised only through the injectable exec seam.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  parseDuration,
  isPipelinePr,
  linkedIssueNumber,
  collectData,
  computeSlos,
  renderMarkdown,
  renderPrometheus,
  parseArgs,
  main,
} = require('./slo-metrics');

// Fixed window: [2025-05-31, 2025-06-30) — deterministic, no wall clock.
const NOW = '2025-06-30T00:00:00Z';
const END_MS = Date.parse(NOW);
const START_MS = END_MS - 30 * 86400e3;

const pr = (over) => ({
  number: 0,
  title: 't',
  state: 'OPEN',
  headRefName: 'x',
  headRefOid: 'sha',
  createdAt: null,
  updatedAt: null,
  closedAt: null,
  mergedAt: null,
  isDraft: false,
  labels: [],
  body: '',
  ...over,
});

const issue = (number, labels = [], createdAt = '2025-06-01T00:00:00Z', closedAt = null) => ({
  number,
  title: 'i',
  state: closedAt ? 'closed' : 'open',
  createdAt,
  closedAt,
  labels: labels.map((name) => ({ name })),
});

const run = (conclusion, createdAt, id) => ({
  databaseId: id,
  createdAt,
  status: 'completed',
  conclusion,
  event: 'pull_request',
  headSha: 'sha',
  workflowName: 'ci',
});

// --- reference fixture --------------------------------------------------------
// PR10 merged (issue 5 exists) · PR11 merged (issue 7 unresolvable → pr basis)
// PR12 open + needs-human on both sides (deduped subject) · PR13 ai-sdlc label,
// closed unmerged, no runs sampled · PR14 merged outside the window
// PR15 not a pipeline PR · issue 20 escalated point-in-time · issue 21 scc-flagged
function fixtureData() {
  return {
    repo: 'o/r',
    collectedAt: NOW,
    pullRequests: [
      pr({
        number: 10, state: 'MERGED', headRefName: 'feat/issue-5-add-foo',
        createdAt: '2025-06-05T00:00:00Z', mergedAt: '2025-06-10T00:00:00Z',
      }),
      pr({
        number: 11, state: 'MERGED', headRefName: 'fix/issue-7-broken',
        createdAt: '2025-06-18T00:00:00Z', mergedAt: '2025-06-20T00:00:00Z',
      }),
      pr({
        number: 12, state: 'OPEN', headRefName: 'feat/issue-8-wip',
        createdAt: '2025-06-25T00:00:00Z', labels: [{ name: 'needs-human' }],
      }),
      pr({
        number: 13, state: 'CLOSED', headRefName: 'chore-tidy',
        createdAt: '2025-06-10T00:00:00Z', closedAt: '2025-06-15T00:00:00Z',
        labels: [{ name: 'ai-sdlc' }],
      }),
      pr({
        number: 14, state: 'MERGED', headRefName: 'feat/issue-9-old',
        createdAt: '2025-03-20T00:00:00Z', mergedAt: '2025-04-01T00:00:00Z',
      }),
      pr({ number: 15, state: 'OPEN', headRefName: 'docs-misc', createdAt: '2025-06-01T00:00:00Z' }),
    ],
    issues: [
      issue(5, [], '2025-06-01T00:00:00Z'),
      issue(8, ['needs-human'], '2025-06-20T00:00:00Z'),
      issue(9, [], '2025-03-01T00:00:00Z'),
      issue(20, ['needs-human'], '2025-01-01T00:00:00Z'),
      issue(21, ['scc:failed'], '2025-06-05T00:00:00Z'),
      issue(22, [], '2025-06-05T00:00:00Z'), // plain issue — not a subject
    ],
    workflowRuns: {
      'feat/issue-5-add-foo': [
        run('success', '2025-06-08T00:00:00Z', 3), // out of order on purpose
        run('failure', '2025-06-06T00:00:00Z', 1),
        run('failure', '2025-06-07T00:00:00Z', 2),
      ],
      'fix/issue-7-broken': [run('success', '2025-06-19T00:00:00Z', 4)],
      'feat/issue-8-wip': [
        run('failure', '2025-06-26T00:00:00Z', 5),
        run('cancelled', '2025-06-27T00:00:00Z', 6),
      ],
      'feat/issue-9-old': [run('success', '2025-03-25T00:00:00Z', 7)],
    },
  };
}

const compute = (data) => computeSlos(data, { startMs: START_MS, endMs: END_MS, windowLabel: '30d' });

// --- helpers ------------------------------------------------------------------
describe('parseDuration', () => {
  it('parses m/h/d suffixes', () => {
    assert.equal(parseDuration('30d'), 30 * 86400e3);
    assert.equal(parseDuration('24h'), 24 * 3600e3);
    assert.equal(parseDuration('90m'), 90 * 60e3);
    assert.equal(parseDuration('bogus', 42), 42);
  });
});

describe('pipeline PR classification', () => {
  it('matches the sweep surface: issue branches or the ai-sdlc label', () => {
    assert.ok(isPipelinePr(pr({ headRefName: 'feat/issue-5-x' })));
    assert.ok(isPipelinePr(pr({ headRefName: 'fix/issue-9' })));
    assert.ok(isPipelinePr(pr({ headRefName: 'any', labels: [{ name: 'ai-sdlc' }] })));
    assert.ok(!isPipelinePr(pr({ headRefName: 'docs-misc' })));
  });

  it('links the issue from the branch, then closing keywords in the body', () => {
    assert.equal(linkedIssueNumber(pr({ headRefName: 'feat/issue-5-x', body: 'Closes #99' })), 5);
    assert.equal(linkedIssueNumber(pr({ headRefName: 'human-branch', body: 'Refs #3. Closes #7' })), 7);
    assert.equal(linkedIssueNumber(pr({ headRefName: 'human-branch', body: 'Refs #3' })), null);
  });
});

// --- computeSlos on the fixture ----------------------------------------------
describe('computeSlos — populated window', () => {
  const report = compute(fixtureData());

  it('counts the pipeline PR surface and window subjects', () => {
    assert.equal(report.counts.pipeline_prs, 5); // PR15 excluded
    assert.equal(report.counts.merged_prs, 3);
    assert.equal(report.counts.open_prs, 1);
    assert.equal(report.counts.closed_unmerged_prs, 1);
    assert.equal(report.counts.window_prs, 4); // PR14 merged before the window
    // issue:5, issue:7, issue:8, pr:13, issue:20 (escalated), issue:21 (scc:*)
    assert.equal(report.counts.subjects, 6);
    assert.equal(report.counts.escalated_subjects, 2);
  });

  it('time-to-merge measures issue creation → merge, falling back to PR creation', () => {
    const ttm = report.time_to_merge_seconds;
    assert.equal(ttm.count, 2); // PR14 merged outside the window
    const s10 = ttm.samples.find((s) => s.pr === 10);
    const s11 = ttm.samples.find((s) => s.pr === 11);
    assert.equal(s10.seconds, 9 * 86400);
    assert.equal(s10.basis, 'issue_created');
    assert.equal(s11.seconds, 2 * 86400);
    assert.equal(s11.basis, 'pr_created');
    assert.equal(ttm.min, 2 * 86400);
    assert.equal(ttm.max, 9 * 86400);
    assert.equal(ttm.median, (9 + 2) / 2 * 86400);
    assert.equal(ttm.p90, 9 * 86400);
  });

  it('iterations-to-green counts runs until the first success, out-of-order safe', () => {
    const it = report.iterations_to_green;
    assert.equal(it.sampled_prs, 3); // PR13 unsampled, PR14 out of window
    assert.equal(it.green, 2);
    assert.equal(it.not_green, 1);
    const s10 = it.samples.find((s) => s.pr === 10);
    assert.equal(s10.iterations, 3); // two failures then success
    const s12 = it.samples.find((s) => s.pr === 12);
    assert.equal(s12.green, false);
    assert.equal(s12.iterations, 2); // censored: attempts observed
    assert.equal(it.mean, 2);
    assert.equal(it.p90, 3);
  });

  it('escalation rate dedupes the needs-human hold across the issue↔PR pair', () => {
    const esc = report.escalation;
    assert.equal(esc.subjects, 6);
    assert.equal(esc.escalated, 2); // issue:8 once (PR12 + issue 8), issue:20
    assert.equal(esc.rate, 33.3);
    assert.deepEqual(esc.items.map((i) => `${i.kind}:${i.number}`), ['issue:8', 'issue:20']);
  });
});

// --- empty-window edge cases ---------------------------------------------------
describe('computeSlos — empty window', () => {
  const empty = { repo: 'o/r', pullRequests: [], issues: [], workflowRuns: {} };
  const report = compute(empty);

  it('produces a defined report with null aggregates, never NaN', () => {
    assert.equal(report.counts.pipeline_prs, 0);
    assert.equal(report.counts.subjects, 0);
    assert.equal(report.time_to_merge_seconds.count, 0);
    assert.equal(report.time_to_merge_seconds.median, null);
    assert.equal(report.iterations_to_green.green, 0);
    assert.equal(report.iterations_to_green.mean, null);
    assert.equal(report.escalation.rate, null);
  });

  it('renders n/a in markdown and zero-count series in prometheus', () => {
    const md = renderMarkdown(report);
    assert.match(md, /median n\/a/);
    const prom = renderPrometheus(report);
    assert.match(prom, /ai_sdlc_time_to_merge_seconds_count\{repo="o\/r"\} 0/);
    assert.match(prom, /ai_sdlc_escalations_total\{repo="o\/r"\} 0/);
    assert.doesNotMatch(prom, /^ai_sdlc_escalation_ratio\{/m); // undefined, not NaN
  });

  it('a live needs-human hold joins the denominator even off-window', () => {
    const data = {
      repo: 'o/r',
      pullRequests: [],
      issues: [issue(50, ['needs-human'], '2024-01-01T00:00:00Z')],
      workflowRuns: {},
    };
    const r = compute(data);
    assert.equal(r.escalation.subjects, 1);
    assert.equal(r.escalation.escalated, 1);
    assert.equal(r.escalation.rate, 100);
  });
});

// --- renderers -----------------------------------------------------------------
describe('renderers — populated window', () => {
  const report = compute(fixtureData());

  it('prometheus output carries repo-labeled histograms and counters', () => {
    const prom = renderPrometheus(report);
    assert.match(prom, /# TYPE ai_sdlc_time_to_merge_seconds histogram/);
    // 172800s (2d) lands in le=259200 (3d); both samples under le=2592000.
    assert.match(prom, /ai_sdlc_time_to_merge_seconds_bucket\{repo="o\/r",le="21600"\} 0/);
    assert.match(prom, /ai_sdlc_time_to_merge_seconds_bucket\{repo="o\/r",le="259200"\} 1/);
    assert.match(prom, /ai_sdlc_time_to_merge_seconds_bucket\{repo="o\/r",le="2592000"\} 2/);
    assert.match(prom, /ai_sdlc_time_to_merge_seconds_bucket\{repo="o\/r",le="\+Inf"\} 2/);
    assert.match(prom, /ai_sdlc_time_to_merge_seconds_sum\{repo="o\/r"\} 950400/);
    // iterations: green samples are {3, 1} → le="1" gets 1, le="3" gets 2.
    assert.match(prom, /ai_sdlc_iterations_to_green_bucket\{repo="o\/r",le="1"\} 1/);
    assert.match(prom, /ai_sdlc_iterations_to_green_bucket\{repo="o\/r",le="3"\} 2/);
    assert.match(prom, /ai_sdlc_escalations_total\{repo="o\/r"\} 2/);
    assert.match(prom, /ai_sdlc_escalation_ratio\{repo="o\/r"\} 33\.3/);
    assert.match(prom, /ai_sdlc_pipeline_prs_not_green\{repo="o\/r"\} 1/);
  });

  it('markdown renders the three SLO rows', () => {
    const md = renderMarkdown(report);
    assert.match(md, /time-to-merge \(issue → merge\) \| median 5\.5d/);
    assert.match(md, /iterations-to-green \| mean 2 · median 2 · p90 3 \| 2 green \/ 3 sampled/);
    assert.match(md, /escalation rate \(needs-human\) \| 33\.3% \| 2\/6 subjects/);
  });
});

// --- collection (mocked gh) ------------------------------------------------------
describe('collectData', () => {
  it('fetches PRs + issues once and runs only for window-active pipeline heads', () => {
    const data = fixtureData();
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      const key = argv.slice(0, 2).join(' ');
      if (key === 'pr list') return JSON.stringify(data.pullRequests);
      if (key === 'issue list') return JSON.stringify(data.issues);
      if (key === 'run list') return JSON.stringify(data.workflowRuns[argv[argv.indexOf('--branch') + 1]] || []);
      throw new Error(`unexpected gh call: ${argv.join(' ')}`);
    };
    const got = collectData({ repo: 'o/r', maxPrs: 100, runsPerPr: 50, startMs: START_MS, endMs: END_MS }, exec);
    const runCalls = calls.filter((c) => c.slice(0, 2).join(' ') === 'run list');
    const branches = runCalls.map((c) => c[c.indexOf('--branch') + 1]).sort();
    // PR14 is outside the window → its head is never scanned.
    assert.deepEqual(branches, ['chore-tidy', 'feat/issue-5-add-foo', 'feat/issue-8-wip', 'fix/issue-7-broken']);
    assert.equal(got.pullRequests.length, 6);
    assert.equal(got.issues.length, 6);
  });

  it('a failed runs fetch yields null (unsampled), not a crash', () => {
    const data = fixtureData();
    const exec = (argv) => {
      const key = argv.slice(0, 2).join(' ');
      if (key === 'pr list') return JSON.stringify([data.pullRequests[0]]);
      if (key === 'issue list') return JSON.stringify([]);
      if (key === 'run list') throw new Error('boom');
      throw new Error('unexpected');
    };
    const got = collectData({ repo: 'o/r', maxPrs: 100, runsPerPr: 50, startMs: START_MS, endMs: END_MS }, exec);
    assert.equal(got.workflowRuns['feat/issue-5-add-foo'], null);
  });
});

// --- CLI -------------------------------------------------------------------------
describe('cli', () => {
  it('--input + --now produces deterministic JSON and --out output', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'slo-'));
    const input = path.join(dir, 'input.json');
    const out = path.join(dir, 'report.json');
    fs.writeFileSync(input, JSON.stringify(fixtureData()));
    const r = main(['--input', input, '--window', '30d', '--now', NOW, '--out', out]);
    assert.equal(r.escalation.rate, 33.3);
    const written = JSON.parse(fs.readFileSync(out, 'utf8'));
    assert.equal(written.window.end, new Date(NOW).toISOString());
    assert.equal(written.time_to_merge_seconds.count, 2);
  });

  it('--format markdown + --github-summary appends the step summary', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'slo-'));
    const input = path.join(dir, 'input.json');
    const summary = path.join(dir, 'SUMMARY.md');
    fs.writeFileSync(input, JSON.stringify(fixtureData()));
    let out = '';
    main(['--input', input, '--now', NOW, '--format', 'markdown', '--github-summary'],
      { stdout: (s) => { out = s; }, env: { GITHUB_STEP_SUMMARY: summary } });
    assert.match(out, /AI-SDLC SLO metrics/);
    assert.match(fs.readFileSync(summary, 'utf8'), /escalation rate/);
  });

  it('--dump-input persists the collected model for replay', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'slo-'));
    const dump = path.join(dir, 'dump.json');
    const data = fixtureData();
    const exec = (argv) => {
      const key = argv.slice(0, 2).join(' ');
      if (key === 'pr list') return JSON.stringify(data.pullRequests);
      if (key === 'issue list') return JSON.stringify(data.issues);
      if (key === 'run list') return JSON.stringify([]);
      throw new Error('unexpected');
    };
    main(['--repo', 'o/r', '--now', NOW, '--dump-input', dump], { exec, stdout: () => {} });
    const saved = JSON.parse(fs.readFileSync(dump, 'utf8'));
    assert.equal(saved.repo, 'o/r');
    assert.equal(saved.pullRequests.length, 6);
  });

  it('rejects a bad --format and a missing repo outside --input', () => {
    assert.throws(() => parseArgs(['--format', 'yaml']), /--format/);
    assert.throws(() => main([], { env: {} }), /--repo is required/);
  });
});
