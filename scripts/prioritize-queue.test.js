// scripts/prioritize-queue.test.js — unit + offline-replay tests for the
// value-scored ready queue (issue #162, epic #147).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  resolveRubric,
  vetoLabels,
  armedStatus,
  priorityTier,
  isEpicLinked,
  scoreIssue,
  rankQueue,
  buildQueue,
  toMarkdown,
  main,
} = require('./prioritize-queue');

// Mirrors the dogfooding contract (.ai-sdlc.yaml) bindings.
const CONTRACT = {
  triggers: {
    issue_label: 'ready-to-implement',
    skip_labels: ['epic', 'wontfix', 'invalid', 'duplicate', 'question', 'dependencies', 'needs-human'],
  },
  escalation: { label: 'needs-human' },
  labels: {
    queued: 'scc:queued',
    'in-progress': 'scc:in-progress',
    'pr-opened': 'scc:pr-opened',
    'not-ready': 'scc:not-ready',
  },
  prioritize: {
    enabled: true,
    priority_override: true,
    weights: {
      priority: { P0: 1000, P1: 300, P2: 100, P3: 25 },
      unlabeled: 0,
      age_per_day: 2,
      age_cap_days: 30,
      epic_linked: 25,
      blocking: 15,
      blocked: -20,
    },
  },
};
const RUBRIC = resolveRubric(CONTRACT);
const NOW = Date.parse('2026-10-17T00:00:00Z');
const T = 'ready-to-implement';

function issue(n, { labels = [], createdAt = '2026-10-01T00:00:00Z', body = '', blockedBy = [], blocking = [] } = {}) {
  return {
    number: n,
    title: `issue ${n}`,
    url: `https://example.test/${n}`,
    createdAt,
    body,
    labels: labels.map((name) => ({ name })),
    blockedBy: { nodes: blockedBy },
    blocking: { nodes: blocking },
  };
}

// --- rubric resolution ----------------------------------------------------------
describe('resolveRubric — contract → scoring rubric', () => {
  it('defaults to disabled when the contract does not declare prioritize', () => {
    assert.equal(resolveRubric({}).enabled, false);
    assert.equal(resolveRubric(CONTRACT).enabled, true);
  });

  it('fills default weights and clamps max to the hard bound', () => {
    const r = resolveRubric({ prioritize: { enabled: true, max: 9999 } });
    assert.equal(r.max, 200);
    assert.equal(r.weights.priority.P0, 1000);
    assert.equal(r.weights.blocked, -20);
    assert.equal(r.priorityOverride, true);
  });

  it('merges declared priority weights over the defaults', () => {
    const r = resolveRubric({ prioritize: { enabled: true, weights: { priority: { p0: 500 } } } });
    assert.equal(r.weights.priority.P0, 500);
    assert.equal(r.weights.priority.P1, 300); // default preserved
  });

  it('honors a renamed epic-child label binding', () => {
    const r = resolveRubric({ prioritize: { enabled: true, epic_child_label: 'custom:child' } });
    assert.equal(r.epicChildLabel, 'custom:child');
  });
});

// --- queue membership -------------------------------------------------------------
describe('armedStatus — trigger ∧ ¬veto (spec/labels.md)', () => {
  it('vetoes cover skip labels, the hold, refinement-hold and run-states', () => {
    const v = vetoLabels(CONTRACT);
    for (const l of ['epic', 'wontfix', 'needs-human', 'scc:not-ready', 'scc:queued', 'scc:in-progress', 'scc:pr-opened']) {
      assert.ok(v.has(l), `missing veto ${l}`);
    }
  });

  it('armed = trigger present, no veto label', () => {
    assert.deepEqual(armedStatus(issue(1, { labels: [T, 'priority:P1'] }), CONTRACT), { armed: true, vetoes: [] });
    assert.equal(armedStatus(issue(2, { labels: ['priority:P1'] }), CONTRACT).armed, false);
  });

  it('drift — trigger + veto label is excluded with the veto recorded', () => {
    const st = armedStatus(issue(3, { labels: [T, 'needs-human'] }), CONTRACT);
    assert.equal(st.armed, true);
    assert.deepEqual(st.vetoes, ['needs-human']);
  });
});

