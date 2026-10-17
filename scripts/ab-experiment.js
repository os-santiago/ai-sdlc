#!/usr/bin/env node
// scripts/ab-experiment.js — A/B experiment promotion on eval evidence
// (issue #192; epic #148 "evidence and economics").
//
// Decision layer for experiment lifecycle: experiments are declared in
// config/experiments.json (variants + traffic split + eval metric), eval
// run records are the evidence, and `decide` compares every challenger arm
// against control — a variant is marked promoted only when its win rate
// beats control at p <= alpha (default 0.05) over a bounded evidence
// window. The promoted variant is what `serving` resolves for subsequent
// runs, unless an explicit override is set. No live traffic is routed
// here — the service consumes run/eval artifacts and records its verdict.
//
// Lifecycle: draft → running ⇄ paused → promoted → rolled_back → concluded
// (desired status is declared in config; applied transitions persist in the
// state file so a promotion never dirties the committed config).
//
// Evidence record shape (--evidence FILE, JSON array or JSONL):
//   {
//     "experiment": "<id>",        // optional — scopes the record; records
//                                  //   tagged for another experiment are skipped
//     "variant": "<name>",         // required — must name a declared variant
//     "at": "<ISO timestamp>",     // optional — window ordering + windowDays
//     "pass": true | false,        // rate-metric outcome (aliases below)
//     "metrics": {"<name>": n}     // continuous metric payload — also
//                                  //   accepted top-level ("<name>": n) or
//                                  //   as "value": n on single-value records
//   }
//   Rate metrics (pass_rate|success_rate|win_rate) read a Bernoulli outcome
//   from `metrics.<name>` (0/1) → `<base>` field (pass/success/win) → generic
//   `pass`/`success`/`win` booleans → `outcome` string (success|passed|win /
//   failure|failed|loss). Continuous metrics (score|iterations|tokens|
//   duration_ms|latency_ms|cost_usd) read `metrics.<name>` → `<name>` →
//   `value`. Records carrying no reading for the experiment's metric do not
//   count toward that arm's sample size.
//
// Bounded windows: per arm, evidence is sorted by `at` (undated records
// sort oldest) and truncated to the newest `window` records; `windowDays`
// additionally drops records older than now − N days (undated records are
// dropped — recency is unverifiable; evidence is fail-closed). `--now`
// injects the clock so evaluations replay deterministically.
//
// Decisions (deterministic over config + evidence + state):
//   promote            a challenger beats control significantly (p<=alpha)
//                      and favorably at >= minSamplesPerArm — the strongest
//                      such arm wins; target names the variant
//   rollback           the promoted winner now loses significantly to
//                      control — reverts serving to the control variant
//   continue           windows are powered (sufficient samples) and no arm
//                      earns a transition — keep collecting / keep serving
//   insufficient_data  no comparison reached minSamplesPerArm — never
//                      promotes; serving stays as currently resolved
//
// CLI:
//   node scripts/ab-experiment.js list [--config F] [--state F] [--json]
//   node scripts/ab-experiment.js decide --experiment ID --evidence FILE
//        [--state F] [--apply] [--override-variant V] [--now ISO]
//        [--window N] [--window-days N] [--min-samples N] [--alpha X]
//        [--json] [--github-output]
//   node scripts/ab-experiment.js serving --experiment ID [--state F]
//        [--override-variant V]
//   node scripts/ab-experiment.js promote --experiment ID --variant V
//        --state F [--reason TEXT]
//   node scripts/ab-experiment.js rollback --experiment ID --state F
//        [--reason TEXT]
//
// Env: AI_SDLC_EXPERIMENTS_CONFIG (config path), AI_SDLC_AB_STATE (state
// file), AI_SDLC_AB_OVERRIDE (variant override), AI_SDLC_AB_DISABLE=1
// (kill switch — serving always resolves control; decide still reports).
//
// Exit: 0 = ok (decision emitted / serving resolved / transition applied),
// 2 = usage, config or state error. `decide` is a report, not a gate — all
// four decisions exit 0.
'use strict';

const fs = require('fs');
const path = require('path');
const os = require('os');

const DEFAULT_CONFIG = path.join(__dirname, '..', 'config', 'experiments.json');
const DEFAULT_ALPHA = 0.05;
const DEFAULT_MIN_SAMPLES = 30;
const DEFAULT_WINDOW = 200;
const MAX_WINDOW = 100000;
const MAX_AUDIT_ENTRIES = 1000;
const MAX_HISTORY_ENTRIES = 200;

const VALID_STATUSES = new Set(['draft', 'running', 'paused', 'promoted', 'rolled_back', 'concluded']);
const RATE_METRICS = new Set(['pass_rate', 'success_rate', 'win_rate']);
const CONTINUOUS_METRICS = new Set(['score', 'iterations', 'tokens', 'duration_ms', 'latency_ms', 'cost_usd']);
const VALID_METRICS = new Set([...RATE_METRICS, ...CONTINUOUS_METRICS]);
const SLUG_RE = /^[\w.-]{1,64}$/;

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

function isFiniteNum(v) { return typeof v === 'number' && Number.isFinite(v); }
function mean(arr) { return arr.length ? arr.reduce((s, x) => s + x, 0) / arr.length : null; }
function variance(arr) {
  if (!arr || arr.length < 2) return 0;
  const m = mean(arr);
  return arr.reduce((s, x) => s + (x - m) * (x - m), 0) / (arr.length - 1);
}
function parseTs(v) {
  if (v === undefined || v === null) return null;
  const t = new Date(v).getTime();
  return Number.isFinite(t) ? t : null;
}

