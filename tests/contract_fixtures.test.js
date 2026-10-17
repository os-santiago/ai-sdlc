// tests/contract_fixtures.test.js — negative fixture suite for the
// contract surface (issue #128). Positive validation alone proves nothing
// about the gate: if the schema silently accepted a wrong type, unknown
// field, or out-of-range value, no test would notice until a consumer
// broke. This suite pins the REJECTION side.
//
//   tests/contracts/valid/*.yaml    — conforming contracts (acceptance side)
//   tests/contracts/invalid/*.yaml  — each file names its expected failure
//     via a header:
//       # expect: yaml    — YAML parse must fail (validator exit ≠ 0)
//       # expect: secret  — schema-valid on purpose; the validator's
//                         pre-schema secret scan is the only tripwire
//       # expect: schema  — jsonschema must reject; `# match: <regex>`
//                         pins the error a conforming validator emits
//   Inline cases below sweep every remaining schema section so each
//   top-level property carries at least one negative probe.
//
// Process-level contract (exit codes + stderr markers) is asserted
// end-to-end in tests/validate-config.test.sh — the CI gate picks both
// files up via tools/validate-repo.sh's test globs.

'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const ROOT = path.join(__dirname, '..');
const SCHEMA_PATH = path.join(ROOT, 'spec', 'ai-sdlc.schema.json');
const RESOLVER = path.join(ROOT, '.github', 'actions', 'load-config', 'resolve.py');
const FIXTURES = path.join(__dirname, 'contracts');
const SCHEMA = JSON.parse(fs.readFileSync(SCHEMA_PATH, 'utf8'));

const HAS_PY = spawnSync('python3', ['-c', 'import yaml, jsonschema'], { stdio: 'ignore' }).status === 0;

// YAML-aware variant of the schemaValidate harness in
// spec/ai-sdlc.schema.test.js: fixtures are .yaml files, so the probe
// parses YAML first and reports which stage produced the verdict —
// 'yaml' (unparseable) or 'schema' (jsonschema errors list).
const PY_PROBE = `
import json, sys, yaml, jsonschema
schema = json.load(open(sys.argv[1]))
try:
    data = yaml.safe_load(open(sys.argv[2]))
except Exception as e:
    print(json.dumps({"stage": "yaml", "errors": [str(e)]}))
    sys.exit(0)
v = jsonschema.Draft202012Validator(schema)
errs = ["%s: %s" % (list(e.absolute_path), e.message) for e in v.iter_errors(data)]
print(json.dumps({"stage": "schema", "valid": not errs, "errors": errs}))
`;

function probeFile(file) {
  const r = spawnSync('python3', ['-c', PY_PROBE, SCHEMA_PATH, file], { encoding: 'utf8' });
  assert.strictEqual(r.status, 0, `schema probe failed on ${file}: ${r.stderr}`);
  return JSON.parse(r.stdout);
}

// Inline-object harness — identical shape to spec/ai-sdlc.schema.test.js
// so verdicts here match what the contract gate emits on a runner.
const PY_VALIDATE = `
import json, sys, jsonschema
schema = json.load(open(sys.argv[1])); data = json.load(open(sys.argv[2]))
v = jsonschema.Draft202012Validator(schema)
errs = ["%s: %s" % (list(e.absolute_path), e.message) for e in v.iter_errors(data)]
print(json.dumps({"valid": not errs, "errors": errs}))
`;
function schemaValidate(contract) {
  const tmp = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-neg-')), 'contract.json');
  fs.writeFileSync(tmp, JSON.stringify(contract));
  const r = spawnSync('python3', ['-c', PY_VALIDATE, SCHEMA_PATH, tmp], { encoding: 'utf8' });
  assert.strictEqual(r.status, 0, `validator harness failed: ${r.stderr}`);
  return JSON.parse(r.stdout);
}

// Fixture self-description: every invalid fixture carries
// `# expect: <stage>`; schema-stage fixtures may pin the error with
// `# match: <regex>` (matched against each jsonschema error, which is
// "absolute_path: message" — e.g. "['spec']: 'spec' is a required
// property").
function fixtureExpect(file) {
  const head = fs.readFileSync(file, 'utf8').split('\n').slice(0, 8).join('\n');
  const expect = /# expect:[ \t]*(\S+)/.exec(head);
  assert.ok(expect, `${file}: missing '# expect: <yaml|secret|schema>' header`);
  const match = /# match:[ \t]*(.+)/.exec(head);
  return { stage: expect[1], match: match ? new RegExp(match[1].trim()) : null };
}

