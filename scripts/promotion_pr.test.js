// scripts/promotion_pr.test.js — unit + mocked-API tests for the
// evidence-driven autonomy promotion PR generator (issue #202).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  LADDER,
  nextLevel,
  branchNameFor,
  buildPrTitle,
  readAutonomyLevel,
  setAutonomyLevel,
  formatEvidence,
  buildPrBody,
  planPromotion,
  diffPreview,
  openPromotionPr,
  applyPromotion,
  main,
} = require('./promotion_pr');

const CONTRACT = [
  'spec: "1.0.0"',
  '',
  'autonomy:',
  '  level: auto-merge-low        # ceiling for this repo',
  '  merge_requires_checks: true',
  '  freeze_env: AI_SDLC_FREEZE',
  '',
  'triggers:',
  '  issue_label: ready-to-implement',
  '',
  'promotion:',
  '  clean_merges_threshold: 3',
  '  window: 30',
  '',
].join('\n');

const EVIDENCE = {
  ready: true,
  window: 30,
  metrics: {
    merge_success_rate: 0.95,
    revert_rate: 0.0,
    escalation_rate: 0.05,
    repair_rounds: 1.1,
  },
  summary: 'Last 30 runs: 19/20 merges clean, zero reverts.',
};

// --- Ladder ------------------------------------------------------------------
describe('nextLevel', () => {
  it('walks the ladder in spec order', () => {
    for (let i = 0; i < LADDER.length; i += 1) {
      assert.equal(nextLevel(LADDER[i]), LADDER[i + 1] ?? null);
    }
  });

  it('auto-merge-low promotes to auto-merge-all', () => {
    assert.equal(nextLevel('auto-merge-low'), 'auto-merge-all');
  });

  it('returns null at the ceiling (auto-deploy)', () => {
    assert.equal(nextLevel('auto-deploy'), null);
  });

  it('rejects an unknown level', () => {
    assert.throws(() => nextLevel('yolo'), /unknown autonomy\.level/);
  });
});

// --- Contract surgery ----------------------------------------------------------
describe('readAutonomyLevel', () => {
  it('reads the configured level', () => {
    assert.deepEqual(readAutonomyLevel(CONTRACT), { level: 'auto-merge-low', explicit: true });
  });

  it('falls back to the schema default (suggest) when level is absent', () => {
    const c = 'autonomy:\n  freeze_env: AI_SDLC_FREEZE\n';
    assert.deepEqual(readAutonomyLevel(c), { level: 'suggest', explicit: false });
  });

  it('handles single- and double-quoted values', () => {
    assert.equal(readAutonomyLevel("autonomy:\n  level: 'auto-pr'\n").level, 'auto-pr');
    assert.equal(readAutonomyLevel('autonomy:\n  level: "shadow"\n').level, 'shadow');
  });

  it('rejects a contract without an autonomy block', () => {
    assert.throws(() => readAutonomyLevel('spec: "1.0.0"\n'), /no top-level `autonomy:`/);
  });

  it('rejects a level value outside the ladder', () => {
    assert.throws(() => readAutonomyLevel('autonomy:\n  level: yolo\n'), /unknown autonomy\.level/);
  });
});

describe('setAutonomyLevel', () => {
  it('changes only the level value — every other line is byte-identical', () => {
    const updated = setAutonomyLevel(CONTRACT, 'auto-merge-all');
    const before = CONTRACT.split('\n');
    const after = updated.split('\n');
    assert.equal(before.length, after.length, 'line count must not change');
    const diffs = after.map((l, i) => [before[i], l]).filter(([b, a]) => b !== a);
    assert.equal(diffs.length, 1, 'exactly one line may differ');
    assert.equal(diffs[0][0], '  level: auto-merge-low        # ceiling for this repo');
    assert.equal(diffs[0][1], '  level: auto-merge-all        # ceiling for this repo');
  });

  it('preserves quoting style and trailing comments', () => {
    const c = 'autonomy:\n  level: "suggest"   # keep me\n';
    assert.equal(setAutonomyLevel(c, 'auto-pr'), 'autonomy:\n  level: "auto-pr"   # keep me\n');
  });

  it('inserts level: when the autonomy block omits it', () => {
    const c = 'autonomy:\n  freeze_env: AI_SDLC_FREEZE\ntriggers:\n  x: 1\n';
    const updated = setAutonomyLevel(c, 'auto-pr');
    assert.equal(
      updated,
      'autonomy:\n  level: auto-pr\n  freeze_env: AI_SDLC_FREEZE\ntriggers:\n  x: 1\n',
    );
  });

  it('refuses to write a value outside the ladder', () => {
    assert.throws(() => setAutonomyLevel(CONTRACT, 'yolo'), /refusing to set unknown/);
  });
});

