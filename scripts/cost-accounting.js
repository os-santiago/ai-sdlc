#!/usr/bin/env node
// scripts/cost-accounting.js — per-run token usage + cost ledger for the
// issue→merge lifecycle (issue #195; epic #148 "evidence and economics").
//
// Every LLM-bearing runtime stage (intake | implement | review | ci-repair |
// automerge | verify) appends one JSONL usage record carrying prompt and
// completion token counts; cost is ALWAYS computed here from the declared
// price table (config/cost_models.json) — the engine's own
// `estimated_cost_usd` is preserved as `engine_cost_usd` for auditing, never
// trusted for rollup math.
//
// Ledger-forward-compat: each record is a self-contained JSON object with
// the same envelope the signed run ledger (issue #196,
// .github/actions/run-ledger) digests — v/stage/ts/repo/issue/pr/run — so
// the record can be passed verbatim as a ledger-append `data` payload:
//   rec=$(node scripts/cost-accounting.js record --stage implement \
//           --issue 42 --manifest scc-manifest.json --ledger usage.jsonl)
//   run-ledger append --stage implement --issue 42 --data "$rec"
//
// Ops:
//   record --ledger F --stage S [--repo o/r] [--issue N] [--pr N]
//          [--model M --tokens-in N --tokens-out N] [--manifest FILE]
//          [--cost-usd X] [--outcome S] [--data JSON] [--github-output]
//   rollup --ledger F [--issue N] [--repo o/r] [--since ISO] [--until ISO]
//          [--period 24h|7d|30d|all] [--json|--markdown]
//   check  --ledger F [--issue N] [--repo o/r] [--strict]
//   price  --model M --tokens-in N --tokens-out N
//
// Config: config/cost_models.json (override: AI_SDLC_COST_CONFIG or --config).
// Bounds (env-overridable): AI_SDLC_COST_MAX_LEDGER_BYTES=16MiB,
//   AI_SDLC_COST_MAX_ENTRIES=20000. Malformed ledger lines are skipped with a
//   stderr note — accounting must never break the pipeline.
//
// Exit codes: 0 ok · 1 budget breached (check --strict only) · 2 usage/I-O.
'use strict';

const fs = require('fs');
const path = require('path');

const FORMAT_V = 1;
const RECORD_TYPE = 'ai-sdlc-usage';
const DEFAULT_CONFIG = path.join(__dirname, '..', 'config', 'cost_models.json');

const STAGES = ['intake', 'implement', 'review', 'ci-repair', 'automerge', 'verify'];
// Stages whose success ends the issue→merge lifecycle.
const MERGE_STAGES = new Set(['automerge', 'merge', 'verify']);

const MAX_LEDGER_BYTES = intEnv('AI_SDLC_COST_MAX_LEDGER_BYTES', 16 * 1024 * 1024);
const MAX_ENTRIES = intEnv('AI_SDLC_COST_MAX_ENTRIES', 20000);

function intEnv(name, dflt) {
  const v = Number(process.env[name]);
  return Number.isFinite(v) && v > 0 ? Math.floor(v) : dflt;
}

/* Throw-with-exit-code (not process.exit) so unit tests can import and
 * exercise these functions; the CLI wrapper at the bottom converts to the
 * documented exit codes. */
function fail(msg, code = 2) {
  const e = new Error(`cost-accounting: ${msg}`);
  e.exitCode = code;
  throw e;
}

/* --------------------------------- config ---------------------------------- */

function loadCostConfig(configPath) {
  const fallback = {
    currency: 'USD',
    budgets: {
      per_issue_usd: 0, per_issue_tokens: 0,
      daily_usd: 0, weekly_usd: 0, monthly_usd: 0,
    },
    alert_thresholds: [0.5, 0.8, 1.0],
    models: { default: { input_per_m: 1.0, output_per_m: 3.0 } },
  };
  const file = configPath || process.env.AI_SDLC_COST_CONFIG || DEFAULT_CONFIG;
  try {
    const doc = JSON.parse(fs.readFileSync(file, 'utf8'));
    return {
      currency: doc.currency || fallback.currency,
      budgets: { ...fallback.budgets, ...(doc.budgets || {}) },
      alert_thresholds: Array.isArray(doc.alert_thresholds) && doc.alert_thresholds.length
        ? doc.alert_thresholds.slice().sort((a, b) => a - b)
        : fallback.alert_thresholds,
      models: { ...fallback.models, ...(doc.models || {}) },
    };
  } catch (e) {
    if (configPath || process.env.AI_SDLC_COST_CONFIG) {
      fail(`cannot read cost config ${file}: ${e.message}`);
    }
    return fallback; // no config on disk → declared defaults
  }
}

