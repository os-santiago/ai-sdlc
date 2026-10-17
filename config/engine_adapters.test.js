// config/engine_adapters.test.js
// Consolidated test suite for the canonical engine-adapter layer:
//   - structural tests for the declarative registry (this file's original scope)
//   - behavior tests for the shared resolver/adapter contract
//     (scripts/engine_adapter.js — also re-exported to the GHA path at
//     scripts/gha/engine-adapter-resolver.js)
// Run with: node --test config/

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const REGISTRY_PATH = path.join(__dirname, 'engine_adapters.json');
const raw = fs.readFileSync(REGISTRY_PATH, 'utf8');
const registry = JSON.parse(raw);
const PLACEHOLDER_RE = /^\{[A-Za-z][A-Za-z0-9]*\}$/;
const PLACEHOLDER_NAME_RE = /^\{([A-Za-z][A-Za-z0-9]*)\}$/;

const {
  EngineAdapterResolver,
  getEngineAdapter,
  hasEngine,
  getRegisteredEngines,
} = require('../scripts/engine_adapter');
const GhaResolver = require('../scripts/gha/engine-adapter-resolver');

// ---------------------------------------------------------------------------
// Registry structure (the canonical declarative contract)
// ---------------------------------------------------------------------------

test('file ends with a trailing newline', () => {
  assert.ok(raw.endsWith('\n'), 'engine_adapters.json must end with a trailing newline');
});

test('registry contains the expected engines', () => {
  assert.deepEqual(Object.keys(registry).sort(),
    ['claude-code', 'codex', 'devin', 'mimoclaw', 'openhands', 'scc']);
});

test('every adapter declares binary, probe, capabilities, argvBuilder, and exitCodeMap', () => {
  for (const [name, adapter] of Object.entries(registry)) {
    assert.equal(typeof adapter.binary, 'string', `${name}.binary`);
    assert.ok(Array.isArray(adapter.probe) && adapter.probe.length > 0, `${name}.probe`);
    assert.equal(typeof adapter.capabilities, 'object', `${name}.capabilities`);
    assert.equal(typeof adapter.argvBuilder, 'object', `${name}.argvBuilder`);
    assert.ok(Array.isArray(adapter.argvBuilder.args), `${name}.argvBuilder.args`);
    assert.equal(typeof adapter.exitCodeMap, 'object', `${name}.exitCodeMap`);
  }
});

test('exitCodeMap always defines a default fallback for unknown codes', () => {
  for (const [name, adapter] of Object.entries(registry)) {
    const map = adapter.exitCodeMap;
    assert.ok('default' in map, `${name}.exitCodeMap must define a "default" fallback`);
    assert.equal(typeof map.default, 'string', `${name}.exitCodeMap.default must be a classification`);
    assert.ok('0' in map, `${name}.exitCodeMap must classify exit code 0`);
    for (const [code, classification] of Object.entries(map)) {
      if (code === 'default') continue;
      assert.match(code, /^\d+$/, `${name}.exitCodeMap key "${code}" must be a numeric exit code`);
      assert.equal(typeof classification, 'string', `${name}.exitCodeMap.${code} must be a classification`);
    }
  }
});

test('unknown exit codes resolve to a generic error, never undefined', () => {
  for (const [name, adapter] of Object.entries(registry)) {
    const map = adapter.exitCodeMap;
    // Simulate the resolver contract: unknown codes fall back to "default".
    for (const code of [-1, 2, 42, 127, 255, 9999]) {
      const resolved = code in map ? map[code] : map.default;
      assert.equal(typeof resolved, 'string', `${name}: exit code ${code} must map to a classification`);
      assert.notEqual(resolved, 'Success', `${name}: unknown exit code ${code} must not silently succeed`);
    }
  }
});

test('every placeholder occupies a whole argv element', () => {
  for (const [name, adapter] of Object.entries(registry)) {
    for (const arg of adapter.argvBuilder.args) {
      if (arg.includes('{') || arg.includes('}')) {
        assert.match(arg, PLACEHOLDER_RE,
          `${name}: "${arg}" must be a bare whole-element placeholder, not an embedded substring`);
      }
    }
  }
});

