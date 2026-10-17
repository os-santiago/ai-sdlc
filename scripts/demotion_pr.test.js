// scripts/demotion_pr.test.js — unit + mocked-API tests for the
// escalation-driven autonomy demotion PR generator (issue #209).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  LADDER,
  PR_TITLE_PREFIX,
  prevLevel,
  branchNameFor,
  buildPrTitle,
  readDemotionConfig,
  evaluateDemotionEvidence,
  buildPrBody,
  planDemotion,
  openDemotionPr,
  applyDemotion,
  main,
} = require('./demotion_pr');
const promotion = require('./promotion_pr');

const CONTRACT = [
  'spec: "1.0.0"',
  '',
  'autonomy:',
  '  level: auto-merge-low        # ceiling for this repo',
  '  merge_requires_checks: true',
  '  freeze_env: AI_SDLC_FREEZE',
  '',
  'demotion:',
  '  escalation_threshold: 1',
  '  window: 30',
  '',
  'triggers:',
  '  issue_label: ready-to-implement',
  '',
].join('\n');

// Merge record helpers — newest-first, the `gh pr list --state merged`
// order. Escalations are marked by the `needs-human` label binding
// (escalation.label) or an explicit `escalated: true`.
const clean = (n) => ({ number: n, mergedAt: `2026-01-0${n}T00:00:00Z`, labels: [] });
const escalated = (n) => ({ number: n, mergedAt: `2026-01-0${n}T00:00:00Z`, labels: [{ name: 'needs-human' }] });

// --- Ladder ------------------------------------------------------------------
describe('prevLevel', () => {
  it('walks the ladder in spec order', () => {
    for (let i = 0; i < LADDER.length; i += 1) {
      assert.equal(prevLevel(LADDER[i]), LADDER[i - 1] ?? null);
    }
  });

  it('auto-merge-low demotes to auto-pr', () => {
    assert.equal(prevLevel('auto-merge-low'), 'auto-pr');
  });

  it('returns null at the floor (shadow)', () => {
    assert.equal(prevLevel('shadow'), null);
  });

  it('rejects an unknown level', () => {
    assert.throws(() => prevLevel('yolo'), /unknown autonomy\.level/);
  });
});

// --- Contract demotion block ---------------------------------------------------
describe('readDemotionConfig', () => {
  it('reads escalation_threshold and window from the contract', () => {
    assert.deepEqual(readDemotionConfig(CONTRACT), { escalation_threshold: 1, window: 30 });
  });

  it('falls back to schema defaults when the demotion block is absent', () => {
    assert.deepEqual(
      readDemotionConfig('autonomy:\n  level: auto-pr\n'),
      { escalation_threshold: 1, window: 30 },
    );
  });

  it('falls back per-key when only one field is set', () => {
    const c = 'demotion:\n  escalation_threshold: 3\n';
    assert.deepEqual(readDemotionConfig(c), { escalation_threshold: 3, window: 30 });
  });

  it('does not bleed into the next top-level block', () => {
    const c = 'demotion:\n  window: 12\npromotion:\n  window: 99\n';
    assert.equal(readDemotionConfig(c).window, 12);
  });

  it('rejects a non-integer threshold', () => {
    assert.throws(
      () => readDemotionConfig('demotion:\n  escalation_threshold: soon\n'),
      /unparseable demotion\.escalation_threshold/,
    );
  });
});