// ---------------------------------------------------------------------------
// Experiment config: load + validate
// ---------------------------------------------------------------------------

function normalizeMetric(m) {
  const name = typeof m === 'string' ? m : (m && m.name);
  if (!VALID_METRICS.has(name)) return null;
  const direction = (m && m.direction)
    || (RATE_METRICS.has(name) || name === 'score' ? 'higher' : 'lower');
  if (direction !== 'higher' && direction !== 'lower') return null;
  return { name, direction };
}

function validateExperiment(exp) {
  const errors = [];
  if (!exp || typeof exp !== 'object' || Array.isArray(exp)) {
    return { ok: false, errors: ['experiment must be an object'] };
  }
  if (!exp.id || !SLUG_RE.test(String(exp.id))) {
    errors.push('id missing or not a slug ([\\w.-]{1,64})');
  }
  if (exp.status !== undefined && !VALID_STATUSES.has(exp.status)) {
    errors.push(`status '${exp.status}' invalid (${[...VALID_STATUSES].join('|')})`);
  }
  if (!normalizeMetric(exp.metric)) {
    errors.push(`metric missing/invalid (allowed: ${[...VALID_METRICS].join(', ')}; direction: higher|lower)`);
  }
  if (exp.alpha !== undefined && !(isFiniteNum(exp.alpha) && exp.alpha > 0 && exp.alpha < 1)) {
    errors.push('alpha must be a number in (0,1)');
  }
  if (exp.minSamplesPerArm !== undefined
    && (!Number.isInteger(exp.minSamplesPerArm) || exp.minSamplesPerArm < 1)) {
    errors.push('minSamplesPerArm must be an integer >= 1');
  }
  if (exp.window !== undefined
    && (!Number.isInteger(exp.window) || exp.window < 1 || exp.window > MAX_WINDOW)) {
    errors.push(`window must be an integer in [1, ${MAX_WINDOW}]`);
  }
  if (exp.windowDays !== undefined && !(isFiniteNum(exp.windowDays) && exp.windowDays > 0)) {
    errors.push('windowDays must be a number > 0');
  }
  if (!Array.isArray(exp.variants) || exp.variants.length === 0) {
    errors.push('variants must be a non-empty array');
  } else {
    const names = new Set();
    for (const v of exp.variants) {
      if (!v || typeof v !== 'object' || !v.name || !SLUG_RE.test(String(v.name))) {
        errors.push('variant missing or invalid name');
        continue;
      }
      if (names.has(v.name)) errors.push(`duplicate variant name '${v.name}'`);
      names.add(v.name);
      if (v.weight !== undefined && !(isFiniteNum(v.weight) && v.weight >= 0)) {
        errors.push(`variant '${v.name}' weight must be a number >= 0`);
      }
    }
    if (exp.control !== undefined && !names.has(exp.control)) {
      errors.push(`control '${exp.control}' does not name a variant`);
    }
    if (exp.winner !== undefined && !names.has(exp.winner)) {
      errors.push(`winner '${exp.winner}' does not name a variant`);
    }
  }
  return { ok: errors.length === 0, errors };
}

/**
 * Loads and validates the experiments config. Invalid experiments are
 * dropped with warnings — one bad entry must never poison the rest.
 */
function loadExperiments(file) {
  const f = file || process.env.AI_SDLC_EXPERIMENTS_CONFIG || DEFAULT_CONFIG;
  let spec;
  try {
    spec = JSON.parse(fs.readFileSync(f, 'utf8'));
  } catch (e) {
    return { file: f, version: 1, experiments: [], warnings: [`config unreadable: ${e.message}`] };
  }
  const warnings = [];
  const seen = new Set();
  const experiments = [];
  for (const exp of (spec && spec.experiments) || []) {
    const v = validateExperiment(exp);
    if (!v.ok) {
      warnings.push(`${(exp && exp.id) || '(no id)'}: ${v.errors.join('; ')}`);
      continue;
    }
    if (seen.has(exp.id)) {
      warnings.push(`duplicate experiment id '${exp.id}' — first definition wins`);
      continue;
    }
    seen.add(exp.id);
    experiments.push(exp);
  }
  return { file: f, version: spec.version || 1, experiments, warnings };
}

function getExperiment(cfg, id) {
  return cfg.experiments.find((e) => e.id === id) || null;
}

function controlName(exp) {
  if (exp.control) return exp.control;
  const names = (exp.variants || []).map((v) => v.name);
  return names.includes('control') ? 'control' : names[0];
}

/** Traffic split: declared weights normalized across variants. */
function trafficSplit(exp) {
  const variants = exp.variants || [];
  const weights = variants.map((v) => (isFiniteNum(v.weight) ? v.weight : 1));
  const total = weights.reduce((s, w) => s + w, 0);
  const out = {};
  for (let i = 0; i < variants.length; i += 1) {
    out[variants[i].name] = total > 0 ? weights[i] / total : 0;
  }
  return out;
}

// ---------------------------------------------------------------------------
// State (applied transitions) + audit log
// ---------------------------------------------------------------------------

function defaultStateFile() {
  return process.env.AI_SDLC_AB_STATE || null;
}

function loadState(file) {
  const f = file || defaultStateFile();
  if (!f) return { file: null, experiments: {} };
  try {
    const st = JSON.parse(fs.readFileSync(f, 'utf8'));
    if (st && typeof st === 'object') {
      return { file: f, version: st.version || 1, experiments: st.experiments || {} };
    }
  } catch (e) { /* missing/corrupt state file = empty state */ }
  return { file: f, version: 1, experiments: {} };
}

