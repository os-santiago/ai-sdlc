// scripts/security-gate.test.js — unit tests for the deterministic
// supply-chain security gate (issue #91, epic #87). All OSV/registry data
// is injected — tests never touch the network.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  normalizeGitleaks,
  normalizeDepVet,
  vulnId,
  osvHasFix,
  enrichVulns,
  resolveConfig,
  applyOverrides,
  outcomeFor,
  evaluateFinding,
  main,
} = require('./security-gate');

const ORG_MATRIX = {
  'pr:risk-low': 'report-only',
  'pr:risk-medium': 'report-only',
  'pr:risk-high': 'blocking-if-fix-available',
  'pr:risk-critical': 'blocking-if-fix-available',
};

const cfg = (over = {}) => resolveConfig(Object.assign({
  enabled: true, min_severity: 'high', block_on_secret: true,
  severity_matrix: ORG_MATRIX,
}, over));

const tmp = (name, doc) => {
  const p = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'sg-')), name);
  fs.writeFileSync(p, JSON.stringify(doc));
  return p;
};

// --- normalization ----------------------------------------------------------

describe('normalizeGitleaks', () => {
  it('maps each entry to a critical secret finding', () => {
    const f = normalizeGitleaks([
      { RuleID: 'aws-key', File: 'a.py', StartLine: 4, Fingerprint: 'fp1' },
      { RuleID: 'generic', File: 'b.txt', StartLine: 1, Fingerprint: 'fp2' },
    ]);
    assert.strictEqual(f.length, 2);
    assert.strictEqual(f[0].category, 'secret');
    assert.strictEqual(f[0].severity, 'critical');
    assert.strictEqual(f[0].location, 'a.py:4');
  });
  it('non-array reports yield no findings', () => {
    assert.deepStrictEqual(normalizeGitleaks({}), []);
    assert.deepStrictEqual(normalizeGitleaks(null), []);
  });
});

describe('normalizeDepVet', () => {
  it('maps vet-pr violations to findings with canonical severities', () => {
    const vet = {
      violations: [{
        dep: { ecosystem: 'npm', name: 'left-pad', version: '1.0.0' },
        findings: [
          { kind: 'vulnerability', severity: 'blocker', detail: 'GHSA-aaaa-bbbb-cccc: RCE' },
          { kind: 'license', severity: 'major', detail: 'license "GPL-3.0" not in allowlist' },
          { kind: 'metadata-unavailable', severity: 'minor', detail: 'registry metadata unavailable' },
        ],
      }],
    };
    const f = normalizeDepVet(vet);
    assert.strictEqual(f.length, 3);
    assert.strictEqual(f[0].severity, 'critical');         // blocker → critical
    assert.strictEqual(f[0].id, 'GHSA-aaaa-bbbb-cccc');
    assert.strictEqual(f[1].category, 'license');
    assert.strictEqual(f[1].severity, 'high');             // major → high
    assert.strictEqual(f[2].severity, 'low');              // minor → low
  });
});

// --- OSV enrichment ---------------------------------------------------------

describe('osvHasFix / enrichVulns', () => {
  it('detects a fixed event in affected ranges', () => {
    assert.ok(osvHasFix({ affected: [{ ranges: [{ events: [{ introduced: '0' }, { fixed: '1.0.1' }] }] }] }));
    assert.ok(!osvHasFix({ affected: [{ ranges: [{ events: [{ introduced: '0' }] }] }] }));
    assert.ok(!osvHasFix({}));
  });

  it('enriches severity + fix availability from injected OSV detail', async () => {
    const findings = [{ kind: 'vulnerability', id: 'GHSA-x', severity: 'critical', fix_available: false }];
    const fetchJson = async () => ({
      database_specific: { severity: 'MODERATE' },
      affected: [{ ranges: [{ events: [{ introduced: '0' }, { fixed: '2.0.0' }] }] }],
    });
    await enrichVulns(findings, { fetchJson });
    assert.strictEqual(findings[0].severity, 'medium');    // OSV severity wins
    assert.strictEqual(findings[0].fix_available, true);
  });

  it('unreachable OSV leaves fix_available null (b-i-f-a degrades)', async () => {
    const findings = [{ kind: 'vulnerability', id: 'GHSA-y', severity: 'high' }];
    await enrichVulns(findings, { fetchJson: async () => null });
    assert.strictEqual(findings[0].fix_available, null);
  });

  it('caps detail lookups at maxLookups', async () => {
    const findings = Array.from({ length: 5 }, (_, i) =>
      ({ kind: 'vulnerability', id: `GHSA-${i}`, severity: 'high' }));
    let calls = 0;
    await enrichVulns(findings, { maxLookups: 2, fetchJson: async () => { calls++; return { affected: [] }; } });
    assert.strictEqual(calls, 2);
  });
});

