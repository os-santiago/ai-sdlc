const fs = require('fs');
const path = require('path');

/**
 * Engine Adapter Resolver
 * Resolves engine configuration from the registry
 */
class EngineAdapterResolver {
  /**
   * @param {string} registryPath - Path to the engine adapters registry JSON file
   */
  constructor(registryPath) {
    this.registryPath = registryPath;
    this.registry = null;
    this.loadRegistry();
  }

  /**
   * Load and parse the engine adapters registry
   * @throws {Error} If registry file is missing or invalid JSON
   */
  loadRegistry() {
    try {
      const registryContent = fs.readFileSync(this.registryPath, 'utf8');
      this.registry = JSON.parse(registryContent);
      
      // Validate registry structure
      if (!this.registry || typeof this.registry !== 'object') {
        throw new Error('Registry must be a valid JSON object');
      }
    } catch (error) {
      if (error.code === 'ENOENT') {
        throw new Error(`Engine adapters registry not found at ${this.registryPath}`);
      } else if (error instanceof SyntaxError) {
        throw new Error(`Invalid JSON in engine adapters registry: ${error.message}`);
      } else {
        throw error;
      }
    }
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