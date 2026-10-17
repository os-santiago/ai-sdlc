#!/usr/bin/env node
// scripts/spec-version.js — spec/v* tagging machinery (issue #134)
//
// The documented contract (spec/README.md, AGENTS.md: "a breaking schema
// change = MAJOR bump of spec/v* tags + migration note") becomes code here:
//
//   1. classify   — the spec/** diff since the newest spec/v* tag is
//      classified semantically: removed/renamed field or tightened
//      constraint = MAJOR, additive field/schema/doc = MINOR, docs-only or
//      annotation-only edits = PATCH. A `type!:`/`BREAKING CHANGE` commit
//      touching spec/ still forces MAJOR even when the tree diff looks
//      additive (prose semantics a structural diff cannot see).
//   2. verify     — spec/README.md declares the version (`**Declared
//      version:** spec/vX.Y.Z`); it must equal the computed next tag, so a
//      spec-changing PR carries its version bump in the same diff.
//   3. tag        — `--publish` annotates + pushes the tag and opens a
//      GitHub release whose notes carry the classification and, on MAJOR,
//      the migration-note template.
//
// Tag machinery is reused from scripts/release-manager.js (tag listing,
// spec-touching commit selection, conventional-commit bump, semver math) —
// this script adds the spec-diff classifier and the declared-version gate
// on top, it does not reimplement them.
//
// Bootstrap: when no spec/v* tag exists yet there is nothing to diff — the
// declared version IS the first tag (e.g. spec/v1.0.0) and the run is a
// planned initial release.
//
// Bounded: pure string/JSON work over bounded git output — no engine, no
// model. `--publish` performs a fixed call sequence (tag → push → release
// create); every subprocess goes through one injectable exec(argv) seam so
// tests run with zero real tags.
//
// CLI:
//   node scripts/spec-version.js [--repo OWNER/NAME] [--ref REF] [--base TAG]
//        [--bump auto|major|minor|patch] [--cwd DIR] [--remote NAME]
//        [--readme FILE] [--no-verify-declared] [--dry-run|--publish]
//        [--json] [--github-output]
//
// Default is a dry run — nothing is tagged or pushed unless --publish is
// passed. Exit: 0 ok (planned|released|exists|no-changes) · 2 error
// (including declared-version mismatch — the gate is fail-closed).
'use strict';

const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const {
  SPEC_TAG_RE,
  SPEC_GLOB,
  computeBump,
  latestTag,
  commitsSince,
  specShasSince,
  nextVersion,
} = require('./release-manager');

const SPEC_TAG_GLOB = 'spec/v*.*.*';
const DECLARED_RE = /\*\*Declared version:\*\*\s*`?spec\/v(\d+)\.(\d+)\.(\d+)`?/i;
const BUMPS = ['auto', 'major', 'minor', 'patch'];
const RANK = { patch: 1, minor: 2, major: 3 };
const RANK_NAME = ['none', 'patch', 'minor', 'major'];
const MAX_REASONS = 100;

const maxRank = (a, b) => Math.max(a || 0, b || 0);
const rankName = (r) => RANK_NAME[r] || 'none';

// --- Schema-diff classification -------------------------------------------------

// Keywords that carry no validation semantics — edits to them are editorial.
const ANNOTATION_KEYS = new Set([
  'title', 'description', '$comment', 'examples', 'default', 'deprecated',
  'readOnly', 'writeOnly', '$anchor', 'markdownDescription',
]);
// Object-valued keywords whose per-entry semantics differ:
//   ADD_MINOR  — a new entry is additive (new optional surface)
//   ADD_MAJOR  — a new entry is a new constraint (tightens validation)
// ('properties' is handled by diffProperties — a new prop is MAJOR when the
// parent `required` names it, MINOR when optional.)
const PROP_MAP_ADD_MINOR = new Set(['$defs', 'definitions']);
const PROP_MAP_ADD_MAJOR = new Set(['patternProperties', 'dependentSchemas']);
// Schema/subschema-valued constraint keywords: added = major, removed =
// minor, both-present = recursive diff (or scalar change = major).
const CONSTRAINT_KEYS = new Set([
  'not', 'if', 'then', 'else', 'propertyNames', 'contains', 'items',
  'unevaluatedItems', 'contentSchema', 'pattern', 'format', 'contentMediaType',
  'contentEncoding',
]);
const MIN_BOUNDS = new Set([
  'minimum', 'exclusiveMinimum', 'minLength', 'minItems', 'minProperties',
  'minContains',
]);
const MAX_BOUNDS = new Set([
  'maximum', 'exclusiveMaximum', 'maxLength', 'maxItems', 'maxProperties',
  'maxContains',
]);

