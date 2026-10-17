#!/usr/bin/env node
// scripts/gha/stage-engine-resolver.js
// Per-stage engine dispatch for the GHA-native runtime (issue #206, epic #152).
//
// Each runtime stage reads the resolved contract (load-config's
// contract.json), selects the engine for the stage —
// `engines.<stage>` (shorthand id or a `{engine: <id>}` stage definition,
// already flattened to a scalar by load-config resolution; both shapes are
// accepted here) — and falls back to the global `engine.name`, which itself
// defaults to `scc` (spec/ai-sdlc.schema.json). The selected id is then
// resolved against the canonical adapter registry
// (config/engine_adapters.json via scripts/engine_adapter.js):
//
//   - stage selects a registered id   → that adapter drives dispatch;
//   - stage omits `engine`            → the global engine adapter;
//   - id unknown to the registry      → exit 1 naming the engine. A
//     certified-but-unregistered contract id (spec $defs.engine_id admits
//     e.g. `codex` before a registry entry exists) can never silently run
//     the wrong engine.
//
// Library surface (also what the tests exercise):
//   resolveStageEngineId(contract, stage)            -> engine id string
//   resolveStageAdapter(contract, stage, resolver?)  -> {stage, engine, source, adapter}
//   dispatchStageEngine(contract, stage, opts)       -> dispatch record
//   StageEngineError
//
// CLI:
//   node stage-engine-resolver.js --stage implement \
//     [--contract <contract.json>] [--registry <engine_adapters.json>] \
//     [--required true|false] [--json] \
//     [--run] [--model M] [--prompt-file F] [--manifest F] [--set name=value]...
//
// Without --run this is resolution + registration gate only: prints
// `stage=… engine=… source=… binary=…` and appends `engine`,
// `engine_source`, `engine_binary` to $GITHUB_OUTPUT. With --run it
// invokes the engine — `adapter.getBinary()` + `adapter.buildArgv(values)`
// spawned without a shell — and exits with the adapter-mapped code
// (`adapter.mapExitCode`), also appending `exit_code`, `engine_exit_code`,
// `engine_classification` to $GITHUB_OUTPUT. An adapter declaring
// `dispatch.rawExitCodes` (scc — issue #228) instead surfaces the engine's
// raw exit code: the caller acts on the engine's own taxonomy (scc's
// 10/20/21/22/23 contract drives the one-shot model fallback and the
// outcome labels), while `engine_classification` still carries the mapped
// class. `--set name=@path` reads the value from a file;
// `--model`/`--prompt-file`/`--manifest` are shortcuts for the conventional
// `model` / `prompt` / `manifestFile` placeholders.

'use strict';

const fs = require('fs');
const { spawnSync } = require('child_process');
const { EngineAdapterResolver, DEFAULT_REGISTRY_PATH } = require('../engine_adapter');

// Stage keys declared by spec/ai-sdlc.schema.json `engines` — informational
// only: resolution is permissive (an unlisted stage simply falls back to
// the global engine, matching the contract's fallback semantics).
const KNOWN_STAGES = ['intake', 'implement', 'review', 'automerge', 'verify', 'ci-repair', 'sweep'];

/** Error type for stage-engine resolution/dispatch failures. */
class StageEngineError extends Error {
  constructor(message) {
    super(message);
    this.name = 'StageEngineError';
  }
}

/** Global default engine id: contract engine.name, else `scc`. */
function globalEngineId(contract) {
  const name = contract && contract.engine && contract.engine.name;
  return typeof name === 'string' && name.trim() !== '' ? name.trim() : 'scc';
}

/**
 * Raw `engines.<stage>` entry, normalizing both schema shapes (shorthand id
 * and stage-definition object) — load-config already flattens to the scalar
 * form, but a caller may hand us an unflattened contract.
 */
function stageEntry(contract, stage) {
  const engines = contract && contract.engines;
  if (!engines || typeof engines !== 'object' || Array.isArray(engines)) return undefined;
  let entry = engines[stage];
  if (entry && typeof entry === 'object' && !Array.isArray(entry)) entry = entry.engine;
  return entry;
}

