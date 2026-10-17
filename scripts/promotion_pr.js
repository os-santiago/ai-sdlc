#!/usr/bin/env node
// scripts/promotion_pr.js — evidence-driven autonomy promotion PR
// generator (issue #202, epic #151 trust machinery).
//
// When promotion evidence indicates readiness, this module opens the
// contract PR that raises `autonomy.level` in `.ai-sdlc.yaml` to the next
// rung of spec/trust-ladder.md. The diff touches ONLY the `level:` value —
// every other byte of the contract is preserved verbatim.
//
// Decoupling boundary: this module does NOT compute evidence. The
// evidence computation itself is issue #201; here the verdict is injected
// as a plain payload (`--evidence FILE` / `--evidence-json` /
// `--evidence-text`, or the `evidence` option of the library API):
//
//   { "ready": true,
//     "window": 30,
//     "metrics": {"merge_success_rate": 0.95, "revert_rate": 0.0, ...},
//     "summary": "optional free-form text" }
//
// Contract: `ready === false` refuses to open the PR (fail-closed); any
// other shape is treated as caller-asserted readiness and is rendered
// verbatim into the PR body.
//
// CLI (default is DRY-RUN — proves the PR payload without touching git or
// the GitHub API; pass --apply to mutate):
//
//   node scripts/promotion_pr.js --repo OWNER/NAME \
//     [--contract .ai-sdlc.yaml] [--base main] \
//     [--branch feat/promotion-autonomy-<next>] \
//     [--evidence FILE | --evidence-json JSON | --evidence-text TEXT] \
//     [--apply] [--json] [--github-output]
'use strict';

const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

// Canonical ladder — the normative order of spec/trust-ladder.md. Kept in
// parity with the autonomy.level enum in spec/ai-sdlc.schema.json.
const LADDER = ['shadow', 'suggest', 'auto-pr', 'auto-merge-low', 'auto-merge-all', 'auto-deploy'];

// Schema default when the contract omits `autonomy.level`
// (spec/ai-sdlc.schema.json: "Default: shadow" — shadow-first
// onboarding, issue #186).
const DEFAULT_LEVEL = 'shadow';

const CONTRACT_PATH = '.ai-sdlc.yaml';
const PR_TITLE_PREFIX = 'feat(promotion): bump autonomy level to ';

// --- Ladder ------------------------------------------------------------------

// Next rung after `current`, or null when already at the ceiling.
function nextLevel(current) {
  const i = LADDER.indexOf(current);
  if (i === -1) {
    throw new Error(`unknown autonomy.level '${current}' — expected one of ${LADDER.join('|')}`);
  }
  return i < LADDER.length - 1 ? LADDER[i + 1] : null;
}

function branchNameFor(next) {
  return `feat/promotion-autonomy-${next}`;
}

function buildPrTitle(next) {
  return `${PR_TITLE_PREFIX}${next}`;
}

// --- Contract surgery --------------------------------------------------------
//
// The contract is plain YAML but we deliberately do NOT round-trip it
// through a YAML serializer: a serializer would reorder keys and drop
// comments, breaking the "diff changes only autonomy.level" invariant.
// Instead we locate the top-level `autonomy:` mapping and rewrite only the
// scalar inside its `level:` line, preserving indentation, quoting style
// and trailing comments byte-for-byte.

