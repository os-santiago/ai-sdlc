// config/engine-adapters.js
// Engine adapters registry for the pluggable engine layer

module.exports = {
  scc: {
    binary: 'npx sc-agent-cli@latest',
    argvBuilder: function(engineArgs) {
      const argv = [
        '--agent-name', engineArgs.agentName || '(hostname)',
        '--max-steps', String(engineArgs.maxSteps || 200),
        '--max-seconds', String(engineArgs.maxSeconds || 900)
      ];
      if (engineArgs.runtimeToken != null) {
        argv.push('--runtime-token', String(engineArgs.runtimeToken));
      }
      if (engineArgs.promptFile != null) {
        argv.push('--prompt-file', String(engineArgs.promptFile));
      }
      if (engineArgs.summaryFile != null) {
        argv.push('--summary-file', String(engineArgs.summaryFile));
      }
      if (engineArgs.auditLog != null) {
        argv.push('--audit-log', String(engineArgs.auditLog));
      }
      if (engineArgs.manifestFile != null) {
        argv.push('--manifest-file', String(engineArgs.manifestFile));
      }
      if (engineArgs.providerBaseUrl != null) {
        argv.push('--provider-base-url', String(engineArgs.providerBaseUrl));
      }
      if (engineArgs.model != null) {
        argv.push('--model', String(engineArgs.model));
      }
      if (engineArgs.apiKey != null) {
        argv.push('--api-key', String(engineArgs.apiKey));
      }
      if (engineArgs.notepadFile != null) {
        argv.push('--notepad-file', String(engineArgs.notepadFile));
      }
      if (engineArgs.seed != null) {
        argv.push('--seed', String(engineArgs.seed));
      }
      argv.push('--log-level', engineArgs.logLevel || 'info');
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
        '--max-seconds', String(engineArgs.maxSeconds || 900)
      ];
      if (engineArgs.promptFile != null) {
        argv.push('--prompt-file', String(engineArgs.promptFile));
      }
      if (engineArgs.summaryFile != null) {
        argv.push('--summary-file', String(engineArgs.summaryFile));
      }
      if (engineArgs.auditLog != null) {
        argv.push('--audit-log', String(engineArgs.auditLog));
      }
      if (engineArgs.manifestFile != null) {
        argv.push('--manifest-file', String(engineArgs.manifestFile));
      }
      if (engineArgs.model != null) {
        argv.push('--model', String(engineArgs.model));
      }
      if (engineArgs.apiKey != null) {
        argv.push('--api-key', String(engineArgs.apiKey));
      }
      if (engineArgs.notepadFile != null) {
        argv.push('--notepad-file', String(engineArgs.notepadFile));
      }
      if (engineArgs.seed != null) {
        argv.push('--seed', String(engineArgs.seed));
      }
      argv.push('--log-level', engineArgs.logLevel || 'info');
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
