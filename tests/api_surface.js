#!/usr/bin/env node
// tests/api_surface.js — golden snapshots of every workflow_call interface
// (issue #135).
//
// The `on.workflow_call` block of each reusable stage — inputs, outputs,
// secrets — is the public API surface every consumer repo depends on.
// Renaming an input, changing a type, or dropping an output silently breaks
// every caller workflow in every consumer repo. This tool extracts the
// interface of each .github/workflows/*.yml into a normalized JSON shape
// and diffs it against the checked-in snapshot in tests/api-surface/:
//
//   node tests/api_surface.js            # diff all interfaces vs snapshots
//   node tests/api_surface.js --bless    # (re)write snapshots intentionally
//   node tests/api_surface.js --json     # machine-readable report
//
// Exit 0 = every interface matches its snapshot (or was blessed),
// 1 = drift / missing / orphan snapshot, 2 = usage or tooling error.
// `node --test` runs the same comparison via tests/api_surface.test.js.
//
// Semver tie-in (AGENTS.md): removing/renaming an input or output, changing
// a type, or adding a required input is a BREAKING interface change — the
// snapshot diff fails the gate and the change needs a MAJOR spec bump plus
// a migration note. Additive optional inputs/outputs are MINOR.
'use strict';

const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');
const { diffObjects } = require('./regression_harness');

const ROOT = path.join(__dirname, '..');
const WORKFLOWS_DIR = path.join(ROOT, '.github', 'workflows');
const DEFAULT_SNAPSHOT_DIR = path.join(__dirname, 'api-surface');

