#!/usr/bin/env node
// scripts/dep-update.js — dependency update worker (issue #177, epic #150)
//
// Scheduled dependency scans that produce vetted dep-update PRs, ported from
// Hermes `dep_update_worker.js` + `dep_vetter.js`. Three pure building blocks:
//
//   1. scan   — parse `npm outdated --json` into update candidates
//               (patch/minor/major/unknown classification, `wanted` stays
//               inside declared semver ranges, `latest` may cross majors).
//   2. vet    — supply-chain gate per candidate target: minimum release age
//               (freshly-published packages are disproportionately
//               malicious), registry existence, license allowlist,
//               typosquat/name-confusion distance, OSV vulnerabilities.
//               Registry/OSV metadata is untrusted T3 input
//               (spec/injection-defense.md) and all egress is bounded by a
//               named egress profile (endpoint allowlist per stage).
//   3. plan   — bucket vetted candidates into `updates` (auto-kinds, clean
//               findings → one bounded consolidated PR), `flagged`
//               (majors, unknown specs, any finding → human review),
//               `deferred` (eligible but over the per-run cap). A batch
//               signature dedupes retries: a human-closed batch is not
//               re-proposed until versions drift.
//
// The module also owns the dep-diff classifier (Hermes issue #54):
// classifyDepUpdateDiff parses a unified diff bounded to dependency
// manifests/lockfiles into patch|minor|major|unknown bumps plus new/removed
// direct deps; depUpdateVerdict decides whether the diff qualifies for the
// low-risk path — fail-closed: majors, new deps, non-dep files and
// unparseable specs never take it. scripts/risk-classifier.js consumes the
// verdict so dep-update PRs carry a deterministic pr:risk-* mapping.
//
// CLI:
//   node scripts/dep-update.js plan [--outdated FILE | --dir DIR]
//        [--target wanted|latest] [--auto-kinds patch,minor] [--max-updates N]
//        [--min-age-days N] [--no-dev] [--egress-profile NAME]
//        [--egress-hosts h1,h2] [--offline] [--json] [--body-file F]
//   node scripts/dep-update.js classify --diff FILE [--json]
//   node scripts/dep-update.js vet-pr --repo owner/repo --pr N [--diff FILE]
//        [--min-age-days N] [--json]
//
// Env: GH_TOKEN (vet-pr gh calls), DEP_UPDATE_* defaults below.
'use strict';

const fs = require('fs');
const https = require('https');
const crypto = require('crypto');
const { spawnSync } = require('child_process');

const DEFAULT_MIN_AGE_DAYS = 7;
const DEFAULT_MAX_EDIT_DISTANCE = 2;
const DEFAULT_MAX_UPDATES = 10;
const DEFAULT_MAX_CANDIDATES = 50;
const LICENSE_ALLOWLIST = (
  'MIT,MIT-0,ISC,BSD-2-Clause,BSD-3-Clause,Apache-2.0,0BSD,CC0-1.0,Unlicense,' +
  'Python-2.0,PSF-2.0,Zlib,MPL-2.0,LGPL-2.1-or-later,LGPL-3.0-or-later,BlueOak-1.0.0'
).split(',');

// Named egress profiles (issue #177 AC: "vetting runs under named egress
// profiles — network allowlist per stage"). The profile bounds every host
// the vetter may fetch; `off` disables network entirely. Unknown names are
// fail-closed. Runner/network-level enforcement (NetworkPolicy,
// harden-runner) is the deployment's job — this is the contract surface the
// worker itself honors.
const EGRESS_PROFILES = {
  'npm-vet': ['registry.npmjs.org', 'api.osv.dev'],
  off: [],
};

const NPM_REGISTRY = 'https://registry.npmjs.org';
const OSV_QUERYBATCH = 'https://api.osv.dev/v1/querybatch';

// ---------------------------------------------------------------------------
// Semver helpers — fail-closed: anything non-semver classifies `unknown`.
// ---------------------------------------------------------------------------

const SEMVER_TRIPLE_RE = /^(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:([-+])[0-9A-Za-z.\-]+)?$/;

/** Reduce a version spec to {major, minor, patch, suffix} for comparison.
 * Range operators, a leading `v` and `npm:alias@` wrappers strip away;
 * non-semver specs (urls, git refs, `*`, `latest`) return null so callers
 * classify them `unknown` — never auto-eligible. */
