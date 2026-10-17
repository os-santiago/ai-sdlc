// scripts/ab-experiment.test.js — tests for the A/B experiment promotion
// decision layer (issue #192). Covers the acceptance criteria: config-declared
// variants, bounded evidence windows, deterministic promote/continue/rollback
// decisions, and the insufficient-data path.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  validateExperiment,
  loadExperiments,
  controlName,
  trafficSplit,
  readEvidence,
  rateOutcome,
  metricValue,
  collectArms,
  twoProportionZTest,
  welchTTest,
  resolveServing,
  evaluateExperiment,
  applyDecision,
  promoteExperiment,
  rollbackExperiment,
  loadState,
  main,
} = require('./ab-experiment');

const CONFIG = path.join(__dirname, '..', 'config', 'experiments.json');
const NOW = '2026-10-14T00:00:00Z';

function tmpdir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'ab-exp-'));
}

/** Minimal two-arm rate-metric experiment. */
function exp(over = {}) {
  return {
    id: 'test-exp',
    status: 'running',
    control: 'control',
    metric: { name: 'pass_rate', direction: 'higher' },
    alpha: 0.05,
    minSamplesPerArm: 20,
    window: 100,
    variants: [
      { name: 'control', weight: 0.5 },
      { name: 'candidate', weight: 0.5 },
    ],
    ...over,
  };
}

/** n Bernoulli records for a variant, passRate fraction passing. */
function rateRecords(variant, n, passRate, opts = {}) {
  const wins = Math.round(n * passRate);
  const records = [];
  for (let i = 0; i < n; i += 1) {
    const at = new Date(new Date(NOW).getTime() - (n - i) * 60e3).toISOString();
    records.push({
      experiment: opts.experimentId || 'test-exp',
      variant,
      pass: i < wins,
      at: opts.noTs ? undefined : at,
    });
  }
  return records;
}

function valueRecords(variant, values, opts = {}) {
  return values.map((v, i) => ({
    experiment: opts.experimentId || 'test-exp',
    variant,
    metrics: { score: v },
    at: new Date(new Date(NOW).getTime() - (values.length - i) * 60e3).toISOString(),
  }));
}

// --- config validation -------------------------------------------------------

describe('validateExperiment', () => {
  it('accepts a well-formed experiment', () => {
    const r = validateExperiment(exp());
    assert.equal(r.ok, true, r.errors.join('; '));
  });

  it('rejects bad ids, metrics, alphas, windows and undeclared control/winner', () => {
    assert.equal(validateExperiment(null).ok, false);
    assert.equal(validateExperiment(exp({ id: 'bad slug!' })).ok, false);
    assert.equal(validateExperiment(exp({ metric: { name: 'nope' } })).ok, false);
    assert.equal(validateExperiment(exp({ alpha: 1.5 })).ok, false);
    assert.equal(validateExperiment(exp({ alpha: 0 })).ok, false);
    assert.equal(validateExperiment(exp({ window: 0 })).ok, false);
    assert.equal(validateExperiment(exp({ minSamplesPerArm: 0 })).ok, false);
    assert.equal(validateExperiment(exp({ control: 'ghost' })).ok, false);
    assert.equal(validateExperiment(exp({ winner: 'ghost' })).ok, false);
    const dup = exp({ variants: [{ name: 'a' }, { name: 'a' }] });
    assert.equal(validateExperiment(dup).ok, false);
    const badWeight = exp({ variants: [{ name: 'a', weight: -1 }, { name: 'b' }] });
    assert.equal(validateExperiment(badWeight).ok, false);
  });

  it('shipped config/experiments.json parses and validates clean', () => {
    const cfg = loadExperiments(CONFIG);
    assert.deepEqual(cfg.warnings, []);
    assert.equal(cfg.experiments.length, 1);
    assert.equal(cfg.experiments[0].id, 'primary-model-quality');
    const split = trafficSplit(cfg.experiments[0]);
    assert.equal(split.primary, 0.5);
    assert.equal(split.candidate, 0.5);
  });
});

describe('controlName / trafficSplit', () => {
  it('resolves explicit control, then the variant literally named control', () => {
    assert.equal(controlName(exp()), 'control');
    assert.equal(controlName(exp({ control: undefined, variants: [{ name: 'a' }, { name: 'control' }] })), 'control');
    assert.equal(controlName(exp({ control: undefined, variants: [{ name: 'a' }, { name: 'b' }] })), 'a');
  });

  it('normalizes weights into a split', () => {
    const e = exp({ variants: [{ name: 'a', weight: 3 }, { name: 'b', weight: 1 }] });
    assert.equal(trafficSplit(e).a, 0.75);
    assert.equal(trafficSplit(e).b, 0.25);
  });
});

