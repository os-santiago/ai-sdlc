// scripts/dora-metrics.test.js — fixture-driven tests for the DORA metrics
// reporter (issues #101/#102, epic #88). All computation is pure over the
// collected model; `gh` is exercised only through the injectable exec seam.
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
  authorTypeOf,
  buildEpisodes,
  buildRevertMap,
  earliestReadyAt,
  collectData,
  computeDora,
  renderMarkdown,
  githubOutputLines,
  isComputedReport,
  parseArgs,
  main,
} = require('./dora-metrics');

// Fixed window: [2025-05-31, 2025-06-30) — deterministic, no wall clock.
const NOW = '2025-06-30T00:00:00Z';
const END_MS = Date.parse(NOW);
const START_MS = END_MS - 30 * 86400e3;
const FAILURE_WINDOW_MS = 48 * 3600e3;

const pr = (over) => ({
  number: 0,
  title: 't',
  state: 'OPEN',
  headRefName: 'x',
  baseRefName: 'main',
  createdAt: null,
  updatedAt: null,
  closedAt: null,
  mergedAt: null,
  isDraft: false,
  labels: [],
  author: { login: 'alice' },
  body: '',
  mergeCommit: null,
  ...over,
});

const issue = (number, labels = [], createdAt = '2025-06-01T00:00:00Z') => ({
  number,
  title: 'i',
  state: 'open',
  createdAt,
  closedAt: null,
  labels: labels.map((name) => ({ name })),
});

const run = (over) => ({
  databaseId: 0,
  createdAt: null,
  updatedAt: null,
  status: 'completed',
  conclusion: 'success',
  event: 'push',
  headSha: null,
  workflowName: 'ci',
  ...over,
});

