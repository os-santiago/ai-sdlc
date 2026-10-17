#!/usr/bin/env node
// scripts/vuln-feed.js — vulnerability feed → bounded, deduped auto-issues
// (issue #99, dogfooding: the pipeline reports its own dependency
// vulnerabilities back through normal intake).
//
// Pipeline per watched repo:
//
//   1. surface — dependency set from package-lock.json/npm-shrinkwrap.json
//      (exact resolved versions, transitive included) when present; else
//      package.json declared ranges resolved to the max-satisfying
//      published version via the npm registry (bounded registry fetch).
//   2. query   — OSV /v1/querybatch over the resolved surface, reusing
//      scripts/dep-update.js helpers (resolveEgress / makeFetcher /
//      queryOsv) under the named `npm-vet` egress profile
//      (registry.npmjs.org + api.osv.dev only). OSV aggregates the
//      GitHub Advisory Database (GHSA entries are OSV records).
//   3. dedup   — open issues are scanned for the filing marker
//      `<!-- ai-sdlc:vuln-feed id:<ADVISORY> pkg:<NAME> -->` (same
//      signature-marker idiom as scripts/postmortem.js and
//      scripts/dup-detector.js): an open issue for the same
//      advisory+package is never duplicated; a *closed* canonical issue
//      also suppresses re-filing (no flood — a human reopened issue is
//      the re-arm path).
//   4. file    — at most --max-findings new issues per run (default 5),
//      each a DoR-complete `security(deps):` issue carrying the
//      `security` label, advisory link, fixed-version hint and a pointer
//      to the dep-update worker's vetted-bump path (issue #177) — the
//      same OSV gate that flags a vulnerable candidate clears it.
//
// Trust boundary: advisory and registry payloads are untrusted T3 input
// (spec/injection-defense.md) — escaped before they land in issue bodies,
// never executed, never pushed. The workflow fetches this script from the
// runtime pin via fetch-runtime-file, never from the scanned checkout.
//
// CLI:
//   node scripts/vuln-feed.js scan --repo OWNER/NAME
//        [--dir DIR | --lockfile FILE | --manifest FILE]
//        [--max-findings N] [--max-deps N] [--max-details N]
//        [--egress-profile npm-vet|off] [--egress-hosts h1,h2]
//        [--registry URL] [--labels csv] [--offline] [--dry-run]
//        [--report FILE] [--json] [--github-output]
'use strict';

const fs = require('fs');
const https = require('https');
const path = require('path');
const { execFileSync } = require('child_process');

const depUpdate = require('./dep-update');

const OSV_VULN = 'https://api.osv.dev/v1/vulns/';
const NPM_REGISTRY = 'https://registry.npmjs.org';

const DEFAULT_MAX_FINDINGS = 5;   // filings per run per repo (bounded)
const DEFAULT_MAX_DEPS = 200;     // dep surface cap → bounds OSV querybatch
const DEFAULT_MAX_DETAILS = 25;   // per-advisory detail GETs per run
const MAX_ISSUES = 100;           // open/closed issue scan bound
const DEFAULT_LABELS = 'security,ai-sdlc';

