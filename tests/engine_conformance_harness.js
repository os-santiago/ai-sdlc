// tests/engine_conformance_harness.js
// Engine-adapter conformance harness (issue #217 / epic #152).
//
// Runs the adapter conformance suite against EVERY engine registered in the
// canonical registry (config/engine_adapters.json) and certifies that each
// resolved adapter satisfies the declared contract surface of
// scripts/engine_adapter.js — the shared headless contract consumed by the
// container worker and the GHA runtime alike:
//
//   resolution    hasEngine / getEngineAdapter resolve the descriptor
//   binary        getBinary() honors {ENGINE}_BINARY, else registry `binary`
//   argv          buildArgv(values) — atomic argv-element placeholder
//                 substitution, pattern validation, env fallback,
//                 determinism, required-placeholder enforcement
//   exit codes    classifyExitCode() honors the declared exitCodeMap and its
//                 mandatory "default"; mapExitCode() normalizes to 0 only for
//                 the engine's declared success class
//   capabilities  probeCapabilities() returns the registry-declared flags
//   manifest      mapManifest() is a passthrough; manifestMap/configRenderer
//                 stay declarative location/format descriptors
//   probe         declared probe argv invokes the engine binary
//                 (executed for real only under --live)
//   isolation     a returned adapter can never mutate the shared registry
//
// Usage:
//   node tests/engine_conformance_harness.js [--registry PATH]
//       [--engine ID] [--live] [--report PATH] [--help]
//   make test-conformance
//
// Exit status: 0 when every adapter passes every check, 1 on any failure
// (including an unloadable registry), 2 on CLI misuse.
//
// The harness is registry-driven: registering a new engine descriptor in
// config/engine_adapters.json automatically subjects it to the full suite —
// certification coverage grows with the registry, not with this file.

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const {
  EngineAdapterResolver,
  DEFAULT_REGISTRY_PATH,
} = require('../scripts/engine_adapter');

const PLACEHOLDER_RE = /^\{([A-Za-z][A-Za-z0-9]*)\}$/;

// ---------------------------------------------------------------------------
// Value synthesis
//
// The suite must build a valid argv without knowing a placeholder's
// semantics. Candidates are tried in order against each placeholder's own
// declared validation pattern; the first match wins. Patterns narrow enough
// to reject the whole corpus fail conformance loudly — the fix is to extend
// KNOWN_GOOD/GENERIC_CANDIDATES, never to weaken the check.
// ---------------------------------------------------------------------------

const KNOWN_GOOD = {
  prompt: 'conformance probe: implement the adapter contract (free text; punctuation allowed)',
  promptFile: 'conformance/task-prompt.md',
  auditLog: 'conformance/audit.jsonl',
  model: 'conformance-model.v1',
  manifestFile: 'conformance/run-manifest.json',
  maxSeconds: '600',
  maxSteps: '200',
};

const GENERIC_CANDIDATES = [
  'conformance-value',
  'conformance_value',
  'value',
  'a',
  'x',
  'test-1',
  'v1.2',
  'a/b/c.json',
  'plain text',
];

// Candidates guaranteed to break any pattern that restricts its alphabet at
// all — used for the negative (rejection) checks. Empty values are excluded:
// an error message always "contains" the empty string, so it cannot prove
// the no-echo guarantee.
const HOSTILE_CANDIDATES = [
  'a;rm -rf /',
  '$(id)',
  '`id`',
  'x\ny',
  'a b',
  '--flag',
  'a|b&c>d',
  'a"b',
  'a\u0000b',
  '\u0001control',
];

// Values carrying a space — an argv-element substitution must keep them in
// ONE element (no re-splitting). Applied where the declared pattern allows.
const ATOMICITY_CANDIDATES = ['two words', 'a;b c', 'x y z', 'v (1)'];

// ---------------------------------------------------------------------------
// Helpers (re-derived locally — a conformance suite must not reuse the
// implementation under test for its own expectations)
// ---------------------------------------------------------------------------

