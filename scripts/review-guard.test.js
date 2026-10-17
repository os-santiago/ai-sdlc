// scripts/review-guard.test.js — fixture tests for the review-stage guard
// decision (issue #127): marker parsing, external-reviewer ingestion, and
// each row of the decision table the workflow heredoc encoded.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { collectExternal, extractFindings, evaluateGuard, main } = require('./review-guard');

const SHA = 'e'.repeat(40);
const env = (over = {}) => ({
  REPO: 'os-santiago/ai-sdlc',
  PIPELINE_AUTHOR: 'ai-sdlc-runtime[bot]',
  PIPELINE_LABEL: 'ai-sdlc',
  REVIEW_MODE: 'ai',
  EXT: '',
  MAX_FIX: '3',
  SKIP_LABELS: '["needs-human","hold"]',
  ESCALATION: 'needs-human',
  KILL_STATE: 'clear',
  KILL_REASON: '',
  ...over,
});

const pr = (over = {}) => ({
  number: 5, title: 'x', state: 'OPEN', isDraft: false,
  labels: [{ name: 'ai-sdlc' }],
  headRefName: 'fix/issue-5-x', headRefOid: SHA,
  headRepositoryOwner: { login: 'os-santiago' }, isCrossRepository: false,
  baseRefName: 'main', author: { login: 'ai-sdlc-runtime[bot]' }, ...over,
});

const evalG = (p, comments = [], e = {}, extra = {}) =>
  evaluateGuard({ pr: p, comments, runs: [], reviews: [], rcomments: [], env: env(e), ...extra });

describe('evaluateGuard — decision rows', () => {
  it('kill-switch unreadable → skip/kill-switch:unreadable-fail-closed', () => {
    assert.equal(evalG(pr(), [], { KILL_STATE: 'unreadable' }).reason, 'kill-switch:unreadable-fail-closed');
    assert.equal(evalG(pr(), [], { KILL_STATE: 'engaged' }).reason, 'kill-switch:engaged');
  });

  it('state/draft/skip-label/fork → skip reasons', () => {
    assert.equal(evalG(pr({ state: 'CLOSED' })).reason, 'pr-closed');
    assert.equal(evalG(pr({ isDraft: true })).reason, 'draft');
    assert.equal(evalG(pr({ labels: [{ name: 'ai-sdlc' }, { name: 'hold' }] })).reason, 'skip-label');
    assert.equal(evalG(pr({ isCrossRepository: true })).reason, 'fork-head');
  });

  it('non-pipeline author without label → skip/non-pipeline-pr', () => {
    assert.equal(evalG(pr({ author: { login: 'human' }, labels: [] })).reason, 'non-pipeline-pr');
    // Label alone arms it even for human authors.
    assert.equal(evalG(pr({ author: { login: 'human' } })).decision, 'review');
  });

  it('review_mode=human → skip/review-mode-human', () => {
    assert.equal(evalG(pr(), [], { REVIEW_MODE: 'human' }).reason, 'review-mode-human');
  });

  it('clean verdict on head → skip/already-reviewed', () => {
    const comments = [{ body: `<!-- ai-sdlc:review sha=${SHA} verdict=clean -->` }];
    assert.equal(evalG(pr(), comments).reason, 'already-reviewed');
  });

  it('blocked verdict + fix marker for head → skip/fix-already-consumed', () => {
    const comments = [
      { body: `<!-- ai-sdlc:review sha=${SHA} verdict=blocked -->` },
      { body: `<!-- ai-sdlc:review-fix attempt=1 sha=${SHA} -->` },
    ];
    assert.equal(evalG(pr(), comments).reason, 'fix-already-consumed');
  });

  it('blocked verdict + exhausted fix attempts → escalate/fix-iterations-exhausted', () => {
    const comments = [
      { body: `<!-- ai-sdlc:review sha=${SHA} verdict=blocked -->` },
      { body: `<!-- ai-sdlc:review-fix attempt=1 sha=${'1'.repeat(40)} -->` },
      { body: `<!-- ai-sdlc:review-fix attempt=2 sha=${'2'.repeat(40)} -->` },
      { body: `<!-- ai-sdlc:review-fix attempt=3 sha=${'3'.repeat(40)} -->` },
    ];
    const r = evalG(pr(), comments);
    assert.equal(r.decision, 'escalate');
    assert.equal(r.reason, 'fix-iterations-exhausted');
  });

  it('blocked verdict, budget left → fix/resume-blocked-verdict + findings extracted', () => {
    const comments = [{ body: `<!-- ai-sdlc:review sha=${SHA} verdict=blocked -->\n\n\`\`\`json\n{"findings":[{"severity":"blocker","path":"a.js","line":3,"summary":"x"}]}\n\`\`\`` }];
    const r = evalG(pr(), comments);
    assert.equal(r.decision, 'fix');
    assert.equal(r.reason, 'resume-blocked-verdict');
    assert.deepEqual(JSON.parse(r.findings), [{ severity: 'blocker', path: 'a.js', line: 3, summary: 'x' }]);
    assert.equal(r.outputs.verdict, 'blocked');
  });

  it('no verdict → review/new-head (external-blocking when ext signals red)', () => {
    assert.equal(evalG(pr()).reason, 'new-head');
    const runs = [{ id: 1, name: 'CodeRabbit', status: 'completed', conclusion: 'failure', app: 'coderabbit' }];
    const r = evalG(pr(), [], { EXT: 'coderabbit' }, { runs });
    assert.equal(r.decision, 'review');
    assert.equal(r.reason, 'external-blocking');
    assert.equal(r.external.blocking, true);
  });
});