// --- evidence ingestion ------------------------------------------------------

describe('readEvidence', () => {
  it('reads JSON arrays and JSONL, skipping malformed lines', () => {
    const dir = tmpdir();
    const arr = path.join(dir, 'e.json');
    fs.writeFileSync(arr, JSON.stringify([{ variant: 'a' }, { variant: 'b' }]));
    assert.equal(readEvidence(arr).length, 2);
    const jl = path.join(dir, 'e.jsonl');
    fs.writeFileSync(jl, '{"variant":"a"}\nnot-json\n{"variant":"b"}\n');
    assert.equal(readEvidence(jl).length, 2);
  });
});

describe('rateOutcome / metricValue', () => {
  it('extracts the Bernoulli outcome from the field precedence chain', () => {
    assert.equal(rateOutcome({ metrics: { pass_rate: 1 }, pass: false }, 'pass_rate'), true);
    assert.equal(rateOutcome({ pass: true }, 'pass_rate'), true);
    assert.equal(rateOutcome({ success: false }, 'pass_rate'), false);
    assert.equal(rateOutcome({ outcome: 'success' }, 'success_rate'), true);
    assert.equal(rateOutcome({ outcome: 'failed' }, 'success_rate'), false);
    assert.equal(rateOutcome({ outcome: 'weird' }, 'pass_rate'), null);
    assert.equal(rateOutcome({}, 'pass_rate'), null);
  });

  it('extracts continuous values from metrics/name/value', () => {
    assert.equal(metricValue({ metrics: { score: 0.9 } }, 'score'), 0.9);
    assert.equal(metricValue({ score: 0.4 }, 'score'), 0.4);
    assert.equal(metricValue({ value: 7 }, 'score'), 7);
    assert.equal(metricValue({ other: 1 }, 'score'), null);
  });
});

describe('collectArms — bounded windows', () => {
  it('truncates to the newest `window` records per arm and counts drops', () => {
    // 30 old control records all failing, then 5 recent all passing.
    const oldFail = rateRecords('control', 30, 0).map((r) => ({
      ...r, at: '2020-01-01T00:00:00Z',
    }));
    const recentPass = rateRecords('control', 5, 1);
    const records = [...oldFail, ...recentPass];
    const { arms, dropped } = collectArms(exp({ window: 10 }), records, { nowMs: new Date(NOW).getTime() });
    // Window keeps the 10 newest: 5 recent passes + 5 newest old fails.
    assert.equal(arms.control.n, 10);
    assert.equal(arms.control.wins, 5);
    assert.equal(dropped.beyondWindow, 25);
  });

  it('windowDays drops stale and undated records', () => {
    const stale = rateRecords('control', 5, 0).map((r) => ({ ...r, at: '2020-01-01T00:00:00Z' }));
    const undated = rateRecords('control', 3, 1, { noTs: true });
    const fresh = rateRecords('control', 4, 1);
    const { arms, dropped } = collectArms(exp({ windowDays: 7 }), [...stale, ...undated, ...fresh], { nowMs: new Date(NOW).getTime() });
    assert.equal(arms.control.n, 4);
    assert.equal(arms.control.wins, 4);
    assert.equal(dropped.outsideWindowDays, 5);
    assert.equal(dropped.undated, 3);
  });

  it('skips records tagged for other experiments or undeclared variants', () => {
    const records = [
      ...rateRecords('control', 3, 1),
      { experiment: 'other-exp', variant: 'control', pass: true },
      { experiment: 'test-exp', variant: 'ghost', pass: true },
      { experiment: 'test-exp', variant: 'control' }, // no metric reading
    ];
    const { arms, dropped, unknownVariants } = collectArms(exp(), records, { nowMs: new Date(NOW).getTime() });
    assert.equal(arms.control.n, 3);
    assert.equal(dropped.otherExperiment, 1);
    assert.equal(dropped.unknownVariant, 1);
    assert.equal(dropped.noMetricReading, 1);
    assert.equal(unknownVariants.ghost, 1);
  });
});

// --- statistics --------------------------------------------------------------

