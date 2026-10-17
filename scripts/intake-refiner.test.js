// scripts/intake-refiner.test.js — unit + mocked-API tests for the
// conversational intake refiner (issue #197).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  acSection,
  acItems,
  specCheck,
  questionsFor,
  authorReplies,
  issueFingerprint,
  parseMarker,
  findRefinementComment,
  refineIssue,
  upsertComment,
  applyLabels,
  main,
} = require('./intake-refiner');

const readyIssue = (over = {}) => ({
  number: 7,
  title: 'feat(intake): add refinement stage',
  author: { login: 'alice' },
  labels: [],
  body: [
    '## Problem',
    'Intake refuses vague issues silently; authors need conversational guidance toward a DoR-complete spec.',
    '',
    '## Acceptance criteria',
    '- [ ] under-specified issues get a clarifying-questions comment',
    '- [ ] spec-ready issues get derived acceptance criteria',
  ].join('\n'),
  ...over,
});

const thinIssue = (over = {}) => ({
  number: 8,
  title: 'broken thing',
  author: { login: 'alice' },
  labels: [],
  body: 'it does not work',
  ...over,
});

const comment = (id, body, login = 'alice') => ({ id, body, user: { login }, author: { login } });
const marker = (fp, status, armed) =>
  `<!-- ai-sdlc:refinement fp:"${fp}" status:"${status}" armed:"${armed}" -->`;

// --- specCheck / extraction -----------------------------------------------------
describe('specCheck — DoR spec subset', () => {
  it('a DoR-complete issue passes every check', () => {
    const { failed } = specCheck(readyIssue());
    assert.deepEqual(failed, []);
  });

  it('flags non-conventional title, missing AC section and thin body', () => {
    const { failed } = specCheck(thinIssue());
    assert.deepEqual(failed, [
      'R1 conventional title',
      'R2 AC section present',
      'R3 >=1 AC item',
      'R4 non-trivial body',
    ]);
  });

  it('flags TBD/TODO placeholders inside the AC section (R6)', () => {
    const { failed } = specCheck(readyIssue({
      body: 'x'.repeat(200) + '\n\n## Acceptance criteria\n- TBD\n',
    }));
    assert.ok(failed.includes('R6 no TBD in AC'));
    // a TBD line is still a bullet — R3 passes, R6 fails
    assert.ok(!failed.includes('R3 >=1 AC item'));
  });

  it('acSection/acItems extract normalized criteria', () => {
    assert.ok(acSection(readyIssue().body).includes('under-specified'));
    assert.deepEqual(acItems(readyIssue()), [
      'under-specified issues get a clarifying-questions comment',
      'spec-ready issues get derived acceptance criteria',
    ]);
    assert.equal(acSection(readyIssue({ body: 'no sections' }).body), '');
  });
});

// --- fingerprint + replies -------------------------------------------------------
describe('fingerprint — edit and reply re-trigger', () => {
  it('title+body identical → identical fingerprint', () => {
    assert.equal(issueFingerprint(readyIssue(), []), issueFingerprint(readyIssue(), []));
  });

  it('a body edit changes the fingerprint', () => {
    const a = issueFingerprint(readyIssue(), []);
    const b = issueFingerprint(readyIssue({ body: readyIssue().body + '\nmore detail' }), []);
    assert.notEqual(a, b);
  });

  it('an author reply changes the fingerprint; other comments do not', () => {
    const issue = readyIssue();
    const fp0 = issueFingerprint(issue, authorReplies(issue, []));
    const reply = [comment(1, 'ACs: endpoint returns 404 on missing', 'alice')];
    const fp1 = issueFingerprint(issue, authorReplies(issue, reply));
    assert.notEqual(fp0, fp1);
    const outsider = [comment(2, 'drive-by remark', 'bob')];
    const fp2 = issueFingerprint(issue, authorReplies(issue, outsider));
    assert.equal(fp0, fp2);
  });

  it('pipeline marker comments never count as replies', () => {
    const issue = readyIssue();
    const cs = [comment(1, `${marker('abc', 'questions', 'false')}\nquestions…`, 'ai-sdlc-runtime[bot]')];
    assert.deepEqual(authorReplies(issue, cs), []);
  });
});

// --- marker ----------------------------------------------------------------------
describe('marker', () => {
  it('parseMarker round-trips fp/status/armed', () => {
    assert.deepEqual(parseMarker(marker('deadbeef', 'ready', 'true')),
      { fp: 'deadbeef', status: 'ready', armed: true });
    assert.equal(parseMarker('no marker here'), null);
  });

  it('findRefinementComment returns the latest marker comment with id', () => {
    const cs = [
      comment(1, marker('aaaa', 'questions', 'false'), 'bot'),
      comment(2, 'human text', 'alice'),
      comment(3, marker('bbbb', 'ready', 'false'), 'bot'),
    ];
    const found = findRefinementComment(cs);
    assert.equal(found.id, 3);
    assert.equal(found.marker.fp, 'bbbb');
    assert.equal(findRefinementComment([]), null);
  });
});