function saveState(state, file) {
  const f = file || defaultStateFile();
  if (!f) return false;
  try {
    fs.mkdirSync(path.dirname(f), { recursive: true });
    const tmp = `${f}.tmp`;
    fs.writeFileSync(tmp, JSON.stringify({ version: 1, ...state, file: undefined }, null, 2) + '\n', 'utf8');
    fs.renameSync(tmp, f);
    return true;
  } catch (e) {
    return false;
  }
}

function auditFileFor(stateFile) {
  return /\.json$/i.test(stateFile)
    ? stateFile.replace(/\.json$/i, '.audit.jsonl')
    : `${stateFile}.audit.jsonl`;
}

/**
 * Append an audit record — one JSONL line per applied transition or manual
 * override. The audit log is the durable "who decided what and why" trail;
 * `decide --json` output is the per-evaluation evidence for the same trail.
 */
function appendAudit(stateFile, entry, nowIso) {
  const f = auditFileFor(stateFile);
  try {
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.appendFileSync(f, JSON.stringify({ at: nowIso || new Date().toISOString(), ...entry }) + '\n', 'utf8');
    return f;
  } catch (e) {
    return null;
  }
}

function effectiveStatus(exp, state) {
  const st = state.experiments[exp.id];
  return (st && st.status) || exp.status || 'draft';
}

function effectiveWinner(exp, state) {
  const st = state.experiments[exp.id];
  return (st && st.winner) || exp.winner || null;
}

function pushHistory(cur, entry) {
  const h = Array.isArray(cur.history) ? cur.history.slice() : [];
  h.push(entry);
  return h.slice(-MAX_HISTORY_ENTRIES);
}

// ---------------------------------------------------------------------------
// Evidence: load + per-arm collection with bounded windows
// ---------------------------------------------------------------------------

/** Reads a JSON array or JSONL evidence file. Malformed lines are skipped. */
function readEvidence(file) {
  const raw = fs.readFileSync(file, 'utf8');
  const trimmed = raw.trim();
  if (!trimmed) return [];
  if (trimmed.startsWith('[')) {
    const doc = JSON.parse(raw);
    if (!Array.isArray(doc)) throw new Error('evidence file must be a JSON array or JSONL');
    return doc.filter((r) => r && typeof r === 'object');
  }
  return trimmed.split('\n')
    .map((l) => { try { return JSON.parse(l); } catch (e) { return null; } })
    .filter((r) => r && typeof r === 'object');
}

const RATE_BASE_FIELD = { pass_rate: 'pass', success_rate: 'success', win_rate: 'win' };
const GENERIC_RATE_FIELDS = ['pass', 'success', 'win'];
const POSITIVE_OUTCOMES = new Set(['success', 'passed', 'pass', 'win', 'won', 'ok']);
const NEGATIVE_OUTCOMES = new Set(['failure', 'failed', 'fail', 'loss', 'lost', 'error']);

/** Bernoulli outcome for rate metrics; null when the record carries none. */
function rateOutcome(record, metricName) {
  const mv = record.metrics && record.metrics[metricName];
  if (isFiniteNum(mv) && (mv === 0 || mv === 1)) return mv === 1;
  const base = RATE_BASE_FIELD[metricName];
  if (base && typeof record[base] === 'boolean') return record[base];
  for (const f of GENERIC_RATE_FIELDS) {
    if (typeof record[f] === 'boolean') return record[f];
  }
  if (typeof record.outcome === 'string') {
    const o = record.outcome.toLowerCase();
    if (POSITIVE_OUTCOMES.has(o)) return true;
    if (NEGATIVE_OUTCOMES.has(o)) return false;
  }
  return null;
}

/** Scalar reading for continuous metrics; null when absent. */
function metricValue(record, metricName) {
  const mv = record.metrics && record.metrics[metricName];
  if (isFiniteNum(mv)) return mv;
  if (isFiniteNum(record[metricName])) return record[metricName];
  if (isFiniteNum(record.value)) return record.value;
  return null;
}

function newArm() { return { n: 0, wins: 0, values: [] }; }

/**
 * Groups evidence records by declared variant, newest `window` per arm and
 * (when windowDays is set) no older than now − windowDays. Counts of every
 * dropped record land in `dropped` — the bound is part of the audit trail,
 * never silent.
 */
function collectArms(exp, records, opts = {}) {
  const window = Number.isInteger(opts.window) && opts.window > 0
    ? Math.min(opts.window, MAX_WINDOW)
    : (Number.isInteger(exp.window) ? exp.window : DEFAULT_WINDOW);
  const windowDays = isFiniteNum(opts.windowDays) ? opts.windowDays
    : (isFiniteNum(exp.windowDays) ? exp.windowDays : null);
  const nowMs = opts.nowMs !== undefined ? opts.nowMs : Date.now();
  const metric = normalizeMetric(exp.metric) || { name: 'pass_rate', direction: 'higher' };

  const declared = new Set((exp.variants || []).map((v) => v.name));
  const buckets = new Map([...declared].map((n) => [n, []]));
  const dropped = {
    otherExperiment: 0, unknownVariant: 0, outsideWindowDays: 0,
    undated: 0, noMetricReading: 0, beyondWindow: 0,
  };
  const unknownVariants = {};

  for (let idx = 0; idx < records.length; idx += 1) {
    const r = records[idx];
    if (r.experiment !== undefined && r.experiment !== null && r.experiment !== exp.id) {
      dropped.otherExperiment += 1;
      continue;
    }
    const variant = r.variant;
    if (!declared.has(variant)) {
      dropped.unknownVariant += 1;
      unknownVariants[String(variant)] = (unknownVariants[String(variant)] || 0) + 1;
      continue;
    }
    const ts = parseTs(r.at);
    if (windowDays !== null) {
      if (ts === null) { dropped.undated += 1; continue; }
      if (nowMs - ts > windowDays * 86400e3) { dropped.outsideWindowDays += 1; continue; }
    }
    let sample;
    if (RATE_METRICS.has(metric.name)) {
      const outcome = rateOutcome(r, metric.name);
      if (outcome === null) { dropped.noMetricReading += 1; continue; }
      sample = outcome;
    } else {
      const value = metricValue(r, metric.name);
      if (value === null) { dropped.noMetricReading += 1; continue; }
      sample = value;
    }
    buckets.get(variant).push({ ts: ts === null ? -Infinity : ts, idx, sample });
  }

  const arms = {};
  for (const [name, list] of buckets) {
    // Sort by timestamp then input order (stable, deterministic); undated
    // records sort oldest so a bounded window prefers dated evidence.
    list.sort((a, b) => (a.ts - b.ts) || (a.idx - b.idx));
    const kept = list.slice(-window);
    dropped.beyondWindow += list.length - kept.length;
    const arm = newArm();
    for (const s of kept) {
      arm.n += 1;
      if (RATE_METRICS.has(metric.name)) {
        if (s.sample === true) arm.wins += 1;
      } else {
        arm.values.push(s.sample);
      }
    }
    arms[name] = arm;
  }
  return { arms, dropped, unknownVariants, window, windowDays, metric };
}