// Index range [start, end) of the top-level `autonomy:` block, plus the
// `level:` line inside it when present. A top-level (column-0) non-comment
// line other than `autonomy:` ends the block; blank lines and comments do
// not (both are legal inside a YAML mapping).
function locateAutonomy(lines) {
  let start = -1;
  for (let i = 0; i < lines.length; i += 1) {
    if (/^autonomy\s*:/.test(lines[i])) { start = i; break; }
  }
  if (start === -1) return null;

  let end = lines.length;
  let levelIdx = -1;
  for (let i = start + 1; i < lines.length; i += 1) {
    const line = lines[i];
    if (/^[^\s#]/.test(line)) { end = i; break; } // next top-level key
    if (levelIdx === -1 && /^\s+level\s*:/.test(line)) levelIdx = i;
  }
  return { start, end, levelIdx };
}

// Reads the configured level. Returns {level, explicit} where `explicit`
// is false when the contract omits `level:` and the schema default
// applies. Throws when there is no `autonomy:` block at all — opening a
// promotion PR against a contract that cannot carry the field is a bug,
// not a promotion.
function readAutonomyLevel(yamlText) {
  const lines = String(yamlText).split('\n');
  const block = locateAutonomy(lines);
  if (!block) {
    throw new Error('contract has no top-level `autonomy:` mapping — cannot promote');
  }
  if (block.levelIdx === -1) {
    return { level: DEFAULT_LEVEL, explicit: false };
  }
  const m = lines[block.levelIdx].match(/^\s*level\s*:\s*(['"]?)([^'"\s#]+)\1/);
  if (!m) {
    throw new Error(`unparseable autonomy.level line: ${lines[block.levelIdx].trim()}`);
  }
  const level = m[2];
  if (!LADDER.includes(level)) {
    throw new Error(`unknown autonomy.level '${level}' — expected one of ${LADDER.join('|')}`);
  }
  return { level, explicit: true };
}

// Returns the contract text with `autonomy.level` set to `next`, changing
// only that field: existing lines are preserved verbatim (indentation,
// quoting style, trailing comments); when `level:` is absent a single
// line is inserted as the first property of the `autonomy:` block.
function setAutonomyLevel(yamlText, next) {
  if (!LADDER.includes(next)) {
    throw new Error(`refusing to set unknown autonomy.level '${next}'`);
  }
  const lines = String(yamlText).split('\n');
  const block = locateAutonomy(lines);
  if (!block) {
    throw new Error('contract has no top-level `autonomy:` mapping — cannot promote');
  }

  if (block.levelIdx === -1) {
    // Insert `level:` as the first property of the block, matching the
    // indentation of the block's first property (default two spaces).
    let indent = '  ';
    for (let i = block.start + 1; i < block.end; i += 1) {
      const m = lines[i].match(/^(\s+)\S/);
      if (m) { indent = m[1]; break; }
    }
    lines.splice(block.start + 1, 0, `${indent}level: ${next}`);
    return lines.join('\n');
  }

  const line = lines[block.levelIdx];
  const m = line.match(/^(\s*level\s*:\s*)(['"]?)([^'"\s#]+)\2(.*)$/);
  if (!m) {
    throw new Error(`unparseable autonomy.level line: ${line.trim()}`);
  }
  // Reuse the original quoting style and trailing comment verbatim.
  lines[block.levelIdx] = `${m[1]}${m[2]}${next}${m[2]}${m[4]}`;
  return lines.join('\n');
}

// --- Evidence rendering ------------------------------------------------------

// Renders the injected evidence payload as markdown for the PR body. The
// shape is owned by the evidence computation (#201); this module renders
// `summary` text, a `metrics` table, remaining scalar fields as bullets,
// and any leftover structure as a JSON block — deterministically.
function formatEvidence(evidence) {
  if (evidence == null) return '_No evidence payload supplied._';
  if (typeof evidence === 'string') {
    return evidence.trim() || '_No evidence payload supplied._';
  }
  if (typeof evidence !== 'object') return String(evidence);

  const parts = [];
  const rest = {};
  for (const [k, v] of Object.entries(evidence)) {
    if (k === 'summary' || k === 'metrics') continue;
    rest[k] = v;
  }

  if (typeof evidence.summary === 'string' && evidence.summary.trim()) {
    parts.push(evidence.summary.trim());
  }

  if (evidence.metrics && typeof evidence.metrics === 'object') {
    const rows = ['| Metric | Value |', '|---|---|'];
    for (const [k, v] of Object.entries(evidence.metrics)) {
      const val = (v !== null && typeof v === 'object') ? JSON.stringify(v) : String(v);
      rows.push(`| \`${k}\` | ${val} |`);
    }
    parts.push(rows.join('\n'));
  }

  const scalarRest = {};
  const nestedRest = {};
  for (const [k, v] of Object.entries(rest)) {
    if (v === null || typeof v !== 'object') scalarRest[k] = v;
    else nestedRest[k] = v;
  }
  const scalars = Object.entries(scalarRest)
    .map(([k, v]) => `- **${k}**: ${v}`);
  if (scalars.length) parts.push(scalars.join('\n'));
  if (Object.keys(nestedRest).length) {
    parts.push('```json\n' + JSON.stringify(nestedRest, null, 2) + '\n```');
  }

  return parts.length ? parts.join('\n\n') : '_No evidence payload supplied._';
}

function buildPrBody({ current, next, evidence }) {
  return [
    `## Promotion proposal — \`${current}\` → \`${next}\``,
    '',
    'Run evidence meets the `spec/trust-ladder.md` promotion gate for the',
    'next autonomy level. This PR raises the `autonomy.level` ceiling in',
    '`.ai-sdlc.yaml`; a maintainer still reviews and merges the promotion —',
    'the pipeline accredits itself with data, it does not grant itself',
    'privileges.',
    '',
    '### Evidence',
    '',
    formatEvidence(evidence),
    '',
    '### Scope',
    '',
    `- Changes **only** \`autonomy.level\` (\`${current}\` → \`${next}\`) — no other`,
    '  field, comment, or byte of the contract is touched.',
    '- The configured level is a **ceiling**: the pipeline may still operate',
    '  below it (freezes, incidents, outages), never above it.',
    '',
    '---',
    '<sub>Generated by `scripts/promotion_pr.js` — epic #151 trust machinery.</sub>',
    '',
  ].join('\n');
}

// --- Planning ----------------------------------------------------------------

// Pure function over the contract text + injected evidence. Returns:
//   { promotable: true, current, next, title, body, contract, branch }
//   { promotable: false, reason, current }
// Throws on unreadable contracts, unknown levels, or `ready === false`
// evidence (fail-closed: a negative verdict is an explicit veto, not an
// absence of signal).
function planPromotion({ contractText, evidence, branch } = {}) {
  if (contractText == null) throw new Error('planPromotion requires contractText');
  if (evidence && typeof evidence === 'object' && evidence.ready === false) {
    throw new Error('evidence does not indicate readiness (ready: false) — refusing to open a promotion PR');
  }

  const { level: current } = readAutonomyLevel(contractText);
  const next = nextLevel(current);
  if (next === null) {
    return {
      promotable: false,
      reason: `already at maximum autonomy level '${current}'`,
      current,
      next: null,
    };
  }

  return {
    promotable: true,
    current,
    next,
    title: buildPrTitle(next),
    body: buildPrBody({ current, next, evidence }),
    contract: setAutonomyLevel(contractText, next),
    branch: branch || branchNameFor(next),
  };
}

// Minimal diff preview for the dry-run payload — one context-free hunk
// covering the single changed line (the only line that can differ).
function diffPreview(path, before, after) {
  const b = String(before).split('\n');
  const a = String(after).split('\n');
  const hunks = [];
  const n = Math.max(b.length, a.length);
  for (let i = 0; i < n; i += 1) {
    if (b[i] !== a[i]) {
      if (b[i] !== undefined) hunks.push(`-${b[i]}`);
      if (a[i] !== undefined) hunks.push(`+${a[i]}`);
    }
  }
  return [`--- a/${path}`, `+++ b/${path}`, '@@ autonomy.level @@', ...hunks].join('\n');
}

// --- PR creation (injectable layer) ------------------------------------------

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultGh(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function defaultGit(argv) {
  return execFileSync('git', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

// Opens the promotion PR. With `dryRun` the exact `gh pr create` payload is
// returned without any API call — the acceptance criterion "dry-run proves
// PR payload correctness".
function openPromotionPr({ repo, base, head, title, body, exec, dryRun }) {
  if (!repo) throw new Error('openPromotionPr requires repo (OWNER/NAME)');
  const argv = [
    'pr', 'create', '-R', repo,
    '--base', base || 'main',
    '--head', head,
    '--title', title,
    '--body', body,
  ];
  const payload = { argv, repo, base: base || 'main', head, title, body };
  if (dryRun) return { ...payload, dryRun: true };
  const url = String((exec || defaultGh)(argv)).trim();
  return { ...payload, url };
}

// Full orchestration for --apply: clean-worktree guard → branch → write the
// contract (single-field diff) → commit → push → gh pr create. `git` and
// `exec` are injectable for tests; `cwd` redirects file I/O for tests.
function applyPromotion({ repo, base, branch, contractPath, contractText, title, body, exec, git, cwd, dryRun }) {
  const run = exec || defaultGh;
  const runGit = git || defaultGit;
  const filePath = path.join(cwd || '.', contractPath || CONTRACT_PATH);

  if (dryRun) {
    return {
      dryRun: true,
      branch,
      steps: [
        ['status', '--porcelain'],
        ['checkout', '-B', branch],
        ['add', '--', contractPath || CONTRACT_PATH],
        ['commit', '-m', title],
        ['push', '-u', 'origin', branch],
      ],
      pr: openPromotionPr({ repo, base, head: branch, title, body, dryRun: true }),
    };
  }

  const dirty = String(runGit(['status', '--porcelain'])).trim();
  if (dirty) {
    throw new Error('worktree is dirty — refusing to bundle unrelated changes into a promotion PR');
  }
  runGit(['checkout', '-B', branch]);
  fs.writeFileSync(filePath, contractText);
  runGit(['add', '--', contractPath || CONTRACT_PATH]);
  runGit(['commit', '-m', title]);
  runGit(['push', '-u', 'origin', branch]);
  const pr = openPromotionPr({ repo, base, head: branch, title, body, exec: run });
  return { branch, pr };
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: promotion_pr.js --repo OWNER/NAME [--contract .ai-sdlc.yaml]',
    '                       [--base main] [--branch NAME]',
    '                       [--evidence FILE | --evidence-json JSON | --evidence-text TEXT]',
    '                       [--apply] [--json] [--github-output]',
    '',
    'Default mode is a dry-run: prints the PR payload (title/body/branch/diff)',
    'without touching git or the GitHub API. --apply creates the branch,',
    'commits the single-field contract bump, pushes, and opens the PR.',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = {
    json: false, apply: false, githubOutput: false,
    contract: CONTRACT_PATH, base: 'main',
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--contract') opts.contract = next();
    else if (a === '--base') opts.base = next();
    else if (a === '--branch') opts.branch = next();
    else if (a === '--evidence') opts.evidenceFile = next();
    else if (a === '--evidence-json') opts.evidenceJson = next();
    else if (a === '--evidence-text') opts.evidenceText = next();
    else if (a === '--apply') opts.apply = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function loadEvidence(opts) {
  if (opts.evidenceFile) {
    const raw = fs.readFileSync(opts.evidenceFile, 'utf8');
    try { return JSON.parse(raw); } catch (e) { return raw; }
  }
  if (opts.evidenceJson != null) return JSON.parse(opts.evidenceJson);
  if (opts.evidenceText != null) return opts.evidenceText;
  return null;
}

function main(argv, { exec, git, stdout, cwd } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  const contractPath = path.join(cwd || '.', opts.contract);
  const contractText = fs.readFileSync(contractPath, 'utf8');
  const evidence = loadEvidence(opts);

  const plan = planPromotion({ contractText, evidence, branch: opts.branch });
  if (!plan.promotable) {
    const out = { promoted: false, reason: plan.reason, current: plan.current };
    emit(JSON.stringify(out));
    return out;
  }

  const result = applyPromotion({
    repo: opts.repo,
    base: opts.base,
    branch: plan.branch,
    contractPath: opts.contract,
    contractText: plan.contract,
    title: plan.title,
    body: plan.body,
    exec, git, cwd,
    dryRun: !opts.apply,
  });

  const out = {
    promoted: !result.dryRun,
    dryRun: Boolean(result.dryRun),
    current: plan.current,
    next: plan.next,
    branch: plan.branch,
    title: plan.title,
    body: plan.body,
    diff: diffPreview(opts.contract, contractText, plan.contract),
    pr: result.pr,
    steps: result.steps,
  };

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      `promotion_level=${plan.next}`,
      `promotion_branch=${plan.branch}`,
      `promotion_dry_run=${result.dryRun}`,
    ];
    if (result.pr && result.pr.url) lines.push(`promotion_pr_url=${result.pr.url}`);
    fs.appendFileSync(process.env.GITHUB_OUTPUT, lines.join('\n') + '\n');
  }

  emit(JSON.stringify(out, null, opts.json ? 2 : 0));
  return out;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`promotion_pr: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  LADDER,
  DEFAULT_LEVEL,
  CONTRACT_PATH,
  PR_TITLE_PREFIX,
  nextLevel,
  branchNameFor,
  buildPrTitle,
  locateAutonomy,
  readAutonomyLevel,
  setAutonomyLevel,
  formatEvidence,
  buildPrBody,
  planPromotion,
  diffPreview,
  openPromotionPr,
  applyPromotion,
  loadEvidence,
  main,
};
