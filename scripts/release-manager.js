#!/usr/bin/env node
// scripts/release-manager.js — semver release driver (issue #178)
//
// Releases as a declared pipeline capability: computes the next semver tag
// from conventional commits since the last release tag, renders a changelog
// from git history, then (on --publish) creates the annotated tag, pushes
// it, and opens the GitHub release via `gh api`. Ported from Hermes
// `scripts/auto_release.py` / `scripts/release_manager.js` into the
// git-history shape this repo uses (no version files to bump — the
// package.json here is a harness manifest pinned at 0.0.0).
//
// Two tag streams exist by design (AGENTS.md: "spec changes are
// semver-sensitive — a breaking schema change = MAJOR bump of spec/v* tags
// + migration note"):
//
//   vX.Y.Z       — repo/runtime release line (drives the GitHub release)
//   spec/vX.Y.Z  — normative-spec line; bumped only when a commit in the
//                  range touches spec/. Its bump is computed from the
//                  spec-touching commits alone, so a `feat(runtime):` commit
//                  never inflates the spec version and a `feat!(spec):`
//                  breaking change always forces spec MAJOR.
//
// Bump rules (conventional commits — https://www.conventionalcommits.org):
//   major — any commit with `BREAKING CHANGE`/`BREAKING-CHANGE` in the body
//           or a `type(scope)!:` subject
//   minor — at least one `feat` commit
//   patch — everything else (fix, perf, docs, ci, chore, ...)
//
// Provenance wiring: the manager emits the released tag + changelog so the
// caller (ai-sdlc-release.yml) can package `git archive` artifacts, upload
// them to the release, and attest them via actions/attest-build-provenance
// (SLSA L3-class, issue #100). The manager itself never signs — cosign
// keyless signing lives in the workflow's least-privilege `provenance` job
// (issue #103), gated on release_status == 'released'.
//
// Bounded: pure string assembly over bounded git output — no engine, no
// model. `--publish` performs a fixed call sequence (verify → tag → push →
// release create); every subprocess goes through one injectable exec(argv)
// seam so tests run with zero real releases.
//
// CLI:
//   node scripts/release-manager.js [--repo OWNER/NAME] [--bump B]
//        [--spec-bump auto|major|minor|patch|none] [--ref REF]
//        [--remote NAME] [--cwd DIR] [--dry-run|--publish] [--force]
//        [--changelog-file FILE] [--json] [--github-output]
//
// Default is a dry run — nothing is tagged, pushed, or released unless
// --publish is passed. Exit: 0 ok (status planned|released|no-changes) ·
// 2 error.
'use strict';

const fs = require('fs');
const { execFileSync } = require('child_process');

// --- Conventional-commit / tag grammar ----------------------------------------

const CONVENTIONAL_RE = /^([A-Za-z]+)(\([^)]*\))?(!)?\s*:\s*(.+)$/;
const RUNTIME_TAG_RE = /^v(\d+)\.(\d+)\.(\d+)$/;
const SPEC_TAG_RE = /^spec\/v(\d+)\.(\d+)\.(\d+)$/;
const BREAKING_BODY_RE = /BREAKING[ -]CHANGE/i;
const SPEC_GLOB = 'spec/';

const TYPE_KIND = {
  feat: 'feat', feature: 'feat',
  fix: 'fix', hotfix: 'fix', bugfix: 'fix',
  perf: 'perf',
  docs: 'docs',
  refactor: 'refactor',
  test: 'test', tests: 'test',
  ci: 'ci', build: 'ci',
  chore: 'chore', style: 'chore',
  revert: 'other',
};
const SECTIONS = [
  ['breaking', '⚠ Breaking Changes'],
  ['feat', 'Features'],
  ['fix', 'Bug Fixes'],
  ['perf', 'Performance'],
  ['refactor', 'Refactoring'],
  ['docs', 'Documentation'],
  ['test', 'Tests'],
  ['ci', 'CI & Build'],
  ['chore', 'Chores'],
  ['other', 'Other Changes'],
];
const BUMPS = ['auto', 'major', 'minor', 'patch'];

function parseConventional(subject) {
  const m = String(subject || '').trim().match(CONVENTIONAL_RE);
  if (!m) return null;
  return {
    type: m[1].toLowerCase(),
    scope: m[2] ? m[2].slice(1, -1) : null,
    breaking: m[3] === '!',
    subject: m[4].trim(),
  };
}

