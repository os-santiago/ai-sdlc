// scripts/umbrella-coordinator.test.js — node --test coverage for umbrella
// coordination (issue #199): per-repo children with parent linkage +
// marker, single upserted status comment, idempotent re-runs, bounded repo
// list.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');

const uc = require('./umbrella-coordinator');

const HUB = 'hub/coord';
const UMBRELLA_REF = `${HUB}#42`;

function umbrellaIssue(over = {}) {
  return {
    number: 42,
    title: 'feat(fleet): roll out X-Contract-Version header',
    body: 'Umbrella body.',
    labels: [{ name: 'umbrella' }],
    url: `https://github.com/${HUB}/issues/42`,
    ...over,
  };
}

function childIssue(number, repo, itemKey, state = 'open', ref = UMBRELLA_REF) {
  return {
    number, title: `child ${number}`, state,
    url: `https://github.com/${repo}/issues/${number}`,
    body: `body\n<!-- ai-sdlc-umbrella-child key:"${itemKey}" umbrella:"${ref}" -->`,
  };
}

/** In-memory client exposing the same call surface the gh adapter uses. */
function fakeClient({ issues = {}, comments = {}, created = [] } = {}) {
  const calls = { creates: [], comments: [], updates: [] };
  let next = 900;
  return {
    calls,
    getIssue({ repo, number }) {
      const all = [...(issues[`${repo}:open`] || []), ...(issues[`${repo}:closed`] || [])];
      return all.find(i => i.number === number) || null;
    },
    listIssues({ repo, state }) { return issues[`${repo}:${state}`] || []; },
    listComments({ issueNumber }) { return comments[issueNumber] || []; },
    addComment({ issueNumber, body }) { calls.comments.push({ issueNumber, body }); },
    updateComment({ commentId, body }) { calls.updates.push({ commentId, body }); },
    createIssue({ repo, title, body, labels }) {
      const n = created.length ? created.shift() : next++;
      calls.creates.push({ repo, title, body, labels });
      return { number: n, url: `https://github.com/${repo}/issues/${n}` };
    },
  };
}

const opts = (over = {}) => ({
  repo: HUB,
  issue: umbrellaIssue(),
  ...over,
});

describe('declaration parsing', () => {
  it('parses the fenced ai-sdlc:umbrella JSON spec', () => {
    const body = 'prose\n```ai-sdlc:umbrella\n{"items":[{"repo":"o/a","title":"feat(api): x"},"o/b — feat(cli): y"]}\n```\nmore';
    const spec = uc.parseUmbrellaSpec(body);
    assert.strictEqual(spec.items.length, 2);
  });

  it('accepts the legacy hermes:umbrella fence for control-plane interop', () => {
    const spec = uc.parseUmbrellaSpec('```hermes:umbrella\no/a — feat(api): x\n```');
    assert.deepStrictEqual(spec.items, ['o/a — feat(api): x']);
  });

  it('parses bullet declarations: titled, bare, and existing-issue forms', () => {
    const out = uc.declaredItems([
      'intro o/a — not a bullet, ignored',
      '- o/a — feat(api): accept header',
      '- o/b',
      '- o/c#12 — adopt existing',
    ].join('\n'));
    assert.strictEqual(out.length, 3);
    assert.deepStrictEqual(out[0], { repo: 'o/a', issue: null, title: 'feat(api): accept header' });
    assert.deepStrictEqual(out[1], { repo: 'o/b', issue: null, title: null });
    assert.strictEqual(out[2].issue, 12);
  });

  it('rejects path-like and file-like pseudo repos', () => {
    assert.strictEqual(uc.isRepoRef('scripts/foo.js'), false);
    assert.strictEqual(uc.isRepoRef('src/main'), false);
    assert.strictEqual(uc.isRepoRef('o/r'), true);
  });
});

describe('item normalization', () => {
  it('dedupes to one child per repo and bounds the list', () => {
    const { items, dupes, truncated } = uc.normalizeItems(
      ['o/a — t1', 'o/a — t2', 'o/b', 'o/c'],
      { umbrellaTitle: 'u', maxRepos: 2 });
    assert.strictEqual(items.length, 2);
    assert.strictEqual(dupes, 1);
    assert.strictEqual(truncated, 1);
  });

  it('bare repos inherit a conventional-commit umbrella-derived title', () => {
    const item = uc.normalizeItem('o/b', { umbrellaTitle: 'roll out header' });
    assert.match(item.title, /^feat\(fleet\): roll out header$/);
    assert.ok(item.key.length > 0);
  });
});

describe('coordinate — filing', () => {
  it('files exactly one child per declared repo with parent linkage + marker', () => {
    const client = fakeClient();
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/a — feat(api): x\n- o/b — feat(cli): y' }),
    }), client);
    assert.strictEqual(r.action, 'coordinated');
    assert.strictEqual(r.created.length, 2);
    assert.strictEqual(client.calls.creates.length, 2);
    const c = client.calls.creates[0];
    assert.strictEqual(c.repo, 'o/a');
    assert.ok(c.body.includes(`umbrella:"${UMBRELLA_REF}"`));
    assert.ok(c.body.includes('ai-sdlc-umbrella-child'));
    assert.ok(c.body.includes('**Umbrella:**'));       // link back to the hub
    assert.deepStrictEqual(c.labels, ['scc:child']);  // child label, no dispatch trigger
  });

  it('refuses issues without the umbrella label', () => {
    const client = fakeClient();
    const r = uc.coordinate(opts({ issue: umbrellaIssue({ labels: [] }) }), client);
    assert.strictEqual(r.action, 'not-umbrella');
    assert.strictEqual(client.calls.creates.length, 0);
  });

  it('files nothing for non-declared repos', () => {
    const client = fakeClient();
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/a — feat(api): x' }),
    }), client);
    assert.strictEqual(client.calls.creates.length, 1);
    assert.strictEqual(client.calls.creates[0].repo, 'o/a');
    assert.ok(!r.created.some(c => c.repo === 'o/zzz'));
  });
});