function parseSemverTriple(spec) {
  let s = String(spec || '').trim().replace(/^["']+|["',]+$/g, '');
  s = s.replace(/^npm:[^@\s]+@/, '');
  s = s.replace(/^[\^~><=v\s]+/, '').trim();
  if (!s || /^(x|\*|latest|next|stable)$/i.test(s)) return null;
  if (/[:\/@]/.test(s)) return null;
  const m = s.match(SEMVER_TRIPLE_RE);
  if (!m) return null;
  return { major: +m[1], minor: +(m[2] || 0), patch: +(m[3] || 0), suffix: m[4] || '' };
}

/** Classify one version pair. `unknown` on unparseable specs or downgrades. */
function classifyVersionBump(from, to) {
  const a = parseSemverTriple(from);
  const b = parseSemverTriple(to);
  if (!a || !b) return 'unknown';
  if (a.major !== b.major) return b.major > a.major ? 'major' : 'unknown';
  if (a.minor !== b.minor) return b.minor > a.minor ? 'minor' : 'unknown';
  if (a.patch !== b.patch) return b.patch > a.patch ? 'patch' : 'unknown';
  return a.suffix !== b.suffix ? 'patch' : 'none';
}

// ---------------------------------------------------------------------------
// Scan — `npm outdated --json` parsing (npm exits 1 when updates exist, so
// callers pass stdout regardless of status).
// ---------------------------------------------------------------------------

function parseNpmOutdated(text, opts = {}) {
  const target = opts.target === 'latest' ? 'latest' : 'wanted';
  const includeDev = opts.includeDev !== undefined ? opts.includeDev : true;
  let data;
  try { data = JSON.parse(text); } catch (e) { return []; }
  if (!data || typeof data !== 'object') return [];
  const out = [];
  for (const [name, info] of Object.entries(data)) {
    if (!info || typeof info !== 'object') continue;
    const current = String(info.current || '');
    if (!current || /^(missing|linked|exotic|git)$/i.test(current)) continue;
    const dev = /dev/i.test(String(info.type || ''));
    if (dev && !includeDev) continue;
    const to = target === 'latest'
      ? (info.latest || info.wanted)
      : (info.wanted || info.latest);
    if (!to || String(to) === current) continue;
    out.push({
      ecosystem: 'npm', name, current,
      wanted: info.wanted || null, latest: info.latest || null,
      target: String(to), dev,
    });
  }
  // Stable order: prod deps first, then devDeps, alphabetical inside.
  out.sort((a, b) => (a.dev === b.dev ? a.name.localeCompare(b.name) : (a.dev ? 1 : -1)));
  return out;
}

// ---------------------------------------------------------------------------
// Dep-diff classification (Hermes dep_update_worker.js, issue #54) — pure +
// diff-bounded. Every file must be a dep manifest/lockfile, every extracted
// version change must classify, and unmatched adds/removes on direct
// manifests surface as newDeps/removedDeps.
// ---------------------------------------------------------------------------

const DEP_DIFF_FILE_RE = /(^|\/)(package\.json|package-lock\.json|npm-shrinkwrap\.json|yarn\.lock|pnpm-lock\.yaml|requirements[^/]*\.txt|pyproject\.toml|poetry\.lock|Pipfile(?:\.lock)?|go\.mod|go\.sum|Cargo\.toml|Cargo\.lock|Gemfile(?:\.lock)?|composer\.(?:json|lock))$/;
const DEP_DIRECT_MANIFEST_RE = /(^|\/)(package\.json|requirements[^/]*\.txt|pyproject\.toml|Pipfile|go\.mod|Cargo\.toml|Gemfile|composer\.json)$/;

const DEP_BUMP_RANK = { none: 0, patch: 1, minor: 2, major: 3, unknown: 4 };

// Manifest keys that are never dependency entries — scripts/metadata/
// lockfile-payload lines must not masquerade as deps.
const NON_DEP_MANIFEST_KEYS = new Set([
  'name', 'version', 'description', 'main', 'type', 'types', 'license', 'licence',
  'author', 'authors', 'contributors', 'maintainers', 'scripts', 'bin', 'man',
  'engines', 'exports', 'imports', 'files', 'keywords', 'publishconfig',
  'repository', 'homepage', 'bugs', 'funding', 'os', 'cpu', 'libc', 'workspaces',
  'packagemanager', 'private', 'preferunstable', 'dependencies', 'devdependencies',
  'peerdependencies', 'optionaldependencies', 'bundleddependencies',
  'bundledependencies', 'peerdependenciesmeta', 'resolutions', 'overrides',
  'requires', 'resolved', 'integrity', 'dev', 'optional', 'bundled', 'peer',
  'devoptional', 'inbundle', 'extraneous', 'requiresbuild', 'hasinstallscript',
  'deprecated', 'directories', 'config', 'readme', 'edition', 'rust-version',
  'requires-python', 'requires_python', 'python-versions', 'categories',
  'exclude', 'include', 'publish', 'resolver', 'source', 'channel', 'checksum',
  'platform', 'specifier', 'dist', 'tarball', 'shasum', 'markers', 'hashes',
  'resolution', 'transitive',
]);

const DEP_SPEC_VALUE_RE = /^[\^~><=v]*\s*v?\d|^\*|^x$|^(latest|next|stable)$/i;
const DEP_SPEC_REF_RE = /^(npm:|workspace:|file:|link:|portal:|patch:|https?:|git[+:]|github:|bitbucket:|svn:)/i;

/** Split a unified diff into per-file hunk streams. */
function splitDiffFiles(diffText) {
  const files = [];
  let cur = null, hunk = null;
  for (const raw of String(diffText || '').split('\n')) {
    const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw;
    const dm = line.match(/^diff --git a\/(.+?) b\/(.+)$/);
    if (dm) {
      cur = { path: dm[2], hunks: [] };
      files.push(cur);
      hunk = null;
      continue;
    }
    if (!cur) continue;
    if (line.startsWith('rename to ')) { cur.path = line.slice(10).trim(); continue; }
    if (line.startsWith('@@')) { hunk = { lines: [] }; cur.hunks.push(hunk); continue; }
    if (!hunk) continue;
    const sign = line[0];
    if (sign === '+' || sign === '-' || sign === ' ') hunk.lines.push({ sign, text: line.slice(1) });
  }
  return files;
}

function depFileKind(base) {
  if (base === 'package.json' || base === 'composer.json') return 'jsonKV';
  if (base === 'package-lock.json' || base === 'npm-shrinkwrap.json') return 'npmLock';
  if (base === 'yarn.lock') return 'yarnLock';
  if (base === 'pnpm-lock.yaml') return 'pnpmLock';
  if (base === 'composer.lock' || base === 'Pipfile.lock') return 'nameVerJson';
  if (base === 'Cargo.lock' || base === 'poetry.lock') return 'nameVerToml';
  if (base === 'Pipfile' || base === 'Cargo.toml') return 'tomlKV';
  if (base === 'pyproject.toml') return 'pyproject';
  if (base === 'go.mod') return 'goMod';
  if (base === 'go.sum') return 'goSum';
  if (base === 'Gemfile') return 'gemfile';
  if (base === 'Gemfile.lock') return 'parenVer';
  if (/^requirements[^/]*\.txt$/.test(base)) return 'requirements';
  return 'generic';
}

function depFileEcosystem(base) {
  if (/requirements[^/]*\.txt$|pyproject\.toml|poetry\.lock|Pipfile/.test(base)) return 'pip';
  if (/^go\.(mod|sum)$/.test(base)) return 'go';
  if (/^Cargo\./.test(base)) return 'cargo';
  if (/^Gemfile/.test(base)) return 'gem';
  if (/^composer\./.test(base)) return 'composer';
  return 'npm';
}

// Extractors return {name, version} (either may be null when the line can't
// prove both). ctx is per-hunk shared state for formats that bind a name on
// one line to a version on the next (lockfile `name:`/`version:` pairs).

const JSON_KV_DEP_LINE = /^\s*"((?:@[\w.-]+\/)?[\w.+_-]+)"\s*:\s*"([^"]*)"\s*,?\s*$/;
function jsonKvDepEntry(text) {
  const m = String(text).match(JSON_KV_DEP_LINE);
  if (!m) return null;
  if (NON_DEP_MANIFEST_KEYS.has(m[1].toLowerCase())) return null;
  const v = m[2].trim();
  if (!DEP_SPEC_VALUE_RE.test(v) && !DEP_SPEC_REF_RE.test(v)) return null;
  return { name: m[1], version: v || null };
}