/** `{ENGINE}_{PLACEHOLDER}` env name: devin + manifestFile -> DEVIN_MANIFEST_FILE. */
function envNameFor(engineId, placeholder) {
  const envEngine = engineId.toUpperCase().replace(/[^A-Z0-9]/g, '_');
  const envPlaceholder = placeholder
    .replace(/([a-z0-9])([A-Z])/g, '$1_$2')
    .toUpperCase();
  return `${envEngine}_${envPlaceholder}`;
}

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function fail(detail) {
  const err = new Error(detail);
  err.isConformanceFailure = true;
  return err;
}

function assert(condition, detail) {
  if (!condition) throw fail(detail);
}

function deepEqual(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

function placeholderNames(descriptor) {
  return Object.keys(descriptor.argvBuilder.substitution.placeholders);
}

function isRequired(descriptor, name) {
  return descriptor.argvBuilder.substitution.placeholders[name].required !== false;
}

function patternFor(descriptor, name) {
  return new RegExp(descriptor.argvBuilder.substitution.placeholders[name].pattern);
}

/** Pick the first candidate that satisfies the pattern and (if possible) is unused. */
function pickValue(descriptor, name, extraCandidates, used) {
  const re = patternFor(descriptor, name);
  const candidates = [
    ...(KNOWN_GOOD[name] !== undefined ? [KNOWN_GOOD[name]] : []),
    ...(extraCandidates || []),
    ...GENERIC_CANDIDATES,
  ];
  const matching = candidates.filter((c) => re.test(c));
  return matching.find((c) => !used.has(c)) || matching[0] || null;
}

/**
 * Synthesize a value for every declared placeholder. Returns
 * { values, missing } — `missing` lists placeholders the corpus could not
 * satisfy (a hard failure for required placeholders).
 */
function synthesizeValues(descriptor, { exclude = [] } = {}) {
  const values = {};
  const missing = [];
  const used = new Set();
  for (const name of placeholderNames(descriptor)) {
    if (exclude.includes(name)) continue;
    const v = pickValue(descriptor, name, null, used);
    if (v === null) {
      missing.push(name);
    } else {
      values[name] = v;
      used.add(v);
    }
  }
  return { values, missing };
}

/** First HOSTILE value the pattern rejects — null if the pattern accepts all. */
function pickInvalidValue(descriptor, name) {
  const re = patternFor(descriptor, name);
  return HOSTILE_CANDIDATES.find((c) => !re.test(c)) ?? null;
}

/** First spaced value the pattern accepts — null if spaces are disallowed. */
function pickSpacedValue(descriptor, name) {
  const re = patternFor(descriptor, name);
  return ATOMICITY_CANDIDATES.find((c) => re.test(c)) ?? null;
}

/** An exit code the declared map does not classify explicitly. */
function pickUnmappedCode(descriptor) {
  for (const candidate of [4242, 1337, 77, 2, -1, 999999]) {
    if (!Object.prototype.hasOwnProperty.call(descriptor.exitCodeMap, String(candidate))) {
      return candidate;
    }
  }
  return null;
}

/**
 * Snapshot and clear every `{ENGINE}_*` env var so env-fallback state from
 * the caller's environment cannot leak into the checks. Returns a restore
 * function.
 */
function isolateEnv(engineId) {
  const prefix = `${engineId.toUpperCase().replace(/[^A-Z0-9]/g, '_')}_`;
  const saved = {};
  for (const key of Object.keys(process.env)) {
    if (key.startsWith(prefix)) {
      saved[key] = process.env[key];
      delete process.env[key];
    }
  }
  return () => {
    for (const key of Object.keys(process.env)) {
      if (key.startsWith(prefix)) delete process.env[key];
    }
    Object.assign(process.env, saved);
  };
}

// ---------------------------------------------------------------------------
// Conformance checks — each throws on failure, may return a note string or
// { skip: reason }.
// ---------------------------------------------------------------------------

const GLOBAL_CHECKS = [
  {
    id: 'registry-loads',
    description: 'registry parses and declares at least one engine',
    run(ctx) {
      assert(Array.isArray(ctx.resolver.getRegisteredEngines()),
        'resolver.getRegisteredEngines() must return an array');
      assert(ctx.resolver.getRegisteredEngines().length > 0,
        'registry declares no engines — nothing can be certified');
    },
  },
  {
    id: 'unknown-engine-rejected',
    description: 'unregistered engine ids fail loudly instead of falling back',
    run(ctx) {
      const ghost = '__conformance_unregistered__';
      assert(ctx.resolver.hasEngine(ghost) === false,
        `hasEngine("${ghost}") must be false`);
      let threw = false;
      try {
        ctx.resolver.getEngineAdapter(ghost);
      } catch (err) {
        threw = /Unknown engine ID/.test(err.message);
      }
      assert(threw, 'getEngineAdapter for an unregistered id must throw "Unknown engine ID"');
    },
  },
];

const ADAPTER_CHECKS = [
  {
    id: 'adapter-resolves',
    description: 'descriptor resolves to an adapter exposing the full contract surface',
    run(ctx) {
      const { adapter, descriptor } = ctx;
      assert(adapter.name === ctx.engineId, 'adapter.name must equal the registered engine id');
      assert(typeof adapter.binary === 'string' && adapter.binary.length > 0,
        'adapter.binary must be a non-empty string');
      assert(Array.isArray(adapter.probe) && adapter.probe.every((e) => typeof e === 'string'),
        'adapter.probe must be an array of argv strings');
      assert(isObject(adapter.capabilities), 'adapter.capabilities must be an object');
      assert(isObject(adapter.argvBuilder), 'adapter.argvBuilder must be an object');
      assert(isObject(adapter.exitCodeMap), 'adapter.exitCodeMap must be an object');
      for (const method of ['getBinary', 'buildArgv', 'probeCapabilities',
        'classifyExitCode', 'mapExitCode', 'mapManifest']) {
        assert(typeof adapter[method] === 'function',
          `adapter.${method}() must be a function`);
      }
      assert(deepEqual(adapter.exitCodeMap, descriptor.exitCodeMap),
        'adapter.exitCodeMap must reflect the registry descriptor');
    },
  },
  {
    id: 'binary-resolution',
    description: 'getBinary() returns the registry binary and honors {ENGINE}_BINARY (trimmed)',
    run(ctx) {
      const { adapter, descriptor, engineId } = ctx;
      const envKey = envNameFor(engineId, 'binary');
      delete process.env[envKey];
      assert(adapter.getBinary() === descriptor.binary,
        `getBinary() must return registry binary "${descriptor.binary}"`);
      process.env[envKey] = '  /conformance/engine-binary  ';
      try {
        assert(adapter.getBinary() === '/conformance/engine-binary',
          'getBinary() must honor the {ENGINE}_BINARY override, trimmed');
      } finally {
        delete process.env[envKey];
      }
    },
  },
  {
    id: 'argv-builds',
    description: 'buildArgv() substitutes every placeholder into a well-formed argv',
    run(ctx) {
      const { adapter, descriptor } = ctx;
      const { values, missing } = synthesizeValues(descriptor);
      const missingRequired = missing.filter((n) => isRequired(descriptor, n));
      assert(missingRequired.length === 0,
        `no conforming test value for required placeholder(s) ${missingRequired.join(', ')} — extend the harness corpus`);

      const argv = adapter.buildArgv(values);
      assert(Array.isArray(argv) && argv.every((e) => typeof e === 'string' && e.length > 0),
        'buildArgv() must return an array of non-empty strings');
      if (descriptor.argvBuilder.command !== undefined) {
        assert(argv[0] === descriptor.argvBuilder.command,
          `argv[0] must be the declared command "${descriptor.argvBuilder.command}"`);
      }
      for (const el of argv) {
        assert(!PLACEHOLDER_RE.test(el), `argv element ${JSON.stringify(el)} is an unsubstituted placeholder`);
      }
      // Every substituted value must land as exactly one whole argv element.
      for (const [name, value] of Object.entries(values)) {
        if (!(name in descriptor.argvBuilder.substitution.placeholders)) continue;
        const hits = argv.filter((el) => el === value).length;
        assert(hits === 1,
          `value for "{${name}}" must appear exactly once as a whole argv element (found ${hits})`);
      }
      ctx.validValues = values;
      ctx.baselineArgv = argv;
    },
  },
  {
    id: 'argv-atomic-elements',
    description: 'placeholder values occupy whole argv elements — spaced values never split',
    run(ctx) {
      const { adapter, descriptor } = ctx;
      const tested = [];
      for (const name of placeholderNames(descriptor)) {
        const spaced = pickSpacedValue(descriptor, name);
        if (spaced === null) continue; // pattern forbids spaces — nothing to prove
        const { values } = synthesizeValues(descriptor, { exclude: [name] });
        values[name] = spaced;
        const argv = adapter.buildArgv(values);
        const hits = argv.filter((el) => el === spaced).length;
        assert(hits === 1,
          `"{${name}}" value ${JSON.stringify(spaced)} must remain ONE argv element (found ${hits})`);
        tested.push(name);
      }
      if (tested.length === 0) {
        return { skip: 'no placeholder pattern accepts spaced values — atomicity not exercisable' };
      }
      return `exercised placeholders: ${tested.join(', ')}`;
    },
  },
  {
    id: 'argv-deterministic',
    description: 'buildArgv() is deterministic — same values produce the identical argv',
    run(ctx) {
      const { adapter } = ctx;
      assert(ctx.validValues, 'requires a successful baseline build (see argv-builds)');
      const first = adapter.buildArgv(ctx.validValues);
      const second = adapter.buildArgv(ctx.validValues);
      assert(deepEqual(first, second), 'two identical buildArgv() calls diverged');
    },
  },
  {
    id: 'argv-required-enforced',
    description: 'missing required placeholders throw and name the placeholder + env fallback',
    run(ctx) {
      const { adapter, descriptor, engineId } = ctx;
      const required = placeholderNames(descriptor).filter((n) => isRequired(descriptor, n));
      assert(required.length > 0, 'adapter declares no required placeholders — nothing enforces the contract');
      for (const name of required) {
        const { values } = synthesizeValues(descriptor, { exclude: [name] });
        let err = null;
        try {
          adapter.buildArgv(values);
        } catch (e) {
          err = e;
        }
        assert(err !== null, `omitting required "{${name}}" must throw`);
        assert(/Missing required value/.test(err.message),
          `omitting "{${name}}" must throw "Missing required value" (got: ${err.message})`);
        assert(err.message.includes(`{${name}}`),
          `error must name the missing placeholder "{${name}}"`);
        assert(err.message.includes(envNameFor(engineId, name)),
          `error must name the ${envNameFor(engineId, name)} env fallback`);
      }
      return `required placeholders: ${required.join(', ')}`;
    },
  },
  {
    id: 'argv-invalid-rejected',
    description: 'pattern-failing values throw without echoing the rejected value',
    run(ctx) {
      const { adapter, descriptor } = ctx;
      const tested = [];
      for (const name of placeholderNames(descriptor)) {
        const hostile = pickInvalidValue(descriptor, name);
        if (hostile === null) continue; // fully permissive pattern by design
        const { values } = synthesizeValues(descriptor, { exclude: [name] });
        values[name] = hostile;
        let err = null;
        try {
          adapter.buildArgv(values);
        } catch (e) {
          err = e;
        }
        assert(err !== null, `pattern-failing value for "{${name}}" must be rejected`);
        assert(/Invalid value for placeholder/.test(err.message),
          `rejection must surface "Invalid value for placeholder" (got: ${err.message})`);
        assert(!err.message.includes(hostile),
          `rejection error for "{${name}}" must not echo the rejected value — it may carry secrets`);
        tested.push(name);
      }
      if (tested.length === 0) {
        return { skip: 'all placeholder patterns accept the hostile corpus — rejection not exercisable' };
      }
      return `validated rejection for: ${tested.join(', ')}`;
    },
  },
  {
    id: 'argv-unknown-inputs-ignored',
    description: 'values for undeclared keys are ignored, never substituted',
    run(ctx) {
      const { adapter } = ctx;
      assert(ctx.validValues && ctx.baselineArgv,
        'requires a successful baseline build (see argv-builds)');
      const argv = adapter.buildArgv({ ...ctx.validValues, __conformance_unknown__: 'zzz' });
      assert(deepEqual(argv, ctx.baselineArgv),
        'undeclared value keys must not alter the built argv');
    },
  },
  {
    id: 'argv-env-fallback',
    description: '{ENGINE}_{PLACEHOLDER} env vars supply values; explicit values win',
    run(ctx) {
      const { adapter, descriptor, engineId } = ctx;
      assert(ctx.validValues, 'requires a successful baseline build (see argv-builds)');
      const required = placeholderNames(descriptor).filter((n) => isRequired(descriptor, n));
      const name = required[0];
      const re = patternFor(descriptor, name);
      const distinct = GENERIC_CANDIDATES.filter((c) => re.test(c) && c !== ctx.validValues[name]);
      assert(distinct.length > 0,
        `need two distinct valid values for "{${name}}" to prove env fallback`);

      const envKey = envNameFor(engineId, name);
      const envValue = distinct[0];
      const { values } = synthesizeValues(descriptor, { exclude: [name] });
      process.env[envKey] = envValue;
      try {
        const fromEnv = adapter.buildArgv(values);
        assert(fromEnv.includes(envValue),
          `buildArgv() must fall back to ${envKey} for "{${name}}"`);
        const explicit = adapter.buildArgv({ ...values, [name]: ctx.validValues[name] });
        assert(explicit.includes(ctx.validValues[name]) && !explicit.includes(envValue),
          'an explicit value must win over the env fallback');
      } finally {
        delete process.env[envKey];
      }
      return `env fallback verified via ${envKey}`;
    },
  },
  {
    id: 'exitcode-classification',
    description: 'classifyExitCode() honors declared codes; unmapped codes fall back to "default"',
    run(ctx) {
      const { adapter, descriptor } = ctx;
      for (const [code, cls] of Object.entries(descriptor.exitCodeMap)) {
        if (code === 'default') continue;
        assert(adapter.classifyExitCode(Number(code)) === cls,
          `exit code ${code} must classify as "${cls}"`);
        assert(adapter.classifyExitCode(code) === cls,
          `exit code "${code}" (string) must classify as "${cls}"`);
      }
      const unmapped = pickUnmappedCode(descriptor);
      assert(unmapped !== null, 'could not find an undeclared exit code to probe');
      assert(adapter.classifyExitCode(unmapped) === descriptor.exitCodeMap.default,
        `unmapped exit code ${unmapped} must classify as the declared default "${descriptor.exitCodeMap.default}"`);
    },
  },
  {
    id: 'exitcode-normalization',
    description: 'mapExitCode() is 0 only for the declared success class, 1 otherwise',
    run(ctx) {
      const { adapter, descriptor } = ctx;
      const successClass = descriptor.exitCodeMap['0'];
      const probes = [
        ...Object.keys(descriptor.exitCodeMap).filter((k) => k !== 'default').map(Number),
        pickUnmappedCode(descriptor),
      ];
      for (const code of probes) {
        const expected = adapter.classifyExitCode(code) === successClass ? 0 : 1;
        assert(adapter.mapExitCode(code) === expected,
          `mapExitCode(${code}) must be ${expected} (classifies as "${adapter.classifyExitCode(code)}")`);
      }
      assert(adapter.mapExitCode(0) === 0, 'exit code 0 must normalize to 0');
    },
  },
  {
    id: 'capabilities-contract',
    description: 'probeCapabilities() returns a fresh copy of the registry-declared capabilities',
    run(ctx) {
      const { adapter, descriptor } = ctx;
      const declared = descriptor.capabilities || {};
      assert(deepEqual(adapter.probeCapabilities(), declared),
        'probeCapabilities() must equal the registry-declared capabilities');
      const mutated = adapter.probeCapabilities();
      mutated.__conformance_mutation__ = true;
      assert(!('__conformance_mutation__' in adapter.probeCapabilities()),
        'probeCapabilities() must return a copy — mutations must not leak');
    },
  },
  {
    id: 'manifest-contract',
    description: 'mapManifest() is a passthrough; manifestMap/configRenderer stay declarative',
    run(ctx) {
      const { adapter, descriptor } = ctx;
      for (const payload of [{ v: 1, success: true }, null, 'raw', 42]) {
        assert(adapter.mapManifest(payload) === payload,
          'mapManifest() must pass the manifest through unchanged');
      }
      if (descriptor.manifestMap !== undefined) {
        assert(typeof descriptor.manifestMap.path === 'string'
          && typeof descriptor.manifestMap.format === 'string',
          'manifestMap must declare path + format (docs/engine_contract.md)');
      }
      if (descriptor.configRenderer !== undefined) {
        assert(isObject(descriptor.configRenderer),
          'configRenderer must be a declarative object');
      }
    },
  },
  {
    id: 'probe-declaration',
    description: 'a declared probe is non-empty argv invoking the engine binary',
    run(ctx) {
      const { adapter } = ctx;
      if (adapter.probe.length === 0) {
        return { skip: 'no probe declared — optional per docs/engine_contract.md' };
      }
      assert(adapter.probe.every((e) => typeof e === 'string' && e.length > 0),
        'probe elements must be non-empty strings');
      assert(adapter.probe[0] === adapter.getBinary(),
        `probe[0] must invoke the engine binary (got ${JSON.stringify(adapter.probe[0])}, binary ${JSON.stringify(adapter.getBinary())})`);
      return undefined;
    },
  },
  {
    id: 'probe-live',
    description: 'declared probe executes successfully against the installed engine (--live only)',
    live: true,
    run(ctx) {
      const { adapter } = ctx;
      if (adapter.probe.length === 0) {
        return { skip: 'no probe declared' };
      }
      const [cmd, ...args] = adapter.probe;
      const res = spawnSync(cmd, args, {
        encoding: 'utf8',
        timeout: 30000,
      });
      assert(!res.error, `probe failed to spawn: ${res.error ? res.error.message : 'unknown'}`);
      assert(res.status === 0,
        `probe ${JSON.stringify(adapter.probe)} exited ${res.status}: ${(res.stderr || '').slice(0, 300)}`);
      return undefined;
    },
  },
  {
    id: 'adapter-isolation',
    description: 'mutating a resolved adapter cannot corrupt the shared registry',
    run(ctx) {
      const { adapter, resolver, engineId } = ctx;
      adapter.argvBuilder.args.push('__conformance_corruption__');
      adapter.exitCodeMap.default = 'Success';
      adapter.binary = 'corrupted-binary';
      const fresh = resolver.getEngineAdapter(engineId);
      assert(!fresh.argvBuilder.args.includes('__conformance_corruption__'),
        'registry argvBuilder must be immune to adapter mutation');
      assert(fresh.exitCodeMap.default !== 'Success',
        'registry exitCodeMap must be immune to adapter mutation');
      assert(fresh.binary !== 'corrupted-binary',
        'registry binary must be immune to adapter mutation');
    },
  },
];

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

/**
 * Run the conformance suite.
 * @param {Object} [opts]
 * @param {string} [opts.registryPath] - registry to certify (default: canonical)
 * @param {Array<string>} [opts.engines] - restrict to these engine ids
 * @param {boolean} [opts.live] - also execute each adapter's declared probe
 * @returns {{ok: boolean, registryPath: string, global: Object[], engines: Object[], summary: Object}}
 */
function runConformance({ registryPath = DEFAULT_REGISTRY_PATH, engines = null, live = false } = {}) {
  const report = {
    ok: true,
    registryPath: path.resolve(registryPath),
    live,
    global: [],
    engines: [],
    summary: { engines: 0, checks: 0, passed: 0, failed: 0, skipped: 0 },
  };

  const record = (bucket, check, fn) => {
    report.summary.checks += 1;
    try {
      const out = fn();
      const note = out && out.skip ? 'skip' : 'pass';
      bucket.push({ id: check.id, description: check.description, status: note, detail: out && (out.skip || out) || undefined });
      report.summary[note === 'skip' ? 'skipped' : 'passed'] += 1;
    } catch (err) {
      bucket.push({ id: check.id, description: check.description, status: 'fail', detail: err.message });
      report.summary.failed += 1;
      report.ok = false;
    }
  };

  let resolver;
  try {
    resolver = new EngineAdapterResolver(registryPath);
  } catch (err) {
    report.global.push({
      id: 'registry-loads',
      description: 'registry parses and declares at least one engine',
      status: 'fail',
      detail: err.message,
    });
    report.summary.checks += 1;
    report.summary.failed += 1;
    report.ok = false;
    return report;
  }

  const globalCtx = { resolver };
  for (const check of GLOBAL_CHECKS) {
    record(report.global, check, () => check.run(globalCtx));
  }

  let engineIds = resolver.getRegisteredEngines();
  if (engines && engines.length > 0) {
    const unknown = engines.filter((id) => !resolver.hasEngine(id));
    if (unknown.length > 0) {
      report.global.push({
        id: 'engine-selection',
        description: 'every --engine selection resolves to a registered adapter',
        status: 'fail',
        detail: `unregistered engine id(s): ${unknown.join(', ')}`,
      });
      report.summary.checks += 1;
      report.summary.failed += 1;
      report.ok = false;
      return report;
    }
    engineIds = engines;
  }

  for (const engineId of engineIds) {
    const entry = { engineId, ok: true, checks: [] };
    report.engines.push(entry);
    report.summary.engines += 1;

    const restoreEnv = isolateEnv(engineId);
    try {
      const descriptor = resolver.registry[engineId];
      const adapter = resolver.getEngineAdapter(engineId);
      const ctx = { engineId, descriptor, adapter, resolver };
      for (const check of ADAPTER_CHECKS) {
        if (check.live && !live) {
          entry.checks.push({ id: check.id, description: check.description, status: 'skip', detail: 'requires --live' });
          report.summary.skipped += 1;
          report.summary.checks += 1;
          continue;
        }
        const before = report.summary.failed;
        record(entry.checks, check, () => check.run(ctx));
        if (report.summary.failed > before) entry.ok = false;
      }
    } finally {
      restoreEnv();
    }
  }

  return report;
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

const USAGE = `Usage: node tests/engine_conformance_harness.js [options]

Runs the engine-adapter conformance suite against every adapter registered
in the canonical registry (config/engine_adapters.json).

Options:
  --registry PATH   certify an alternate registry document
  --engine ID       certify only this engine (repeatable)
  --live            also execute each adapter's declared probe argv
  --report PATH     write the full JSON report to PATH
  --help            show this text

Exit status: 0 = all adapters conform, 1 = conformance failure, 2 = misuse.`;

function parseArgs(argv) {
  const opts = { registryPath: DEFAULT_REGISTRY_PATH, engines: [], live: false, reportPath: null };
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    const take = (flag) => {
      const v = argv[++i];
      if (v === undefined) throw new Error(`${flag} requires a value`);
      return v;
    };
    if (arg === '--registry') opts.registryPath = take(arg);
    else if (arg === '--engine') opts.engines.push(take(arg));
    else if (arg === '--live') opts.live = true;
    else if (arg === '--report') opts.reportPath = take(arg);
    else if (arg === '--help' || arg === '-h') return { help: true };
    else throw new Error(`unknown option: ${arg}`);
  }
  return opts;
}

function printReport(report, out = process.stdout) {
  const mark = { pass: 'PASS', fail: 'FAIL', skip: 'SKIP' };
  const line = (engineId, check) => {
    const detail = check.detail ? ` — ${check.detail}` : '';
    out.write(`[${mark[check.status]}] ${engineId}/${check.id}: ${check.description}${detail}\n`);
  };
  for (const check of report.global) line('(registry)', check);
  for (const engine of report.engines) {
    for (const check of engine.checks) line(engine.engineId, check);
  }
  const s = report.summary;
  out.write(
    `\nconformance: ${s.passed} passed, ${s.failed} failed, ${s.skipped} skipped ` +
    `across ${s.engines} engine(s) — ${report.ok ? 'CONFORMING' : 'NON-CONFORMING'}\n`
  );
}

function main(argv) {
  let opts;
  try {
    opts = parseArgs(argv);
  } catch (err) {
    process.stderr.write(`error: ${err.message}\n\n${USAGE}\n`);
    return 2;
  }
  if (opts.help) {
    process.stdout.write(`${USAGE}\n`);
    return 0;
  }

  const report = runConformance({
    registryPath: opts.registryPath,
    engines: opts.engines,
    live: opts.live,
  });

  printReport(report);

  if (opts.reportPath) {
    try {
      fs.writeFileSync(opts.reportPath, `${JSON.stringify(report, null, 2)}\n`);
      process.stdout.write(`report written to ${opts.reportPath}\n`);
    } catch (err) {
      process.stderr.write(`error: could not write report to ${opts.reportPath}: ${err.message}\n`);
      return 2;
    }
  }

  return report.ok ? 0 : 1;
}

if (require.main === module) {
  process.exit(main(process.argv.slice(2)));
}

module.exports = {
  runConformance,
  main,
  GLOBAL_CHECKS,
  ADAPTER_CHECKS,
  DEFAULT_REGISTRY_PATH,
};
