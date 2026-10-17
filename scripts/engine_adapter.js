// scripts/engine_adapter.js
// Canonical engine-adapter layer: single registry, single contract.
//
// The registry is `config/engine_adapters.json` — the declarative adapter
// contract (no code evaluation surface). Both runtime consumers resolve
// engines through this module:
//   - the container worker (scripts/autonomous_pr_worker.js) via the
//     module-level helpers bound to the canonical registry path;
//   - the GHA-native path via the EngineAdapterResolver class (also
//     re-exported from scripts/gha/engine-adapter-resolver.js for the
//     historical import location).
//
// Adapter contract (per-engine object returned by getEngineAdapter):
//   name                  engine id as registered
//   binary                engine executable (registry `binary`)
//   probe                 argv that verifies engine availability
//   capabilities          declared capability flags
//   argvBuilder           raw declarative argv spec (command + args + substitution)
//   configRenderer        rendered-config descriptor (engine's config surface)
//   exitCodeMap           engine exit code -> pipeline classification (+ "default")
//   manifestMap           run-manifest descriptor (path + format)
//   dispatch              dispatch-time options (rawExitCodes: return the
//                         engine's raw exit code instead of the mapped 0/1 —
//                         for engines whose exit taxonomy the pipeline acts
//                         on directly, like scc's 10/20/21/22/23 contract)
//   envPassthrough        env var names the engine honors (inherited env —
//                         declared so no engine knob stays hidden config)
//   getBinary()           {ENGINE}_BINARY env override (trimmed) ?? binary
//   buildArgv(values)     validated placeholder substitution -> [command, ...args]
//   probeCapabilities()   -> capabilities
//   classifyExitCode(c)   -> exitCodeMap[String(c)] ?? exitCodeMap.default
//   mapExitCode(c)        -> 0 iff c classifies as the engine's success class
//   mapManifest(m)        -> m (manifestMap is declarative location/format metadata)
//
// Value sources for buildArgv: an explicit `values` object; per-placeholder
// fallback to the `{ENGINE}_{PLACEHOLDER}` environment variable
// (camelCase placeholders map to SNAKE — e.g. manifestFile -> DEVIN_MANIFEST_FILE,
// SCC_MANIFEST_FILE). Substitution is atomic per argv element, never via a
// shell; values that fail a placeholder's pattern are rejected, never coerced.

'use strict';

const fs = require('fs');
const path = require('path');

const DEFAULT_REGISTRY_PATH = path.join(__dirname, '..', 'config', 'engine_adapters.json');
const PLACEHOLDER_RE = /^\{([A-Za-z][A-Za-z0-9]*)\}$/;
const EXIT_CODE_KEY_RE = /^\d+$/;

/** `{ENGINE}_{PLACEHOLDER}` env name: devin + manifestFile -> DEVIN_MANIFEST_FILE. */
function envNameFor(engineName, placeholder) {
  const envEngine = engineName.toUpperCase().replace(/[^A-Z0-9]/g, '_');
  const envPlaceholder = placeholder
    .replace(/([a-z0-9])([A-Z])/g, '$1_$2')
    .toUpperCase();
  return `${envEngine}_${envPlaceholder}`;
}

/**
 * EngineAdapterResolver — loads the declarative engine-adapter registry and
 * resolves per-engine adapter objects implementing the shared contract.
 */
class EngineAdapterResolver {
  /**
   * @param {string} [registryPath] - Path to the engine adapters registry
   *   (JSON document conforming to the declarative contract). Defaults to the
   *   canonical `config/engine_adapters.json`.
   */
  constructor(registryPath = DEFAULT_REGISTRY_PATH) {
    this.registryPath = registryPath;
    this.registry = null;
    this.loadRegistry();
  }