test('every placeholder is declared and validated in the substitution contract', () => {
  for (const [name, adapter] of Object.entries(registry)) {
    const contract = adapter.argvBuilder.substitution;
    assert.equal(typeof contract, 'object', `${name}.argvBuilder.substitution must exist`);
    assert.equal(contract.mode, 'argv-element', `${name} must use atomic argv-element substitution`);
    assert.equal(contract.shell, 'never', `${name} must forbid shell evaluation`);
    assert.equal(typeof contract.placeholders, 'object', `${name} must declare placeholders`);

    const used = adapter.argvBuilder.args
      .map((a) => a.match(PLACEHOLDER_NAME_RE))
      .filter(Boolean)
      .map((m) => m[1]);
    for (const placeholder of used) {
      const spec = contract.placeholders[placeholder];
      assert.ok(spec, `${name}: placeholder "{${placeholder}}" used in args must be declared`);
      assert.equal(typeof spec.pattern, 'string', `${name}: "{${placeholder}}" must define a validation pattern`);
      // Pattern must compile so a consumer can validate values before substitution.
      new RegExp(spec.pattern);
    }
    for (const declared of Object.keys(contract.placeholders)) {
      assert.ok(used.includes(declared),
        `${name}: declared placeholder "{${declared}}" must appear in args`);
    }
  }
});

test('placeholder validation patterns reject injection-shaped values where appropriate', () => {
  // Constrained tokens (model ids, file paths) must not accept whitespace,
  // quotes, or shell metacharacters that could split an argument. An
  // adapter only gets tested on the constrained placeholders it declares —
  // scc takes no model argv (the model lives in the rendered config.json,
  // issue #228), and prompt-style free-text placeholders legitimately
  // accept spaces as a single argv element.
  const CONSTRAINED = ['model', 'manifestFile', 'promptFile', 'auditLog', 'maxSeconds', 'maxSteps'];
  for (const [name, adapter] of Object.entries(registry)) {
    const placeholders = adapter.argvBuilder.substitution.placeholders;
    const declared = CONSTRAINED.filter((k) => placeholders[k]);
    assert.ok(declared.length > 0, `${name} must declare at least one constrained placeholder`);
    for (const key of declared) {
      const spec = placeholders[key];
      const re = new RegExp(spec.pattern);
      for (const hostile of ['a;rm -rf /', '$(id)', '`id`', 'a"b', 'a b', 'a\nb', 'a|b', 'a&b', 'a>b']) {
        assert.equal(re.test(hostile), false,
          `${name}: "{${key}}" pattern must reject ${JSON.stringify(hostile)}`);
      }
      // Legitimate values must still pass.
      const ordinary = { model: 'claude-3-opus', maxSeconds: '900', maxSteps: '200' }[key]
        || 'path/to/manifest.json';
      assert.equal(re.test(ordinary), true,
        `${name}: "{${key}}" pattern must accept ordinary values`);
    }
  }
});

// ---------------------------------------------------------------------------
// Resolver + adapter contract (single implementation, both consumers)
// ---------------------------------------------------------------------------

test('GHA resolver path re-exports the canonical resolver class', () => {
  assert.equal(GhaResolver, EngineAdapterResolver,
    'scripts/gha/engine-adapter-resolver must re-export the shared class, not a second implementation');
});

test('resolver loads the canonical registry by default', () => {
  const resolver = new EngineAdapterResolver();
  assert.deepEqual(resolver.getRegisteredEngines().sort(),
    ['claude-code', 'codex', 'devin', 'mimoclaw', 'openhands', 'scc']);
  assert.ok(resolver.hasEngine('scc'));
  assert.ok(!resolver.hasEngine('unknown'));
});

test('module facade resolves adapters from the canonical registry', () => {
  assert.deepEqual(getRegisteredEngines().sort(),
    ['claude-code', 'codex', 'devin', 'mimoclaw', 'openhands', 'scc']);
  assert.ok(hasEngine('devin'));
  const adapter = getEngineAdapter('scc');
  assert.equal(adapter.name, 'scc');
  assert.equal(adapter.binary, 'scc');
  assert.deepEqual(adapter.probe, ['scc', '--version']);
});

test('getEngineAdapter rejects unknown engine names instead of silently falling back', () => {
  assert.throws(() => getEngineAdapter('nonexistent-engine'), /Unknown engine adapter/);
  const resolver = new EngineAdapterResolver();
  assert.throws(() => resolver.getEngineAdapter('nonexistent-engine'), /Unknown engine ID/);
});

