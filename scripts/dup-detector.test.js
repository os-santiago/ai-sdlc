// scripts/dup-detector.test.js — unit + mocked-API tests for the intake
// duplicate detector (issue #161, epic #147).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  tokenize,
  jaccard,
  stripBoilerplate,
  signatureHashOf,
  markerKeysOf,
  similarity,
  findMatches,
  parseScanMarker,
  findScanComment,
  matchFingerprint,
  detectIssue,
  sweepBacklog,
  main,
} = require('./dup-detector');

const issue = (over = {}) => ({
  number: 10,
  title: 'feat(intake): deduplicate new issues against the open backlog',
  body: [
    '## Problem',
    'Duplicate issues split discussion and dispatch work twice.',
    '',
    '## Acceptance criteria',
    '- [ ] new issues are compared against the open backlog',
  ].join('\n'),
  url: 'https://example.test/issues/10',
  ...over,
});

const candidate = (over = {}) => ({
  number: 42,
  title: 'feat(intake): deduplicate new issues against the open backlog',
  body: 'Duplicate issues split discussion and dispatch work twice over.',
  url: 'https://example.test/issues/42',
  ...over,
});

const comment = (id, body, login = 'bot') => ({ id, body, user: { login }, author: { login } });
const marker = (fp) => `<!-- ai-sdlc:dup-scan fp:"${fp}" -->`;

// --- tokenize / jaccard / boilerplate ---------------------------------------------
describe('tokenize / jaccard', () => {
  it('lowercases, drops stopwords and strips code blocks', () => {
    const t = tokenize('Fix THE Bug ```const x = secretToken()``` in Intake');
    assert.ok(t.has('fix') === false); // 'fix' is a stopword (conventional-type)
    assert.ok(t.has('intake'));
    assert.ok(!t.has('secrettoken'));
    assert.ok(!t.has('the'));
  });

  it('keeps single digits so numbered work items stay distinct', () => {
    const t = tokenize('task 1 vs task 2');
    assert.ok(t.has('1') && t.has('2'));
  });

  it('jaccard is a symmetric 0..1 coefficient', () => {
    const a = new Set(['x', 'y', 'z']);
    const b = new Set(['y', 'z', 'w']);
    assert.equal(jaccard(a, b), 0.5);
    assert.equal(jaccard(b, a), 0.5);
    assert.equal(jaccard(a, new Set()), 0);
  });

  it('stripBoilerplate removes markers, headings, tables and task lists', () => {
    const body = [
      '<!-- ai-sdlc-failure-signature: deadbeef -->',
      '## Problem',
      '| col | col |',
      '- [ ] checklist item',
      'real free text stays',
    ].join('\n');
    const stripped = stripBoilerplate(body);
    assert.ok(!stripped.includes('deadbeef'));
    assert.ok(!stripped.includes('Problem'));
    assert.ok(!stripped.includes('|'));
    assert.ok(!stripped.includes('checklist'));
    assert.ok(stripped.includes('real free text stays'));
  });
});

// --- marker keys --------------------------------------------------------------------
describe('marker keys', () => {
  it('reads the signature comment and the meta-blob fallback', () => {
    assert.equal(
      signatureHashOf('x <!-- ai-sdlc-failure-signature: AbCdEf12 --> y'),
      'abcdef12',
    );
    assert.equal(
      signatureHashOf('<!-- ai-sdlc-postmortem-meta: {"sigHash":"Cafe1234"} -->'),
      'cafe1234',
    );
    assert.equal(signatureHashOf('<!-- ai-sdlc-postmortem-meta: {oops -->'), null);
    assert.equal(signatureHashOf('plain body'), null);
  });

  it('collects sig, epic-child and sub marker keys', () => {
    const keys = markerKeysOf([
      '<!-- ai-sdlc-failure-signature: aa11bb22 -->',
      '<!-- hermes:epic-child key:"dup-detection" parent:#147 -->',
      '<!-- hermes:sub:part-2 -->',
    ].join('\n'));
    assert.ok(keys.has('sig:aa11bb22'));
    assert.ok(keys.has('child:147:dup-detection'));
    assert.ok(keys.has('sub:part-2'));
  });
});

