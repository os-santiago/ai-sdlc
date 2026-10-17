// scripts/epic-decomposer.test.js — node --test coverage for epic →
// scc:child decomposition (issue #198, epic #147). All GitHub calls go
// through an injected fake client — no `gh` invocation in tests.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');

const dec = require('./epic-decomposer');

const REPO = 'o/r';

const EPIC_BODY = `## Goal

Umbrella goal text.

## Scope

- Conversational refinement: clarifying questions on under-specified issues
- Epic decomposition: epic label to scc:child sub-issues with ACs
  - Children carry a parent marker
  - Parent gets a live checklist
- #229 — already-filed child issue

## Acceptance criteria (epic-level)

- [ ] Under-specified issue → refinement comment + blocked label
- [ ] epic-labeled issue → decomposed children filed with ACs
`;

function epic(over = {}) {
  return {
    number: 100,
    title: '[EPIC] Intake intelligence — refinement, dedup, prioritization',
    body: EPIC_BODY,
    state: 'open',
    labels: [{ name: 'epic' }, { name: 'priority:P2' }],
    url: 'u/100',
    ...over,
  };
}

/** In-memory client exposing the same call surface the gh adapter uses. */
function fakeClient({ issue = epic(), open = [], closed = [] } = {}) {
  const store = { open: [...open], closed: [...closed] };
  const calls = { creates: [], ensured: [], bodyUpdates: [] };
  let next = 900;
  return {
    calls,
    getIssue() { return { ...issue, body: calls.bodyUpdates.length ? calls.bodyUpdates[calls.bodyUpdates.length - 1].body : issue.body }; },
    listIssues({ state }) { return state === 'open' ? store.open : store.closed; },
    ensureLabel({ label }) { calls.ensured.push(label); },
    createIssue({ title, body, labels }) {
      const rec = { number: next, title, body, labels, state: 'open', url: `u/${next}` };
      next += 1;
      calls.creates.push(rec);
      store.open.push(rec);
      return rec;
    },
    updateIssueBody({ issueNumber, body }) { calls.bodyUpdates.push({ issueNumber, body }); },
  };
}

const opts = (over = {}) => ({ repo: REPO, issueNumber: 100, ...over });

describe('slugifyKey / markers', () => {
  it('produces bounded lowercase hyphen slugs', () => {
    assert.strictEqual(dec.slugifyKey('Epic decomposition: `epic` label → scc:child'), 'epic-decomposition-epic-label-scc-child');
    const long = dec.slugifyKey('x'.repeat(200) + '   ' + 'y'.repeat(50));
    assert.ok(long.length <= 48);
    assert.ok(!long.endsWith('-'));
    assert.strictEqual(dec.slugifyKey('!!!'), 'child');
  });

  it('stamps the canonical hermes:epic-child marker format', () => {
    const m = dec.childMarker('my-key', 147);
    assert.strictEqual(m, '<!-- hermes:epic-child key:"my-key" parent:#147 -->');
  });

  it('parses markers exactly as filed on existing scc:child issues', () => {
    // Shape copied verbatim from a real child body (issue #229, parent #220).
    const real = '---\n<!-- hermes:epic-child key:"migrate-config-task-queue-js-to-use-engine-adapt" parent:#220 -->';
    assert.deepStrictEqual(dec.parseChildMarkers(real),
      [{ key: 'migrate-config-task-queue-js-to-use-engine-adapt', parent: 220 }]);
  });
});

describe('extractWorkItems', () => {
  it('collects scope bullets as specs and issue refs as adoptions', () => {
    const items = dec.extractWorkItems(EPIC_BODY);
    assert.strictEqual(items.length, 3);
    assert.strictEqual(items[0].kind, 'spec');
    assert.match(items[0].text, /Conversational refinement/);
    assert.deepStrictEqual(items[1].ac, ['Children carry a parent marker', 'Parent gets a live checklist']);
    assert.deepStrictEqual(items[2], { kind: 'ref', number: 229, text: '#229 — already-filed child issue', section: items[2].section });
  });

  it('never re-reads the managed checklist block as scope', () => {
    const body = EPIC_BODY + '\n## Children\n\n' + dec.BLOCK_START +
      '\n\n- [ ] #1 — ghost <!-- hermes:sub:ghost -->\n\n' + dec.BLOCK_END + '\n';
    const items = dec.extractWorkItems(body);
    assert.ok(!items.some((i) => /ghost/.test(i.text || '')));
  });

  it('falls back to epic-level AC items when no work-item section exists', () => {
    const body = '## Problem\nBig thing.\n\n## Acceptance criteria\n\n- [ ] slice one\n- [ ] slice two\n';
    assert.strictEqual(dec.extractWorkItems(body).length, 0);
    const ac = dec.extractAcItems(body);
    assert.strictEqual(ac.length, 2);
    assert.match(ac[0].text, /slice one/);
  });
});