const isObj = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
const isSchema = (v) => isObj(v) || typeof v === 'boolean';

function deepEqual(a, b) {
  if (a === b) return true;
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((v, i) => deepEqual(v, b[i]));
  }
  if (isObj(a) && isObj(b)) {
    const ka = Object.keys(a); const kb = Object.keys(b);
    return ka.length === kb.length && ka.every((k) => deepEqual(a[k], b[k]));
  }
  return false;
}

const normSet = (v) => new Set(Array.isArray(v) ? v.map((x) => JSON.stringify(x)) : [JSON.stringify(v)]);

function note(out, level, msg) {
  if (out.reasons.length < MAX_REASONS) out.reasons.push({ level: rankName(level), path: msg });
}

function diffKeyList(oldObj, newObj, ptr, out) {
  // 'required' / dependency-name lists: added entries tighten, removed loosen.
  const ov = oldObj || []; const nv = newObj || [];
  const oset = new Set(ov); const nset = new Set(nv);
  let rank = 0;
  for (const k of nv) {
    if (!oset.has(k)) { note(out, 3, `${ptr}: '${k}' became required`); rank = 3; }
  }
  for (const k of ov) {
    if (!nset.has(k)) { note(out, 2, `${ptr}: '${k}' no longer required`); rank = maxRank(rank, 2); }
  }
  return rank;
}

function diffPropMap(ov, nv, additiveRank, ptr, out) {
  const oldMap = ov || {}; const newMap = nv || {};
  let rank = 0;
  for (const k of Object.keys(newMap)) {
    const p = `${ptr}/${k}`;
    if (!Object.hasOwn(oldMap, k)) {
      note(out, additiveRank, `${p}: added`);
      rank = maxRank(rank, additiveRank);
    } else if (isSchema(oldMap[k]) && isSchema(newMap[k])) {
      rank = maxRank(rank, diffSchema(oldMap[k], newMap[k], p, out));
    } else {
      rank = maxRank(rank, deepEqual(oldMap[k], newMap[k]) ? 0 : 3);
      if (rank === 3) note(out, 3, `${p}: changed`);
    }
  }
  for (const k of Object.keys(oldMap)) {
    if (!Object.hasOwn(newMap, k)) {
      note(out, 3, `${ptr}/${k}: removed`);
      rank = 3;
    }
  }
  return rank;
}

function diffCombiner(ov, nv, kind, ptr, out) {
  // kind 'widen': branch added widens acceptance (minor), removed narrows
  // (major). kind 'tighten': branch added is a new constraint (major).
  if (!Array.isArray(ov) || !Array.isArray(nv)) return deepEqual(ov, nv) ? 0 : 3;
  const len = Math.min(ov.length, nv.length);
  let rank = 0;
  for (let i = 0; i < len; i += 1) {
    rank = maxRank(rank, diffSchema(ov[i], nv[i], `${ptr}[${i}]`, out));
  }
  if (nv.length > ov.length) {
    const r = kind === 'tighten' ? 3 : 2;
    note(out, r, `${ptr}: ${nv.length - ov.length} branch(es) added`);
    rank = maxRank(rank, r);
  } else if (nv.length < ov.length) {
    const r = kind === 'tighten' ? 2 : 3;
    note(out, r, `${ptr}: ${ov.length - nv.length} branch(es) removed`);
    rank = maxRank(rank, r);
  }
  return rank;
}

function diffSchema(oldNode, newNode, ptr, out) {
  if (deepEqual(oldNode, newNode)) return 0;
  if (typeof oldNode === 'boolean' || typeof newNode === 'boolean' || !isObj(oldNode) || !isObj(newNode)) {
    note(out, 3, `${ptr}: schema node kind changed`);
    return 3;
  }
  let rank = 0;
  const keys = new Set([...Object.keys(oldNode), ...Object.keys(newNode)]);
  for (const key of keys) {
    const p = ptr === '#' ? `#/${key}` : `${ptr}/${key}`;
    const hasOld = Object.hasOwn(oldNode, key);
    const hasNew = Object.hasOwn(newNode, key);
    const ov = oldNode[key]; const nv = newNode[key];
    const r = classifyKey(key, ov, nv, hasOld, hasNew, newNode, p, out);
    rank = maxRank(rank, r);
  }
  return rank;
}

