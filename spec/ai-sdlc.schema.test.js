const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawnSync } = require('child_process');

const ROOT = path.join(__dirname, '..');
const SCHEMA_PATH = path.join(ROOT, 'spec', 'ai-sdlc.schema.json');
const schema = JSON.parse(fs.readFileSync(SCHEMA_PATH, 'utf8'));
const REGISTRY_PATH = path.join(ROOT, 'config', 'engine_adapters.json');
const RESOLVER = path.join(ROOT, '.github', 'actions', 'load-config', 'resolve.py');

const HAS_PY = spawnSync('python3', ['-c', 'import jsonschema'], { stdio: 'ignore' }).status === 0;

// Validate a contract object against the schema with python3 + jsonschema —
// one of the two validators tools/validate-ai-sdlc-config.sh supports, so
// these verdicts match what the contract gate emits on a runner.
// Returns {valid: bool, errors: [str]}.
const PY_VALIDATE = `
import json, sys, jsonschema
schema = json.load(open(sys.argv[1])); data = json.load(open(sys.argv[2]))
v = jsonschema.Draft202012Validator(schema)
errs = ["%s: %s" % (list(e.absolute_path), e.message) for e in v.iter_errors(data)]
print(json.dumps({"valid": not errs, "errors": errs}))
`;
function schemaValidate(contract) {
  const tmp = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-')), 'contract.json');
  fs.writeFileSync(tmp, JSON.stringify(contract));
  const r = spawnSync('python3', ['-c', PY_VALIDATE, SCHEMA_PATH, tmp], { encoding: 'utf8' });
  assert.strictEqual(r.status, 0, `validator harness failed: ${r.stderr}`);
  return JSON.parse(r.stdout);
}

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

  it('declares promotion.required_signals as the closed four-signal taxonomy (issue #179)', () => {
    const rs = schema.properties.promotion.properties.required_signals;
    assert.ok(rs, 'missing promotion.required_signals');
    assert.deepStrictEqual(
      rs.items.enum.slice().sort(),
      ['audit-trail', 'behavioral-eval', 'static-config', 'track-record'],
    );
    // "Track record alone never suffices": mandatory signal + minItems 2.
    assert.strictEqual(rs.minItems, 2);
    assert.deepStrictEqual(rs.contains, { const: 'track-record' });
    assert.strictEqual(rs.uniqueItems, true);
  });

  it('declares promotion.overrides as a closed per-rung threshold map (issue #179)', () => {
    const ov = schema.properties.promotion.properties.overrides;
    assert.ok(ov, 'missing promotion.overrides');
    assert.strictEqual(ov.additionalProperties, false);
    assert.deepStrictEqual(
      Object.keys(ov.properties).sort(),
      ['auto-deploy', 'auto-merge-all', 'auto-merge-low', 'auto-pr', 'suggest'],
    );
    for (const lvl of Object.keys(ov.properties)) {
      assert.strictEqual(ov.properties[lvl].$ref, '#/$defs/promotion_thresholds');
    }
    const t = schema.$defs.promotion_thresholds;
    assert.strictEqual(t.additionalProperties, false);
    assert.strictEqual(t.properties.clean_runs_threshold.minimum, 1);
    assert.strictEqual(t.properties.clean_merges_threshold.minimum, 1);
    assert.strictEqual(t.properties.window.minimum, 10);
  });

  it('accepts the new evidence fields and rejects track-record-only signals', { skip: !HAS_PY }, () => {
    const ok = schemaValidate({
      spec: '1.0.0',
      promotion: {
        clean_merges_threshold: 3,
        window: 30,
        required_signals: ['track-record', 'static-config'],
        overrides: { 'auto-merge-all': { clean_merges_threshold: 10 } },
      },
      demotion: { escalation_threshold: 1, window: 30, priority_label: 'priority:P0' },
    });
    assert.ok(ok.valid, `valid evidence contract rejected: ${ok.errors}`);

    // track-record alone never suffices — schema enforces it.
    const lone = schemaValidate({
      spec: '1.0.0',
      promotion: {
        clean_merges_threshold: 3,
        window: 30,
        required_signals: ['track-record'],
      },
    });
    assert.strictEqual(lone.valid, false);

    // Unknown signal id and unknown rung key are rejected, not silently dropped.
    const badSignal = schemaValidate({
      spec: '1.0.0',
      promotion: { clean_merges_threshold: 3, window: 30, required_signals: ['track-record', 'vibes'] },
    });
    assert.strictEqual(badSignal.valid, false);
    const badRung = schemaValidate({
      spec: '1.0.0',
      promotion: { clean_merges_threshold: 3, window: 30, overrides: { 'auto-yolo': { clean_merges_threshold: 5 } } },
    });
    assert.strictEqual(badRung.valid, false);
  });

  it('declares review.adversarial with the contract-owned trigger threshold (issue #180)', () => {
    const r = schema.properties.review;
    assert.ok(r, 'missing properties.review');
    const adv = r.properties.adversarial;
    assert.ok(adv, 'missing review.adversarial — the adversarial trigger threshold must be contract-declared');
    assert.strictEqual(adv.properties.min_tier.default, 'high');
    assert.deepStrictEqual(adv.properties.min_tier.enum, ['low', 'medium', 'high', 'critical']);
    assert.strictEqual(adv.properties.max_attacks.type, 'integer');
  });

  it('declares demotion with the issue-#211 trigger shape', () => {
    const d = schema.properties.demotion;
    assert.ok(d, 'missing properties.demotion');
    assert.deepStrictEqual(d.required.sort(), ['escalation_threshold', 'window'].sort());
    assert.strictEqual(d.properties.escalation_threshold.type, 'integer');
    assert.strictEqual(d.properties.escalation_threshold.minimum, 1);
    assert.strictEqual(d.properties.window.type, 'integer');
    assert.strictEqual(d.properties.window.minimum, 1);
    // issue #179: sentinel-halt demotion proposals carry a priority label.
    assert.strictEqual(d.properties.priority_label.type, 'string');
    assert.strictEqual(d.properties.priority_label.default, 'priority:P0');
  });

  it('declares postmortem with the spec lifecycle knobs (issue #191)', () => {
    const p = schema.properties.postmortem;
    assert.ok(p, 'missing properties.postmortem');
    assert.strictEqual(p.type, 'object');
    assert.strictEqual(p.additionalProperties, false);
    // spec/postmortem-lifecycle.md: reopen threshold + closed-issue window.
    assert.strictEqual(p.properties.reopen_min.type, 'integer');
    assert.strictEqual(p.properties.reopen_min.minimum, 1);
    assert.strictEqual(p.properties.dedup_window_days.type, 'integer');
    assert.strictEqual(p.properties.dedup_window_days.minimum, 1);
  });

  it('accepts a postmortem section and resolves its defaults (load-config)', { skip: !HAS_PY }, () => {
    const r = schemaValidate({ spec: '1.0.0', postmortem: { reopen_min: 3, dedup_window_days: 30 } });
    assert.deepStrictEqual(r.errors, []);
    assert.ok(r.valid);
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-resolve-'));
    const input = path.join(dir, 'in.json');
    fs.writeFileSync(input, JSON.stringify({ spec: '1.0.0' }));
    const rr = spawnSync('python3', [RESOLVER, '--input', input, '--out-dir', dir], { encoding: 'utf8' });
    assert.strictEqual(rr.status, 0, `resolve.py failed: ${rr.stderr}`);
    const contract = JSON.parse(fs.readFileSync(path.join(dir, 'contract.json'), 'utf8'));
    assert.deepStrictEqual(contract.postmortem, { reopen_min: 2, dedup_window_days: 90 });
    assert.strictEqual(contract._sources['postmortem.reopen_min'], 'default');
  });

  it('declares dependencies with the issue-#177 dep-update worker shape', () => {
    const d = schema.properties.dependencies;
    assert.ok(d, 'missing properties.dependencies');
    assert.strictEqual(d.type, 'object');
    assert.strictEqual(d.additionalProperties, false);
    // Supply-chain age gate + risk thresholds must be contract-declared.
    assert.strictEqual(d.properties.min_release_age_days.type, 'integer');
    assert.strictEqual(d.properties.min_release_age_days.default, 7);
    assert.deepStrictEqual(d.properties.auto_kinds.items.enum.sort(), ['minor', 'patch']);
    assert.deepStrictEqual(d.properties.target.enum, ['wanted', 'latest']);
    assert.strictEqual(d.properties.enabled.default, false);
    assert.ok(d.properties.egress_profile, 'missing egress_profile (named network allowlist)');
  });

  it('accepts a dependencies block and rejects undeclared fields', { skip: !HAS_PY }, () => {
    const ok = schemaValidate({ spec: '1.0.0', dependencies: { enabled: true, auto_kinds: ['patch'] } });
    assert.deepStrictEqual(ok.errors, []);
    assert.ok(ok.valid);
    const bad = schemaValidate({ spec: '1.0.0', dependencies: { enabled: true, auto_kinds: ['major'], bogus: 1 } });
    assert.strictEqual(bad.valid, false);
    assert.ok(bad.errors.length >= 2, `expected enum + additionalProperties errors: ${bad.errors}`);
  });

  it('declares release with the issue-#100/#103 provenance shape', () => {
    const r = schema.properties.release;
    assert.ok(r, 'missing properties.release');
    assert.strictEqual(r.type, 'object');
    assert.strictEqual(r.additionalProperties, false);
    // Secure postures are the resolved defaults — opting out is explicit.
    assert.deepStrictEqual(r.properties.provenance.enum, ['attest', 'none']);
    assert.strictEqual(r.properties.provenance.default, 'attest');
    assert.deepStrictEqual(r.properties.signing.enum, ['cosign-keyless', 'none']);
    assert.strictEqual(r.properties.signing.default, 'cosign-keyless');
  });

  it('accepts a release block and rejects undeclared fields', { skip: !HAS_PY }, () => {
    const ok = schemaValidate({ spec: '1.0.0', release: { provenance: 'attest', signing: 'cosign-keyless' } });
    assert.deepStrictEqual(ok.errors, []);
    assert.ok(ok.valid);
    const bad = schemaValidate({ spec: '1.0.0', release: { provenance: 'sigstore', bogus: 1 } });
    assert.strictEqual(bad.valid, false);
    assert.ok(bad.errors.length >= 2, `expected enum + additionalProperties errors: ${bad.errors}`);
  });

  it('declares the issue-#90 scan modes and sbom contract on security.*', () => {
    const sec = schema.properties.security;
    assert.ok(sec, 'missing properties.security');
    const mode = schema.$defs.security_scan_mode;
    assert.ok(mode, 'missing $defs.security_scan_mode');
    assert.deepStrictEqual(mode.enum, ['off', 'report', 'enforce']);
    for (const f of ['secrets_scan', 'dependency_audit', 'license_check']) {
      assert.strictEqual(sec.properties[f].$ref, '#/$defs/security_scan_mode',
        `security.${f} must share the security_scan_mode taxonomy`);
    }
    assert.strictEqual(sec.properties.secrets_scan.default, 'enforce');
    assert.strictEqual(sec.properties.dependency_audit.default, 'enforce');
    assert.strictEqual(sec.properties.license_check.default, 'report');
    const sbom = sec.properties.sbom;
    assert.ok(sbom, 'missing security.sbom');
    assert.strictEqual(sbom.type, 'object');
    assert.strictEqual(sbom.additionalProperties, false);
    assert.strictEqual(sbom.properties.enabled.type, 'boolean');
    assert.strictEqual(sbom.properties.enabled.default, false);
    assert.deepStrictEqual(sbom.properties.format.enum, ['cyclonedx', 'spdx']);
    assert.strictEqual(sbom.properties.format.default, 'cyclonedx');
    assert.deepStrictEqual(sbom.properties.attach_to.items.enum, ['pr', 'release']);
    assert.strictEqual(sbom.properties.attach_to.uniqueItems, true);
    assert.deepStrictEqual(sbom.properties.attach_to.default, ['pr']);
  });

  it('accepts the richer security surface and rejects off-taxonomy values', { skip: !HAS_PY }, () => {
    const ok = schemaValidate({
      spec: '1.0.0',
      security: {
        secrets_scan: 'enforce',
        dependency_audit: 'report',
        license_check: 'enforce',
        sbom: { enabled: true, format: 'spdx', attach_to: ['pr', 'release'] },
      },
    });
    assert.deepStrictEqual(ok.errors, []);
    assert.ok(ok.valid);
    const bad = schemaValidate({
      spec: '1.0.0',
      security: { secrets_scan: 'paranoid', sbom: { attach_to: ['slack'], bogus: 1 } },
    });
    assert.strictEqual(bad.valid, false);
    assert.ok(bad.errors.length >= 3, `expected enum + enum + additionalProperties errors: ${bad.errors}`);
  });
});

