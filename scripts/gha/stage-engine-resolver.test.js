// scripts/gha/stage-engine-resolver.test.js
// Per-stage engine dispatch tests (issue #206, epic #152): stage selection
// via `engines.<stage>`, fallback to the global `engine.name` / `scc`
// default, loud failure on an unregistered adapter, and end-to-end dispatch
// through the adapter argv contract.
// Run with: node --test scripts/gha/

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const RESOLVER = path.join(__dirname, 'stage-engine-resolver.js');
const REGISTRY = path.join(__dirname, '..', '..', 'config', 'engine_adapters.json');
const {
  StageEngineError,
  resolveStageEngineId,
  resolveStageAdapter,
  dispatchStageEngine,
} = require(RESOLVER);
const { EngineAdapterResolver } = require('../engine_adapter');

function tmpdir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'stage-engine-'));
}

function writeJson(dir, name, doc) {
  const p = path.join(dir, name);
  fs.writeFileSync(p, JSON.stringify(doc));
  return p;
}

// Fixture registry mirroring the canonical descriptor contract: binary +
// argvBuilder (whole-element placeholders) + exitCodeMap with '0'/'default'.
const FIXTURE_REGISTRY = {
  devin: {
    binary: 'devin',
    argvBuilder: {
      command: 'run',
      substitution: {
        mode: 'argv-element',
        shell: 'never',
        placeholders: {
          prompt: { pattern: '^[\\s\\S]{1,65536}$' },
          model: { pattern: '^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$' },
          manifestFile: { pattern: '^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$' },
        },
      },
      args: ['--prompt', '{prompt}', '--model', '{model}', '--manifest', '{manifestFile}'],
    },
    exitCodeMap: { '0': 'Success', '10': 'NoChanges', default: 'AgentError' },
  },
  openhands: {
    binary: 'openhands',
    argvBuilder: {
      command: 'exec',
      substitution: {
        mode: 'argv-element',
        shell: 'never',
        placeholders: {
          task: { pattern: '^[\\s\\S]{1,65536}$' },
          maxSeconds: { pattern: '^[0-9]{1,6}$' },
        },
      },
      args: ['--task', '{task}', '--max-seconds', '{maxSeconds}'],
    },
    exitCodeMap: { '0': 'Success', default: 'AgentError' },
  },
};

function fixtureResolver(dir) {
  const p = writeJson(dir || tmpdir(), 'registry.json', FIXTURE_REGISTRY);
  return new EngineAdapterResolver(p);
}

// ---------------------------------------------------------------------------
// Engine-id resolution: engines.<stage> → engine.name → 'scc'
// ---------------------------------------------------------------------------

test('stage entry wins over the global engine (shorthand id form)', () => {
  const contract = { spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'devin' } };
  assert.equal(resolveStageEngineId(contract, 'implement'), 'devin');
  // Untouched stages keep the global engine.
  assert.equal(resolveStageEngineId(contract, 'review'), 'scc');
});

test('stage-definition object form {engine: id} resolves the same id', () => {
  const contract = { spec: '1.0.0', engine: { name: 'scc' }, engines: { review: { engine: 'openhands' } } };
  assert.equal(resolveStageEngineId(contract, 'review'), 'openhands');
});

test('omitted stage engine falls back to the global engine.name', () => {
  const contract = { spec: '1.0.0', engine: { name: 'codex' }, engines: { implement: 'devin' } };
  assert.equal(resolveStageEngineId(contract, 'verify'), 'codex');
  // No engines map at all — still the global engine.
  assert.equal(resolveStageEngineId({ spec: '1.0.0', engine: { name: 'devin' } }, 'implement'), 'devin');
});

test('no stage entry and no engine.name falls back to the schema default scc', () => {
  assert.equal(resolveStageEngineId({ spec: '1.0.0' }, 'implement'), 'scc');
  assert.equal(resolveStageEngineId({ spec: '1.0.0', engines: {} }, 'intake'), 'scc');
  assert.equal(resolveStageEngineId({}, 'sweep'), 'scc');
});

