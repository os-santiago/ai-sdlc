// scripts/stall-classifier.test.js — unit tests for engine-stall
// classification (issue #158): transport vs tooling vs spec vs budget vs
// livelock, driven by the registry exitCodeMap/stallClassMap, manifest
// resolutions and mutation evidence.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  classifyStall,
  manifestResolution,
  toolCallCount,
  registryLabel,
  stallClassOverride,
  main,
  CLASSES,
  ROUTES,
} = require('./stall-classifier');

const REGISTRY = path.join(__dirname, '..', 'config', 'engine_adapters.json');
const opts = { registryPath: REGISTRY };

// --- AC: every class routes to a distinct terminal state ----------------------
describe('routing table', () => {
  it('declares a route + terminal for every class', () => {
    for (const c of CLASSES) {
      assert.ok(ROUTES[c], `route for ${c}`);
      assert.ok(ROUTES[c].route && ROUTES[c].terminal, `${c} route/terminal`);
    }
    assert.equal(new Set(CLASSES.map((c) => ROUTES[c].terminal)).size, CLASSES.length,
      'terminals must be distinct per class');
  });

  it('only transport is retryable, with a bounded ladder', () => {
    for (const c of CLASSES) {
      if (c === 'transport') {
        assert.equal(ROUTES[c].retryable, true);
        assert.ok(ROUTES[c].maxRetries > 0 && ROUTES[c].maxRetries <= 3,
          'transport retry must be bounded');
      } else {
        assert.equal(ROUTES[c].retryable, false, `${c} must not retry`);
      }
    }
  });
});

// --- AC: zero-mutation timeouts classified separately from crashes ------------
describe('timeout arm', () => {
  it('exit 24 (TimeoutError) with zero mutations and no steps → transport', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 24, mutations: false, toolCalls: 0 }, opts);
    assert.equal(r.class, 'transport');
    assert.equal(r.route, 'retry');
  });

  it('exit 124 with tool calls but zero mutations → livelock, not crash', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 124, mutations: false, toolCalls: 14 }, opts);
    assert.equal(r.class, 'livelock');
    assert.equal(r.route, 'needs-human');
  });

  it('SIGKILL with tool calls but zero mutations → livelock', () => {
    const r = classifyStall({ engine: 'scc', exitCode: null, signal: 'SIGKILL', mutations: false, toolCalls: 7 }, opts);
    assert.equal(r.class, 'livelock');
  });

  it('timeout with real mutations → budget (partial work)', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 137, mutations: true, toolCalls: 30 }, opts);
    assert.equal(r.class, 'budget');
  });

  it('timeout output carrying a transport signature → transport', () => {
    const r = classifyStall({
      engine: 'scc', exitCode: 24, mutations: false, toolCalls: 0,
      outputText: 'Error: fetch failed — connection timed out reaching upstream',
    }, opts);
    assert.equal(r.class, 'transport');
  });
});

// --- transport vs tooling -----------------------------------------------------
describe('transport class', () => {
  it('scc exit 20 resolves transport via the registry stallClassMap', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 20, mutations: false }, opts);
    assert.equal(r.class, 'transport');
    assert.equal(r.retryable, true);
    assert.equal(r.terminal, 'needs-human');
  });

  it('quota signature on a generic failed exit promotes tooling → transport', () => {
    const r = classifyStall({
      engine: 'codex', exitCode: 1, mutations: false,
      outputText: 'API Error 429 — all accounts exhausted their quota',
    }, opts);
    assert.equal(r.class, 'transport');
  });

  it('prose signatures never decide classes on a clean exit', () => {
    const r = classifyStall({
      engine: 'scc', exitCode: 0, mutations: true,
      outputText: 'fixed the fetch failed handler and the HTTP 429 retry',
    }, opts);
    assert.equal(r.class, 'ok');
  });
});

describe('tooling class', () => {
  it('spawn ENOENT → tooling, never retryable', () => {
    const r = classifyStall({ engine: 'scc', exitCode: null, spawnError: 'ENOENT' }, opts);
    assert.equal(r.class, 'tooling');
    assert.equal(r.retryable, false);
  });

  it('shell-launch codes 126/127 → tooling', () => {
    for (const code of [126, 127]) {
      assert.equal(classifyStall({ engine: 'scc', exitCode: code }, opts).class, 'tooling');
    }
  });

  it('scc exit 21 (auth) → tooling', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 21, mutations: false }, opts);
    assert.equal(r.class, 'tooling');
    assert.equal(r.terminal, 'engine-postmortem');
  });

  it('unclassified nonzero exit with no evidence → tooling', () => {
    const r = classifyStall({ engine: 'devin', exitCode: 1, mutations: false }, opts);
    assert.equal(r.class, 'tooling');
  });
});

// --- spec / budget ------------------------------------------------------------
describe('spec class', () => {
  it('scc exit 10 → spec → needs-refinement', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 10, mutations: false }, opts);
    assert.equal(r.class, 'spec');
    assert.equal(r.route, 'needs-refinement');
  });

  it('clean exit with measured zero mutations → spec', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 0, mutations: false, toolCalls: 3 }, opts);
    assert.equal(r.class, 'spec');
  });

  it('clean exit with real mutations → ok', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 0, mutations: true }, opts);
    assert.equal(r.class, 'ok');
    assert.equal(r.route, 'publish');
  });

  it('manifest not_actionable resolution → spec regardless of exit code', () => {
    const r = classifyStall({
      engine: 'scc', exitCode: 0, mutations: false,
      manifest: { resolution: 'not_actionable', final_message: 'cannot fix from code' },
    }, opts);
    assert.equal(r.class, 'spec');
  });
});