function fixtureFiles(sub) {
  const dir = path.join(FIXTURES, sub);
  return fs.readdirSync(dir).filter((f) => f.endsWith('.yaml')).sort()
    .map((f) => path.join(dir, f));
}

describe('tests/contracts fixtures — self-describing suite shape', () => {
  it('keeps at least one valid fixture', () => {
    assert.ok(fixtureFiles('valid').length >= 1, 'tests/contracts/valid must not be empty');
  });

  it('every invalid fixture declares a known expected-failure stage (>=5 cases)', () => {
    const files = fixtureFiles('invalid');
    assert.ok(files.length >= 5, `want >=5 invalid fixtures, found ${files.length}`);
    const stages = new Set();
    for (const f of files) {
      const { stage } = fixtureExpect(f);
      assert.ok(
        ['yaml', 'secret', 'schema'].includes(stage),
        `${f}: unknown expect stage ${stage}`,
      );
      stages.add(stage);
    }
    for (const stage of ['schema', 'secret', 'yaml']) {
      assert.ok(stages.has(stage), `invalid suite must cover a ${stage} rejection`);
    }
  });
});

describe('tests/contracts/valid — schema accepts', { skip: !HAS_PY }, () => {
  for (const f of fixtureFiles('valid')) {
    it(path.basename(f), () => {
      const r = probeFile(f);
      assert.strictEqual(r.stage, 'schema', `${f} failed YAML parsing: ${r.errors}`);
      assert.ok(r.valid, `${f} must validate — got: ${r.errors.join('; ')}`);
    });
  }
});

describe('tests/contracts/invalid — rejected for the declared reason', { skip: !HAS_PY }, () => {
  for (const f of fixtureFiles('invalid')) {
    const { stage, match } = fixtureExpect(f);
    it(`${path.basename(f)} → ${stage} rejection`, () => {
      const r = probeFile(f);
      if (stage === 'yaml') {
        assert.strictEqual(r.stage, 'yaml', `${f} must fail YAML parsing, got ${JSON.stringify(r)}`);
        return;
      }
      assert.strictEqual(r.stage, 'schema', `${f} failed YAML parsing: ${r.errors}`);
      if (stage === 'secret') {
        // The schema alone must NOT catch these — the shell validator's
        // pre-schema secret scan is the rejection (asserted e2e in
        // tests/validate-config.test.sh). Keeping the fixture schema-valid
        // proves the scan is the only tripwire.
        assert.ok(
          r.valid,
          `${f}: secret fixture must stay schema-valid — got: ${r.errors.join('; ')}`,
        );
        return;
      }
      // stage === 'schema'
      assert.strictEqual(r.valid, false, `${f} must be schema-invalid`);
      if (match) {
        assert.ok(
          r.errors.some((e) => match.test(e)),
          `${f}: no schema error matches ${match} — got: ${r.errors.join('; ')}`,
        );
      }
    });
  }
});