describe('per-stage engine selection (issue #205)', () => {
  const STAGES = ['intake', 'implement', 'review', 'automerge', 'verify', 'ci-repair', 'sweep', 'dep-update'];

  it('declares engines as a closed stage → adapter map over the known pipeline stages', () => {
    const e = schema.properties.engines;
    assert.ok(e, 'missing properties.engines');
    assert.strictEqual(e.type, 'object');
    assert.strictEqual(e.additionalProperties, false);
    assert.deepStrictEqual(Object.keys(e.properties).sort(), STAGES.slice().sort());
  });

  it('constrains engine ids to the certified adapter set in $defs.engine_id', () => {
    const ids = schema.$defs.engine_id;
    assert.ok(ids, 'missing $defs.engine_id');
    // claude-code must be a contract-valid id (epic #152 certification
    // target) even before its adapter registry entry lands (issue #184).
    for (const id of ['scc', 'devin', 'claude-code', 'codex', 'openhands', 'mimoclaw']) {
      assert.ok(ids.enum.includes(id), `engine_id enum missing ${id}`);
    }
  });

  it('keeps every registry adapter selectable — engine_adapters.json ⊆ engine_id enum', () => {
    const registry = JSON.parse(fs.readFileSync(REGISTRY_PATH, 'utf8'));
    const allowed = new Set(schema.$defs.engine_id.enum);
    for (const id of Object.keys(registry)) {
      assert.ok(allowed.has(id), `registry adapter ${id} is not a contract-valid engine id`);
    }
  });

  it('applies the certified set to the global engine.name default too', () => {
    assert.strictEqual(schema.properties.engine.properties.name.$ref, '#/$defs/engine_id');
  });

  it('accepts a stage-definition object with `engine: <id>`', { skip: !HAS_PY }, () => {
    const r = schemaValidate({ spec: '1.0.0', engines: { implement: { engine: 'claude-code' } } });
    assert.deepStrictEqual(r.errors, []);
    assert.ok(r.valid);
  });

  it('accepts the shorthand `engines.<stage>: <id>` (docs/engine_contract.md)', { skip: !HAS_PY }, () => {
    const r = schemaValidate({ spec: '1.0.0', engines: { implement: 'claude-code', 'ci-repair': 'scc' } });
    assert.deepStrictEqual(r.errors, []);
    assert.ok(r.valid);
  });

  it('rejects an unknown engine id with a clear error', { skip: !HAS_PY }, () => {
    const r = schemaValidate({ spec: '1.0.0', engines: { implement: { engine: 'not-an-engine' } } });
    assert.strictEqual(r.valid, false);
    assert.ok(r.errors.some(e => /implement/.test(e)), `error should name the stage: ${r.errors}`);
    const r2 = schemaValidate({ spec: '1.0.0', engines: { review: 'not-an-engine' } });
    assert.strictEqual(r2.valid, false);
  });

  it('rejects an unknown stage key', { skip: !HAS_PY }, () => {
    const r = schemaValidate({ spec: '1.0.0', engines: { 'not-a-stage': 'scc' } });
    assert.strictEqual(r.valid, false);
  });

  it('rejects an unknown global engine.name', { skip: !HAS_PY }, () => {
    const r = schemaValidate({ spec: '1.0.0', engine: { name: 'not-an-engine' } });
    assert.strictEqual(r.valid, false);
  });

  it('resolves the parsed structure and the engine.name → scc fallback (load-config)', { skip: !HAS_PY }, () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-resolve-'));
    const input = path.join(dir, 'in.json');
    fs.writeFileSync(input, JSON.stringify({
      spec: '1.0.0',
      engine: { name: 'scc' },
      engines: { implement: { engine: 'claude-code' }, 'ci-repair': 'devin' },
    }));
    const r = spawnSync('python3', [RESOLVER, '--input', input, '--out-dir', dir], { encoding: 'utf8' });
    assert.strictEqual(r.status, 0, `resolve.py failed: ${r.stderr}`);
    const contract = JSON.parse(fs.readFileSync(path.join(dir, 'contract.json'), 'utf8'));
    // Both forms normalize to one shape: stage → engine id.
    assert.strictEqual(contract.engines.implement, 'claude-code');
    assert.strictEqual(contract.engines['ci-repair'], 'devin');
    // Omitted stages carry no entry — dispatch falls back to engine.name
    // ('scc' here, i.e. sc-agent-cli), per the contract's default chain.
    assert.ok(!('review' in contract.engines));
    assert.strictEqual(contract.engine.name, 'scc');
    assert.strictEqual(contract._sources['engines.implement.engine'], 'contract');

    // AC: stage_engine(contract, stage) — engines.<stage> wins, then the
    // global engine.name, then the built-in sc-agent-cli default.
    const probe = spawnSync('python3', ['-c', [
      'import sys; sys.dont_write_bytecode = True',
      'import importlib.util, json',
      'spec = importlib.util.spec_from_file_location("resolve", sys.argv[1])',
      'm = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)',
      'c = json.load(open(sys.argv[2]))',
      'print(json.dumps({s: m.stage_engine(c, s) for s in ["implement", "ci-repair", "review", "sweep"]}))',
    ].join('\n'), RESOLVER, path.join(dir, 'contract.json')], { encoding: 'utf8' });
    assert.strictEqual(probe.status, 0, `stage_engine probe failed: ${probe.stderr}`);
    assert.deepStrictEqual(JSON.parse(probe.stdout), {
      implement: 'claude-code',
      'ci-repair': 'devin',
      review: 'scc',   // no stage entry → global engine.name
      sweep: 'scc',
    });
  });

  it('falls back to a non-default engine.name when the stage has no entry', { skip: !HAS_PY }, () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-resolve-'));
    const input = path.join(dir, 'in.json');
    fs.writeFileSync(input, JSON.stringify({
      spec: '1.0.0',
      engine: { name: 'devin' },
      engines: { implement: 'claude-code' },
    }));
    const r = spawnSync('python3', [RESOLVER, '--input', input, '--out-dir', dir], { encoding: 'utf8' });
    assert.strictEqual(r.status, 0, `resolve.py failed: ${r.stderr}`);
    const probe = spawnSync('python3', ['-c', [
      'import sys; sys.dont_write_bytecode = True',
      'import importlib.util, json',
      'spec = importlib.util.spec_from_file_location("resolve", sys.argv[1])',
      'm = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)',
      'c = json.load(open(sys.argv[2]))',
      'print(json.dumps({s: m.stage_engine(c, s) for s in ["implement", "review"]}))',
    ].join('\n'), RESOLVER, path.join(dir, 'contract.json')], { encoding: 'utf8' });
    assert.strictEqual(probe.status, 0, `stage_engine probe failed: ${probe.stderr}`);
    assert.deepStrictEqual(JSON.parse(probe.stdout), {
      implement: 'claude-code',
      review: 'devin',   // stage omitted → global engine.name, not the scc built-in
    });
  });
});