/* eslint-disable-next-line complexity */
function classifyKey(key, ov, nv, hasOld, hasNew, newObj, ptr, out) {
  if (ANNOTATION_KEYS.has(key) || key.startsWith('x-')) {
    if (hasOld && hasNew && deepEqual(ov, nv)) return 0;
    return 1; // annotation edit — editorial
  }

  if (key === 'properties') return diffProperties(ov, nv, newObj, ptr, out);
  if (PROP_MAP_ADD_MINOR.has(key)) return diffPropMap(ov, nv, 2, ptr, out);
  if (PROP_MAP_ADD_MAJOR.has(key)) return diffPropMap(ov, nv, 3, ptr, out);

  switch (key) {
    case 'required':
      return diffKeyList(ov, nv, ptr, out);
    case 'dependentRequired':
    case 'dependencies': {
      // per-key: added dep → major; removed → minor; value diff recurses.
      const oldMap = ov || {}; const newMap = nv || {};
      let rank = 0;
      for (const k of Object.keys(newMap)) {
        if (!Object.hasOwn(oldMap, k)) { note(out, 3, `${ptr}/${k}: dependency added`); rank = 3; }
        else if (Array.isArray(newMap[k])) {
          rank = maxRank(rank, diffKeyList(oldMap[k], newMap[k], `${ptr}/${k}`, out));
        } else {
          rank = maxRank(rank, diffSchema(oldMap[k], newMap[k], `${ptr}/${k}`, out));
        }
      }
      for (const k of Object.keys(oldMap)) {
        if (!Object.hasOwn(newMap, k)) { note(out, 2, `${ptr}/${k}: dependency removed`); rank = maxRank(rank, 2); }
      }
      return rank;
    }
    case 'enum': {
      if (!hasOld) { note(out, 3, `${ptr}: enum constraint added`); return 3; }
      if (!hasNew) { note(out, 2, `${ptr}: enum constraint removed`); return 2; }
      const oset = normSet(ov); const nset = normSet(nv);
      let rank = 0;
      for (const v of nset) if (!oset.has(v)) { rank = maxRank(rank, 2); }
      for (const v of oset) if (!nset.has(v)) { note(out, 3, `${ptr}: enum value removed`); rank = 3; }
      if (rank === 2) note(out, 2, `${ptr}: enum value added`);
      return rank;
    }
    case 'const':
      if (!hasOld) { note(out, 3, `${ptr}: const constraint added`); return 3; }
      if (!hasNew) { note(out, 2, `${ptr}: const removed`); return 2; }
      if (!deepEqual(ov, nv)) { note(out, 3, `${ptr}: const changed`); return 3; }
      return 0;
    case 'type': {
      if (!hasOld) { note(out, 3, `${ptr}: type constraint added`); return 3; }
      if (!hasNew) { note(out, 2, `${ptr}: type constraint removed`); return 2; }
      const oset = normSet(ov); const nset = normSet(nv);
      const added = [...nset].filter((v) => !oset.has(v));
      const removed = [...oset].filter((v) => !nset.has(v));
      if (!added.length && !removed.length) return 0;
      if (removed.length) { note(out, 3, `${ptr}: type narrowed`); return 3; }
      note(out, 2, `${ptr}: type widened`);
      return 2;
    }
    case 'anyOf':
    case 'oneOf':
      return diffCombiner(ov, nv, 'widen', ptr, out);
    case 'allOf':
      return diffCombiner(ov, nv, 'tighten', ptr, out);
    case 'additionalProperties':
    case 'unevaluatedProperties': {
      if (!hasOld || ov === true) {
        return nv === false || isObj(nv) ? (note(out, 3, `${ptr}: extra properties closed`), 3) : 1;
      }
      if (!hasNew || nv === true) { note(out, 2, `${ptr}: extra properties opened`); return 2; }
      if (isObj(ov) && isObj(nv)) return diffSchema(ov, nv, ptr, out);
      return deepEqual(ov, nv) ? 0 : 3;
    }
    case 'uniqueItems':
      if (!hasOld) return nv === true ? (note(out, 3, `${ptr}: uniqueItems added`), 3) : 1;
      if (!hasNew) return ov === true ? (note(out, 2, `${ptr}: uniqueItems removed`), 2) : 1;
      if (ov !== nv) return nv === true ? 3 : 2;
      return 0;
    case 'prefixItems':
      return diffCombiner(ov, nv, 'tighten', ptr, out);
    default:
      break;
  }

  if (MIN_BOUNDS.has(key)) {
    if (!hasOld) { note(out, 3, `${ptr}: lower bound added`); return 3; }
    if (!hasNew) { note(out, 2, `${ptr}: lower bound removed`); return 2; }
    if (nv > ov) { note(out, 3, `${ptr}: lower bound raised ${ov}→${nv}`); return 3; }
    if (nv < ov) { note(out, 2, `${ptr}: lower bound relaxed ${ov}→${nv}`); return 2; }
    return 0;
  }
  if (MAX_BOUNDS.has(key)) {
    if (!hasOld) { note(out, 3, `${ptr}: upper bound added`); return 3; }
    if (!hasNew) { note(out, 2, `${ptr}: upper bound removed`); return 2; }
    if (nv < ov) { note(out, 3, `${ptr}: upper bound lowered ${ov}→${nv}`); return 3; }
    if (nv > ov) { note(out, 2, `${ptr}: upper bound relaxed ${ov}→${nv}`); return 2; }
    return 0;
  }

  if (CONSTRAINT_KEYS.has(key)) {
    if (!hasOld) { note(out, 3, `${ptr}: constraint '${key}' added`); return 3; }
    if (!hasNew) { note(out, 2, `${ptr}: constraint '${key}' removed`); return 2; }
    if (isSchema(ov) && isSchema(nv)) return diffSchema(ov, nv, ptr, out);
    if (!deepEqual(ov, nv)) { note(out, 3, `${ptr}: constraint '${key}' changed`); return 3; }
    return 0;
  }

  // Unknown keyword — fail-safe defaults: removal or any deep change could
  // be semantic (major); pure addition is treated as additive (minor).
  if (!hasOld) { note(out, 2, `${ptr}: keyword added`); return 2; }
  if (!hasNew) { note(out, 3, `${ptr}: keyword removed`); return 3; }
  if (isSchema(ov) && isSchema(nv)) return diffSchema(ov, nv, ptr, out);
  if (!deepEqual(ov, nv)) { note(out, 3, `${ptr}: keyword changed`); return 3; }
  return 0;
}

