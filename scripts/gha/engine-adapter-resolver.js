const path = require('path');

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
   * a JSON registry. The source is repo-owned configuration evaluated under
   * strict mode; it must evaluate to a function.
   * @param {string} source - Serialized function source
   * @param {string} engineId - Engine identifier for error context
   * @returns {Function} The reconstructed argvBuilder function
   * @throws {Error} If the source does not evaluate to a function
   */
  deserializeArgvBuilder(source, engineId) {
    let builder;
    try {
      builder = new Function(`"use strict"; return (${source});`)();
    } catch (error) {
      throw new Error(`Invalid argvBuilder for engine ${engineId}: ${error.message}`);
    }
    if (typeof builder !== 'function') {
      throw new Error(`Invalid argvBuilder for engine ${engineId}: source did not evaluate to a function`);
    }
    return builder;
  }

  /**
   * Get adapter configuration for a given engine ID
   * @param {string} engineId - The engine identifier (e.g., 'scc', 'devin')
   * @returns {Object} Adapter configuration with binary, argvBuilder, and exitCodeMap
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

    if (!adapter.exitCodeMap || typeof adapter.exitCodeMap !== 'object') {
      throw new Error(`Invalid or missing exitCodeMap for engine ${engineId}`);
    }

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