describe('statistics', () => {
  it('two-proportion z-test flags a clear difference', () => {
    const sig = twoProportionZTest(40, 80, 60, 80);
    assert.ok(sig.diff > 0);
    assert.ok(sig.pValue < 0.05, `p=${sig.pValue}`);
    const tied = twoProportionZTest(40, 80, 40, 80);
    assert.equal(tied.pValue, 1);
  });

  it('welch t-test flags separated means and handles n<2', () => {
    const c = [1, 1.1, 0.9, 1, 1.05, 0.95];
    const v = [2, 2.1, 1.9, 2.05, 2, 1.95];
    const r = welchTTest(c, v);
    assert.ok(r.pValue < 0.05);
    assert.equal(welchTTest([1], [2, 3]).pValue, null);
  });
});

// --- decisions ---------------------------------------------------------------

describe('evaluateExperiment', () => {
  it('promotes a challenger beating control significantly', () => {
    const records = [
      ...rateRecords('control', 80, 0.5),
      ...rateRecords('candidate', 80, 0.75),
    ];
    const r = evaluateExperiment(exp(), { evidenceRecords: records, now: NOW });
    assert.equal(r.decision.action, 'promote');
    assert.equal(r.decision.target, 'candidate');
    assert.equal(r.bestArm, 'candidate');
    assert.ok(r.comparisons[0].pValue <= 0.05);
  });

  it('continues when powered comparisons show no winner', () => {
    const records = [
      ...rateRecords('control', 80, 0.5),
      ...rateRecords('candidate', 80, 0.52),
    ];
    const r = evaluateExperiment(exp(), { evidenceRecords: records, now: NOW });
    assert.equal(r.decision.action, 'continue');
    assert.equal(r.decision.target, null);
    assert.equal(r.serving, 'control');
  });

  it('reports insufficient_data below minSamplesPerArm', () => {
    const records = [
      ...rateRecords('control', 5, 0.2),
      ...rateRecords('candidate', 5, 1),
    ];
    const r = evaluateExperiment(exp(), { evidenceRecords: records, now: NOW });
    assert.equal(r.decision.action, 'insufficient_data');
    assert.equal(r.comparisons[0].sufficient, false);
    // Serving never changes on insufficient evidence.
    assert.equal(r.serving, 'control');
  });

  it('reports insufficient_data on empty evidence', () => {
    const r = evaluateExperiment(exp(), { evidenceRecords: [], now: NOW });
    assert.equal(r.decision.action, 'insufficient_data');
  });

  it('rolls back a promoted winner that regresses vs control', () => {
    const state = { experiments: { 'test-exp': { status: 'promoted', winner: 'candidate' } } };
    const records = [
      ...rateRecords('control', 100, 0.7),
      ...rateRecords('candidate', 100, 0.45),
    ];
    const r = evaluateExperiment(exp(), { state, evidenceRecords: records, now: NOW });
    assert.equal(r.decision.action, 'rollback');
    assert.equal(r.decision.target, 'control');
    // Until applied, serving still resolves the recorded promotion.
    assert.equal(r.serving, 'candidate');
    assert.equal(r.servingSource, 'promotion');
  });

  it('keeps serving the promoted winner while it holds (continue)', () => {
    const state = { experiments: { 'test-exp': { status: 'promoted', winner: 'candidate' } } };
    const records = [
      ...rateRecords('control', 80, 0.5),
      ...rateRecords('candidate', 80, 0.75),
    ];
    const r = evaluateExperiment(exp(), { state, evidenceRecords: records, now: NOW });
    assert.equal(r.decision.action, 'continue');
    assert.equal(r.serving, 'candidate');
  });

  it('supports continuous metrics via Welch (score, higher-better)', () => {
    const e = exp({ metric: { name: 'score', direction: 'higher' }, minSamplesPerArm: 5 });
    const records = [
      ...valueRecords('control', [0.5, 0.55, 0.45, 0.5, 0.52, 0.48, 0.5, 0.51, 0.49, 0.5]),
      ...valueRecords('candidate', [0.8, 0.82, 0.79, 0.81, 0.8, 0.83, 0.78, 0.8, 0.82, 0.8]),
    ];
    const r = evaluateExperiment(e, { evidenceRecords: records, now: NOW });
    assert.equal(r.decision.action, 'promote');
    assert.equal(r.decision.target, 'candidate');
    assert.ok(r.arms.candidate.mean > r.arms.control.mean);
  });

  it('lower-is-better direction promotes a latency regression fix', () => {
    const e = exp({ metric: { name: 'latency_ms' }, minSamplesPerArm: 5 }); // direction defaults lower
    const records = [
      ...[900, 950, 920, 880, 910, 940, 900, 930, 890, 920].map((v, i) => ({
        experiment: 'test-exp', variant: 'control', latency_ms: v,
        at: new Date(new Date(NOW).getTime() - i * 60e3).toISOString(),
      })),
      ...[400, 420, 410, 390, 405, 415, 400, 395, 410, 405].map((v, i) => ({
        experiment: 'test-exp', variant: 'candidate', latency_ms: v,
        at: new Date(new Date(NOW).getTime() - i * 60e3).toISOString(),
      })),
    ];
    const r = evaluateExperiment(e, { evidenceRecords: records, now: NOW });
    assert.equal(r.decision.action, 'promote');
    assert.equal(r.decision.target, 'candidate');
    assert.ok(r.comparisons[0].diff < 0);
    assert.equal(r.comparisons[0].direction, 'lower');
  });

  it('is deterministic — same inputs produce identical decisions', () => {
    const records = [
      ...rateRecords('control', 80, 0.5),
      ...rateRecords('candidate', 80, 0.75),
    ];
    const a = evaluateExperiment(exp(), { evidenceRecords: records, now: NOW });
    const b = evaluateExperiment(exp(), { evidenceRecords: records, now: NOW });
    assert.deepEqual(a, b);
  });
});