const REQ_DEP_LINE = /^([A-Za-z0-9][\w.-]*)(?:\[[^\]]*\])?\s*(===|==|>=|<=|~=|!=|>|<)?\s*([A-Za-z0-9][\w.*+\-!]*)?/;
function reqDepEntry(text) {
  let t = String(text).trim();
  t = t.replace(/\s*;.*$/, '').replace(/\s+#.*$/, '').trim();
  if (!t || t[0] === '-' || t[0] === '#' || /^(https?|git|ssh|file)[:+]/i.test(t)) return null;
  const m = t.match(REQ_DEP_LINE);
  if (!m) return null;
  return { name: m[1], version: m[3] || null };
}

const NPM_LOCK_KEY_RE = /^\s*"(?:[^"]*?node_modules\/)?((?:@[\w.-]+\/)?[^"\/]*)"\s*:\s*\{/;
function npmLockEntry(text, ctx) {
  const t = String(text);
  let m = t.match(NPM_LOCK_KEY_RE);
  if (m) { ctx.name = m[1] || null; return null; }
  m = t.match(/^\s*"version"\s*:\s*"([^"]+)"/);
  if (m && ctx.name) return { name: ctx.name, version: m[1] };
  return jsonKvDepEntry(text);
}

function yarnLockEntry(text, ctx) {
  const t = String(text);
  if (/^\S/.test(t) && /:\s*$/.test(t)) {
    const h = t.match(/^["']?((?:@[\w.-]+\/)?[\w.+_-]+)@/);
    ctx.name = h ? h[1] : null;
    return null;
  }
  const m = t.match(/^\s+version\s*:?\s+"?([^"\s]+)"?\s*$/);
  return m && ctx.name ? { name: ctx.name, version: m[1] } : null;
}

function pnpmLockEntry(text, ctx) {
  const t = String(text);
  let m = t.match(/^\s*\/((?:@[\w.-]+\/)?[\w.+_-]+)@([\w.\-+]+?)(?:\([^)]*\))?:\s*$/);
  if (m) return { name: m[1], version: m[2] };
  m = t.match(/^\s*((?:@[\w.-]+\/)?[\w.+_-]+):\s*$/);
  if (m) { ctx.name = m[1]; return null; }
  m = t.match(/^\s+(?:specifier|version):\s+(\S+)\s*$/);
  if (m && ctx.name) return { name: ctx.name, version: m[1] };
  m = t.match(/^\s{2,}((?:@[\w.-]+\/)?[\w.+_-]+):\s+(\S+)\s*$/);
  if (m && !NON_DEP_MANIFEST_KEYS.has(m[1].toLowerCase()) && /^[\d~^><=v]/.test(m[2])) {
    return { name: m[1], version: m[2] };
  }
  return null;
}

function nameVerJsonEntry(text, ctx) {
  const t = String(text);
  let m = t.match(/"name"\s*:\s*"([^"]+)"/);
  if (m) { ctx.name = m[1]; return null; }
  m = t.match(/"version"\s*:\s*"([^"]+)"/);
  return m && ctx.name ? { name: ctx.name, version: m[1] } : null;
}

function nameVerTomlEntry(text, ctx) {
  const t = String(text);
  let m = t.match(/^\s*name\s*=\s*"([^"]+)"/);
  if (m) { ctx.name = m[1]; return null; }
  m = t.match(/^\s*version\s*=\s*"([^"]+)"/);
  return m && ctx.name ? { name: ctx.name, version: m[1] } : null;
}