// --- reference fixture ---------------------------------------------------------
// Agentic: PR10 clean (ready-label basis), PR11 merged then red main (attributed
// via merge sha, recovered next day), PR12 merged but linked issue holds
// needs-human (non-converging), PR24 agentic by author login only.
// Human:   PR20 reverted by PR21, PR21 is the revert itself, PR22 followed by a
// still-red streak (ongoing), PR23 merged before the window.
// Episodes: shaB → agentic (24h), shaX → unattributed (25h, no merge in the
// 48h attribution window), shaY → human via nearest-preceding-merge (ongoing).
function fixtureData() {
  return {
    repo: 'o/r',
    collectedAt: NOW,
    pullRequests: [
      pr({
        number: 10, title: 'feat: add foo', state: 'MERGED',
        headRefName: 'feat/issue-5-add-foo', author: { login: 'ai-sdlc-runtime[bot]' },
        createdAt: '2025-06-05T00:00:00Z', mergedAt: '2025-06-10T00:00:00Z',
        mergeCommit: { oid: 'shaA' },
      }),
      pr({
        number: 11, title: 'fix: broken', state: 'MERGED',
        headRefName: 'fix/issue-7-broken', author: { login: 'ai-sdlc-runtime[bot]' },
        createdAt: '2025-06-18T00:00:00Z', mergedAt: '2025-06-20T00:00:00Z',
        mergeCommit: { oid: 'shaB' },
      }),
      pr({
        number: 12, title: 'feat: wip', state: 'MERGED',
        headRefName: 'feat/issue-8-wip', author: { login: 'ai-sdlc-runtime[bot]' },
        createdAt: '2025-06-24T00:00:00Z', mergedAt: '2025-06-25T00:00:00Z',
        mergeCommit: { oid: 'shaC' },
      }),
      pr({
        number: 20, title: 'fix: human patch', state: 'MERGED',
        headRefName: 'human-fix', createdAt: '2025-06-03T00:00:00Z',
        mergedAt: '2025-06-05T00:00:00Z', mergeCommit: { oid: 'shaD' },
      }),
      pr({
        number: 21, title: 'Revert "fix: human patch"', state: 'MERGED',
        headRefName: 'revert-20-human-fix', createdAt: '2025-06-07T00:00:00Z',
        mergedAt: '2025-06-08T00:00:00Z', mergeCommit: { oid: 'shaF' },
        body: 'Reverts #20',
      }),
      pr({
        number: 22, title: 'docs: tidy', state: 'MERGED',
        headRefName: 'docs-tidy', author: { login: 'bob' },
        createdAt: '2025-06-27T00:00:00Z', mergedAt: '2025-06-28T00:00:00Z',
        mergeCommit: { oid: 'shaE' },
      }),
      pr({
        number: 23, title: 'old merge', state: 'MERGED',
        headRefName: 'feat/issue-9-old', author: { login: 'ai-sdlc-runtime[bot]' },
        createdAt: '2025-03-20T00:00:00Z', mergedAt: '2025-04-01T00:00:00Z',
        mergeCommit: { oid: 'shaOld' },
      }),
      pr({
        number: 24, title: 'bot work on a plain branch', state: 'MERGED',
        headRefName: 'bot-work', author: { login: 'ai-sdlc-runtime[bot]' },
        createdAt: '2025-06-26T00:00:00Z', mergedAt: '2025-06-27T00:00:00Z',
        mergeCommit: { oid: 'shaG' },
      }),
      pr({ number: 25, title: 'open pr', state: 'OPEN', headRefName: 'feat/issue-30-wip' }),
    ],
    issues: [
      issue(5),
      issue(7, [], '2025-06-10T00:00:00Z'),
      issue(8, ['needs-human'], '2025-06-20T00:00:00Z'),
      issue(9, [], '2025-03-01T00:00:00Z'),
    ],
    mainRuns: [
      run({ databaseId: 1, headSha: 'shaA', conclusion: 'success',
        createdAt: '2025-06-10T00:30:00Z', updatedAt: '2025-06-10T01:00:00Z' }),
      run({ databaseId: 2, headSha: 'shaX', conclusion: 'failure',
        createdAt: '2025-06-15T00:00:00Z', updatedAt: '2025-06-15T01:00:00Z' }),
      run({ databaseId: 3, headSha: 'shaX', conclusion: 'success',
        createdAt: '2025-06-16T00:00:00Z', updatedAt: '2025-06-16T02:00:00Z' }),
      run({ databaseId: 4, headSha: 'shaB', conclusion: 'failure',
        createdAt: '2025-06-21T00:00:00Z', updatedAt: '2025-06-21T00:30:00Z' }),
      run({ databaseId: 5, headSha: 'shaB', conclusion: 'success',
        createdAt: '2025-06-22T00:00:00Z', updatedAt: '2025-06-22T00:30:00Z' }),
      run({ databaseId: 6, headSha: 'shaC', conclusion: 'success',
        createdAt: '2025-06-25T00:30:00Z', updatedAt: '2025-06-25T01:00:00Z' }),
      run({ databaseId: 7, headSha: 'shaY', conclusion: 'failure',
        createdAt: '2025-06-29T00:00:00Z', updatedAt: '2025-06-29T01:00:00Z' }),
      // pending run — never signal
      run({ databaseId: 8, headSha: 'shaY', status: 'in_progress', conclusion: null,
        createdAt: '2025-06-29T12:00:00Z', updatedAt: '2025-06-29T12:00:00Z' }),
    ],
    releases: [
      { tagName: 'v1.2.0', name: 'v1.2.0', isDraft: false, isPrerelease: false,
        createdAt: '2025-06-02T00:00:00Z', publishedAt: '2025-06-02T00:00:00Z' },
      { tagName: 'v1.3.0-rc1', name: 'v1.3.0-rc1', isDraft: false, isPrerelease: true,
        createdAt: '2025-06-20T00:00:00Z', publishedAt: '2025-06-20T00:00:00Z' },
      { tagName: 'v1.3.0-draft', name: 'draft', isDraft: true, isPrerelease: false,
        createdAt: '2025-06-21T00:00:00Z', publishedAt: '2025-06-21T00:00:00Z' },
      { tagName: 'v1.0.0', name: 'v1.0.0', isDraft: false, isPrerelease: false,
        createdAt: '2025-01-01T00:00:00Z', publishedAt: '2025-01-01T00:00:00Z' },
    ],
    readyAt: {
      'issue:5': '2025-06-01T00:00:00Z',
      'issue:7': '2025-06-15T00:00:00Z',
      'issue:8': null, // timeline fetched — label never applied
      'pr:20': null,
      'pr:21': null,
      'pr:22': null,
      'pr:24': null,
    },
    timelineErrors: [],
  };
}