// --- similarity / findMatches --------------------------------------------------------
describe('similarity + findMatches', () => {
  it('identical titles score high; unrelated issues score ~0', () => {
    const s = similarity(issue(), candidate());
    assert.ok(s >= 0.6, `expected >= 0.6, got ${s}`);
    const far = similarity(issue(), candidate({
      title: 'chore(release): bump pipeline dependencies',
      body: 'Quarterly dependency refresh for workflow actions.',
    }));
    assert.ok(far < 0.3, `expected < 0.3, got ${far}`);
  });

  it('excludes self by number and partitions duplicates vs related', () => {
    const { duplicates, related } = findMatches(issue(), [
      issue(), // self — must never match itself
      candidate(), // near-identical → duplicate
      candidate({ number: 7, title: 'fix(cli): unrelated flag parsing', body: 'different topic entirely about flags' }),
    ]);
    assert.deepEqual(duplicates.map((d) => d.number), [42]);
    assert.ok(!related.some((r) => r.number === 10));
  });

  it('a shared postmortem signature is a certain duplicate (score 1)', () => {
    const a = issue({ body: '<!-- ai-sdlc-failure-signature: aa11bb22cc33dd44 -->\n## Problem\nlint blew up' });
    const b = candidate({ body: 'different prose entirely <!-- ai-sdlc-failure-signature: aa11bb22cc33dd44 -->' });
    const { duplicates } = findMatches(a, [b]);
    assert.equal(duplicates.length, 1);
    assert.equal(duplicates[0].score, 1);
    assert.deepEqual(duplicates[0].via, ['sig:aa11bb22cc33dd44']);
  });

  it('different signatures never flag — each filing is canonical (postmortem dedup contract)', () => {
    const a = issue({ body: '## Problem\nintake crashed hard\n<!-- ai-sdlc-failure-signature: aa11bb22 -->' });
    const b = candidate({ title: issue().title, body: '## Problem\nintake crashed hard\n<!-- ai-sdlc-failure-signature: ff99ee88 -->' });
    const { duplicates, related } = findMatches(a, [b]);
    assert.equal(duplicates.length, 0);
    assert.equal(related.length, 0);
  });

  it('a repeated epic-child key for the same parent is a certain duplicate', () => {
    const mk = (n) => ({
      number: n,
      title: `feat: different title ${n}`,
      body: '<!-- hermes:epic-child key:"dup-detection" parent:#147 -->\ndistinct prose',
    });
    const { duplicates } = findMatches(mk(1), [mk(2)]);
    assert.equal(duplicates.length, 1);
    assert.deepEqual(duplicates[0].via, ['child:147:dup-detection']);
  });

  it('same child key under a DIFFERENT parent is not a marker match', () => {
    const a = issue({ body: '<!-- hermes:epic-child key:"x" parent:#1 -->' });
    const b = candidate({ body: '<!-- hermes:epic-child key:"x" parent:#2 -->' });
    const { duplicates } = findMatches(a, [b]);
    // falls back to text similarity — titles here are identical so it may
    // still score, but never via the marker key
    if (duplicates.length) assert.equal(duplicates[0].via, undefined);
  });

  it('shared template chrome alone cannot push a pair over threshold', () => {
    const template = [
      '## Problem', '## Context', '## Acceptance criteria',
      '- [ ] item', '<!-- ai-sdlc:generated-by: pipeline -->',
    ].join('\n');
    const a = issue({ title: 'fix(api): null token handling', body: `${template}\nnull tokens crash` });
    const b = candidate({ title: 'feat(ui): dark mode toggle', body: `${template}\npalette swap` });
    const { duplicates } = findMatches(a, [b]);
    assert.equal(duplicates.length, 0);
  });
});