// ---------------------------------------------------------------------------
// Statistics — ported from hermes/scripts/ab_experiments.js (issue #79):
// two-proportion z-test for rate metrics, Welch's t-test for continuous.
// ---------------------------------------------------------------------------

/** Abramowitz–Stegun 7.1.26 — |ε| < 1.5e-7. Exact at x = 0. */
function erf(x) {
  const ax = Math.abs(x);
  if (ax === 0) return 0;
  const sign = x < 0 ? -1 : 1;
  const t = 1 / (1 + 0.3275911 * ax);
  const y = 1 - (((((1.061405429 * t - 1.453152027) * t) + 1.421413741) * t - 0.284496736) * t + 0.254829592) * t * Math.exp(-ax * ax);
  return sign * y;
}

function normalCdf(z) { return 0.5 * (1 + erf(z / Math.SQRT2)); }

const LANCZOS = [
  0.99999999999980993, 676.5203681218851, -1259.1392167224028, 771.32342877765313,
  -176.61502916214059, 12.507343278686905, -0.13857109526572012,
  9.9843695780195716e-6, 1.5056327351493116e-7,
];
function lnGamma(z) {
  if (z < 0.5) return Math.log(Math.PI / Math.sin(Math.PI * z)) - lnGamma(1 - z);
  z -= 1;
  let x = LANCZOS[0];
  for (let i = 1; i < LANCZOS.length; i += 1) x += LANCZOS[i] / (z + i);
  const t = z + 7.5;
  return 0.5 * Math.log(2 * Math.PI) + (z + 0.5) * Math.log(t) - t + Math.log(x);
}

/** Continued fraction for the incomplete beta function (Numerical Recipes). */
function betacf(a, b, x) {
  const MAXIT = 200, EPS = 3e-12, FPMIN = 1e-300;
  const qab = a + b, qap = a + 1, qam = a - 1;
  let c = 1, d = 1 - qab * x / qap;
  if (Math.abs(d) < FPMIN) d = FPMIN;
  d = 1 / d;
  let h = d;
  for (let m = 1; m <= MAXIT; m += 1) {
    const m2 = 2 * m;
    let aa = m * (b - m) * x / ((qam + m2) * (a + m2));
    d = 1 + aa * d; if (Math.abs(d) < FPMIN) d = FPMIN;
    c = 1 + aa / c; if (Math.abs(c) < FPMIN) c = FPMIN;
    d = 1 / d; h *= d * c;
    aa = -(a + m) * (qab + m) * x / ((a + m2) * (qap + m2));
    d = 1 + aa * d; if (Math.abs(d) < FPMIN) d = FPMIN;
    c = 1 + aa / c; if (Math.abs(c) < FPMIN) c = FPMIN;
    d = 1 / d;
    const del = d * c; h *= del;
    if (Math.abs(del - 1) < EPS) break;
  }
  return h;
}

/** Regularized incomplete beta I_x(a,b). */
function ibeta(x, a, b) {
  if (x <= 0) return 0;
  if (x >= 1) return 1;
  const bt = Math.exp(lnGamma(a + b) - lnGamma(a) - lnGamma(b) + a * Math.log(x) + b * Math.log(1 - x));
  if (x < (a + 1) / (a + b + 2)) return bt * betacf(a, b, x) / a;
  return 1 - bt * betacf(b, a, 1 - x) / b;
}

/** Two-sided p-value for a t-statistic with df degrees of freedom. */
function tDistTwoSidedP(t, df) {
  if (!Number.isFinite(t)) return t === 0 ? 1 : 0;
  const v = Math.max(df, 1);
  return ibeta(v / (v + t * t), v / 2, 0.5);
}

/**
 * Two-proportion z-test. diff = variantRate − controlRate (positive = the
 * variant wins more often). Degenerate SE (pooled p at 0 or 1) resolves by
 * direct comparison.
 */
