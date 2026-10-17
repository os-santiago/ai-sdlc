// tests/regression.test.js — node --test entry point for the pipeline
// regression harness (issue #193). Replays every fixture case in
// tests/regression/cases against its golden and fails on any drift —
// picked up by the `*/*.test.js` step of tools/validate-repo.sh, hence by
// `npm test` and CI. Run the harness directly for the console diff report:
//
//   node tests/regression_harness.js            # replay + diff report
//   node tests/regression_harness.js --bless    # re-record goldens
//
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const HARNESS_PATH = path.join(__dirname, 'regression_harness.js');
const {
  loadCases,
  runCase,
  runAll,
  bless,
  extractNodeHeredoc,
  diffObjects,
  CASES_DIR,
  GOLDEN_DIR,
} = require('./regression_harness');

function runCli(args) {
  return spawnSync(process.execPath, [HARNESS_PATH, ...args], { encoding: 'utf8' });
}

// Every fixture case replays green — one subtest each so a drift names its
// case and surfaces the diff in the failure message.
test('regression suite: every fixture case matches its golden', async (t) => {
  const cases = loadCases();
  assert.ok(cases.length > 0, 'no regression cases found');
  for (const c of cases) {
    await t.test(`${c.id} (${c.type})`, (st) => {
      const r = runCase(c, { goldenDir: GOLDEN_DIR });
      if (r.status === 'skip') return st.skip(r.detail);
      if (r.status === 'fail') {
        const diffText = (r.diffs || [])
          .map((d) => `${d.path}: expected ${d.expected}, got ${d.actual}`)
          .join('\n  ');
        assert.fail(`behavior drifted from golden — re-run \`node tests/regression_harness.js --case ${c.id}\` for the full report; bless intentionally with --bless ${c.id}\n  ${diffText}`);
      }
      assert.equal(r.status, 'pass', r.detail || `unexpected status ${r.status}`);
      return undefined;
    });
  }
});

// --- Harness internals ---------------------------------------------------------

test('extractNodeHeredoc pulls the shipped DoR script, not a copy', () => {
  const src = extractNodeHeredoc(
    path.join(__dirname, '..', '.github', 'workflows', 'ai-sdlc-intake.yml'),
    { mustContain: 'const checks = {' });
  assert.match(src, /const checks = \{/);
  assert.match(src, /R1 conventional title/);
});

test('extractNodeHeredoc fails loudly when the decision code moved', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'reg-wf-'));
  const wf = path.join(dir, 'wf.yml');
  fs.writeFileSync(wf, "run: |\n  node - <<'NODE'\n  console.log('unrelated');\n  NODE\n");
  assert.throws(
    () => extractNodeHeredoc(wf, { mustContain: 'const checks = {' }),
    /no longer contains marker/);
});

test('diffObjects produces readable leaf diffs and ignores equal subtrees', () => {
  const diffs = diffObjects(
    { a: 1, b: { c: 'x' }, gone: true },
    { a: 1, b: { c: 'y' }, extra: 'new' });
  assert.equal(diffs.length, 3);
  assert.deepEqual(diffs.map((d) => d.path).sort(), ['b.c', 'extra', 'gone']);
  const bc = diffs.find((d) => d.path === 'b.c');
  assert.equal(bc.expected, '"x"');
  assert.equal(bc.actual, '"y"');
});

// --- CLI contract --------------------------------------------------------------

test('harness exits 0 when every golden matches', () => {
  const res = runCli([]);
  assert.equal(res.status, 0, res.stderr || res.stdout);
  assert.match(res.stdout, /\d+ pass \/ 0 fail \/ 0 error/);
});

test('harness writes the diff report artifact', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'reg-report-'));
  const reportPath = path.join(dir, 'report.json');
  const res = runCli(['--case', 'risk-docs-only-low', '--report', reportPath]);
  assert.equal(res.status, 0, res.stderr || res.stdout);
  const report = JSON.parse(fs.readFileSync(reportPath, 'utf8'));
  assert.equal(report.summary.total, 1);
  assert.equal(report.cases[0].id, 'risk-docs-only-low');
});

test('a drifted golden fails the suite with a readable diff', () => {
  // Replay the suite over a scratch copy with one tampered golden — the
  // checked-in goldens stay untouched.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'reg-drift-'));
  const casesDir = path.join(dir, 'cases');
  const goldenDir = path.join(dir, 'golden');
  fs.mkdirSync(casesDir, { recursive: true });
  fs.mkdirSync(goldenDir, { recursive: true });
  fs.copyFileSync(path.join(CASES_DIR, 'risk-docs-only-low.json'), path.join(casesDir, 'risk-docs-only-low.json'));
  const golden = JSON.parse(fs.readFileSync(path.join(GOLDEN_DIR, 'risk-docs-only-low.expected.json'), 'utf8'));
  golden.result.tier = 'critical';
  fs.writeFileSync(path.join(goldenDir, 'risk-docs-only-low.expected.json'), JSON.stringify(golden, null, 2));

  const res = runCli(['--cases-dir', casesDir, '--golden-dir', goldenDir, '--report', path.join(dir, 'r.json')]);
  assert.equal(res.status, 1, res.stdout);
  assert.match(res.stdout, /result\.tier: expected "critical", got "low"/);
  const report = JSON.parse(fs.readFileSync(path.join(dir, 'r.json'), 'utf8'));
  assert.equal(report.summary.ok, false);
  assert.ok(report.cases[0].diffs.length > 0, 'report artifact must carry the diffs');
});

test('a missing golden fails with a bless hint instead of crashing', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'reg-missing-'));
  const casesDir = path.join(dir, 'cases');
  const goldenDir = path.join(dir, 'golden');
  fs.mkdirSync(casesDir, { recursive: true });
  fs.mkdirSync(goldenDir, { recursive: true });
  fs.copyFileSync(path.join(CASES_DIR, 'risk-no-files-medium.json'), path.join(casesDir, 'risk-no-files-medium.json'));
  const report = runAll({ casesDir, goldenDir });
  assert.equal(report.summary.fail, 1);
  assert.match(report.cases[0].diffs[0].actual, /--bless/);
});

test('--bless records a golden that a subsequent run matches', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'reg-bless-'));
  const casesDir = path.join(dir, 'cases');
  const goldenDir = path.join(dir, 'golden');
  fs.mkdirSync(casesDir, { recursive: true });
  fs.copyFileSync(path.join(CASES_DIR, 'risk-no-files-medium.json'), path.join(casesDir, 'risk-no-files-medium.json'));
  const b = bless({ casesDir, goldenDir });
  assert.equal(b.written.length, 1);
  assert.equal(b.problems.length, 0);
  const report = runAll({ casesDir, goldenDir });
  assert.equal(report.summary.ok, true, JSON.stringify(report.cases[0].diffs));
});