const OPTS = {
  startMs: START_MS,
  endMs: END_MS,
  windowLabel: '30d',
  base: 'main',
  failureWindowMs: FAILURE_WINDOW_MS,
  gateWorkflows: [],
  agenticAuthors: ['ai-sdlc-runtime[bot]'],
};
const compute = (data) => computeDora(data, OPTS);

// --- helpers ----------------------------------------------------------------------
describe('parseDuration', () => {
  it('parses m/h/d suffixes and falls back on garbage', () => {
    assert.equal(parseDuration('30d'), 30 * 86400e3);
    assert.equal(parseDuration('48h'), 48 * 3600e3);
    assert.equal(parseDuration('90m'), 90 * 60e3);
    assert.equal(parseDuration('bogus', 42), 42);
  });
});

describe('author type classification', () => {
  const agentic = new Set(['ai-sdlc-runtime[bot]']);
  it('agentic = pipeline branch, ai-sdlc label, or known bot author', () => {
    assert.equal(authorTypeOf(pr({ headRefName: 'feat/issue-5-x' }), agentic), 'agentic');
    assert.equal(authorTypeOf(pr({ headRefName: 'any', labels: [{ name: 'ai-sdlc' }] }), agentic), 'agentic');
    assert.equal(authorTypeOf(pr({ headRefName: 'bot-work', author: { login: 'ai-sdlc-runtime[bot]' } }), agentic), 'agentic');
    assert.equal(authorTypeOf(pr({ headRefName: 'docs-x', author: { login: 'alice' } }), agentic), 'human');
  });

  it('pipeline surface matches the sweep (branch or label)', () => {
    assert.ok(isPipelinePr(pr({ headRefName: 'fix/issue-9' })));
    assert.ok(isPipelinePr(pr({ headRefName: 'x', labels: [{ name: 'ai-sdlc' }] })));
    assert.ok(!isPipelinePr(pr({ headRefName: 'docs-misc' })));
  });

  it('links the issue from the branch, then closing keywords', () => {
    assert.equal(linkedIssueNumber(pr({ headRefName: 'feat/issue-5-x', body: 'Closes #99' })), 5);
    assert.equal(linkedIssueNumber(pr({ headRefName: 'human-branch', body: 'Refs #3. Closes #7' })), 7);
    assert.equal(linkedIssueNumber(pr({ headRefName: 'human-branch', body: 'Refs #3' })), null);
  });
});