/** 'breaking' | changelog kind — subject wins; body carries BREAKING CHANGE. */
function classifyCommit(commit) {
  const conv = parseConventional(commit.subject);
  if ((conv && conv.breaking) || BREAKING_BODY_RE.test(commit.body || '')) {
    return 'breaking';
  }
  if (conv && TYPE_KIND[conv.type]) return TYPE_KIND[conv.type];
  return 'other';
}

/** any breaking → major · any feat → minor · else patch. */
function computeBump(commits, override) {
  if (override && override !== 'auto') return override;
  const kinds = new Set((commits || []).map(classifyCommit));
  if (kinds.has('breaking')) return 'major';
  if (kinds.has('feat')) return 'minor';
  return 'patch';
}

function parseTag(name) {
  const spec = String(name || '').trim().match(SPEC_TAG_RE);
  if (spec) return { spec: true, version: spec.slice(1).map(Number) };
  const rt = String(name || '').trim().match(RUNTIME_TAG_RE);
  if (rt) return { spec: false, version: rt.slice(1).map(Number) };
  return null;
}

function compareVersions(a, b) {
  for (let i = 0; i < 3; i += 1) {
    if (a[i] !== b[i]) return a[i] - b[i];
  }
  return 0;
}

function nextVersion(base, bump) {
  const v = base || [0, 0, 0];
  if (bump === 'major') return [v[0] + 1, 0, 0];
  if (bump === 'minor') return [v[0], v[1] + 1, 0];
  return [v[0], v[1], v[2] + 1];
}

const fmtTag = (v, spec) => `${spec ? 'spec/' : ''}v${v.join('.')}`;

// --- git log parsing -----------------------------------------------------------

// `git log --format=%H%x00%s%x00%b%x1e` — NUL fields, RS (\x1e) records.
// Commit payloads can't carry either byte, so the split is lossless.
function parseGitLog(text) {
  return String(text || '')
    .split('\x1e')
    .map((rec) => rec.trim())
    .filter(Boolean)
    .map((rec) => {
      const [sha, subject, ...rest] = rec.split('\x00');
      return { sha: String(sha || '').trim(), subject: subject || '', body: rest.join('\x00') };
    })
    .filter((c) => c.sha);
}

// --- Changelog (pure) ----------------------------------------------------------

function groupedSections(commits) {
  const grouped = {};
  for (const [kind] of SECTIONS) grouped[kind] = [];
  for (const c of commits) grouped[classifyCommit(c)].push(c);
  return grouped;
}

function commitLine(c) {
  return `* ${c.subject} (${String(c.sha).slice(0, 7)})`;
}

function renderSections(commits) {
  const grouped = groupedSections(commits);
  const lines = [];
  for (const [kind, heading] of SECTIONS) {
    if (!grouped[kind].length) continue;
    lines.push(`### ${heading}`, '');
    for (const c of grouped[kind]) lines.push(commitLine(c));
    lines.push('');
  }
  if (!lines.length) lines.push('_No changes._', '');
  return lines.join('\n').trimEnd();
}

function renderChangelog({ repo, tag, baseTag, ref, commits, spec }) {
  const lines = ['## What\'s Changed', '', renderSections(commits), ''];
  if (spec && spec.touched) {
    lines.push(
      `### Spec`,
      '',
      `Normative spec changed in this range — tagging \`${spec.tag}\` ` +
      `(${spec.bump} bump of \`${spec.baseTag || 'none'}\`). ` +
      (spec.bump === 'major'
        ? '⚠ Breaking spec change: a migration note is required per AGENTS.md.'
        : ''),
      '',
    );
  }
  if (repo && baseTag) {
    lines.push(`**Full Changelog**: https://github.com/${repo}/compare/${baseTag}...${tag}`);
  } else if (repo) {
    lines.push(`**Full Changelog**: https://github.com/${repo}/commits/${ref || 'HEAD'}`);
  }
  return `${lines.join('\n').replace(/\n{3,}/g, '\n\n')}\n`;
}

// --- Subprocess seam (injectable for tests) -------------------------------------

// exec(argv) -> stdout string; throws on non-zero. argv[0] selects the
// binary from a fixed allowlist — never caller-controlled (SAST).
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

// --- Release computation ---------------------------------------------------------

/** Newest tag matching `pattern`+`re` by semver order; null when none. */
function latestTag(exec, cwd, pattern, re) {
  const r = tryExec(exec, ['git', '-C', cwd, 'tag', '--list', pattern]);
  if (!r.ok) return null;
  let best = null;
  for (const line of String(r.out).split('\n')) {
    const name = line.trim();
    const m = name.match(re);
    if (!m) continue;
    const v = m.slice(1).map(Number);
    if (!best || compareVersions(v, best.version) > 0) best = { name, version: v };
  }
  return best;
}

