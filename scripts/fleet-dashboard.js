#!/usr/bin/env node
// scripts/fleet-dashboard.js — org fleet dashboard (issue #173)
//
// Cross-repo enrollment, autonomy and run-health view for the AI-SDLC
// runtime — the GHA-native port of Hermes' scripts/fleet_dashboard.js.
// For every enrolled repo it reports:
//
//   - Contract: whether `.ai-sdlc.yaml` is readable on the default branch
//     (enrolled) and the declared `autonomy.level` trust-ladder ceiling.
//   - Run health: recent `actions/runs` inside a sliding window (default
//     7d), classified as healthy / degraded / critical / idle; repos the
//     API cannot reach at all are reported `unknown`, never dropped.
//   - Flags: `unreachable` (no contract AND no runs readable), `no-contract`
//     (reachable but no .ai-sdlc.yaml — not enrolled), `stale` (enrolled
//     but no completed run inside the window).
//
// Fleet membership resolution, first hit wins:
//   1. --repos owner/a,owner/b   (CLI)
//   2. AI_SDLC_FLEET_REPOS       (env, csv)
//   3. config/fleet.json         ({ "repos": [...] } — the enrolled list)
//   4. GITHUB_REPOSITORY         (env — self only)
//
// Bounded: repo list capped at MAX_REPOS, runs fetched per repo capped at
// MAX_RUNS_PER_REPO, contract bodies scanned within MAX_CONTRACT_LINES.
// Deterministic: pass --now ISO-8601 to pin generatedAt; equal inputs
// produce byte-identical output.
//
// CLI:
//   node scripts/fleet-dashboard.js [--repos o/a,o/b] [--window 7d]
//     [--stale 14d] [--max-runs 20] [--now ISO] [--json | --md]
//     [--out FILE] [--github-summary]
//
// Scheduled runs: .github/workflows/ai-sdlc-fleet.yml regenerates
// docs/fleet-dashboard.md daily with a diff-gated commit.
'use strict';

const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const MAX_REPOS = 100;
const MAX_RUNS_PER_REPO = 30;
const MAX_CONTRACT_BYTES = 256 * 1024;
const MAX_CONTRACT_LINES = 500;
const DEFAULT_CONFIG = path.join(__dirname, '..', 'config', 'fleet.json');
const REPO_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

// Conclusions counted as failures for the health classification.
const FAILED_CONCLUSIONS = new Set(['failure', 'timed_out', 'startup_failure']);

const HEALTH_ICON = {
  healthy: '🟢',
  degraded: '🟡',
  critical: '🔴',
  idle: '⚪',
  unknown: '❓',
};

// --- Fleet membership ---------------------------------------------------------

