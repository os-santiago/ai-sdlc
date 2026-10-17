// scripts/cost-accounting.test.js — tests for the per-run token/cost ledger
// (issue #195). Covers the acceptance criteria: recording, issue→merge
// rollup, and budget thresholds.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { execFileSync } = require('child_process');

const {
  RECORD_TYPE,
  loadCostConfig,
  resolvePrice,
  priceTokens,
  readManifest,
  buildRecord,
  appendRecord,
  readUsageLedger,
  normalizeRecord,
  filterRecords,
  rollupByIssue,
  evaluateBudgets,
  rollupMarkdown,
} = require('./cost-accounting');

const CONFIG = path.join(__dirname, '..', 'config', 'cost_models.json');

function tmpdir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'cost-acct-'));
}

const TEST_MODELS = {
  'test/expensive': { input_per_m: 2.0, output_per_m: 10.0 },
  'test/free': { input_per_m: 0.0, output_per_m: 0.0 },
  default: { input_per_m: 1.0, output_per_m: 3.0 },
};

function rec(over = {}) {
  return {
    stage: 'implement',
    repo: 'o/r',
    issue: 42,
    model: 'test/expensive',
    tokens: { in: 1000, out: 500, total: 1500 },
    cost_usd: 0.007,
    outcome: 'success',
    ts: '2026-10-01T00:00:00Z',
    ...over,
  };
}

// --- config / price table -----------------------------------------------------

describe('loadCostConfig', () => {
  it('loads the declared table and budgets', () => {
    const c = loadCostConfig(CONFIG);
    assert.equal(c.currency, 'USD');
    assert.ok(c.budgets.per_issue_usd > 0);
    assert.ok(c.models['nvidia/nemotron-3-super-120b-a12b']);
    assert.ok(c.models.default);
    assert.ok(c.alert_thresholds.includes(1.0));
  });
});

describe('resolvePrice', () => {
  it('matches exact ids and provider suffixes', () => {
    assert.equal(resolvePrice('test/expensive', TEST_MODELS).key, 'test/expensive');
    assert.equal(resolvePrice('expensive', TEST_MODELS).key, 'test/expensive');
  });
  it('falls back to default for unknown models (never zero by accident)', () => {
    assert.equal(resolvePrice('some-future-model', TEST_MODELS).key, 'default');
    assert.equal(resolvePrice('', TEST_MODELS).key, 'default');
  });
});

describe('priceTokens — cost computed from the declared table', () => {
  it('computes in+out cost per million tokens', () => {
    const p = priceTokens('test/expensive', 1_000_000, 500_000, TEST_MODELS);
    assert.equal(p.cost_usd, 2.0 + 5.0);
    assert.equal(p.price.key, 'test/expensive');
  });
  it('prices zero-token runs at zero', () => {
    assert.equal(priceTokens('test/expensive', 0, 0, TEST_MODELS).cost_usd, 0);
  });
});

// --- recording -----------------------------------------------------------------

describe('record — per-run usage recording', () => {
  it('builds a ledger-friendly record with computed cost', () => {
    const config = loadCostConfig(CONFIG);
    const r = buildRecord(
      { stage: 'implement', repo: 'o/r', issue: '42', model: 'openai/gpt-4o', tokens_in: '2000', tokens_out: '1000' },
      config,
    );
    assert.equal(r.v, 1);
    assert.equal(r.type, RECORD_TYPE);
    assert.equal(r.stage, 'implement');
    assert.equal(r.issue, 42);
    assert.equal(r.tokens.total, 3000);
    // openai/gpt-4o: 2.50/10.00 per M → 2000*2.5e-6 + 1000*10e-6 = 0.015
    assert.equal(r.cost_usd, 0.015);
    assert.equal(r.cost_source, 'price_table');
    assert.equal(r.price.key, 'openai/gpt-4o');
  });

  it('appends one JSONL line per record and reads it back', () => {
    const dir = tmpdir();
    const ledger = path.join(dir, 'usage.jsonl');
    const config = { ...loadCostConfig(CONFIG), models: TEST_MODELS };
    appendRecord(ledger, buildRecord({ stage: 'intake', issue: '1', model: 'test/free', tokens_in: '10', tokens_out: '5' }, config));
    appendRecord(ledger, buildRecord({ stage: 'implement', issue: '1', model: 'test/expensive', tokens_in: '100', tokens_out: '50' }, config));
    const records = readUsageLedger(ledger);
    assert.equal(records.length, 2);
    assert.equal(records[0].stage, 'intake');
    assert.equal(records[1].tokens.total, 150);
  });

  it('imports tokens and engine cost from a run manifest', () => {
    const dir = tmpdir();
    const mf = path.join(dir, 'scc-manifest.json');
    fs.writeFileSync(mf, JSON.stringify({
      model: 'test/expensive',
      tokens_in: 1234,
      tokens_out: 567,
      estimated_cost_usd: 9.99,
      iterations: 7,
      duration_ms: 42000,
      success: true,
    }));
    const m = readManifest(mf);
    assert.equal(m.model, 'test/expensive');
    assert.equal(m.tokens_in, 1234);
    assert.equal(m.tokens_out, 567);
    assert.equal(m.engine_cost_usd, 9.99);

    const r = buildRecord({ stage: 'implement', issue: '7', manifest: mf }, { models: TEST_MODELS });
    // Computed from OUR table, not the engine's claim.
    assert.equal(r.cost_usd, Number((1234 * 2e-6 + 567 * 10e-6).toFixed(6)));
    assert.equal(r.engine_cost_usd, 9.99); // preserved for audit
    assert.equal(r.outcome, 'success');
    assert.equal(r.meta.iterations, 7);
  });

  it('rejects invalid stage names and negative tokens', () => {
    assert.throws(() => buildRecord({ stage: 'BAD STAGE!' }, { models: TEST_MODELS }), /invalid stage/);
    assert.throws(
      () => buildRecord({ stage: 'implement', model: 'x', tokens_in: '-5', tokens_out: '0' }, { models: TEST_MODELS }),
      /tokens-in/,
    );
  });

  it('skips malformed ledger lines instead of failing', () => {
    const dir = tmpdir();
    const ledger = path.join(dir, 'usage.jsonl');
    fs.writeFileSync(ledger, '{"stage":"implement","issue":1,"tokens":{"in":1,"out":1,"total":2},"cost_usd":0.001}\nnot-json\n\n');
    const records = readUsageLedger(ledger);
    assert.equal(records.length, 1);
  });
});