// --- contract resolution + override clamping ---------------------------------

describe('resolveConfig / applyOverrides', () => {
  it('schema defaults apply when the contract omits security', () => {
    const c = resolveConfig({});
    assert.strictEqual(c.enabled, true);
    assert.strictEqual(c.min_severity, 'high');
    assert.strictEqual(c.block_on_secret, true);
    assert.strictEqual(c.severity_matrix['pr:risk-high'], 'blocking-if-fix-available');
  });

  it('overrides clamp strictest-wins: loosening is suppressed, tightening lands', () => {
    const c = cfg();
    const out = applyOverrides(c, {
      enabled: false,                       // loosen — suppressed
      min_severity: 'low',                  // tighten — lands
      block_on_secret: false,               // loosen — suppressed
      severity_matrix: { 'pr:risk-high': 'report-only', 'pr:risk-medium': 'blocking' },
    });
    assert.strictEqual(out.enabled, true);
    assert.strictEqual(out.min_severity, 'low');
    assert.strictEqual(out.block_on_secret, true);
    assert.strictEqual(out.severity_matrix['pr:risk-high'], 'blocking-if-fix-available');
    assert.strictEqual(out.severity_matrix['pr:risk-medium'], 'blocking');
  });

  it('a disabled contract can only be tightened to enabled by override', () => {
    const off = cfg({ enabled: false });
    assert.strictEqual(applyOverrides(off, {}).enabled, false);
    assert.strictEqual(applyOverrides(off, { enabled: true }).enabled, true);
    assert.strictEqual(applyOverrides(off, { enabled: false }).enabled, false);
  });
});

// --- outcome + per-finding verdict -------------------------------------------