describe('childBody', () => {
  it('carries Problem/Context/AC structure plus the parent marker', () => {
    const spec = dec.childSpec({ kind: 'spec', text: 'Do the thing', ac: [], section: 'Scope' }, epic());
    const body = dec.childBody({ epic: epic(), spec });
    assert.match(body, /## Problem\nDo the thing/);
    assert.match(body, /## Context[\s\S]*Part of epic #100 — "[^"]+"/);
    assert.match(body, /## Acceptance criteria\n- \[ \]/);
    assert.ok(body.endsWith(dec.childMarker(spec.key, 100)));
    assert.ok(spec.ac.length >= 2); // deliverable + validation criterion
  });

  it('uses nested bullets as the AC checklist when present', () => {
    const spec = dec.childSpec(
      { kind: 'spec', text: 'Parent item', ac: ['nested one', 'nested two'], section: 'Scope' }, epic());
    assert.deepStrictEqual(spec.ac, ['nested one', 'nested two']);
  });
});

describe('upsertChecklistBlock', () => {
  const entries = [
    { key: 'a', number: 1, title: 'first', state: 'closed' },
    { key: 'b', number: 2, title: 'second', state: 'open' },
  ];

  it('appends a managed block when the body has none', () => {
    const out = dec.upsertChecklistBlock('## Body\n', entries);
    assert.match(out, /## Body\n\n<!-- hermes:epic-decomposition -->[\s\S]*<!-- \/hermes:epic-decomposition -->/);
    assert.match(out, /- \[x\] #1 — first <!-- hermes:sub:a -->/);
    assert.match(out, /- \[ \] #2 — second <!-- hermes:sub:b -->/);
  });

  it('regenerates in place: keeps human lines, refreshes state, adds new keys', () => {
    const body = 'text\n\n' + dec.BLOCK_START + '\n\n' + dec.BLOCK_HEADING + '\n\n' +
      '- [ ] #1 — first <!-- hermes:sub:a -->\n' +
      '- a human note that survives\n\n' + dec.BLOCK_END + '\ntail';
    const out = dec.upsertChecklistBlock(body, entries);
    assert.match(out, /- \[x\] #1 — first <!-- hermes:sub:a -->/);   // state refreshed
    assert.match(out, /- a human note that survives/);               // unmarked preserved
    assert.match(out, /- \[ \] #2 — second <!-- hermes:sub:b -->/);  // new key appended
    assert.match(out, /tail$/);                                      // surroundings intact
    assert.strictEqual(out.indexOf(dec.BLOCK_START), out.lastIndexOf(dec.BLOCK_START));
  });
});

describe('decomposeEpic', () => {
  it('skips issues without the epic label — no children, no mutations', () => {
    const client = fakeClient({ issue: epic({ labels: [{ name: 'bug' }] }) });
    const r = dec.decomposeEpic(opts(), client);
    assert.strictEqual(r.action, 'skipped');
    assert.strictEqual(client.calls.creates.length, 0);
    assert.strictEqual(client.calls.bodyUpdates.length, 0);
  });

  it('files bounded scc:child issues with parent linkage + live checklist', () => {
    const client = fakeClient();
    const r = dec.decomposeEpic(opts(), client);
    assert.strictEqual(r.action, 'decomposed');
    // 2 scope bullets → 2 filed; the #229 ref is adopted, never re-filed.
    assert.strictEqual(client.calls.creates.length, 2);
    const linked = r.children.find((c) => c.status === 'linked');
    assert.strictEqual(linked.number, 229);

    for (const c of client.calls.creates) {
      assert.ok(c.labels.includes('scc:child'));
      assert.ok(c.labels.includes('priority:P2'));      // inherited from epic
      assert.match(c.body, /hermes:epic-child key:"[a-z0-9-]+" parent:#100/);
      assert.match(c.body, /## Acceptance criteria\n- \[ \]/);
    }
    assert.deepStrictEqual(client.calls.ensured, ['scc:child']);

    const upd = client.calls.bodyUpdates;
    assert.strictEqual(upd.length, 1);
    assert.strictEqual(upd[0].issueNumber, 100);
    assert.match(upd[0].body, /<!-- hermes:epic-decomposition -->/);
    assert.match(upd[0].body, /- \[ \] #900 — /);      // filed children listed
    assert.match(upd[0].body, /#229 —/);               // adopted ref listed
  });

  it('is idempotent: a re-run files nothing and keeps the checklist', () => {
    const client = fakeClient();
    const first = dec.decomposeEpic(opts(), client);
    assert.strictEqual(first.childrenFiled, 2);

    // Second run sees the filed children via their markers → zero creates.
    const second = dec.decomposeEpic(opts({ issue: undefined }), client);
    assert.strictEqual(second.action, 'synced');
    assert.strictEqual(second.childrenFiled, 0);
    assert.strictEqual(client.calls.creates.length, 2);
    assert.ok(second.children.every((c) => c.status !== 'filed'));
  });

  it('never re-files a key carried by a closed child (marker index covers closed)', () => {
    const existing = {
      number: 229, state: 'closed', title: 'old child',
      body: 'x\n' + dec.childMarker(dec.slugifyKey('Conversational refinement: clarifying questions on under-specified issues'), 100),
      labels: [], url: 'u/229',
    };
    const client = fakeClient({ closed: [existing] });
    const r = dec.decomposeEpic(opts(), client);
    const statuses = r.children.map((c) => c.status);
    assert.strictEqual(client.calls.creates.length, 1);  // only the second spec files
    assert.ok(statuses.includes('existing'));
  });

  it('adopts children already tracked on the epic under different keys (Hermes-filed lineage)', () => {
    // Epic body already carries a managed block listing a child filed by
    // another tool — same scope, different key. Never re-filed.
    const block = dec.BLOCK_START + '\n\n' + dec.BLOCK_HEADING + '\n\n' +
      '- [ ] #310 — feat(intake): conversational refinement — clarifying questions and derived acceptance criteria <!-- hermes:sub:conversational-refinement -->\n\n' +
      dec.BLOCK_END;
    const client = fakeClient({ issue: epic({ body: EPIC_BODY + '\n' + block }) });
    const r = dec.decomposeEpic(opts(), client);
    // The "Conversational refinement…" spec adopts #310; the second scope
    // bullet still files; the #229 ref links.
    const adopted = r.children.find((c) => c.status === 'adopted');
    assert.strictEqual(adopted.number, 310);
    assert.strictEqual(client.calls.creates.length, 1);
    // The adopted child renders once in the checklist, against its number.
    assert.strictEqual(
      (client.calls.bodyUpdates[0].body.match(/#310/g) || []).length, 1);
  });

  it('bounds the children count by --max-children', () => {
    const client = fakeClient();
    const r = dec.decomposeEpic(opts({ maxChildren: 1 }), client);
    assert.strictEqual(r.children.length, 1);
    assert.strictEqual(client.calls.creates.length, 1);
  });

  it('dry-run plans without mutating', () => {
    const client = fakeClient();
    const r = dec.decomposeEpic(opts({ dryRun: true }), client);
    assert.strictEqual(r.action, 'dry_run');
    assert.strictEqual(client.calls.creates.length, 0);
    assert.strictEqual(client.calls.bodyUpdates.length, 0);
    assert.ok(r.children.every((c) => c.status !== 'filed'));
  });

  it('accepts an explicit upstream plan (children-file path)', () => {
    const client = fakeClient();
    const r = dec.decomposeEpic(opts({
      children: [{ title: 'planned slice', ac: ['custom ac'] }],
    }), client);
    assert.strictEqual(r.action, 'decomposed');
    assert.strictEqual(client.calls.creates.length, 1);
    assert.match(client.calls.creates[0].body, /- \[ \] custom ac/);
  });

  it('skips a closed epic', () => {
    const client = fakeClient({ issue: epic({ state: 'closed' }) });
    const r = dec.decomposeEpic(opts(), client);
    assert.strictEqual(r.action, 'skipped');
    assert.strictEqual(client.calls.creates.length, 0);
  });

  it('skips an epic with no decomposition input', () => {
    const client = fakeClient({ issue: epic({ body: '## Problem\nJust a title and prose, no lists at all here.\n'.repeat(3) }) });
    const r = dec.decomposeEpic(opts(), client);
    assert.strictEqual(r.action, 'skipped');
    assert.match(r.reason, /no decomposition input/);
    assert.strictEqual(client.calls.creates.length, 0);
  });
});
