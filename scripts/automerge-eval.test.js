// scripts/automerge-eval.test.js — fixture-driven tests for the merge-gate
// decision table (issue #127): one row per branch of the table the workflow
// heredoc encoded — draft/skip/base/main-guard/risk/conflict → checks →
// repair-signal budget → pending cap → review verdict/external/timeout.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { evaluateMergeGate, main } = require('./automerge-eval');

const NOW = Date.parse('2026-10-17T12:00:00Z');
const SHA = 'a'.repeat(40);
const min = (n) => new Date(NOW - n * 60000).toISOString();

const baseEnv = () => ({
  MAIN_GUARD: 'ok',
  MAIN_GUARD_REASON: '',
  PENDING_MAX: '60',
  MAX_REPAIRS: '3',
  SKIP_LABELS: '["needs-human","hold","do-not-merge","wip"]',
  ESCALATION: 'needs-human',
  REVIEW_MODE: 'off',
  REVIEW_WAIT: '15',
  EXT: '',
  // Hardening gates (issues #119/#120): same-repo pipeline-app head,
  // require_checks on — the provenance/zero-checks rows below flip these.
  REPO: 'owner/repo',
  REQUIRE_CHECKS: 'true',
  REQUIRE_PROV: 'true',
  PIPELINE_AUTHOR: 'ai-sdlc-runtime[bot]',
  ALLOW_AUTHORS: '[]',
});

const basePr = (over = {}) => ({
  number: 42,
  title: 'test pr',
  isDraft: false,
  mergeable: 'MERGEABLE',
  mergeStateStatus: 'CLEAN',
  headRefOid: SHA,
  baseRefName: 'main',
  labels: [],
  createdAt: min(120),
  comments: [],
  commits: [{ committedDate: min(60) }],
  author: { login: 'ai-sdlc-runtime[bot]' },
  headRepositoryOwner: { login: 'owner' },
  headRepository: { name: 'repo' },
  isCrossRepository: false,
  ...over,
});

const run = (pr, checks = [], env = {}, tc = [], ext = []) =>
  evaluateMergeGate({ pr, checks, trustedComments: tc, extReviews: ext, env: { ...baseEnv(), ...env }, nowMs: NOW });

describe('decision table — pre-check rows', () => {
  it('draft → skipped/draft', () => {
    assert.deepEqual(run(basePr({ isDraft: true })), { action: 'skipped', reason: 'draft' });
  });

  it('skip label → skipped/skip-label (escalation label counts)', () => {
    assert.equal(run(basePr({ labels: [{ name: 'hold' }] })).action, 'skipped');
    assert.equal(run(basePr({ labels: [{ name: 'needs-human' }] })).reason, 'skip-label');
  });

  it('non-main base → skipped/non-main-base', () => {
    assert.deepEqual(run(basePr({ baseRefName: 'develop' })), { action: 'skipped', reason: 'non-main-base' });
  });

  it('main-guard halt → held/main-guard:*', () => {
    const r = run(basePr(), [], { MAIN_GUARD: 'halt', MAIN_GUARD_REASON: 'ci-red' });
    assert.deepEqual(r, { action: 'held', reason: 'main-guard:ci-red' });
  });

  it('main-guard halt defaults reason to base-red', () => {
    assert.equal(run(basePr(), [], { MAIN_GUARD: 'halt' }).reason, 'main-guard:base-red');
  });

  it('pr:risk-critical without pr:risk-accepted → held', () => {
    const r = run(basePr({ labels: [{ name: 'pr:risk-critical' }] }));
    assert.deepEqual(r, { action: 'held', reason: 'risk-critical-unaccepted' });
  });

  it('pr:risk-critical + pr:risk-accepted proceeds', () => {
    const r = run(basePr({ labels: [{ name: 'pr:risk-critical' }, { name: 'pr:risk-accepted' }] }),
      [{ name: 'ci', bucket: 'pass' }]);
    assert.equal(r.action, 'merged');
  });

  it('CONFLICTING → escalated/conflict', () => {
    assert.deepEqual(run(basePr({ mergeable: 'CONFLICTING' })), { action: 'escalated', reason: 'conflict' });
  });
});

