const EngineAdapterResolver = require('./engine-adapter-resolver');
const path = require('path');
const assert = require('assert');

describe = function(description, fn) {
  try {
    fn();
    console.log(`✓ ${description}`);
  } catch (err) {
    console.error(`✗ ${description}`);
    console.error(err);
    process.exit(1);
  }
};

it = function(description, fn) {
  try {
    fn();
    console.log(`  ✓ ${description}`);
  } catch (err) {
    console.error(`  ✗ ${description}`);
    console.error(err);
    process.exit(1);
  }
};

const registryPath = path.join(__dirname, '..', '..', 'config', 'engine-adapters.json');
const registryJsPath = path.join(__dirname, '..', '..', 'config', 'engine-adapters.js');
let resolver;

beforeEach = function() {
  resolver = new EngineAdapterResolver(registryPath);
};

describe('EngineAdapterResolver', () => {
  beforeEach(() => {
    resolver = new EngineAdapterResolver(registryPath);
  });

  describe('constructor', () => {
    it('should load the registry successfully', () => {
      assert.ok(resolver.registry);
      assert.ok(resolver.registry.scc);
      assert.ok(resolver.registry.devin);
    });

    it('should also load the .js module registry variant', () => {
      const jsResolver = new EngineAdapterResolver(registryJsPath);
      assert.ok(jsResolver.registry.scc);
      assert.ok(jsResolver.registry.devin);
      const adapter = jsResolver.getEngineAdapter('scc');
      assert.strictEqual(typeof adapter.argvBuilder, 'function');
    });
  });

  describe('getEngineAdapter', () => {
    it('should throw contract error for unknown engine id', () => {
      try {
        resolver.getEngineAdapter('unknown-engine');
        assert.fail('Expected to throw');
      } catch (err) {
        assert.ok(err.message.includes('Unknown engine ID: unknown-engine'));
      }
    });

    it('should return correct adapter for scc', () => {
      const adapter = resolver.getEngineAdapter('scc');
      
      assert.strictEqual(adapter.binary, 'npx sc-agent-cli@latest');
      assert.strictEqual(typeof adapter.argvBuilder, 'function');
      assert.strictEqual(typeof adapter.exitCodeMap, 'object');
      
      // Test argvBuilder function
      const engineArgs = {
        agentName: 'test-agent',
        maxSteps: 100,
        maxSeconds: 600,
        runtimeToken: '[REDACTED]',
        promptFile: '/tmp/prompt.txt',
        summaryFile: '/tmp/summary.txt',
        auditLog: '/tmp/audit.jsonl',
        manifestFile: '/tmp/manifest.json',
        providerBaseUrl: 'https://example.com',
        model: 'test-model',
        apiKey: '[REDACTED]',
        notepadFile: '/tmp/notepad.txt',
        seed: '12345',
        logLevel: 'debug',
        extraFlags: ['--flag1', '--flag2']
      };
      
      const argv = adapter.argvBuilder(engineArgs);
      assert.ok(argv.includes('--agent-name'));
      assert.ok(argv.includes('test-agent'));
      assert.ok(argv.includes('--max-steps'));
      assert.ok(argv.includes('100'));
      assert.ok(argv.includes('--max-seconds'));
      assert.ok(argv.includes('600'));
      assert.ok(argv.includes('--runtime-token'));
      assert.ok(argv.includes('[REDACTED]'));
      assert.ok(argv.includes('--flag1'));
      assert.ok(argv.includes('--flag2'));
    });

    it('should return correct adapter for devin', () => {
      const adapter = resolver.getEngineAdapter('devin');
      
      assert.strictEqual(adapter.binary, 'devin');
      assert.strictEqual(typeof adapter.argvBuilder, 'function');
      assert.strictEqual(typeof adapter.exitCodeMap, 'object');
      
      // Test argvBuilder function
      const engineArgs = {
        agentName: 'test-agent',
        maxSteps: 150,
        maxSeconds: 700,
        promptFile: '/tmp/prompt.txt',
        summaryFile: '/tmp/summary.txt',
        auditLog: '/tmp/audit.jsonl',
        manifestFile: '/tmp/manifest.json',
        model: 'test-model',
        apiKey: '[REDACTED]',
        notepadFile: '/tmp/notepad.txt',
        seed: '67890',
        logLevel: 'warn',
        extraFlags: ['--verbose']
      };
      
      const argv = adapter.argvBuilder(engineArgs);
      assert.ok(argv.includes('--agent-name'));
      assert.ok(argv.includes('test-agent'));
      assert.ok(argv.includes('--max-steps'));
      assert.ok(argv.includes('150'));
      assert.ok(argv.includes('--max-seconds'));
      assert.ok(argv.includes('700'));
      assert.ok(argv.includes('--verbose'));
    });

    it('should handle missing optional fields in argvBuilder', () => {
      const adapter = resolver.getEngineAdapter('scc');
      
      const engineArgs = {}; // Empty args
      
      const argv = adapter.argvBuilder(engineArgs);
      assert.ok(argv.includes('--agent-name'));
      assert.ok(argv.includes('(hostname)')); // Default value
      assert.ok(argv.includes('--max-steps'));
      assert.ok(argv.includes('200')); // Default value
      assert.ok(argv.includes('--max-seconds'));
      assert.ok(argv.includes('900')); // Default value
      // Missing optional args must not emit flags or 'undefined' values
      assert.ok(!argv.includes('--runtime-token'));
      assert.ok(!argv.includes('--prompt-file'));
      assert.ok(!argv.includes('--seed'));
      assert.ok(!argv.includes(undefined));
      assert.ok(!argv.includes('undefined'));
    });
  });

  describe('hasEngine', () => {
    it('should return true for registered engines', () => {
      assert.strictEqual(resolver.hasEngine('scc'), true);
      assert.strictEqual(resolver.hasEngine('devin'), true);
    });

    it('should return false for unregistered engines', () => {
      assert.strictEqual(resolver.hasEngine('unknown'), false);
    });
  });

  describe('getRegisteredEngines', () => {
    it('should return list of registered engine IDs', () => {
      const engines = resolver.getRegisteredEngines();
      assert.ok(engines.includes('scc'));
      assert.ok(engines.includes('devin'));
      assert.strictEqual(engines.length, 2);
    });
  });

  describe('error handling', () => {
    it('should throw error for missing registry file', () => {
      assert.throws(
        () => new EngineAdapterResolver('/non/existent/path.json'),
        /Engine adapters registry not found/
      );
    });

    it('should throw error for invalid registry file', () => {
      // Create a temporary invalid JSON file
      const invalidPath = path.join(__dirname, 'invalid.json');
      const fs = require('fs');
      fs.writeFileSync(invalidPath, '{ invalid json }');

      try {
        assert.throws(
          () => new EngineAdapterResolver(invalidPath),
          /Invalid engine adapters registry/
        );
      } finally {
        fs.unlinkSync(invalidPath);
      }
    });
  });
});