test('resolution rejects malformed inputs loudly', () => {
  assert.throws(() => resolveStageEngineId(null, 'implement'), StageEngineError);
  assert.throws(() => resolveStageEngineId([], 'implement'), StageEngineError);
  assert.throws(() => resolveStageEngineId({ spec: '1.0.0' }, ''), StageEngineError);
  assert.throws(() => resolveStageEngineId({ spec: '1.0.0' }, '  '), StageEngineError);
});

// ---------------------------------------------------------------------------
// Adapter resolution against the registry
// ---------------------------------------------------------------------------

test('resolveStageAdapter returns the registered adapter for the stage engine', () => {
  const contract = { spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'devin' } };
  const r = resolveStageAdapter(contract, 'implement', REGISTRY);
  assert.equal(r.engine, 'devin');
  assert.equal(r.source, 'engines.implement');
  assert.equal(r.adapter.name, 'devin');
  assert.equal(r.adapter.binary, 'devin');
  // Global fallback reports its provenance.
  const g = resolveStageAdapter(contract, 'verify', REGISTRY);
  assert.equal(g.engine, 'scc');
  assert.equal(g.source, 'engine.name');
  assert.equal(resolveStageAdapter({}, 'sweep', REGISTRY).source, 'default');
});

test('certified-but-unregistered adapter id fails loudly, naming the engine', () => {
  // `codex` is a certified schema id (spec $defs.engine_id) — the canonical
  // registry now ships an entry for it, so the gate is exercised against a
  // fixture registry that omits it: dispatch must still refuse an id the
  // schema admits instead of silently running scc.
  const resolver = fixtureResolver();
  const contract = { spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'codex' } };
  assert.throws(
    () => resolveStageAdapter(contract, 'implement', resolver),
    (err) => {
      assert.ok(err instanceof StageEngineError);
      assert.match(err.message, /codex/);
      assert.match(err.message, /implement/);
      assert.match(err.message, /no adapter is registered/i);
      return true;
    }
  );
  // Same for the global engine when it names an unregistered id.
  assert.throws(
    () => resolveStageAdapter({ spec: '1.0.0', engine: { name: 'codex' } }, 'review', resolver),
    /codex/
  );
});

// ---------------------------------------------------------------------------
// Dispatch: adapter argv contract + exit-code mapping (issue #206 AC)
// ---------------------------------------------------------------------------

test('dispatch invokes the stage-selected adapter with its contract argv', () => {
  const resolver = fixtureResolver();
  const calls = [];
  const spawn = (cmd, argv, opts) => { calls.push({ cmd, argv, opts }); return { status: 0 }; };
  const contract = {
    spec: '1.0.0',
    engine: { name: 'scc' },
    engines: { intake: 'devin', review: { engine: 'openhands' } },
  };

  // Integration surface (issue AC): the intake/triage stage dispatches to
  // devin, the review stage to openhands — each through its own argv shape.
  const triage = dispatchStageEngine(contract, 'intake', {
    resolver,
    spawnImpl: spawn,
    values: { prompt: 'triage me', model: 'dev-model', manifestFile: 'run/manifest.json' },
  });
  assert.equal(triage.engine, 'devin');
  assert.equal(triage.command, 'devin');
  assert.deepEqual(triage.argv, ['run', '--prompt', 'triage me', '--model', 'dev-model', '--manifest', 'run/manifest.json']);
  assert.equal(triage.engineExitCode, 0);
  assert.equal(triage.classification, 'Success');
  assert.equal(triage.exitCode, 0);

  const review = dispatchStageEngine(contract, 'review', {
    resolver,
    spawnImpl: spawn,
    values: { task: 'review the diff', maxSeconds: '120' },
  });
  assert.equal(review.engine, 'openhands');
  assert.deepEqual(review.argv, ['exec', '--task', 'review the diff', '--max-seconds', '120']);
  assert.equal(calls.length, 2);
  assert.equal(calls[0].cmd, 'devin');
  assert.equal(calls[1].cmd, 'openhands');

  // A stage without an entry dispatches the global engine (scc) — fallback
  // path also goes through dispatch, not just resolution. The scc argv is
  // the tuned headless contract call (issue #228).
  const fallback = dispatchStageEngine(contract, 'sweep', {
    resolver: REGISTRY,
    spawnImpl: spawn,
    values: {
      promptFile: 'task-prompt.md', auditLog: 'scc-audit.jsonl',
      manifestFile: 'scc-manifest.json', maxSeconds: '900', maxSteps: '200',
    },
  });
  assert.equal(fallback.engine, 'scc');
  assert.equal(calls[2].cmd, 'scc');
  assert.deepEqual(calls[2].argv, [
    'chat', '-yq',
    '--prompt-file', 'task-prompt.md',
    '--no-commit',
    '--audit-log', 'scc-audit.jsonl',
    '--summary-file', 'scc-manifest.json',
    '--max-seconds', '900',
    '--max-steps', '200',
  ]);
});

