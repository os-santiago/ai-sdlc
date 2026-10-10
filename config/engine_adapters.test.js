// config/engine_adapters.test.js
// Structural tests for the declarative engine adapter registry.
// Run with: node --test config/

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const REGISTRY_PATH = path.join(__dirname, 'engine_adapters.json');
const raw = fs.readFileSync(REGISTRY_PATH, 'utf8');
const registry = JSON.parse(raw);
const PLACEHOLDER_RE = /^\{[A-Za-z][A-Za-z0-9]*\}$/;
const PLACEHOLDER_NAME_RE = /^\{([A-Za-z][A-Za-z0-9]*)\}$/;

test('file ends with a trailing newline', () => {
  assert.ok(raw.endsWith('\n'), 'engine_adapters.json must end with a trailing newline');
});

test('registry contains the expected engines', () => {
  assert.deepEqual(Object.keys(registry).sort(), ['devin', 'scc']);
});

test('every adapter declares binary, probe, argvBuilder, and exitCodeMap', () => {
  for (const [name, adapter] of Object.entries(registry)) {
    assert.equal(typeof adapter.binary, 'string', `${name}.binary`);
    assert.ok(Array.isArray(adapter.probe) && adapter.probe.length > 0, `${name}.probe`);
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
  // model and manifestFile are constrained tokens: they must not accept
  // whitespace, quotes, or shell metacharacters that could split an argument.
  for (const [name, adapter] of Object.entries(registry)) {
    const placeholders = adapter.argvBuilder.substitution.placeholders;
    for (const key of ['model', 'manifestFile']) {
      const spec = placeholders[key];
      assert.ok(spec, `${name} must declare "{${key}}"`);
      const re = new RegExp(spec.pattern);
      for (const hostile of ['a;rm -rf /', '$(id)', '`id`', 'a"b', 'a b', 'a\nb', 'a|b', 'a&b', 'a>b']) {
        assert.equal(re.test(hostile), false,
          `${name}: "{${key}}" pattern must reject ${JSON.stringify(hostile)}`);
      }
      // Legitimate values must still pass.
      assert.equal(re.test(key === 'model' ? 'claude-3-opus' : 'path/to/manifest.json'), true,
        `${name}: "{${key}}" pattern must accept ordinary values`);
    }
  }
});