describe('budget + livelock classes', () => {
  it('scc exit 22 (MaxStepsExceeded) → budget', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 22, mutations: true }, opts);
    assert.equal(r.class, 'budget');
    assert.equal(r.terminal, 're-scope-or-raise-budget');
  });

  it('scc exit 23 (MaxRepairsExceeded) → livelock', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 23, mutations: false }, opts);
    assert.equal(r.class, 'livelock');
  });

  it('mimoclaw labels classify natively (budget_exceeded / no_changes)', () => {
    assert.equal(classifyStall({ engine: 'mimoclaw', exitCode: 22, mutations: true }, opts).class, 'budget');
    assert.equal(classifyStall({ engine: 'mimoclaw', exitCode: 10, mutations: false }, opts).class, 'spec');
    assert.equal(classifyStall({ engine: 'mimoclaw', exitCode: 0, mutations: true }, opts).class, 'ok');
  });
});

// --- manifest resolution precedence -------------------------------------------
describe('manifest resolution authority', () => {
  it('a declared resolution suppresses signature scraping', () => {
    const r = classifyStall({
      engine: 'scc', exitCode: 0, mutations: true,
      manifest: { resolution: { code: 'budget_exceeded', reason: 'max steps' } },
      outputText: 'fetch failed\nHTTP 429 quota exhausted',
    }, opts);
    assert.equal(r.class, 'budget');
    assert.equal(r.resolution, 'budget_exceeded');
  });

  it("'error' resolutions fall through to code+signature rules", () => {
    const r = classifyStall({
      engine: 'scc', exitCode: 20,
      manifest: { resolution: 'error' },
    }, opts);
    assert.equal(r.class, 'transport'); // stallClassMap wins below the manifest
  });
});

// --- helpers ------------------------------------------------------------------
describe('helpers', () => {
  it('manifestResolution reads string, object and legacy fields', () => {
    assert.equal(manifestResolution({ resolution: 'no_changes' }).code, 'no_changes');
    assert.equal(manifestResolution({ resolution: { code: 'Budget Exceeded' } }).code, 'budget_exceeded');
    assert.equal(manifestResolution({ exit_reason: 'engine_stall' }).code, 'engine_stall');
    assert.equal(manifestResolution({}), null);
    assert.equal(manifestResolution(null), null);
  });

  it('toolCallCount prefers explicit, then manifest, then audit scan', () => {
    assert.equal(toolCallCount({ toolCalls: 5, manifest: { tool_calls_total: 9 } }), 5);
    assert.equal(toolCallCount({ manifest: { tool_calls_total: 9 } }), 9);
    assert.equal(toolCallCount({ manifest: { iterations: 4 } }), 4);
    const audit = '{"type":"tool_call","x":1}\n{"type":"note"}\n{"type":"tool_call","x":2}\n';
    assert.equal(toolCallCount({ auditText: audit }), 2);
    assert.equal(toolCallCount({}), 0);
  });

  it('registryLabel + stallClassOverride read the real registry', () => {
    const { loadRegistry, adapterFor } = require('./stall-classifier');
    const scc = adapterFor(loadRegistry(REGISTRY), 'scc');
    assert.equal(registryLabel(scc, 20), 'AgentError');
    assert.equal(stallClassOverride(scc, 20), 'transport');
    assert.equal(stallClassOverride(scc, 9999), null);
  });
});

// --- CLI ----------------------------------------------------------------------
describe('CLI', () => {
  const tmp = (name, content) => {
    const f = path.join(os.tmpdir(), `stall-test-${process.pid}-${name}`);
    fs.writeFileSync(f, content);
    return f;
  };

  it('emits the verdict JSON with route + terminal evidence', () => {
    let out = '';
    const r = main(['--engine', 'scc', '--exit-code', '23', '--mutations', '0', '--json'],
      { stdout: (s) => { out = s; } });
    assert.equal(r.class, 'livelock');
    const parsed = JSON.parse(out);
    assert.equal(parsed.route, 'needs-human');
    assert.equal(parsed.terminal, 'livelock-escalation');
  });

  it('--field prints a scalar for shell capture', () => {
    let out = '';
    main(['--engine', 'scc', '--exit-code', '20', '--field', 'class'],
      { stdout: (s) => { out = s; } });
    assert.equal(out, 'transport');
  });

  it('reads manifest + audit + log files and writes --out', () => {
    const manifest = tmp('m.json', JSON.stringify({ resolution: 'no_changes', tool_calls_total: 2 }));
    const audit = tmp('a.jsonl', '{"type":"tool_call"}\n');
    const log = tmp('run.log', 'nothing happened');
    const outFile = tmp('out.json', '');
    const r = main(['--engine', 'scc', '--exit-code', '10', '--manifest', manifest,
      '--audit', audit, '--log', log, '--mutations', '0', '--out', outFile, '--json'],
      { stdout: () => {} });
    assert.equal(r.class, 'spec');
    assert.equal(JSON.parse(fs.readFileSync(outFile, 'utf8')).class, 'spec');
  });

  it('missing registry degrades to built-in defaults, never crashes', () => {
    // Shell timeout codes classify without any registry; unknown engine codes
    // fall to the generic tooling verdict.
    const r = classifyStall({ engine: 'scc', exitCode: 124, mutations: false, toolCalls: 0 },
      { registryPath: '/nonexistent/registry.json' });
    assert.equal(r.class, 'transport');
    assert.equal(classifyStall({ engine: 'scc', exitCode: 24, mutations: false },
      { registryPath: '/nonexistent/registry.json' }).class, 'tooling');
  });
});
