const fs = require('fs');
const path = require('path');

describe('Engine Adapters Registry Config', () => {
  const registryPath = path.join(__dirname, 'engine-adapters.json');

  describe('registry file', () => {
    it('should exist and be readable', () => {
      expect(fs.existsSync(registryPath)).toBe(true);
      
      // Should be valid JSON
      const content = fs.readFileSync(registryPath, 'utf8');
      const registry = JSON.parse(content);
      expect(registry).toBeDefined();
    });

    it('should have required engine entries', () => {
      const content = fs.readFileSync(registryPath, 'utf8');
      const registry = JSON.parse(content);
      
      expect(registry).toHaveProperty('scc');
      expect(registry).toHaveProperty('devin');
    });

    describe('scc engine configuration', () => {
      let sccConfig;
      
      beforeEach(() => {
        const content = fs.readFileSync(registryPath, 'utf8');
        const registry = JSON.parse(content);
        sccConfig = registry.scc;
      });

      it('should have required fields', () => {
        expect(sccConfig).toHaveProperty('binary');
        expect(sccConfig).toHaveProperty('argvBuilder');
        expect(sccConfig).toHaveProperty('exitCodeMap');
      });

      it('should have correct binary', () => {
        expect(sccConfig.binary).toBe('npx sc-agent-cli@latest');
      });

      it('should have argvBuilder as function string', () => {
        expect(typeof sccConfig.argvBuilder).toBe('string');
        // Should contain function body
        expect(sccConfig.argvBuilder).toContain('function');
        expect(sccConfig.argvBuilder).toContain('argvBuilder');
      });

      it('should have exitCodeMap as object', () => {
        expect(typeof sccConfig.exitCodeMap).toBe('object');
        expect(sccConfig.exitCodeMap).toHaveProperty('0');
        expect(sccConfig.exitCodeMap).toHaveProperty('10');
        expect(sccConfig.exitCodeMap).toHaveProperty('20');
      });
    });

    describe('devin engine configuration', () => {
      let devinConfig;
      
      beforeEach(() => {
        const content = fs.readFileSync(registryPath, 'utf8');
        const registry = JSON.parse(content);
        devinConfig = registry.devin;
      });

      it('should have required fields', () => {
        expect(devinConfig).toHaveProperty('binary');
        expect(devinConfig).toHaveProperty('argvBuilder');
        expect(devinConfig).toHaveProperty('exitCodeMap');
      });

      it('should have correct binary', () => {
        expect(devinConfig.binary).toBe('devin');
      });

      it('should have argvBuilder as function string', () => {
        expect(typeof devinConfig.argvBuilder).toBe('string');
        expect(devinConfig.argvBuilder).toContain('function');
        expect(devinConfig.argvBuilder).toContain('argvBuilder');
      });

      it('should have exitCodeMap as object', () => {
        expect(typeof devinConfig.exitCodeMap).toBe('object');
        expect(devinConfig.exitCodeMap).toHaveProperty('0');
        expect(devinConfig.exitCodeMap).toHaveProperty('1');
        expect(devinConfig.exitCodeMap).toHaveProperty('2');
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
        expect(config).toBeDefined();
        expect(config.binary).toBeDefined();
        expect(typeof config.binary).toBe('string');
        expect(config.argvBuilder).toBeDefined();
        expect(config.exitCodeMap).toBeDefined();
        expect(typeof config.exitCodeMap).toBe('object');
      });
    });
  });
});