function tomlKvDepEntry(text) {
  const t = String(text);
  let m = t.match(/^\s*["']?([A-Za-z0-9][\w.+_-]*)["']?\s*=\s*\{[^}]*?\bversion\s*=\s*"([^"]+)"/);
  if (m) return { name: m[1], version: m[2] };
  m = t.match(/^\s*["']?([A-Za-z0-9][\w.+_-]*)["']?\s*=\s*"([^"]*)"\s*,?\s*$/);
  if (!m) return null;
  if (NON_DEP_MANIFEST_KEYS.has(m[1].toLowerCase())) return null;
  const v = m[2].trim();
  if (!DEP_SPEC_VALUE_RE.test(v) && !DEP_SPEC_REF_RE.test(v)) return null;
  return { name: m[1], version: v || null };
}
const PEP508_DEP_RE = /["']([A-Za-z0-9][\w.-]*)(?:\[[^\]]*\])?\s*(?:(===|==|>=|<=|~=|!=|@|>|<)\s*([\w.!*+\-]*))?["']/;
function pyprojectEntry(text, ctx) {
  const kv = tomlKvDepEntry(text);
  if (kv) return kv;
  const m = String(text).match(PEP508_DEP_RE);
  if (!m) return null;
  return { name: m[1], version: m[3] || null };
}

function goModEntry(text) {
  const m = String(text).match(/^\s*(?:require\s+)?([A-Za-z0-9][\w.\-~\/]+)\s+(v[\w.\-+]+)\s*(?:\/\/.*)?$/);
  if (!m || m[1] === 'go' || m[1] === 'module' || m[1] === 'toolchain' || m[1] === 'replace') return null;
  return { name: m[1], version: m[2] };
}

function goSumEntry(text) {
  const m = String(text).match(/^\s*([A-Za-z0-9][\w.\-~\/]+)\s+(v[\w.\-+]+)(?:\/go\.mod)?\s+h1:/);
  return m ? { name: m[1], version: m[2] } : null;
}

function gemfileEntry(text) {
  const m = String(text).match(/\bgem\s+['"]([^'"]+)['"](?:\s*,\s*['"]([^'"]+)['"])?/);
  if (!m) return null;
  const vm = m[2] ? m[2].match(/\d[\w.\-]*/) : null;
  return { name: m[1], version: vm ? vm[0] : null };
}

function parenVerEntry(text) {
  const m = String(text).match(/^\s+([\w.+-]+)\s+\(([^)]+)\)\s*$/);
  if (!m) return null;
  const vm = m[2].match(/\d[\w.\-]*/);
  return { name: m[1], version: vm ? vm[0] : null };
}

function genericVerEntry(text) {
  const m = String(text).match(/v?\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.\-]+)?/);
  return m ? { name: null, version: m[0] } : null;
}

function depEntryForLine(text, kind, ctx) {
  switch (kind) {
    case 'jsonKV': return jsonKvDepEntry(text);
    case 'requirements': return reqDepEntry(text);
    case 'npmLock': return npmLockEntry(text, ctx);
    case 'yarnLock': return yarnLockEntry(text, ctx);
    case 'pnpmLock': return pnpmLockEntry(text, ctx);
    case 'nameVerJson': return nameVerJsonEntry(text, ctx);
    case 'nameVerToml': return nameVerTomlEntry(text, ctx);
    case 'tomlKV': return tomlKvDepEntry(text);
    case 'pyproject': return pyprojectEntry(text, ctx);
    case 'goMod': return goModEntry(text);
    case 'goSum': return goSumEntry(text);
    case 'gemfile': return gemfileEntry(text);
    case 'parenVer': return parenVerEntry(text);
    default: return genericVerEntry(text);
  }
}

/** Collect deduped removed/added dep entries for one file's hunks. Context
 * lines still run through the extractor — key-bearing context lines
 * (`"node_modules/express": {`) bind the `version` lines that follow. */
function collectFileDepEntries(hunks, kind) {
  const removed = [], added = [];
  for (const hunk of hunks) {
    const ctx = {};
    for (const l of hunk.lines) {
      const e = depEntryForLine(l.text, kind, ctx);
      if (l.sign === ' ' || !e) continue;
      (l.sign === '-' ? removed : added).push(e);
    }
  }
  const uniq = (list) => {
    const seen = new Set(), out = [];
    for (const e of list) {
      const k = `${e.name || ''}@${e.version || ''}`;
      if (seen.has(k)) continue;
      seen.add(k);
      out.push(e);
    }
    return out;
  };
  return { removed: uniq(removed), added: uniq(added) };
}

/** Pair same-name removed/added entries in order — a pair is a version bump,
 * an unmatched add is a new dep, an unmatched remove is a dropped dep.
 * Unnamed (generic) entries pair positionally. */
function pairDepEntries(hunks, kind) {
  const { removed, added } = collectFileDepEntries(hunks, kind);
  const updates = [], leftoverAdds = [], leftoverRems = [];
  const remByName = new Map();
  const unnamedRem = [];
  for (const e of removed) {
    if (e.name) {
      const q = remByName.get(e.name) || [];
      q.push(e);
      remByName.set(e.name, q);
    } else {
      unnamedRem.push(e);
    }
  }
  for (const e of added) {
    const q = e.name ? remByName.get(e.name) : unnamedRem;
    const old = q && q.length ? q.shift() : null;
    if (old) {
      if (String(old.version) !== String(e.version)) {
        updates.push({ name: e.name || null, from: old.version, to: e.version });
      }
    } else {
      leftoverAdds.push(e);
    }
  }
  for (const q of remByName.values()) leftoverRems.push(...q);
  leftoverRems.push(...unnamedRem);
  return { updates, leftoverAdds, leftoverRems };
}

/**
 * Classify a dependency-update PR diff: {kind, updates, newDeps,
 * removedDeps, depFiles, nonDepFiles}. `kind` is the highest-severity bump
 * across the diff: patch|minor|major|unknown|none.
 */
function classifyDepUpdateDiff(diffText) {
  const out = { kind: 'none', updates: [], newDeps: [], removedDeps: [], depFiles: [], nonDepFiles: [] };
  for (const f of splitDiffFiles(diffText)) {
    const base = f.path.split('/').pop();
    if (!DEP_DIFF_FILE_RE.test(f.path)) { out.nonDepFiles.push(f.path); continue; }
    out.depFiles.push(f.path);
    const kind = depFileKind(base);
    const eco = depFileEcosystem(base);
    const direct = DEP_DIRECT_MANIFEST_RE.test(f.path);
    const { updates, leftoverAdds, leftoverRems } = pairDepEntries(f.hunks, kind);
    // The same dep can surface twice in one file (specifier line + resolved
    // version in lockfiles) — collapse to the highest-severity pair per name.
    const byName = new Map();
    for (const u of updates) {
      u.kind = classifyVersionBump(u.from, u.to);
      const key = u.name || `v:${u.from}->${u.to}`;
      const prev = byName.get(key);
      if (!prev || DEP_BUMP_RANK[u.kind] > DEP_BUMP_RANK[prev.kind]) byName.set(key, u);
    }
    for (const u of byName.values()) {
      out.updates.push({ name: u.name, ecosystem: eco, file: f.path, from: u.from, to: u.to, kind: u.kind });
    }
    if (direct) {
      for (const d of leftoverAdds) out.newDeps.push({ name: d.name, version: d.version || null, ecosystem: eco, file: f.path });
      for (const d of leftoverRems) out.removedDeps.push({ name: d.name, version: d.version || null, ecosystem: eco, file: f.path });
    }
  }
  for (const u of out.updates) {
    if (DEP_BUMP_RANK[u.kind] > DEP_BUMP_RANK[out.kind]) out.kind = u.kind;
  }
  return out;
}

/**
 * Low-risk eligibility verdict for a dep-update diff. Eligible only when
 * the diff is bounded to dep manifests/lockfiles, every bump is
 * patch|minor, and no direct manifest gains a dependency. Every other
 * shape — majors, new deps, non-dep files, unclassifiable or empty diffs —
 * is ineligible and stays on the normal review+merge path (fail-closed).
 */
function depUpdateVerdict(cls) {
  const r = cls || {};
  const depFiles = r.depFiles || [];
  const updates = r.updates || [];
  const newDeps = r.newDeps || [];
  const nonDepFiles = r.nonDepFiles || [];
  const removedDeps = r.removedDeps || [];
  const base = { updates, newDeps, removedDeps, depFiles, nonDepFiles };
  if (!depFiles.length) {
    return Object.assign(base, { eligible: false, kind: r.kind || 'none', reason: 'no-dep-files' });
  }
  if (nonDepFiles.length) {
    return Object.assign(base, { eligible: false, kind: r.kind || 'unknown', reason: 'non-dep-files' });
  }
  if (newDeps.length) {
    return Object.assign(base, { eligible: false, kind: r.kind || 'unknown', reason: 'new-direct-deps' });
  }
  if (!updates.length) {
    return Object.assign(base, { eligible: false, kind: 'none', reason: 'no-version-bumps' });
  }
  const worst = updates.reduce((acc, u) =>
    (DEP_BUMP_RANK[u.kind] > DEP_BUMP_RANK[acc] ? u.kind : acc), 'none');
  if (worst === 'patch' || worst === 'minor') {
    return Object.assign(base, { eligible: true, kind: worst, reason: `${worst} bumps only, dep-files-only diff, no new direct deps` });
  }
  return Object.assign(base, { eligible: false, kind: worst, reason: `dep diff classified ${worst} — normal review path` });
}

// ---------------------------------------------------------------------------
// Manifest-diff dep extraction (Hermes dep_vetter.js) — added direct deps in
// a unified diff, for vetting new dependencies on arbitrary PRs.
// ---------------------------------------------------------------------------

const MANIFEST_RE = /(^|\/)(package\.json|requirements\.txt|requirements-.*\.txt|pyproject\.toml|Pipfile|go\.mod|Cargo\.toml|Gemfile|composer\.json|pom\.xml)$/;
const MANIFEST_FILES = new Set(['package.json', 'requirements.txt', 'pyproject.toml', 'Pipfile', 'go.mod', 'Cargo.toml', 'Gemfile', 'composer.json', 'pom.xml']);

function diffTouchesManifest(diffText) {
  for (const line of String(diffText || '').split('\n')) {
    const m = line.match(/^\+\+\+ b\/(.+)$/);
    if (m && (MANIFEST_RE.test(m[1]) || MANIFEST_FILES.has(m[1].split('/').pop()))) return true;
  }
  return false;
}

/** Extract added dependencies from a unified diff touching manifests. */
function extractAddedDeps(diffText) {
  const deps = [];
  let file = null;
  for (const line of String(diffText || '').split('\n')) {
    const fm = line.match(/^\+\+\+ b\/(.+)$/);
    if (fm) { file = fm[1]; continue; }
    if (!file || !line.startsWith('+') || line.startsWith('+++')) continue;
    const added = line.slice(1).trim();
    const base = file.split('/').pop();
    if (base === 'package.json') {
      const m = added.match(/^"((?:@[\w.-]+\/)?[\w.-]+)"\s*:\s*"([^"]+)"/);
      // Only dep-shaped VALUES count: inside a diff hunk we can't tell a
      // dependencies block from "scripts" — but script values are commands
      // ("node --test ..."), never version/registry specs.
      const SPEC = /^([\^~><=]?\s*\d|\*|latest|next|npm:|workspace:|file:|link:|https?:|git[+:]|github:)/i;
      if (m && !/^(name|version|description|main|type|license|author|scripts|bin|engines|exports|imports|files|keywords|publishConfig)$/i.test(m[1])
          && SPEC.test(m[2].trim())) {
        deps.push({ ecosystem: 'npm', name: m[1], version: m[2].replace(/^[\^~>=\s]+/, '') });
      }
    } else if (base === 'requirements.txt' || /^requirements-.*\.txt$/.test(base)) {
      const m = added.match(/^([A-Za-z0-9_.-]+)\s*(?:==|>=|<=|~=|>|<)?\s*([A-Za-z0-9_.*+-]*)/);
      if (m && !added.startsWith('#')) {
        deps.push({ ecosystem: 'pip', name: m[1], version: m[2] || null });
      }
    } else if (base === 'go.mod') {
      const m = added.match(/^([a-z0-9.-]+\.[a-z]+\/[\w./-]+)\s+(v[\w.-]+)/i);
      if (m) deps.push({ ecosystem: 'go', name: m[1], version: m[2] });
    } else if (base === 'Cargo.toml') {
      const m = added.match(/^([\w-]+)\s*=\s*"([^"]+)"/);
      if (m && !/^(name|version|edition|authors|description|license)$/i.test(m[1])) {
        deps.push({ ecosystem: 'rust', name: m[1], version: m[2].replace(/^[\^~>=\s]+/, '') });
      }
    }
  }
  return deps;
}