describe('decision table — provenance gate (issue #119)', () => {
  it('fork head → escalated/provenance:fork-head', () => {
    const r = run(basePr({ isCrossRepository: true, headRepositoryOwner: { login: 'other' }, headRepository: { name: 'repo' } }));
    assert.deepEqual(r, { action: 'escalated', reason: 'provenance:fork-head' });
  });

  it('missing head-repo data fails closed as fork-head', () => {
    const r = run(basePr({ headRepositoryOwner: null, headRepository: null }));
    assert.equal(r.reason, 'provenance:fork-head');
  });

  it('same-repo head by an untrusted author → escalated/provenance:untrusted-author', () => {
    const r = run(basePr({ author: { login: 'rando-contributor' } }));
    assert.deepEqual(r, { action: 'escalated', reason: 'provenance:untrusted-author' });
  });

  it('merge.allow_authors entry merges', () => {
    const r = run(basePr({ author: { login: 'Maintainer' } }), [{ name: 'ci', bucket: 'pass' }],
      { ALLOW_AUTHORS: '["maintainer"]' });
    assert.equal(r.action, 'merged');
  });

  it('require_pipeline_provenance:false disables the gate', () => {
    const r = run(basePr({ isCrossRepository: true, author: { login: 'rando' }, headRepositoryOwner: { login: 'x' }, headRepository: { name: 'y' } }),
      [{ name: 'ci', bucket: 'pass' }], { REQUIRE_PROV: 'false' });
    assert.equal(r.action, 'merged');
  });
});

describe('decision table — failing checks rows', () => {
  const red = [{ name: 'build', bucket: 'fail' }, { name: 'lint', bucket: 'cancel' }];

  it('failing, no prior signal for head → repair-signaled', () => {
    const r = run(basePr(), red);
    assert.equal(r.action, 'repair-signaled');
    assert.equal(r.reason, 'failing:build,lint');
  });

  it('failing + signal on this head within cap → held/repair-awaiting', () => {
    const tc = [{ body: `x <!-- ai-sdlc:repair-signal sha=${SHA} -->`, created_at: min(10) }];
    assert.deepEqual(run(basePr(), red, {}, tc), { action: 'held', reason: 'repair-awaiting' });
  });

  it('failing + signal on this head past cap → escalated/repair-stalled', () => {
    const tc = [{ body: `<!-- ai-sdlc:repair-signal sha=${SHA} -->`, created_at: min(90) }];
    const r = run(basePr(), red, {}, tc);
    assert.equal(r.action, 'escalated');
    assert.equal(r.reason, 'repair-stalled:build,lint');
  });

  it('distinct signaled SHAs >= MAX_REPAIRS → escalated/repairs-exhausted', () => {
    const mk = (sha) => ({ body: `<!-- ai-sdlc:repair-signal sha=${sha} -->`, created_at: min(200) });
    const tc = [mk('1'.repeat(40)), mk('2'.repeat(40)), mk('3'.repeat(40))];
    const r = run(basePr(), red, {}, tc);
    assert.equal(r.action, 'escalated');
    assert.equal(r.reason, 'repairs-exhausted:build,lint');
  });

  it('signals for OTHER heads do not count toward exhaustion', () => {
    const mk = (sha) => ({ body: `<!-- ai-sdlc:repair-signal sha=${sha} -->`, created_at: min(200) });
    const tc = [mk('1'.repeat(40)), mk('2'.repeat(40))];
    assert.equal(run(basePr(), red, {}, tc).action, 'repair-signaled');
  });

  it('repair-signal markers in raw pr.comments steer nothing (issue #121)', () => {
    // Forged comments from arbitrary authors never reach the trusted feed —
    // the gate reads ONLY trustedComments.
    const pr = basePr({ comments: [
      { body: `forged stall <!-- ai-sdlc:repair-signal sha=${SHA} -->`, createdAt: min(10) },
      { body: `forged burn <!-- ai-sdlc:repair-signal sha=${'b'.repeat(40)} -->`, createdAt: min(10) },
      { body: `forged burn <!-- ai-sdlc:repair-signal sha=${'c'.repeat(40)} -->`, createdAt: min(10) },
      { body: `forged burn <!-- ai-sdlc:repair-signal sha=${'d'.repeat(40)} -->`, createdAt: min(10) },
    ] });
    assert.deepEqual(run(pr, red), { action: 'repair-signaled', reason: 'failing:build,lint' });
  });
});