describe('buildEpisodes', () => {
  it('groups red streaks into episodes closed by the next green', () => {
    const eps = buildEpisodes([
      run({ databaseId: 1, conclusion: 'failure', createdAt: '2025-06-10T00:00:00Z', updatedAt: '2025-06-10T01:00:00Z' }),
      run({ databaseId: 2, conclusion: 'failure', createdAt: '2025-06-11T00:00:00Z', updatedAt: '2025-06-11T01:00:00Z' }),
      run({ databaseId: 3, conclusion: 'success', createdAt: '2025-06-12T00:00:00Z', updatedAt: '2025-06-12T02:00:00Z' }),
      run({ databaseId: 4, conclusion: 'failure', createdAt: '2025-06-20T00:00:00Z', updatedAt: '2025-06-20T01:00:00Z' }),
    ], { startMs: START_MS, endMs: END_MS, gateWorkflows: [] });
    assert.equal(eps.length, 2);
    assert.equal(eps[0].restoreMs, (2 * 86400 + 3600) * 1000); // 10th 01:00 → 12th 02:00
    assert.equal(eps[0].ongoing, false);
    assert.equal(eps[1].ongoing, true);
    assert.equal(eps[1].restoreMs, null);
  });

  it('orders runs chronologically even when listed newest-first', () => {
    const eps = buildEpisodes([
      run({ databaseId: 3, conclusion: 'success', createdAt: '2025-06-12T00:00:00Z', updatedAt: '2025-06-12T01:00:00Z' }),
      run({ databaseId: 1, conclusion: 'failure', createdAt: '2025-06-10T00:00:00Z', updatedAt: '2025-06-10T01:00:00Z' }),
    ], { startMs: START_MS, endMs: END_MS, gateWorkflows: [] });
    assert.equal(eps.length, 1);
    // failure 06-10 01:00 → success 06-12 01:00 regardless of list order.
    assert.equal(eps[0].restoreMs, 2 * 86400e3);
  });

  it('filters to gate workflows and skips non-completed runs', () => {
    const eps = buildEpisodes([
      run({ databaseId: 1, workflowName: 'lint', conclusion: 'failure', createdAt: '2025-06-10T00:00:00Z' }),
      run({ databaseId: 2, workflowName: 'ci', conclusion: 'success', createdAt: '2025-06-11T00:00:00Z' }),
      run({ databaseId: 3, workflowName: 'ci', status: 'in_progress', conclusion: null, createdAt: '2025-06-09T00:00:00Z' }),
    ], { startMs: START_MS, endMs: END_MS, gateWorkflows: ['ci'] });
    assert.equal(eps.length, 0); // lint failure is not a gate workflow
  });

  it('drops episodes fully outside the window', () => {
    const eps = buildEpisodes([
      run({ databaseId: 1, conclusion: 'failure', createdAt: '2025-01-01T00:00:00Z', updatedAt: '2025-01-01T01:00:00Z' }),
      run({ databaseId: 2, conclusion: 'success', createdAt: '2025-01-02T00:00:00Z', updatedAt: '2025-01-02T01:00:00Z' }),
    ], { startMs: START_MS, endMs: END_MS, gateWorkflows: [] });
    assert.equal(eps.length, 0);
  });
});

describe('buildRevertMap', () => {
  it('binds merged reverts to their targets via branch, title and body', () => {
    const map = buildRevertMap([
      pr({ number: 20, state: 'MERGED', mergedAt: '2025-06-05T00:00:00Z' }),
      pr({ number: 21, state: 'MERGED', headRefName: 'revert-20-human-fix',
        title: 'Revert "fix"', mergedAt: '2025-06-08T00:00:00Z', body: 'Reverts #20' }),
      pr({ number: 30, state: 'OPEN', headRefName: 'revert-22-x', title: 'Revert "x"' }), // not merged — no binding
      pr({ number: 31, state: 'MERGED', title: 'normal merge', mergedAt: '2025-06-09T00:00:00Z' }),
    ]);
    assert.equal(map.get(20).pr, 21);
    assert.equal(map.has(22), false);
    assert.equal(map.has(31), false);
  });
});