// A `properties` addition is MINOR when optional, MAJOR when the parent
// `required` (in the NEW node) names it. Wrap the map diff, then upgrade any
// added-property reason whose key is required in the new node.
function diffProperties(ov, nv, newObj, ptr, out) {
  const before = out.reasons.length;
  const rank = diffPropMap(ov, nv, 2, ptr, out);
  const req = new Set((newObj && newObj.required) || []);
  let upgraded = 0;
  for (let i = before; i < out.reasons.length; i += 1) {
    const r = out.reasons[i];
    const m = r.path.match(new RegExp(`^${escapeRe(ptr)}/([^/]+): added$`));
    if (m && r.level === 'minor' && req.has(m[1])) {
      r.level = 'major';
      r.path = `${ptr}/${m[1]}: added as required property`;
      upgraded += 1;
    }
  }
  return upgraded ? 3 : rank;
}

function escapeRe(s) { return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); }

// --- File-level classification ----------------------------------------------------

/**
 * Classify one name-status change under spec/.
 *   D → major (normative surface removed)
 *   R → major (renamed path breaks consumers fetching by URL)
 *   A → minor (additive surface)
 *   M .json → schema diff walk
 *   M .md   → patch (prose is editorial to a structural classifier —
 *             conventional commits or --bump escalate a normative
 *             prose change the diff cannot see)
 *   M other → minor (unknown format — additive assumption)
 */