function twoProportionZTest(succControl, nControl, succVariant, nVariant) {
  if (!nControl || !nVariant) {
    return { diff: null, z: null, pValue: null, reason: 'empty arm' };
  }
  const pC = succControl / nControl;
  const pV = succVariant / nVariant;
  const pooled = (succControl + succVariant) / (nControl + nVariant);
  const se = Math.sqrt(pooled * (1 - pooled) * (1 / nControl + 1 / nVariant));
  const diff = pV - pC;
  if (se === 0) {
    return { diff, z: diff === 0 ? 0 : (diff > 0 ? Infinity : -Infinity), pValue: diff === 0 ? 1 : 0, pooled };
  }
  const z = diff / se;
  const pValue = Math.min(1, Math.max(0, 2 * (1 - normalCdf(Math.abs(z)))));
  return { diff, z, pValue, pooled };
}

/** Welch's t-test (unequal variances). Same sign convention as the z-test. */
function welchTTest(controlSamples, variantSamples) {
  const n1 = controlSamples.length, n2 = variantSamples.length;
  if (n1 < 2 || n2 < 2) {
    return { diff: n1 && n2 ? mean(variantSamples) - mean(controlSamples) : null, t: null, df: null, pValue: null, reason: 'n<2' };
  }
  const m1 = mean(controlSamples), m2 = mean(variantSamples);
  const v1 = variance(controlSamples), v2 = variance(variantSamples);
  const se2 = v1 / n1 + v2 / n2;
  const diff = m2 - m1;
  if (se2 === 0) {
    return { diff, t: diff === 0 ? 0 : (diff > 0 ? Infinity : -Infinity), df: n1 + n2 - 2, pValue: diff === 0 ? 1 : 0 };
  }
  const t = diff / Math.sqrt(se2);
  const df = (se2 * se2) / (((v1 / n1) * (v1 / n1)) / (n1 - 1) + ((v2 / n2) * (v2 / n2)) / (n2 - 1));
  return { diff, t, df: Math.max(df, 1), pValue: tDistTwoSidedP(Math.abs(t), Math.max(df, 1)) };
}

/**
 * Compare a variant arm against control on the experiment's metric.
 * diff is always (variant − control); favorable depends on direction.
 */
function compareArms(controlArm, variantArm, metric, alpha) {
  const cmp = {
    metric: metric.name,
    direction: metric.direction,
    diff: null, pValue: null, significant: false, favorable: false,
    controlValue: null, variantValue: null,
  };
  if (RATE_METRICS.has(metric.name)) {
    cmp.controlValue = controlArm.n ? controlArm.wins / controlArm.n : null;
    cmp.variantValue = variantArm.n ? variantArm.wins / variantArm.n : null;
    const t = twoProportionZTest(controlArm.wins, controlArm.n, variantArm.wins, variantArm.n);
    cmp.diff = t.diff; cmp.pValue = t.pValue; cmp.statistic = t.z;
  } else {
    cmp.controlValue = mean(controlArm.values);
    cmp.variantValue = mean(variantArm.values);
    if (!controlArm.values.length || !variantArm.values.length) {
      cmp.reason = 'no samples for metric';
      return cmp;
    }
    const t = welchTTest(controlArm.values, variantArm.values);
    cmp.diff = t.diff; cmp.pValue = t.pValue; cmp.statistic = t.t; cmp.df = t.df;
  }
  if (cmp.pValue === null) return cmp;
  cmp.favorable = metric.direction === 'higher' ? cmp.diff > 0 : cmp.diff < 0;
  cmp.significant = cmp.pValue <= alpha;
  return cmp;
}

// ---------------------------------------------------------------------------
// Serving + decision
// ---------------------------------------------------------------------------

/**
 * The variant a subsequent run should use: kill switch → control; explicit
 * override → override; applied/declared promotion → winner; otherwise
 * control. Override that does not name a declared variant is ignored with a
 * warning (fail-operational on routing, fail-visible in the output).
 */
function resolveServing(exp, state, opts = {}) {
  const control = controlName(exp);
  const warnings = [];
  if (opts.disabled || process.env.AI_SDLC_AB_DISABLE === '1') {
    return { variant: control, source: 'disabled', warnings };
  }
  const override = opts.override !== undefined ? opts.override : process.env.AI_SDLC_AB_OVERRIDE;
  if (override) {
    const names = new Set((exp.variants || []).map((v) => v.name));
    if (names.has(override)) return { variant: override, source: 'override', warnings };
    warnings.push(`override '${override}' is not a declared variant — ignored`);
  }
  const status = effectiveStatus(exp, state);
  const winner = effectiveWinner(exp, state);
  if (status === 'promoted' && winner) {
    return { variant: winner, source: 'promotion', warnings };
  }
  return { variant: control, source: status === 'rolled_back' ? 'rollback' : 'control', warnings };
}

function summarizeArm(arm, metricName) {
  const s = { n: arm.n };
  if (RATE_METRICS.has(metricName)) {
    s.wins = arm.wins;
    s.rate = arm.n ? arm.wins / arm.n : null;
  } else {
    s.mean = mean(arm.values);
  }
  return s;
}

/**
 * Full evaluation: bounded arm summaries + per-variant statistical
 * comparison vs control + the lifecycle decision.
 *
 * opts: { state, evidenceRecords, now, window, windowDays, minSamples,
 *         alpha, override, disabled }
 */