// --- YAML → JSON -------------------------------------------------------------
// Same dual-backend posture as tools/validate-repo.sh and
// scripts/contract-drift.js: python3+PyYAML preferred, npx js-yaml fallback.
function parseYaml(file) {
  try {
    const out = execFileSync('python3', ['-c',
      'import yaml,json,sys; json.dump(yaml.safe_load(open(sys.argv[1])), sys.stdout)',
      file], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
    return JSON.parse(out);
  } catch (e) { /* fall through */ }
  try {
    const out = execFileSync('npx', ['-y', '-q', 'js-yaml', file],
      { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
    return JSON.parse(out);
  } catch (e) { /* fall through */ }
  throw new Error('no YAML backend — need python3 + PyYAML or npx js-yaml');
}

// --- Interface normalization --------------------------------------------------
// Recursively sort object keys so the snapshot is byte-stable regardless of
// source ordering. PyYAML parses a top-level `on:` as boolean true (YAML 1.1)
// — read both spellings.
function sortDeep(v) {
  if (Array.isArray(v)) return v.map(sortDeep);
  if (v && typeof v === 'object') {
    const out = {};
    for (const k of Object.keys(v).sort()) out[k] = sortDeep(v[k]);
    return out;
  }
  return v;
}

function onSection(doc) {
  if (!doc || typeof doc !== 'object') return {};
  if (doc.on !== undefined) return doc.on || {};
  if (doc[true] !== undefined) return doc[true] || {}; // YAML 1.1 `on:` → true
  return {};
}

/** Extract the workflow_call interface → normalized snapshot shape, or null. */
function extractInterface(workflowPath) {
  const doc = parseYaml(workflowPath);
  const wc = onSection(doc).workflow_call;
  if (!wc) return null;
  return sortDeep({
    workflow: path.basename(workflowPath),
    inputs: wc.inputs || {},
    outputs: wc.outputs || {},
    secrets: wc.secrets || {},
  });
}

// --- Compare / bless ------------------------------------------------------------

function snapshotPath(snapshotDir, workflowFile) {
  return path.join(snapshotDir, `${path.basename(workflowFile, path.extname(workflowFile))}.json`);
}

function listWorkflows(workflowsDir = WORKFLOWS_DIR) {
  return fs.readdirSync(workflowsDir)
    .filter((f) => /\.ya?ml$/.test(f))
    .sort()
    .map((f) => path.join(workflowsDir, f));
}

/**
 * Diff every reusable workflow's interface against its snapshot.
 * Returns {ok, results:[{workflow, status, diffs?, detail?}]} where status is
 * pass|fail|missing|skip and 'skip' means the workflow declares no
 * workflow_call (not part of the public surface).
 */
function runAll({ workflowsDir = WORKFLOWS_DIR, snapshotDir = DEFAULT_SNAPSHOT_DIR } = {}) {
  const results = [];
  for (const wf of listWorkflows(workflowsDir)) {
    const rel = path.basename(wf);
    const snap = snapshotPath(snapshotDir, wf);
    const iface = extractInterface(wf);
    if (iface === null) {
      results.push({ workflow: rel, status: 'skip', detail: 'no workflow_call — not a reusable stage' });
      if (fs.existsSync(snap)) {
        results.push({ workflow: rel, status: 'fail', diffs: [{ path: '', expected: 'no snapshot', actual: 'orphan snapshot present' }] });
      }
      continue;
    }
    if (!fs.existsSync(snap)) {
      results.push({ workflow: rel, status: 'fail', diffs: [{ path: '', expected: 'checked-in snapshot', actual: 'none — record it with `node tests/api_surface.js --bless`' }] });
      continue;
    }
    const expected = JSON.parse(fs.readFileSync(snap, 'utf8'));
    const diffs = diffObjects(expected, iface);
    if (diffs.length) {
      results.push({ workflow: rel, status: 'fail', diffs });
    } else {
      results.push({ workflow: rel, status: 'pass' });
    }
  }
  return { ok: results.every((r) => r.status !== 'fail'), results };
}

/** (Re)write a snapshot for every workflow that declares workflow_call. */
function bless({ workflowsDir = WORKFLOWS_DIR, snapshotDir = DEFAULT_SNAPSHOT_DIR } = {}) {
  fs.mkdirSync(snapshotDir, { recursive: true });
  const written = [];
  for (const wf of listWorkflows(workflowsDir)) {
    const iface = extractInterface(wf);
    if (iface === null) continue;
    const snap = snapshotPath(snapshotDir, wf);
    fs.writeFileSync(snap, JSON.stringify(iface, null, 2) + '\n');
    written.push(snap);
  }
  return written;
}

function main(argv) {
  const blessMode = argv.includes('--bless');
  const json = argv.includes('--json');
  if (blessMode) {
    const written = bless();
    for (const f of written) console.log(`[bless] ${path.relative(ROOT, f)}`);
    return 0;
  }
  const report = runAll();
  if (json) {
    console.log(JSON.stringify(report, null, 2));
    return report.ok ? 0 : 1;
  }
  for (const r of report.results) {
    if (r.status === 'pass') console.log(`[✓] ${r.workflow}`);
    else if (r.status === 'skip') console.log(`[–] ${r.workflow}: ${r.detail}`);
    else {
      console.log(`[X] ${r.workflow}: interface drift — if intentional, run \`node tests/api_surface.js --bless\` and flag breaking changes per spec semver rules`);
      for (const d of r.diffs || []) {
        console.log(`      ${d.path}: expected ${d.expected}, got ${d.actual}`);
      }
    }
  }
  const fails = report.results.filter((r) => r.status === 'fail').length;
  const passes = report.results.filter((r) => r.status === 'pass').length;
  console.log(`${passes} pass / ${fails} fail / ${report.results.length - passes - fails} skip`);
  return fails ? 1 : 0;
}

if (require.main === module) {
  try {
    process.exit(main(process.argv.slice(2)));
  } catch (e) {
    console.error(`api-surface: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  ROOT,
  WORKFLOWS_DIR,
  DEFAULT_SNAPSHOT_DIR,
  parseYaml,
  onSection,
  extractInterface,
  snapshotPath,
  listWorkflows,
  runAll,
  bless,
  main,
};
