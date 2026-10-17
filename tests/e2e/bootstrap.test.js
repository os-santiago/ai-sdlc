// tests/e2e/bootstrap.test.js
// End-to-end test for bootstrap PR generation and trust level (issue
// #208, epic #153). Exercises the documented `gh ai-sdlc init` path —
// docs/onboarding.md — in a temp fixture repo, fully hermetic:
//
//   fresh contract → tools/validate-ai-sdlc-config.sh → trust level shadow
//   bootstrap PR payload → mocked gh layer → asserted shape (label
//   `bootstrap`, onboarding title, secrets checklist, canonical label set)
//
// Picked up by the `tests/**/*.test.js` step of tools/validate-repo.sh,
// hence by the contract job in CI. No network: the `gh` layer is a local
// recording stub; temp dirs are removed after each test.

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const REPO_ROOT = path.join(__dirname, '..', '..');
const VALIDATOR = path.join(REPO_ROOT, 'tools', 'validate-ai-sdlc-config.sh');
const SCHEMA = path.join(REPO_ROOT, 'spec', 'ai-sdlc.schema.json');
const TRUST_LADDER = path.join(REPO_ROOT, 'spec', 'trust-ladder.md');
const {
  starterContract,
  buildBootstrapPR,
  runBootstrapInit,
} = require('./bootstrap');

function mkFixture(t, name) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `ai-sdlc-${name}-`));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

// Extract `autonomy.level` from a contract file. The starter contract is
// single-keyed under `autonomy:` so a scoped regex suffices — no YAML dep.
function contractLevel(contractPath) {
  const text = fs.readFileSync(contractPath, 'utf8');
  const block = text.match(/autonomy:\s*\n((?:\s{2,}[^\n]*\n?)*)/);
  assert.ok(block, `no autonomy: block in ${contractPath}`);
  const level = block[1].match(/^\s*level:\s*([^\s#]+)/m);
  assert.ok(level, `no autonomy.level in ${contractPath}`);
  return level[1];
}

// The trust-ladder's declared start: L0's name in the levels table.
function trustLadderStartLevel() {
  const doc = fs.readFileSync(TRUST_LADDER, 'utf8');
  const l0 = doc.match(/\|\s*L0\s*\|\s*`([^`]+)`\s*\|/);
  assert.ok(l0, 'spec/trust-ladder.md: no L0 row in the levels table');
  return l0[1];
}

function validateContract(contractPath) {
  return spawnSync('bash', [VALIDATOR, contractPath, SCHEMA], { encoding: 'utf8' });
}

test('fresh contract validates against the schema', (t) => {
  const dir = mkFixture(t, 'contract');
  const contract = path.join(dir, '.ai-sdlc.yaml');
  fs.writeFileSync(contract, starterContract());

  const res = validateContract(contract);
  assert.equal(res.status, 0, res.stderr || res.stdout);
  assert.match(res.stdout, /conforms to .*ai-sdlc\.schema\.json/);
});

test('fresh contract yields the trust-ladder start level', (t) => {
  const dir = mkFixture(t, 'trust');
  const contract = path.join(dir, '.ai-sdlc.yaml');
  fs.writeFileSync(contract, starterContract());

  const start = trustLadderStartLevel();
  assert.equal(start, 'shadow',
    'spec/trust-ladder.md: L0 is expected to be `shadow` — the ramp start');
  assert.equal(contractLevel(contract), start,
    'the starter contract must pin the trust-ladder start level');
});

test('bootstrap PR payload carries the documented shape (mocked gh)', (t) => {
  const dir = mkFixture(t, 'payload');
  const repo = 'octo-org/octo-repo';

  // Mock gh layer: a local stub that records argv — no network.
  const binDir = path.join(dir, 'bin');
  fs.mkdirSync(binDir);
  const logFile = path.join(dir, 'gh-calls.jsonl');
  const mockGh = path.join(binDir, 'gh');
  fs.writeFileSync(mockGh, [
    '#!/usr/bin/env node',
    "require('node:fs').appendFileSync(process.env.GH_MOCK_LOG,",
    '  JSON.stringify(process.argv.slice(2)) + \'\\n\');',
    '',
  ].join('\n'), { mode: 0o755 });

  const { payload, ghStatus } = runBootstrapInit({
    repo, dir: path.join(dir, 'repo'), ghBin: mockGh,
    env: { GH_MOCK_LOG: logFile },
  });
  assert.equal(ghStatus, 0, 'mocked gh layer should record the call');

  const calls = fs.readFileSync(logFile, 'utf8')
    .trim().split('\n').map((l) => JSON.parse(l));
  assert.equal(calls.length, 1, 'exactly one gh invocation');
  const argv = calls[0];

  // `gh pr create` against the target repo.
  assert.equal(argv[0], 'pr');
  assert.equal(argv[1], 'create');
  const repoIdx = argv.indexOf('--repo');
  assert.ok(repoIdx > -1 && argv[repoIdx + 1] === repo);

  // Title per docs/onboarding.md: `chore(ai-sdlc): onboard owner/repo`.
  const titleIdx = argv.indexOf('--title');
  assert.equal(argv[titleIdx + 1], `chore(ai-sdlc): onboard ${repo}`);

  // The PR carries the `bootstrap` label (acceptance criterion).
  const labelIdx = argv.indexOf('--label');
  assert.ok(labelIdx > -1, 'no --label on the bootstrap PR');
  assert.equal(argv[labelIdx + 1], 'bootstrap');

  // Body file carries the secrets checklist + label set.
  const bodyIdx = argv.indexOf('--body-file');
  assert.ok(bodyIdx > -1, 'no --body-file on the bootstrap PR');
  const body = fs.readFileSync(argv[bodyIdx + 1], 'utf8');
  for (const token of [
    'AI_SDLC_APP_ID', 'AI_SDLC_APP_PRIVATE_KEY', 'MODEL_API_KEY',
    'AI_SDLC_PROVIDER_URL', 'AI_SDLC_OFF', 'AI_SDLC_FREEZE',
  ]) {
    assert.ok(body.includes(token), `PR body missing secrets-checklist entry ${token}`);
  }
  for (const label of payload.label_set) {
    assert.ok(body.includes(`\`${label}\``), `PR body missing label ${label}`);
  }

  // Canonical label set per docs/onboarding.md + spec/labels.md.
  for (const label of [
    'ready-to-implement', 'needs-refinement', 'needs-human',
    'scc:queued', 'scc:in-progress', 'scc:pr-opened', 'scc:failed',
    'scc:not-ready', 'scc:no-changes',
    'pr:risk-low', 'pr:risk-medium', 'pr:risk-high', 'pr:risk-critical',
    'epic', 'wontfix', 'invalid', 'duplicate', 'question',
    'dependencies', 'umbrella', 'scc:child',
  ]) {
    assert.ok(payload.label_set.includes(label), `label set missing ${label}`);
  }

  // The PR diff carries the contract and the thin caller workflow.
  assert.ok(Object.keys(payload.files).includes('.ai-sdlc.yaml'));
  assert.ok(Object.keys(payload.files).includes('.github/workflows/ai-sdlc.yml'));
});