/**
 * Resolved engine id for a pipeline stage: `engines.<stage>` wins, then the
 * global `engine.name`, then the schema default `scc`.
 * @param {Object} contract - Resolved contract (load-config contract.json shape)
 * @param {string} stage - Stage key (implement, review, ci-repair, …)
 * @returns {string}
 * @throws {StageEngineError} on a non-object contract or empty stage
 */
function resolveStageEngineId(contract, stage) {
  if (!contract || typeof contract !== 'object' || Array.isArray(contract)) {
    throw new StageEngineError('resolved contract must be an object');
  }
  if (typeof stage !== 'string' || stage.trim() === '') {
    throw new StageEngineError('stage must be a non-empty string');
  }
  const entry = stageEntry(contract, stage.trim());
  if (entry != null && String(entry).trim() !== '') return String(entry).trim();
  return globalEngineId(contract);
}

/** Where the resolved id came from — for output provenance. */
function engineSource(contract, stage) {
  const entry = stageEntry(contract, stage);
  if (entry != null && String(entry).trim() !== '') return `engines.${stage}`;
  return contract && contract.engine && typeof contract.engine.name === 'string'
    && contract.engine.name.trim() !== '' ? 'engine.name' : 'default';
}

/**
 * Resolve the stage's engine id AND its registered adapter.
 * @param {Object} contract - Resolved contract
 * @param {string} stage - Stage key
 * @param {EngineAdapterResolver|string} [resolverOrPath] - Resolver instance
 *   (tests inject one on a fixture registry) or a registry path; default is
 *   the canonical config/engine_adapters.json.
 * @returns {{stage: string, engine: string, source: string, adapter: Object}}
 * @throws {StageEngineError} when the selected engine has no registered
 *   adapter — the message names the stage, the engine, and the registry.
 */
function resolveStageAdapter(contract, stage, resolverOrPath) {
  const engine = resolveStageEngineId(contract, stage);
  const resolver = resolverOrPath instanceof EngineAdapterResolver
    ? resolverOrPath
    : new EngineAdapterResolver(resolverOrPath || DEFAULT_REGISTRY_PATH);
  if (!resolver.hasEngine(engine)) {
    const known = resolver.getRegisteredEngines().join(', ') || '(none)';
    throw new StageEngineError(
      `stage "${stage}" selects engine "${engine}" (${engineSource(contract, stage)}) but no ` +
      `adapter is registered for it in ${resolver.registryPath} (registered: ${known})`
    );
  }
  return {
    stage,
    engine,
    source: engineSource(contract, stage),
    adapter: resolver.getEngineAdapter(engine),
  };
}

/**
 * Invoke the stage's engine adapter: build argv via the registry
 * substitution contract (validated placeholders, atomic argv elements, no
 * shell), spawn the adapter binary, and map the engine's raw exit code
 * through the adapter's declared exitCodeMap.
 *
 * @param {Object} contract - Resolved contract
 * @param {string} stage - Stage key
 * @param {Object} [opts]
 * @param {Object} [opts.values] - Placeholder values for buildArgv
 * @param {EngineAdapterResolver|string} [opts.resolver] - Resolver or registry path
 * @param {Function} [opts.spawnImpl] - (command, argv, spawnOpts) => result;
 *   defaults to child_process.spawnSync (tests inject a recorder)
 * @param {string} [opts.cwd]
 * @param {Object} [opts.env]
 * @param {*} [opts.stdio] - default 'inherit'
 * @returns {{stage, engine, source, command, argv, engineExitCode, classification, exitCode, spawnError?}}
 */