// Inline negative probes — at least one per top-level schema section, so
// a section weakened in a future schema edit fails here even if no
// fixture covers it. {contract} always carries spec so the failure under
// test is the named section, never the required-key tripwire.
const SPEC = '1.0.0';
const INLINE_INVALID = [
  // spec — required, pattern, type are fixture-covered; suffix check is inline.
  ['spec: prerelease suffix rejected', { spec: '1.0.0-rc.1' }, /does not match/],
  // repo
  ['repo.archetype: closed enum', { spec: SPEC, repo: { archetype: 'banana' } }, /not one of/],
  ['repo.primary_language: wrong type', { spec: SPEC, repo: { primary_language: 42 } }, /not of type/],
  // autonomy
  ['autonomy.merge_requires_checks: wrong type', { spec: SPEC, autonomy: { merge_requires_checks: 'maybe' } }, /not of type/],
  // promotion — beyond the fixture, the evidence-signal contract itself.
  ['promotion: track-record alone never suffices', { spec: SPEC, promotion: { clean_merges_threshold: 3, window: 30, required_signals: ['track-record'] } }, /too short/],
  ['promotion.required_signals: track-record is mandatory', { spec: SPEC, promotion: { clean_merges_threshold: 3, window: 30, required_signals: ['audit-trail', 'static-config'] } }, /does not contain/],
  ['promotion.overrides: unknown rung rejected', { spec: SPEC, promotion: { clean_merges_threshold: 3, window: 30, overrides: { 'auto-yolo': { clean_merges_threshold: 5 } } } }, /Additional properties/],
  // demotion
  ['demotion: window below minimum', { spec: SPEC, demotion: { escalation_threshold: 1, window: 0 } }, /less than the minimum/],
  ['demotion: missing required escalation_threshold', { spec: SPEC, demotion: { window: 30 } }, /required/],
  // dependencies
  ['dependencies.auto_kinds: majors never auto', { spec: SPEC, dependencies: { enabled: true, auto_kinds: ['major'] } }, /not one of/],
  ['dependencies.cadence: closed enum', { spec: SPEC, dependencies: { cadence: 'minutely' } }, /not one of/],
  ['dependencies.max_updates: bounds', { spec: SPEC, dependencies: { max_updates: 0 } }, /less than the minimum/],
  // security
  ['security.min_severity: closed enum', { spec: SPEC, security: { min_severity: 'ultra' } }, /not one of/],
  ['security.block_on_secret: wrong type', { spec: SPEC, security: { block_on_secret: 'no' } }, /not of type/],
  ['security.severity_matrix: unknown tier key', { spec: SPEC, security: { severity_matrix: { 'pr:risk-extreme': 'blocking' } } }, /Additional properties/],
  // triggers
  ['triggers.min_body_length: below DoR floor', { spec: SPEC, triggers: { min_body_length: 0 } }, /less than the minimum/],
  ['triggers.acceptance_criteria.max: below minimum', { spec: SPEC, triggers: { acceptance_criteria: { max: 0 } } }, /less than the minimum/],
  // prioritize
  ['prioritize.weights.priority: weights are numeric', { spec: SPEC, prioritize: { enabled: true, weights: { priority: { P0: 'lots' } } } }, /not of type/],
  ['prioritize.max: below minimum', { spec: SPEC, prioritize: { max: 0 } }, /less than the minimum/],
  // verify
  ['verify.timeout_seconds: below bounded floor', { spec: SPEC, verify: { timeout_seconds: 30 } }, /less than the minimum/],
  ['verify.commands: must be an array', { spec: SPEC, verify: { commands: 'npm test' } }, /not of type/],
  // review
  ['review.mode: closed enum', { spec: SPEC, review: { mode: 'sentient' } }, /not one of/],
  ['review.max_fix_iterations: above bound', { spec: SPEC, review: { max_fix_iterations: 11 } }, /greater than the maximum/],
  ['review.adversarial.min_tier: closed enum', { spec: SPEC, review: { adversarial: { min_tier: 'extreme' } } }, /not one of/],
  // merge
  ['merge.method: closed enum', { spec: SPEC, merge: { method: 'spicy' } }, /not one of/],
  ['merge.pending_max_minutes: below minimum', { spec: SPEC, merge: { pending_max_minutes: 1 } }, /less than the minimum/],
  ['merge.main_guard.max_age_hours: below minimum', { spec: SPEC, merge: { main_guard: { max_age_hours: 0 } } }, /less than the minimum/],
  // postmortem
  ['postmortem.reopen_min: below minimum', { spec: SPEC, postmortem: { reopen_min: 0 } }, /less than the minimum/],
  // release
  ['release.provenance: closed enum', { spec: SPEC, release: { provenance: 'sigstore' } }, /not one of/],
  ['release.signing: closed enum', { spec: SPEC, release: { signing: 'gpg' } }, /not one of/],
  // labels
  ['labels: flow-state values are strings', { spec: SPEC, labels: { queued: 42 } }, /not of type/],
  ['labels.state_machine.veto_pairs: loser is required', { spec: SPEC, labels: { state_machine: { veto_pairs: [{ winner: 'a' }] } } }, /required/],
  // model
  ['model.fallback: string|null only', { spec: SPEC, model: { fallback: 42 } }, /not of type/],
  ['model.routing_hints: values are strings', { spec: SPEC, model: { routing_hints: { fast: 7 } } }, /not of type/],
  // engine
  ['engine.max_steps: below bounded floor', { spec: SPEC, engine: { max_steps: 5 } }, /less than the minimum/],
  ['engine.flags: must be an array', { spec: SPEC, engine: { flags: '--verbose' } }, /not of type/],
  // engines — beyond fixtures: stage value wrong type, model wrong type.
  ['engines stage: bare non-id rejected', { spec: SPEC, engines: { implement: 42 } }, /oneOf|not valid/],
  ['engines stage model: wrong type', { spec: SPEC, engines: { review: { model: 7 } } }, /oneOf|not valid/],
  // escalation
  ['escalation.contacts: must be an array', { spec: SPEC, escalation: { contacts: '@me' } }, /not of type/],
  ['alert_route.mentions: must be an array', { spec: SPEC, escalation: { routes: { default: { mentions: '@me' } } } }, /not of type/],
  ['alert_route: undeclared field rejected', { spec: SPEC, escalation: { routes: { default: { webhook: 'x' } } } }, /Additional properties/],
  // policy_floor — wrong pin type is fixture-covered; item types inline.
  ['policy_floor.exempt: items are strings', { spec: SPEC, policy_floor: { exempt: [42] } }, /not of type/],
];