describe('collectExternal', () => {
  it('tolerant slug match across app/name/login; last review per login wins', () => {
    const ext = collectExternal({
      ext: 'coderabbit',
      runs: [{ id: 1, name: 'CodeRabbit', status: 'completed', conclusion: 'failure', app: 'x' }],
      reviews: [
        { login: 'coderabbitai[bot]', state: 'COMMENTED' },
        { login: 'coderabbitai[bot]', state: 'CHANGES_REQUESTED' },
      ],
      rcomments: [{ login: 'coderabbitai[bot]' }, { login: 'human' }],
    });
    assert.equal(ext.blocking, true);
    assert.deepEqual(ext.changes_requested_by, ['coderabbitai[bot]']);
    assert.equal(ext.inline_comments, 1);
  });

  it('no EXT configured → never blocking', () => {
    const ext = collectExternal({ ext: '', runs: [{ name: 'anything', status: 'completed', conclusion: 'failure' }], reviews: [], rcomments: [] });
    assert.equal(ext.blocking, false);
    assert.equal(ext.reviewer, null);
  });
});

describe('extractFindings', () => {
  it('parses the embedded json block; garbage → []', () => {
    assert.equal(extractFindings('no block'), '[]');
    assert.equal(extractFindings('```json\nnot-json\n```'), '[]');
    assert.equal(extractFindings('```json\n{"findings":[{"severity":"major"}]}\n```'), '[{"severity":"major"}]');
  });
});

describe('main', () => {
  it('writes external.json + fix-findings.json and appends outputs', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rg-'));
    fs.writeFileSync(path.join(dir, 'pr.json'), JSON.stringify(pr()));
    const comments = [{ body: `<!-- ai-sdlc:review sha=${SHA} verdict=blocked -->\n\`\`\`json\n{"findings":[]}\n\`\`\`` }];
    fs.writeFileSync(path.join(dir, 'comments.json'), JSON.stringify(comments));
    for (const f of ['check-runs.json', 'reviews.json', 'review-comments.json']) {
      fs.writeFileSync(path.join(dir, f), '[]');
    }
    const gho = path.join(dir, 'out.txt');
    const r = main(['--dir', dir], { env: { ...env(), GITHUB_OUTPUT: gho } });
    assert.equal(r.decision, 'fix');
    assert.equal(fs.readFileSync(path.join(dir, 'fix-findings.json'), 'utf8'), '[]');
    const out = fs.readFileSync(gho, 'utf8');
    assert.match(out, /decision=fix/);
    assert.match(out, /fix_budget_remaining=true/);
  });
});
