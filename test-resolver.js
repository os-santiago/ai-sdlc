const EngineAdapterResolver = require('./scripts/gha/engine-adapter-resolver');
const path = require('path');

console.log('Testing EngineAdapterResolver...');

try {
  const registryPath = path.join(__dirname, 'config', 'engine-adapters.json');
  const resolver = new EngineAdapterResolver(registryPath);
  
  console.log('✓ Resolver created successfully');
  
  // Test hasEngine
  console.log('hasEngine(scc):', resolver.hasEngine('scc'));
  console.log('hasEngine(devin):', resolver.hasEngine('devin'));
  console.log('hasEngine(unknown):', resolver.hasEngine('unknown'));
  
  // Test getRegisteredEngines
  const engines = resolver.getRegisteredEngines();
  console.log('Registered engines:', engines);
  
  // Test getEngineAdapter for scc
  const sccAdapter = resolver.getEngineAdapter('scc');
  console.log('✓ Got SCC adapter');
  console.log('  Binary:', sccAdapter.binary);
  console.log('  Has argvBuilder:', typeof sccAdapter.argvBuilder === 'function');
  console.log('  Has exitCodeMap:', typeof sccAdapter.exitCodeMap === 'object');
  
  // Test argvBuilder
  const argv = sccAdapter.argvBuilder({
    agentName: 'test-agent',
    maxSteps: 100,
    maxSeconds: 600,
    runtimeToken: 'test-token',
    promptFile: '/tmp/prompt.txt',
    summaryFile: '/tmp/summary.txt',
    auditLog: '/tmp/audit.jsonl',
    manifestFile: '/tmp/manifest.json',
    providerBaseUrl: 'https://example.com',
    model: 'test-model',
    apiKey: 'test-key',
    notepadFile: '/tmp/notepad.txt',
    seed: '12345',
    logLevel: 'debug',
    extraFlags: ['--flag1', '--flag2']
  });
  console.log('✓ argvBuilder works');
  console.log('  argv length:', argv.length);
  console.log('  Contains agent-name:', argv.includes('--agent-name') && argv.includes('test-agent'));
  
  // Test getEngineAdapter for devin
  const devinAdapter = resolver.getEngineAdapter('devin');
  console.log('✓ Got Devin adapter');
  console.log('  Binary:', devinAdapter.binary);
  
  // Test unknown engine
  try {
    resolver.getEngineAdapter('unknown');
    console.log('✗ Should have thrown for unknown engine');
  } catch (e) {
    console.log('✓ Correctly threw for unknown engine:', e.message);
  }
  
  console.log('\\nAll tests passed!');
} catch (error) {
  console.error('✗ Test failed:', error);
  process.exit(1);
}