describe('inline negative cases — every schema section carries one', { skip: !HAS_PY }, () => {
  it('sweeps every top-level schema property', () => {
    const covered = new Set(Object.keys(SCHEMA.properties));
    const mentioned = new Set();
    for (const f of fixtureFiles('invalid')) mentioned.add(path.basename(f, '.yaml').split('-')[0]);
    for (const [name] of INLINE_INVALID) mentioned.add(name.split(/[:.]/, 1)[0]);
    // 'malformed' is a parse-stage fixture — reports no section. 'missing'/
    // 'spec-*'/'nested'/'unknown' fixtures cover spec + root sections.
    const uncovered = [...covered].filter((s) => !mentioned.has(s) && s !== 'spec');
    assert.deepStrictEqual(uncovered, [],
      `schema sections with no negative probe: ${uncovered.join(', ')}`);
  });

  for (const [name, contract, match] of INLINE_INVALID) {
    it(name, () => {
      const r = schemaValidate(contract);
      assert.strictEqual(r.valid, false, `${name} must be schema-invalid`);
      assert.ok(
        r.errors.some((e) => match.test(e)),
        `${name}: no error matches ${match} — got: ${r.errors.join('; ')}`,
      );
    });
  }
});

// --- org-floor enforcement (load-config resolve.py) ----------------------
// Schema rejection is half the org-floor contract; the other half is the
// resolver refusing to honor a repo value that relaxes an org-declared
// floor. These drive the real resolver CLI the load-config action uses.

function runResolver({ repo, org, overrides, sourceRepo }) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-floor-'));
  const input = path.join(dir, 'repo.json');
  fs.writeFileSync(input, JSON.stringify(repo));
  const args = [RESOLVER, '--input', input, '--out-dir', dir];
  if (org !== undefined) {
    const orgFile = path.join(dir, 'org.json');
    fs.writeFileSync(orgFile, JSON.stringify(org));
    args.push('--org', orgFile, '--org-repo', 'org/baseline');
  }
  if (overrides !== undefined) args.push('--overrides', JSON.stringify(overrides));
  if (sourceRepo !== undefined) args.push('--source-repo', sourceRepo);
  const r = spawnSync('python3', args, { encoding: 'utf8' });
  const contractPath = path.join(dir, 'contract.json');
  const contract = fs.existsSync(contractPath)
    ? JSON.parse(fs.readFileSync(contractPath, 'utf8'))
    : null;
  return { status: r.status, stdout: r.stdout, stderr: r.stderr, contract };
}

