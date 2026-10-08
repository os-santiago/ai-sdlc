#!/usr/bin/env node
/**
 * Autonomous PR Worker - Refactored to use engine adapter resolver
 * 
 * This script runs the pipeline using the configured engine adapter.
 * It eliminates direct DEVIN_* environment reads and devin-specific argv construction.
 */

const { spawn } = require('child_process');
const { getEngineAdapter } = require('./engine_adapter');

// Get the engine name from environment, default to 'devin' for backward compatibility
const engineName = process.env.HERMES_AGENT_ENGINE || 'devin';
const engineAdapter = getEngineAdapter(engineName);

// Call all required functions from the engine adapter (as per acceptance criteria)
const binary = engineAdapter.getBinary();
const argv = engineAdapter.buildArgv();
const capabilities = engineAdapter.probeCapabilities();
// mapManifest is part of the adapter contract; the worker exercises it even
// though this worker has no manifest payload of its own to translate.
engineAdapter.mapManifest({});

// Build the command and arguments
const additionalArgs = [];
// For example, if there are standard arguments like --timeout, --workspace, etc.
// These would be added based on the original script's behavior

// Construct the full command
const command = [binary, ...argv, ...additionalArgs].filter(Boolean);

console.error(`[autonomous_pr_worker] Engine: ${engineName}`);
console.error(`[autonomous_pr_worker] Binary: ${binary}`);
console.error(`[autonomous_pr_worker] Arguments: ${argv.join(' ')}`);
console.error(`[autonomous_pr_worker] Capabilities: ${JSON.stringify(capabilities)}`);

// Run the engine binary, streaming its stdio through to the caller, and
// propagate the adapter-mapped exit code back to the pipeline.
const child = spawn(command[0], command.slice(1), { stdio: 'inherit' });

child.on('error', (err) => {
  // e.g. ENOENT when the engine binary is not installed or not on PATH
  console.error(`[autonomous_pr_worker] failed to spawn '${command[0]}': ${err.message}`);
  process.exit(engineAdapter.mapExitCode(127));
});

child.on('exit', (code, signal) => {
  if (signal) {
    console.error(`[autonomous_pr_worker] '${command[0]}' terminated by signal ${signal}`);
  }
  // code is null when the child was killed by a signal — treat as failure
  process.exit(engineAdapter.mapExitCode(code === null ? 1 : code));
});