function classifyFileChange(exec, cwd, base, ref, change, out) {
  const { status, path: p, oldPath } = change;
  if (status === 'D') return { file: p, level: 'major', detail: 'deleted' };
  if (status === 'R') return { file: p, level: 'major', detail: `renamed from ${oldPath}` };
  if (status === 'A') return { file: p, level: 'minor', detail: 'added' };

  if (!p.endsWith('.json')) {
    const level = /\.(md|markdown|txt)$/i.test(p) ? 'patch' : 'minor';
    return { file: p, level, detail: 'modified (non-schema → editorial heuristic)' };
  }

  const oldText = String(exec(['git', '-C', cwd, 'show', `${base}:${p}`]));
  const newText = String(exec(['git', '-C', cwd, 'show', `${ref}:${p}`]));
  let oldJson; let newJson;
  try { oldJson = JSON.parse(oldText); newJson = JSON.parse(newText); } catch (e) {
    return { file: p, level: 'major', detail: `unparseable JSON diff — fail-safe (${e.message})` };
  }
  const before = out.reasons.length;
  const rank = diffSchema(oldJson, newJson, '#', out);
  const reasons = out.reasons.splice(before).map((r) => `${r.level} ${r.path}`);
  return { file: p, level: rankName(rank), detail: reasons.slice(0, 10).join('; ') || 'no semantic change' };
}

function parseNameStatus(text) {
  return String(text || '')
    .split('\n').map((l) => l.trim()).filter(Boolean)
    .map((l) => {
      const parts = l.split('\t');
      const code = parts[0][0];
      if (code === 'R' || code === 'C') return { status: code, oldPath: parts[1], path: parts[2] };
      return { status: code, path: parts[1] };
    });
}

function specChanges(exec, cwd, base, ref) {
  if (!base) {
    // No spec tag yet — the whole spec/ tree is new surface.
    const out = exec(['git', '-C', cwd, 'ls-tree', '-r', '--name-only', ref, '--', SPEC_GLOB]);
    return String(out || '').split('\n').map((s) => s.trim()).filter(Boolean)
      .map((p) => ({ status: 'A', path: p }));
  }
  const out = exec(['git', '-C', cwd, 'diff', '--name-status', `${base}..${ref}`, '--', SPEC_GLOB]);
  return parseNameStatus(out);
}

// --- Declared version --------------------------------------------------------------

function readDeclared(readmePath) {
  const text = fs.readFileSync(readmePath, 'utf8');
  const m = text.match(DECLARED_RE);
  if (!m) return null;
  return { version: m.slice(1).map(Number), tag: `spec/v${m.slice(1).join('.')}` };
}

// --- Plan ---------------------------------------------------------------------------

function planSpec(exec, opts) {
  const cwd = opts.cwd;
  const ref = opts.ref || 'HEAD';
  const base = opts.base !== undefined
    ? (opts.base ? { name: opts.base, version: (opts.base.match(SPEC_TAG_RE) || []).slice(1).map(Number) } : null)
    : latestTag(exec, cwd, SPEC_TAG_GLOB, SPEC_TAG_RE);
  if (opts.base && !base.version.length) throw new Error(`--base '${opts.base}' is not a spec/vX.Y.Z tag`);

  const declared = opts.verifyDeclared === false
    ? { skipped: true }
    : readDeclared(opts.readme || path.join(cwd, 'spec', 'README.md'));

  const changes = specChanges(exec, cwd, base && base.name, ref);
  const out = { reasons: [] };
  const files = changes.map((c) => classifyFileChange(exec, cwd, base && base.name, ref, c, out));

  let fileRank = 0;
  for (const f of files) fileRank = maxRank(fileRank, RANK[f.level] || 0);
  const diffBump = rankName(fileRank);

  // Commit signal: conventional-commit bump over the spec-touching commits.
  // A BREAKING CHANGE footer beats a structural diff that cannot read prose.
  let commitBump = null;
  if (base && changes.length) {
    const shas = specShasSince(exec, cwd, base.name, ref);
    const specCommits = commitsSince(exec, cwd, base.name, ref).filter((c) => shas.has(c.sha));
    if (specCommits.length) commitBump = computeBump(specCommits);
  }

  if (!changes.length) {
    return {
      status: 'no-changes', repo: opts.repo || null, base_tag: base ? base.name : null,
      ref, bump: null, tag: null, declared: declared || null, verified: true,
      files: [], reasons: [],
    };
  }

  if (!base) {
    // Bootstrap: nothing to diff against — the declared version IS the
    // first tag. Any override still applies.
    if (!declared || declared.skipped) {
      if (opts.bump && opts.bump !== 'auto') {
        const v = nextVersion(null, opts.bump);
        return planResult(opts, null, ref, opts.bump, `spec/v${v.join('.')}`, declared, files, diffBump, commitBump, true);
      }
      throw new Error('no spec/v* tag exists and spec/README.md declares no ' +
        'version — add "**Declared version:** `spec/vX.Y.Z`" (or pass --no-verify-declared --bump B)');
    }
    return planResult(opts, null, ref, 'initial', declared.tag, declared, files, diffBump, commitBump, true);
  }

  const computed = maxRank(RANK[diffBump] || 0, RANK[commitBump] || 0) || RANK.patch;
  const bump = (opts.bump && opts.bump !== 'auto') ? opts.bump : RANK_NAME[computed];
  const tag = `spec/v${nextVersion(base.version, bump).join('.')}`;
  const verified = !declared || declared.skipped ? null : declared.tag === tag;
  return planResult(opts, base.name, ref, bump, tag, declared, files, diffBump, commitBump, verified);
}