// --- serving resolution ------------------------------------------------------

describe('resolveServing', () => {
  const e = exp();

  it('resolves control by default', () => {
    const s = resolveServing(e, { experiments: {} });
    assert.equal(s.variant, 'control');
    assert.equal(s.source, 'control');
  });

  it('resolves the promoted winner after a promotion', () => {
    const state = { experiments: { 'test-exp': { status: 'promoted', winner: 'candidate' } } };
    const s = resolveServing(e, state);
    assert.equal(s.variant, 'candidate');
    assert.equal(s.source, 'promotion');
  });

  it('explicit override beats promotion; invalid override is ignored with a warning', () => {
    const state = { experiments: { 'test-exp': { status: 'promoted', winner: 'candidate' } } };
    const s = resolveServing(e, state, { override: 'control' });
    assert.equal(s.variant, 'control');
    assert.equal(s.source, 'override');
    const bad = resolveServing(e, state, { override: 'ghost' });
    assert.equal(bad.variant, 'candidate');
    assert.equal(bad.warnings.length, 1);
  });

  it('AI_SDLC_AB_DISABLE forces control (kill switch)', () => {
    const state = { experiments: { 'test-exp': { status: 'promoted', winner: 'candidate' } } };
    assert.equal(resolveServing(e, state, { disabled: true }).variant, 'control');
    process.env.AI_SDLC_AB_DISABLE = '1';
    try {
      assert.equal(resolveServing(e, state).variant, 'control');
    } finally {
      delete process.env.AI_SDLC_AB_DISABLE;
    }
  });

  it('rolled_back state restores control', () => {
    const state = { experiments: { 'test-exp': { status: 'rolled_back', winner: null } } };
    assert.equal(resolveServing(e, state).variant, 'control');
  });
});

// --- applied transitions + audit --------------------------------------------