/* --------------------------------- pricing --------------------------------- */

/* Exact → case-insensitive → provider-suffix (gpt-4o matches openai/gpt-4o)
 * → 'default'. Deterministic: a model never silently prices at zero unless
 * its table entry says so. */
function resolvePrice(model, models) {
  const table = models || {};
  const id = String(model || '').trim();
  if (id && table[id]) return { key: id, ...table[id] };
  const lower = id.toLowerCase();
  for (const [key, p] of Object.entries(table)) {
    if (key.toLowerCase() === lower) return { key, ...p };
  }
  if (lower) {
    for (const [key, p] of Object.entries(table)) {
      const k = key.toLowerCase();
      if (k !== 'default' && (k.endsWith(`/${lower}`) || lower.endsWith(`/${k}`))) {
        return { key, ...p };
      }
    }
  }
  const d = table.default || { input_per_m: 1.0, output_per_m: 3.0 };
  return { key: 'default', ...d };
}

function priceTokens(model, tokensIn, tokensOut, models) {
  const price = resolvePrice(model, models);
  const inCost = (Number(tokensIn) || 0) * ((price.input_per_m || 0) / 1e6);
  const outCost = (Number(tokensOut) || 0) * ((price.output_per_m || 0) / 1e6);
  return {
    cost_usd: Number((inCost + outCost).toFixed(6)),
    price: {
      key: price.key,
      input_per_m: price.input_per_m || 0,
      output_per_m: price.output_per_m || 0,
    },
  };
}

/* ------------------------------- record (op) -------------------------------- */

function runContext() {
  const e = process.env;
  if (!e.GITHUB_RUN_ID) return null;
  const run = {
    id: e.GITHUB_RUN_ID,
    attempt: e.GITHUB_RUN_ATTEMPT || null,
    workflow: e.GITHUB_WORKFLOW || null,
    job: e.GITHUB_JOB || null,
    sha: e.GITHUB_SHA || null,
  };
  if (e.GITHUB_SERVER_URL && e.GITHUB_REPOSITORY) {
    run.url = `${e.GITHUB_SERVER_URL}/${e.GITHUB_REPOSITORY}/actions/runs/${e.GITHUB_RUN_ID}`;
  }
  return run;
}

/* Pull token/model/cost signals out of an engine run manifest
 * (--summary-file per docs/engine_contract.md). Field names are read
 * flexibly — engines versions differ — but the manifest is evidence, not
 * config: unknown/missing fields read as zero, never fatal. */
function readManifest(file) {
  let doc;
  try {
    doc = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (e) {
    fail(`cannot read manifest ${file}: ${e.message}`);
  }
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) {
    fail(`manifest is not a JSON object: ${file}`);
  }
  const num = (...keys) => {
    for (const k of keys) {
      const v = doc[k] ?? (doc.usage && doc.usage[k]);
      if (Number.isFinite(Number(v))) return Number(v);
    }
    return 0;
  };
  return {
    model: doc.model || (doc.usage && doc.usage.model) || null,
    tokens_in: num('tokens_in', 'tokens_input', 'input_tokens', 'prompt_tokens'),
    tokens_out: num('tokens_out', 'tokens_output', 'output_tokens', 'completion_tokens'),
    engine_cost_usd: Number.isFinite(Number(doc.estimated_cost_usd)) ? Number(doc.estimated_cost_usd) : null,
    outcome: doc.resolution || doc.exit_reason || (doc.success === true ? 'success' : doc.success === false ? 'failure' : null),
    iterations: num('iterations', 'steps'),
    duration_ms: num('duration_ms', 'durationMs'),
  };
}

