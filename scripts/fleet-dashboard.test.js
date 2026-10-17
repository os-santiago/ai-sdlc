// scripts/fleet-dashboard.test.js — unit + fixture-driven tests for the
// org fleet dashboard (issue #173).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  parseRepoCsv,
  parseDuration,
  fmtDuration,
  resolveFleetRepos,
  loadFleetConfig,
  extractAutonomy,
  classifyHealth,
  buildRepoEntry,
  buildFleetDashboard,
  renderMarkdown,
  main,
  MAX_REPOS,
} = require('./fleet-dashboard');

const NOW = Date.parse('2026-10-14T12:00:00Z');
const H = 3600e3;
const D = 86400e3;

// --- Fixtures ------------------------------------------------------------------

const CONTRACT = [
  'spec: "1.0.0"',
  '',
  'repo:',
  '  archetype: tooling',
  '',
  'autonomy:',
  '  level: auto-merge-low',
  '  merge_requires_checks: true',
  '  freeze_env: AI_SDLC_FREEZE',
  '',
  'triggers:',
  '  issue_label: ready-to-implement',
  '',
].join('\n');

const run = (conclusion, hoursAgo, name = 'ai-sdlc (self)') => ({
  name,
  status: 'completed',
  conclusion,
  created_at: new Date(NOW - hoursAgo * H).toISOString(),
  updated_at: new Date(NOW - hoursAgo * H + 5 * 60e3).toISOString(),
});

const okRuns = (n) => Array.from({ length: n }, (_, i) => run('success', (i + 1) * 6));

// exec stub: argv-driven — contract path returns CONTRACT (or throws),
// actions/runs returns the repo's fixture payload.
const execStub = ({ contracts = {}, runsByRepo = {} } = {}) => (argv) => {
  const endpoint = argv[1] || '';
  const m = endpoint.match(/^repos\/([^/]+\/[^/]+)\//);
  const repo = m && m[1];
  if (endpoint.includes('/contents/.ai-sdlc.yaml')) {
    if (contracts[repo] === undefined) throw new Error(`404 Not Found: ${endpoint}`);
    return contracts[repo];
  }
  if (endpoint.includes('/actions/runs')) {
    if (runsByRepo[repo] === undefined) throw new Error(`404 Not Found: ${endpoint}`);
    return JSON.stringify({ workflow_runs: runsByRepo[repo] });
  }
  throw new Error(`unexpected gh call: ${argv.join(' ')}`);
};

// --- Small parsers ---------------------------------------------------------------

describe('parseRepoCsv / parseDuration', () => {
  it('csv trims, drops empties', () => {
    assert.deepEqual(parseRepoCsv(' o/a ,o/b,, '), ['o/a', 'o/b']);
    assert.deepEqual(parseRepoCsv(''), []);
  });

  it('durations parse and format', () => {
    assert.equal(parseDuration('7d'), 7 * D);
    assert.equal(parseDuration('24h'), 24 * H);
    assert.equal(parseDuration('30m'), 30 * 60e3);
    assert.equal(parseDuration('2w'), 14 * D);
    assert.equal(parseDuration('bogus', 42), 42);
    assert.equal(fmtDuration(7 * D), '1w');
    assert.equal(fmtDuration(5 * D), '5d');
    assert.equal(fmtDuration(5 * H), '5h');
  });
});

// --- Fleet membership --------------------------------------------------------------

describe('resolveFleetRepos', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'fleet-'));
  const cfg = path.join(tmp, 'fleet.json');
  fs.writeFileSync(cfg, JSON.stringify({ repos: ['cfg/repo'] }));

  it('explicit --repos wins over env and config', () => {
    const repos = resolveFleetRepos({
      repos: ['cli/repo'], configPath: cfg,
      env: (k) => ({ AI_SDLC_FLEET_REPOS: 'env/repo' })[k],
    });
    assert.deepEqual(repos, ['cli/repo']);
  });

  it('env beats config file; config beats GITHUB_REPOSITORY', () => {
    assert.deepEqual(
      resolveFleetRepos({ configPath: cfg, env: (k) => ({ AI_SDLC_FLEET_REPOS: 'env/a,env/b' })[k] }),
      ['env/a', 'env/b'],
    );
    assert.deepEqual(
      resolveFleetRepos({ configPath: cfg, env: () => undefined }),
      ['cfg/repo'],
    );
    assert.deepEqual(
      resolveFleetRepos({ configPath: '/nonexistent.json', env: (k) => ({ GITHUB_REPOSITORY: 'self/repo' })[k] }),
      ['self/repo'],
    );
  });

  it('dedupes, drops malformed entries, caps at MAX_REPOS', () => {
    const big = Array.from({ length: MAX_REPOS + 10 }, (_, i) => `o/r${i}`);
    const repos = resolveFleetRepos({ repos: ['o/a', 'o/a', 'bad', '', 'o/b', ...big] });
    assert.equal(repos.length, MAX_REPOS);
    assert.ok(!repos.includes('bad'));
    assert.equal(repos.filter((r) => r === 'o/a').length, 1);
  });

  it('malformed config is a loud error, missing config is a soft miss', () => {
    const bad = path.join(tmp, 'bad.json');
    fs.writeFileSync(bad, '{ not json');
    assert.throws(() => loadFleetConfig(bad), /invalid JSON/);
    assert.deepEqual(loadFleetConfig('/nonexistent.json'), {});
  });
});

