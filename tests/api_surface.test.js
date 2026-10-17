// tests/api_surface.test.js — golden snapshots of every workflow_call
// interface (issue #135).
//
// Each reusable stage's `on.workflow_call` block (inputs / outputs /
// secrets) is the public API surface consumer repos bind to. The checked-in
// snapshots in tests/api-surface/*.json pin that surface: any drift — a
// renamed input, a changed type, a dropped output — fails here with a
// readable diff and a `--bless` hint, instead of silently breaking every
// caller workflow downstream. Picked up by the `*/*.test.js` step of
// tools/validate-repo.sh → the `contract` job of ai-sdlc.yml, so interface
// drift fails the PR.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const TOOL = path.join(__dirname, 'api_surface.js');
const {
  runAll,
  bless,
  extractInterface,
  onSection,
  snapshotPath,
  DEFAULT_SNAPSHOT_DIR,
  WORKFLOWS_DIR,
} = require('./api_surface');

function runCli(args) {
  return spawnSync(process.execPath, [TOOL, ...args], { encoding: 'utf8' });
}

// Every workflow_call interface matches its checked-in snapshot — one
// subtest per workflow so a drift names the stage and shows the diff.
test('api surface: every reusable workflow matches its snapshot', async (t) => {
  const report = runAll();
  assert.ok(report.results.length > 0, 'no workflows found');
  for (const r of report.results) {
    if (r.status === 'skip') {
      await t.test(`${r.workflow} (not reusable)`, (st) => st.skip(r.detail));
      continue;
    }
    await t.test(r.workflow, () => {
      if (r.status === 'fail') {
        const diffText = (r.diffs || [])
          .map((d) => `${d.path}: expected ${d.expected}, got ${d.actual}`)
          .join('\n  ');
        assert.fail(`workflow_call interface drifted from tests/api-surface snapshot — intentional changes re-record with \`node tests/api_surface.js --bless\`; breaking input/output changes are MAJOR per spec semver rules\n  ${diffText}`);
      }
      assert.equal(r.status, 'pass', r.detail || `unexpected status ${r.status}`);
    });
  }
});

test('api surface: every snapshot file corresponds to a workflow with workflow_call', () => {
  const snaps = fs.readdirSync(DEFAULT_SNAPSHOT_DIR).filter((f) => f.endsWith('.json'));
  assert.ok(snaps.length > 0, 'no snapshots checked in');
  for (const s of snaps) {
    const wf = path.join(WORKFLOWS_DIR, s.replace(/\.json$/, '.yml'));
    assert.ok(fs.existsSync(wf), `orphan snapshot ${s} — no workflow ${path.basename(wf)}`);
    assert.ok(extractInterface(wf) !== null, `orphan snapshot ${s} — ${path.basename(wf)} declares no workflow_call`);
  }
});

// --- Harness internals ---------------------------------------------------------

test('onSection reads `on:` under both YAML 1.1 (true key) and 1.2 spellings', () => {
  assert.deepEqual(onSection({ on: { workflow_call: {} } }), { workflow_call: {} });
  assert.deepEqual(onSection({ true: { workflow_call: {} } }), { workflow_call: {} });
  assert.deepEqual(onSection({}), {});
  assert.deepEqual(onSection(null), {});
});

test('a drifted interface fails with a readable diff', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'api-surface-'));
  const wfDir = path.join(dir, 'workflows');
  const snapDir = path.join(dir, 'snapshots');
  fs.mkdirSync(wfDir, { recursive: true });
  fs.mkdirSync(snapDir, { recursive: true });
  fs.copyFileSync(
    path.join(WORKFLOWS_DIR, 'ai-sdlc-verify.yml'),
    path.join(wfDir, 'ai-sdlc-verify.yml'));
  fs.copyFileSync(
    snapshotPath(DEFAULT_SNAPSHOT_DIR, 'ai-sdlc-verify.yml'),
    path.join(snapDir, 'ai-sdlc-verify.json'));
  // Tamper: rename an input inside the snapshot — a breaking interface change.
  const snap = JSON.parse(fs.readFileSync(path.join(snapDir, 'ai-sdlc-verify.json'), 'utf8'));
  const firstInput = Object.keys(snap.inputs).sort()[0];
  snap.inputs[`${firstInput}-renamed`] = snap.inputs[firstInput];
  delete snap.inputs[firstInput];
  fs.writeFileSync(path.join(snapDir, 'ai-sdlc-verify.json'), JSON.stringify(snap, null, 2));

  const report = runAll({ workflowsDir: wfDir, snapshotDir: snapDir });
  assert.equal(report.ok, false);
  const failed = report.results.find((r) => r.status === 'fail');
  assert.ok(failed, 'drifted snapshot must fail');
  assert.ok(failed.diffs.some((d) => d.path.includes(firstInput)),
    'diff should name the renamed input');
});

test('a missing snapshot fails with a bless hint', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'api-surface-'));
  const wfDir = path.join(dir, 'workflows');
  const snapDir = path.join(dir, 'snapshots');
  fs.mkdirSync(wfDir, { recursive: true });
  fs.mkdirSync(snapDir, { recursive: true });
  fs.copyFileSync(
    path.join(WORKFLOWS_DIR, 'ai-sdlc-verify.yml'),
    path.join(wfDir, 'ai-sdlc-verify.yml'));
  const report = runAll({ workflowsDir: wfDir, snapshotDir: snapDir });
  assert.equal(report.ok, false);
  assert.match(report.results[0].diffs[0].actual, /--bless/);
});

test('--bless records a snapshot that a subsequent run matches', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'api-surface-'));
  const wfDir = path.join(dir, 'workflows');
  const snapDir = path.join(dir, 'snapshots');
  fs.mkdirSync(wfDir, { recursive: true });
  fs.copyFileSync(
    path.join(WORKFLOWS_DIR, 'ai-sdlc-verify.yml'),
    path.join(wfDir, 'ai-sdlc-verify.yml'));
  const written = bless({ workflowsDir: wfDir, snapshotDir: snapDir });
  assert.equal(written.length, 1);
  const report = runAll({ workflowsDir: wfDir, snapshotDir: snapDir });
  assert.equal(report.ok, true, JSON.stringify(report.results[0].diffs));
});

test('CLI exits 0 when every snapshot matches and prints the summary', () => {
  const res = runCli([]);
  assert.equal(res.status, 0, res.stderr || res.stdout);
  assert.match(res.stdout, /\d+ pass \/ 0 fail \/ \d+ skip/);
});
