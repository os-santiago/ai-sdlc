// scripts/ci-repair-guard.test.js — fixture tests for the ci-repair guard
// + attempt-budget decision (issue #127): one row per branch of the
// decision table the workflow heredoc encoded.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { collectChecks, evaluateGuard, main } = require('./ci-repair-guard');
const { signatureFor } = require('./repair-stall-guard');

const SHA = 'c'.repeat(40);
const env = (over = {}) => ({
  REPO: 'os-santiago/ai-sdlc',
  BRANCH_PATTERN: '^(feat|fix)/issue-[0-9]+',
  MAX_REPAIRS: '3',
  MAX_CHECKS: '5',
  STALL_MAX: '3',
  ...over,
});

const pr = (over = {}) => ({
  number: 9, title: 'x', state: 'OPEN', isDraft: false, labels: [],
  headRefName: 'fix/issue-9-x', headRefOid: SHA,
  headRepositoryOwner: { login: 'os-santiago' }, isCrossRepository: false,
  baseRefName: 'main', ...over,
});

const redRun = { id: 1, name: 'ci', status: 'completed', conclusion: 'failure', app: 'github-actions', url: 'u', output: {} };

describe('collectChecks', () => {
  it('latest run per name wins; statuses join as red signal', () => {
    const { failing, pending } = collectChecks({
      runs: [
        { id: 1, name: 'ci', status: 'completed', conclusion: 'failure' },
        { id: 2, name: 'ci', status: 'completed', conclusion: 'success' },   // re-run green
        { id: 3, name: 'lint', status: 'completed', conclusion: 'timed_out' },
        { id: 4, name: 'e2e', status: 'in_progress', conclusion: null },
      ],
      statuses: [{ context: 'external/bot', state: 'failure', description: 'nope', target_url: 'x' }],
    });
    assert.deepEqual(failing.map((f) => f.name).sort(), ['external/bot', 'lint']);
    assert.deepEqual(pending, ['e2e']);
    const status = failing.find((f) => f.kind === 'status');
    assert.equal(status.output.summary, 'nope');
  });
});

describe('evaluateGuard — decision rows', () => {
  const failingRuns = [redRun];

  it('closed PR → skip/pr-merged', () => {
    const r = evaluateGuard({ pr: pr({ state: 'MERGED' }), markers: [], runs: failingRuns, statuses: [], env: env() });
    assert.equal(r.decision, 'skip');
    assert.equal(r.reason, 'pr-merged');
  });

  it('draft → skip/draft', () => {
    assert.equal(evaluateGuard({ pr: pr({ isDraft: true }), markers: [], runs: failingRuns, statuses: [], env: env() }).reason, 'draft');
  });

  it('skip label → skip/skip-label', () => {
    const r = evaluateGuard({ pr: pr({ labels: [{ name: 'needs-human' }] }), markers: [], runs: failingRuns, statuses: [], env: env() });
    assert.equal(r.reason, 'skip-label');
  });

  it('fork head → skip/fork-head', () => {
    for (const over of [{ isCrossRepository: true }, { headRepositoryOwner: { login: 'someone-else' } }]) {
      assert.equal(evaluateGuard({ pr: pr(over), markers: [], runs: failingRuns, statuses: [], env: env() }).reason, 'fork-head');
    }
  });

  it('branch outside pattern → skip/non-pipeline-branch', () => {
    const r = evaluateGuard({ pr: pr({ headRefName: 'random-branch' }), markers: [], runs: failingRuns, statuses: [], env: env() });
    assert.equal(r.reason, 'non-pipeline-branch');
  });

  it('no failing checks → skip/no-failing-checks', () => {
    const r = evaluateGuard({ pr: pr(), markers: [], runs: [{ ...redRun, conclusion: 'success' }], statuses: [], env: env() });
    assert.equal(r.reason, 'no-failing-checks');
  });

  it('attempt marker on this head → skip/head-already-attempted', () => {
    const r = evaluateGuard({ pr: pr(), markers: [{ attempt: '1', sha: SHA }], runs: failingRuns, statuses: [], env: env() });
    assert.equal(r.reason, 'head-already-attempted');
  });

  it('attempts >= max → escalate/repairs-exhausted', () => {
    const markers = [{ attempt: '1', sha: '1'.repeat(40) }, { attempt: '2', sha: '2'.repeat(40) }, { attempt: '3', sha: '3'.repeat(40) }];
    const r = evaluateGuard({ pr: pr(), markers, runs: failingRuns, statuses: [], env: env() });
    assert.equal(r.decision, 'escalate');
    assert.equal(r.reason, 'repairs-exhausted');
  });

  it('stall signature over the burn cap → escalate/repair-stall:<sig>', () => {
    const sig = signatureFor(['ci']);
    const markers = [{ attempt: '1', sha: '1'.repeat(40), sig }, { attempt: '2', sha: '2'.repeat(40), sig }, { attempt: '3', sha: '3'.repeat(40), sig }];
    // Without the guard module the flat budget would fire; with the real
    // sibling module the stall row wins evaluation order.
    const r = evaluateGuard({
      pr: pr(), markers, runs: failingRuns, statuses: [], env: env({ MAX_REPAIRS: '9' }),
      stallGuardPath: path.join(__dirname, 'repair-stall-guard.js'),
    });
    assert.equal(r.decision, 'escalate');
    assert.equal(r.reason, `repair-stall:${sig}`);
    assert.equal(r.outputs.stall_sig, sig);
  });

  it('healthy failing PR → repair/failing-checks + full outputs', () => {
    const r = evaluateGuard({ pr: pr(), markers: [], runs: failingRuns, statuses: [], env: env() });
    assert.equal(r.decision, 'repair');
    assert.equal(r.reason, 'failing-checks');
    assert.equal(r.outputs.attempt, 1);
    assert.equal(r.outputs.head_sha, SHA);
    assert.equal(r.outputs.failing, 'ci');
  });
});

describe('main — work-dir wiring', () => {
  it('reads the four inputs, writes failing/pending, appends outputs', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'crg-'));
    fs.writeFileSync(path.join(dir, 'pr.json'), JSON.stringify(pr()));
    fs.writeFileSync(path.join(dir, 'markers.json'), '[]');
    fs.writeFileSync(path.join(dir, 'check-runs.json'), JSON.stringify([redRun]));
    fs.writeFileSync(path.join(dir, 'statuses.json'), '[]');
    const gho = path.join(dir, 'out.txt');
    const r = main(['--dir', dir], { env: { ...env(), GITHUB_OUTPUT: gho } });
    assert.equal(r.decision, 'repair');
    const failing = JSON.parse(fs.readFileSync(path.join(dir, 'failing.json'), 'utf8'));
    assert.equal(failing[0].name, 'ci');
    assert.match(fs.readFileSync(gho, 'utf8'), /decision=repair\nreason=failing-checks/);
  });

  it('caps failing.json at MAX_CHECKS', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'crg-'));
    fs.writeFileSync(path.join(dir, 'pr.json'), JSON.stringify(pr()));
    fs.writeFileSync(path.join(dir, 'markers.json'), '[]');
    fs.writeFileSync(path.join(dir, 'check-runs.json'), JSON.stringify(
      [1, 2, 3].map((i) => ({ ...redRun, id: i, name: `c${i}` }))));
    fs.writeFileSync(path.join(dir, 'statuses.json'), '[]');
    main(['--dir', dir], { env: env({ MAX_CHECKS: '2' }) });
    assert.equal(JSON.parse(fs.readFileSync(path.join(dir, 'failing.json'), 'utf8')).length, 2);
  });
});