describe('outcomeFor / evaluateFinding', () => {
  it('tier lookup hits the declared matrix leaf', () => {
    assert.strictEqual(outcomeFor(ORG_MATRIX, 'pr:risk-high'), 'blocking-if-fix-available');
    assert.strictEqual(outcomeFor(ORG_MATRIX, 'pr:risk-low'), 'report-only');
  });

  it('unknown tier resolves to the strictest declared outcome (ambiguity resolves UP)', () => {
    assert.strictEqual(outcomeFor(ORG_MATRIX, null), 'blocking-if-fix-available');
    assert.strictEqual(outcomeFor({ 'pr:risk-low': 'blocking' }, null), 'blocking');
    assert.strictEqual(outcomeFor({}, null), 'report-only');
  });

  it('secret findings block unconditionally under block_on_secret', () => {
    const f = { category: 'secret', severity: 'low' };
    assert.strictEqual(evaluateFinding(f, cfg(), 'pr:risk-low').verdict, 'blocking');
    assert.strictEqual(evaluateFinding(f, cfg({ block_on_secret: false }), 'pr:risk-low').verdict, 'report-only');
  });

  it('license findings are report-only under every tier', () => {
    const f = { category: 'license', severity: 'critical' };
    assert.strictEqual(evaluateFinding(f, cfg(), 'pr:risk-critical').verdict, 'report-only');
  });

  it('blocking-class findings consult the matrix', () => {
    const c = cfg();
    const fixable = { category: 'supply-chain', severity: 'critical', fix_available: true };
    const unfixable = { category: 'supply-chain', severity: 'critical', fix_available: false };
    const unknownFix = { category: 'supply-chain', severity: 'critical', fix_available: null };
    assert.strictEqual(evaluateFinding(fixable, c, 'pr:risk-high').verdict, 'blocking');
    assert.strictEqual(evaluateFinding(fixable, c, 'pr:risk-low').verdict, 'report-only');
    // Unfixable/unknown-fix findings degrade to report-only under b-i-f-a.
    assert.strictEqual(evaluateFinding(unfixable, c, 'pr:risk-high').verdict, 'report-only');
    assert.strictEqual(evaluateFinding(unknownFix, c, 'pr:risk-high').verdict, 'report-only');
    // …but a `blocking` tier fails unconditionally.
    const hard = cfg({ severity_matrix: { 'pr:risk-high': 'blocking' } });
    assert.strictEqual(evaluateFinding(unfixable, hard, 'pr:risk-high').verdict, 'blocking');
  });

  it('sub-threshold severities are always report-only', () => {
    const f = { category: 'supply-chain', severity: 'medium', fix_available: true };
    assert.strictEqual(evaluateFinding(f, cfg(), 'pr:risk-critical').verdict, 'report-only');
    // A lowered min_severity pulls the same finding into blocking-class —
    // the tier's matrix outcome then arbitrates (report-only here).
    assert.strictEqual(evaluateFinding(f, cfg({ min_severity: 'low' }), 'pr:risk-low').verdict, 'report-only');
  });
});

// --- end-to-end main() --------------------------------------------------------