// --- Escalation evaluation -----------------------------------------------------
describe('evaluateDemotionEvidence', () => {
  it('triggers when escalations exceed the threshold', () => {
    const r = evaluateDemotionEvidence(
      [escalated(5), escalated(4), clean(3)],
      { window: 30, threshold: 1 },
    );
    assert.equal(r.triggered, true);
    assert.equal(r.escalationCount, 2);
    assert.equal(r.examined, 3);
    assert.ok(r.reasons[0].includes('exceeds demotion.escalation_threshold=1'));
  });

  it('does not trigger below the threshold', () => {
    const r = evaluateDemotionEvidence([clean(3), clean(2)], { window: 30, threshold: 1 });
    assert.equal(r.triggered, false);
    assert.equal(r.escalationCount, 0);
    assert.ok(r.reasons[0].includes('does not exceed'));
  });

  it('boundary: count == threshold does NOT trigger (strictly-greater bound)', () => {
    const r = evaluateDemotionEvidence(
      [escalated(9), clean(8), clean(7)],
      { window: 30, threshold: 1 },
    );
    assert.equal(r.triggered, false, 'threshold is a strict > bound');
    assert.equal(r.escalationCount, 1);
  });

  it('boundary: count == threshold + 1 triggers', () => {
    const r = evaluateDemotionEvidence(
      [escalated(9), escalated(8), clean(7)],
      { window: 3, threshold: 1 },
    );
    assert.equal(r.triggered, true);
  });

  it('scopes the count to the rolling window — old escalations do not count', () => {
    // Window of 2 over newest-first records: the escalated PR at index 2
    // is outside the window.
    const r = evaluateDemotionEvidence(
      [clean(9), clean(8), escalated(7)],
      { window: 2, threshold: 0 },
    );
    assert.equal(r.triggered, false);
    assert.equal(r.examined, 2);
    assert.equal(r.escalationCount, 0);
  });

  it('honors an explicit escalated flag and extra escalation records', () => {
    const r = evaluateDemotionEvidence(
      [{ number: 4, mergedAt: 'x', labels: [], escalated: true }],
      { window: 30, threshold: 0, escalations: [{ number: 77 }] },
    );
    assert.equal(r.escalationCount, 2);
    assert.equal(r.triggered, true);
    assert.deepEqual(r.escalations.map((e) => e.number), [4, 77]);
  });

  it('supports custom escalation-label bindings', () => {
    const r = evaluateDemotionEvidence(
      [{ number: 4, labels: [{ name: 'scc:failed' }] }],
      { window: 30, threshold: 0, escalationLabels: ['scc:failed'] },
    );
    assert.equal(r.triggered, true);
  });

  it('empty window does not trigger', () => {
    const r = evaluateDemotionEvidence([], { window: 30, threshold: 0 });
    assert.equal(r.triggered, false);
    assert.ok(r.reasons[0].includes('empty window'));
  });
});

// --- Plan ----------------------------------------------------------------------
describe('planDemotion', () => {
  const TRIGGERED = evaluateDemotionEvidence([escalated(1), escalated(2)], { window: 30, threshold: 1 });

  it('builds the conventional title, single-field diff and evidence body', () => {
    const p = planDemotion({ contractText: CONTRACT, evidence: TRIGGERED });
    assert.equal(p.demotable, true);
    assert.equal(p.current, 'auto-merge-low');
    assert.equal(p.next, 'auto-pr');
    // AC: conventional-commit PR title.
    assert.equal(p.title, 'fix(demotion): lower autonomy level to auto-pr');
    assert.equal(p.branch, 'fix/demotion-autonomy-auto-pr');
    // AC: body summarizes the evidence and names the crossed bound.
    assert.ok(p.body.includes('`auto-merge-low` → `auto-pr`'));
    assert.ok(p.body.includes('demotion.escalation_threshold'));
    assert.ok(p.body.includes('escalations_in_window'));
    assert.ok(p.body.includes('#1'));
    // AC: diff changes only autonomy.level.
    assert.equal(promotion.readAutonomyLevel(p.contract).level, 'auto-pr');
    const diffs = p.contract.split('\n').filter((l, i) => l !== CONTRACT.split('\n')[i]);
    assert.equal(diffs.length, 1);
    assert.ok(diffs[0].startsWith('  level: auto-pr'));
  });

  it('triggered:false is a clean no-op verdict, not an error', () => {
    const p = planDemotion({
      contractText: CONTRACT,
      evidence: { triggered: false, reasons: ['0 escalation(s)'] },
    });
    assert.equal(p.demotable, false);
    assert.ok(p.reason.includes('not crossed'));
  });

  it('reports not-demotable at the floor instead of throwing', () => {
    const c = CONTRACT.replace('auto-merge-low', 'shadow');
    const p = planDemotion({ contractText: c, evidence: TRIGGERED });
    assert.equal(p.demotable, false);
    assert.ok(p.reason.includes('minimum'));
  });

  it('caller-asserted evidence without a triggered flag still files', () => {
    const p = planDemotion({ contractText: CONTRACT, evidence: { summary: 'human says so' } });
    assert.equal(p.demotable, true);
    assert.equal(p.next, 'auto-pr');
  });
});