describe('per-stage model selection by task class (issue #171)', () => {
  // Probe helper: load resolve.py as a module and evaluate an expression
  // against a resolved contract.json — same pattern as the stage_engine
  // probes above.
  function probeResolver(expr, contractPath) {
    const probe = spawnSync('python3', ['-c', [
      'import sys; sys.dont_write_bytecode = True',
      'import importlib.util, json',
      'spec = importlib.util.spec_from_file_location("resolve", sys.argv[1])',
      'm = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)',
      'c = json.load(open(sys.argv[2]))',
      `print(json.dumps(${expr}))`,
    ].join('\n'), RESOLVER, contractPath], { encoding: 'utf8' });
    assert.strictEqual(probe.status, 0, `resolver probe failed: ${probe.stderr}`);
    return JSON.parse(probe.stdout);
  }

  function resolveContract(cfg) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-resolve-'));
    const input = path.join(dir, 'in.json');
    fs.writeFileSync(input, JSON.stringify(cfg));
    const r = spawnSync('python3', [RESOLVER, '--input', input, '--out-dir', dir], { encoding: 'utf8' });
    assert.strictEqual(r.status, 0, `resolve.py failed: ${r.stderr}`);
    return path.join(dir, 'contract.json');
  }

  it('accepts a stage definition carrying engine + model', { skip: !HAS_PY }, () => {
    const r = schemaValidate({
      spec: '1.0.0',
      engines: { implement: { engine: 'scc', model: 'openai/gpt-4o' } },
    });
    assert.deepStrictEqual(r.errors, []);
    assert.ok(r.valid);
  });

  it('accepts a model-only stage definition — model routing without an adapter pin', { skip: !HAS_PY }, () => {
    const r = schemaValidate({
      spec: '1.0.0',
      engines: { review: { model: 'openai/o3' }, 'ci-repair': { model: 'auto/best-reasoning' } },
    });
    assert.deepStrictEqual(r.errors, []);
    assert.ok(r.valid);
  });
  it('rejects an empty stage definition and unknown fields in one', { skip: !HAS_PY }, () => {
    assert.strictEqual(schemaValidate({ spec: '1.0.0', engines: { implement: {} } }).valid, false);
    assert.strictEqual(
      schemaValidate({ spec: '1.0.0', engines: { implement: { engine: 'scc', temperature: 0.5 } } }).valid,
      false);
  });

  it('rejects a non-string or empty stage model', { skip: !HAS_PY }, () => {
    assert.strictEqual(schemaValidate({ spec: '1.0.0', engines: { review: { model: '' } } }).valid, false);
    assert.strictEqual(schemaValidate({ spec: '1.0.0', engines: { review: { model: 42 } } }).valid, false);
  });

  it('still rejects an unknown engine id inside a stage definition that also names a model', { skip: !HAS_PY }, () => {
    const r = schemaValidate({
      spec: '1.0.0',
      engines: { implement: { engine: 'not-an-engine', model: 'openai/gpt-4o' } },
    });
    assert.strictEqual(r.valid, false);
  });

  it('resolves stage_model → model.primary and preserves the engine.name chain', { skip: !HAS_PY }, () => {
    const contractPath = resolveContract({
      spec: '1.0.0',
      model: { primary: 'openai/gpt-4o' },
      engines: {
        implement: { engine: 'scc', model: 'anthropic/claude-sonnet' },
        review: { model: 'openai/o3' },
        'ci-repair': 'scc',
        intake: { model: 'auto/best-fast' },   // OmniRoute route — dead on GHA
      },
    });
    const contract = JSON.parse(fs.readFileSync(contractPath, 'utf8'));
    // Entries declaring a model keep the object form; {engine}-only
    // entries flatten back to the bare id — pre-#171 resolved shape.
    assert.deepStrictEqual(contract.engines.implement, { engine: 'scc', model: 'anthropic/claude-sonnet' });
    assert.deepStrictEqual(contract.engines.review, { model: 'openai/o3' });
    assert.strictEqual(contract.engines['ci-repair'], 'scc');

    assert.deepStrictEqual(probeResolver(
      '{s: m.stage_model(c, s) for s in ["implement", "review", "ci-repair", "intake", "sweep"]}',
      contractPath), {
        implement: 'anthropic/claude-sonnet',   // declared stage model wins
        review: 'openai/o3',                    // model-only entry still routes
        'ci-repair': 'openai/gpt-4o',           // no model → model.primary
        intake: 'openai/gpt-4o',                // OmniRoute route filtered → model.primary
        sweep: 'openai/gpt-4o',                 // no entry → model.primary
      });
    // The engine chain is unaffected by a model-only entry.
    assert.deepStrictEqual(probeResolver(
      '{s: m.stage_engine(c, s) for s in ["implement", "review"]}',
      contractPath), { implement: 'scc', review: 'scc' });
  });

  it('lets the dedicated review.ai_reviewer.model outrank engines.review.model', { skip: !HAS_PY }, () => {
    const contractPath = resolveContract({
      spec: '1.0.0',
      model: { primary: 'openai/gpt-4o' },
      review: { ai_reviewer: { model: 'openai/o3-mini' } },
      engines: { review: { model: 'openai/o3' } },
    });
    assert.strictEqual(probeResolver('m.review_model(c)', contractPath), 'openai/o3-mini');

    // … and engines.review.model feeds the reviewer when the dedicated
    // field is unset — one resolution chain, documented in
    // spec/model-routing.md.
    const contractPath2 = resolveContract({
      spec: '1.0.0',
      model: { primary: 'openai/gpt-4o' },
      engines: { review: { model: 'openai/o3' } },
    });
    assert.strictEqual(probeResolver('m.review_model(c)', contractPath2), 'openai/o3');
  });

  it('emits the stage_models output with OmniRoute routes filtered', { skip: !HAS_PY }, () => {
    const contractPath = resolveContract({
      spec: '1.0.0',
      model: { primary: 'openai/gpt-4o' },
      engines: {
        implement: { model: 'anthropic/claude-sonnet' },
        intake: { model: 'auto/best-fast' },
        verify: { engine: 'scc' },
      },
    });
    assert.deepStrictEqual(probeResolver('m.stage_models(c)', contractPath), {
      implement: 'anthropic/claude-sonnet',
      // intake dropped (OmniRoute) — resolves to model_primary on GHA;
      // verify absent (no model declared).
    });
  });
});