// ---------------------------------------------------------------------------
// Supply-chain vetting (Hermes dep_vetter.js) — release-age gate, registry
// existence, license allowlist, typosquat distance, OSV vulnerabilities.
// ---------------------------------------------------------------------------

// Curated popular-package baselines for typosquat/name-confusion distance.
const TOP_NAMES = {
  npm: ['react', 'react-dom', 'express', 'lodash', 'axios', 'moment', 'chalk', 'commander', 'webpack', 'typescript', 'eslint', 'prettier', 'jest', 'mocha', 'vue', 'angular', 'svelte', 'next', 'nuxt', 'vite', 'rollup', 'babel', 'mongoose', 'sequelize', 'pg', 'mysql', 'redis', 'socket.io', 'cors', 'dotenv', 'fs-extra', 'yargs', 'inquirer', 'ora', 'cheerio', 'puppeteer', 'playwright', 'nodemon', 'ts-node', 'uuid', 'async', 'request', 'node-fetch', 'got', 'minimist', 'semver', 'glob', 'rimraf', 'mkdirp'],
  pip: ['requests', 'numpy', 'pandas', 'django', 'flask', 'fastapi', 'sqlalchemy', 'pytest', 'boto3', 'urllib3', 'setuptools', 'wheel', 'pip', 'certifi', 'charset-normalizer', 'idna', 'python-dateutil', 'pytz', 'six', 'pyyaml', 'cryptography', 'pillow', 'matplotlib', 'scipy', 'scikit-learn', 'tensorflow', 'torch', 'transformers', 'openai', 'anthropic', 'httpx', 'aiohttp', 'click', 'rich', 'typer', 'pydantic', 'celery', 'redis', 'psycopg2', 'pymongo', 'alembic', 'uvicorn', 'gunicorn', 'black', 'ruff', 'mypy', 'poetry', 'tox'],
};

function levenshtein(a, b) {
  const m = a.length, n = b.length;
  if (!m) return n; if (!n) return m;
  const dp = Array.from({ length: m + 1 }, (_, i) => [i, ...Array(n).fill(0)]);
  for (let j = 1; j <= n; j++) dp[0][j] = j;
  for (let i = 1; i <= m; i++) {
    for (let j = 1; j <= n; j++) {
      dp[i][j] = Math.min(
        dp[i - 1][j] + 1,
        dp[i][j - 1] + 1,
        dp[i - 1][j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1)
      );
    }
  }
  return dp[m][n];
}

/** Find confusingly-similar popular names (edit distance ≤ threshold, not equal). */
function findNameConfusion(name, ecosystem, maxDist = DEFAULT_MAX_EDIT_DISTANCE) {
  const list = TOP_NAMES[ecosystem] || [];
  const hits = [];
  const low = name.toLowerCase();
  for (const pop of list) {
    if (pop === low) continue;
    if (Math.abs(pop.length - low.length) > maxDist) continue;
    const d = levenshtein(low, pop);
    if (d > 0 && d <= maxDist) hits.push({ similarTo: pop, distance: d });
  }
  return hits;
}

/**
 * Vet a single dep/update against policy. `dep` is
 * {name, version, ecosystem} (version = the TARGET version for updates);
 * `meta` is injected: {exists, publishedAt, license, vulns:[{id,summary}]}
 * — callers fetch it under the egress profile; tests stub it.
 * `opts.now` fixes the clock for the age gate.
 */