test('scc dispatch argv is the tuned headless call, byte for byte (issue #228)', () => {
  // Parity with the workflow's legacy inline invocation:
  //   scc chat -yq --prompt-file task-prompt.md --no-commit \
  //     --audit-log <a> --summary-file <m> --max-seconds <s> --max-steps <t>
  const resolver = new EngineAdapterResolver(REGISTRY);
  const adapter = resolver.getEngineAdapter('scc');
  assert.deepEqual(
    adapter.buildArgv({
      promptFile: 'task-prompt.md',
      auditLog: 'scc-audit.jsonl',
      manifestFile: 'scc-manifest.json',
      maxSeconds: '900',
      maxSteps: '200',
    }),
    ['chat', '-yq',
      '--prompt-file', 'task-prompt.md',
      '--no-commit',
      '--audit-log', 'scc-audit.jsonl',
      '--summary-file', 'scc-manifest.json',
      '--max-seconds', '900',
      '--max-steps', '200']
  );
  // Absolute paths (RUNNER_TEMP work dirs) satisfy the path patterns.
  assert.deepEqual(
    adapter.buildArgv({
      promptFile: '/tmp/ci-repair/repair-prompt.md',
      auditLog: '/tmp/ci-repair/scc-audit.jsonl',
      manifestFile: '/tmp/ci-repair/scc-manifest.json',
      maxSeconds: '900',
      maxSteps: '200',
    })[3],
    '/tmp/ci-repair/repair-prompt.md'
  );
  // The model is NOT an argv placeholder — it lives in the rendered
  // config.json (engine's single source of truth); the fallback loop
  // re-renders it instead of passing a flag.
  assert.ok(!adapter.argvBuilder.args.includes('{model}'));
  // The declared SC_* env knobs stay visible in the descriptor.
  assert.deepEqual(adapter.envPassthrough, [
    'SC_PROVIDER_CONNECT_TIMEOUT_MS',
    'SC_PROVIDER_ATTEMPT_TIMEOUT_MS',
    'SC_FAILOVER',
  ]);
});

test('dispatch maps the engine exit code through the adapter exitCodeMap', () => {
  const resolver = fixtureResolver();
  const contract = { spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'devin' } };
  // Engine exit 10 classifies NoChanges → non-success class → pipeline exit 1.
  const r = dispatchStageEngine(contract, 'implement', {
    resolver,
    spawnImpl: () => ({ status: 10 }),
    values: { prompt: 'p', model: 'm', manifestFile: 'm.json' },
  });
  assert.equal(r.engineExitCode, 10);
  assert.equal(r.classification, 'NoChanges');
  assert.equal(r.exitCode, 1);
});

test('scc dispatch surfaces the raw engine exit code — the fallback taxonomy (issue #228)', () => {
  // dispatch.rawExitCodes on the scc descriptor keeps the engine's own
  // exit contract visible to the caller: 20/21 earn the one-shot model
  // fallback, 10 is no-changes — none may be flattened to a mapped 1.
  const contract = { spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'scc' } };
  const values = {
    promptFile: 'p.md', auditLog: 'a.jsonl', manifestFile: 'm.json',
    maxSeconds: '60', maxSteps: '10',
  };
  for (const raw of [0, 10, 20, 21, 22, 23, 24, 1]) {
    const r = dispatchStageEngine(contract, 'implement', {
      resolver: REGISTRY,
      spawnImpl: () => ({ status: raw }),
      values,
    });
    assert.equal(r.engine, 'scc');
    assert.equal(r.engineExitCode, raw);
    assert.equal(r.exitCode, raw, `raw exit code ${raw} must pass through unmapped`);
  }
  const r10 = dispatchStageEngine(contract, 'implement', {
    resolver: REGISTRY, spawnImpl: () => ({ status: 10 }), values,
  });
  assert.equal(r10.classification, 'ValidationError');
  const r21 = dispatchStageEngine(contract, 'implement', {
    resolver: REGISTRY, spawnImpl: () => ({ status: 21 }), values,
  });
  assert.equal(r21.classification, 'AgentFailedError');
});

