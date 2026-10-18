#!/usr/bin/env node
// security-gate.js — deterministic supply-chain security gate evaluator
// (issue #91, epic #87; contract: spec/org-policy.md — Security Gate,
// `security` section of spec/ai-sdlc.schema.json).
//
// Reads scanner evidence for a PR head and applies the resolved
// `security.*` contract — never generative judgment:
//
//   * gitleaks JSON report            → category `secret` findings
//   * dep-update.js vet-pr verdict    → supply-chain + license findings
//   * OSV vuln detail (bounded)       → severity + upstream-fix enrichment
//
// Verdict rules (all deterministic):
//   * `block_on_secret` → secret findings block unconditionally.
//   * severity ≥ `min_severity` → blocking-class; the PR's `pr:risk-*`
//     tier resolves the gate outcome through `severity_matrix`:
//       report-only                → record, never fail
//       blocking                   → fail unconditionally
//       blocking-if-fix-available  → fail only when an upstream fix exists
//                                    (unknown/no-fix degrades to report-only)
//   * license observations and sub-threshold findings are always
//     report-only (spec floor).
//   * a scanner that failed to produce parseable evidence is a
//     `scanner-error` — fail-closed: a gate that cannot see is not a pass.
//
// Input overrides (`--overrides` JSON of the security.* section) clamp
// strictest-wins against the resolved contract — a caller may tighten,
// never loosen (the same floor rule resolve.py applies to org baselines).
//
// Emits: findings JSON + markdown summary; `security_gate_outcome` and
// verdict to --github-output / stdout. Exit 1 on a blocking verdict,
// 0 otherwise (report-only findings never fail the job), 2 on usage
// errors.
//
// Trust boundary: run only from the trusted runtime pin
// (fetch-runtime-file) — never from a PR checkout.

'use strict';
const fs = require('fs');

// ---------------------------------------------------------------------------
// Contract vocabulary
// ---------------------------------------------------------------------------

// Schema severities (security.min_severity) — rank order is normative.
const SEVERITY_RANK = { low: 0, medium: 1, high: 2, critical: 3 };
// Schema security_gate_outcome enum — strictness order for floor clamping.
const OUTCOME_RANK = { 'report-only': 0, 'blocking-if-fix-available': 1, 'blocking': 2 };

// dep-update.js vetFinding.severity → schema severity (deterministic map).
const DEP_SEVERITY_MAP = { blocker: 'critical', major: 'high', minor: 'low' };
// OSV database_specific.severity → schema severity (GHSA-class records).
const OSV_DB_SEVERITY = { CRITICAL: 'critical', HIGH: 'high', MODERATE: 'medium', MEDIUM: 'medium', LOW: 'low' };

// dep-update.js finding kinds → gate category. License observations are a
// separate, always-report-only class per spec/org-policy.md.
const DEP_KIND_CATEGORY = {
  vulnerability: 'supply-chain',
  typosquat: 'supply-chain',
  nonexistent: 'supply-chain',
  'fresh-release': 'supply-chain',
  'metadata-unavailable': 'supply-chain',
  license: 'license',
};

const SCHEMA_DEFAULTS = {
  enabled: true,
  min_severity: 'high',
  block_on_secret: true,
  severity_matrix: {
    'pr:risk-low': 'report-only',
    'pr:risk-medium': 'report-only',
    'pr:risk-high': 'blocking-if-fix-available',
    'pr:risk-critical': 'blocking-if-fix-available',
  },
};

const OSV_VULN_URL = 'https://api.osv.dev/v1/vulns/';
const DEFAULT_MAX_VULN_LOOKUPS = 20;

// ---------------------------------------------------------------------------
// Normalization — scanner outputs → uniform finding records
// ---------------------------------------------------------------------------

function readJsonFile(path) {
  try {
    return JSON.parse(fs.readFileSync(path, 'utf8'));
  } catch (e) {
    return { __error: `${path}: ${e.message}` };
  }
}

/** gitleaks report → secret findings (severity critical — block_on_secret
 * arbitrates the verdict, not the scanner's own rank). */