// --- Contract extraction --------------------------------------------------------------

describe('extractAutonomy', () => {
  it('reads level + freeze_env from a real contract', () => {
    const a = extractAutonomy(CONTRACT);
    assert.equal(a.level, 'auto-merge-low');
    assert.equal(a.freezeEnv, 'AI_SDLC_FREEZE');
  });

  it('missing contract → nulls, never throws', () => {
    assert.deepEqual(extractAutonomy(null), { level: null, freezeEnv: null });
    assert.deepEqual(extractAutonomy(''), { level: null, freezeEnv: null });
  });

  it('does not bleed into sibling top-level blocks', () => {
    const a = extractAutonomy('autonomy:\n  level: auto-pr\ntriggers:\n  level: bogus\n');
    assert.equal(a.level, 'auto-pr');
  });
});

// --- Health classification --------------------------------------------------------------

describe('classifyHealth', () => {
  it('bands: idle / critical / degraded / healthy', () => {
    assert.equal(classifyHealth({ completed: 0, failed: 0 }), 'idle');
    assert.equal(classifyHealth({ completed: 2, failed: 1 }), 'critical'); // 50%
    assert.equal(classifyHealth({ completed: 4, failed: 1 }), 'degraded'); // 25%
    assert.equal(classifyHealth({ completed: 5, failed: 1, lastConclusion: 'failure' }), 'degraded');
    assert.equal(classifyHealth({ completed: 8, failed: 1, lastConclusion: 'success' }), 'healthy'); // 12.5%
  });
});

// --- buildRepoEntry ------------------------------------------------------------------

describe('buildRepoEntry', () => {
  const base = { contractText: CONTRACT, runs: okRuns(6), nowMs: NOW, windowMs: 7 * D };

  it('AC: healthy fleet — enrolled repo, autonomy + healthy signal', () => {
    const e = buildRepoEntry('o/a', base);
    assert.equal(e.enrolled, true);
    assert.equal(e.autonomy.level, 'auto-merge-low');
    assert.equal(e.health.status, 'healthy');
    assert.equal(e.health.completed, 6);
    assert.equal(e.health.failed, 0);
    assert.deepEqual(e.flags, []);
  });

  it('AC: missing contract — enrolled=false, no-contract flag, health still computed', () => {
    const e = buildRepoEntry('o/b', { ...base, contractText: null });
    assert.equal(e.enrolled, false);
    assert.equal(e.autonomy.level, null);
    assert.equal(e.health.status, 'healthy'); // runs still readable
    assert.deepEqual(e.flags, ['no-contract']);
  });

  it('AC: unreachable repo — fail-soft, marked unknown, never throws', () => {
    const e = buildRepoEntry('o/gone', { contractText: null, runs: null, nowMs: NOW, windowMs: 7 * D });
    assert.equal(e.enrolled, false);
    assert.equal(e.health.status, 'unknown');
    assert.deepEqual(e.flags, ['unreachable']);
  });

  it('failure-dominated window → critical; single failure → degraded', () => {
    const crit = buildRepoEntry('o/a', { ...base, runs: [run('failure', 1), run('failure', 2)] });
    assert.equal(crit.health.status, 'critical');
    const deg = buildRepoEntry('o/a', { ...base, runs: [...okRuns(3), run('failure', 1)] });
    assert.equal(deg.health.status, 'degraded');
  });

  it('enrolled repo with no in-window runs → idle + stale flag', () => {
    const e = buildRepoEntry('o/a', { ...base, runs: [run('success', 30 * 24)] });
    assert.equal(e.health.status, 'idle');
    assert.deepEqual(e.flags, ['stale']);
  });
});

// --- buildFleetDashboard ------------------------------------------------------------------