describe('main', () => {
  const run = (args, extra = {}) => main(args, { stdout: () => {}, fetchJson: async () => null, ...extra });

  it('blocking verdict exits 1; clean scan exits 0', async () => {
    const gl = tmp('gl.json', [{ RuleID: 'aws-key', File: 'a.py', StartLine: 4, Fingerprint: 'fp' }]);
    const r = await run(['--gitleaks', gl, '--tier', 'pr:risk-low']);
    assert.strictEqual(r.verdict, 'fail');
    assert.strictEqual(r.exitCode, 1);
    assert.strictEqual(r.blocking_count, 1);

    const clean = await run(['--tier', 'pr:risk-low']);
    assert.strictEqual(clean.verdict, 'pass');
    assert.strictEqual(clean.exitCode, 0);
    assert.strictEqual(clean.security_gate_outcome, 'report-only');
  });

  it('report-only findings never fail the job', async () => {
    const vet = tmp('vet.json', { violations: [{
      dep: { ecosystem: 'npm', name: 'x', version: '1.0.0' },
      findings: [{ kind: 'license', severity: 'major', detail: 'GPL-3.0 not in allowlist' }],
    }] });
    const r = await run(['--dep-vet', vet, '--tier', 'pr:risk-high']);
    assert.strictEqual(r.verdict, 'pass');
    assert.strictEqual(r.report_only_count, 1);
  });

  it('fixable high-severity vuln blocks on a b-i-f-a tier; unfixable degrades', async () => {
    const vet = tmp('vet.json', { violations: [{
      dep: { ecosystem: 'npm', name: 'x', version: '1.0.0' },
      findings: [{ kind: 'vulnerability', severity: 'blocker', detail: 'GHSA-z: known vuln' }],
    }] });
    const withFix = await run(['--dep-vet', vet, '--tier', 'pr:risk-high'], {
      fetchJson: async () => ({ affected: [{ ranges: [{ events: [{ introduced: '0' }, { fixed: '1.0.1' }] }] }] }),
    });
    assert.strictEqual(withFix.verdict, 'fail');
    assert.strictEqual(withFix.security_gate_outcome, 'blocking-if-fix-available');

    const noFix = await run(['--dep-vet', vet, '--tier', 'pr:risk-high'], {
      fetchJson: async () => ({ affected: [{ ranges: [{ events: [{ introduced: '0' }] }] }] }),
    });
    assert.strictEqual(noFix.verdict, 'pass');
  });

  it('scanner errors fail closed', async () => {
    const bad = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'sg-')), 'missing.json');
    const r = await run(['--gitleaks', bad]);
    assert.strictEqual(r.verdict, 'fail');
    assert.strictEqual(r.scanner_errors.length, 1);
  });

  it('unreadable dep-vet verdict is a scanner error', async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sg-'));
    const p = path.join(dir, 'vet.json');
    fs.writeFileSync(p, 'not-json{');
    const r = await run(['--dep-vet', p]);
    assert.strictEqual(r.verdict, 'fail');
    assert.ok(r.scanner_errors[0].includes('dep-vet'));
  });

  it('disabled gate skips with a pass', async () => {
    const contract = tmp('c.json', { security: { enabled: false } });
    const gl = tmp('gl.json', [{ RuleID: 'aws-key', File: 'a.py', StartLine: 4 }]);
    const r = await run(['--gitleaks', gl, '--contract', contract]);
    assert.strictEqual(r.action, 'skipped');
    assert.strictEqual(r.verdict, 'pass');
    assert.strictEqual(r.exitCode, 0);
  });

  it('override JSON clamps strictest-wins over the resolved contract', async () => {
    const contract = tmp('c.json', { security: { severity_matrix: { 'pr:risk-low': 'report-only' } } });
    const vet = tmp('vet.json', { violations: [{
      dep: { ecosystem: 'npm', name: 'x', version: '1.0.0' },
      findings: [{ kind: 'vulnerability', severity: 'blocker', detail: 'GHSA-z: vuln' }],
    }] });
    // Tighten pr:risk-low to blocking — the vuln now fails a risk-low PR.
    const hard = await run([
      '--dep-vet', vet, '--contract', contract, '--tier', 'pr:risk-low',
      '--overrides', JSON.stringify({ severity_matrix: { 'pr:risk-low': 'blocking' } }),
    ], { fetchJson: async () => ({ affected: [{ ranges: [{ events: [{ introduced: '0' }, { fixed: '1' }] }] }] }) });
    assert.strictEqual(hard.verdict, 'fail');
    // Loosening the org-declared high tier to report-only is suppressed.
    const soft = await run([
      '--dep-vet', vet, '--contract', contract, '--tier', 'pr:risk-high',
      '--overrides', JSON.stringify({ severity_matrix: { 'pr:risk-high': 'report-only' } }),
    ], { fetchJson: async () => ({ affected: [{ ranges: [{ events: [{ introduced: '0' }, { fixed: '1' }] }] }] }) });
    assert.strictEqual(soft.verdict, 'fail');
    assert.strictEqual(soft.severity_matrix['pr:risk-high'], 'blocking-if-fix-available');
  });

  it('writes the findings artifact + github outputs', async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sg-'));
    const out = path.join(dir, 'findings.json');
    const sum = path.join(dir, 'summary.md');
    const go = path.join(dir, 'gh-out');
    process.env.GITHUB_OUTPUT = go;
    try {
      const gl = tmp('gl.json', [{ RuleID: 'aws-key', File: 'a.py', StartLine: 4 }]);
      await run(['--gitleaks', gl, '--findings', out, '--summary', sum, '--github-output']);
      const doc = JSON.parse(fs.readFileSync(out, 'utf8'));
      assert.strictEqual(doc.verdict, 'fail');
      assert.ok(fs.readFileSync(sum, 'utf8').includes('ai-sdlc-security'));
      assert.ok(fs.readFileSync(go, 'utf8').includes('verdict=fail'));
      assert.ok(fs.readFileSync(go, 'utf8').includes('security_gate_outcome=blocking'));
    } finally {
      delete process.env.GITHUB_OUTPUT;
    }
  });
});