// --- Evidence rendering ---------------------------------------------------------
describe('formatEvidence', () => {
  it('renders summary + metrics table + scalar fields', () => {
    const md = formatEvidence(EVIDENCE);
    assert.ok(md.includes('19/20 merges clean'));
    assert.ok(md.includes('| `merge_success_rate` | 0.95 |'));
    assert.ok(md.includes('**window**: 30'));
  });

  it('renders a free-form string verbatim', () => {
    assert.equal(formatEvidence('all green'), 'all green');
  });

  it('degrades gracefully on null evidence', () => {
    assert.equal(formatEvidence(null), '_No evidence payload supplied._');
  });
});

// --- Plan ----------------------------------------------------------------------
describe('planPromotion', () => {
  it('builds the conventional-commit title and evidence body', () => {
    const p = planPromotion({ contractText: CONTRACT, evidence: EVIDENCE });
    assert.equal(p.promotable, true);
    assert.equal(p.current, 'auto-merge-low');
    assert.equal(p.next, 'auto-merge-all');
    // AC: conventional-commit PR title.
    assert.equal(p.title, 'feat(promotion): bump autonomy level to auto-merge-all');
    // AC: body includes the evidence summary.
    assert.ok(p.body.includes('19/20 merges clean'));
    assert.ok(p.body.includes('merge_success_rate'));
    assert.ok(p.body.includes('`auto-merge-low` → `auto-merge-all`'));
    // AC: diff changes only autonomy.level.
    assert.equal(readAutonomyLevel(p.contract).level, 'auto-merge-all');
    const diffs = p.contract.split('\n').filter((l, i) => l !== CONTRACT.split('\n')[i]);
    assert.equal(diffs.length, 1);
    assert.ok(diffs[0].startsWith('  level: auto-merge-all'));
    assert.equal(p.branch, 'feat/promotion-autonomy-auto-merge-all');
  });

  it('reports not-promotable at the ceiling instead of throwing', () => {
    const c = CONTRACT.replace('auto-merge-low', 'auto-deploy');
    const p = planPromotion({ contractText: c, evidence: EVIDENCE });
    assert.equal(p.promotable, false);
    assert.ok(p.reason.includes('maximum'));
  });

  it('fails closed when evidence says ready: false', () => {
    assert.throws(
      () => planPromotion({ contractText: CONTRACT, evidence: { ready: false } }),
      /does not indicate readiness/,
    );
  });
});

// --- PR creation (mocked gh layer) ------------------------------------------------
describe('openPromotionPr', () => {
  const base = { repo: 'o/r', base: 'main', head: 'feat/promotion-autonomy-auto-merge-all' };

  it('dry-run returns the exact gh payload without calling the API', () => {
    let calls = 0;
    const r = openPromotionPr({
      ...base,
      title: 'feat(promotion): bump autonomy level to auto-merge-all',
      body: 'evidence here',
      exec: () => { calls += 1; return ''; },
      dryRun: true,
    });
    assert.equal(calls, 0, 'dry-run must not touch the API');
    assert.equal(r.dryRun, true);
    assert.deepEqual(r.argv.slice(0, 4), ['pr', 'create', '-R', 'o/r']);
    assert.ok(r.argv.includes('--title'));
    assert.ok(r.argv.includes('--body'));
    const title = r.argv[r.argv.indexOf('--title') + 1];
    assert.equal(title, 'feat(promotion): bump autonomy level to auto-merge-all');
    assert.equal(r.argv[r.argv.indexOf('--head') + 1], base.head);
    assert.equal(r.argv[r.argv.indexOf('--base') + 1], 'main');
  });

  it('apply calls gh pr create once and returns the URL', () => {
    const seen = [];
    const r = openPromotionPr({
      ...base,
      title: 'feat(promotion): bump autonomy level to auto-merge-all',
      body: 'body',
      exec: (argv) => { seen.push(argv); return 'https://github.com/o/r/pull/42\n'; },
    });
    assert.equal(seen.length, 1);
    assert.equal(r.url, 'https://github.com/o/r/pull/42');
  });
});