// --- scoring ----------------------------------------------------------------------
describe('scoreIssue — declared rubric → score + rationale', () => {
  it('parses the lowest priority tier', () => {
    assert.equal(priorityTier(issue(1, { labels: ['priority:P2', 'priority:P0'] })), 0);
    assert.equal(priorityTier(issue(1, { labels: [] })), null);
  });

  it('detects epic linkage via label or body marker', () => {
    assert.equal(isEpicLinked(issue(1, { labels: ['scc:child'] }), RUBRIC), true);
    assert.equal(isEpicLinked(issue(1, { body: 'Part of epic #147 — parent:#147' }), RUBRIC), true);
    assert.equal(isEpicLinked(issue(1), RUBRIC), false);
  });

  it('sums priority + age + linkage + dependency components', () => {
    // 16 days old → 16×2=32; P1=300; epic=25; blocking 1×15; blocked 1×-20.
    const e = scoreIssue(issue(9, {
      labels: [T, 'priority:P1', 'scc:child'],
      createdAt: '2026-10-01T00:00:00Z',
      blocking: [{ number: 20, state: 'OPEN' }],
      blockedBy: [{ number: 8, state: 'OPEN' }, { number: 7, state: 'CLOSED' }],
    }), RUBRIC, NOW);
    assert.equal(e.score, 300 + 32 + 25 + 15 - 20);
    assert.match(e.reason, /priority:P1=300/);
    assert.match(e.reason, /blocking\(1\)=15/);
    assert.match(e.reason, /blocked\(1\)=-20/);
    assert.equal(e.blockedBy, 1); // closed blocker ignored
  });

  it('caps the age component at age_cap_days', () => {
    const e = scoreIssue(issue(1, { labels: [T], createdAt: '2026-01-01T00:00:00Z' }), RUBRIC, NOW);
    assert.equal(e.score, 30 * 2);
    assert.equal(e.ageDays, 289); // evidence keeps the true age
  });
});

// --- ordering -----------------------------------------------------------------------
describe('rankQueue — dispatch order', () => {
  it('priority override: P0 always first even when its weight is zeroed', () => {
    // The override is a tier ordering, not a weight — a repo that zeroes
    // the P0 weight still dispatches P0 first (AC: "P0 always first").
    const r = resolveRubric({
      ...CONTRACT,
      prioritize: { ...CONTRACT.prioritize, weights: { ...CONTRACT.prioritize.weights, priority: { P0: 0 } } },
    });
    const p0 = scoreIssue(issue(1, { labels: [T, 'priority:P0'], createdAt: '2026-10-16T00:00:00Z' }), r, NOW);
    const fat = scoreIssue(issue(2, {
      labels: [T], createdAt: '2026-01-01T00:00:00Z',
      blocking: [{ number: 9, state: 'OPEN' }, { number: 10, state: 'OPEN' }],
    }), r, NOW);
    assert.ok(fat.score > p0.score);
    const q = rankQueue([fat, p0], r);
    assert.equal(q[0].number, 1);
    assert.equal(q[0].rank, 1);
  });

  it('score-first ordering when priority_override is off', () => {
    const r = { ...RUBRIC, priorityOverride: false };
    const lo = scoreIssue(issue(1, { labels: [T, 'priority:P3'], createdAt: '2026-10-16T00:00:00Z' }), r, NOW);
    const hi = scoreIssue(issue(2, {
      labels: [T], createdAt: '2026-01-01T00:00:00Z',
      blocking: [{ number: 9, state: 'OPEN' }],
    }), r, NOW);
    assert.ok(hi.score > lo.score);
    assert.equal(rankQueue([lo, hi], r)[0].number, 2);
  });

  it('older issue wins the tie inside a tier', () => {
    const a = scoreIssue(issue(1, { labels: [T, 'priority:P2'], createdAt: '2026-10-10T00:00:00Z' }), RUBRIC, NOW);
    const b = scoreIssue(issue(2, { labels: [T, 'priority:P2'], createdAt: '2026-10-12T00:00:00Z' }), RUBRIC, NOW);
    // a is older → higher age score AND the deterministic tiebreak.
    assert.equal(rankQueue([b, a], RUBRIC)[0].number, 1);
  });
});