test('dispatch treats a missing binary (spawn error) as an adapter failure, never success', () => {
  const resolver = fixtureResolver();
  const contract = { spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'devin' } };
  const r = dispatchStageEngine(contract, 'implement', {
    resolver,
    spawnImpl: () => ({ error: new Error('spawn devin ENOENT'), status: null }),
    values: { prompt: 'p', model: 'm', manifestFile: 'm.json' },
  });
  assert.equal(r.engineExitCode, null);
  assert.match(r.spawnError, /ENOENT/);
  assert.equal(r.classification, 'AgentError');
  assert.equal(r.exitCode, 1);
});

test('dispatch refuses an unregistered engine before spawning', () => {
  const resolver = fixtureResolver();
  let spawned = false;
  const contract = { spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'codex' } };
  assert.throws(
    () => dispatchStageEngine(contract, 'implement', {
      resolver,
      spawnImpl: () => { spawned = true; return { status: 0 }; },
      values: {},
    }),
    /codex/
  );
  assert.equal(spawned, false, 'nothing may spawn when the adapter is unknown');
});

test('dispatch surfaces missing required placeholder values as a stage error', () => {
  const resolver = fixtureResolver();
  const contract = { spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'devin' } };
  assert.throws(
    () => dispatchStageEngine(contract, 'implement', {
      resolver,
      spawnImpl: () => ({ status: 0 }),
      values: { prompt: 'p' }, // model + manifestFile missing
    }),
    (err) => {
      assert.ok(err instanceof StageEngineError);
      assert.match(err.message, /implement/);
      assert.match(err.message, /devin/);
      assert.match(err.message, /model/);
      return true;
    }
  );
});

// ---------------------------------------------------------------------------
// CLI surface (what the reusable workflows invoke)
// ---------------------------------------------------------------------------

function runCli(args, env = {}, cwd) {
  return spawnSync(process.execPath, [RESOLVER, ...args], {
    encoding: 'utf8',
    cwd: cwd || __dirname,
    env: { ...process.env, ...env },
  });
}

test('CLI resolves the per-stage engine and writes GITHUB_OUTPUT', () => {
  const dir = tmpdir();
  const contract = writeJson(dir, 'contract.json', {
    spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'devin' },
  });
  const ghOut = path.join(dir, 'gh-output');
  fs.writeFileSync(ghOut, '');
  const r = runCli(
    ['--stage', 'implement', '--contract', contract, '--registry', REGISTRY],
    { GITHUB_OUTPUT: ghOut }
  );
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /engine=devin/);
  const outputs = fs.readFileSync(ghOut, 'utf8');
  assert.match(outputs, /^engine=devin$/m);
  assert.match(outputs, /^engine_source=engines\.implement$/m);
  assert.match(outputs, /^engine_binary=devin$/m);
});

test('CLI falls back to the global engine when the stage omits one', () => {
  const dir = tmpdir();
  const contract = writeJson(dir, 'contract.json', {
    spec: '1.0.0', engine: { name: 'devin' }, engines: { implement: 'scc' },
  });
  const r = runCli(['--stage', 'review', '--contract', contract, '--registry', REGISTRY]);
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /engine=devin/);
  assert.match(r.stdout, /source=engine\.name/);
});