// --- marker comment + fingerprint ------------------------------------------------------
describe('scan marker', () => {
  it('parseScanMarker round-trips the fingerprint', () => {
    assert.deepEqual(parseScanMarker(`${marker('deadbeefcafe1234')}\nbody`), { fp: 'deadbeefcafe1234' });
    assert.equal(parseScanMarker('no marker'), null);
  });

  it('findScanComment returns the latest marker comment with REST id', () => {
    const cs = [
      comment(1, marker('aaaa'), 'bot'),
      comment(2, 'human remark', 'alice'),
      comment(3, marker('bbbb'), 'bot'),
    ];
    const found = findScanComment(cs);
    assert.equal(found.id, 3);
    assert.equal(found.marker.fp, 'bbbb');
    assert.equal(findScanComment([]), null);
  });

  it('matchFingerprint is stable for an identical match set and moves on change', () => {
    const d = [{ number: 42, score: 0.8 }];
    const r = [{ number: 7, score: 0.4 }];
    assert.equal(matchFingerprint(d, r), matchFingerprint([{ number: 42, score: 0.8 }], r));
    assert.notEqual(matchFingerprint(d, r), matchFingerprint(d, []));
  });
});

// --- detectIssue decision ------------------------------------------------------------------
describe('detectIssue — decision', () => {
  it('no matches and no prior comment → none (zero writes)', () => {
    const r = detectIssue({ issue: issue(), candidates: [], comments: [] });
    assert.equal(r.action, 'none');
    assert.equal(r.comment, undefined);
  });

  it('a duplicate above threshold → report with an advisory comment', () => {
    const r = detectIssue({ issue: issue(), candidates: [candidate()], comments: [] });
    assert.equal(r.action, 'report');
    assert.ok(r.duplicates.some((d) => d.number === 42));
    assert.ok(r.comment.includes('ai-sdlc:dup-scan'));
    assert.ok(r.comment.includes('#42'));
    // Advisory contract: never auto-close, never label-block.
    assert.ok(r.comment.includes('advisory'));
    assert.ok(!/closed this issue/i.test(r.comment));
  });

  it('related-only matches stay silent — no comment noise', () => {
    const weak = candidate({ number: 7, title: 'feat(intake): backlog hygiene notes', body: 'loosely similar words about the backlog' });
    const r = detectIssue({ issue: issue(), candidates: [weak], comments: [], relatedThreshold: 0.01 });
    // related band only (score < dup threshold) → no report
    if (r.related.length) {
      assert.equal(r.action, 'none');
      assert.equal(r.comment, undefined);
    }
  });

  it('same fingerprint → unchanged (idempotent upsert)', () => {
    const first = detectIssue({ issue: issue(), candidates: [candidate()], comments: [] });
    const prior = comment(9, `${marker(first.fingerprint)}\nold scan`, 'bot');
    const again = detectIssue({ issue: issue(), candidates: [candidate()], comments: [prior] });
    assert.equal(again.action, 'unchanged');
    assert.equal(again.priorCommentId, 9);
    assert.equal(again.comment, undefined);
  });

  it('a changed match set re-renders against the prior comment id', () => {
    const first = detectIssue({ issue: issue(), candidates: [candidate()], comments: [] });
    const prior = comment(9, `${marker(first.fingerprint)}\nold scan`, 'bot');
    const r = detectIssue({
      issue: issue(),
      candidates: [candidate(), candidate({ number: 77 })],
      comments: [prior],
    });
    assert.equal(r.action, 'report');
    assert.equal(r.priorCommentId, 9);
    assert.ok(r.comment.includes('#77'));
  });

  it('matches gone but a stale scan comment exists → clear rewrites it', () => {
    const prior = comment(9, `${marker('aa11')}\nold scan`, 'bot');
    const r = detectIssue({ issue: issue(), candidates: [], comments: [prior] });
    assert.equal(r.action, 'clear');
    assert.equal(r.priorCommentId, 9);
    assert.ok(r.comment.includes('no longer'));
  });
});