function evaluateExperiment(exp, opts = {}) {
  const state = opts.state || { experiments: {} };
  const metric = normalizeMetric(exp.metric) || { name: 'pass_rate', direction: 'higher' };
  const alpha = isFiniteNum(opts.alpha) ? opts.alpha : (isFiniteNum(exp.alpha) ? exp.alpha : DEFAULT_ALPHA);
  const minN = Number.isInteger(opts.minSamples) ? opts.minSamples
    : (Number.isInteger(exp.minSamplesPerArm) ? exp.minSamplesPerArm : DEFAULT_MIN_SAMPLES);
  const control = controlName(exp);
  const nowMs = (opts.now && parseTs(opts.now)) || Date.now();

  const records = Array.isArray(opts.evidenceRecords) ? opts.evidenceRecords : [];
  const collected = collectArms(exp, records, {
    window: opts.window, windowDays: opts.windowDays, nowMs,
  });
  const { arms, dropped } = collected;
  const controlArm = arms[control] || newArm();

  const comparisons = [];
  for (const v of exp.variants || []) {
    if (v.name === control) continue;
    const arm = arms[v.name] || newArm();
    const cmp = compareArms(controlArm, arm, metric, alpha);
    cmp.variant = v.name;
    cmp.nControl = controlArm.n;
    cmp.nVariant = arm.n;
    cmp.sufficient = controlArm.n >= minN && arm.n >= minN && cmp.pValue !== null;
    comparisons.push(cmp);
  }

  const status = effectiveStatus(exp, state);
  const winner = effectiveWinner(exp, state);
  const viable = comparisons.filter((c) => c.sufficient);
  const winners = viable.filter((c) => c.significant && c.favorable);
  let bestArm = null;
  if (winners.length) {
    const better = (a, b) => (metric.direction === 'higher' ? a.diff > b.diff : a.diff < b.diff);
    bestArm = winners.reduce((best, c) => (better(c, best) ? c : best), winners[0]).variant;
  }

  const serving = resolveServing(exp, state, opts);
  const reasons = [];
  let action;
  let target = null;
  if (bestArm && bestArm !== winner) {
    action = 'promote';
    target = bestArm;
    const cmp = winners.find((c) => c.variant === bestArm);
    reasons.push(
      `variant '${bestArm}' beats control '${control}' on ${metric.name}` +
      ` (diff=${cmp.diff}, p=${cmp.pValue} <= alpha=${alpha})`,
    );
  } else if (bestArm && bestArm === winner) {
    action = 'continue';
    reasons.push(`promoted winner '${bestArm}' remains the significant best arm`);
  } else if (status === 'promoted' && winner) {
    const wCmp = comparisons.find((c) => c.variant === winner);
    if (wCmp && wCmp.sufficient && wCmp.significant && !wCmp.favorable) {
      action = 'rollback';
      target = control;
      reasons.push(
        `promoted winner '${winner}' is significantly worse than control '${control}'` +
        ` (diff=${wCmp.diff}, p=${wCmp.pValue} <= alpha=${alpha})`,
      );
    } else if (viable.length) {
      action = 'continue';
      reasons.push(`promoted winner '${winner}' shows no significant regression vs control`);
    } else {
      action = 'insufficient_data';
      reasons.push(`no comparison reached minSamplesPerArm=${minN}; serving stays '${serving.variant}'`);
    }
  } else if (viable.length) {
    action = 'continue';
    reasons.push('powered comparison(s) show no significant winner — keep collecting evidence');
  } else {
    action = 'insufficient_data';
    reasons.push(`no comparison reached minSamplesPerArm=${minN}`);
  }

  return {
    experiment: {
      id: exp.id,
      status,
      control,
      metric,
      alpha,
      minSamplesPerArm: minN,
      window: collected.window,
      windowDays: collected.windowDays,
      traffic: trafficSplit(exp),
      winner,
    },
    arms: Object.fromEntries(
      Object.entries(arms).map(([k, a]) => [k, summarizeArm(a, metric.name)]),
    ),
    dropped: { ...dropped, unknownVariants: collected.unknownVariants },
    comparisons,
    bestArm,
    decision: { action, target, reasons },
    serving: serving.variant,
    servingSource: serving.source,
    warnings: serving.warnings,
    evaluatedAt: opts.now || new Date(nowMs).toISOString(),
  };
}

// ---------------------------------------------------------------------------
// Applied transitions — persisted state + audit trail (reversible)
// ---------------------------------------------------------------------------

/**
 * Records the evaluation decision in the state file + audit log. promote and
 * rollback mutate lifecycle; continue/insufficient_data only audit.
 */
function applyDecision(exp, result, opts = {}) {
  const stateFile = opts.stateFile || defaultStateFile();
  if (!stateFile) {
    return { applied: false, reason: 'no state file — pass --state or set AI_SDLC_AB_STATE' };
  }
  const state = loadState(stateFile);
  const cur = state.experiments[exp.id] || {};
  const now = opts.now || new Date().toISOString();
  const { action, target } = result.decision;
  const auditEntry = {
    experiment: exp.id,
    event: `decision:${action}`,
    target,
    metric: result.experiment.metric.name,
    alpha: result.experiment.alpha,
    decidedBy: opts.decidedBy || 'evaluator',
    reasons: result.decision.reasons,
  };

  if (action === 'promote') {
    const from = effectiveStatus(exp, state);
    state.experiments[exp.id] = Object.assign({}, cur, {
      status: 'promoted',
      winner: target,
      promotedAt: now,
      promotion: {
        at: now, variant: target, decidedBy: 'evaluator',
        metric: result.experiment.metric.name, alpha: result.experiment.alpha,
        comparisons: result.comparisons.map((c) => ({
          variant: c.variant, diff: c.diff, pValue: c.pValue,
          significant: c.significant, favorable: c.favorable, sufficient: c.sufficient,
        })),
      },
      history: pushHistory(cur, { at: now, event: 'promote', from, winner: target, decidedBy: 'evaluator' }),
    });
  } else if (action === 'rollback') {
    state.experiments[exp.id] = Object.assign({}, cur, {
      status: 'rolled_back',
      previousWinner: cur.winner || effectiveWinner(exp, state) || null,
      winner: null,
      rolledBackAt: now,
      rollback: { at: now, from: cur.winner || null, reason: result.decision.reasons.join('; ') },
      history: pushHistory(cur, { at: now, event: 'rollback', from: 'promoted', winner: cur.winner || null }),
    });
  }

  if (!saveState(state, stateFile)) {
    return { applied: false, reason: 'state file write failed' };
  }
  const auditFile = appendAudit(stateFile, auditEntry, now);
  return { applied: true, stateFile, auditFile, action, target };
}