// --- rollup (issue→merge, queryable) --------------------------------------------

describe('rollupByIssue — issue→merge rollup', () => {
  const records = [
    rec({ stage: 'intake', tokens: { in: 100, out: 50, total: 150 }, cost_usd: 0.001, outcome: 'success' }),
    rec({ stage: 'implement', tokens: { in: 10000, out: 4000, total: 14000 }, cost_usd: 0.06, outcome: 'success', pr: 7 }),
    rec({ stage: 'review', tokens: { in: 3000, out: 800, total: 3800 }, cost_usd: 0.014 }),
    rec({ stage: 'ci-repair', tokens: { in: 2000, out: 900, total: 2900 }, cost_usd: 0.013 }),
    rec({ stage: 'automerge', tokens: { in: 0, out: 0, total: 0 }, cost_usd: 0, outcome: 'success' }),
    rec({ issue: 99, stage: 'implement', tokens: { in: 5, out: 5, total: 10 }, cost_usd: 0.00003 }),
  ];

  it('aggregates every stage into the issue total', () => {
    const [it] = rollupByIssue(records, { issue: 42 });
    assert.equal(it.issue, 42);
    assert.equal(it.record_count, 5);
    assert.equal(it.tokens_in, 15100);
    assert.equal(it.tokens_total, 20850);
    assert.equal(it.cost_usd, Number((0.001 + 0.06 + 0.014 + 0.013).toFixed(6)));
    assert.equal(it.status, 'merged');
    assert.deepEqual(it.prs, [7]);
    assert.deepEqual(Object.keys(it.by_stage).sort(), ['automerge', 'ci-repair', 'implement', 'intake', 'review']);
    assert.equal(it.by_stage.implement.cost_usd, 0.06);
    assert.ok(it.by_model['test/expensive'].records === 5);
  });

  it('keeps issues separate and is queryable by issue number', () => {
    const all = rollupByIssue(records);
    assert.equal(all.length, 2);
    const [only99] = rollupByIssue(records, { issue: 99 });
    assert.equal(only99.record_count, 1);
    assert.equal(only99.status, 'pr_opened'); // implement success, no merge stage yet
  });

  it('marks non-merged lifecycles distinctly', () => {
    const failed = rollupByIssue([rec({ outcome: 'failure' })])[0];
    assert.equal(failed.status, 'failed');
    const esc = rollupByIssue([rec({ outcome: 'needs-human' })])[0];
    assert.equal(esc.status, 'escalated');
    const running = rollupByIssue([rec({ outcome: null })])[0];
    assert.equal(running.status, 'in_progress');
  });

  it('unwraps a usage record embedded in a run-ledger entry (forward-compat)', () => {
    const embedded = {
      v: 1, seq: 1, stage: 'implement', ts: '2026-10-01T00:00:00Z',
      repo: 'o/r', issue: 42, pr: 7, hash: 'a'.repeat(64), prev: '0'.repeat(64),
      data: { type: RECORD_TYPE, model: 'test/expensive', tokens: { in: 10, out: 5, total: 15 }, cost_usd: 0.00007 },
    };
    const n = normalizeRecord(embedded);
    assert.equal(n.stage, 'implement');
    assert.equal(n.issue, 42);
    assert.equal(n.tokens.total, 15);
    assert.equal(n.cost_usd, 0.00007);
  });

  it('renders a markdown summary table', () => {
    const md = rollupMarkdown(rollupByIssue(records));
    assert.match(md, /issue→merge cost rollup/);
    assert.match(md, /o\/r#42/);
    assert.match(md, /merged/);
  });
});

describe('filterRecords — windows', () => {
  it('filters by repo and period', () => {
    const now = '2026-10-01T12:00:00Z';
    const records = [
      rec({ ts: '2026-10-01T06:00:00Z' }),
      rec({ ts: '2026-09-20T00:00:00Z' }),
      rec({ ts: '2026-10-01T06:00:00Z', repo: 'other/repo' }),
    ];
    assert.equal(filterRecords(records, { period: '24h', now }).length, 2);
    assert.equal(filterRecords(records, { period: '24h', now, repo: 'o/r' }).length, 1);
    assert.equal(filterRecords(records, { period: '30d', now }).length, 3);
  });
});

// --- budget thresholds -----------------------------------------------------------

describe('evaluateBudgets — budget thresholds', () => {
  const config = {
    alert_thresholds: [0.5, 0.8, 1.0],
    budgets: { per_issue_usd: 0.10, per_issue_tokens: 5000, daily_usd: 0.20, weekly_usd: 0, monthly_usd: 0 },
    models: TEST_MODELS,
  };
  const now = '2026-10-01T12:00:00Z';

  it('is silent under all budgets', () => {
    const alerts = evaluateBudgets([rec({ cost_usd: 0.01, ts: now })], config, { now });
    assert.equal(alerts.length, 0);
  });

  it('emits the highest crossed threshold per budget dimension', () => {
    // 0.085 vs 0.10 → 85%: crosses 0.5 and 0.8, reports only 0.8.
    const alerts = evaluateBudgets([rec({ cost_usd: 0.085, ts: now })], config, { now });
    const issueAlert = alerts.find((a) => a.scope === 'issue' && a.metric === 'cost_usd');
    assert.equal(issueAlert.threshold, 0.8);
    assert.equal(issueAlert.severity, 'critical');
    assert.equal(issueAlert.percent_used, 85);
  });

  it('flags over-budget issues as exceeded (cost + tokens)', () => {
    const alerts = evaluateBudgets(
      [rec({ cost_usd: 0.15, tokens: { in: 4000, out: 3000, total: 7000 }, ts: now })],
      config, { now },
    );
    const byMetric = Object.fromEntries(alerts.filter((a) => a.scope === 'issue').map((a) => [a.metric, a]));
    assert.equal(byMetric.cost_usd.severity, 'exceeded');
    assert.equal(byMetric.tokens.severity, 'exceeded');
    assert.equal(byMetric.tokens.spent, 7000);
  });

  it('evaluates repo period budgets over the window only', () => {
    const records = [
      rec({ cost_usd: 0.18, ts: '2026-10-01T06:00:00Z' }),        // inside 24h
      rec({ cost_usd: 5.0, ts: '2026-09-01T00:00:00Z', issue: 9 }), // old — outside
    ];
    const alerts = evaluateBudgets(records, config, { now });
    const daily = alerts.find((a) => a.scope === 'repo' && a.period === '24h');
    assert.equal(daily.severity, 'critical'); // 0.18/0.20 = 90%
    assert.equal(daily.spent, 0.18);
  });
});

describe('check --strict CLI — exit semantics', () => {
  it('exits 1 on an exceeded budget, 0 otherwise', () => {
    const dir = tmpdir();
    const ledger = path.join(dir, 'usage.jsonl');
    const cfg = path.join(dir, 'cost.json');
    fs.writeFileSync(cfg, JSON.stringify({
      budgets: { per_issue_usd: 0.001, per_issue_tokens: 0, daily_usd: 0, weekly_usd: 0, monthly_usd: 0 },
      alert_thresholds: [0.5, 0.8, 1.0],
      models: TEST_MODELS,
    }));
    appendRecord(ledger, rec({ cost_usd: 0.01 }));

    // No --strict: alerts print, exit 0.
    const ok = execFileSync('node', [
      path.join(__dirname, 'cost-accounting.js'),
      'check', '--ledger', ledger, '--config', cfg,
    ], { encoding: 'utf8' });
    assert.match(ok, /"alerts"/);

    // --strict + an under-budget issue filter → exit 0.
    execFileSync('node', [
      path.join(__dirname, 'cost-accounting.js'),
      'check', '--ledger', ledger, '--config', cfg, '--strict', '--issue', '43',
    ], { encoding: 'utf8' });

    // --strict over the over-budget issue → exit 1.
    assert.throws(
      () => execFileSync('node', [
        path.join(__dirname, 'cost-accounting.js'),
        'check', '--ledger', ledger, '--config', cfg, '--strict',
      ], { encoding: 'utf8', stdio: 'pipe' }),
      (e) => e.status === 1,
    );
  });
});