function logRange(baseTag, ref) {
  return baseTag ? `${baseTag}..${ref}` : ref;
}

function commitsSince(exec, cwd, baseTag, ref) {
  const r = exec(['git', '-C', cwd, 'log', '--format=%H%x00%s%x00%b%x1e', logRange(baseTag, ref)]);
  return parseGitLog(r);
}

/** SHAs of commits in the range that touched spec/ (spec-tag stream input). */
function specShasSince(exec, cwd, baseTag, ref) {
  const r = exec(['git', '-C', cwd, 'log', '--format=%H', logRange(baseTag, ref), '--', SPEC_GLOB]);
  return new Set(String(r || '').split('\n').map((s) => s.trim()).filter(Boolean));
}

/**
 * Build the full release plan without mutating anything.
 * Returns {status, base_tag, tag, bump, commits, spec, changelog}.
 */
function planRelease(exec, opts) {
  const cwd = opts.cwd;
  const ref = opts.ref || 'HEAD';

  const base = latestTag(exec, cwd, 'v*.*.*', RUNTIME_TAG_RE);
  const specBase = latestTag(exec, cwd, 'spec/v*.*.*', SPEC_TAG_RE);
  const commits = commitsSince(exec, cwd, base && base.name, ref);

  if (!commits.length && !opts.force) {
    return {
      status: 'no-changes',
      repo: opts.repo || null,
      base_tag: base ? base.name : null,
      commits: 0,
      bump: null,
      tag: null,
      spec: { touched: false, base_tag: specBase ? specBase.name : null, tag: null, bump: null },
      changelog: '',
    };
  }

  const bump = computeBump(commits, opts.bump);
  const tag = fmtTag(nextVersion(base && base.version, bump));

  // Spec stream: only when commits touched spec/ since the spec base (or the
  // runtime base when no spec tag exists yet). `none` opts out explicitly.
  const specShas = specShasSince(exec, cwd, (specBase && specBase.name) || (base && base.name), ref);
  const specCommits = commits.filter((c) => specShas.has(c.sha));
  const specTouched = specCommits.length > 0;
  const specBumpOpt = opts.specBump || 'auto';
  let spec = { touched: specTouched, base_tag: specBase ? specBase.name : null, tag: null, bump: null };
  if (specTouched && specBumpOpt !== 'none') {
    const specBump = computeBump(specCommits, specBumpOpt);
    spec = {
      touched: true,
      base_tag: specBase ? specBase.name : null,
      bump: specBump,
      tag: fmtTag(nextVersion(specBase && specBase.version, specBump), true),
      commits: specCommits.length,
    };
  }

  const changelog = renderChangelog({
    repo: opts.repo, tag, baseTag: base && base.name, ref, commits, spec,
  });

  return {
    status: 'planned',
    repo: opts.repo || null,
    base_tag: base ? base.name : null,
    ref,
    commits: commits.length,
    bump,
    tag,
    spec,
    changelog,
  };
}

// --- Publish ----------------------------------------------------------------------

function publishRelease(exec, opts, plan) {
  const cwd = opts.cwd;
  const ref = opts.ref || 'HEAD';
  const remote = opts.remote || 'origin';

  const exists = tryExec(exec, ['git', '-C', cwd, 'rev-parse', '-q', '--verify', `refs/tags/${plan.tag}`]);
  if (exists.ok && String(exists.out).trim()) {
    throw new Error(`tag ${plan.tag} already exists — refusing to re-release`);
  }

  exec(['git', '-C', cwd, 'tag', '-a', plan.tag, '-m',
    `${plan.tag} — release (${plan.bump})`, ref]);
  exec(['git', '-C', cwd, 'push', remote, `refs/tags/${plan.tag}`]);

  const spec = plan.spec;
  if (spec && spec.touched && spec.tag) {
    const specExists = tryExec(exec, ['git', '-C', cwd, 'rev-parse', '-q', '--verify', `refs/tags/${spec.tag}`]);
    if (!specExists.ok || !String(specExists.out).trim()) {
      exec(['git', '-C', cwd, 'tag', '-a', spec.tag, '-m',
        `${spec.tag} — spec release (${spec.bump})`, ref]);
      exec(['git', '-C', cwd, 'push', remote, `refs/tags/${spec.tag}`]);
      spec.pushed = true;
    } else {
      spec.pushed = false;
      spec.skipped = 'tag already exists';
    }
  }

  if (!opts.repo) throw new Error('--publish requires --repo OWNER/NAME (or $GITHUB_REPOSITORY)');
  const rel = exec([
    'gh', 'api', '-X', 'POST', `repos/${opts.repo}/releases`,
    '-f', `tag_name=${plan.tag}`,
    '-f', `target_commitish=${ref}`,
    '-f', `name=${plan.tag}`,
    '-f', `body=${plan.changelog}`,
    '-F', 'draft=false',
    '-F', 'prerelease=false',
    '-F', 'generate_release_notes=false',
  ]);
  let url = null;
  try { url = JSON.parse(rel).html_url || null; } catch (e) { /* non-JSON gh output */ }

  return { status: 'released', url, pushed: true };
}