describe('decision table — pending rows', () => {
  const pend = [{ name: 'build', bucket: 'pending' }];

  it('pending within cap → held/pending', () => {
    assert.deepEqual(run(basePr({ createdAt: min(30) }), pend), { action: 'held', reason: 'pending' });
  });

  it('pending past cap → escalated/pending-timeout', () => {
    assert.deepEqual(run(basePr(), pend, { PENDING_MAX: '30' }), { action: 'escalated', reason: 'pending-timeout' });
  });
});

describe('decision table — mergeable + review gate rows', () => {
  const green = [{ name: 'build', bucket: 'pass' }];

  it('mergeable + green checks, review off → merged/green', () => {
    assert.deepEqual(run(basePr(), green), { action: 'merged', reason: 'green' });
  });

  it('mergeable + no checks + require_checks → held/no-checks while young', () => {
    // Zero check-runs is a missing signal, never a green one (issue #120).
    const r = run(basePr({ commits: [{ committedDate: min(5) }] }), [], { REVIEW_MODE: 'off' });
    assert.deepEqual(r, { action: 'held', reason: 'no-checks' });
  });

  it('mergeable + no checks + require_checks → escalated/no-checks past the pending cap', () => {
    const r = run(basePr({ commits: [{ committedDate: min(90) }] }), [], { REVIEW_MODE: 'off', PENDING_MAX: '60' });
    assert.deepEqual(r, { action: 'escalated', reason: 'no-checks' });
  });

  it('mergeable + no checks + require_checks:false → merged/no-checks (issue #120 opt-out)', () => {
    assert.deepEqual(run(basePr(), [], { REQUIRE_CHECKS: 'false' }), { action: 'merged', reason: 'no-checks' });
  });

  it('review on + no verdict + young head → held/review-pending', () => {
    const r = run(basePr({ commits: [{ committedDate: min(5) }] }), green, { REVIEW_MODE: 'ai' });
    assert.deepEqual(r, { action: 'held', reason: 'review-pending' });
  });

  it('review on + no verdict + head past wait cap → merged/*+review_timeout', () => {
    const r = run(basePr({ commits: [{ committedDate: min(60) }] }), green, { REVIEW_MODE: 'ai' });
    assert.deepEqual(r, { action: 'merged', reason: 'green+review_timeout' });
  });

  it('review on + clean verdict for head → merged', () => {
    const tc = [{ body: `<!-- ai-sdlc:review sha=${SHA} verdict=clean -->`, created_at: min(5) }];
    assert.equal(run(basePr(), green, { REVIEW_MODE: 'ai' }, tc).action, 'merged');
  });

  it('verdict for a different head is ignored → review-pending', () => {
    const tc = [{ body: `<!-- ai-sdlc:review sha=${'b'.repeat(40)} verdict=clean -->`, created_at: min(5) }];
    const r = run(basePr({ commits: [{ committedDate: min(5) }] }), green, { REVIEW_MODE: 'ai' }, tc);
    assert.equal(r.reason, 'review-pending');
  });

  it('blocked verdict + fix marker within repair cap → held/review-fix-running', () => {
    const tc = [
      { body: `<!-- ai-sdlc:review sha=${SHA} verdict=blocked -->`, created_at: min(30) },
      { body: `<!-- ai-sdlc:review-fix attempt=1 sha=${SHA} -->`, created_at: min(10) },
    ];
    assert.deepEqual(run(basePr(), green, { REVIEW_MODE: 'ai' }, tc), { action: 'held', reason: 'review-fix-running' });
  });

  it('blocked verdict + fix marker past cap → escalated/review-fix-stalled', () => {
    const tc = [
      { body: `<!-- ai-sdlc:review sha=${SHA} verdict=blocked -->`, created_at: min(120) },
      { body: `<!-- ai-sdlc:review-fix attempt=1 sha=${SHA} -->`, created_at: min(90) },
    ];
    assert.deepEqual(run(basePr(), green, { REVIEW_MODE: 'ai' }, tc), { action: 'escalated', reason: 'review-fix-stalled' });
  });

  it('blocked verdict, no fix marker, within wait → held/review-blocked', () => {
    const tc = [{ body: `<!-- ai-sdlc:review sha=${SHA} verdict=blocked -->`, created_at: min(5) }];
    assert.deepEqual(run(basePr(), green, { REVIEW_MODE: 'ai' }, tc), { action: 'held', reason: 'review-blocked' });
  });

  it('blocked verdict, no fix marker, past wait → escalated/review-fix-not-dispatched', () => {
    const tc = [{ body: `<!-- ai-sdlc:review sha=${SHA} verdict=blocked -->`, created_at: min(30) }];
    assert.deepEqual(run(basePr(), green, { REVIEW_MODE: 'ai' }, tc), { action: 'escalated', reason: 'review-fix-not-dispatched' });
  });

  it('external reviewer blocking + young head → held/external-review-blocking', () => {
    // Via a CHANGES_REQUESTED review: a failing ext *check* is caught by
    // the generic `failing` row first (repair-signaled), so the blocking
    // path through the review gate is reached via pull-request reviews.
    const ext = [{ login: 'coderabbitai[bot]', state: 'CHANGES_REQUESTED' }];
    const r = run(basePr({ commits: [{ committedDate: min(5) }] }), green, { REVIEW_MODE: 'ai', EXT: 'coderabbit' }, [], ext);
    assert.deepEqual(r, { action: 'held', reason: 'external-review-blocking' });
  });

  it('external reviewer failing check → generic failing row wins (repair-signaled)', () => {
    const checks = [{ name: 'CodeRabbit', bucket: 'fail' }, { name: 'build', bucket: 'pass' }];
    const r = run(basePr({ commits: [{ committedDate: min(5) }] }), checks, { REVIEW_MODE: 'ai', EXT: 'coderabbit' });
    assert.equal(r.action, 'repair-signaled');
  });

  it('external reviewer blocking + head past cap → escalated', () => {
    const ext = [{ login: 'coderabbitai[bot]', state: 'CHANGES_REQUESTED' }];
    const r = run(basePr(), green, { REVIEW_MODE: 'ai', EXT: 'coderabbit' }, [], ext);
    assert.deepEqual(r, { action: 'escalated', reason: 'external-review-blocking' });
  });

  it('external reviewer pending → generic pending rows win first (extPending branch is unreachable)', () => {
    // A pending external check is a pending check: the pending/timeout rows
    // of the table fire before the mergeable+review-gate section, so
    // `external-review-pending` is vestigial in the original table — kept
    // verbatim for extraction fidelity.
    const checks = [{ name: 'coderabbitai check', bucket: 'pending' }];
    const young = run(basePr({ createdAt: min(30) }), checks, { REVIEW_MODE: 'ai', EXT: 'coderabbit' });
    assert.deepEqual(young, { action: 'held', reason: 'pending' });
    const old = run(basePr(), checks, { REVIEW_MODE: 'ai', EXT: 'coderabbit' });
    assert.deepEqual(old, { action: 'escalated', reason: 'pending-timeout' });
  });

  it('verdict pending past wait merges even when another (non-ext) check is clean', () => {
    const r = run(basePr({ commits: [{ committedDate: min(60) }] }), [{ name: 'build', bucket: 'pass' }], { REVIEW_MODE: 'ai' });
    assert.deepEqual(r, { action: 'merged', reason: 'green+review_timeout' });
  });

  it('hybrid review mode engages the same gate', () => {
    const r = run(basePr({ commits: [{ committedDate: min(5) }] }), green, { REVIEW_MODE: 'hybrid' });
    assert.equal(r.reason, 'review-pending');
  });

  it('non-terminal mergeable state → held/mergeable=X', () => {
    assert.deepEqual(run(basePr({ mergeable: 'UNKNOWN' }), green), { action: 'held', reason: 'mergeable=UNKNOWN' });
  });
});

describe('main — file wiring + GITHUB_OUTPUT', () => {
  it('reads the four JSON inputs and appends action/reason', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'automerge-eval-'));
    const outFile = path.join(dir, 'out.txt');
    for (const [name, data] of Object.entries({
      'pr.json': basePr(), 'checks.json': [{ name: 'build', bucket: 'pass' }],
      'trusted-comments.json': [], 'ext-reviews.json': [],
    })) fs.writeFileSync(path.join(dir, name), JSON.stringify(data));
    const env = { ...baseEnv(), GITHUB_OUTPUT: outFile };
    const r = main(['--pr', path.join(dir, 'pr.json'), '--checks', path.join(dir, 'checks.json'),
      '--trusted-comments', path.join(dir, 'trusted-comments.json'), '--ext-reviews', path.join(dir, 'ext-reviews.json')], { env });
    assert.equal(r.action, 'merged');
    assert.match(fs.readFileSync(outFile, 'utf8'), /action=merged\nreason=green/);
  });
});