describe('escalation.routes — failure-class alert routing (issue #175)', () => {
  it('declares routes as an open failure-class → alert_route map under escalation', () => {
    const e = schema.properties.escalation;
    assert.ok(e, 'missing properties.escalation');
    assert.ok(e.properties.routes, 'missing escalation.routes');
    assert.strictEqual(e.properties.routes.type, 'object');
    assert.deepStrictEqual(e.properties.routes.additionalProperties, { $ref: '#/$defs/alert_route' });
  });

  it('alert_route is a closed {mentions, labels, channel} shape', () => {
    const r = schema.$defs.alert_route;
    assert.ok(r, 'missing $defs.alert_route');
    assert.strictEqual(r.type, 'object');
    assert.strictEqual(r.additionalProperties, false);
    assert.deepStrictEqual(Object.keys(r.properties).sort(), ['channel', 'labels', 'mentions']);
  });

  it('accepts a contract with class routes + a default route', { skip: !HAS_PY }, () => {
    const r = schemaValidate({
      spec: '1.0.0',
      escalation: {
        label: 'needs-human',
        routes: {
          'contract-error': { mentions: ['@ops'], labels: ['priority:P0'], channel: 'issue-comment' },
          default: { mentions: ['@oncall'] },
        },
      },
    });
    assert.deepStrictEqual(r.errors, []);
    assert.ok(r.valid);
  });
  it('rejects a route with an undeclared property', { skip: !HAS_PY }, () => {
    const r = schemaValidate({
      spec: '1.0.0',
      escalation: { routes: { security: { mentions: ['@x'], webhook: 'https://evil' } } },
    });
    assert.strictEqual(r.valid, false);
    assert.ok(r.errors.some(e => /webhook/.test(e)), `error should name the stray key: ${r.errors}`);
  });

  it('resolved contract carries routes merged key-wise (load-config)', { skip: !HAS_PY }, () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-resolve-'));
    const input = path.join(dir, 'in.json');
    fs.writeFileSync(input, JSON.stringify({
      spec: '1.0.0',
      escalation: { routes: { conflict: { mentions: ['@a'] }, default: { mentions: ['@d'] } } },
    }));
    const r = spawnSync('python3', [RESOLVER, '--input', input, '--out-dir', dir], { encoding: 'utf8' });
    assert.strictEqual(r.status, 0, `resolve.py failed: ${r.stderr}`);
    const contract = JSON.parse(fs.readFileSync(path.join(dir, 'contract.json'), 'utf8'));
    assert.deepStrictEqual(contract.escalation.routes.conflict.mentions, ['@a']);
    assert.deepStrictEqual(contract.escalation.routes.default.mentions, ['@d']);
  });
});