// --- buildQueue / output --------------------------------------------------------------
describe('buildQueue — the emitted queue artifact', () => {
  it('filters vetoed drift out of the queue and records it', () => {
    const res = buildQueue({
      repo: 'o/r', contract: CONTRACT, rubric: RUBRIC, max: 50, now: NOW,
      issues: [
        issue(1, { labels: [T, 'priority:P1'] }),
        issue(2, { labels: [T, 'wontfix'] }),
        issue(3, { labels: ['priority:P0'] }), // not armed — never listed in practice
      ],
    });
    assert.equal(res.queue.length, 1);
    assert.equal(res.queue[0].number, 1);
    assert.deepEqual(res.excluded, [{ number: 2, vetoes: ['wontfix'] }]);
    assert.equal(res.deps, true);
  });

  it('marks deps:false when dependency fields are absent (fail-open evidence)', () => {
    const i = issue(1, { labels: [T] });
    delete i.blockedBy; delete i.blocking;
    const res = buildQueue({ repo: 'o/r', issues: [i], contract: CONTRACT, rubric: RUBRIC, max: 50, now: NOW });
    assert.equal(res.deps, false);
  });

  it('renders the markdown queue table', () => {
    const res = buildQueue({
      repo: 'o/r', contract: CONTRACT, rubric: RUBRIC, max: 50, now: NOW,
      issues: [issue(5, { labels: [T, 'priority:P0'] })],
    });
    const md = toMarkdown(res);
    assert.match(md, /### ai-sdlc prioritize — scored ready queue/);
    assert.match(md, /\| 1 \| #5 issue 5 \| 1032 \| P0 \|/);
  });
});

// --- CLI ------------------------------------------------------------------------------
describe('main — CLI + offline replay', () => {
  function fixture(dir, issues) {
    const f = path.join(dir, 'issues.json');
    fs.writeFileSync(f, JSON.stringify(issues));
    const c = path.join(dir, 'contract.json');
    fs.writeFileSync(c, JSON.stringify(CONTRACT));
    return { f, c };
  }

  it('replays an --issues fixture deterministically (--now pins the clock)', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pq-'));
    const { f, c } = fixture(dir, [issue(7, { labels: [T, 'priority:P0'] }), issue(8, { labels: [T] })]);
    const out = [];
    const r = main(['--issues', f, '--contract', c, '--now', '2026-10-17T00:00:00Z', '--json'], { stdout: (s) => out.push(s) });
    assert.equal(r.queue[0].number, 7);
    assert.equal(JSON.parse(out[0]).generatedAt, '2026-10-17T00:00:00.000Z');
  });

  it('writes PREFIX.json + PREFIX.md and github-output head/size', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pq-'));
    const { f, c } = fixture(dir, [issue(3, { labels: [T, 'priority:P1'] })]);
    const gout = path.join(dir, 'gout');
    fs.writeFileSync(gout, '');
    const prev = process.env.GITHUB_OUTPUT;
    process.env.GITHUB_OUTPUT = gout;
    try {
      main(['--issues', f, '--contract', c, '--now', '2026-10-17T00:00:00Z',
        '--out', path.join(dir, 'queue'), '--json', '--github-output'],
      { stdout: () => {} });
    } finally { process.env.GITHUB_OUTPUT = prev; }
    assert.ok(fs.existsSync(path.join(dir, 'queue.json')));
    assert.match(fs.readFileSync(path.join(dir, 'queue.md'), 'utf8'), /scored ready queue/);
    assert.match(fs.readFileSync(gout, 'utf8'), /queue_size=1\nqueue_head=3\n/);
  });

  it('emits an empty enabled:false result when the contract does not opt in', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pq-'));
    const { f } = fixture(dir, [issue(1, { labels: [T] })]);
    const c = path.join(dir, 'c.json');
    fs.writeFileSync(c, JSON.stringify({ triggers: { issue_label: T } }));
    const r = main(['--issues', f, '--contract', c, '--json'], { stdout: () => {} });
    assert.equal(r.enabled, false);
    assert.equal(r.queue.length, 0);
  });

  it('fetches armed issues via gh when --repo is given (mocked exec)', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pq-'));
    const c = path.join(dir, 'c.json');
    fs.writeFileSync(c, JSON.stringify(CONTRACT));
    const seen = [];
    const exec = (argv) => {
      seen.push(argv.join(' '));
      return JSON.stringify([issue(4, { labels: [T, 'priority:P0'] })]);
    };
    const r = main(['--repo', 'o/r', '--contract', c, '--now', '2026-10-17T00:00:00Z'], { exec, stdout: () => {} });
    assert.equal(r.queue[0].number, 4);
    assert.match(seen[0], /issue list -R o\/r --state open --label ready-to-implement/);
  });
});