function vetUpdate(dep, meta, opts = {}) {
  const minAgeDays = opts.minAgeDays !== undefined ? opts.minAgeDays : DEFAULT_MIN_AGE_DAYS;
  const allowlist = opts.licenseAllowlist || LICENSE_ALLOWLIST;
  const now = opts.now !== undefined ? opts.now : Date.now();
  const findings = [];

  for (const hit of findNameConfusion(dep.name, dep.ecosystem, opts.maxEditDistance)) {
    findings.push({ kind: 'typosquat', severity: 'major', detail: `name "${dep.name}" is ${hit.distance} edit(s) from popular package "${hit.similarTo}" — possible typosquat` });
  }
  if (!meta) {
    findings.push({ kind: 'metadata-unavailable', severity: 'minor', detail: 'registry metadata unavailable — manual check advised' });
    return findings;
  }
  if (meta.exists === false) {
    findings.push({ kind: 'nonexistent', severity: 'blocker', detail: `package "${dep.name}" not found on its registry` });
    return findings;
  }
  if (meta.license && !allowlist.includes(meta.license)) {
    findings.push({ kind: 'license', severity: 'major', detail: `license "${meta.license}" not in allowlist` });
  }
  if (meta.publishedAt) {
    const ageDays = (now - new Date(meta.publishedAt).getTime()) / 86400000;
    if (Number.isFinite(ageDays) && ageDays < minAgeDays) {
      findings.push({ kind: 'fresh-release', severity: 'major', detail: `version published ${ageDays.toFixed(1)}d ago (< ${minAgeDays}d minimum release age)` });
    }
  }
  for (const v of meta.vulns || []) {
    findings.push({ kind: 'vulnerability', severity: 'blocker', detail: `${v.id}: ${v.summary || 'known vulnerability'}` });
  }
  return findings;
}

// ---------------------------------------------------------------------------
// Egress-bounded network fetchers — every URL's host must be in the
// resolved egress allowlist; blocked hosts resolve to null metadata so the
// caller flags the dep (fail-closed, never silently trusted).
// ---------------------------------------------------------------------------

function resolveEgress(profile, extraHosts) {
  const name = profile === undefined || profile === null || profile === '' ? 'npm-vet' : profile;
  if (!(name in EGRESS_PROFILES)) {
    throw new Error(`unknown egress profile '${name}' (allowed: ${Object.keys(EGRESS_PROFILES).join(', ')})`);
  }
  const hosts = new Set(EGRESS_PROFILES[name]);
  for (const h of extraHosts || []) if (h) hosts.add(h);
  return { profile: name, hosts };
}

function httpsJson(url, opts = {}) {
  return new Promise((resolve) => {
    const req = https.request(url, { method: opts.method || 'GET', timeout: 15000, headers: opts.body ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(opts.body) } : {} }, res => {
      let body = '';
      res.on('data', c => body += c);
      res.on('end', () => {
        if (res.statusCode === 404) return resolve({ __status: 404 });
        try { resolve(JSON.parse(body)); } catch (e) { resolve(null); }
      });
    });
    req.on('error', () => resolve(null));
    req.on('timeout', () => { req.destroy(); resolve(null); });
    if (opts.body) req.write(opts.body);
    req.end();
  });
}

function makeFetcher(egress) {
  return function fetchJson(url) {
    try {
      const host = new URL(url).hostname;
      if (!egress.hosts.has(host)) {
        console.error(`[egress] ${egress.profile}: blocked ${host} — not in profile allowlist`);
        return Promise.resolve(null);
      }
    } catch (e) { return Promise.resolve(null); }
    return httpsJson(url);
  };
}

/** npm registry metadata for one dep: {exists, license, publishedAt}. The
 * publishedAt is for the dep's TARGET version (dep.version). */
async function fetchDepMeta(dep, fetchJson, registryBase) {
  if (dep.ecosystem !== 'npm') return null; // v1: npm metadata only
  const base = registryBase || NPM_REGISTRY;
  const j = await fetchJson(`${base}/${encodeURIComponent(dep.name).replace('%40', '@')}`);
  if (!j) return null;
  if (j.__status === 404 || j.error) return { exists: false };
  const ver = dep.version && j.time && j.time[dep.version] ? dep.version : (j['dist-tags'] && j['dist-tags'].latest);
  return {
    exists: true,
    license: (j.versions && ver && j.versions[ver] && (typeof j.versions[ver].license === 'string' ? j.versions[ver].license : j.versions[ver].license && j.versions[ver].license.type)) || j.license || null,
    publishedAt: j.time && ver ? j.time[ver] : null,
  };
}

const OSV_ECOSYSTEM = { npm: 'npm', pip: 'PyPI', go: 'Go', rust: 'crates.io' };

function httpsJsonPost(url, body) {
  return new Promise((resolve) => {
    let req;
    try { req = https.request(url, { method: 'POST', timeout: 15000, headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) } }, res => {
      let data = '';
      res.on('data', c => data += c);
      res.on('end', () => { try { resolve(JSON.parse(data)); } catch (e) { resolve(null); } });
    }); } catch (e) { return resolve(null); }
    req.on('error', () => resolve(null));
    req.on('timeout', () => { req.destroy(); resolve(null); });
    req.write(body);
    req.end();
  });
}

/** OSV batch query — {ecosystem:name@version → vulns[]}. `egress` bounds the
 * endpoint host like every other fetch. */
async function queryOsv(deps, egress) {
  const queries = deps
    .filter(d => OSV_ECOSYSTEM[d.ecosystem] && d.version)
    .map(d => ({ package: { name: d.name, ecosystem: OSV_ECOSYSTEM[d.ecosystem] }, version: d.version }));
  const map = new Map();
  if (!queries.length) return map;
  if (!egress.hosts.has(new URL(OSV_QUERYBATCH).hostname)) return map;
  const res = await httpsJsonPost(OSV_QUERYBATCH, JSON.stringify({ queries }));
  (res && res.results || []).forEach((r, i) => {
    const q = queries[i];
    map.set(`${q.package.ecosystem}:${q.package.name}@${q.version}`, (r.vulns || []).map(v => ({ id: v.id, summary: v.summary })));
  });
  return map;
}

// ---------------------------------------------------------------------------
// Plan — candidates → vetted update set + flagged set. Fail-closed: any
// finding (blocker/major/minor) or a kind outside `autoKinds` flags the
// update for human review instead of landing it.
// ---------------------------------------------------------------------------

/** Stable batch signature — dedupes retries: a human-closed batch is not
 * re-proposed until versions drift. */
function depSignature(updates) {
  const sig = updates.map(u => `${u.name}@${u.target}`).sort().join(',');
  return crypto.createHash('sha256').update(sig).digest('hex').slice(0, 16);
}