function buildRecord(opts, config) {
  const stage = String(opts.stage || '').trim();
  if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(stage)) fail(`invalid stage name: ${stage}`);

  const manifest = opts.manifest ? readManifest(opts.manifest) : {};
  const model = opts.model || manifest.model || 'unknown';
  const tokensIn = opts.tokens_in != null ? Number(opts.tokens_in) : (manifest.tokens_in || 0);
  const tokensOut = opts.tokens_out != null ? Number(opts.tokens_out) : (manifest.tokens_out || 0);
  if (!Number.isFinite(tokensIn) || tokensIn < 0) fail(`invalid --tokens-in: ${opts.tokens_in}`);
  if (!Number.isFinite(tokensOut) || tokensOut < 0) fail(`invalid --tokens-out: ${opts.tokens_out}`);

  // Declared table wins; an explicit --cost-usd override is accepted for
  // engines that bill per-run (not per-token) but is recorded as such.
  const priced = priceTokens(model, tokensIn, tokensOut, config.models);
  const costOverride = opts.cost_usd != null ? Number(opts.cost_usd) : null;
  if (costOverride != null && (!Number.isFinite(costOverride) || costOverride < 0)) {
    fail(`invalid --cost-usd: ${opts.cost_usd}`);
  }

  let meta = {};
  if (opts.data) {
    try { meta = JSON.parse(opts.data); } catch (e) { fail(`--data is not valid JSON: ${e.message}`); }
    if (meta === null || typeof meta !== 'object' || Array.isArray(meta)) fail('--data must be a JSON object');
  }
  if (manifest.iterations) meta.iterations = manifest.iterations;
  if (manifest.duration_ms) meta.duration_ms = manifest.duration_ms;

  return {
    v: FORMAT_V,
    type: RECORD_TYPE,
    stage,
    ts: new Date().toISOString(),
    repo: opts.repo || process.env.GITHUB_REPOSITORY || null,
    issue: opts.issue ? Number(opts.issue) : null,
    pr: opts.pr ? Number(opts.pr) : null,
    run: runContext(),
    model,
    tokens: { in: tokensIn, out: tokensOut, total: tokensIn + tokensOut },
    cost_usd: costOverride != null ? Number(costOverride.toFixed(6)) : priced.cost_usd,
    cost_source: costOverride != null ? 'override' : 'price_table',
    price: priced.price,
    engine_cost_usd: manifest.engine_cost_usd != null ? manifest.engine_cost_usd : null,
    outcome: opts.outcome || manifest.outcome || null,
    meta,
  };
}

function appendRecord(ledgerFile, rec) {
  fs.mkdirSync(path.dirname(path.resolve(ledgerFile)), { recursive: true });
  fs.appendFileSync(ledgerFile, JSON.stringify(rec) + '\n');
}

/* --------------------------------- ledger ---------------------------------- */

function readUsageLedger(file) {
  const records = [];
  if (!file || !fs.existsSync(file)) return records;
  const st = fs.statSync(file);
  if (st.size > MAX_LEDGER_BYTES) fail(`ledger exceeds cap (${st.size} > ${MAX_LEDGER_BYTES} bytes): ${file}`);
  const lines = fs.readFileSync(file, 'utf8').split('\n');
  for (const [i, line] of lines.entries()) {
    if (!line.trim()) continue;
    if (records.length >= MAX_ENTRIES) fail(`entry cap reached (${MAX_ENTRIES}): ${file}`);
    let rec;
    try {
      rec = JSON.parse(line);
    } catch (e) {
      console.error(`cost-accounting: skipping malformed line ${i + 1} in ${file}: ${e.message}`);
      continue;
    }
    const n = normalizeRecord(rec);
    if (n) records.push(n);
  }
  return records;
}

/* Tolerate both this tool's nested shape and flat producer variants
 * (tokens_in/tokens_out at top level) so rollups keep working if the run
 * ledger embeds records or another writer emits a flatter record. */