describe('buildFleetDashboard', () => {
  const repos = ['o/healthy', 'o/broken', 'o/noentry', 'o/gone'];
  const fetchers = {
    fetchContract: (r) => ({
      'o/healthy': CONTRACT,
      'o/broken': CONTRACT,
      'o/noentry': null,
      'o/gone': null,
    })[r],
    fetchRuns: (r) => ({
      'o/healthy': okRuns(5),
      'o/broken': [run('failure', 1), run('failure', 2), run('success', 3)],
      'o/noentry': okRuns(2),
      'o/gone': null,
    })[r],
  };

  it('aggregates per-repo entries + fleet totals', () => {
    const d = buildFleetDashboard({ repos, ...fetchers, nowMs: NOW, windowMs: 7 * D });
    assert.equal(d.fleet.repoCount, 4);
    assert.equal(d.fleet.enrolledCount, 2);
    assert.equal(d.fleet.byHealth.healthy, 2); // o/healthy + o/noentry
    assert.equal(d.fleet.byHealth.critical, 1);
    assert.equal(d.fleet.byHealth.unknown, 1);
    assert.equal(d.repos[2].health.status, 'healthy'); // no-contract still reports runs
    assert.ok(d.fleet.flaggedCount >= 2);
  });

  it('AC: deterministic — same fixtures + pinned now give identical output', () => {
    const a = buildFleetDashboard({ repos, ...fetchers, nowMs: NOW, windowMs: 7 * D });
    const b = buildFleetDashboard({ repos, ...fetchers, nowMs: NOW, windowMs: 7 * D });
    assert.equal(JSON.stringify(a), JSON.stringify(b));
    assert.equal(a.generatedAt, '2026-10-14T12:00:00.000Z');
  });

  it('bounded: repo list and per-repo runs are capped', () => {
    const many = Array.from({ length: MAX_REPOS + 5 }, (_, i) => `o/r${i}`);
    const d = buildFleetDashboard({
      repos: many,
      fetchContract: () => CONTRACT,
      fetchRuns: () => Array.from({ length: 60 }, (_, i) => run('success', i + 1)),
      nowMs: NOW, windowMs: 7 * D,
    });
    assert.equal(d.fleet.repoCount, MAX_REPOS);
    assert.ok(d.repos.every((e) => e.health.runs <= 30));
  });
});

// --- renderMarkdown ------------------------------------------------------------------

describe('renderMarkdown', () => {
  it('renders one row per repo with autonomy, health icon and flags', () => {
    const d = buildFleetDashboard({
      repos: ['o/healthy', 'o/gone'],
      fetchContract: (r) => (r === 'o/healthy' ? CONTRACT : null),
      fetchRuns: (r) => (r === 'o/healthy' ? okRuns(3) : null),
      nowMs: NOW, windowMs: 7 * D,
    });
    const md = renderMarkdown(d);
    assert.match(md, /^# AI-SDLC Fleet Dashboard/m);
    assert.match(md, /o\/healthy.*auto-merge-low.*🟢 healthy/s);
    assert.match(md, /o\/gone.*not enrolled.*❓ unknown.*unreachable/s);
    assert.match(md, /\*\*Fleet:\*\*/);
  });
});

// --- main() e2e with stubbed gh ---------------------------------------------------------

describe('main — mocked gh e2e', () => {
  it('healthy fleet end-to-end: exec stub + pinned now → JSON model', () => {
    const out = [];
    const d = main(['--repos', 'o/a,o/b', '--now', '2026-10-14T12:00:00Z', '--json'], {
      exec: execStub({
        contracts: { 'o/a': CONTRACT, 'o/b': CONTRACT },
        runsByRepo: { 'o/a': okRuns(4), 'o/b': okRuns(4) },
      }),
      stdout: (s) => out.push(s),
      env: {},
    });
    assert.equal(d.fleet.byHealth.healthy, 2);
    assert.equal(JSON.parse(out[0]).generatedAt, '2026-10-14T12:00:00.000Z');
  });

  it('--out writes the markdown artifact; unreachable repo survives', () => {
    const tmp = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'fleet-out-')), 'dash.md');
    main(['--repos', 'o/a,o/gone', '--now', '2026-10-14T12:00:00Z', '--out', tmp], {
      exec: execStub({ contracts: { 'o/a': CONTRACT }, runsByRepo: { 'o/a': okRuns(2) } }),
      stdout: () => {},
      env: {},
    });
    const md = fs.readFileSync(tmp, 'utf8');
    assert.match(md, /o\/a.*🟢 healthy/s);
    assert.match(md, /o\/gone.*❓ unknown/s);
  });
});
