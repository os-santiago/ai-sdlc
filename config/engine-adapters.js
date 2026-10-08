// config/engine-adapters.js
// Engine adapters registry for the pluggable engine layer

module.exports = {
  scc: {
    binary: 'npx sc-agent-cli@latest',
    argvBuilder: function(engineArgs) {
      const argv = [
        '--agent-name', engineArgs.agentName || '(hostname)',
        '--max-steps', String(engineArgs.maxSteps || 200),
        '--max-seconds', String(engineArgs.maxSeconds || 900),
        '--runtime-token', engineArgs.runtimeToken,
        '--prompt-file', engineArgs.promptFile,
        '--summary-file', engineArgs.summaryFile,
        '--audit-log', engineArgs.auditLog,
        '--manifest-file', engineArgs.manifestFile,
        '--provider-base-url', engineArgs.providerBaseUrl || '',
        '--model', engineArgs.model,
        '--api-key', engineArgs.apiKey || '',
        '--notepad-file', engineArgs.notepadFile,
        '--seed', engineArgs.seed || '',
        '--log-level', engineArgs.logLevel || 'info'
      ];
      // Add any extra flags from engineArgs.extraFlags (array)
      if (Array.isArray(engineArgs.extraFlags)) {
        argv.push(...engineArgs.extraFlags);
      }
      return argv;
    },
    exitCodeMap: {
      0: 'implemented + PR opened',
      10: 'no workspace mutations (clean no-op)',
      20: 'provider/auth failure',
      21: 'provider/auth failure',
      22: 'budget exceeded',
      23: 'livelock detected',
      1: 'fatal error'
    }
  },
  devin: {
    binary: 'devin',
    argvBuilder: function(engineArgs) {
      const argv = [
        '--agent-name', engineArgs.agentName || '(hostname)',
        '--max-steps', String(engineArgs.maxSteps || 200),
        '--max-seconds', String(engineArgs.maxSeconds || 900),
        '--prompt-file', engineArgs.promptFile,
        '--summary-file', engineArgs.summaryFile,
        '--audit-log', engineArgs.auditLog,
        '--manifest-file', engineArgs.manifestFile,
        '--model', engineArgs.model,
        '--api-key', engineArgs.apiKey || '',
        '--notepad-file', engineArgs.notepadFile,
        '--seed', engineArgs.seed || '',
        '--log-level', engineArgs.logLevel || 'info'
      ];
      if (Array.isArray(engineArgs.extraFlags)) {
        argv.push(...engineArgs.extraFlags);
      }
      return argv;
    },
    exitCodeMap: {
      0: 'success',
      1: 'error',
      2: 'timeout',
      3: 'invalid input'
    }
  }
};