  /**
   * Load and validate the registry. The document is pure data — it is parsed
   * as JSON, never require()'d or evaluated.
   * @throws {Error} If the file is missing, unparsable, or not an object
   */
  loadRegistry() {
    const resolvedPath = path.resolve(this.registryPath);
    let raw;
    try {
      raw = fs.readFileSync(resolvedPath, 'utf8');
    } catch {
      throw new Error(`Engine adapters registry not found at ${this.registryPath}`);
    }

    let registry;
    try {
      registry = JSON.parse(raw);
    } catch (error) {
      throw new Error(`Invalid engine adapters registry at ${this.registryPath}: ${error.message}`);
    }

    if (!registry || typeof registry !== 'object' || Array.isArray(registry)) {
      throw new Error('Engine adapters registry must be an object');
    }

    for (const engineId of Object.keys(registry)) {
      this.validateDescriptor(engineId, registry[engineId]);
    }
    this.registry = registry;
  }

  /**
   * Validate a single registry descriptor against the declarative contract.
   * Mirrors the static registry test so a malformed descriptor fails at load
   * time instead of mid-dispatch.
   * @param {string} engineId
   * @param {Object} d - Raw descriptor
   */
  validateDescriptor(engineId, d) {
    const fail = (msg) => { throw new Error(`Invalid engine adapter "${engineId}": ${msg}`); };
    if (!d || typeof d !== 'object' || Array.isArray(d)) fail('descriptor must be an object');
    if (typeof d.binary !== 'string' || d.binary.length === 0) fail('missing binary');

    if (d.probe !== undefined) {
      if (!Array.isArray(d.probe) || d.probe.length === 0
          || d.probe.some((e) => typeof e !== 'string' || e.length === 0)) {
        fail('probe must be a non-empty array of argv strings');
      }
    }
    if (d.capabilities !== undefined
        && (typeof d.capabilities !== 'object' || d.capabilities === null || Array.isArray(d.capabilities))) {
      fail('capabilities must be an object');
    }

    // argvBuilder: declarative placeholder contract.
    const ab = d.argvBuilder;
    if (!ab || typeof ab !== 'object' || Array.isArray(ab)) fail('missing argvBuilder');
    if (!Array.isArray(ab.args) || ab.args.length === 0
        || ab.args.some((a) => typeof a !== 'string')) {
      fail('argvBuilder.args must be a non-empty array of strings');
    }
    if (ab.command !== undefined
        && (typeof ab.command !== 'string' || ab.command.length === 0 || /[{}]/.test(ab.command))) {
      fail('argvBuilder.command must be a non-empty literal string');
    }
    const sub = ab.substitution;
    if (!sub || typeof sub !== 'object') fail('argvBuilder.substitution must be an object');
    if (sub.mode !== 'argv-element') fail('substitution.mode must be "argv-element"');
    if (sub.shell !== 'never') fail('substitution.shell must be "never"');
    if (!sub.placeholders || typeof sub.placeholders !== 'object') {
      fail('substitution.placeholders must be an object');
    }
    const declared = Object.keys(sub.placeholders);
    const used = ab.args.map((a) => a.match(PLACEHOLDER_RE)).filter(Boolean).map((m) => m[1]);
    for (const name of declared) {
      const spec = sub.placeholders[name];
      if (!spec || typeof spec.pattern !== 'string') {
        fail(`placeholder "{${name}}" must declare a validation pattern`);
      }
      try {
        new RegExp(spec.pattern);
      } catch {
        fail(`placeholder "{${name}}" pattern does not compile`);
      }
      if (spec.required !== undefined && typeof spec.required !== 'boolean') {
        fail(`placeholder "{${name}}" required flag must be boolean`);
      }
      if (!used.includes(name)) fail(`declared placeholder "{${name}}" never used in args`);
    }
    for (const arg of ab.args) {
      if (arg.includes('{') || arg.includes('}')) {
        const m = arg.match(PLACEHOLDER_RE);
        if (!m) fail(`arg "${arg}" embeds a placeholder substring — placeholders occupy whole elements`);
        if (!sub.placeholders[m[1]]) fail(`arg "${arg}" uses undeclared placeholder "{${m[1]}}"`);
      }
    }

    // exitCodeMap: code -> classification, with a mandatory default so unknown
    // codes never resolve to undefined.
    const ecm = d.exitCodeMap;
    if (!ecm || typeof ecm !== 'object' || Array.isArray(ecm)) fail('missing exitCodeMap');
    if (!('0' in ecm)) fail('exitCodeMap must classify exit code 0');
    if (!('default' in ecm) || typeof ecm.default !== 'string') {
      fail('exitCodeMap must declare a string "default" classification');
    }
    for (const [code, cls] of Object.entries(ecm)) {
      if (code !== 'default' && !EXIT_CODE_KEY_RE.test(code)) {
        fail(`exitCodeMap key "${code}" is not a decimal exit code`);
      }
      if (typeof cls !== 'string') fail(`exitCodeMap["${code}"] must be a classification string`);
    }

    for (const key of ['configRenderer', 'manifestMap']) {
      if (d[key] !== undefined
          && (typeof d[key] !== 'object' || d[key] === null || Array.isArray(d[key]))) {
        fail(`${key} must be an object`);
      }
    }

    // dispatch: optional dispatch-time options. rawExitCodes tells the
    // dispatch path to surface the engine's raw exit code (the caller
    // interprets the engine's own taxonomy) rather than the mapped 0/1.
    if (d.dispatch !== undefined) {
      if (typeof d.dispatch !== 'object' || d.dispatch === null || Array.isArray(d.dispatch)) {
        fail('dispatch must be an object');
      }
      if (d.dispatch.rawExitCodes !== undefined && typeof d.dispatch.rawExitCodes !== 'boolean') {
        fail('dispatch.rawExitCodes must be a boolean');
      }
    }
    // envPassthrough: optional list of env var names the engine consumes —
    // dispatch inherits the caller's environment; the declaration keeps
    // engine-specific knobs (e.g. scc's SC_* timeouts/failover) visible in
    // the registry instead of hidden config.
    if (d.envPassthrough !== undefined) {
      if (!Array.isArray(d.envPassthrough)
          || d.envPassthrough.some((e) => typeof e !== 'string' || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(e))) {
        fail('envPassthrough must be an array of env var names');
      }
    }
  }