describe('org-floor enforcement — repo cannot relax an org baseline', { skip: !HAS_PY }, () => {
  it('pinned autonomy.level: repo relaxation resolves to the org ceiling', () => {
    const { status, stderr, contract } = runResolver({
      repo: { spec: SPEC, autonomy: { level: 'auto-deploy' } },
      org: { spec: SPEC, autonomy: { level: 'suggest' }, policy_floor: { pin: ['autonomy.level'] } },
    });
    assert.strictEqual(status, 0, stderr);
    assert.strictEqual(contract.autonomy.level, 'suggest',
      'a pinned org ceiling must stand over a more permissive repo value');
    assert.strictEqual(contract._sources['autonomy.level'], 'org-pin');
    assert.ok(contract._source.enforced.includes('autonomy.level'),
      'suppressed relaxation must be recorded in the enforced audit trail');
  });

  it('security floors cannot be disabled, de-fanged, or softened', () => {
    const { status, stderr, contract } = runResolver({
      repo: {
        spec: SPEC,
        security: {
          enabled: false,
          block_on_secret: false,
          min_severity: 'critical',
          severity_matrix: { 'pr:risk-low': 'report-only' },
        },
      },
      org: {
        spec: SPEC,
        security: {
          enabled: true,
          block_on_secret: true,
          min_severity: 'medium',
          severity_matrix: { 'pr:risk-low': 'blocking' },
        },
        policy_floor: {
          pin: [
            'security.enabled',
            'security.block_on_secret',
            'security.min_severity',
            'security.severity_matrix.pr:risk-low',
          ],
        },
      },
    });
    assert.strictEqual(status, 0, stderr);
    assert.strictEqual(contract.security.enabled, true, 'an org-enabled gate cannot be switched off');
    assert.strictEqual(contract.security.block_on_secret, true, 'block_on_secret cannot be de-fanged');
    assert.strictEqual(contract.security.min_severity, 'medium', 'floor clamps to the stricter severity');
    assert.strictEqual(contract.security.severity_matrix['pr:risk-low'], 'blocking',
      'a repo may harden a tier, never soften it');
    for (const p of ['security.enabled', 'security.block_on_secret', 'security.min_severity',
      'security.severity_matrix.pr:risk-low']) {
      assert.strictEqual(contract._sources[p], 'org-pin', `${p} must resolve org-pin`);
    }
  });

  it('tightening below the floor stays a contract value — only relaxation is suppressed', () => {
    const { status, stderr, contract } = runResolver({
      repo: { spec: SPEC, autonomy: { level: 'shadow' }, security: { min_severity: 'low' } },
      org: { spec: SPEC, autonomy: { level: 'suggest' }, security: { min_severity: 'medium' },
        policy_floor: { pin: ['autonomy.level', 'security.min_severity'] } },
    });
    assert.strictEqual(status, 0, stderr);
    assert.strictEqual(contract.autonomy.level, 'shadow');
    assert.strictEqual(contract._sources['autonomy.level'], 'contract',
      'a stricter repo level is honored, not suppressed');
    assert.strictEqual(contract.security.min_severity, 'low');
    assert.strictEqual(contract._sources['security.min_severity'], 'contract');
    assert.deepStrictEqual(contract._source.enforced, [],
      'nothing was suppressed — enforced must be empty');
  });

  it('a repo-level policy_floor is ignored — repos cannot pin floors', () => {
    const { status, stderr, stdout, contract } = runResolver({
      repo: {
        spec: SPEC,
        policy_floor: { pin: ['merge.method'], exempt: ['*'] },
        merge: { method: 'merge' },
      },
    });
    assert.strictEqual(status, 0, stderr);
    assert.match(stdout, /repo-policy_floor-ignored/, 'resolver must flag the ignored section');
    assert.ok(!('policy_floor' in contract), 'policy_floor must never merge into the contract');
    assert.strictEqual(contract.merge.method, 'merge', 'the repo keeps its own (unpinned) value');
    assert.ok(!Object.values(contract._sources).includes('org-pin'));
  });

  it('policy_floor.exempt skips the baseline for a listed repo', () => {
    const { status, stderr, contract } = runResolver({
      repo: { spec: SPEC, autonomy: { level: 'auto-deploy' } },
      org: { spec: SPEC, autonomy: { level: 'suggest' }, policy_floor: {
        pin: ['autonomy.level'], exempt: ['owner/free-repo'],
      } },
      sourceRepo: 'owner/free-repo',
    });
    assert.strictEqual(status, 0, stderr);
    assert.strictEqual(contract.autonomy.level, 'auto-deploy',
      'an exempt repo keeps its own autonomy declaration');
    assert.strictEqual(contract._source.org_policy, 'exempt');
  });
});

describe('resolver hard rejections (load-config resolve.py)', { skip: !HAS_PY }, () => {
  const REJECTIONS = [
    {
      name: 'autonomy.level is not a workflow-overridable path',
      repo: { spec: SPEC },
      overrides: { 'autonomy.level': 'auto-deploy' },
      match: /unknown override path/,
    },
    {
      name: 'enum override outside the closed set',
      repo: { spec: SPEC },
      overrides: { 'merge.method': 'spicy' },
      match: /not in/,
    },
    {
      name: 'engine budget override below the schema floor',
      repo: { spec: SPEC },
      overrides: { 'engine.max_seconds': '30' },
      match: /budgets below schema minimum/,
    },
  ];
  for (const { name, repo, overrides, match } of REJECTIONS) {
    it(name, () => {
      const r = runResolver({ repo, overrides });
      assert.strictEqual(r.status, 1, `expected exit 1, got ${r.status}: ${r.stdout}`);
      assert.match(r.stderr, match);
    });
  }

  it('non-mapping contract root is rejected', () => {
    const r = runResolver({ repo: ['not', 'a', 'mapping'] });
    assert.strictEqual(r.status, 1);
    assert.match(r.stderr, /root must be a mapping/);
  });

  it('a resolved contract with no spec version is rejected', () => {
    const r = runResolver({ repo: { autonomy: { level: 'suggest' } } });
    assert.strictEqual(r.status, 1);
    assert.match(r.stderr, /no spec version/);
  });
});
