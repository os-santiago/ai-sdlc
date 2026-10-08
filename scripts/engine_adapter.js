// scripts/engine_adapter.js
// Engine adapter resolver and adapters for different engines.

const devinAdapter = {
  getBinary() {
    // For devin, the binary is likely 'devin' or from DEVIN_BINARY.
    // Trim env-provided values: stray whitespace would make spawn fail ENOENT.
    const binary = (process.env.DEVIN_BINARY || '').trim();
    return binary || 'devin';
  },
  buildArgv() {
    const argv = [];
    // Read DEVIN_* environment variables as per original script
    if (process.env.DEVIN_PROJECT_ID) {
      argv.push('-p', process.env.DEVIN_PROJECT_ID);
    }
    if (process.env.DEVIN_MODEL) {
      argv.push('--model', process.env.DEVIN_MODEL);
    }
    if (process.env.DEVIN_RESPECT_WORKSPACE_TRUST === 'true') {
      argv.push('--respect-workspace-trust');
    }
    // Additional flags from other DEVIN_* variables can be added here
    // For example, DEVIN_API_KEY, etc. but the original script might not have used them in argv.
    return argv;
  },
  probeCapabilities() {
    // Return capabilities for devin engine
    return {
      supportsWorkspaceTrust: true,
      modelConfigurable: true,
      projectIdRequired: true,
    };
  },
  mapExitCode(code) {
    // Map devin exit codes to standard codes
    // Assuming devin uses 0 for success, non-zero for failure
    return code === 0 ? 0 : 1;
  },
  mapManifest(manifest) {
    // Map devin manifest to a common format if needed
    return manifest;
  },
};

const sccAdapter = {
  getBinary() {
    // For scc, the binary is likely 'scc' or from SCC_BINARY.
    // Trim env-provided values: stray whitespace would make spawn fail ENOENT.
    const binary = (process.env.SCC_BINARY || '').trim();
    return binary || 'scc';
  },
  buildArgv() {
    const argv = [];
    // SCC specific arguments from environment
    if (process.env.SCC_PROJECT_ID) {
      argv.push('--project-id', process.env.SCC_PROJECT_ID);
    }
    if (process.env.SCC_MODEL) {
      argv.push('--model', process.env.SCC_MODEL);
    }
    // Add other SCC specific flags as needed
    return argv;
  },
  probeCapabilities() {
    return {
      supportsWorkspaceTrust: false, // SCC might not have this concept
      modelConfigurable: true,
      projectIdRequired: true,
    };
  },
  mapExitCode(code) {
    // Map scc exit codes
    return code === 0 ? 0 : 1;
  },
  mapManifest(manifest) {
    return manifest;
  },
};

// Resolver function to get the adapter by engine name
function getEngineAdapter(engineName) {
  switch (engineName) {
    case 'scc':
      return sccAdapter;
    case 'devin':
    default:
      return devinAdapter;
  }
}

module.exports = {
  getEngineAdapter,
  devinAdapter,
  sccAdapter,
};