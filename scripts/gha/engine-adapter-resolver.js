const path = require('path');
const vm = require('vm');

/**
 * Engine Adapter Resolver
 * Resolves engine configuration from the registry
 */
class EngineAdapterResolver {
  /**
   * @param {string} registryPath - Path to the engine adapters registry file
   *   (a CommonJS .js module exporting the registry, or a .json document where
   *   each argvBuilder is stored as serialized function source)
   */
  constructor(registryPath) {
    this.registryPath = registryPath;
    this.registry = null;
    this.loadRegistry();
  }

  /**
   * Load the engine adapters registry. Loading goes through require() so both
   * registry forms are supported: a .js module exporting functions directly,
   * or a JSON document whose argvBuilder fields are serialized function
   * sources (reconstructed lazily in getEngineAdapter).
   * @throws {Error} If registry file is missing or cannot be loaded
   */
  loadRegistry() {
    const resolvedPath = path.resolve(this.registryPath);
    let registry;
    try {
      delete require.cache[require.resolve(resolvedPath)];
      registry = require(resolvedPath);
    } catch (error) {
      if (error.code === 'MODULE_NOT_FOUND' || error.code === 'ENOENT') {
        throw new Error(`Engine adapters registry not found at ${this.registryPath}`);
      } else if (error instanceof SyntaxError) {
        throw new Error(`Invalid engine adapters registry at ${this.registryPath}: ${error.message}`);
      } else {
        throw error;
      }
    }

    if (!registry || typeof registry !== 'object' || Array.isArray(registry)) {
      throw new Error('Engine adapters registry must be an object');
    }

    this.registry = registry;
  }

  /**
   * Reconstruct an argvBuilder function from its serialized source stored in
   * a JSON registry. The source is repo-owned configuration; it is evaluated
   * in a fresh V8 context whose global carries no ambient host capabilities
   * (no process, require, timers, fetch or other Node globals), so a tampered
   * registry cannot reach the host environment through the deserialized
   * builder. The source must evaluate to a function.
   * @param {string} source - Serialized function source
   * @param {string} engineId - Engine identifier for error context
   * @returns {Function} The reconstructed argvBuilder function
   * @throws {Error} If the source does not evaluate to a function
   */
  deserializeArgvBuilder(source, engineId) {
    let builder;
    try {
      const context = vm.createContext(Object.create(null));
      builder = vm.runInContext(`"use strict"; (${source})`, context, { timeout: 1000 });
    } catch (error) {
      throw new Error(`Invalid argvBuilder for engine ${engineId}: ${error.message}`);
    }
    if (typeof builder !== 'function') {
      throw new Error(`Invalid argvBuilder for engine ${engineId}: source did not evaluate to a function`);
    }
    return builder;
  }

  /**
   * Normalize an exitCodeMap to a Map keyed by numeric exit code. Registry
   * documents carry exit codes under string keys — JSON object keys are
   * always strings, and even the .js registry's numeric literals land as
   * strings — so lookups by numeric process exit code would miss unless the
   * keys are converted.
   * @param {Object|Map} exitCodeMap - Raw exitCodeMap (string- or number-keyed)
   * @param {string} engineId - Engine identifier for error context
   * @returns {Map<number, string>} Map of numeric exit code to meaning
   * @throws {Error} If any key is not an integer exit code
   */
  normalizeExitCodeMap(exitCodeMap, engineId) {
    const entries = exitCodeMap instanceof Map
      ? exitCodeMap.entries()
      : Object.entries(exitCodeMap);
    const normalized = new Map();
    for (const [code, meaning] of entries) {
      const numericCode = typeof code === 'number' ? code : Number(code);
      const validKey = typeof code === 'number' ||
        (typeof code === 'string' && code.trim() !== '');
      if (!validKey || !Number.isInteger(numericCode)) {
        throw new Error(`Invalid exit code "${code}" in exitCodeMap for engine ${engineId}`);
      }
      normalized.set(numericCode, meaning);
    }
    return normalized;
  }

  /**
   * Get adapter configuration for a given engine ID
   * @param {string} engineId - The engine identifier (e.g., 'scc', 'devin')
   * @returns {Object} Adapter configuration with binary, argvBuilder, and
   *   exitCodeMap (a Map keyed by numeric exit code)
   * @throws {Error} If engine ID is unknown or configuration is invalid
   */
  getEngineAdapter(engineId) {
    if (!this.registry) {
      throw new Error('Registry not loaded');
    }

    const adapter = this.registry[engineId];
    if (!adapter) {
      throw new Error(`Unknown engine ID: ${engineId}`);
    }

    // Validate required fields
    if (!adapter.binary || typeof adapter.binary !== 'string') {
      throw new Error(`Invalid or missing binary for engine ${engineId}`);
    }

    if (typeof adapter.argvBuilder === 'string') {
      adapter.argvBuilder = this.deserializeArgvBuilder(adapter.argvBuilder, engineId);
    }

    if (!adapter.argvBuilder || typeof adapter.argvBuilder !== 'function') {
      throw new Error(`Invalid or missing argvBuilder for engine ${engineId}`);
    }

    if (!adapter.exitCodeMap || typeof adapter.exitCodeMap !== 'object' || Array.isArray(adapter.exitCodeMap)) {
      throw new Error(`Invalid or missing exitCodeMap for engine ${engineId}`);
    }

    // Convert string exit-code keys to numbers so lookups by numeric exit
    // code work identically for JSON and .js registries.
    adapter.exitCodeMap = this.normalizeExitCodeMap(adapter.exitCodeMap, engineId);

    return adapter;
  }

  /**
   * Check if an engine ID exists in the registry
   * @param {string} engineId - The engine identifier to check
   * @returns {boolean} True if engine ID exists
   */
  hasEngine(engineId) {
    return this.registry && this.registry.hasOwnProperty(engineId);
  }

  /**
   * Get all registered engine IDs
   * @returns {Array<string>} List of registered engine IDs
   */
  getRegisteredEngines() {
    return this.registry ? Object.keys(this.registry) : [];
  }
}

module.exports = EngineAdapterResolver;
