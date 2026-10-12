const { describe, it } = require('node:test');
const { before } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const path = require('path');

describe('Engine Adapters Registry Config', () => {
  const registryPath = path.join(__dirname, 'engine-adapters.json');

  describe('registry file', () => {
    it('should exist and be readable', () => {
      assert.strictEqual(fs.existsSync(registryPath), true);
      
      // Should be valid JSON
      const content = fs.readFileSync(registryPath, 'utf8');
      const registry = JSON.parse(content);
      assert.notStrictEqual(registry, undefined);
    });

    it('should have required engine entries', () => {
      const content = fs.readFileSync(registryPath, 'utf8');
      const registry = JSON.parse(content);
      
      assert.ok('scc' in registry, 'missing property ' + 'scc');
      assert.ok('devin' in registry, 'missing property ' + 'devin');
    });

    describe('scc engine configuration', () => {
      let sccConfig;
      
      before(() => {
        const content = fs.readFileSync(registryPath, 'utf8');
        const registry = JSON.parse(content);
        sccConfig = registry.scc;
      });

      it('should have required fields', () => {
        assert.ok('binary' in sccConfig, 'missing property ' + 'binary');
        assert.ok('argvBuilder' in sccConfig, 'missing property ' + 'argvBuilder');
        assert.ok('exitCodeMap' in sccConfig, 'missing property ' + 'exitCodeMap');
      });

      it('should have correct binary', () => {
        assert.strictEqual(sccConfig.binary, 'npx sc-agent-cli@latest');
      });

      it('should have argvBuilder as function string', () => {
        assert.strictEqual(typeof sccConfig.argvBuilder, 'string');
        // Should contain function body
        assert.ok(sccConfig.argvBuilder.includes('function'), 'expected to contain function');
        assert.ok(sccConfig.argvBuilder.includes('argvBuilder'), 'expected to contain argvBuilder');
      });

      it('should have exitCodeMap as object', () => {
        assert.strictEqual(typeof sccConfig.exitCodeMap, 'object');
        assert.ok(0 in sccConfig.exitCodeMap, 'missing property ' + '0');
        assert.ok(10 in sccConfig.exitCodeMap, 'missing property ' + '10');
        assert.ok(20 in sccConfig.exitCodeMap, 'missing property ' + '20');
      });
    });

    describe('devin engine configuration', () => {
      let devinConfig;
      
      before(() => {
        const content = fs.readFileSync(registryPath, 'utf8');
        const registry = JSON.parse(content);
        devinConfig = registry.devin;
      });

      it('should have required fields', () => {
        assert.ok('binary' in devinConfig, 'missing property ' + 'binary');
        assert.ok('argvBuilder' in devinConfig, 'missing property ' + 'argvBuilder');
        assert.ok('exitCodeMap' in devinConfig, 'missing property ' + 'exitCodeMap');
      });

      it('should have correct binary', () => {
        assert.strictEqual(devinConfig.binary, 'devin');
      });

      it('should have argvBuilder as function string', () => {
        assert.strictEqual(typeof devinConfig.argvBuilder, 'string');
        assert.ok(devinConfig.argvBuilder.includes('function'), 'expected to contain function');
        assert.ok(devinConfig.argvBuilder.includes('argvBuilder'), 'expected to contain argvBuilder');
      });

      it('should have exitCodeMap as object', () => {
        assert.strictEqual(typeof devinConfig.exitCodeMap, 'object');
        assert.ok(0 in devinConfig.exitCodeMap, 'missing property ' + '0');
        assert.ok(1 in devinConfig.exitCodeMap, 'missing property ' + '1');
        assert.ok(2 in devinConfig.exitCodeMap, 'missing property ' + '2');
      });
    });
  });

  describe('validation functions', () => {
    // Test that the registry validates required fields
    it('should reject malformed entries', () => {
      // This test would typically be done by the resolver, but we can verify
      // that our registry has the correct structure
      const content = fs.readFileSync(registryPath, 'utf8');
      const registry = JSON.parse(content);
      
      // Valid entries should have all required fields
      ['scc', 'devin'].forEach(engineId => {
        const config = registry[engineId];
        assert.notStrictEqual(config, undefined);
        assert.notStrictEqual(config.binary, undefined);
        assert.strictEqual(typeof config.binary, 'string');
        assert.notStrictEqual(config.argvBuilder, undefined);
        assert.notStrictEqual(config.exitCodeMap, undefined);
        assert.strictEqual(typeof config.exitCodeMap, 'object');
      });
    });
  });
});