// --- computeDora on the fixture ----------------------------------------------------
describe('computeDora — populated window', () => {
  const report = compute(fixtureData());

  it('reports the four keys split by authorType', () => {
    for (const k of ['deployments', 'leadTimeHours', 'changeFailureRate', 'mttrHours']) {
      assert.ok(report[k], `missing ${k}`);
      assert.ok(report[k].agentic !== undefined && report[k].human !== undefined, `${k} not split by authorType`);
    }
    assert.equal(report.version, 1);
  });

  it('deployment frequency counts merges to base in the window', () => {
    const d = report.deployments;
    assert.equal(d.agentic.count, 4); // PR10, PR11, PR12, PR24
    assert.equal(d.human.count, 3); // PR20, PR21 (the revert), PR22
    assert.equal(d.total.count, 7); // PR23 out of window, PR25 unmerged
    assert.equal(d.releaseTags, 2); // draft + out-of-window excluded
    assert.ok(d.total.perWeek > 0);
  });

  it('lead time uses ready-to-implement label ts, falling back to PR creation', () => {
    const lt = report.leadTimeHours;
    const s10 = lt.agentic.samples.find((s) => s.pr === 10);
    const s11 = lt.agentic.samples.find((s) => s.pr === 11);
    const s12 = lt.agentic.samples.find((s) => s.pr === 12);
    assert.equal(s10.hours, 216); // 2025-06-01 label → 2025-06-10 merge
    assert.equal(s10.basis, 'ready_label');
    assert.equal(s11.hours, 120);
    assert.equal(s12.hours, 24); // label never applied → pr_created basis
    assert.equal(s12.basis, 'pr_created');
    assert.equal(lt.agentic.count, 4);
    assert.equal(lt.agentic.median, 72); // (24 + 120) / 2
    assert.equal(lt.agentic.mean, 96);
    assert.equal(lt.agentic.p90, 216);
    assert.equal(lt.human.count, 3);
    assert.equal(lt.human.median, 24); // {48, 24, 24}
  });

  it('change failure rate = failed merges / total merges per authorType', () => {
    const cfr = report.changeFailureRate;
    assert.equal(cfr.agentic.merges, 4);
    assert.equal(cfr.agentic.failed, 2); // PR11 red_main, PR12 non_converging
    assert.equal(cfr.agentic.rate, 50);
    const f11 = cfr.agentic.failures.find((f) => f.pr === 11);
    const f12 = cfr.agentic.failures.find((f) => f.pr === 12);
    assert.deepEqual(f11.signals, ['red_main']);
    assert.deepEqual(f12.signals, ['non_converging_repair']);

    assert.equal(cfr.human.merges, 3);
    assert.equal(cfr.human.failed, 2); // PR20 revert, PR22 red_main + ongoing
    assert.equal(cfr.human.rate, 66.7);
    const f20 = cfr.human.failures.find((f) => f.pr === 20);
    assert.deepEqual(f20.signals, ['revert']);
    assert.equal(f20.revertedBy, 21);
    const f22 = cfr.human.failures.find((f) => f.pr === 22);
    assert.deepEqual(f22.signals, ['red_main', 'non_converging_repair']);

    assert.equal(cfr.all.merges, 7);
    assert.equal(cfr.all.failed, 4);
    assert.equal(cfr.all.rate, 57.1);
  });

  it('MTTR measures base red→green attributed to the introducing merge', () => {
    const mt = report.mttrHours;
    assert.equal(mt.all.episodes, 3); // shaX, shaB, shaY
    assert.equal(mt.all.recovered, 2);
    assert.equal(mt.all.ongoing, 1);

    assert.equal(mt.agentic.episodes, 1); // shaB → PR11 merge sha
    assert.equal(mt.agentic.mean, 24);
    const epB = mt.agentic.samples[0];
    assert.equal(epB.causedBy, 11);
    assert.equal(epB.hours, 24);

    assert.equal(mt.human.episodes, 1); // shaY → nearest merge PR22 inside failure window
    assert.equal(mt.human.ongoing, 1);
    assert.equal(mt.human.mean, null); // nothing recovered yet
    assert.equal(mt.human.samples[0].causedBy, 22);

    assert.equal(mt.unattributed.episodes, 1); // shaX — no merge within 48h
    assert.equal(mt.unattributed.mean, 25);

    assert.equal(mt.all.mean, 24.5); // (24 + 25) / 2
  });
});

// --- edge cases ---------------------------------------------------------------------
describe('computeDora — empty window', () => {
  const empty = { repo: 'o/r', pullRequests: [], issues: [], mainRuns: [], releases: [], readyAt: {} };
  const report = compute(empty);

  it('produces a defined report with null aggregates, never NaN', () => {
    assert.equal(report.deployments.total.count, 0);
    assert.equal(report.leadTimeHours.agentic.count, 0);
    assert.equal(report.leadTimeHours.agentic.median, null);
    assert.equal(report.changeFailureRate.all.rate, null);
    assert.equal(report.mttrHours.all.mean, null);
    assert.equal(report.mttrHours.all.episodes, 0);
  });

  it('renders n/a in markdown, never NaN', () => {
    const md = renderMarkdown(report);
    assert.doesNotMatch(md, /NaN/);
    assert.match(md, /Change Failure Rate \| n\/a/);
  });
});