// --- sweep --------------------------------------------------------------------------------------
describe('sweepBacklog — pairwise backlog scan', () => {
  it('reports each unordered pair once, duplicate-flagged', () => {
    const a = issue({ number: 1 });
    const b = candidate({ number: 2 });
    const c = issue({ number: 3, title: 'chore: totally different', body: 'unrelated words' });
    const { pairs } = sweepBacklog({ candidates: [a, b, c], opts: { dupThreshold: 0.6, relatedThreshold: 0.3 } });
    assert.equal(pairs.length, 1);
    assert.deepEqual({ a: pairs[0].a, b: pairs[0].b }, { a: 1, b: 2 });
    assert.equal(pairs[0].duplicate, true);
  });
});

// --- CLI (offline + mocked exec) --------------------------------------------------------------------
describe('main — CLI', () => {
  const tmpdir = () => fs.mkdtempSync(path.join(os.tmpdir(), 'dup-detector-'));
  const writeJson = (dir, name, obj) => {
    const p = path.join(dir, name);
    fs.writeFileSync(p, JSON.stringify(obj));
    return p;
  };

  it('offline preview emits the decision without comment body', () => {
    const dir = tmpdir();
    const out = [];
    const result = main([
      '--issue-file', writeJson(dir, 'issue.json', issue()),
      '--candidates-file', writeJson(dir, 'cand.json', [candidate()]),
      '--json',
    ], { stdout: (s) => out.push(s) });
    assert.equal(result.action, 'report');
    assert.equal(result.comment, undefined); // preview redacts the body
    assert.deepEqual(JSON.parse(out[0]).duplicates.map((d) => d.number), [42]);
  });

  it('--apply POSTs a new comment when no prior marker exists', () => {
    const dir = tmpdir();
    const calls = [];
    const exec = (argv) => { calls.push(argv); return ''; };
    const result = main([
      '--repo', 'o/r', '--apply',
      '--issue-file', writeJson(dir, 'issue.json', issue()),
      '--candidates-file', writeJson(dir, 'cand.json', [candidate()]),
    ], { exec, stdout: () => {} });
    assert.equal(result.action, 'report');
    assert.equal(result.applied.comment.op, 'create');
    const post = calls.find((c) => c[0] === 'issue' && c[1] === 'comment');
    assert.ok(post, 'expected an `issue comment` call');
    assert.ok(post.join(' ').includes('ai-sdlc:dup-scan'));
  });

  it('--apply PATCHes the prior marker comment (upsert, never double-post)', () => {
    const dir = tmpdir();
    const first = detectIssue({ issue: issue(), candidates: [candidate()], comments: [] });
    const prior = comment(55, `${marker('differentfp')}\nold`, 'bot');
    const calls = [];
    const exec = (argv) => { calls.push(argv); return ''; };
    const result = main([
      '--repo', 'o/r', '--apply',
      '--issue-file', writeJson(dir, 'issue.json', issue()),
      '--candidates-file', writeJson(dir, 'cand.json', [candidate()]),
      '--comments-file', writeJson(dir, 'comments.json', [prior]),
    ], { exec, stdout: () => {} });
    assert.equal(result.action, 'report');
    assert.equal(result.applied.comment.op, 'patch');
    assert.equal(result.applied.comment.id, 55);
    const patch = calls.find((c) => c[0] === 'api' && c.includes('PATCH'));
    assert.ok(patch && patch[1].includes('issues/comments/55'));
    assert.ok(!calls.some((c) => c[0] === 'issue' && c[1] === 'comment'), 'must not double-post');
    void first;
  });

  it('live mode fetches issue + backlog + comments via gh', () => {
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      if (argv[0] === 'issue' && argv[1] === 'view') return JSON.stringify(issue());
      if (argv[0] === 'issue' && argv[1] === 'list') return JSON.stringify([candidate()]);
      if (argv[0] === 'api') return '[]';
      return '';
    };
    const result = main(['--repo', 'o/r', '--issue', '10', '--json'], { exec, stdout: () => {} });
    assert.equal(result.action, 'report');
    assert.ok(calls.some((c) => c[0] === 'issue' && c[1] === 'list' && c.includes('--state') && c.includes('open')));
  });
});