describe('coordinate — status comment', () => {
  it('creates a single status comment carrying the checklist', () => {
    const client = fakeClient();
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/a — feat(api): x\n- o/b — feat(cli): y' }),
      now: new Date('2026-10-15T00:00:00Z'),
    }), client);
    assert.strictEqual(r.commentAction, 'created');
    assert.strictEqual(client.calls.comments.length, 1);
    const body = client.calls.comments[0].body;
    assert.ok(body.includes('ai-sdlc-umbrella-status'));
    assert.ok(body.includes('`o/a#'));
    assert.ok(body.includes('`o/b#'));
  });

  it('upserts the existing status comment in place — never stacks', () => {
    const existing = { id: 77, body: `old\n${uc.statusMarker(uc.umbrellaKey(umbrellaIssue()))}` };
    const client = fakeClient({ comments: { 42: [existing, { id: 78, body: 'human note' }] } });
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/a — feat(api): x' }),
    }), client);
    assert.strictEqual(r.commentAction, 'updated');
    assert.deepStrictEqual(client.calls.updates.map(u => u.commentId), [77]);
    assert.strictEqual(client.calls.comments.length, 0);
  });

  it('checks off closed children in the hub checklist', () => {
    const key = uc.itemKeyFor('o/a', 'feat(api): x');
    const client = fakeClient({
      issues: { 'o/a:closed': [childIssue(5, 'o/a', key, 'closed')] },
    });
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/a — feat(api): x' }),
    }), client);
    const row = r.statusBody.split('\n').find(l => l.includes('o/a'));
    assert.ok(row.startsWith('- [x]'), `expected checked row, got: ${row}`);
    assert.ok(row.includes('o/a#5'));
  });
});

describe('coordinate — idempotency + adoption', () => {
  it('adopts a marker-carrying child instead of re-filing', () => {
    const key = uc.itemKeyFor('o/a', 'feat(api): x');
    const client = fakeClient({ issues: { 'o/a:open': [childIssue(9, 'o/a', key)] } });
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/a — feat(api): x' }),
    }), client);
    assert.strictEqual(r.created.length, 0);
    assert.deepStrictEqual(r.adopted, [{ repo: 'o/a', number: 9 }]);
    assert.strictEqual(client.calls.creates.length, 0);
  });

  it('a second pass converges to zero mutations (idempotent)', () => {
    // Simulate run 2: children filed in run 1 are found by marker scan, the
    // status comment already exists → update only.
    const key = uc.itemKeyFor('o/a', 'feat(api): x');
    const status = { id: 50, body: uc.statusMarker(uc.umbrellaKey(umbrellaIssue())) };
    const client = fakeClient({
      issues: { 'o/a:open': [childIssue(9, 'o/a', key)] },
      comments: { 42: [status] },
    });
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/a — feat(api): x' }),
    }), client);
    assert.strictEqual(client.calls.creates.length, 0);
    assert.strictEqual(client.calls.comments.length, 0);
    assert.strictEqual(client.calls.updates.length, 1);
    assert.strictEqual(r.commentAction, 'updated');
  });

  it('adopts a declared existing issue via o/r#N without filing', () => {
    const client = fakeClient({
      issues: { 'o/b:open': [{ number: 12, title: 'existing', state: 'open', body: 'no marker' }] },
    });
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/b#12 — adopt the existing ticket' }),
    }), client);
    assert.strictEqual(client.calls.creates.length, 0);
    assert.deepStrictEqual(r.adopted, [{ repo: 'o/b', number: 12, declared: true }]);
    const row = r.statusBody.split('\n').find(l => l.includes('o/b'));
    assert.ok(row.includes('o/b#12'));
  });

  it('explicit --repos/--spec items win over body declarations', () => {
    const client = fakeClient();
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/body — feat(x): ignored' }),
      items: ['o/flag — feat(api): from flag'],
    }), client);
    assert.strictEqual(client.calls.creates.length, 1);
    assert.strictEqual(client.calls.creates[0].repo, 'o/flag');
  });
});

describe('coordinate — bounds + dry run', () => {
  it('bounds the repo list and reports truncation', () => {
    const client = fakeClient();
    const r = uc.coordinate(opts({
      issue: umbrellaIssue({ body: '- o/a — t\n- o/b — t\n- o/c — t' }),
      maxRepos: 2,
    }), client);
    assert.strictEqual(client.calls.creates.length, 2);
    assert.strictEqual(r.truncated, 1);
    assert.ok(r.statusBody.includes('max-repos'));
  });

  it('dry-run plans children + status without mutating', () => {
    const client = fakeClient();
    const r = uc.coordinate(opts({
      dryRun: true,
      issue: umbrellaIssue({ body: '- o/a — feat(api): x' }),
    }), client);
    assert.strictEqual(r.action, 'dry_run');
    assert.strictEqual(client.calls.creates.length, 0);
    assert.strictEqual(client.calls.comments.length, 0);
    assert.strictEqual(client.calls.updates.length, 0);
    assert.ok(r.statusBody.includes('would file'));
  });

  it('reports no-repos when nothing is declared', () => {
    const client = fakeClient();
    const r = uc.coordinate(opts({ issue: umbrellaIssue({ body: 'no declarations' }) }), client);
    assert.strictEqual(r.action, 'no-repos');
  });
});