function mdEscape(s) {
  // Dep names/versions flow into the PR body from package manifests and
  // registry metadata — untrusted input (spec/injection-defense.md T1/T3).
  // Neutralize markdown/html metacharacters and cap length.
  return String(s == null ? '' : s)
    .replace(/[<>`|\\\[\]]/g, c => '\\' + c)
    .slice(0, 120);
}

/**
 * Build the update plan. `candidates` come from parseNpmOutdated;
 * `fetchMeta` is injectable (async dep → meta) so tests never hit npm.
 * `osvMap` is a Map key `${eco}:${name}@${version}` → vulns[] (injectable).
 * Returns {sig, updates, flagged, deferred, scanned, riskTier, egress}.
 */
async function buildPlan(candidates, opts = {}) {
  const autoKinds = opts.autoKinds || ['patch', 'minor'];
  const maxUpdates = opts.maxUpdates || DEFAULT_MAX_UPDATES;
  const maxCandidates = opts.maxCandidates || DEFAULT_MAX_CANDIDATES;
  const fetchMeta = opts.fetchMeta || (async () => null);
  const osvMap = opts.osvMap || new Map();
  const vetOpts = { minAgeDays: opts.minAgeDays, licenseAllowlist: opts.licenseAllowlist, maxEditDistance: opts.maxEditDistance, now: opts.now };

  const updates = [], flagged = [], deferred = [];
  const scanned = candidates.slice(0, maxCandidates);
  const overScanCap = candidates.length - scanned.length;
  for (const c of scanned) {
    const kind = classifyVersionBump(c.current, c.target);
    const meta = await fetchMeta(c);
    if (meta) {
      const key = `${OSV_ECOSYSTEM[c.ecosystem] || c.ecosystem}:${c.name}@${c.target}`;
      meta.vulns = osvMap.get(key) || meta.vulns || [];
    }
    const findings = vetUpdate({ ecosystem: c.ecosystem, name: c.name, version: c.target }, meta, vetOpts);
    const entry = Object.assign({}, c, { kind, findings });
    entry.meta = meta ? {
      exists: meta.exists !== false,
      license: meta.license || null,
      publishedAt: meta.publishedAt || null,
      vulns: meta.vulns || [],
    } : null;
    if (!autoKinds.includes(kind) || findings.length) {
      flagged.push(entry);
    } else if (updates.length < maxUpdates) {
      updates.push(entry);
    } else {
      deferred.push(entry);
    }
  }
  if (overScanCap > 0) {
    // Candidates beyond the scan bound are deferred wholesale — never
    // dropped silently, never auto-applied.
    deferred.push(...candidates.slice(maxCandidates).map(c => ({ name: c.name, kind: 'unscanned', deferred: true })));
  }
  return {
    sig: depSignature(updates),
    updates,
    flagged,
    deferred,
    scanned: scanned.length,
    candidates: candidates.length,
    // The PR diff only carries `updates` — vetted patch|minor bumps on dep
    // files, so the mapped taxonomy tier is low (spec/risk-taxonomy.md;
    // scripts/risk-classifier.js dep fast-path computes the same label).
    riskTier: updates.length ? 'low' : 'none',
  };
}

/** PR body for a consolidated dep-update batch — vetting verdict table +
 * flagged-for-human section + dedup marker. */
function buildPrBody(plan, opts = {}) {
  const minAge = opts.minAgeDays !== undefined ? opts.minAgeDays : DEFAULT_MIN_AGE_DAYS;
  const lines = [];
  lines.push('## Vetted dependency updates');
  lines.push('');
  lines.push('Opened by the scheduled dep-update worker (issue #177). Every bump below passed the supply-chain gate — release age, registry existence, license allowlist, typosquat distance, OSV vulnerabilities.');
  lines.push('');
  lines.push('| Package | From | To | Kind | Published | Vet |');
  lines.push('|---|---|---|---|---|---|');
  for (const u of plan.updates) {
    const pub = u.meta && u.meta.publishedAt ? u.meta.publishedAt.slice(0, 10) : 'n/a';
    const lic = u.meta && u.meta.license ? ` · ${mdEscape(u.meta.license)}` : '';
    lines.push(`| \`${mdEscape(u.name)}\`${u.dev ? ' (dev)' : ''} | ${mdEscape(u.current)} | ${mdEscape(u.target)} | ${u.kind} | ${pub} | clean${lic} |`);
  }
  if (plan.flagged.length) {
    lines.push('');
    lines.push('### Flagged for human review — **not applied** in this PR');
    lines.push('');
    for (const f of plan.flagged) {
      const why = [];
      if (!['patch', 'minor'].includes(f.kind)) why.push(`${f.kind} bump`);
      for (const g of f.findings || []) why.push(`${g.severity}: ${mdEscape(g.detail)}`);
      lines.push(`- \`${mdEscape(f.name)}\` ${mdEscape(f.current)} → ${mdEscape(f.target)} — ${why.join('; ') || 'policy flag'}`);
    }
    lines.push('');
    lines.push('Flagged updates require a human-driven bump (or a follow-up PR) — the worker never lands majors, unknown specs, or updates with vetting findings.');
  }
  if (plan.deferred && plan.deferred.length) {
    lines.push('');
    lines.push(`${plan.deferred.length} eligible update(s) deferred past this run's bound — picked up by a later scan.`);
  }
  lines.push('');
  lines.push('---');
  lines.push(`vetting: release-age ≥ ${minAge}d · registry existence · license allowlist · typosquat distance · OSV · upstream registry content is treated as untrusted input (spec/injection-defense.md)`);
  lines.push(`<!-- ai-sdlc:dep-update sig=${plan.sig} v=1 -->`);
  return lines.join('\n') + '\n';
}

// ---------------------------------------------------------------------------
// vet-pr — vet the added direct deps of an arbitrary PR diff (Dependabot
// supersede path): extract → fetch meta → findings → verdict + tier.
// ---------------------------------------------------------------------------