function normalizeRecord(raw) {
  if (!raw || typeof raw !== 'object') return null;
  // Unwrap a run-ledger entry whose data payload is a usage record; envelope
  // fields on the ledger entry fill any the payload lacks.
  if (raw.data && raw.data.type === RECORD_TYPE) {
    return normalizeRecord({
      stage: raw.stage, ts: raw.ts, repo: raw.repo, issue: raw.issue, pr: raw.pr, ...raw.data,
      run: raw.data.run || raw.run,
    });
  }
  const t = raw.tokens || {};
  const tokensIn = Number(t.in ?? raw.tokens_in ?? raw.prompt_tokens ?? 0);
  const tokensOut = Number(t.out ?? raw.tokens_out ?? raw.completion_tokens ?? 0);
  if (!Number.isFinite(tokensIn) || !Number.isFinite(tokensOut)) return null;
  return {
    v: raw.v || FORMAT_V,
    type: raw.type || RECORD_TYPE,
    stage: String(raw.stage || 'unknown'),
    ts: raw.ts || raw.timestamp || null,
    dateObj: new Date(raw.ts || raw.timestamp || 0),
    repo: raw.repo || null,
    issue: raw.issue != null ? Number(raw.issue) : null,
    pr: raw.pr != null ? Number(raw.pr) : null,
    model: String(raw.model || 'unknown'),
    tokens: { in: tokensIn, out: tokensOut, total: Number(t.total ?? tokensIn + tokensOut) },
    cost_usd: Number(raw.cost_usd ?? raw.costUsd ?? 0),
    outcome: raw.outcome ? String(raw.outcome).toLowerCase() : null,
    run: raw.run || null,
  };
}

/* Accept raw producer records OR normalizeRecord() output — callers that
 * pass freshly-built fixtures must not need a ledger round-trip first. */
function prepareRecords(records) {
  return (records || [])
    .map((r) => (r && r.dateObj instanceof Date && !isNaN(r.dateObj.getTime()) ? r : normalizeRecord(r)))
    .filter(Boolean);
}

function filterRecords(records, opts = {}) {
  let out = prepareRecords(records);
  if (opts.repo) out = out.filter((r) => r.repo === opts.repo);
  if (opts.issue != null) out = out.filter((r) => r.issue === Number(opts.issue));
  if (opts.since) {
    const since = new Date(opts.since).getTime();
    out = out.filter((r) => r.dateObj.getTime() >= since);
  }
  if (opts.until) {
    const until = new Date(opts.until).getTime();
    out = out.filter((r) => r.dateObj.getTime() <= until);
  }
  if (opts.period && opts.period !== 'all') {
    const ms = periodMs(opts.period);
    if (ms != null) {
      const now = opts.now ? new Date(opts.now).getTime() : Date.now();
      out = out.filter((r) => r.dateObj.getTime() >= now - ms && r.dateObj.getTime() <= now);
    }
  }
  return out;
}

function periodMs(period) {
  const p = String(period || '').toLowerCase().trim();
  const table = {
    '24h': 864e5, '1d': 864e5, today: 864e5, daily: 864e5,
    '7d': 7 * 864e5, week: 7 * 864e5, weekly: 7 * 864e5,
    '30d': 30 * 864e5, month: 30 * 864e5, monthly: 30 * 864e5,
  };
  if (table[p] != null) return table[p];
  if (p.endsWith('h') && Number.isFinite(parseFloat(p))) return parseFloat(p) * 36e5;
  if (p.endsWith('d') && Number.isFinite(parseFloat(p))) return parseFloat(p) * 864e5;
  return null;
}

/* --------------------------------- rollup ---------------------------------- */

/* Issue→merge rollup: every stage record attributed to an issue folds into
 * one total, with per-stage and per-model breakdowns and a lifecycle status
 * (merged > escalated > failed > pr_opened > in_progress). */