test('post-merge fixture repo holds trust level shadow and a valid contract', (t) => {
  const dir = mkFixture(t, 'merged');
  const repo = 'octo-org/merged-repo';
  const repoDir = path.join(dir, 'repo');

  // Bootstrap write + mocked gh (records to an unused path).
  const binDir = path.join(dir, 'bin');
  fs.mkdirSync(binDir);
  const mockGh = path.join(binDir, 'gh');
  fs.writeFileSync(mockGh, '#!/usr/bin/env node\n', { mode: 0o755 });

  const { ghStatus } = runBootstrapInit({ repo, dir: repoDir, ghBin: mockGh });
  assert.equal(ghStatus, 0);

  // Merging the bootstrap PR leaves the fixture repo with the payload
  // files — the resulting contract is valid and pinned at shadow.
  const contract = path.join(repoDir, '.ai-sdlc.yaml');
  const res = validateContract(contract);
  assert.equal(res.status, 0, res.stderr || res.stdout);
  assert.equal(contractLevel(contract), 'shadow',
    'trust level after the bootstrap PR merge must equal shadow');

  const workflow = path.join(repoDir, '.github', 'workflows', 'ai-sdlc.yml');
  assert.ok(fs.existsSync(workflow), 'caller workflow missing after merge');
  const wf = fs.readFileSync(workflow, 'utf8');
  assert.match(wf, /ai-sdlc-intake\.yml@main/);
  assert.match(wf, /ai-sdlc-implement\.yml@main/);
});