test('resolver throws for a missing or unparsable registry file', () => {
  assert.throws(
    () => new EngineAdapterResolver('/non/existent/path.json'),
    /Engine adapters registry not found/
  );
  const tmp = path.join(os.tmpdir(), `bad-registry-${process.pid}.json`);
  fs.writeFileSync(tmp, '{ invalid json }');
  try {
    assert.throws(() => new EngineAdapterResolver(tmp), /Invalid engine adapters registry/);
  } finally {
    fs.unlinkSync(tmp);
  }
});

test('resolver rejects malformed descriptors at load time', () => {
  const cases = [
    [{ bad: { probe: ['x'] } }, /missing binary/],
    [{ bad: { binary: 'x', argvBuilder: { args: [] } } }, /non-empty array/],
    [{
      bad: {
        binary: 'x',
        argvBuilder: {
          args: ['--model', '{model}'],
          substitution: { mode: 'argv-element', shell: 'never', placeholders: {} },
        },
        exitCodeMap: { 0: 'Success', default: 'AgentError' },
      },
    }, /undeclared placeholder/],
    [{
      bad: {
        binary: 'x',
        argvBuilder: {
          args: ['--model', '{model}'],
          substitution: {
            mode: 'argv-element', shell: 'never',
            placeholders: { model: { pattern: '^.*$', required: true }, ghost: { pattern: '^.*$' } },
          },
        },
        exitCodeMap: { 0: 'Success', default: 'AgentError' },
      },
    }, /never used in args/],
    [{
      bad: {
        binary: 'x',
        argvBuilder: {
          args: ['--model', '{model}'],
          substitution: {
            mode: 'argv-element', shell: 'never',
            placeholders: { model: { pattern: '^.*$' } },
          },
        },
        exitCodeMap: { 0: 'Success' },
      },
    }, /"default"/],
    [{
      bad: {
        binary: 'x',
        argvBuilder: {
          args: ['{model}'],
          substitution: {
            mode: 'argv-element', shell: 'never',
            placeholders: { model: { pattern: '^.*$' } },
          },
        },
        exitCodeMap: { 0: 'Success', '0x10': 'sneaky', default: 'AgentError' },
      },
    }, /not a decimal exit code/],
  ];
  for (const [doc, re] of cases) {
    const tmp = path.join(os.tmpdir(), `malformed-registry-${process.pid}-${Math.random().toString(36).slice(2)}.json`);
    fs.writeFileSync(tmp, JSON.stringify(doc));
    try {
      assert.throws(() => new EngineAdapterResolver(tmp), re, JSON.stringify(doc));
    } finally {
      fs.unlinkSync(tmp);
    }
  }
});

test('buildArgv substitutes placeholders atomically and prepends command', () => {
  // scc (issue #228): the tuned headless contract argv — the model stays
  // out of argv (rendered config.json is the engine's source of truth).
  const scc = getEngineAdapter('scc');
  assert.deepEqual(scc.buildArgv({
    promptFile: 'task-prompt.md',
    auditLog: 'out/scc-audit.jsonl',
    manifestFile: 'out/scc-manifest.json',
    maxSeconds: '900',
    maxSteps: '200',
  }), [
    'chat', '-yq',
    '--prompt-file', 'task-prompt.md',
    '--no-commit',
    '--audit-log', 'out/scc-audit.jsonl',
    '--summary-file', 'out/scc-manifest.json',
    '--max-seconds', '900',
    '--max-steps', '200',
  ]);
  // Atomicity on a free-text placeholder: a value with spaces/metachars
  // must land as exactly ONE argv element.
  const devin = getEngineAdapter('devin');
  const argv = devin.buildArgv({
    model: 'claude-3-opus',
    prompt: 'a;b c $(id)',
    manifestFile: 'out/manifest.json',
  });
  assert.deepEqual(argv, [
    'run',
    '--prompt', 'a;b c $(id)',
    '--model', 'claude-3-opus',
    '--manifest', 'out/manifest.json',
  ]);
});

test('buildArgv rejects values that fail a placeholder pattern without echoing them', () => {
  const adapter = getEngineAdapter('devin');
  assert.throws(
    () => adapter.buildArgv({ prompt: 'p', model: 'a;rm -rf /', manifestFile: 'm.json' }),
    (err) => {
      assert.match(err.message, /Invalid value for placeholder "\{model\}"/);
      assert.ok(!err.message.includes('rm -rf'), 'error must not echo the rejected value');
      return true;
    }
  );
});

