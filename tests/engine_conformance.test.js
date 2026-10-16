// tests/engine_conformance.test.js
// node --test entry point for the engine-adapter conformance suite (issue
// #217). Wraps tests/engine_conformance_harness.js so every registered
// adapter's conformance checks land as named subtests — picked up by the
// `*/*.test.js` step of tools/validate-repo.sh, hence by the contract job in
// CI. Also asserts the harness's process-level contract: exit 0 when all
// adapters conform, non-zero on any failure, and the JSON report artifact.

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const HARNESS_PATH = path.join(__dirname, 'engine_conformance_harness.js');
const {
  runConformance,
  DEFAULT_REGISTRY_PATH,
} = require('./engine_conformance_harness');
const { EngineAdapterResolver } = require('../scripts/engine_adapter');

function runCli(args) {
  return spawnSync(process.execPath, [HARNESS_PATH, ...args], { encoding: 'utf8' });
}

test('conformance suite: global registry checks', async (t) => {
  const report = runConformance();
  for (const check of report.global) {
    await t.test(check.id, (st) => {
      if (check.status === 'skip') return st.skip(check.detail);
      assert.equal(check.status, 'pass', check.detail);
      return undefined;
    });
  }
});

test('conformance suite: every registered adapter conforms', async (t) => {
  const resolver = new EngineAdapterResolver(DEFAULT_REGISTRY_PATH);
  for (const engineId of resolver.getRegisteredEngines()) {
    await t.test(engineId, async (st) => {
      const report = runConformance({ engines: [engineId] });
      assert.equal(report.engines.length, 1);
      for (const check of report.engines[0].checks) {
        await st.test(check.id, (sst) => {
          if (check.status === 'skip') return sst.skip(check.detail);
          assert.equal(check.status, 'pass', check.detail);
          return undefined;
        });
      }
    });
  }
});

test('harness exits 0 when every adapter conforms', () => {
  const res = runCli([]);
  assert.equal(res.status, 0, res.stderr || res.stdout);
  assert.match(res.stdout, /CONFORMING/);
});

test('harness exits non-zero when an adapter fails a check', () => {
  // A descriptor that passes load-time validation but violates a behavioral
  // invariant: probe[0] does not invoke the declared binary.
  const registry = {
    fake: {
      binary: 'fake-engine',
      probe: ['not-the-binary', '--version'],
      capabilities: { modelConfigurable: true },
      argvBuilder: {
        command: 'run',
        substitution: {
          mode: 'argv-element',
          shell: 'never',
          placeholders: {
            prompt: { pattern: '^[^\\u0000]{1,65536}$', required: true },
          },
        },
        args: ['--prompt', '{prompt}'],
      },
      exitCodeMap: { 0: 'Success', default: 'AgentError' },
      manifestMap: { path: '{manifestFile}', format: 'json' },
    },
  };
  const tmp = path.join(os.tmpdir(), `nonconforming-registry-${process.pid}.json`);
  fs.writeFileSync(tmp, JSON.stringify(registry));
  try {
    const res = runCli(['--registry', tmp]);
    assert.equal(res.status, 1, `expected exit 1; stdout:\n${res.stdout}\nstderr:\n${res.stderr}`);
    assert.match(res.stdout, /\[FAIL\] fake\/probe-declaration/);
    assert.match(res.stdout, /NON-CONFORMING/);
  } finally {
    fs.unlinkSync(tmp);
  }
});

test('harness exits non-zero when the registry cannot be loaded', () => {
  const res = runCli(['--registry', '/non/existent/registry.json']);
  assert.equal(res.status, 1);
});

test('harness rejects an unregistered --engine selection', () => {
  const res = runCli(['--engine', 'no-such-engine']);
  assert.equal(res.status, 1, res.stdout);
  assert.match(res.stdout, /unregistered engine/);
});

test('harness --engine restricts the run to the selected adapter', () => {
  const res = runCli(['--engine', 'devin']);
  assert.equal(res.status, 0, res.stderr || res.stdout);
  assert.match(res.stdout, /across 1 engine\(s\)/);
});

test('harness --report writes the machine-readable report artifact', () => {
  const reportPath = path.join(os.tmpdir(), `conformance-report-${process.pid}.json`);
  try {
    const res = runCli(['--report', reportPath]);
    assert.equal(res.status, 0, res.stderr || res.stdout);
    const report = JSON.parse(fs.readFileSync(reportPath, 'utf8'));
    assert.equal(report.ok, true);
    assert.equal(report.summary.failed, 0);
    const resolver = new EngineAdapterResolver(DEFAULT_REGISTRY_PATH);
    assert.deepEqual(
      report.engines.map((e) => e.engineId).sort(),
      resolver.getRegisteredEngines().sort()
    );
    assert.ok(report.engines.every((e) => e.checks.length > 0),
      'every engine entry must carry its per-check results');
  } finally {
    if (fs.existsSync(reportPath)) fs.unlinkSync(reportPath);
  }
});