function planResult(opts, baseTag, ref, bump, tag, declared, files, diffBump, commitBump, verified) {
  return {
    status: verified === false ? 'declared-mismatch' : 'planned',
    repo: opts.repo || null,
    base_tag: baseTag,
    ref,
    bump,
    diff_bump: diffBump,
    commit_bump: commitBump,
    tag,
    declared: declared || null,
    verified,
    files,
  };
}

// --- Publish ----------------------------------------------------------------------

function renderSpecNotes(plan) {
  const lines = [`## ${plan.tag}`, '',
    `Spec release — bump: **${plan.bump}**` +
    (plan.base_tag ? ` (base \`${plan.base_tag}\`)` : ' (initial spec tag)'), '',
    '### Classification', ''];
  for (const f of plan.files.slice(0, 50)) lines.push(`- \`${f.file}\` — ${f.level}${f.detail ? ` (${f.detail})` : ''}`);
  if (plan.files.length > 50) lines.push(`- … and ${plan.files.length - 50} more`);
  lines.push('');
  if (plan.bump === 'major') {
    lines.push(
      '### Migration note', '',
      '> A MAJOR spec bump requires a migration note (AGENTS.md). Fill before announcing:', '',
      '- **What breaks:** ', '- **Consumer action required:** ', '- **Engine/runtime impact:** ', '',
    );
  }
  if (plan.repo) {
    lines.push(plan.base_tag
      ? `**Full Changelog**: https://github.com/${plan.repo}/compare/${plan.base_tag}...${plan.tag}`
      : `**Spec**: https://github.com/${plan.repo}/tree/${plan.tag}/spec`);
  }
  return `${lines.join('\n')}\n`;
}

function publishSpec(exec, opts, plan) {
  const cwd = opts.cwd;
  const ref = opts.ref || 'HEAD';
  const remote = opts.remote || 'origin';

  const exists = tryExec(exec, ['git', '-C', cwd, 'rev-parse', '-q', '--verify', `refs/tags/${plan.tag}`]);
  if (exists.ok && String(exists.out).trim()) {
    return { status: 'exists', note: `tag ${plan.tag} already exists — nothing to do` };
  }

  exec(['git', '-C', cwd, 'tag', '-a', plan.tag, '-m',
    `${plan.tag} — spec release (${plan.bump})`, ref]);
  exec(['git', '-C', cwd, 'push', remote, `refs/tags/${plan.tag}`]);

  let url = null;
  if (opts.repo) {
    const rel = exec([
      'gh', 'api', '-X', 'POST', `repos/${opts.repo}/releases`,
      '-f', `tag_name=${plan.tag}`,
      '-f', `target_commitish=${ref}`,
      '-f', `name=${plan.tag}`,
      '-f', `body=${renderSpecNotes(plan)}`,
      '-F', 'draft=false',
      '-F', 'prerelease=false',
      '-F', 'generate_release_notes=false',
    ]);
    try { url = JSON.parse(rel).html_url || null; } catch (e) { /* non-JSON gh output */ }
  }
  return { status: 'released', url, pushed: true };
}

// --- Subprocess seam ----------------------------------------------------------------