function parseRepoCsv(v) {
  return String(v || '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
}

// config/fleet.json: { "repos": ["owner/name", ...] }
// Missing file → {} (fall through to the next source); malformed JSON is a
// loud configuration error, not a soft miss (publish-contract precedent:
// config errors escalate, they don't silently degrade).
function loadFleetConfig(configPath) {
  const p = configPath || DEFAULT_CONFIG;
  if (!fs.existsSync(p)) return {};
  let doc;
  try {
    doc = JSON.parse(fs.readFileSync(p, 'utf8'));
  } catch (e) {
    throw new Error(`fleet config ${p}: invalid JSON — ${e.message}`);
  }
  const repos = Array.isArray(doc.repos) ? doc.repos.map((r) => String(r)) : [];
  return { repos };
}

function resolveFleetRepos({ env, configPath, repos } = {}) {
  const envGet = env || ((k) => process.env[k]);
  let list = Array.isArray(repos) ? repos : null;
  if (!list || !list.length) list = parseRepoCsv(envGet('AI_SDLC_FLEET_REPOS'));
  if (!list.length) list = loadFleetConfig(configPath).repos || [];
  if (!list.length) list = parseRepoCsv(envGet('GITHUB_REPOSITORY'));
  const seen = new Set();
  return list
    .map((r) => String(r).trim())
    .filter((r) => REPO_RE.test(r))
    .filter((r) => !seen.has(r) && seen.add(r))
    .slice(0, MAX_REPOS);
}

// --- Small parsers ------------------------------------------------------------

// "30m", "24h", "7d", "1w"; bare number = hours. Returns ms, or fallback.
function parseDuration(v, fallbackMs) {
  const m = String(v || '').trim().match(/^(\d+(?:\.\d+)?)(ms|s|m|h|d|w)?$/i);
  if (!m) return fallbackMs;
  const n = Number(m[1]);
  const unit = (m[2] || 'h').toLowerCase();
  const mult = { ms: 1, s: 1e3, m: 60e3, h: 3600e3, d: 86400e3, w: 604800e3 }[unit];
  return n * mult;
}

function fmtDuration(ms) {
  if (ms % 604800e3 === 0) return `${ms / 604800e3}w`;
  if (ms % 86400e3 === 0) return `${ms / 86400e3}d`;
  if (ms % 3600e3 === 0) return `${ms / 3600e3}h`;
  if (ms % 60e3 === 0) return `${ms / 60e3}m`;
  return `${ms}ms`;
}

// Bounded YAML-subset read of the `autonomy:` block — enough for
// autonomy.level / autonomy.freeze_env without a YAML dependency. The
// contract validates against spec/ai-sdlc.schema.json upstream; here we
// only need the two scalar leaves for display.
function extractAutonomy(contractText) {
  const out = { level: null, freezeEnv: null };
  if (contractText == null) return out;
  const lines = String(contractText)
    .slice(0, MAX_CONTRACT_BYTES)
    .split('\n')
    .slice(0, MAX_CONTRACT_LINES);
  let inAutonomy = false;
  for (const raw of lines) {
    const line = raw.replace(/\s+#.*$/, ''); // trailing comments
    if (!line.trim()) continue;
    const indent = line.length - line.trimStart().length;
    if (indent === 0) {
      inAutonomy = /^autonomy\s*:/.test(line);
      continue;
    }
    if (!inAutonomy) continue;
    const m = line.match(/^\s*(level|freeze_env)\s*:\s*["']?([A-Za-z0-9_-]+)["']?\s*$/);
    if (!m) continue;
    if (m[1] === 'level') out.level = m[2];
    else out.freezeEnv = m[2];
  }
  return out;
}

// --- GitHub fetch (injectable) -------------------------------------------------

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

// .ai-sdlc.yaml on the default branch, raw. Null on any failure (missing
// file, private repo without grant, API error) — fail-soft by contract.
function fetchContractText(repo, exec) {
  try {
    return (exec || defaultExec)([
      'api', `repos/${repo}/contents/.ai-sdlc.yaml`,
      '-H', 'Accept: application/vnd.github.raw',
    ]);
  } catch (e) {
    return null;
  }
}

// Recent workflow runs, newest first. Null on failure (fail-soft).
function fetchWorkflowRuns(repo, perPage, exec) {
  try {
    const out = (exec || defaultExec)([
      'api', `repos/${repo}/actions/runs?per_page=${Math.min(perPage || 20, MAX_RUNS_PER_REPO)}`,
    ]);
    const doc = JSON.parse(out);
    return Array.isArray(doc.workflow_runs) ? doc.workflow_runs : [];
  } catch (e) {
    return null;
  }
}

// --- Model ---------------------------------------------------------------------

function runTs(r) {
  const t = new Date(r.updated_at || r.created_at || r.run_started_at || 0).getTime();
  return isNaN(t) ? 0 : t;
}

// Health over the window:
//   idle     — no completed runs in-window.
//   critical — failure-dominated (≥50% failures with ≥2 completed runs).
//   degraded — failure rate ≥25% or the latest completed run failed.
//   healthy  — activity below the degraded bands.
// Callers set 'unknown' themselves when fetches failed.
function classifyHealth({ completed, failed, lastConclusion }) {
  if (!completed) return 'idle';
  const rate = (failed / completed) * 100;
  if (completed >= 2 && rate >= 50) return 'critical';
  if (rate >= 25 || FAILED_CONCLUSIONS.has(lastConclusion)) return 'degraded';
  return 'healthy';
}

// Pure per-repo entry — pass contractText/runs as fetched (null = failed).
function buildRepoEntry(repo, { contractText, runs, nowMs, windowMs }) {
  const enrolled = contractText != null;
  const autonomy = enrolled ? extractAutonomy(contractText) : { level: null, freezeEnv: null };
  const runsKnown = Array.isArray(runs);
  const reachable = enrolled || runsKnown;

  const startMs = nowMs - windowMs;
  const inWindow = (runsKnown ? runs : [])
    .filter((r) => runTs(r) >= startMs && runTs(r) <= nowMs)
    .slice(0, MAX_RUNS_PER_REPO);
  const completed = inWindow
    .filter((r) => String(r.status || 'completed') === 'completed' || r.conclusion)
    .sort((a, b) => runTs(b) - runTs(a));
  const failed = completed.filter((r) => FAILED_CONCLUSIONS.has(String(r.conclusion)));
  const inProgress = inWindow.length - completed.length;
  const last = completed[0] || null;
  const lastConclusion = last ? String(last.conclusion || 'unknown') : null;

  let status;
  if (!reachable || !runsKnown) status = 'unknown';
  else status = classifyHealth({ completed: completed.length, failed: failed.length, lastConclusion });

  const flags = [];
  if (!reachable) flags.push('unreachable');
  else {
    if (!enrolled) flags.push('no-contract');
    if (enrolled && !completed.length) flags.push('stale');
  }

  return {
    repo,
    enrolled,
    autonomy,
    health: {
      status,
      runs: inWindow.length,
      completed: completed.length,
      succeeded: completed.length - failed.length,
      failed: failed.length,
      inProgress,
      failureRate: completed.length ? Number(((failed.length / completed.length) * 100).toFixed(1)) : 0,
      lastConclusion,
      lastRunAt: last ? (last.updated_at || last.created_at || null) : null,
      lastWorkflow: last ? String(last.name || last.display_title || '') : null,
    },
    flags,
  };
}

// Full dashboard model. fetchers are injectable for tests; live defaults go
// through `gh api`. Every declared repo gets an entry — a repo whose fetches
// all fail surfaces as `unknown`, never silently dropped.
function buildFleetDashboard({
  repos, fetchContract, fetchRuns, nowMs, windowMs, exec,
} = {}) {
  const now = typeof nowMs === 'number' ? nowMs : Date.now();
  const window = typeof windowMs === 'number' ? windowMs : 7 * 86400e3;
  const list = Array.isArray(repos) ? repos.slice(0, MAX_REPOS) : resolveFleetRepos();
  const getContract = fetchContract || ((r) => fetchContractText(r, exec));
  const getRuns = fetchRuns || ((r) => fetchWorkflowRuns(r, MAX_RUNS_PER_REPO, exec));

  const entries = list.map((repo) => buildRepoEntry(repo, {
    contractText: getContract(repo),
    runs: getRuns(repo),
    nowMs: now,
    windowMs: window,
  }));

  const byHealth = { healthy: 0, degraded: 0, critical: 0, idle: 0, unknown: 0 };
  for (const e of entries) byHealth[e.health.status] = (byHealth[e.health.status] || 0) + 1;

  return {
    generatedAt: new Date(now).toISOString(),
    window: { start: new Date(now - window).toISOString(), end: new Date(now).toISOString(), ms: window },
    repos: entries,
    fleet: {
      repoCount: entries.length,
      enrolledCount: entries.filter((e) => e.enrolled).length,
      flaggedCount: entries.filter((e) => e.flags.length).length,
      byHealth,
      runs: entries.reduce((a, e) => a + e.health.runs, 0),
      failures: entries.reduce((a, e) => a + e.health.failed, 0),
    },
  };
}

// --- Render ---------------------------------------------------------------------

function fmtRepo(repo) {
  return repo.length > 38 ? '…' + repo.slice(-37) : repo;
}

function fmtAgo(iso, nowMs) {
  const t = new Date(iso).getTime();
  if (isNaN(t)) return 'n/a';
  const d = Math.max(0, nowMs - t);
  if (d < 3600e3) return `${Math.floor(d / 60e3)}m ago`;
  if (d < 86400e3) return `${Math.floor(d / 3600e3)}h ago`;
  return `${Math.floor(d / 86400e3)}d ago`;
}

// Markdown table — one row per repo; flagged repos are visually marked so
// stale/failing members stand out at a glance.
function renderMarkdown(dashboard, { nowMs } = {}) {
  const now = typeof nowMs === 'number' ? nowMs : Date.parse(dashboard.generatedAt);
  const lines = [
    '# AI-SDLC Fleet Dashboard',
    '',
    `_Generated ${dashboard.generatedAt} · window ${fmtDuration(dashboard.window.ms)} · ` +
      `${dashboard.fleet.repoCount} repo(s), ${dashboard.fleet.enrolledCount} enrolled_`,
    '',
    '| Repo | Autonomy | Health | Runs in window | Last run | Flags |',
    '| --- | --- | --- | --- | --- | --- |',
  ];

  for (const e of dashboard.repos) {
    const icon = HEALTH_ICON[e.health.status] || '❓';
    const aut = e.autonomy.level || (e.enrolled ? '—' : 'not enrolled');
    const runsStr = e.health.runs
      ? `${e.health.runs} (${e.health.succeeded}✓/${e.health.failed}✗${e.health.inProgress ? `/${e.health.inProgress}▶` : ''})`
      : '—';
    const lastStr = e.health.lastRunAt
      ? `${e.health.lastConclusion} · ${fmtAgo(e.health.lastRunAt, now)}`
      : 'none';
    const flagStr = e.flags.length ? `⚠ ${e.flags.join(', ')}` : '';
    lines.push(`| \`${fmtRepo(e.repo)}\` | ${aut} | ${icon} ${e.health.status} | ${runsStr} | ${lastStr} | ${flagStr} |`);
  }

  const b = dashboard.fleet.byHealth;
  lines.push('');
  lines.push(
    `**Fleet:** ${HEALTH_ICON.healthy}${b.healthy || 0} ${HEALTH_ICON.degraded}${b.degraded || 0} ` +
      `${HEALTH_ICON.critical}${b.critical || 0} ${HEALTH_ICON.idle}${b.idle || 0} ${HEALTH_ICON.unknown}${b.unknown || 0} · ` +
      `${dashboard.fleet.runs} run(s), ${dashboard.fleet.failures} failure(s) · ` +
      `${dashboard.fleet.flaggedCount} repo(s) flagged`,
  );
  lines.push('');
  lines.push('_Regenerated on schedule by `ai-sdlc-fleet.yml` from `scripts/fleet-dashboard.js`._');
  return lines.join('\n');
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: fleet-dashboard.js [--repos o/a,o/b] [--config FILE] [--window 7d]',
    '                          [--stale 14d] [--max-runs N] [--now ISO]',
    '                          [--json | --md] [--out FILE] [--github-summary]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = { json: false, githubSummary: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repos') opts.repos = parseRepoCsv(next());
    else if (a === '--config') opts.config = next();
    else if (a === '--window') opts.window = next();
    else if (a === '--stale') opts.stale = next();
    else if (a === '--max-runs') opts.maxRuns = Number(next());
    else if (a === '--now') opts.now = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--md') opts.md = true;
    else if (a === '--out') opts.out = next();
    else if (a === '--github-summary') opts.githubSummary = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { exec, stdout, env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const emit = stdout || ((s) => console.log(s));

  const nowMs = opts.now ? Date.parse(opts.now) : Date.now();
  if (isNaN(nowMs)) throw new Error(`invalid --now '${opts.now}'`);
  const windowMs = parseDuration(opts.window || e.AI_SDLC_FLEET_WINDOW || '7d', 7 * 86400e3);
  const maxRuns = Math.min(Math.max(opts.maxRuns || 20, 1), MAX_RUNS_PER_REPO);

  const repos = resolveFleetRepos({ env: (k) => e[k], configPath: opts.config, repos: opts.repos });
  const dashboard = buildFleetDashboard({
    repos,
    nowMs,
    windowMs,
    exec,
    fetchRuns: (r) => fetchWorkflowRuns(r, maxRuns, exec),
  });
  // --stale is an alias for the reporting window of the stale flag: repos
  // with no completed run in-window are already flagged; the flag exists so
  // the CLI surface matches the scheduled workflow inputs.
  void opts.stale;

  const md = renderMarkdown(dashboard, { nowMs });
  if (opts.out) fs.writeFileSync(opts.out, md + '\n');
  if (opts.githubSummary && e.GITHUB_STEP_SUMMARY) {
    fs.appendFileSync(e.GITHUB_STEP_SUMMARY, md + '\n');
  }

  emit(opts.json ? JSON.stringify(dashboard, null, 2) : md);
  return dashboard;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`fleet-dashboard: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  MAX_REPOS,
  MAX_RUNS_PER_REPO,
  HEALTH_ICON,
  FAILED_CONCLUSIONS,
  parseRepoCsv,
  parseDuration,
  fmtDuration,
  resolveFleetRepos,
  loadFleetConfig,
  extractAutonomy,
  fetchContractText,
  fetchWorkflowRuns,
  classifyHealth,
  buildRepoEntry,
  buildFleetDashboard,
  renderMarkdown,
  parseArgs,
  main,
};