describe('computeDora — missing fields', () => {
  it('does not crash on sparse records', () => {
    const data = {
      repo: 'o/r',
      pullRequests: [
        { number: 1, state: 'MERGED', mergedAt: '2025-06-10T00:00:00Z' }, // no head/sha/labels/author
        { number: 2, mergedAt: '2025-06-11T00:00:00Z' }, // no state either
      ],
      issues: null,
      mainRuns: [
        { conclusion: 'failure' }, // undated — skipped by episode builder
        { createdAt: '2025-06-12T00:00:00Z', status: 'completed', conclusion: 'failure', headSha: 'zzz' },
      ],
      releases: null, // fetch failed
      readyAt: {},
    };
    const report = compute(data);
    assert.equal(report.deployments.total.count, 2);
    assert.equal(report.deployments.releaseTags, null);
    assert.equal(report.leadTimeHours.all.count, 0); // no createdAt, no label ts
    assert.equal(report.mttrHours.all.episodes, 1);
    // 'zzz' sha matches nothing — nearest-preceding merge inside the 48h
    // failure window attributes the episode to PR2 (human).
    assert.equal(report.mttrHours.human.episodes, 1);
    assert.equal(report.mttrHours.human.samples[0].causedBy, 2);
    assert.equal(report.mttrHours.unattributed.episodes, 0);
  });

  it('negative lead times (clock skew) are dropped, not emitted', () => {
    const data = {
      repo: 'o/r',
      pullRequests: [pr({
        number: 1, state: 'MERGED', headRefName: 'feat/issue-5-x',
        createdAt: '2025-06-20T00:00:00Z', mergedAt: '2025-06-10T00:00:00Z',
      })],
      issues: [], mainRuns: [], releases: [],
      readyAt: { 'issue:5': '2025-06-25T00:00:00Z' }, // label after merge → negative
    };
    const report = compute(data);
    assert.equal(report.leadTimeHours.all.count, 0);
  });
});