/** Manual promotion — the operator override path (decidedBy 'manual'). */
function promoteExperiment(exp, variant, opts = {}) {
  const stateFile = opts.stateFile || defaultStateFile();
  if (!stateFile) return { promoted: false, reason: 'no state file — pass --state or set AI_SDLC_AB_STATE' };
  const names = (exp.variants || []).map((v) => v.name);
  if (!names.includes(variant)) {
    return { promoted: false, reason: `variant '${variant}' is not defined in experiment '${exp.id}'` };
  }
  const state = loadState(stateFile);
  const cur = state.experiments[exp.id] || {};
  const from = effectiveStatus(exp, state);
  if (from === 'promoted' && cur.winner === variant) {
    return { promoted: false, reason: `already promoted to '${variant}'` };
  }
  const now = opts.now || new Date().toISOString();
  state.experiments[exp.id] = Object.assign({}, cur, {
    status: 'promoted',
    winner: variant,
    promotedAt: now,
    promotion: { at: now, variant, decidedBy: 'manual', reason: opts.reason || null },
    history: pushHistory(cur, { at: now, event: 'promote', from, winner: variant, decidedBy: 'manual' }),
  });
  if (!saveState(state, stateFile)) return { promoted: false, reason: 'state file write failed' };
  appendAudit(stateFile, { experiment: exp.id, event: 'promote', target: variant, decidedBy: 'manual', reason: opts.reason || null }, now);
  return { promoted: true, winner: variant, from };
}