// Filing marker — the dedup signature (postmortem.js idiom: lowest-numbered
// open issue carrying the marker is the canonical; we never create a second).
const MARKER_PREFIX = 'ai-sdlc:vuln-feed';
const MARKER_RE = /<!--\s*ai-sdlc:vuln-feed\s+id:"?([^\s">]+)"?\s+pkg:"?([^\s">]+)"?\s*-->/g;

function vulnMarker(id, pkg) {
  return `<!-- ${MARKER_PREFIX} id:${id} pkg:"${pkg}" -->`;
}

const esc = (s) => String(s == null ? '' : s)
  .replace(/[<>`|\\\[\]]/g, (c) => '\\' + c)
  .slice(0, 200);

// ---------------------------------------------------------------------------
// Semver — minimal npm range resolution for the package.json fallback path.
// Supports ^ ~ >= <= > < = exact, x/* wildcards, hyphen ranges and ||
// unions — the declared-spec shapes package.json actually carries.
// Registry specs (npm:, file:, git+:, https?:, workspace:) are
// unresolvable → the dep is reported `unresolved`, never guessed.
// ---------------------------------------------------------------------------

const VER_RE = /^(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/;

function parseVersion(s) {
  const m = String(s || '').trim().replace(/^v/, '').match(VER_RE);
  if (!m) return null;
  return {
    major: +m[1],
    minor: m[2] === undefined ? null : +m[2],
    patch: m[3] === undefined ? null : +m[3],
    pre: m[4] || '',
  };
}

function cmpIdent(a, b) {
  const an = /^\d+$/.test(a), bn = /^\d+$/.test(b);
  if (an && bn) return Math.sign(+a - +b);
  if (an) return -1;               // numeric identifiers rank below alphanumeric
  if (bn) return 1;
  return a < b ? -1 : a > b ? 1 : 0;
}

/** semver precedence: a < b → -1, equal → 0, a > b → 1. Null fields = 0. */
function cmpVersion(a, b) {
  for (const k of ['major', 'minor', 'patch']) {
    const d = (a[k] == null ? 0 : a[k]) - (b[k] == null ? 0 : b[k]);
    if (d) return d < 0 ? -1 : 1;
  }
  if (!a.pre && !b.pre) return 0;
  if (!a.pre) return 1;            // release outranks its prereleases
  if (!b.pre) return -1;
  const pa = a.pre.split('.'), pb = b.pre.split('.');
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    if (pa[i] === undefined) return -1;
    if (pb[i] === undefined) return 1;
    const d = cmpIdent(pa[i], pb[i]);
    if (d) return d;
  }
  return 0;
}

function bumpAt(v, idx) {
  // Upper bound when a partial spec is widened: increment the last named
  // component, zero the rest (`1.2` → <1.3.0, `1` → <2.0.0).
  const out = { major: v.major == null ? 0 : v.major,
    minor: v.minor == null ? 0 : v.minor,
    patch: v.patch == null ? 0 : v.patch, pre: '' };
  if (idx === 'patch') out.patch += 1;
  else if (idx === 'minor') { out.minor += 1; out.patch = 0; }
  else { out.major += 1; out.minor = 0; out.patch = 0; }
  return out;
}

/** One comparator token → [{op, v}] or null when unparseable. */
function comparatorTokens(tok) {
  if (!tok || tok === '*' || /^[xX]$/.test(tok)) return [{ op: 'any' }];
  let m = tok.match(/^([~^])(.+)$/);
  if (m) {
    const v = parseVersion(m[2]);
    if (!v) return null;
    const lo = Object.assign({}, v, { minor: v.minor || 0, patch: v.patch || 0 });
    let hi;
    if (m[1] === '^') {
      if (v.major > 0 || v.minor == null) hi = bumpAt(v, 'major');
      else if (v.minor > 0 || v.patch == null) hi = bumpAt(v, 'minor');
      else hi = bumpAt(v, 'patch');
    } else { // '~'
      hi = v.minor == null ? bumpAt(v, 'major') : bumpAt(v, 'minor');
    }
    return [{ op: '>=', v: lo }, { op: '<', v: hi }];
  }
  m = tok.match(/^(>=|<=|>|<|=)?\s*(.+)$/);
  if (!m) return null;
  const op = m[1] || '=';
  let raw = m[2].trim();
  if (raw === '*' || /^[xX]$/.test(raw)) return [{ op: 'any' }];
  // x/* wildcard tails truncate to a partial spec (`1.x` ≡ `1`, `1.2.*` ≡ `1.2`).
  raw = raw.replace(/^v/i, '').replace(/(\.(x|X|\*))+$/, '');
  if (!raw) return [{ op: 'any' }];
  const v = parseVersion(raw);
  if (!v) return null;
  // Partial wildcard tail (`1.2.x`, `>=1.x`) widens per npm rules.
  if (v.minor == null) {
    if (op === '>=' || op === '>') return [{ op: '>=', v: { major: v.major, minor: 0, patch: 0, pre: '' } }];
    if (op === '<' || op === '<=') return [{ op: '<', v: bumpAt(v, 'major') }];
    return [{ op: '>=', v: { major: v.major, minor: 0, patch: 0, pre: '' } },
      { op: '<', v: bumpAt(v, 'major') }];
  }
  if (v.patch == null) {
    const lo = { major: v.major, minor: v.minor, patch: 0, pre: '' };
    if (op === '>=' || op === '>') return [{ op: '>=', v: lo }];
    if (op === '<' || op === '<=') return [{ op: '<', v: bumpAt(v, 'minor') }];
    return [{ op: '>=', v: lo }, { op: '<', v: bumpAt(v, 'minor') }];
  }
  return [{ op, v }];
}

/**
 * Compile a spec → list of alternatives, each a comparator list.
 * Returns null when the spec cannot be interpreted as a semver range
 * (registry/url/git specs — the caller marks the dep `unresolved`).
 */
function compileSpec(spec) {
  const s = String(spec || '').trim();
  if (!s || s === '*' || /^[xX]$/.test(s) || /^(latest|next|stable)$/i.test(s)) {
    return [[{ op: 'any' }]];
  }
  if (/^(npm:|workspace:|file:|link:|portal:|patch:|https?:|git[+:]|github:|bitbucket:|svn:)/i.test(s)) {
    return null;
  }
  const alts = [];
  for (const altRaw of s.split('||')) {
    const alt = altRaw.trim();
    if (!alt) continue;
    const comps = [];
    const hy = alt.match(/^(.+?)\s+-\s+(.+)$/);   // hyphen range `a - b`
    if (hy) {
      const lo = parseVersion(hy[1]), hi = parseVersion(hy[2]);
      if (!lo || !hi) return null;
      comps.push({ op: '>=', v: { major: lo.major, minor: lo.minor || 0, patch: lo.patch || 0, pre: lo.pre || '' } });
      if (hi.minor == null) comps.push({ op: '<', v: bumpAt(hi, 'major') });
      else if (hi.patch == null) comps.push({ op: '<', v: bumpAt(hi, 'minor') });
      else comps.push({ op: '<=', v: hi });
    } else {
      for (const tok of alt.split(/\s+/).filter(Boolean)) {
        const t = comparatorTokens(tok);
        if (!t) return null;
        comps.push(...t);
      }
    }
    alts.push(comps);
  }
  return alts.length ? alts : null;
}

function satisfyComps(comps, v) {
  let preAllowed = false;
  if (v.pre) {
    // npm rule: a prerelease satisfies only when a comparator in the same
    // alternative names its exact base triple with a prerelease tag.
    preAllowed = comps.some((c) => c.v && c.v.pre
      && c.v.major === v.major && c.v.minor === v.minor && c.v.patch === v.patch);
  }
  for (const c of comps) {
    if (c.op === 'any') continue;
    const d = cmpVersion(v, c.v);
    if (c.op === '>=' && d < 0) return false;
    if (c.op === '>' && d <= 0) return false;
    if (c.op === '<=' && d > 0) return false;
    if (c.op === '<' && d >= 0) return false;
    if (c.op === '=' && d !== 0) return false;
  }
  return !v.pre || preAllowed;
}

function satisfies(spec, version, compiledCache) {
  const comps = compiledCache || compileSpec(spec);
  if (!comps) return false;
  const v = typeof version === 'string' ? parseVersion(version) : version;
  if (!v) return false;
  return comps.some((alt) => satisfyComps(alt, v));
}

/** Highest published version satisfying the spec, or null. */
function maxSatisfying(spec, versions) {
  const comps = compileSpec(spec);
  if (!comps) return null;
  let best = null;
  for (const raw of versions || []) {
    const v = parseVersion(raw);
    if (!v) continue;
    const full = { major: v.major, minor: v.minor == null ? 0 : v.minor, patch: v.patch == null ? 0 : v.patch, pre: v.pre };
    if (!satisfyCompsAny(comps, full)) continue;
    if (!best || cmpVersion(full, best) > 0) best = full;
  }
  if (!best) return null;
  return `${best.major}.${best.minor}.${best.patch}${best.pre ? `-${best.pre}` : ''}`;
}

function satisfyCompsAny(comps, v) {
  return comps.some((alt) => satisfyComps(alt, v));
}

// ---------------------------------------------------------------------------
// Dependency surface — lockfile (exact) preferred, manifest ranges else.
// ---------------------------------------------------------------------------

/** package-lock.json v2/v3 `packages` map (or v1 `dependencies` tree) →
 * deduped [{name, version, dev, ecosystem:'npm'}]. */
function parseLockfileDeps(text) {
  let j;
  try { j = JSON.parse(text); } catch (e) { return []; }
  const out = new Map();
  const add = (name, entry) => {
    if (!name || !entry || !entry.version) return;
    const key = `${name}@${entry.version}`;
    if (!out.has(key)) {
      out.set(key, { ecosystem: 'npm', name, version: String(entry.version), dev: !!entry.dev });
    }
  };
  if (j.packages && typeof j.packages === 'object') {
    for (const [p, entry] of Object.entries(j.packages)) {
      const i = p.lastIndexOf('node_modules/');
      if (i === -1) continue;
      add(p.slice(i + 'node_modules/'.length), entry);
    }
  }
  const walk = (deps) => {
    for (const [name, entry] of Object.entries(deps || {})) {
      add(name, entry);
      walk(entry && entry.dependencies);
    }
  };
  walk(j.dependencies);
  return [...out.values()];
}

/** package.json → declared [{name, spec, dev, ecosystem:'npm'}]. */
function parseManifestDeps(text) {
  let j;
  try { j = JSON.parse(text); } catch (e) { return []; }
  const out = [];
  for (const [name, spec] of Object.entries(j.dependencies || {})) {
    out.push({ ecosystem: 'npm', name, spec: String(spec), dev: false });
  }
  for (const [name, spec] of Object.entries(j.optionalDependencies || {})) {
    out.push({ ecosystem: 'npm', name, spec: String(spec), dev: false });
  }
  for (const [name, spec] of Object.entries(j.devDependencies || {})) {
    out.push({ ecosystem: 'npm', name, spec: String(spec), dev: true });
  }
  return out;
}

/** Resolve declared ranges → resolved deps via injectable versions
 * provider (async name → [versions]). Unresolvable specs/deps land in
 * `unresolved` — reported, never silently dropped. */
async function resolveManifestDeps(manifestDeps, fetchVersions) {
  const deps = [], unresolved = [];
  for (const d of manifestDeps) {
    const comps = compileSpec(d.spec);
    if (!comps) {
      unresolved.push({ name: d.name, spec: d.spec, reason: 'unparseable-spec' });
      continue;
    }
    const versions = await fetchVersions(d.name);
    if (!versions || !versions.length) {
      unresolved.push({ name: d.name, spec: d.spec, reason: 'registry-unavailable' });
      continue;
    }
    const v = maxSatisfying(d.spec, versions);
    if (!v) unresolved.push({ name: d.name, spec: d.spec, reason: 'no-satisfying-version' });
    else deps.push({ ecosystem: 'npm', name: d.name, version: v, spec: d.spec, dev: d.dev });
  }
  return { deps, unresolved };
}

// ---------------------------------------------------------------------------
// OSV — batch vuln query reuses dep-update's egress-bounded queryOsv;
// per-advisory detail GETs (severity/aliases/fixed) are bounded by
// --max-details and only run for findings that survived dedup.
// ---------------------------------------------------------------------------

async function fetchPackumentVersions(name, fetchJson, registryBase) {
  const base = registryBase || NPM_REGISTRY;
  const j = await fetchJson(`${base}/${encodeURIComponent(name).replace(/%40/g, '@')}`);
  if (!j || j.__status === 404 || !j.versions) return [];
  return Object.keys(j.versions);
}

async function fetchOsvDetail(id, fetchJson) {
  const j = await fetchJson(`${OSV_VULN}${encodeURIComponent(id)}`);
  return (j && typeof j === 'object' && j.id) ? j : null;
}

const SEVERITY_RANK = { critical: 5, high: 4, medium: 3, moderate: 3, low: 2, unknown: 1 };

function severityOf(detail) {
  const s = detail && detail.database_specific && detail.database_specific.severity;
  const t = String(s || '').toLowerCase();
  return SEVERITY_RANK[t] !== undefined ? t : 'unknown';
}

function fixedVersionsOf(detail, pkg) {
  const fixed = new Set();
  for (const a of (detail && detail.affected) || []) {
    for (const r of a.ranges || []) {
      for (const e of r.events || []) {
        if (e.fixed && (!a.package || !a.package.name || a.package.name === pkg)) {
          fixed.add(e.fixed);
        }
      }
    }
  }
  return [...fixed].sort((x, y) => cmpVersion(parseVersion(x) || { major: 0 }, parseVersion(y) || { major: 0 }));
}

/** Group raw dep×vuln hits into advisory findings keyed by id+pkg —
 * one issue per advisory per package regardless of how many installed
 * versions trip it. */
function groupFindings(deps, osvMap) {
  const groups = new Map();
  for (const d of deps) {
    const key = `npm:${d.name}@${d.version}`;   // queryOsv key shape (OSV eco name)
    for (const v of osvMap.get(key) || []) {
      const gk = `${v.id}|${d.name}`;
      if (!groups.has(gk)) {
        groups.set(gk, {
          id: v.id, name: d.name, summary: v.summary || '',
          versions: [], dev: !!d.dev, specs: new Set(),
        });
      }
      const g = groups.get(gk);
      if (!g.versions.includes(d.version)) g.versions.push(d.version);
      if (d.spec) g.specs.add(d.spec);
    }
  }
  return [...groups.values()].map((g) => Object.assign(g, { specs: [...g.specs] }));
}

// ---------------------------------------------------------------------------
// Dedup — marker idiom (postmortem.js): scan open issues for the
// ai-sdlc:vuln-feed signature; a closed canonical also suppresses refiling.
// ---------------------------------------------------------------------------

function markersOf(body) {
  const out = [];
  const text = String(body || '');
  let m;
  MARKER_RE.lastIndex = 0;
  while ((m = MARKER_RE.exec(text)) !== null) out.push({ id: m[1], pkg: m[2] });
  return out;
}

/** Partitions grouped findings: {file, dupes, suppressed}.
 * dupes = open canonical exists; suppressed = closed canonical exists. */
function dedupeFindings(findings, openIssues, closedIssues) {
  const openKeys = new Set(), closedMap = new Map();
  for (const i of openIssues || []) {
    for (const m of markersOf(i.body)) openKeys.add(`${m.id}|${m.pkg}`);
  }
  for (const i of closedIssues || []) {
    for (const m of markersOf(i.body)) {
      const k = `${m.id}|${m.pkg}`;
      if (!closedMap.has(k) || i.number < closedMap.get(k)) closedMap.set(k, i.number);
    }
  }
  const file = [], dupes = [], suppressed = [];
  for (const f of findings) {
    const k = `${f.id}|${f.name}`;
    if (openKeys.has(k)) dupes.push(f);
    else if (closedMap.has(k)) suppressed.push({ finding: f, issue: closedMap.get(k) });
    else file.push(f);
  }
  return { file, dupes, suppressed };
}

// ---------------------------------------------------------------------------
// Issue body — DoR-complete (spec/definition-of-ready.md R1–R6 shape:
// conventional title, ## Problem/Context/Acceptance criteria sections).
// ---------------------------------------------------------------------------

function buildTitle(f) {
  const t = `security(deps): ${f.id} affects ${f.name}@${f.versions[0] || '?'}`;
  return t.length <= 88 ? t : `security(deps): ${f.id} affects ${f.name}`;
}

function buildIssueBody(f, opts = {}) {
  const detail = f.detail || {};
  const aliases = (detail.aliases || []).filter((a) => a !== f.id).slice(0, 8);
  const ghsa = (detail.aliases || []).concat([f.id]).find((a) => /^GHSA-/i.test(a));
  const severity = severityOf(detail);
  const fixed = fixedVersionsOf(detail, f.name);
  const scope = f.dev ? 'devDependencies' : (f.source === 'package.json' ? 'declared dependencies' : 'resolved dependency tree');
  const lines = [];
  lines.push('## Problem', '');
  lines.push(`\`${esc(f.name)}\` version(s) \`${f.versions.map(esc).join('`, `')}\` are affected by **${esc(f.id)}**${f.summary ? ` — ${esc(f.summary)}` : ''}.`);
  lines.push('');
  lines.push('## Context', '');
  lines.push(`- **Package:** \`${esc(f.name)}\` (npm, ${scope}${f.specs && f.specs.length ? `, declared spec \`${f.specs.map(esc).join('` `')}\`` : ''})`);
  lines.push(`- **Detected by:** ai-sdlc-vuln-feed scheduled sweep (issue #99) over \`${esc(opts.repo || '')}\` — source: \`${esc(f.source || 'dependency surface')}\``);
  lines.push(`- **Advisory:** https://osv.dev/vulnerability/${encodeURIComponent(f.id)}${ghsa ? ` · https://github.com/advisories/${encodeURIComponent(ghsa)}` : ''}`);
  if (aliases.length) lines.push(`- **Aliases:** ${aliases.map((a) => `\`${esc(a)}\``).join(', ')}`);
  lines.push(`- **Severity (OSV):** ${severity === 'unknown' ? 'unclassified' : severity}`);
  lines.push(`- **Fixed in:** ${fixed.length ? fixed.map((v) => `\`${esc(v)}\``).join(', ') : 'no fixed release published yet'}`);
  lines.push('');
  lines.push('Fix path: the dep-update stage (`ai-sdlc-dep-update`, `scripts/dep-update.js`) already vets candidate bumps through the same OSV gate — a vetted update moving `' + esc(f.name) + '` to a fixed version resolves this finding. Advisory and registry metadata are untrusted third-party input (spec/injection-defense.md §T3): quoted here as data only.');
  lines.push('');
  lines.push('## Acceptance criteria', '');
  const fixHint = fixed.length ? ` (e.g. \`>= ${esc(fixed[0])}\`)` : '';
  lines.push(`- [ ] \`${esc(f.name)}\` resolves to a version outside ${esc(f.id)}'s affected range${fixHint}, or the dependency is removed/patched`);
  lines.push(`- [ ] a vuln-feed rescan reports no remaining ${esc(f.id)} finding for \`${esc(f.name)}\``);
  lines.push('');
  lines.push(vulnMarker(f.id, f.name));
  return lines.join('\n') + '\n';
}

// ---------------------------------------------------------------------------
// Scan orchestration — client is injectable (ghClient() in the workflow,
// fakes in tests). Returns the run report; never throws on a missing
// surface (a repo without JS deps is a clean `skipped`, not a failure).
// ---------------------------------------------------------------------------

async function runScan(opts, client) {
  const repo = opts.repo;
  const maxFindings = Number.isFinite(opts.maxFindings) ? opts.maxFindings : DEFAULT_MAX_FINDINGS;
  const maxDeps = Number.isFinite(opts.maxDeps) ? opts.maxDeps : DEFAULT_MAX_DEPS;
  const maxDetails = Number.isFinite(opts.maxDetails) ? opts.maxDetails : DEFAULT_MAX_DETAILS;
  const dryRun = !!opts.dryRun;
  const labels = Array.isArray(opts.labels)
    ? opts.labels
    : String(opts.labels || DEFAULT_LABELS).split(',').map((s) => s.trim()).filter(Boolean);
  const fetchJson = opts.fetchJson || (async () => null);
  const report = {
    repo, action: 'clean', filed: [], duped: [], suppressed: [], deferred: 0,
    scanned: 0, findings: 0, unresolved: [], source: null, dry_run: dryRun,
  };

  // 1. Surface.
  const lockPath = opts.lockfile
    || (opts.dir ? path.join(opts.dir, 'package-lock.json') : null);
  const shrinkPath = opts.dir ? path.join(opts.dir, 'npm-shrinkwrap.json') : null;
  const manPath = opts.manifest
    || (opts.dir ? path.join(opts.dir, 'package.json') : null);
  let deps = [];
  if (lockPath && fs.existsSync(lockPath)) {
    report.source = path.basename(lockPath);
    deps = parseLockfileDeps(fs.readFileSync(lockPath, 'utf8'));
  } else if (shrinkPath && fs.existsSync(shrinkPath)) {
    report.source = 'npm-shrinkwrap.json';
    deps = parseLockfileDeps(fs.readFileSync(shrinkPath, 'utf8'));
  } else if (manPath && fs.existsSync(manPath)) {
    report.source = 'package.json';
    const resolved = await resolveManifestDeps(
      parseManifestDeps(fs.readFileSync(manPath, 'utf8')),
      (name) => fetchPackumentVersions(name, fetchJson, opts.registry));
    deps = resolved.deps;
    report.unresolved = resolved.unresolved;
  } else {
    report.action = 'skipped';
    report.reason = 'no dependency surface (no package-lock.json/npm-shrinkwrap.json/package.json)';
    return report;
  }
  deps.sort((a, b) => a.name.localeCompare(b.name) || String(a.version).localeCompare(String(b.version)));
  report.surfaceOverCap = Math.max(0, deps.length - maxDeps);
  deps = deps.slice(0, maxDeps);
  report.scanned = deps.length;
  if (!deps.length) {
    report.action = report.unresolved.length ? 'skipped' : 'clean';
    if (report.unresolved.length) report.reason = 'all declared specs unresolved';
    return report;
  }

  // 2. OSV batch (egress-bounded helper from dep-update.js).
  const osvMap = opts.osvMap !== undefined
    ? opts.osvMap
    : await depUpdate.queryOsv(deps.map((d) => ({ ecosystem: 'npm', name: d.name, version: d.version })), opts.egress);
  const findings = groupFindings(deps, osvMap);
  report.findings = findings.length;
  if (!findings.length) return report;   // clean

  // 3. Dedup against the issue markers (open suppresses; closed suppresses).
  const openIssues = client.listIssues({ repo, state: 'open' });
  const closedIssues = client.listIssues({ repo, state: 'closed' });
  const { file, dupes, suppressed } = dedupeFindings(findings, openIssues, closedIssues);
  report.duped = dupes.map((d) => ({ id: d.id, pkg: d.name }));
  report.suppressed = suppressed.map((s) => ({ id: s.finding.id, pkg: s.finding.name, issue: s.issue }));

  // 4. Bound + file. Details fetched only for candidates, ≤ maxDetails.
  const candidates = file.slice(0, maxDetails);
  const detailSkipped = file.length - candidates.length;
  for (const f of candidates) {
    f.detail = await fetchOsvDetail(f.id, fetchJson);
    f.source = report.source;
    f.severity = severityOf(f.detail);
  }
  candidates.sort((a, b) =>
    (SEVERITY_RANK[b.severity] - SEVERITY_RANK[a.severity])
    || a.id.localeCompare(b.id) || a.name.localeCompare(b.name));
  const toFile = candidates.slice(0, maxFindings);
  report.deferred = (candidates.length - toFile.length) + detailSkipped;

  for (const f of toFile) {
    const title = buildTitle(f);
    const body = buildIssueBody(f, { repo });
    if (dryRun) {
      report.filed.push({ dry_run: true, title, id: f.id, pkg: f.name });
      continue;
    }
    if (typeof client.ensureLabel === 'function') {
      for (const l of labels) client.ensureLabel({ repo, label: l });
    }
    const created = client.createIssue({ repo, title, body, labels });
    report.filed.push({ number: created.number, url: created.url, title, id: f.id, pkg: f.name });
  }
  if (report.filed.length) report.action = 'filed';
  else if (dupes.length || suppressed.length) report.action = 'deduped';
  return report;
}

// ---------------------------------------------------------------------------
// gh-backed client — same thin injectable surface as scripts/postmortem.js.
// ---------------------------------------------------------------------------

function ghClient() {
  const run = (args, allowFail = false) => {
    try {
      return execFileSync('gh', args, {
        encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 30000,
      });
    } catch (e) {
      if (allowFail) return '';
      throw e;
    }
  };
  const bodyFile = (body) => {
    const f = `/tmp/ai_sdlc_vulnfeed_${Date.now()}_${Math.random().toString(36).slice(2, 8)}.md`;
    fs.writeFileSync(f, body, 'utf8');
    return f;
  };
  return {
    listIssues({ repo, state }) {
      const out = run(['issue', 'list', '--repo', repo, '--state', state,
        '--limit', String(MAX_ISSUES),
        '--json', 'number,title,body,url,closedAt'], true);
      try { return out ? JSON.parse(out) : []; }
      catch (e) { console.warn(`[!] gh issue list (${state}) failed for ${repo} — treating as empty`); return []; }
    },
    ensureLabel({ repo, label }) {
      run(['label', 'create', label, '--repo', repo, '--force',
        '--color', 'B60205',
        '--description', 'ai-sdlc-vuln-feed advisory filing (issue #99)'], true);
    },
    createIssue({ repo, title, body, labels }) {
      const f = bodyFile(body);
      try {
        let out = '';
        const base = ['issue', 'create', '--repo', repo, '--title', title, '--body-file', f];
        try {
          out = run([...base, '--label', (labels || []).join(',')]);
        } catch (e) {
          console.warn(`[!] issue create with labels failed (${String(e.message).split('\n')[0]}) — retrying without labels`);
          out = run(base);
        }
        const m = out.trim().match(/\/issues\/(\d+)/);
        return { number: m ? parseInt(m[1], 10) : null, url: out.trim() };
      } finally { try { fs.unlinkSync(f); } catch (e) { /* ignore */ } }
    },
  };
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
    if (a === '--repo') opts.repo = next();
    else if (a === '--dir') opts.dir = next();
    else if (a === '--lockfile') opts.lockfile = next();
    else if (a === '--manifest') opts.manifest = next();
    else if (a === '--max-findings') opts.maxFindings = Number(next());
    else if (a === '--max-deps') opts.maxDeps = Number(next());
    else if (a === '--max-details') opts.maxDetails = Number(next());
    else if (a === '--egress-profile') opts.egressProfile = next();
    else if (a === '--egress-hosts') opts.egressHosts = next().split(',').map((s) => s.trim()).filter(Boolean);
    else if (a === '--registry') opts.registry = next();
    else if (a === '--labels') opts.labels = next();
    else if (a === '--report') opts.report = next();
    else if (a === '--offline') opts.offline = true;
    else if (a === '--dry-run') opts.dryRun = true;
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
    'usage: vuln-feed.js scan --repo OWNER/NAME',
    '         [--dir DIR | --lockfile FILE | --manifest FILE]',
    '         [--max-findings N] [--max-deps N] [--max-details N]',
    '         [--egress-profile npm-vet|off] [--egress-hosts h1,h2]',
    '         [--registry URL] [--labels csv] [--offline] [--dry-run]',
    '         [--report FILE] [--json] [--github-output]',
  ].join('\n');
}

async function main(argv, { stdout, client } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  if (opts.help || opts._[0] !== 'scan') {
    emit(usage());
    if (opts._[0] && opts._[0] !== 'scan') throw new Error(`unknown mode '${opts._[0]}'`);
    return null;
  }
  if (!opts.repo || !/^[-A-Za-z0-9_.]+\/[-A-Za-z0-9_.]+$/.test(opts.repo)) {
    throw new Error('scan requires --repo OWNER/NAME');
  }
  const egress = depUpdate.resolveEgress(
    opts.offline ? 'off' : opts.egressProfile, opts.egressHosts);
  const fetchJson = opts.offline ? async () => null : depUpdate.makeFetcher(egress);
  const report = await runScan(
    Object.assign({}, opts, { egress, fetchJson }),
    client || ghClient());
  if (opts.report) fs.writeFileSync(opts.report, JSON.stringify(report, null, 2) + '\n');
  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT, [
      `vuln_action=${report.action}`,
      `vuln_filed=${report.filed.length}`,
      `vuln_duped=${report.duped.length}`,
      `vuln_suppressed=${report.suppressed.length}`,
      `vuln_findings=${report.findings}`,
      `vuln_scanned=${report.scanned}`,
      '',
    ].join('\n'));
  }
  emit(JSON.stringify(report, null, 2));
  return report;
}

if (require.main === module) {
  main(process.argv.slice(2)).catch((e) => {
    console.error(`vuln-feed: ${e.message}`);
    process.exit(2);
  });
}

module.exports = {
  MARKER_PREFIX,
  MARKER_RE,
  vulnMarker,
  markersOf,
  parseVersion,
  cmpVersion,
  compileSpec,
  satisfies,
  maxSatisfying,
  parseLockfileDeps,
  parseManifestDeps,
  resolveManifestDeps,
  fetchPackumentVersions,
  fetchOsvDetail,
  severityOf,
  fixedVersionsOf,
  groupFindings,
  dedupeFindings,
  buildTitle,
  buildIssueBody,
  runScan,
  ghClient,
  parseArgs,
  main,
  SEVERITY_RANK,
  DEFAULT_MAX_FINDINGS,
};