  /**
   * Resolve a value for one placeholder: explicit values map wins, then the
   * {ENGINE}_{PLACEHOLDER} environment fallback.
   */
  resolveValue(engineName, name, values) {
    let value;
    if (values && Object.prototype.hasOwnProperty.call(values, name) && values[name] != null) {
      value = values[name];
    } else {
      value = process.env[envNameFor(engineName, name)];
    }
    return value == null ? undefined : String(value);
  }

  /**
   * Build the engine argv from the descriptor's declarative contract.
   * Each {name} placeholder occupies a whole argv element and is substituted
   * atomically after passing its validation pattern — no re-splitting, no
   * globbing, no shell joining. Missing required placeholders and
   * pattern-failing values are rejected, never substituted.
   * @returns {string[]} argv (command element first when declared)
   */
  buildArgv(engineId, descriptor, values) {
    const sub = descriptor.argvBuilder.substitution;
    const out = [];
    let lastWasLiteralFlag = false;

    for (const arg of descriptor.argvBuilder.args) {
      const m = arg.match(PLACEHOLDER_RE);
      if (!m) {
        out.push(arg);
        lastWasLiteralFlag = arg.startsWith('-');
        continue;
      }
      const name = m[1];
      const spec = sub.placeholders[name]; // guaranteed declared by validateDescriptor
      const required = spec.required !== false;
      const value = this.resolveValue(engineId, name, values);
      if (value === undefined) {
        if (required) {
          throw new Error(
            `Missing required value for placeholder "{${name}}" of engine "${engineId}"` +
            ` — pass it to buildArgv() or set ${envNameFor(engineId, name)}`
          );
        }
        // Optional and unset: drop the placeholder element together with its
        // flag element when the flag emitted immediately before it.
        if (lastWasLiteralFlag) out.pop();
        lastWasLiteralFlag = false;
        continue;
      }
      // Never echo the candidate value into the error — it may carry secrets
      // or prompt payload.
      if (!new RegExp(spec.pattern).test(value)) {
        throw new Error(`Invalid value for placeholder "{${name}}" of engine "${engineId}": fails contract pattern`);
      }
      out.push(value);
      lastWasLiteralFlag = false;
    }

    const argv = descriptor.argvBuilder.command
      ? [descriptor.argvBuilder.command, ...out]
      : out;
    return argv;
  }