describe('applyDecision / manual transitions', () => {
  it('apply persists a promotion; subsequent serving resolves the winner', () => {
    const dir = tmpdir();
    const stateFile = path.join(dir, 'state.json');
    const e = exp();
    const records = [
      ...rateRecords('control', 80, 0.5),
      ...rateRecords('candidate', 80, 0.75),
    ];
    const result = evaluateExperiment(e, { evidenceRecords: records, now: NOW });
    const applied = applyDecision(e, result, { stateFile, now: NOW });
    assert.equal(applied.applied, true);
    assert.equal(applied.target, 'candidate');

    const st = loadState(stateFile);
    assert.equal(st.experiments['test-exp'].status, 'promoted');
    assert.equal(st.experiments['test-exp'].winner, 'candidate');
    assert.equal(resolveServing(e, st).variant, 'candidate');

    // Audit log records the decision trail.
    const audit = fs.readFileSync(stateFile.replace(/\.json$/, '.audit.jsonl'), 'utf8')
      .trim().split('\n').map(JSON.parse);
    assert.equal(audit.length, 1);
    assert.equal(audit[0].event, 'decision:promote');
    assert.equal(audit[0].target, 'candidate');
    assert.ok(audit[0].reasons.length > 0);
  });

  it('apply persists a rollback; serving returns to control', () => {
    const dir = tmpdir();
    const stateFile = path.join(dir, 'state.json');
    const e = exp();
    // Seed the promoted state — applyDecision reloads state from the file.
    fs.writeFileSync(stateFile, JSON.stringify({
      version: 1,
      experiments: { 'test-exp': { status: 'promoted', winner: 'candidate' } },
    }));
    const state = loadState(stateFile);
    const records = [
      ...rateRecords('control', 100, 0.7),
      ...rateRecords('candidate', 100, 0.45),
    ];
    const result = evaluateExperiment(e, { state, evidenceRecords: records, now: NOW });
    assert.equal(result.decision.action, 'rollback');
    const applied = applyDecision(e, result, { stateFile, now: NOW });
    assert.equal(applied.applied, true);
    const st = loadState(stateFile);
    assert.equal(st.experiments['test-exp'].status, 'rolled_back');
    assert.equal(st.experiments['test-exp'].previousWinner, 'candidate');
    assert.equal(resolveServing(e, st).variant, 'control');
  });

  it('manual promote + rollback round-trip through the state file', () => {
    const dir = tmpdir();
    const stateFile = path.join(dir, 'state.json');
    const e = exp();
    const p = promoteExperiment(e, 'candidate', { stateFile, now: NOW });
    assert.equal(p.promoted, true);
    assert.equal(resolveServing(e, loadState(stateFile)).variant, 'candidate');
    const r = rollbackExperiment(e, { stateFile, reason: 'operator test', now: NOW });
    assert.equal(r.rolledBack, true);
    assert.equal(r.previousWinner, 'candidate');
    assert.equal(resolveServing(e, loadState(stateFile)).variant, 'control');
    // Second rollback refuses — nothing promoted.
    assert.equal(rollbackExperiment(e, { stateFile }).rolledBack, false);
    // Undeclared variant refuses.
    assert.equal(promoteExperiment(e, 'ghost', { stateFile }).promoted, false);
  });

  it('apply without a state file fails closed', () => {
    const result = evaluateExperiment(exp(), {
      evidenceRecords: [...rateRecords('control', 80, 0.5), ...rateRecords('candidate', 80, 0.75)],
      now: NOW,
    });
    const applied = applyDecision(exp(), result, { stateFile: null });
    assert.equal(applied.applied, false);
  });
});

// --- CLI ---------------------------------------------------------------------

describe('CLI', () => {
  it('decide emits the decision JSON; serving resolves plain variant', () => {
    const dir = tmpdir();
    const cfgFile = path.join(dir, 'experiments.json');
    fs.writeFileSync(cfgFile, JSON.stringify({ version: 1, experiments: [exp()] }));
    const evFile = path.join(dir, 'evidence.jsonl');
    fs.writeFileSync(evFile, [
      ...rateRecords('control', 80, 0.5),
      ...rateRecords('candidate', 80, 0.75),
    ].map((r) => JSON.stringify(r)).join('\n'));

    let out = '';
    const code = main([
      'decide', '--config', cfgFile, '--experiment', 'test-exp',
      '--evidence', evFile, '--now', NOW,
    ], { stdout: (s) => { out = s; } });
    assert.equal(code, 0);
    const decision = JSON.parse(out);
    assert.equal(decision.decision.action, 'promote');
    assert.equal(decision.serving, 'control'); // not applied yet

    const stateFile = path.join(dir, 'state.json');
    out = '';
    main([
      'decide', '--config', cfgFile, '--experiment', 'test-exp',
      '--evidence', evFile, '--now', NOW, '--apply', '--state', stateFile,
    ], { stdout: (s) => { out = s; } });
    assert.equal(JSON.parse(out).serving, 'candidate'); // applied

    out = '';
    main(['serving', '--config', cfgFile, '--experiment', 'test-exp', '--state', stateFile], { stdout: (s) => { out = s; } });
    assert.equal(out, 'candidate');
  });

  it('list and usage errors behave', () => {
    const dir = tmpdir();
    const cfgFile = path.join(dir, 'experiments.json');
    fs.writeFileSync(cfgFile, JSON.stringify({ version: 1, experiments: [exp()] }));
    let out = '';
    assert.equal(main(['list', '--config', cfgFile], { stdout: (s) => { out = s; } }), 0);
    assert.ok(out.includes('test-exp'));
    assert.throws(() => main(['bogus'], {}), /unknown op/);
  });
});