test('CLI exits 1 with a clear message naming the unknown adapter', () => {
  const dir = tmpdir();
  const contract = writeJson(dir, 'contract.json', {
    spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'codex' },
  });
  // Fixture registry without `codex` — the canonical registry registers it.
  const registry = writeJson(dir, 'registry.json', FIXTURE_REGISTRY);
  const r = runCli(['--stage', 'implement', '--contract', contract, '--registry', registry]);
  assert.equal(r.status, 1, `expected exit 1, got ${r.status}: ${r.stdout}${r.stderr}`);
  const log = r.stdout + r.stderr;
  assert.match(log, /codex/);
  assert.match(log, /implement/);
  assert.match(log, /no adapter is registered/i);
});

test('CLI missing contract: --required true fails, --required false resolves the default engine', () => {
  const dir = tmpdir();
  const missing = path.join(dir, 'absent.json');
  const fatal = runCli(['--stage', 'implement', '--contract', missing, '--registry', REGISTRY]);
  assert.equal(fatal.status, 1);
  assert.match(fatal.stdout + fatal.stderr, /contract not found/i);
  const advisory = runCli(
    ['--stage', 'implement', '--contract', missing, '--registry', REGISTRY, '--required', 'false'],
    { GITHUB_OUTPUT: '' }
  );
  assert.equal(advisory.status, 0, advisory.stderr);
  assert.match(advisory.stdout, /engine=scc/);
});

test('CLI --run invokes the adapter binary with registry argv and maps the exit code', () => {
  const dir = tmpdir();
  // Fake engine binary: records argv, then exits per FAKE_ENG_EXIT.
  const fake = path.join(dir, 'fake-eng-bin');
  fs.writeFileSync(fake, [
    '#!/usr/bin/env node',
    "require('fs').writeFileSync(process.env.FAKE_ENG_ARGV_OUT, JSON.stringify(process.argv.slice(2)));",
    'process.exit(Number(process.env.FAKE_ENG_EXIT || 0));',
    '',
  ].join('\n'));
  fs.chmodSync(fake, 0o755);
  const registry = writeJson(dir, 'registry.json', {
    'fake-eng': {
      binary: 'fake-eng-bin',
      argvBuilder: {
        command: 'do',
        substitution: {
          mode: 'argv-element',
          shell: 'never',
          placeholders: {
            prompt: { pattern: '^[\\s\\S]{1,65536}$' },
            model: { pattern: '^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$' },
          },
        },
        args: ['--prompt', '{prompt}', '--model', '{model}', '--json'],
      },
      exitCodeMap: { '0': 'Success', '10': 'NoChanges', default: 'AgentError' },
    },
  });
  const contract = writeJson(dir, 'contract.json', {
    spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'fake-eng' },
  });
  const argvOut = path.join(dir, 'argv.json');
  const ghOut = path.join(dir, 'gh-output');
  fs.writeFileSync(ghOut, '');

  // Success class → process exit 0, argv built by the adapter contract.
  let r = runCli(
    ['--stage', 'implement', '--contract', contract, '--registry', registry,
      '--run', '--model', 'm-1', '--set', 'prompt=do the thing'],
    { FAKE_ENG_BINARY: fake, FAKE_ENG_ARGV_OUT: argvOut, FAKE_ENG_EXIT: '0', GITHUB_OUTPUT: ghOut }
  );
  assert.equal(r.status, 0, `${r.stdout}${r.stderr}`);
  assert.deepEqual(JSON.parse(fs.readFileSync(argvOut, 'utf8')),
    ['do', '--prompt', 'do the thing', '--model', 'm-1', '--json']);
  assert.match(fs.readFileSync(ghOut, 'utf8'), /^exit_code=0$/m);

  // Non-success engine class (10 → NoChanges) → adapter-mapped exit 1.
  r = runCli(
    ['--stage', 'implement', '--contract', contract, '--registry', registry,
      '--run', '--model', 'm-1', '--set', 'prompt=do the thing'],
    { FAKE_ENG_BINARY: fake, FAKE_ENG_ARGV_OUT: argvOut, FAKE_ENG_EXIT: '10' }
  );
  assert.equal(r.status, 1, `${r.stdout}${r.stderr}`);
  assert.match(r.stdout, /classification=NoChanges/);
  assert.match(r.stdout, /engine_exit=10/);

  // Registered adapter, absent binary → mapped failure, never silent success.
  r = runCli(
    ['--stage', 'implement', '--contract', contract, '--registry', registry,
      '--run', '--model', 'm-1', '--set', 'prompt=x'],
    { FAKE_ENG_BINARY: path.join(dir, 'no-such-binary'), FAKE_ENG_ARGV_OUT: argvOut }
  );
  assert.equal(r.status, 1);
  assert.match(r.stdout + r.stderr, /failed to spawn/);
});