function defaultExec(argv) {
  const [bin, ...rest] = argv.map(String);
  if (bin !== 'git' && bin !== 'gh') {
    throw new Error(`refused: non-allowlisted binary "${bin}"`);
  }
  return execFileSync(bin, rest, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function tryExec(exec, argv) {
  try {
    return { ok: true, out: exec(argv) };
  } catch (e) {
    return { ok: false, out: '', error: e };
  }
}

// --- CLI ------------------------------------------------------------------------------

function usage() {
  return [
    'usage: spec-version.js [--repo OWNER/NAME] [--ref REF] [--base TAG]',
    '       [--bump auto|major|minor|patch] [--cwd DIR] [--remote NAME]',
    '       [--readme FILE] [--no-verify-declared] [--dry-run|--publish]',
    '       [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv, env) {
  const opts = {
    bump: 'auto', ref: 'HEAD', remote: 'origin', publish: false,
    json: false, githubOutput: false, verifyDeclared: true,
    repo: (env && env.GITHUB_REPOSITORY) || '', cwd: '', base: undefined,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--ref') opts.ref = next();
    else if (a === '--base') opts.base = next();
    else if (a === '--bump') opts.bump = next();
    else if (a === '--cwd') opts.cwd = next();
    else if (a === '--remote') opts.remote = next();
    else if (a === '--readme') opts.readme = next();
    else if (a === '--no-verify-declared') opts.verifyDeclared = false;
    else if (a === '--publish') opts.publish = true;
    else if (a === '--dry-run') opts.publish = false;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  if (!BUMPS.includes(opts.bump)) throw new Error(`invalid --bump '${opts.bump}'`);
  return opts;
}

function main(argv, { exec, stdout, env } = {}) {
  const e = exec || defaultExec;
  const runEnv = env || process.env;
  const opts = parseArgs(argv, runEnv);
  const emit = stdout || ((s) => console.log(s));

  const cwd = opts.cwd || String(e(['git', 'rev-parse', '--show-toplevel']) || '').trim();
  if (!cwd) throw new Error('not inside a git work tree — pass --cwd DIR');

  const plan = planSpec(e, { ...opts, cwd });

  if (plan.status === 'declared-mismatch') {
    throw new Error(
      `declared version gate failed: spec/README.md declares ${plan.declared.tag} ` +
      `but the computed next tag is ${plan.tag} (diff=${plan.diff_bump}` +
      `${plan.commit_bump ? `, commits=${plan.commit_bump}` : ''}). ` +
      'Update the "**Declared version:**" line in spec/README.md to match the ' +
      'classification, or pass --bump/--no-verify-declared to override.',
    );
  }

  let result = { ...plan };
  if (opts.publish && plan.status === 'planned') {
    result = { ...plan, ...publishSpec(e, { ...opts, cwd }, plan) };
  } else if (opts.publish && plan.status === 'no-changes') {
    result = { ...plan, note: 'spec/ unchanged since base — nothing to tag' };
  } else {
    result = { ...plan, dry_run: true };
  }

  if (opts.githubOutput && runEnv.GITHUB_OUTPUT) {
    const lines = [
      `spec_status=${result.status}`,
      `spec_tag=${result.tag || ''}`,
      `spec_bump=${result.bump || ''}`,
      `spec_base_tag=${result.base_tag || ''}`,
      `spec_declared=${(result.declared && result.declared.tag) || ''}`,
      `spec_verified=${result.verified === null || result.verified === undefined ? '' : result.verified}`,
      `spec_release_url=${result.url || ''}`,
    ];
    fs.appendFileSync(runEnv.GITHUB_OUTPUT, `${lines.join('\n')}\n`);
  }

  if (opts.json) emit(JSON.stringify(result, null, 2));
  else if (result.status === 'no-changes') emit(`[i] spec/ unchanged since ${result.base_tag || 'start'} — nothing to tag`);
  else emit(`${result.dry_run ? '[dry-run] ' : ''}${result.status}: ${result.tag}` +
    ` (bump=${result.bump}, base=${result.base_tag || 'none'}` +
    `${result.declared && result.declared.tag ? `, declared=${result.declared.tag}` : ''})` +
    (result.url ? ` — ${result.url}` : ''));

  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`spec-version: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  SPEC_TAG_GLOB,
  DECLARED_RE,
  ANNOTATION_KEYS,
  deepEqual,
  diffSchema,
  classifyKey,
  diffPropMap,
  diffProperties,
  classifyFileChange,
  parseNameStatus,
  specChanges,
  readDeclared,
  planSpec,
  renderSpecNotes,
  publishSpec,
  parseArgs,
  main,
};