// --- renderers ----------------------------------------------------------------------
describe('renderMarkdown', () => {
  const report = compute(fixtureData());
  const md = renderMarkdown(report);

  it('carries a heading per DORA metric plus the comparison table', () => {
    assert.match(md, /## Deployment Frequency/);
    assert.match(md, /## Lead Time for Changes/);
    assert.match(md, /## Change Failure Rate/);
    assert.match(md, /## MTTR/);
    assert.match(md, /\| Metric \| Agentic \| Human \| All \|/);
  });

  it('includes the ISO generation timestamp', () => {
    assert.match(md, /_Generated 2025-06-30T00:00:00\.000Z/);
  });

  it('renders per-authorType numbers in the comparison table', () => {
    assert.match(md, /\| Deployment frequency \| 4 merges · [0-9.]+\/wk \| 3 merges · [0-9.]+\/wk \| 7 merges/);
    assert.match(md, /\| Change Failure Rate \| 50% \(2\/4\) \| 66\.7% \(2\/3\) \| 57\.1% \(4\/7\)/);
  });
});

describe('githubOutputLines', () => {
  it('emits flat dora_* keys', () => {
    const lines = githubOutputLines(compute(fixtureData()));
    const joined = lines.join('\n');
    assert.match(joined, /dora_deployments_total=7/);
    assert.match(joined, /dora_deployments_agentic=4/);
    assert.match(joined, /dora_lead_time_median_hours_agentic=72/);
    assert.match(joined, /dora_change_failure_rate=57\.1/);
    assert.match(joined, /dora_mttr_episodes=3/);
    assert.match(joined, /dora_mttr_ongoing=1/);
    assert.match(joined, /dora_release_tags=2/);
  });
});

// --- collection (mocked gh) ------------------------------------------------------------
describe('earliestReadyAt', () => {
  it('parses JSONL timestamps (gh --jq per-page output) and JSON arrays', () => {
    assert.equal(
      earliestReadyAt('"2025-06-05T00:00:00Z"\n"2025-06-01T00:00:00Z"\n'),
      '2025-06-01T00:00:00.000Z',
    );
    assert.equal(
      earliestReadyAt('["2025-06-05T00:00:00Z","2025-06-01T00:00:00Z"]'),
      '2025-06-01T00:00:00.000Z',
    );
    assert.equal(earliestReadyAt('[]'), null);
    assert.equal(earliestReadyAt(''), null);
    assert.equal(earliestReadyAt('null\nnull'), null);
  });
});

describe('collectData', () => {
  function mockExec(data) {
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      const key = argv.slice(0, 2).join(' ');
      if (key === 'pr list') return JSON.stringify(data.pullRequests);
      if (key === 'issue list') return JSON.stringify(data.issues);
      if (key === 'run list') return JSON.stringify(data.mainRuns);
      if (key === 'release list') return JSON.stringify(data.releases);
      if (argv[0] === 'api') {
        const m = argv[1].match(/issues\/(\d+)\/timeline/);
        const n = m ? m[1] : null;
        const labels = { 5: ['2025-06-01T00:00:00Z'], 7: ['2025-06-15T00:00:00Z'] };
        return JSON.stringify(labels[n] || []);
      }
      throw new Error(`unexpected gh call: ${argv.join(' ')}`);
    };
    return { calls, exec };
  }

  it('fetches each list once and timelines only for merged-in-window PRs', () => {
    const data = fixtureData();
    const { calls, exec } = mockExec(data);
    const got = collectData({
      repo: 'o/r', base: 'main', maxPrs: 200, runsLimit: 200, releasesLimit: 50,
      startMs: START_MS, endMs: END_MS,
    }, exec);
    assert.equal(got.pullRequests.length, 9);
    const apiCalls = calls.filter((c) => c[0] === 'api');
    const targets = apiCalls.map((c) => c[1].match(/issues\/(\d+)\/timeline/)[1]).sort();
    // linked issues for PR10/11/12; PRs themselves when no linked issue;
    // PR23 out-of-window and PR25 unmerged never fetched.
    assert.deepEqual(targets, ['20', '21', '22', '24', '5', '7', '8']);
    assert.equal(Date.parse(got.readyAt['issue:5']), Date.parse('2025-06-01T00:00:00Z'));
    assert.equal(Date.parse(got.readyAt['issue:7']), Date.parse('2025-06-15T00:00:00Z'));
    assert.equal(got.readyAt['issue:8'], null);
  });

  it('a failed timeline fetch lands as null, never a crash', () => {
    const data = fixtureData();
    const exec = (argv) => {
      const key = argv.slice(0, 2).join(' ');
      if (key === 'pr list') return JSON.stringify([data.pullRequests[0]]);
      if (key === 'issue list') return JSON.stringify([]);
      if (key === 'run list') return JSON.stringify([]);
      if (key === 'release list') return JSON.stringify([]);
      if (argv[0] === 'api') throw new Error('boom');
      throw new Error('unexpected');
    };
    const got = collectData({
      repo: 'o/r', base: 'main', maxPrs: 200, runsLimit: 200, releasesLimit: 50,
      startMs: START_MS, endMs: END_MS,
    }, exec);
    assert.equal(got.readyAt['issue:5'], null);
  });

  it('a failed release list lands as null (secondary signal)', () => {
    const data = fixtureData();
    const exec = (argv) => {
      const key = argv.slice(0, 2).join(' ');
      if (key === 'pr list') return JSON.stringify([]);
      if (key === 'issue list') return JSON.stringify([]);
      if (key === 'run list') return JSON.stringify([]);
      if (key === 'release list') throw new Error('no releases');
      throw new Error('unexpected');
    };
    const got = collectData({
      repo: 'o/r', base: 'main', maxPrs: 200, runsLimit: 200, releasesLimit: 50,
      startMs: START_MS, endMs: END_MS,
    }, exec);
    assert.equal(got.releases, null);
  });
});

// --- CLI --------------------------------------------------------------------------------
describe('cli', () => {
  it('--input + --now produces deterministic JSON and --out output', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dora-'));
    const input = path.join(dir, 'input.json');
    const out = path.join(dir, 'dora-metrics.json');
    fs.writeFileSync(input, JSON.stringify(fixtureData()));
    const r = main(['--input', input, '--window', '30d', '--now', NOW, '--out', out]);
    assert.equal(r.deployments.total.count, 7);
    const written = JSON.parse(fs.readFileSync(out, 'utf8'));
    assert.equal(written.window.end, new Date(NOW).toISOString());
    assert.equal(written.changeFailureRate.all.rate, 57.1);
    for (const k of ['deployments', 'leadTimeHours', 'changeFailureRate', 'mttrHours']) {
      assert.ok(written[k]);
    }
  });

  it('--report writes docs/metrics/dora.md and creates missing directories', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dora-'));
    const input = path.join(dir, 'input.json');
    const mdPath = path.join(dir, 'docs', 'metrics', 'dora.md');
    fs.writeFileSync(input, JSON.stringify(fixtureData()));
    main(['--input', input, '--now', NOW, '--report', mdPath], { stdout: () => {} });
    const md = fs.readFileSync(mdPath, 'utf8');
    assert.match(md, /## Deployment Frequency/);
    assert.match(md, /## Lead Time for Changes/);
    assert.match(md, /## Change Failure Rate/);
    assert.match(md, /## MTTR/);
    assert.match(md, /_Generated 2025-06-30T00:00:00\.000Z/);
  });

  it('--github-output appends flat keys to $GITHUB_OUTPUT', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dora-'));
    const input = path.join(dir, 'input.json');
    const gout = path.join(dir, 'github-output.txt');
    fs.writeFileSync(input, JSON.stringify(fixtureData()));
    main(['--input', input, '--now', NOW, '--github-output'],
      { stdout: () => {}, env: { GITHUB_OUTPUT: gout } });
    const lines = fs.readFileSync(gout, 'utf8');
    assert.match(lines, /dora_deployments_total=7/);
    assert.match(lines, /dora_change_failure_rate_agentic=50/);
  });

  it('--github-summary appends markdown to $GITHUB_STEP_SUMMARY', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dora-'));
    const input = path.join(dir, 'input.json');
    const summary = path.join(dir, 'SUMMARY.md');
    fs.writeFileSync(input, JSON.stringify(fixtureData()));
    main(['--input', input, '--now', NOW, '--github-summary'],
      { stdout: () => {}, env: { GITHUB_STEP_SUMMARY: summary } });
    assert.match(fs.readFileSync(summary, 'utf8'), /DORA metrics — o\/r/);
  });

  it('--input on a computed report renders markdown directly (issue #102 hand-off)', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dora-'));
    const input = path.join(dir, 'input.json');
    const reportJson = path.join(dir, 'dora-metrics.json');
    fs.writeFileSync(input, JSON.stringify(fixtureData()));
    main(['--input', input, '--now', NOW, '--out', reportJson]);
    let md = '';
    main(['--input', reportJson, '--format', 'markdown'], { stdout: (s) => { md = s; } });
    assert.match(md, /## Change Failure Rate/);
    assert.ok(isComputedReport(JSON.parse(fs.readFileSync(reportJson, 'utf8'))));
    assert.ok(!isComputedReport(fixtureData()));
  });

  it('--dump-input persists the collected model for replay', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dora-'));
    const dump = path.join(dir, 'dump.json');
    const data = fixtureData();
    const exec = (argv) => {
      const key = argv.slice(0, 2).join(' ');
      if (key === 'pr list') return JSON.stringify(data.pullRequests);
      if (key === 'issue list') return JSON.stringify(data.issues);
      if (key === 'run list') return JSON.stringify(data.mainRuns);
      if (key === 'release list') return JSON.stringify(data.releases);
      if (argv[0] === 'api') return '[]';
      throw new Error('unexpected');
    };
    main(['--repo', 'o/r', '--now', NOW, '--dump-input', dump], { exec, stdout: () => {} });
    const saved = JSON.parse(fs.readFileSync(dump, 'utf8'));
    assert.equal(saved.repo, 'o/r');
    assert.equal(saved.pullRequests.length, 9);
    assert.ok(saved.readyAt);
  });

  it('rejects a bad --format and a missing repo outside --input', () => {
    assert.throws(() => parseArgs(['--format', 'yaml']), /--format/);
    assert.throws(() => main([], { env: {} }), /--repo is required/);
  });
});