test('CLI --run scc dispatches the tuned argv and exits with the raw engine code (issue #228)', () => {
  const dir = tmpdir();
  // Fake scc binary: records argv, then exits per FAKE_SCC_EXIT. Exercised
  // through the {ENGINE}_BINARY override on the CANONICAL registry — the
  // descriptor under test is the shipped scc adapter, not a fixture.
  const fake = path.join(dir, 'fake-scc');
  fs.writeFileSync(fake, [
    '#!/usr/bin/env node',
    "require('fs').writeFileSync(process.env.FAKE_SCC_ARGV_OUT, JSON.stringify(process.argv.slice(2)));",
    'process.exit(Number(process.env.FAKE_SCC_EXIT || 0));',
    '',
  ].join('\n'));
  fs.chmodSync(fake, 0o755);
  const contract = writeJson(dir, 'contract.json', {
    spec: '1.0.0', engine: { name: 'scc' }, engines: { implement: 'scc' },
  });
  const argvOut = path.join(dir, 'argv.json');
  const ghOut = path.join(dir, 'gh-output');
  fs.writeFileSync(ghOut, '');
  const args = [
    '--stage', 'implement', '--contract', contract, '--registry', REGISTRY,
    '--run',
    '--set', 'promptFile=task-prompt.md',
    '--set', 'auditLog=scc-audit.jsonl',
    '--manifest', 'scc-manifest.json',
    '--set', 'maxSeconds=900',
    '--set', 'maxSteps=200',
  ];
  const env = { SCC_BINARY: fake, FAKE_SCC_ARGV_OUT: argvOut, GITHUB_OUTPUT: ghOut };

  // The workflow's legacy inline call, argv for argv:
  //   scc chat -yq --prompt-file task-prompt.md --no-commit \
  //     --audit-log scc-audit.jsonl --summary-file scc-manifest.json \
  //     --max-seconds 900 --max-steps 200
  let r = runCli(args, { ...env, FAKE_SCC_EXIT: '0' });
  assert.equal(r.status, 0, `${r.stdout}${r.stderr}`);
  assert.deepEqual(JSON.parse(fs.readFileSync(argvOut, 'utf8')), [
    'chat', '-yq',
    '--prompt-file', 'task-prompt.md',
    '--no-commit',
    '--audit-log', 'scc-audit.jsonl',
    '--summary-file', 'scc-manifest.json',
    '--max-seconds', '900',
    '--max-steps', '200',
  ]);

  // Raw-exit dispatch: the engine's own taxonomy reaches the caller —
  // 10 no-changes and 20 provider-failure (the fallback trigger) must NOT
  // collapse to the mapped 1.
  for (const code of ['10', '20']) {
    r = runCli(args, { ...env, FAKE_SCC_EXIT: code });
    assert.equal(r.status, Number(code), `expected raw exit ${code}, got ${r.status}: ${r.stdout}${r.stderr}`);
  }
  r = runCli(args, { ...env, FAKE_SCC_EXIT: '20' });
  assert.match(r.stdout, /classification=AgentError/);
  assert.match(fs.readFileSync(ghOut, 'utf8'), /^exit_code=20$/m);
  assert.match(fs.readFileSync(ghOut, 'utf8'), /^engine_exit_code=20$/m);

  // A missing placeholder fails before spawn (bounded contract) — never
  // substitutes garbage argv.
  r = runCli(
    ['--stage', 'implement', '--contract', contract, '--registry', REGISTRY,
      '--run', '--set', 'promptFile=task-prompt.md'],
    { SCC_BINARY: fake, FAKE_SCC_ARGV_OUT: argvOut }
  );
  assert.equal(r.status, 1);
});