// --- refineIssue decision ---------------------------------------------------------
describe('refineIssue — decision', () => {
  it('under-specified + unarmed → questions, hold applied, nothing stripped', () => {
    const r = refineIssue({ issue: thinIssue(), comments: [], trigger: 'ready-to-implement', hold: 'scc:not-ready' });
    assert.equal(r.action, 'questions');
    assert.ok(r.questions.length >= 1);
    assert.deepEqual(r.addLabels, ['scc:not-ready']);
    assert.deepEqual(r.removeLabels, []);
    assert.equal(r.armed, false);
    assert.ok(r.comment.includes('status:"questions"'));
  });

  it('under-specified + armed → hold wins, trigger stripped (F2), armed latched', () => {
    const issue = thinIssue({ labels: [{ name: 'ready-to-implement' }] });
    const r = refineIssue({ issue, comments: [], trigger: 'ready-to-implement', hold: 'scc:not-ready' });
    assert.equal(r.action, 'questions');
    assert.equal(r.armed, true);
    assert.deepEqual(r.addLabels, ['scc:not-ready']);
    assert.deepEqual(r.removeLabels, ['ready-to-implement']);
    assert.ok(r.comment.includes('armed:"true"'));
  });

  it('spec-ready + unarmed → derived AC comment, no label mutation', () => {
    const r = refineIssue({ issue: readyIssue(), comments: [], trigger: 'ready-to-implement', hold: 'scc:not-ready' });
    assert.equal(r.action, 'ready');
    assert.equal(r.items.length, 2);
    assert.deepEqual(r.addLabels, []);
    assert.deepEqual(r.removeLabels, []);
    assert.ok(r.comment.includes('- [ ] under-specified issues get a clarifying-questions comment'));
    assert.ok(r.comment.includes('status:"ready"'));
  });

  it('author reply fixes the spec → hold cleared AND trigger restored (T3)', () => {
    const issue = readyIssue({ labels: [{ name: 'scc:not-ready' }] });
    const prior = comment(5, `${marker('aa11bb22', 'questions', 'true')}\nold questions`, 'bot');
    const r = refineIssue({ issue, comments: [prior], trigger: 'ready-to-implement', hold: 'scc:not-ready' });
    assert.equal(r.action, 'ready');
    assert.equal(r.restored, true);
    assert.deepEqual(r.addLabels, ['ready-to-implement']);
    assert.deepEqual(r.removeLabels, ['scc:not-ready']);
    assert.equal(r.priorCommentId, 5);
  });

  it('pass with prior questions but armed=false → hold cleared, trigger NOT added', () => {
    const issue = readyIssue({ labels: [{ name: 'scc:not-ready' }] });
    const prior = comment(5, `${marker('aa11bb22', 'questions', 'false')}\nold questions`, 'bot');
    const r = refineIssue({ issue, comments: [prior], trigger: 'ready-to-implement', hold: 'scc:not-ready' });
    assert.equal(r.action, 'ready');
    assert.equal(r.restored, false);
    assert.deepEqual(r.addLabels, []);
    assert.deepEqual(r.removeLabels, ['scc:not-ready']);
  });

  it('a veto label blocks trigger restoration', () => {
    const issue = readyIssue({ labels: [{ name: 'scc:not-ready' }, { name: 'scc:in-progress' }] });
    const prior = comment(5, `${marker('aa11bb22', 'questions', 'true')}\nold`, 'bot');
    const r = refineIssue({
      issue, comments: [prior],
      trigger: 'ready-to-implement', hold: 'scc:not-ready',
      skipLabels: [], vetoLabels: ['scc:queued', 'scc:in-progress', 'scc:pr-opened'],
    });
    assert.equal(r.action, 'ready');
    assert.equal(r.restored, false);
    assert.equal(r.vetoedBy, 'scc:in-progress');
    assert.deepEqual(r.addLabels, []);
    assert.deepEqual(r.removeLabels, ['scc:not-ready']);
  });

  it('same fingerprint → unchanged, zero writes', () => {
    const issue = thinIssue();
    const fp = issueFingerprint(issue, []);
    const prior = comment(9, `${marker(fp, 'questions', 'false')}\nprev`, 'bot');
    const r = refineIssue({ issue, comments: [prior], trigger: 'ready-to-implement', hold: 'scc:not-ready' });
    assert.equal(r.action, 'unchanged');
    assert.equal(r.comment, undefined);
  });

  it('skip label → skip entirely (no comment, no labels)', () => {
    const issue = thinIssue({ labels: [{ name: 'epic' }] });
    const r = refineIssue({
      issue, comments: [], trigger: 'ready-to-implement', hold: 'scc:not-ready',
      skipLabels: ['epic', 'wontfix', 'needs-human'],
    });
    assert.equal(r.action, 'skip');
    assert.equal(r.comment, undefined);
  });
});