// --- PR creation (mocked gh layer) ------------------------------------------------
describe('openDemotionPr', () => {
  const base = { repo: 'o/r', base: 'main', head: 'fix/demotion-autonomy-auto-pr' };

  it('dry-run returns the exact gh payload without calling the API', () => {
    let calls = 0;
    const r = openDemotionPr({
      ...base,
      title: 'fix(demotion): lower autonomy level to auto-pr',
      body: 'evidence',
      exec: () => { calls += 1; return ''; },
      dryRun: true,
    });
    assert.equal(calls, 0, 'dry-run must not touch the API');
    assert.equal(r.dryRun, true);
    assert.deepEqual(r.argv.slice(0, 4), ['pr', 'create', '-R', 'o/r']);
    assert.equal(r.argv[r.argv.indexOf('--title') + 1], 'fix(demotion): lower autonomy level to auto-pr');
    assert.equal(r.argv[r.argv.indexOf('--head') + 1], base.head);
  });

  it('apply calls gh pr create once and returns the URL', () => {
    const seen = [];
    const r = openDemotionPr({
      ...base, title: 't', body: 'b',
      exec: (argv) => { seen.push(argv); return 'https://github.com/o/r/pull/9\n'; },
    });
    assert.equal(seen.length, 1);
    assert.equal(r.url, 'https://github.com/o/r/pull/9');
  });
});

// --- Apply orchestration (mocked git + gh) -----------------------------------------
describe('applyDemotion', () => {
  function mockGit() {
    const calls = [];
    return { calls, run: (argv) => { calls.push(argv); return ''; } };
  }

  it('dry-run lists the bounded step plan and PR payload, no side effects', () => {
    const { calls, run } = mockGit();
    const r = applyDemotion({
      repo: 'o/r', base: 'main', branch: 'b',
      contractPath: '.ai-sdlc.yaml', contractText: CONTRACT,
      title: 'fix(demotion): lower autonomy level to auto-pr',
      body: 'b', git: run, exec: () => { throw new Error('must not call gh'); },
      dryRun: true,
    });
    assert.equal(calls.length, 0, 'dry-run must not run git');
    assert.equal(r.pr.dryRun, true);
    assert.deepEqual(r.steps[3], ['commit', '-m', 'fix(demotion): lower autonomy level to auto-pr']);
  });

  it('apply: clean worktree → branch → commit → push → pr create', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'demo-'));
    const cfg = path.join(dir, '.ai-sdlc.yaml');
    fs.writeFileSync(cfg, CONTRACT);
    const { calls, run } = mockGit();
    const gh = [];
    const r = applyDemotion({
      repo: 'o/r', base: 'main', branch: 'fix/demotion-autonomy-auto-pr',
      contractPath: '.ai-sdlc.yaml',
      contractText: promotion.setAutonomyLevel(CONTRACT, 'auto-pr'),
      title: 'fix(demotion): lower autonomy level to auto-pr',
      body: 'evidence', git: run, exec: (argv) => { gh.push(argv); return 'https://x/2'; },
      cwd: dir,
    });
    assert.deepEqual(calls.map((c) => c[0]), ['status', 'checkout', 'add', 'commit', 'push']);
    assert.equal(gh.length, 1);
    assert.equal(gh[0][0], 'pr');
    assert.equal(r.pr.url, 'https://x/2');
    assert.equal(promotion.readAutonomyLevel(fs.readFileSync(cfg, 'utf8')).level, 'auto-pr');
  });

  it('refuses to apply on a dirty worktree', () => {
    const dirty = (argv) => (argv[0] === 'status' ? ' M other.js\n' : '');
    assert.throws(
      () => applyDemotion({
        repo: 'o/r', branch: 'b', contractPath: '.ai-sdlc.yaml', contractText: CONTRACT,
        title: 't', body: 'b', git: dirty, exec: () => '',
      }),
      /worktree is dirty/,
    );
  });
});