// --- CLI --------------------------------------------------------------------------

function usage() {
  return [
    'usage: release-manager.js [--repo OWNER/NAME] [--bump auto|major|minor|patch]',
    '       [--spec-bump auto|major|minor|patch|none] [--ref REF] [--remote NAME]',
    '       [--cwd DIR] [--dry-run|--publish] [--force] [--changelog-file FILE]',
    '       [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv, env) {
  const opts = {
    bump: 'auto', specBump: 'auto', ref: 'HEAD', remote: 'origin',
    publish: false, force: false, json: false, githubOutput: false,
    repo: (env && env.GITHUB_REPOSITORY) || '',
    cwd: '',
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--bump') opts.bump = next();
    else if (a === '--spec-bump') opts.specBump = next();
    else if (a === '--ref') opts.ref = next();
    else if (a === '--remote') opts.remote = next();
    else if (a === '--cwd') opts.cwd = next();
    else if (a === '--publish') opts.publish = true;
    else if (a === '--dry-run') opts.publish = false;
    else if (a === '--force') opts.force = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--changelog-file') opts.changelogFile = next();
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  if (!BUMPS.includes(opts.bump)) throw new Error(`invalid --bump '${opts.bump}'`);
  if (![...BUMPS, 'none'].includes(opts.specBump)) {
    throw new Error(`invalid --spec-bump '${opts.specBump}'`);
  }
  return opts;
}

function main(argv, { exec, stdout, env } = {}) {
  const e = exec || defaultExec;
  const runEnv = env || process.env;
  const opts = parseArgs(argv, runEnv);
  const emit = stdout || ((s) => console.log(s));

  const cwd = opts.cwd
    || String(e(['git', 'rev-parse', '--show-toplevel']) || '').trim();
  if (!cwd) throw new Error('not inside a git work tree — pass --cwd DIR');

  const plan = planRelease(e, { ...opts, cwd });

  let result = { ...plan };
  if (opts.publish && plan.status === 'planned') {
    result = { ...plan, ...publishRelease(e, { ...opts, cwd }, plan) };
  } else if (opts.publish && plan.status === 'no-changes') {
    result = { ...plan, status: 'no-changes', note: 'nothing to release (use --force to override)' };
  } else {
    result = { ...plan, dry_run: true };
  }

  if (opts.changelogFile) fs.writeFileSync(opts.changelogFile, plan.changelog || '');

  if (opts.githubOutput && runEnv.GITHUB_OUTPUT) {
    const lines = [
      `release_status=${result.status}`,
      `release_tag=${result.tag || ''}`,
      `release_bump=${result.bump || ''}`,
      `release_base_tag=${result.base_tag || ''}`,
      `release_spec_tag=${(result.spec && result.spec.tag) || ''}`,
      `release_url=${result.url || ''}`,
    ];
    fs.appendFileSync(runEnv.GITHUB_OUTPUT, `${lines.join('\n')}\n`);
  }

  if (opts.json) emit(JSON.stringify(result, null, 2));
  else if (result.status === 'no-changes') emit(`[i] no commits since ${result.base_tag || 'start'} — nothing to release`);
  else emit(`${result.dry_run ? '[dry-run] ' : ''}${result.status}: ${result.tag}` +
    (result.spec && result.spec.tag ? ` (+ spec ${result.spec.tag})` : '') +
    (result.url ? ` — ${result.url}` : ''));

  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`release-manager: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  RUNTIME_TAG_RE,
  SPEC_TAG_RE,
  SPEC_GLOB,
  SECTIONS,
  parseConventional,
  classifyCommit,
  computeBump,
  parseTag,
  compareVersions,
  nextVersion,
  parseGitLog,
  groupedSections,
  renderSections,
  renderChangelog,
  latestTag,
  commitsSince,
  specShasSince,
  planRelease,
  publishRelease,
  parseArgs,
  main,
};