// --- Apply orchestration (mocked git + gh) -----------------------------------------
describe('applyPromotion', () => {
  function mockGit() {
    const calls = [];
    return { calls, run: (argv) => { calls.push(argv); return ''; } };
  }

  it('dry-run lists the bounded step plan and PR payload, no side effects', () => {
    const { calls, run } = mockGit();
    const r = applyPromotion({
      repo: 'o/r', base: 'main', branch: 'b',
      contractPath: '.ai-sdlc.yaml', contractText: CONTRACT,
      title: 'feat(promotion): bump autonomy level to auto-merge-all',
      body: 'b', git: run, exec: () => { throw new Error('must not call gh'); },
      dryRun: true,
    });
    assert.equal(calls.length, 0, 'dry-run must not run git');
    assert.equal(r.pr.dryRun, true);
    assert.deepEqual(r.steps[3], ['commit', '-m', 'feat(promotion): bump autonomy level to auto-merge-all']);
  });

  it('apply: clean worktree → branch → commit → push → pr create', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'promo-'));
    const cfg = path.join(dir, '.ai-sdlc.yaml');
    fs.writeFileSync(cfg, CONTRACT);
    const { calls, run } = mockGit();
    const gh = [];
    const r = applyPromotion({
      repo: 'o/r', base: 'main', branch: 'feat/promotion-autonomy-auto-merge-all',
      contractPath: '.ai-sdlc.yaml',
      contractText: setAutonomyLevel(CONTRACT, 'auto-merge-all'),
      title: 'feat(promotion): bump autonomy level to auto-merge-all',
      body: 'evidence', git: run, exec: (argv) => { gh.push(argv); return 'https://x/1'; },
      cwd: dir,
    });
    assert.deepEqual(calls.map((c) => c[0]), ['status', 'checkout', 'add', 'commit', 'push']);
    assert.equal(gh.length, 1);
    assert.equal(gh[0][0], 'pr');
    assert.equal(r.pr.url, 'https://x/1');
    // The file on disk carries the promoted level.
    assert.equal(readAutonomyLevel(fs.readFileSync(cfg, 'utf8')).level, 'auto-merge-all');
  });

  it('refuses to apply on a dirty worktree', () => {
    const dirty = (argv) => (argv[0] === 'status' ? ' M other.js\n' : '');
    assert.throws(
      () => applyPromotion({
        repo: 'o/r', branch: 'b', contractPath: '.ai-sdlc.yaml', contractText: CONTRACT,
        title: 't', body: 'b', git: dirty, exec: () => '',
      }),
      /worktree is dirty/,
    );
  });
});

// --- CLI dry-run end-to-end ---------------------------------------------------------
describe('cli', () => {
  it('dry-run prints the PR payload without touching git or the API', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'promo-cli-'));
    fs.writeFileSync(path.join(dir, '.ai-sdlc.yaml'), CONTRACT);
    let out = '';
    const r = main(
      ['--repo', 'o/r', '--evidence-json', JSON.stringify(EVIDENCE), '--json'],
      {
        cwd: dir,
        stdout: (s) => { out = s; },
        git: () => { throw new Error('git must not run in dry-run'); },
        exec: () => { throw new Error('gh must not run in dry-run'); },
      },
    );
    assert.equal(r.dryRun, true);
    assert.equal(r.promoted, false);
    assert.equal(r.title, 'feat(promotion): bump autonomy level to auto-merge-all');
    assert.ok(r.body.includes('19/20 merges clean'));
    assert.ok(r.diff.includes('-  level: auto-merge-low'));
    assert.ok(r.diff.includes('+  level: auto-merge-all'));
    const printed = JSON.parse(out);
    assert.equal(printed.pr.argv[0], 'pr');
    assert.equal(printed.pr.argv[1], 'create');
  });

  it('exits cleanly at the ceiling with promoted:false', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'promo-max-'));
    fs.writeFileSync(path.join(dir, '.ai-sdlc.yaml'), CONTRACT.replace('auto-merge-low', 'auto-deploy'));
    const r = main(['--repo', 'o/r'], { cwd: dir, stdout: () => {} });
    assert.equal(r.promoted, false);
    assert.ok(r.reason.includes('maximum'));
  });
});

// --- diffPreview ---------------------------------------------------------------------
describe('diffPreview', () => {
  it('renders a single-line hunk', () => {
    const d = diffPreview('.ai-sdlc.yaml', CONTRACT, setAutonomyLevel(CONTRACT, 'auto-merge-all'));
    assert.ok(d.includes('--- a/.ai-sdlc.yaml'));
    assert.ok(d.includes('+++ b/.ai-sdlc.yaml'));
    assert.equal((d.match(/^[-+]/gm) || []).length, 4); // 2 headers + 1 del + 1 add
  });
});