// --- CLI dry-run end-to-end ---------------------------------------------------------
describe('cli', () => {
  function withMerges(dir, records) {
    fs.writeFileSync(path.join(dir, '.ai-sdlc.yaml'), CONTRACT);
    const merges = path.join(dir, 'merges.json');
    fs.writeFileSync(merges, JSON.stringify(records));
    return merges;
  }

  it('triggered: files the demotion PR payload in dry-run without touching git or the API', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'demo-cli-'));
    const merges = withMerges(dir, [escalated(5), escalated(4), clean(3)]);
    let out = '';
    const r = main(['--merges', merges, '--repo', 'o/r', '--json'], {
      cwd: dir,
      stdout: (s) => { out = s; },
      git: () => { throw new Error('git must not run in dry-run'); },
      exec: () => { throw new Error('gh must not run in dry-run'); },
    });
    assert.equal(r.dryRun, true);
    assert.equal(r.demoted, false);
    assert.equal(r.title, 'fix(demotion): lower autonomy level to auto-pr');
    assert.ok(r.diff.includes('-  level: auto-merge-low'));
    assert.ok(r.diff.includes('+  level: auto-pr'));
    assert.equal(r.evidence.triggered, true);
    const printed = JSON.parse(out);
    assert.equal(printed.pr.argv[0], 'pr');
    assert.equal(printed.pr.argv[1], 'create');
  });

  it('no-trigger: emits demoted:false with the verdict', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'demo-none-'));
    const merges = withMerges(dir, [clean(3), clean(2)]);
    const r = main(['--merges', merges], { cwd: dir, stdout: () => {} });
    assert.equal(r.demoted, false);
    assert.ok(r.reason.includes('not crossed'));
    assert.equal(r.evidence.triggered, false);
  });

  it('threshold and window default to the contract demotion block', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'demo-cfg-'));
    const merges = withMerges(dir, [escalated(1)]); // 1 == threshold 1 → strict > misses
    const r = main(['--merges', merges], { cwd: dir, stdout: () => {} });
    assert.equal(r.demoted, false);
    assert.equal(r.evidence.threshold, 1); // from `demotion.escalation_threshold` in the contract
    assert.equal(r.evidence.window, 30);
  });

  it('CLI flags override contract thresholds', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'demo-ovr-'));
    const merges = withMerges(dir, [escalated(1)]);
    const r = main(['--merges', merges, '--repo', 'o/r', '--threshold', '0'], { cwd: dir, stdout: () => {} });
    assert.equal(r.demoted, false); // dry-run
    assert.equal(r.title, 'fix(demotion): lower autonomy level to auto-pr');
  });

  it('injected --evidence-json verdict bypasses merge counting', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'demo-ev-'));
    fs.writeFileSync(path.join(dir, '.ai-sdlc.yaml'), CONTRACT);
    const r = main(
      ['--repo', 'o/r', '--evidence-json', JSON.stringify({ triggered: true, escalationCount: 4, window: 30, threshold: 1 })],
      { cwd: dir, stdout: () => {} },
    );
    assert.equal(r.title, 'fix(demotion): lower autonomy level to auto-pr');
    assert.ok(r.body.includes('escalations_in_window'));
  });

  it('exits cleanly at the floor with demoted:false', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'demo-min-'));
    fs.writeFileSync(path.join(dir, '.ai-sdlc.yaml'), CONTRACT.replace('auto-merge-low', 'shadow'));
    const merges = path.join(dir, 'merges.json');
    fs.writeFileSync(merges, JSON.stringify([escalated(1), escalated(2)]));
    const r = main(['--merges', merges], { cwd: dir, stdout: () => {} });
    assert.equal(r.demoted, false);
    assert.ok(r.reason.includes('minimum'));
  });
});

// --- Title helper -------------------------------------------------------------------
describe('buildPrTitle / branchNameFor', () => {
  it('uses the fix(demotion) conventional prefix', () => {
    assert.equal(PR_TITLE_PREFIX, 'fix(demotion): lower autonomy level to ');
    assert.equal(buildPrTitle('suggest'), 'fix(demotion): lower autonomy level to suggest');
    assert.equal(branchNameFor('suggest'), 'fix/demotion-autonomy-suggest');
  });
});