  /**
   * Get the adapter object for a registered engine.
   * @param {string} engineId - The engine identifier (e.g., 'scc', 'devin')
   * @returns {Object} Adapter implementing the shared engine contract
   * @throws {Error} If engine ID is unknown
   */
  getEngineAdapter(engineId) {
    if (!this.registry) {
      throw new Error('Registry not loaded');
    }
    const descriptor = this.registry[engineId];
    if (!descriptor) {
      throw new Error(`Unknown engine ID: ${engineId}`);
    }

    // The adapter carries its own copy of the descriptor so a consumer can
    // never mutate the shared registry through the returned object.
    const d = structuredClone(descriptor);
    const resolver = this;

    return {
      name: engineId,
      binary: d.binary,
      probe: d.probe || [],
      capabilities: d.capabilities || {},
      argvBuilder: d.argvBuilder,
      configRenderer: d.configRenderer,
      exitCodeMap: d.exitCodeMap,
      manifestMap: d.manifestMap,
      dispatch: d.dispatch || {},
      envPassthrough: d.envPassthrough || [],

      getBinary() {
        // {ENGINE}_BINARY override; trim — stray whitespace makes spawn fail ENOENT.
        const override = (process.env[envNameFor(engineId, 'binary')] || '').trim();
        return override || d.binary;
      },

      buildArgv(values) {
        return resolver.buildArgv(engineId, d, values);
      },

      probeCapabilities() {
        return { ...(d.capabilities || {}) };
      },

      /** Engine exit code -> pipeline classification (registry-declared). */
      classifyExitCode(code) {
        const key = String(code);
        return Object.prototype.hasOwnProperty.call(d.exitCodeMap, key)
          ? d.exitCodeMap[key]
          : d.exitCodeMap.default;
      },

      /**
       * Normalize an engine exit code to the pipeline's process exit code:
       * 0 iff the engine's own map classifies it like its success class
       * (the classification declared for exit code 0), 1 otherwise.
       */
      mapExitCode(code) {
        return this.classifyExitCode(code) === d.exitCodeMap['0'] ? 0 : 1;
      },

      /**
       * manifestMap is a declarative descriptor (where/how the run manifest
       * materializes), not a transform — the payload passes through unchanged.
       */
      mapManifest(manifest) {
        return manifest;
      },
    };
  }

  /**
   * Check if an engine ID exists in the registry.
   * @param {string} engineId
   * @returns {boolean}
   */
  hasEngine(engineId) {
    return this.registry != null
      && Object.prototype.hasOwnProperty.call(this.registry, engineId);
  }

  /**
   * Get all registered engine IDs.
   * @returns {Array<string>}
   */
  getRegisteredEngines() {
    return this.registry ? Object.keys(this.registry) : [];
  }
}

// --- Module-level facade over the canonical registry -------------------------
// The container worker resolves adapters through these helpers; the GHA path
// instantiates EngineAdapterResolver directly (same class, same registry).
let sharedResolver = null;
function resolver() {
  if (sharedResolver === null) {
    sharedResolver = new EngineAdapterResolver();
  }
  return sharedResolver;
}

/**
 * Get the adapter for an engine from the canonical registry.
 * Unknown names must fail loudly — silently falling back would run an
 * unintended engine contract.
 * @param {string} engineName
 * @returns {Object} Adapter implementing the shared engine contract
 */
function getEngineAdapter(engineName) {
  if (!resolver().hasEngine(engineName)) {
    throw new Error(`Unknown engine adapter: ${engineName}`);
  }
  return resolver().getEngineAdapter(engineName);
}

function hasEngine(engineName) {
  return resolver().hasEngine(engineName);
}

function getRegisteredEngines() {
  return resolver().getRegisteredEngines();
}

module.exports = {
  EngineAdapterResolver,
  getEngineAdapter,
  hasEngine,
  getRegisteredEngines,
  DEFAULT_REGISTRY_PATH,
};