function rollupByIssue(records, opts = {}) {
  const filtered = filterRecords(records, opts).filter((r) => r.issue != null);
  const map = new Map();

  for (const r of filtered) {
    const key = `${r.repo || 'unknown'}#${r.issue}`;
    if (!map.has(key)) {
      map.set(key, {
        key,
        repo: r.repo,
        issue: r.issue,
        prs: new Set(),
        tokens_in: 0,
        tokens_out: 0,
        tokens_total: 0,
        cost_usd: 0,
        record_count: 0,
        by_stage: {},
        by_model: {},
        models: new Set(),
        first_at: r.ts,
        last_at: r.ts,
        status: 'in_progress',
      });
    }
    const it = map.get(key);
    it.tokens_in += r.tokens.in;
    it.tokens_out += r.tokens.out;
    it.tokens_total += r.tokens.total;
    it.cost_usd += r.cost_usd;
    it.record_count += 1;
    if (r.pr != null) it.prs.add(r.pr);
    it.models.add(r.model);

    if (!it.by_stage[r.stage]) it.by_stage[r.stage] = { records: 0, tokens: 0, cost_usd: 0 };
    it.by_stage[r.stage].records += 1;
    it.by_stage[r.stage].tokens += r.tokens.total;
    it.by_stage[r.stage].cost_usd += r.cost_usd;

    if (!it.by_model[r.model]) it.by_model[r.model] = { records: 0, tokens_in: 0, tokens_out: 0, cost_usd: 0 };
    it.by_model[r.model].records += 1;
    it.by_model[r.model].tokens_in += r.tokens.in;
    it.by_model[r.model].tokens_out += r.tokens.out;
    it.by_model[r.model].cost_usd += r.cost_usd;

    if (r.dateObj < new Date(it.first_at)) it.first_at = r.ts;
    if (r.dateObj > new Date(it.last_at)) it.last_at = r.ts;

    if (MERGE_STAGES.has(r.stage) && (r.outcome === 'success' || r.outcome === 'merged')) {
      it.status = 'merged';
    } else if (r.outcome === 'escalated' || r.outcome === 'needs-human' || it.status === 'escalated') {
      if (it.status !== 'merged') it.status = 'escalated';
    } else if (it.status !== 'merged' && it.status !== 'escalated') {
      if (r.outcome === 'success' || r.outcome === 'pr-opened') it.status = 'pr_opened';
      else if (r.outcome === 'failure' && it.status === 'in_progress') it.status = 'failed';
    }
  }

  const out = Array.from(map.values()).map((it) => ({
    ...it,
    cost_usd: Number(it.cost_usd.toFixed(6)),
    prs: Array.from(it.prs).sort((a, b) => a - b),
    models: Array.from(it.models).sort(),
    by_stage: roundNested(it.by_stage),
    by_model: roundNested(it.by_model),
  }));
  out.sort((a, b) => b.cost_usd - a.cost_usd);
  return out;
}

function roundNested(obj) {
  const out = {};
  for (const [k, v] of Object.entries(obj)) {
    out[k] = { ...v, cost_usd: Number(v.cost_usd.toFixed(6)) };
  }
  return out;
}

/* --------------------------------- budgets --------------------------------- */

function severityFor(threshold) {
  if (threshold >= 1.0) return 'exceeded';
  if (threshold >= 0.8) return 'critical';
  return 'warning';
}

function pushHighestCrossed(alerts, thresholds, base) {
  const ratio = base.spent / base.budget;
  let crossed = null;
  for (const t of thresholds) {
    if (ratio >= t) crossed = t;
  }
  if (crossed == null) return;
  alerts.push({
    ...base,
    id: `${base.scope}:${base.entity}:${base.metric}:${Math.round(crossed * 100)}`,
    threshold: crossed,
    percent_used: Number((ratio * 100).toFixed(1)),
    spent: Number(base.spent.toFixed(6)),
    budget: base.budget,
    severity: severityFor(crossed),
  });
}

/* Evaluate per-issue caps (cost + tokens) and repo period budgets
 * (daily/weekly/monthly). One alert per budget dimension at the highest
 * crossed threshold. */
function evaluateBudgets(records, config, opts = {}) {
  const thresholds = (config.alert_thresholds || [0.5, 0.8, 1.0]).slice().sort((a, b) => a - b);
  const budgets = config.budgets || {};
  const alerts = [];
  const now = opts.now || new Date().toISOString();

  for (const it of rollupByIssue(records, opts)) {
    if (budgets.per_issue_usd > 0 && it.cost_usd > 0) {
      pushHighestCrossed(alerts, thresholds, {
        scope: 'issue', entity: it.key, metric: 'cost_usd',
        spent: it.cost_usd, budget: budgets.per_issue_usd, period: 'lifecycle',
      });
    }
    if (budgets.per_issue_tokens > 0 && it.tokens_total > 0) {
      pushHighestCrossed(alerts, thresholds, {
        scope: 'issue', entity: it.key, metric: 'tokens',
        spent: it.tokens_total, budget: budgets.per_issue_tokens, period: 'lifecycle',
      });
    }
  }

  for (const [metric, period] of [['daily_usd', '24h'], ['weekly_usd', '7d'], ['monthly_usd', '30d']]) {
    const budget = budgets[metric];
    if (!budget || budget <= 0) continue;
    const spent = filterRecords(records, { ...opts, period, now })
      .reduce((s, r) => s + r.cost_usd, 0);
    pushHighestCrossed(alerts, thresholds, {
      scope: 'repo', entity: opts.repo || 'all', metric: 'cost_usd',
      spent, budget, period,
    });
  }
  return alerts;
}