function dispatchStageEngine(contract, stage, opts = {}) {
  const resolved = resolveStageAdapter(contract, stage, opts.resolver);
  const { adapter, engine } = resolved;
  let argv;
  try {
    argv = adapter.buildArgv(opts.values || {});
  } catch (error) {
    throw new StageEngineError(`stage "${stage}" engine "${engine}": ${error.message}`);
  }
  const command = adapter.getBinary();
  const spawnImpl = opts.spawnImpl || spawnSync;
  const spawnOpts = { stdio: opts.stdio || 'inherit' };
  if (opts.cwd) spawnOpts.cwd = opts.cwd;
  if (opts.env) spawnOpts.env = opts.env;
  // Adapters declaring dispatch.rawExitCodes surface the engine's raw exit
  // code (the caller interprets the engine's own taxonomy — scc's 10/20/21
  // provider/auth contract drives the one-shot model fallback); everyone
  // else gets the exitCodeMap-normalized 0/1.
  const rawCodes = adapter.dispatch && adapter.dispatch.rawExitCodes === true;
  const result = spawnImpl(command, argv, spawnOpts);

  if (result && result.error) {
    // ENOENT and friends — the adapter is registered but the binary is not
    // on PATH. Classify like a missing engine (127), never as success.
    return {
      stage, engine, source: resolved.source, command, argv,
      engineExitCode: null,
      spawnError: result.error.message,
      classification: adapter.classifyExitCode(127),
      exitCode: rawCodes ? 127 : adapter.mapExitCode(127),
    };
  }
  const raw = result && result.status != null ? result.status : 1;
  return {
    stage, engine, source: resolved.source, command, argv,
    engineExitCode: raw,
    classification: adapter.classifyExitCode(raw),
    exitCode: rawCodes ? raw : adapter.mapExitCode(raw),
  };
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

function usage() {
  return [
    'usage: stage-engine-resolver.js --stage <name> [options]',
    '',
    '  --stage <name>       pipeline stage key (intake|implement|review|automerge|verify|ci-repair|sweep)',
    '  --contract <path>    resolved contract.json from load-config',
    '                       (default: $RUNNER_TEMP/ai-sdlc-contract/contract.json)',
    '  --registry <path>    engine adapter registry (default: config/engine_adapters.json)',
    "  --required <bool>    missing contract file is fatal when 'true' (default: true);",
    "                       'false' resolves the global default engine instead",
    '  --json               print the resolution record as JSON',
    '  --run                invoke the resolved adapter (spawn binary + argv, mapped exit code)',
    '  --model <id>         value for the {model} placeholder',
    '  --prompt-file <path> file contents as the {prompt} placeholder value',
    '  --manifest <path>    value for the {manifestFile} placeholder',
    '  --set <name=value>   extra placeholder value; name=@path reads a file',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = { set: {}, stage: null, contract: null, registry: null, required: true, json: false, run: false };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const next = () => {
      if (i + 1 >= argv.length) throw new StageEngineError(`missing value for ${a}`);
      return argv[++i];
    };
    switch (a) {
      case '--stage': opts.stage = next(); break;
      case '--contract': opts.contract = next(); break;
      case '--registry': opts.registry = next(); break;
      case '--required': {
        const v = String(next()).trim().toLowerCase();
        if (v !== 'true' && v !== 'false') throw new StageEngineError(`--required expects true|false, got ${v}`);
        opts.required = v === 'true';
        break;
      }
      case '--json': opts.json = true; break;
      case '--run': opts.run = true; break;
      case '--model': opts.set.model = next(); break;
      case '--prompt-file': opts.promptFile = next(); break;
      case '--manifest': opts.set.manifestFile = next(); break;
      case '--set': {
        const kv = next();
        const eq = kv.indexOf('=');
        if (eq < 1) throw new StageEngineError(`--set expects name=value, got ${JSON.stringify(kv)}`);
        opts.set[kv.slice(0, eq)] = kv.slice(eq + 1);
        break;
      }
      case '-h': case '--help': opts.help = true; break;
      default: throw new StageEngineError(`unknown argument ${a}\n${usage()}`);
    }
  }
  return opts;
}

/** Append a key=value step output (values here are single-line ids/paths). */
function emitOutput(key, value) {
  const out = process.env.GITHUB_OUTPUT;
  if (!out) return;
  fs.appendFileSync(out, `${key}=${value}\n`);
}

function annotateError(message) {
  const m = String(message).replace(/%/g, '%25').replace(/\r/g, '').replace(/\n/g, '%0A');
  console.log(`::error title=ai-sdlc engine dispatch::${m}`);
  console.error(`stage-engine-resolver: ${message}`);
}

function readContract(contractPath, required) {
  if (!fs.existsSync(contractPath)) {
    if (required) {
      throw new StageEngineError(`resolved contract not found: ${contractPath} — run the load-config action first`);
    }
    console.log(`::warning title=ai-sdlc engine dispatch::contract not found: ${contractPath} — falling back to the global default engine (required=false)`);
    return {};
  }
  let parsed;
  try {
    parsed = JSON.parse(fs.readFileSync(contractPath, 'utf8'));
  } catch (error) {
    throw new StageEngineError(`resolved contract is not valid JSON: ${contractPath} — ${error.message}`);
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new StageEngineError(`resolved contract must be a JSON object: ${contractPath}`);
  }
  return parsed;
}

/** --set name=@path values and --prompt-file read their payload from disk. */
function buildValues(opts) {
  const values = { ...opts.set };
  if (opts.promptFile) {
    values.prompt = fs.readFileSync(opts.promptFile, 'utf8');
  }
  for (const [name, v] of Object.entries(values)) {
    if (typeof v === 'string' && v.startsWith('@')) {
      values[name] = fs.readFileSync(v.slice(1), 'utf8');
    }
  }
  return values;
}

function main(argv) {
  let opts;
  try {
    opts = parseArgs(argv);
  } catch (error) {
    annotateError(error.message);
    return 1;
  }
  if (opts.help) {
    console.log(usage());
    return 0;
  }
  if (!opts.stage) {
    annotateError(`--stage is required\n${usage()}`);
    return 1;
  }
  const contractPath = opts.contract
    || `${process.env.RUNNER_TEMP || '/tmp'}/ai-sdlc-contract/contract.json`;
  const registryPath = opts.registry || DEFAULT_REGISTRY_PATH;

  let contract;
  try {
    contract = readContract(contractPath, opts.required);
  } catch (error) {
    annotateError(error.message);
    return 1;
  }

  if (opts.run) {
    let record;
    try {
      record = dispatchStageEngine(contract, opts.stage, {
        values: buildValues(opts),
        resolver: registryPath,
      });
    } catch (error) {
      annotateError(error.message);
      return 1;
    }
    if (record.spawnError) {
      annotateError(
        `stage "${record.stage}" engine "${record.engine}": failed to spawn ` +
        `'${record.command}' — ${record.spawnError}`
      );
    } else {
      console.log(
        `stage-engine dispatch: stage=${record.stage} engine=${record.engine} ` +
        `binary=${record.command} engine_exit=${record.engineExitCode} ` +
        `classification=${record.classification} exit_code=${record.exitCode}`
      );
    }
    emitOutput('engine', record.engine);
    emitOutput('exit_code', String(record.exitCode));
    emitOutput('engine_exit_code', record.engineExitCode == null ? '' : String(record.engineExitCode));
    emitOutput('engine_classification', record.classification);
    return record.exitCode;
  }

  let resolved;
  try {
    resolved = resolveStageAdapter(contract, opts.stage, registryPath);
  } catch (error) {
    annotateError(error.message);
    return 1;
  }
  const binary = resolved.adapter.getBinary();
  if (opts.json) {
    console.log(JSON.stringify({
      stage: resolved.stage,
      engine: resolved.engine,
      source: resolved.source,
      binary,
      capabilities: resolved.adapter.probeCapabilities(),
    }, null, 2));
  } else {
    console.log(
      `stage-engine: stage=${resolved.stage} engine=${resolved.engine} ` +
      `source=${resolved.source} binary=${binary}`
    );
  }
  emitOutput('engine', resolved.engine);
  emitOutput('engine_source', resolved.source);
  emitOutput('engine_binary', binary);
  return 0;
}

if (require.main === module) {
  process.exit(main(process.argv.slice(2)));
}

module.exports = {
  KNOWN_STAGES,
  StageEngineError,
  resolveStageEngineId,
  resolveStageAdapter,
  dispatchStageEngine,
  main,
};