async function vetPrDeps(diffText, opts = {}) {
  if (!diffTouchesManifest(diffText)) return { touched: false, checked: [], violations: [], tier: null, flagHuman: false };
  const deps = extractAddedDeps(diffText);
  if (!deps.length) return { touched: true, checked: [], violations: [], tier: null, flagHuman: false };
  const fetchMeta = opts.fetchMeta || (async () => null);
  const osvMap = opts.osvMap || new Map();
  const violations = [];
  for (const dep of deps) {
    const meta = await fetchMeta(dep);
    if (meta) {
      const key = `${OSV_ECOSYSTEM[dep.ecosystem] || dep.ecosystem}:${dep.name}@${dep.version}`;
      meta.vulns = osvMap.get(key) || meta.vulns || [];
    }
    const findings = vetUpdate(dep, meta, opts);
    if (findings.length) violations.push({ dep, findings });
  }
  const worst = violations.flatMap(v => v.findings).some(f => f.severity === 'blocker') ? 'blocker'
    : violations.length ? 'major' : 'clean';
  return {
    touched: true,
    checked: deps,
    violations,
    // Taxonomy mapping: clean vetted new deps → medium ("new dependencies
    // (vetted)"); any finding → high + human flag.
    tier: worst === 'clean' ? 'medium' : 'high',
    flagHuman: worst !== 'clean',
  };
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

function gh(argsArr, timeoutMs = 60000) {
  const res = spawnSync('gh', argsArr.map(String), { encoding: 'utf8', timeout: timeoutMs, maxBuffer: 64 * 1024 * 1024 });
  if (res.error) throw res.error;
  if (res.status !== 0) throw new Error(`gh failed (${res.status}): ${(res.stderr || '').slice(0, 300)}`);
  return (res.stdout || '').trim();
}

function runOutdated(dir) {
  const res = spawnSync('npm', ['outdated', '--json'], { encoding: 'utf8', cwd: dir || '.', timeout: 120000, maxBuffer: 32 * 1024 * 1024 });
  // npm exits 1 when updates exist — stdout is valid either way.
  return res.stdout || '{}';
}

function parseArgs(argv) {
  const opts = { _: [], json: true };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--outdated') opts.outdated = next();
    else if (a === '--dir') opts.dir = next();
    else if (a === '--diff') opts.diff = next();
    else if (a === '--repo') opts.repo = next();
    else if (a === '--pr') opts.pr = next();
    else if (a === '--target') opts.target = next();
    else if (a === '--auto-kinds') opts.autoKinds = next().split(',').map(s => s.trim()).filter(Boolean);
    else if (a === '--max-updates') opts.maxUpdates = Number(next());
    else if (a === '--max-candidates') opts.maxCandidates = Number(next());
    else if (a === '--min-age-days') opts.minAgeDays = Number(next());
    else if (a === '--no-dev') opts.includeDev = false;
    else if (a === '--egress-profile') opts.egressProfile = next();
    else if (a === '--egress-hosts') opts.egressHosts = next().split(',').map(s => s.trim()).filter(Boolean);
    else if (a === '--registry') opts.registry = next();
    else if (a === '--offline') opts.offline = true;
    else if (a === '--body-file') opts.bodyFile = next();
    else if (a === '--help' || a === '-h') { opts.help = true; }
    else if (a.startsWith('--')) throw new Error(`unknown argument: ${a}`);
    else opts._.push(a);
  }
  return opts;
}

function usage() {
  return [
    'usage: dep-update.js plan [--outdated FILE|--dir DIR] [--target wanted|latest]',
    '                          [--auto-kinds patch,minor] [--max-updates N] [--min-age-days N]',
    '                          [--no-dev] [--egress-profile npm-vet|off] [--egress-hosts h1,h2]',
    '                          [--offline] [--json] [--body-file F]',
    '       dep-update.js classify --diff FILE [--json]',
    '       dep-update.js vet-pr --repo owner/repo --pr N [--diff FILE] [--min-age-days N] [--json]',
  ].join('\n');
}

async function cmdPlan(opts) {
  let outdatedText;
  if (opts.outdated) outdatedText = fs.readFileSync(opts.outdated, 'utf8');
  else outdatedText = runOutdated(opts.dir || '.');
  const candidates = parseNpmOutdated(outdatedText, { target: opts.target, includeDev: opts.includeDev });

  const egress = resolveEgress(opts.offline ? 'off' : opts.egressProfile, opts.egressHosts);
  const registry = opts.registry || NPM_REGISTRY;
  const fetchJson = makeFetcher(egress);
  const fetchMeta = (dep) => fetchDepMeta(dep, fetchJson, registry);
  const osvMap = opts.offline ? new Map()
    : await queryOsv(candidates.map(c => ({ ecosystem: c.ecosystem, name: c.name, version: c.target })), egress);

  const plan = await buildPlan(candidates, {
    autoKinds: opts.autoKinds,
    maxUpdates: opts.maxUpdates,
    maxCandidates: opts.maxCandidates,
    minAgeDays: opts.minAgeDays,
    fetchMeta,
    osvMap,
  });
  plan.egress = egress.profile;
  plan.target = opts.target === 'latest' ? 'latest' : 'wanted';
  plan.body = buildPrBody(plan, { minAgeDays: opts.minAgeDays });
  plan.action = plan.updates.length ? 'open-pr' : (plan.flagged.length ? 'flagged-only' : 'none');
  if (opts.bodyFile) fs.writeFileSync(opts.bodyFile, plan.body);
  return plan;
}

async function cmdVetPr(opts) {
  const diff = opts.diff !== undefined
    ? fs.readFileSync(opts.diff, 'utf8')
    : gh(['pr', 'diff', String(opts.pr), '--repo', opts.repo], 120000);
  const egress = resolveEgress(opts.offline ? 'off' : opts.egressProfile, opts.egressHosts);
  const fetchJson = makeFetcher(egress);
  const fetchMeta = (dep) => fetchDepMeta(dep, fetchJson, opts.registry || NPM_REGISTRY);
  const deps = extractAddedDeps(diff);
  const osvMap = opts.offline ? new Map() : await queryOsv(deps, egress);
  const verdict = await vetPrDeps(diff, { fetchMeta, osvMap, minAgeDays: opts.minAgeDays });
  verdict.diffClass = classifyDepUpdateDiff(diff);
  return verdict;
}

async function main(argv, { stdout } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  const mode = opts._[0] || 'plan';
  if (opts.help) { emit(usage()); return null; }

  let result;
  if (mode === 'plan') {
    result = await cmdPlan(opts);
  } else if (mode === 'classify') {
    if (!opts.diff) throw new Error('classify requires --diff FILE');
    const cls = classifyDepUpdateDiff(fs.readFileSync(opts.diff, 'utf8'));
    result = { classification: cls, verdict: depUpdateVerdict(cls) };
  } else if (mode === 'vet-pr') {
    if (!opts.repo || !opts.pr) throw new Error('vet-pr requires --repo owner/repo --pr N (or --diff FILE)');
    if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(opts.repo)) throw new Error(`bad --repo '${opts.repo}'`);
    result = await cmdVetPr(opts);
  } else {
    throw new Error(`unknown mode '${mode}'\n${usage()}`);
  }
  emit(JSON.stringify(result, null, 2));
  return result;
}

if (require.main === module) {
  main(process.argv.slice(2)).catch(e => {
    console.error(`dep-update: ${e.message}`);
    process.exit(2);
  });
}

module.exports = {
  parseSemverTriple,
  classifyVersionBump,
  parseNpmOutdated,
  splitDiffFiles,
  classifyDepUpdateDiff,
  depUpdateVerdict,
  diffTouchesManifest,
  extractAddedDeps,
  levenshtein,
  findNameConfusion,
  vetUpdate,
  resolveEgress,
  makeFetcher,
  fetchDepMeta,
  queryOsv,
  depSignature,
  buildPlan,
  buildPrBody,
  vetPrDeps,
  runOutdated,
  parseArgs,
  main,
  DEP_DIFF_FILE_RE,
  DEP_DIRECT_MANIFEST_RE,
  DEP_BUMP_RANK,
  EGRESS_PROFILES,
  TOP_NAMES,
  LICENSE_ALLOWLIST,
};