/** Manual rollback — restores control as the serving variant. */
function rollbackExperiment(exp, opts = {}) {
  const stateFile = opts.stateFile || defaultStateFile();
  if (!stateFile) return { rolledBack: false, reason: 'no state file — pass --state or set AI_SDLC_AB_STATE' };
  const state = loadState(stateFile);
  const cur = state.experiments[exp.id] || {};
  const status = effectiveStatus(exp, state);
  if (status !== 'promoted') {
    return { rolledBack: false, reason: `status is '${status}' — only promoted experiments roll back` };
  }
  const now = opts.now || new Date().toISOString();
  state.experiments[exp.id] = Object.assign({}, cur, {
    status: 'rolled_back',
    previousWinner: cur.winner || null,
    winner: null,
    rolledBackAt: now,
    rollback: { at: now, from: cur.winner || null, reason: opts.reason || 'manual rollback' },
    history: pushHistory(cur, { at: now, event: 'rollback', from: 'promoted', winner: cur.winner || null }),
  });
  if (!saveState(state, stateFile)) return { rolledBack: false, reason: 'state file write failed' };
  appendAudit(stateFile, { experiment: exp.id, event: 'rollback', from: cur.winner || null, decidedBy: 'manual', reason: opts.reason || null }, now);
  return { rolledBack: true, previousWinner: cur.winner || null };
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

function usage() {
  return [
    'usage: ab-experiment.js <op> [options]',
    '',
    'ops:',
    '  list      declared experiments + effective status + serving variant',
    '  decide    --experiment ID --evidence FILE [--apply] — evaluate evidence',
    '            and emit the promotion decision (JSON)',
    '  serving   --experiment ID — the variant subsequent runs should use',
    '  promote   --experiment ID --variant V --state F — manual override',
    '  rollback  --experiment ID --state F — revert a promotion to control',
    '',
    'options:',
    '  --config F            experiments config (default config/experiments.json;',
    '                        AI_SDLC_EXPERIMENTS_CONFIG overrides)',
    '  --state F             lifecycle state file (AI_SDLC_AB_STATE)',
    '  --evidence F          run/eval evidence — JSON array or JSONL',
    '  --experiment ID       experiment slug',
    '  --variant V           variant name',
    '  --apply               persist the decision to the state file + audit log',
    '  --override-variant V  force serving to V (AI_SDLC_AB_OVERRIDE)',
    '  --now ISO             inject the clock (deterministic replay)',
    '  --window N            max evidence records per arm',
    '  --window-days N       drop evidence older than N days',
    '  --min-samples N       minimum samples per arm for a powered comparison',
    '  --alpha X             significance threshold (default 0.05)',
    '  --reason TEXT         recorded on manual promote/rollback',
    '  --json                JSON output (decide default)',
    '  --github-output       write decision/serving outputs to $GITHUB_OUTPUT',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = { op: null, json: false, githubOutput: false, apply: false };
  const take = (i) => {
    if (i + 1 >= argv.length) throw new Error(`missing value for ${argv[i]}`);
    return argv[i + 1];
  };
  let i = 0;
  if (argv.length && !argv[0].startsWith('-')) {
    opts.op = argv[0];
    i = 1;
  }
  for (; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === '--config') opts.config = take(i++);
    else if (a === '--state') opts.stateFile = take(i++);
    else if (a === '--evidence') opts.evidence = take(i++);
    else if (a === '--experiment') opts.experiment = take(i++);
    else if (a === '--variant') opts.variant = take(i++);
    else if (a === '--override-variant') opts.override = take(i++);
    else if (a === '--now') opts.now = take(i++);
    else if (a === '--window') opts.window = Number(take(i++));
    else if (a === '--window-days') opts.windowDays = Number(take(i++));
    else if (a === '--min-samples') opts.minSamples = Number(take(i++));
    else if (a === '--alpha') opts.alpha = Number(take(i++));
    else if (a === '--reason') opts.reason = take(i++);
    else if (a === '--apply') opts.apply = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function requireExperiment(opts) {
  const cfg = loadExperiments(opts.config);
  for (const w of cfg.warnings) console.error(`[!] ${w}`);
  if (!opts.experiment) throw new Error('missing --experiment ID');
  const exp = getExperiment(cfg, opts.experiment);
  if (!exp) throw new Error(`experiment '${opts.experiment}' not found in ${cfg.file}`);
  return exp;
}

function cmdList(opts, emit) {
  const cfg = loadExperiments(opts.config);
  const state = loadState(opts.stateFile);
  const rows = cfg.experiments.map((exp) => {
    const serving = resolveServing(exp, state, opts);
    const metric = normalizeMetric(exp.metric);
    return {
      id: exp.id,
      status: effectiveStatus(exp, state),
      control: controlName(exp),
      metric: metric.name,
      direction: metric.direction,
      traffic: trafficSplit(exp),
      winner: effectiveWinner(exp, state),
      serving: serving.variant,
      servingSource: serving.source,
      description: exp.description || null,
    };
  });
  if (opts.json) emit(JSON.stringify({ config: cfg.file, warnings: cfg.warnings, experiments: rows }, null, 2));
  else {
    for (const w of cfg.warnings) emit(`[!] ${w}`);
    if (!rows.length) emit(`No valid experiments in ${cfg.file}`);
    for (const r of rows) {
      emit(`• ${r.id} — ${r.status} (metric=${r.metric}/${r.direction}, serving='${r.serving}' via ${r.servingSource})`);
      if (r.description) emit(`    ${r.description}`);
    }
  }
  return 0;
}

function cmdDecide(opts, emit) {
  const exp = requireExperiment(opts);
  if (!opts.evidence) throw new Error('decide requires --evidence FILE');
  const records = readEvidence(opts.evidence);
  const state = loadState(opts.stateFile);
  const result = evaluateExperiment(exp, {
    state,
    evidenceRecords: records,
    now: opts.now,
    window: opts.window,
    windowDays: opts.windowDays,
    minSamples: opts.minSamples,
    alpha: opts.alpha,
    override: opts.override,
    disabled: opts.disabled,
  });

  if (opts.apply) {
    result.applied = applyDecision(exp, result, { stateFile: opts.stateFile, now: opts.now });
    if (result.applied.applied && result.applied.target) {
      result.serving = result.decision.action === 'promote' ? result.applied.target : controlName(exp);
      result.servingSource = result.decision.action;
    }
  }

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      `decision=${result.decision.action}`,
      `decision_target=${result.decision.target || ''}`,
      `serving=${result.serving}`,
      `best_arm=${result.bestArm || ''}`,
    ];
    fs.appendFileSync(process.env.GITHUB_OUTPUT, lines.join('\n') + '\n');
  }

  for (const w of result.warnings || []) console.error(`[!] ${w}`);
  emit(JSON.stringify(result, null, 2));
  return 0;
}

function cmdServing(opts, emit) {
  const exp = requireExperiment(opts);
  const state = loadState(opts.stateFile);
  const serving = resolveServing(exp, state, opts);
  for (const w of serving.warnings) console.error(`[!] ${w}`);
  if (opts.json) {
    emit(JSON.stringify({ experiment: exp.id, status: effectiveStatus(exp, state), ...serving }));
  } else {
    emit(serving.variant);
  }
  return 0;
}

function cmdPromote(opts, emit) {
  const exp = requireExperiment(opts);
  if (!opts.variant) throw new Error('promote requires --variant V');
  const r = promoteExperiment(exp, opts.variant, { stateFile: opts.stateFile, reason: opts.reason, now: opts.now });
  emit(JSON.stringify(r));
  return r.promoted ? 0 : 2;
}

function cmdRollback(opts, emit) {
  const exp = requireExperiment(opts);
  const r = rollbackExperiment(exp, { stateFile: opts.stateFile, reason: opts.reason, now: opts.now });
  emit(JSON.stringify(r));
  return r.rolledBack ? 0 : 2;
}

function main(argv, { stdout } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  switch (opts.op) {
    case 'list': return cmdList(opts, emit);
    case 'decide': return cmdDecide(opts, emit);
    case 'serving': return cmdServing(opts, emit);
    case 'promote': return cmdPromote(opts, emit);
    case 'rollback': return cmdRollback(opts, emit);
    case null: throw new Error(`no op given\n${usage()}`);
    default: throw new Error(`unknown op '${opts.op}'\n${usage()}`);
  }
}

if (require.main === module) {
  try {
    process.exit(main(process.argv.slice(2)));
  } catch (e) {
    console.error(`ab-experiment: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  DEFAULT_CONFIG,
  DEFAULT_ALPHA,
  DEFAULT_MIN_SAMPLES,
  DEFAULT_WINDOW,
  RATE_METRICS,
  CONTINUOUS_METRICS,
  VALID_STATUSES,
  normalizeMetric,
  validateExperiment,
  loadExperiments,
  controlName,
  trafficSplit,
  loadState,
  saveState,
  auditFileFor,
  appendAudit,
  effectiveStatus,
  effectiveWinner,
  readEvidence,
  rateOutcome,
  metricValue,
  collectArms,
  twoProportionZTest,
  welchTTest,
  compareArms,
  resolveServing,
  evaluateExperiment,
  applyDecision,
  promoteExperiment,
  rollbackExperiment,
  parseArgs,
  main,
};
