const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const path = require('path');

const ROOT = path.join(__dirname, '..');
const SCHEMA_PATH = path.join(ROOT, 'spec', 'ai-sdlc.schema.json');
const schema = JSON.parse(fs.readFileSync(SCHEMA_PATH, 'utf8'));

// Top-level keys of a contract file — every contract in this repo uses
// `key:` at column 0 for top-level mappings, so a line regex is enough.
// Root `additionalProperties: false` turns any undeclared key into a hard
// load-config failure in every stage — repo-wide (issue #255).
function contractTopLevelKeys(file) {
  const keys = [];
  for (const line of fs.readFileSync(file, 'utf8').split('\n')) {
    const m = /^([A-Za-z_][A-Za-z0-9_-]*):/.exec(line);
    if (m) keys.push(m[1]);
  }
  return keys;
}

function contractFiles() {
  const files = [path.join(ROOT, '.ai-sdlc.yaml')];
  for (const f of fs.readdirSync(path.join(ROOT, 'examples')).sort()) {
    if (f.endsWith('.ai-sdlc.yaml')) files.push(path.join(ROOT, 'examples', f));
  }
  return files;
}

describe('ai-sdlc.schema.json — contract surface', () => {
  it('rejects undeclared top-level keys (the drift tripwire)', () => {
    assert.strictEqual(schema.additionalProperties, false);
    assert.ok(schema.required.includes('spec'));
  });

  it('declares promotion with the issue-#211 evidence gate shape', () => {
    const p = schema.properties.promotion;
    assert.ok(p, 'missing properties.promotion');
    assert.deepStrictEqual(p.required.sort(), ['clean_merges_threshold', 'window'].sort());
    assert.strictEqual(p.properties.clean_merges_threshold.type, 'integer');
    assert.strictEqual(p.properties.clean_merges_threshold.minimum, 1);
    assert.strictEqual(p.properties.window.type, 'integer');
    // spec/trust-ladder.md: minimum N = 10 completed runs before promotion.
    assert.strictEqual(p.properties.window.minimum, 10);
  });

  it('declares demotion with the issue-#211 trigger shape', () => {
    const d = schema.properties.demotion;
    assert.ok(d, 'missing properties.demotion');
    assert.deepStrictEqual(d.required.sort(), ['escalation_threshold', 'window'].sort());
    assert.strictEqual(d.properties.escalation_threshold.type, 'integer');
    assert.strictEqual(d.properties.escalation_threshold.minimum, 1);
    assert.strictEqual(d.properties.window.type, 'integer');
    assert.strictEqual(d.properties.window.minimum, 1);
  });
});

describe('contract ↔ schema drift guard (issue #255)', () => {
  // Regression: a contract key merged without a matching schema property
  // fails `additionalProperties: false` — but only on main, where every
  // stage resolves the contract from. Caught here instead.
  for (const file of contractFiles()) {
    it(`${path.relative(ROOT, file)} declares only schema-known top-level keys`, () => {
      const undeclared = contractTopLevelKeys(file).filter(k => !(k in schema.properties));
      assert.deepStrictEqual(undeclared, []);
    });
  }
});