function normalizeGitleaks(report) {
  if (!Array.isArray(report)) return [];
  const out = [];
  for (const f of report) {
    out.push({
      scanner: 'gitleaks',
      category: 'secret',
      kind: 'secret',
      id: f.RuleID || null,
      severity: 'critical',
      fix_available: false,
      location: f.File ? `${f.File}:${f.StartLine || '?'}` : null,
      fingerprint: f.Fingerprint || null,
      detail: `gitleaks rule \`${f.RuleID || 'unknown'}\` — credential material in range (redacted)`,
    });
  }
  return out;
}

/** Leading vuln id out of a dep-vet detail string ("GHSA-…: summary"). */
function vulnId(detail) {
  const m = String(detail || '').match(/^([A-Za-z][A-Za-z0-9._-]*)(?=:|\s|$)/);
  return m ? m[1] : null;
}

/** dep-update.js vet-pr verdict → supply-chain/license findings. */
function normalizeDepVet(vet) {
  const out = [];
  for (const v of (vet && vet.violations) || []) {
    const dep = v.dep || {};
    const pkg = dep.name ? `${dep.ecosystem || '?'}:${dep.name}@${dep.version || '?'}` : null;
    for (const f of v.findings || []) {
      const kind = f.kind || 'unknown';
      out.push({
        scanner: 'dep-audit',
        category: DEP_KIND_CATEGORY[kind] || 'supply-chain',
        kind,
        id: kind === 'vulnerability' ? vulnId(f.detail) : null,
        severity: DEP_SEVERITY_MAP[f.severity] || 'medium',
        fix_available: false,   // vulnerabilities enriched below via OSV
        package: pkg,
        detail: f.detail || '',
      });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// OSV enrichment — severity + upstream-fix availability (bounded lookups)
// ---------------------------------------------------------------------------

/** True when any affected range records a `fixed` event — an upstream fix
 * exists, so `blocking-if-fix-available` stays hard for this finding. */
function osvHasFix(vuln) {
  for (const a of (vuln && vuln.affected) || []) {
    for (const r of a.ranges || []) {
      for (const ev of r.events || []) {
        if (ev && ev.fixed) return true;
      }
    }
  }
  return false;
}

function osvSeverity(vuln) {
  const sev = vuln && vuln.database_specific && vuln.database_specific.severity;
  const mapped = OSV_DB_SEVERITY[String(sev || '').toUpperCase()];
  return mapped || null;
}

/** Enrich vulnerability findings with OSV vuln detail. `fetchJson` is
 * injectable (tests never hit the network); a failed lookup leaves
 * fix_available null → the b-i-f-a tier degrades to report-only, per the
 * contract's "never paralyzed by what cannot be fixed" rule. */
async function enrichVulns(findings, opts = {}) {
  const maxLookups = opts.maxLookups !== undefined ? opts.maxLookups : DEFAULT_MAX_VULN_LOOKUPS;
  const fetchJson = opts.fetchJson;
  if (typeof fetchJson !== 'function') return;
  const ids = [...new Set(findings
    .filter((f) => f.kind === 'vulnerability' && f.id)
    .map((f) => f.id))].slice(0, maxLookups);
  const details = new Map();
  for (const id of ids) {
    const v = await fetchJson(OSV_VULN_URL + encodeURIComponent(id));
    if (v && !v.__status) details.set(id, v);
  }
  for (const f of findings) {
    if (f.kind !== 'vulnerability') continue;
    const v = f.id ? details.get(f.id) : null;
    if (!v) {
      f.fix_available = null;   // unknown — b-i-f-a degrades to report-only
      continue;
    }
    const sev = osvSeverity(v);
    if (sev) f.severity = sev;
    f.fix_available = osvHasFix(v);
  }
}

// ---------------------------------------------------------------------------
// Contract resolution + verdict
// ---------------------------------------------------------------------------

/** Merge schema defaults with the resolved contract's `security` object. */
function resolveConfig(contract) {
  const c = contract && typeof contract === 'object' ? contract : {};
  return {
    enabled: c.enabled !== undefined ? !!c.enabled : SCHEMA_DEFAULTS.enabled,
    min_severity: c.min_severity in SEVERITY_RANK ? c.min_severity : SCHEMA_DEFAULTS.min_severity,
    block_on_secret: c.block_on_secret !== undefined ? !!c.block_on_secret : SCHEMA_DEFAULTS.block_on_secret,
    severity_matrix: Object.assign({}, SCHEMA_DEFAULTS.severity_matrix, c.severity_matrix || {}),
  };
}

/** Strictest-wins input overrides — a caller may tighten the resolved
 * contract, never loosen it (org-floor semantics). */
function applyOverrides(cfg, o) {
  const out = Object.assign({}, cfg, { severity_matrix: Object.assign({}, cfg.severity_matrix) });
  if (!o || typeof o !== 'object') return out;
  if (o.enabled !== undefined) {
    const v = o.enabled === true || o.enabled === 'true';
    out.enabled = cfg.enabled === false ? v : true;
  }
  if (o.min_severity !== undefined && o.min_severity in SEVERITY_RANK &&
      SEVERITY_RANK[o.min_severity] < SEVERITY_RANK[out.min_severity]) {
    out.min_severity = o.min_severity;
  }
  if (o.block_on_secret !== undefined) {
    const v = o.block_on_secret === true || o.block_on_secret === 'true';
    out.block_on_secret = cfg.block_on_secret === false ? v : true;
  }
  for (const [tier, outcome] of Object.entries(o.severity_matrix || {})) {
    if (!(outcome in OUTCOME_RANK)) continue;
    const cur = out.severity_matrix[tier];
    if (!cur || OUTCOME_RANK[cur] < OUTCOME_RANK[outcome]) out.severity_matrix[tier] = outcome;
  }
  return out;
}

/** Gate outcome for a blocking-class finding on the PR's risk tier. An
 * unknown/absent tier resolves to the strictest declared matrix outcome —
 * ambiguity resolves UP, never silently to report-only. */
function outcomeFor(matrix, tier) {
  if (tier && matrix[tier]) return matrix[tier];
  let best = 'report-only';
  for (const v of Object.values(matrix || {})) {
    if (OUTCOME_RANK[v] > OUTCOME_RANK[best]) best = v;
  }
  return best;
}

/** Per-finding verdict: {verdict, outcome, rule}. */
function evaluateFinding(f, cfg, tier) {
  if (f.category === 'secret') {
    return cfg.block_on_secret
      ? { verdict: 'blocking', outcome: 'blocking', rule: 'block_on_secret' }
      : { verdict: 'report-only', outcome: 'report-only', rule: 'block_on_secret disabled' };
  }
  // License observations stay report-only under every tier (spec floor).
  if (f.category === 'license') {
    return { verdict: 'report-only', outcome: 'report-only', rule: 'license advisory' };
  }
  if ((SEVERITY_RANK[f.severity] === undefined ? -1 : SEVERITY_RANK[f.severity]) <
      SEVERITY_RANK[cfg.min_severity]) {
    return { verdict: 'report-only', outcome: 'report-only', rule: `below min_severity (${cfg.min_severity})` };
  }
  const outcome = outcomeFor(cfg.severity_matrix, tier);
  const label = tier || 'unlabeled';
  if (outcome === 'blocking') {
    return { verdict: 'blocking', outcome, rule: `${label} → blocking` };
  }
  if (outcome === 'blocking-if-fix-available') {
    return f.fix_available === true
      ? { verdict: 'blocking', outcome, rule: `${label} → blocking-if-fix-available (upstream fix exists)` }
      : { verdict: 'report-only', outcome,
          rule: `${label} → blocking-if-fix-available degraded (${f.fix_available === null ? 'fix availability unknown' : 'no upstream fix'})` };
  }
  return { verdict: 'report-only', outcome, rule: `${label} → report-only` };
}

// ---------------------------------------------------------------------------
// Evidence rendering
// ---------------------------------------------------------------------------

function esc(s) {
  return String(s == null ? '' : s).replace(/\\/g, '\\\\').replace(/\|/g, '\\|').replace(/\n/g, ' ').slice(0, 160);
}

function buildSummary(result) {
  const L = [];
  L.push('### ai-sdlc-security — deterministic supply-chain gate');
  L.push('');
  L.push(`- verdict: \`${result.verdict}\` · outcome: \`${result.security_gate_outcome}\` · action: \`${result.action}\``);
  L.push(`- contract: min_severity \`${result.min_severity}\` · block_on_secret \`${result.block_on_secret}\` · tier \`${result.tier || 'unlabeled'}\``);
  L.push(`- findings: ${result.findings_count} total · ${result.blocking_count} blocking · ${result.report_only_count} report-only`);
  if (result.scanner_errors.length) {
    L.push(`- scanner errors (fail-closed): ${result.scanner_errors.join('; ')}`);
  }
  if (result.findings.length) {
    L.push('');
    L.push('| scanner | kind | severity | fix | where | verdict | rule |');
    L.push('|---|---|---|---|---|---|---|');
    for (const f of result.findings) {
      L.push(`| ${f.scanner} | ${esc(f.kind)} | ${f.severity} | ${f.fix_available === null ? '?' : f.fix_available ? 'yes' : 'no'} | ${esc(f.location || f.package || f.id || '—')} | ${f.verdict} | ${esc(f.rule)} |`);
    }
  }
  L.push('');
  return L.join('\n');
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

function parseArgs(argv) {
  const opts = { _: [] };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--gitleaks') opts.gitleaks = next();
    else if (a === '--gitleaks-status') opts.gitleaksStatus = Number(next());
    else if (a === '--dep-vet') opts.depVet = next();
    else if (a === '--contract') opts.contract = next();
    else if (a === '--overrides') opts.overrides = next();
    else if (a === '--tier') opts.tier = next();
    else if (a === '--findings') opts.findings = next();
    else if (a === '--summary') opts.summary = next();
    else if (a === '--max-vuln-lookups') opts.maxVulnLookups = Number(next());
    else if (a === '--egress-profile') opts.egressProfile = next();
    else if (a === '--offline') opts.offline = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') opts.help = true;
    else if (a.startsWith('--')) throw new Error(`unknown argument: ${a}`);
    else opts._.push(a);
  }
  return opts;
}

function usage() {
  return [
    'usage: security-gate.js [--gitleaks FILE [--gitleaks-status N]] [--dep-vet FILE]',
    '                        [--contract FILE] [--overrides JSON] [--tier pr:risk-*]',
    '                        [--findings FILE] [--summary FILE] [--json] [--github-output]',
    '                        [--max-vuln-lookups N] [--egress-profile P|--offline]',
    '',
    'Evaluates scanner evidence against the resolved security.* contract.',
    'Exit 1 = blocking verdict (the check fails); 0 = pass/skipped; 2 = usage error.',
  ].join('\n');
}

/** Lazy egress-bounded fetcher from the dep-update module (same runtime
 * pin — registry.npmjs.org + api.osv.dev under the npm-vet profile). */
function defaultFetcher(opts) {
  try {
    const dep = require('./dep-update.js');
    const egress = dep.resolveEgress(opts.offline ? 'off' : opts.egressProfile);
    return dep.makeFetcher(egress);
  } catch (e) {
    console.error(`security-gate: dep-update module unavailable (${e.message}) — vuln enrichment skipped`);
    return null;
  }
}

async function main(argv, { stdout, fetchJson } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  if (opts.help) { emit(usage()); return { action: 'help' }; }

  const findings = [];
  const scannerErrors = [];

  // --- secret findings ------------------------------------------------------
  if (opts.gitleaks) {
    const report = readJsonFile(opts.gitleaks);
    const status = opts.gitleaksStatus !== undefined ? opts.gitleaksStatus : 0;
    if (report && report.__error) {
      scannerErrors.push(`gitleaks report unreadable (${report.__error})`);
    } else {
      // Exit contract (callers map gitleaks' --exit-code onto it):
      // 0 = clean scan, 1 = findings — the report is evidence. Any other
      // status is a scanner failure — never a clean scan (fail closed).
      if (status !== 0 && status !== 1) {
        scannerErrors.push(`gitleaks exited ${status} — scanner failure, not a clean scan`);
      } else {
        const normalized = normalizeGitleaks(report);
        // A leak-class exit with no parseable findings means the evidence
        // was lost — an empty report at status 1 is not a pass.
        if (status === 1 && normalized.length === 0) {
          scannerErrors.push('gitleaks reported findings (status 1) but the report held none — evidence unavailable');
        }
        findings.push(...normalized);
      }
    }
  }

  // --- dependency audit findings --------------------------------------------
  if (opts.depVet) {
    const vet = readJsonFile(opts.depVet);
    if (vet && vet.__error) {
      scannerErrors.push(`dep-vet verdict unreadable (${vet.__error})`);
    } else {
      findings.push(...normalizeDepVet(vet));
    }
  }

  // --- OSV enrichment (bounded, egress-checked) ------------------------------
  await enrichVulns(findings, {
    maxLookups: Number.isFinite(opts.maxVulnLookups) ? opts.maxVulnLookups : DEFAULT_MAX_VULN_LOOKUPS,
    fetchJson: fetchJson !== undefined ? fetchJson : defaultFetcher(opts),
  });

  // --- contract --------------------------------------------------------------
  const contractDoc = opts.contract ? readJsonFile(opts.contract) : {};
  const rawSecurity = (contractDoc && contractDoc.__error) ? {} : (contractDoc.security || contractDoc);
  let cfg = resolveConfig(rawSecurity);
  let overrides = {};
  if (opts.overrides) {
    try { overrides = JSON.parse(opts.overrides); }
    catch (e) { throw new Error(`--overrides is not valid JSON: ${e.message}`); }
  }
  cfg = applyOverrides(cfg, overrides);

  const tier = opts.tier || null;
  const result = {
    action: 'evaluated',
    verdict: 'pass',
    security_gate_outcome: 'report-only',
    tier,
    min_severity: cfg.min_severity,
    block_on_secret: cfg.block_on_secret,
    severity_matrix: cfg.severity_matrix,
    scanner_errors: scannerErrors,
    findings_count: 0,
    blocking_count: 0,
    report_only_count: 0,
    findings: [],
  };

  if (cfg.enabled === false) {
    result.action = 'skipped';
    result.reason = 'security.enabled=false in the resolved contract';
  } else {
    for (const f of findings) {
      const r = evaluateFinding(f, cfg, tier);
      f.verdict = r.verdict;
      f.outcome = r.outcome;
      f.rule = r.rule;
      result.findings.push(f);
      if (r.verdict === 'blocking') {
        result.blocking_count += 1;
        result.verdict = 'fail';
      } else {
        result.report_only_count += 1;
      }
      // The emitted gate outcome is the strictest outcome actually applied.
      if (OUTCOME_RANK[r.outcome] > OUTCOME_RANK[result.security_gate_outcome]) {
        result.security_gate_outcome = r.outcome;
      }
    }
    // Scanner errors fail closed — the gate cannot certify what it never saw.
    if (scannerErrors.length) result.verdict = 'fail';
  }
  result.findings_count = result.findings.length;

  const summary = buildSummary(result);
  if (opts.summary) fs.writeFileSync(opts.summary, summary);
  if (opts.findings) fs.writeFileSync(opts.findings, JSON.stringify(result, null, 2) + '\n');
  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT, [
      `action=${result.action}`,
      `verdict=${result.verdict}`,
      `security_gate_outcome=${result.security_gate_outcome}`,
      `findings_count=${result.findings_count}`,
      `blocking_count=${result.blocking_count}`,
      '',
    ].join('\n'));
  }
  emit(JSON.stringify(result, null, 2));
  result.exitCode = result.verdict === 'fail' ? 1 : 0;
  return result;
}

if (require.main === module) {
  main(process.argv.slice(2)).then((r) => {
    process.exit(typeof r === 'object' && r ? r.exitCode || 0 : 0);
  }).catch((e) => {
    console.error(`security-gate: ${e.message}`);
    process.exit(2);
  });
}

module.exports = {
  SEVERITY_RANK,
  OUTCOME_RANK,
  DEP_SEVERITY_MAP,
  normalizeGitleaks,
  normalizeDepVet,
  vulnId,
  osvHasFix,
  osvSeverity,
  enrichVulns,
  resolveConfig,
  applyOverrides,
  outcomeFor,
  evaluateFinding,
  buildSummary,
  parseArgs,
  main,
};