// --- e2e with a mocked gh ----------------------------------------------------------
describe('end-to-end (mocked gh)', () => {
  // Records argv of every gh call; serves canned responses.
  function mockGh({ issue, comments = [] } = {}) {
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      if (argv[0] === 'issue' && argv[1] === 'view') return JSON.stringify(issue);
      if (argv[0] === 'api' && String(argv[2] || '').includes('/comments?')) {
        return JSON.stringify(comments.map((c) => ({ id: c.id, body: c.body, user: { login: c.user ? c.user.login : c.author.login } })));
      }
      if (argv[0] === 'api' && argv[2] === '-X' && argv[3] === 'PATCH') return '{}';
      if (argv[0] === 'issue' && argv[1] === 'comment') return '';
      if (argv[0] === 'issue' && argv[1] === 'edit') return '';
      if (argv[0] === 'label' && argv[1] === 'create') return '';
      throw new Error(`unexpected gh call: ${argv.join(' ')}`);
    };
    return { calls, exec };
  }

  it('under-specified armed issue: one comment POST + hold in, trigger out (one edit)', () => {
    const issue = thinIssue({ labels: [{ name: 'ready-to-implement' }] });
    const { calls, exec } = mockGh({ issue });
    const r = main(
      ['--repo', 'o/r', '--issue', '8', '--trigger', 'ready-to-implement', '--hold', 'scc:not-ready',
        '--skip-labels', '["epic","wontfix","needs-human"]', '--apply', '--json'],
      { exec, stdout: () => {} },
    );
    assert.equal(r.action, 'questions');
    const edit = calls.find((c) => c[0] === 'issue' && c[1] === 'edit');
    assert.ok(edit.includes('--add-label'));
    assert.ok(edit.includes('scc:not-ready'));
    assert.ok(edit.includes('--remove-label'));
    assert.ok(edit.includes('ready-to-implement'));
    const posted = calls.find((c) => c[0] === 'issue' && c[1] === 'comment');
    assert.ok(posted, 'expected one comment create');
    const body = posted[posted.indexOf('--body') + 1];
    assert.ok(body.includes('status:"questions"'));
    assert.ok(body.includes('armed:"true"'));
  });

  it('spec-ready issue after author reply: PATCHes the same comment + restores arm', () => {
    const issue = readyIssue({ labels: [{ name: 'scc:not-ready' }] });
    const priorBody = `${marker('aaaaaaaaaaaaaaaa', 'questions', 'true')}\nold questions`;
    const { calls, exec } = mockGh({
      issue,
      comments: [
        { id: 42, body: priorBody, author: { login: 'ai-sdlc-runtime[bot]' } },
        { id: 43, body: 'Added the ACs you asked for', author: { login: 'alice' } },
      ],
    });
    const r = main(
      ['--repo', 'o/r', '--issue', '7', '--trigger', 'ready-to-implement', '--hold', 'scc:not-ready',
        '--apply', '--json'],
      { exec, stdout: () => {} },
    );
    assert.equal(r.action, 'ready');
    assert.equal(r.restored, true);
    // comment upsert = PATCH against the existing comment id — no second comment
    const patch = calls.find((c) => c[0] === 'api' && String(c[1]).includes('/comments/42'));
    assert.ok(patch, 'expected PATCH on comment 42');
    assert.equal(calls.filter((c) => c[0] === 'issue' && c[1] === 'comment').length, 0);
    const edit = calls.find((c) => c[0] === 'issue' && c[1] === 'edit');
    assert.ok(edit.includes('--add-label'));
    assert.ok(edit.includes('ready-to-implement'));
    assert.ok(edit.includes('--remove-label'));
    assert.ok(edit.includes('scc:not-ready'));
  });

  it('same fingerprint → no comment mutation, no label mutation', () => {
    const issue = thinIssue();
    const fp = issueFingerprint(issue, []);
    const { calls, exec } = mockGh({
      issue,
      comments: [{ id: 9, body: `${marker(fp, 'questions', 'false')}\nprev`, author: { login: 'bot' } }],
    });
    const r = main(
      ['--repo', 'o/r', '--issue', '8', '--apply', '--json'],
      { exec, stdout: () => {} },
    );
    assert.equal(r.action, 'unchanged');
    const mutations = calls.filter((c) =>
      (c[0] === 'issue' && ['comment', 'edit'].includes(c[1])) ||
      (c[0] === 'api' && c.includes('PATCH')) ||
      (c[0] === 'label'));
    assert.equal(mutations.length, 0);
  });

  it('offline --issue-file preview computes without gh calls', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ir-'));
    const f = path.join(dir, 'issue.json');
    fs.writeFileSync(f, JSON.stringify(readyIssue()));
    let out = '';
    const r = main(['--issue-file', f, '--json'], { exec: () => { throw new Error('no gh'); }, stdout: (s) => { out = s; } });
    assert.equal(r.action, 'ready');
    assert.deepEqual(JSON.parse(out).items.length, 2);
  });
});