test('buildArgv fails loudly on a missing required placeholder and names the env fallback', () => {
  const adapter = getEngineAdapter('devin');
  assert.throws(
    () => adapter.buildArgv({ prompt: 'p', model: 'm' }),
    /Missing required value for placeholder "\{manifestFile\}".*DEVIN_MANIFEST_FILE/
  );
});

test('buildArgv falls back to {ENGINE}_{PLACEHOLDER} environment variables', () => {
  process.env.DEVIN_MODEL = 'env-model';
  process.env.DEVIN_PROMPT = 'env prompt';
  process.env.DEVIN_MANIFEST_FILE = 'env/manifest.json';
  try {
    const adapter = getEngineAdapter('devin');
    assert.deepEqual(adapter.buildArgv(), [
      'run', '--prompt', 'env prompt', '--model', 'env-model', '--manifest', 'env/manifest.json',
    ]);
    // Explicit values win over the environment.
    assert.deepEqual(
      adapter.buildArgv({ model: 'explicit-model' }),
      ['run', '--prompt', 'env prompt', '--model', 'explicit-model', '--manifest', 'env/manifest.json']
    );
  } finally {
    delete process.env.DEVIN_MODEL;
    delete process.env.DEVIN_PROMPT;
    delete process.env.DEVIN_MANIFEST_FILE;
  }
});

test('getBinary honors the {ENGINE}_BINARY override and trims it', () => {
  const adapter = getEngineAdapter('scc');
  assert.equal(adapter.getBinary(), 'scc');
  process.env.SCC_BINARY = '  /opt/scc/bin  ';
  try {
    assert.equal(getEngineAdapter('scc').getBinary(), '/opt/scc/bin');
  } finally {
    delete process.env.SCC_BINARY;
  }
});

test('probeCapabilities returns the registry-declared capabilities', () => {
  assert.deepEqual(getEngineAdapter('scc').probeCapabilities(), {
    supportsWorkspaceTrust: false,
    modelConfigurable: true,
    projectIdRequired: true,
  });
  assert.deepEqual(getEngineAdapter('devin').probeCapabilities(), {
    supportsWorkspaceTrust: true,
    modelConfigurable: true,
    projectIdRequired: true,
  });
});

test('classifyExitCode maps declared codes and falls back to default', () => {
  const scc = getEngineAdapter('scc');
  assert.equal(scc.classifyExitCode(0), 'Success');
  assert.equal(scc.classifyExitCode(10), 'ValidationError');
  assert.equal(scc.classifyExitCode(24), 'TimeoutError');
  assert.equal(scc.classifyExitCode(9999), 'AgentError');
  const devin = getEngineAdapter('devin');
  assert.equal(devin.classifyExitCode(0), 'Success');
  assert.equal(devin.classifyExitCode(1), 'AgentError');
  assert.equal(devin.classifyExitCode(42), 'AgentError');
});

test('mapExitCode normalizes to 0 only for the declared success class', () => {
  for (const name of getRegisteredEngines()) {
    const adapter = getEngineAdapter(name);
    assert.equal(adapter.mapExitCode(0), 0, `${name}: success must exit 0`);
    for (const code of [1, 10, 20, 24, 127, 9999]) {
      assert.equal(adapter.mapExitCode(code), 1, `${name}: exit ${code} must normalize to failure`);
    }
  }
});

test('mapManifest is a passthrough; manifestMap stays declarative metadata', () => {
  const adapter = getEngineAdapter('devin');
  const manifest = { v: 1, success: true };
  assert.equal(adapter.mapManifest(manifest), manifest);
  assert.deepEqual(adapter.manifestMap, { path: '{manifestFile}', format: 'yaml' });
});

test('returned adapters cannot mutate the shared registry', () => {
  const adapter = getEngineAdapter('scc');
  adapter.argvBuilder.args.push('--corrupted');
  adapter.exitCodeMap.default = 'Success';
  const fresh = getEngineAdapter('scc');
  assert.ok(!fresh.argvBuilder.args.includes('--corrupted'));
  assert.equal(fresh.classifyExitCode(9999), 'AgentError');
});