describe('security gate contract + org floor (issues #107, #108)', () => {
  it('declares a closed security section with the floor fields', () => {
    const s = schema.properties.security;
    assert.ok(s, 'missing properties.security');
    assert.strictEqual(s.type, 'object');
    assert.strictEqual(s.additionalProperties, false);
    // Floor fields per spec/org-policy.md — Security Gate.
    assert.strictEqual(s.properties.enabled.type, 'boolean');
    assert.strictEqual(s.properties.enabled.default, true);
    assert.deepStrictEqual(s.properties.min_severity.enum, ['low', 'medium', 'high', 'critical']);
    assert.strictEqual(s.properties.min_severity.default, 'high');
    assert.strictEqual(s.properties.block_on_secret.type, 'boolean');
    assert.strictEqual(s.properties.block_on_secret.default, true);
  });

  it('declares severity_matrix as a closed pr:risk-* → outcome map', () => {
    const m = schema.properties.security.properties.severity_matrix;
    assert.ok(m, 'missing security.severity_matrix');
    assert.strictEqual(m.additionalProperties, false);
    assert.deepStrictEqual(
      Object.keys(m.properties).sort(),
      ['pr:risk-critical', 'pr:risk-high', 'pr:risk-low', 'pr:risk-medium'],
    );
    // Issue #108 AC: pr:risk-high and pr:risk-critical default to
    // blocking-when-fix-available (else report-only); low/medium stay
    // report-only.
    assert.strictEqual(m.properties['pr:risk-high'].default, 'blocking-if-fix-available');
    assert.strictEqual(m.properties['pr:risk-critical'].default, 'blocking-if-fix-available');
    assert.strictEqual(m.properties['pr:risk-low'].default, 'report-only');
    assert.strictEqual(m.properties['pr:risk-medium'].default, 'report-only');
    for (const k of Object.keys(m.properties)) {
      assert.strictEqual(m.properties[k].$ref, '#/$defs/security_gate_outcome');
    }
    const out = schema.$defs.security_gate_outcome;
    assert.ok(out, 'missing $defs.security_gate_outcome');
    assert.deepStrictEqual(
      out.enum.slice().sort(),
      ['blocking', 'blocking-if-fix-available', 'report-only'],
    );
  });

  it('accepts a security block; rejects unknown tiers, outcomes and severities', { skip: !HAS_PY }, () => {
    const ok = schemaValidate({
      spec: '1.0.0',
      security: {
        enabled: true,
        min_severity: 'high',
        block_on_secret: true,
        severity_matrix: { 'pr:risk-high': 'blocking', 'pr:risk-low': 'report-only' },
      },
    });
    assert.deepStrictEqual(ok.errors, []);
    assert.ok(ok.valid);

    assert.strictEqual(schemaValidate({
      spec: '1.0.0',
      security: { severity_matrix: { 'pr:risk-high': 'sometimes' } },
    }).valid, false);
    assert.strictEqual(schemaValidate({
      spec: '1.0.0',
      security: { severity_matrix: { 'pr:risk-accepted': 'blocking' } },
    }).valid, false);
    assert.strictEqual(schemaValidate({
      spec: '1.0.0',
      security: { min_severity: 'fatal' },
    }).valid, false);
    assert.strictEqual(schemaValidate({
      spec: '1.0.0',
      security: { scanner: 'trivy' },   // undeclared leaf — closed object
    }).valid, false);
  });

  it('org floor survives repo loosening; repo tightening still lands (load-config)', { skip: !HAS_PY }, () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-resolve-'));
    const input = path.join(dir, 'in.json');
    const orgFile = path.join(dir, 'org.json');
    // Repo tries to relax every floor the org pinned; tightening
    // pr:risk-medium must still land.
    fs.writeFileSync(input, JSON.stringify({
      spec: '1.0.0',
      verify: { required_checks: [] },
      security: {
        enabled: false,
        min_severity: 'critical',
        block_on_secret: false,
        severity_matrix: { 'pr:risk-high': 'report-only', 'pr:risk-medium': 'blocking' },
      },
    }));
    fs.writeFileSync(orgFile, JSON.stringify({
      spec: '1.0.0',
      verify: { required_checks: ['ai-sdlc-security'] },
      security: {
        enabled: true,
        min_severity: 'high',
        block_on_secret: true,
        severity_matrix: {
          'pr:risk-low': 'report-only',
          'pr:risk-medium': 'report-only',
          'pr:risk-high': 'blocking-if-fix-available',
          'pr:risk-critical': 'blocking-if-fix-available',
        },
      },
      policy_floor: {
        pin: [
          'security.enabled', 'security.min_severity', 'security.block_on_secret',
          'security.severity_matrix.pr:risk-high', 'security.severity_matrix.pr:risk-critical',
          'verify.required_checks',
        ],
      },
    }));
    const r = spawnSync('python3', [
      RESOLVER, '--input', input, '--out-dir', dir,
      '--org', orgFile, '--org-repo', 'fixtures/org', '--org-ref', 'main',
    ], { encoding: 'utf8' });
    assert.strictEqual(r.status, 0, `resolve.py failed: ${r.stderr}`);
    const contract = JSON.parse(fs.readFileSync(path.join(dir, 'contract.json'), 'utf8'));

    // Loosening is suppressed — the org floor stands everywhere.
    assert.strictEqual(contract.security.enabled, true);
    assert.strictEqual(contract.security.min_severity, 'high');
    assert.strictEqual(contract.security.block_on_secret, true);
    assert.strictEqual(contract.security.severity_matrix['pr:risk-high'], 'blocking-if-fix-available');
    assert.strictEqual(contract.security.severity_matrix['pr:risk-critical'], 'blocking-if-fix-available');
    assert.strictEqual(contract.security.severity_matrix['pr:risk-low'], 'report-only');
    // Tightening is still allowed (epic #87: repos can tighten, never loosen).
    assert.strictEqual(contract.security.severity_matrix['pr:risk-medium'], 'blocking');
    // Union keeps the org-mandated check even when the repo lists none.
    assert.ok(contract.verify.required_checks.includes('ai-sdlc-security'));
    // Audit trail records the enforcement.
    for (const p of ['security.enabled', 'security.min_severity', 'security.block_on_secret',
      'security.severity_matrix.pr:risk-high']) {
      assert.strictEqual(contract._sources[p], 'org-pin', `${p} should be org-pin`);
    }
    assert.strictEqual(contract._sources['security.severity_matrix.pr:risk-medium'], 'contract');
  });

  it('a blocking finding fails the required check (issue #108)', { skip: !HAS_PY }, () => {
    // The gate outcome is a pure function of the resolved contract: a
    // finding is blocking-class when it is a secret (block_on_secret) or
    // its severity reaches min_severity; the severity_matrix then maps the
    // PR's pr:risk-* tier to the gate outcome. Evaluate that rule against
    // the shipped org baseline to prove a blocking finding fails the
    // required ai-sdlc-security check.
    const org = JSON.parse(spawnSync('python3', [
      '-c', 'import yaml,json,sys; json.dump(yaml.safe_load(open(sys.argv[1])), sys.stdout)',
      path.join(ROOT, 'examples', 'org-baseline.ai-sdlc.yaml'),
    ], { encoding: 'utf8' }).stdout);
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-resolve-'));
    const input = path.join(dir, 'in.json');
    const orgFile = path.join(dir, 'org.json');
    fs.writeFileSync(input, JSON.stringify({ spec: '1.0.0' }));
    fs.writeFileSync(orgFile, JSON.stringify(org));
    const r = spawnSync('python3', [
      RESOLVER, '--input', input, '--out-dir', dir,
      '--org', orgFile, '--org-repo', 'fixtures/org', '--org-ref', 'main',
    ], { encoding: 'utf8' });
    assert.strictEqual(r.status, 0, `resolve.py failed: ${r.stderr}`);
    const c = JSON.parse(fs.readFileSync(path.join(dir, 'contract.json'), 'utf8'));

    const SEVERITY = ['low', 'medium', 'high', 'critical'];
    const gateFails = ({ tier, secret, severity, fixAvailable }) => {
      if (!c.security.enabled) return false;
      // Secrets block unconditionally — the severity matrix only arbitrates
      // severity-class findings (spec/org-policy.md — Security Gate).
      if (c.security.block_on_secret && secret) return true;
      if (SEVERITY.indexOf(severity) < SEVERITY.indexOf(c.security.min_severity)) return false;
      const outcome = c.security.severity_matrix[tier] || 'report-only';
      return outcome === 'blocking'
        || (outcome === 'blocking-if-fix-available' && fixAvailable);
    };

    // Blocking finding: critical CVE with an upstream fix on a risk-high PR.
    assert.ok(gateFails({ tier: 'pr:risk-high', secret: false, severity: 'critical', fixAvailable: true }));
    assert.ok(gateFails({ tier: 'pr:risk-critical', secret: false, severity: 'high', fixAvailable: true }));
    // Secret material blocks unconditionally under the org floor.
    assert.ok(gateFails({ tier: 'pr:risk-low', secret: true, severity: 'low', fixAvailable: false }));
    // Unfixable findings and low/medium tiers stay report-only.
    assert.ok(!gateFails({ tier: 'pr:risk-high', secret: false, severity: 'critical', fixAvailable: false }));
    assert.ok(!gateFails({ tier: 'pr:risk-low', secret: false, severity: 'critical', fixAvailable: true }));
    // …and the check the gate emits is a required one — a red
    // ai-sdlc-security holds the merge via the existing check-gate table.
    assert.ok(c.verify.required_checks.includes('ai-sdlc-security'));
  });

  it('org baseline example carries the gate + floor and still validates', { skip: !HAS_PY }, () => {
    const file = path.join(ROOT, 'examples', 'org-baseline.ai-sdlc.yaml');
    const org = JSON.parse(spawnSync('python3', [
      '-c', 'import yaml,json,sys; json.dump(yaml.safe_load(open(sys.argv[1])), sys.stdout)', file,
    ], { encoding: 'utf8' }).stdout);
    const v = schemaValidate(org);
    assert.deepStrictEqual(v.errors, []);
    assert.ok(v.valid);
    assert.ok(org.verify.required_checks.includes('ai-sdlc-security'),
      'org baseline must pin ai-sdlc-security as a required check');
    for (const pin of ['security.min_severity', 'security.block_on_secret']) {
      assert.ok(org.policy_floor.pin.includes(pin), `policy_floor.pin missing ${pin}`);
    }
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