/* ---------------------------------- output ---------------------------------- */

function rollupMarkdown(items) {
  let md = '## AI-SDLC issue→merge cost rollup\n\n';
  md += '| Issue | Status | Records | Tokens (in/out) | Cost (USD) | Stages |\n';
  md += '|---|---|---|---|---|---|\n';
  for (const it of items) {
    const stages = Object.entries(it.by_stage).map(([s, v]) => `${s}:${v.records}`).join(', ');
    md += `| ${it.key} | ${it.status} | ${it.record_count} | ${it.tokens_in.toLocaleString()}/${it.tokens_out.toLocaleString()} | $${it.cost_usd.toFixed(4)} | ${stages} |\n`;
  }
  return md;
}

function emitGithubOutput(kv) {
  if (!process.env.GITHUB_OUTPUT) return;
  try {
    for (const [k, v] of Object.entries(kv)) {
      fs.appendFileSync(process.env.GITHUB_OUTPUT, `${k}=${v}\n`);
    }
  } catch (_) { /* output channel is best-effort */ }
}

/* ----------------------------------- CLI ------------------------------------ */

function usage() {
  console.log(`cost-accounting — token/cost ledger for the issue→merge lifecycle

Ops:
  record --ledger F --stage S [--repo o/r] [--issue N] [--pr N]
         [--model M --tokens-in N --tokens-out N] [--manifest FILE]
         [--cost-usd X] [--outcome S] [--data JSON] [--github-output]
  rollup --ledger F [--issue N] [--repo o/r] [--since ISO] [--until ISO]
         [--period 24h|7d|30d|all] [--json|--markdown]
  check  --ledger F [--issue N] [--repo o/r] [--strict]
  price  --model M --tokens-in N --tokens-out N [--config F]

Stages: ${STAGES.join(' | ')} (any [a-z0-9-] slug accepted).
Config: ${DEFAULT_CONFIG} (AI_SDLC_COST_CONFIG overrides).`);
}

function parseArgs(argv) {
  const opts = { _: [] };
  const FLAGS = new Set(['json', 'markdown', 'github-output', 'strict', 'help']);
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const raw = a.slice(2);
      const k = raw.replace(/-/g, '_');
      opts[k] = FLAGS.has(raw) ? true : argv[++i];
    } else {
      opts._.push(a);
    }
  }
  return opts;
}

function main(argv) {
  const opts = parseArgs(argv);
  const op = opts._[0];
  if (!op || opts.help) { usage(); if (!op) process.exit(2); return; }

  const config = loadCostConfig(opts.config);

  switch (op) {
    case 'record': {
      if (!opts.stage) fail('record: --stage is required');
      const rec = buildRecord(opts, config);
      if (opts.ledger) appendRecord(opts.ledger, rec);
      console.log(JSON.stringify(rec));
      if (opts.github_output) {
        emitGithubOutput({
          cost_usd: rec.cost_usd,
          tokens_total: rec.tokens.total,
          cost_record: JSON.stringify(rec),
        });
      }
      return;
    }
    case 'rollup': {
      const records = readUsageLedger(opts.ledger);
      const items = rollupByIssue(records, opts);
      if (opts.markdown) console.log(rollupMarkdown(items));
      else console.log(JSON.stringify(items.length === 1 && opts.issue != null ? items[0] : items, null, 2));
      return;
    }
    case 'check': {
      const records = readUsageLedger(opts.ledger);
      const alerts = evaluateBudgets(records, config, opts);
      console.log(JSON.stringify({ alerts, evaluated_at: new Date().toISOString() }, null, 2));
      if (opts.strict && alerts.some((a) => a.severity === 'exceeded')) {
        fail('budget threshold exceeded (severity: exceeded)', 1);
      }
      return;
    }
    case 'price': {
      if (!opts.model) fail('price: --model is required');
      const p = priceTokens(opts.model, Number(opts.tokens_in || 0), Number(opts.tokens_out || 0), config.models);
      console.log(JSON.stringify({ model: opts.model, ...p }, null, 2));
      return;
    }
    default:
      fail(`unknown op: ${op}`);
  }
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(e.message.startsWith('cost-accounting:') ? e.message : `cost-accounting: ${e.message}`);
    process.exit(e.exitCode || 2);
  }
}

module.exports = {
  RECORD_TYPE,
  STAGES,
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
  main,
};
