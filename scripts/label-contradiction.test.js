// scripts/label-contradiction.test.js — unit + mocked-API tests for the
// label-contradiction repair (issue #200, spec/labels.md).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  MARKER,
  DRIFT_CAUSE,
  forbiddenPairs,
  findContradictions,
  planRepairs,
  auditBody,
  repairIssue,
  sweepRepo,
  main,
} = require('./label-contradiction');

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
    failed: 'scc:failed',
    'not-ready': 'scc:not-ready',
    'no-changes': 'scc:no-changes',
    state_machine: {
      veto_pairs: [
        { winner: 'needs-human', loser: 'ready-to-implement' },
        { winner: 'scc:not-ready', loser: 'ready-to-implement' },
        { winner: 'needs-human', loser: 'scc:not-ready' },
        { winner: 'scc:queued', loser: 'ready-to-implement' },
        { winner: 'scc:in-progress', loser: 'ready-to-implement' },
        { winner: 'scc:pr-opened', loser: 'ready-to-implement' },
      ],
    },
  },
};
const PAIRS = forbiddenPairs(CONTRACT);
const T = 'ready-to-implement';

// --- forbidden set derivation ---------------------------------------------------
describe('forbiddenPairs — contract → effective forbidden set', () => {
  it('unions declared veto_pairs with every skip_label vetoing the trigger', () => {
    const winners = new Set(PAIRS.map((p) => `${p.winner}|${p.loser}`));
    for (const p of CONTRACT.labels.state_machine.veto_pairs) {
      assert.ok(winners.has(`${p.winner}|${p.loser}`), `missing declared pair ${p.winner}>${p.loser}`);
    }
    for (const s of CONTRACT.triggers.skip_labels) {
      assert.ok(winners.has(`${s}|${T}`), `missing derived pair ${s}>${T}`);
    }
  });

  it('dedupes pairs (needs-human appears in both veto_pairs and skip_labels)', () => {
    const seen = PAIRS.map((p) => `${p.winner}|${p.loser}`);
    assert.equal(seen.length, new Set(seen).size);
  });

  it('derives bindings from an empty contract (canonical-name defaults)', () => {
    const winners = new Set(forbiddenPairs({}).map((p) => `${p.winner}|${p.loser}`));
    assert.ok(winners.has(`needs-human|${T}`));
    assert.ok(winners.has(`needs-refinement|${T}`));
    assert.ok(winners.has(`epic|${T}`));
    assert.ok(winners.has(`scc:queued|${T}`));
  });

  it('honors renamed bindings — labels are bindings, not constants', () => {
    const pairs = forbiddenPairs({
      triggers: { issue_label: 'arm-me', skip_labels: ['custom-hold'] },
      escalation: { label: 'human-hold' },
      labels: { 'not-ready': 'refine-me', queued: 'run:queued' },
    });
    const plan = planRepairs(['arm-me', 'custom-hold', 'bug'], pairs);
    assert.deepEqual(plan.remove, ['arm-me']);
    assert.deepEqual(plan.vetoes, ['custom-hold']);
  });
});

// --- contradiction matrix (AC: all label-combination scenarios) -----------------
describe('planRepairs — label combination matrix', () => {
  const noOp = [
    [[], 'empty'],
    [[T], 'trigger only'],
    [['epic'], 'skip label only'],
    [['needs-human'], 'hold only'],
    [['scc:not-ready'], 'refinement hold only'],
    [['bug', 'priority:P2'], 'unrelated labels only'],
    [[T, 'scc:failed'], 'trigger + terminal run-state is deliberately legal'],
    [[T, 'scc:no-changes'], 'trigger + no-changes is deliberately legal'],
    [['needs-human', 'scc:in-progress'], 'hold + in-flight run is deliberately legal'],
  ];
  for (const [labels, name] of noOp) {
    it(`takes no action — ${name}`, () => {
      const plan = planRepairs(labels, PAIRS);
      assert.equal(plan.violated.length, 0);
      assert.deepEqual(plan.remove, []);
    });
  }

  it('removes the trigger for EVERY skip label (F4)', () => {
    for (const s of CONTRACT.triggers.skip_labels) {
      const plan = planRepairs([T, s, 'bug'], PAIRS);
      assert.deepEqual(plan.remove, [T], `${s} should veto ${T}`);
      assert.ok(plan.vetoes.includes(s));
    }
  });

  it('needs-human vetoes the trigger (F1)', () => {
    const plan = planRepairs([T, 'needs-human'], PAIRS);
    assert.deepEqual(plan.remove, [T]);
    assert.deepEqual(plan.vetoes, ['needs-human']);
  });

  it('refinement hold vetoes the trigger (F2)', () => {
    const plan = planRepairs([T, 'scc:not-ready'], PAIRS);
    assert.deepEqual(plan.remove, [T]);
    assert.deepEqual(plan.vetoes, ['scc:not-ready']);
  });

  it('needs-human outranks the refinement hold (F3)', () => {
    const plan = planRepairs(['needs-human', 'scc:not-ready'], PAIRS);
    assert.deepEqual(plan.remove, ['scc:not-ready']);
    assert.deepEqual(plan.vetoes, ['needs-human']);
  });

  it('active run-state labels veto the trigger (F5)', () => {
    for (const q of ['scc:queued', 'scc:in-progress', 'scc:pr-opened']) {
      const plan = planRepairs([T, q], PAIRS);
      assert.deepEqual(plan.remove, [T], `${q} should veto ${T}`);
      assert.deepEqual(plan.vetoes, [q]);
    }
  });

  it('multiple vetoes on one trigger → strip once, name every survivor', () => {
    const plan = planRepairs([T, 'needs-human', 'duplicate'], PAIRS);
    assert.deepEqual(plan.remove, [T]);
    assert.deepEqual(plan.vetoes.sort(), ['duplicate', 'needs-human']);
  });

  it('cascading losers: hold + refinement + trigger strips both losers', () => {
    const plan = planRepairs([T, 'needs-human', 'scc:not-ready'], PAIRS);
    assert.deepEqual(plan.remove.sort(), [T, 'scc:not-ready'].sort());
    assert.deepEqual(plan.vetoes, ['needs-human']);
  });

  it('matching is case-insensitive but removal uses the actual label name', () => {
    const plan = planRepairs(['Ready-To-Implement', 'EPIC'], PAIRS);
    assert.deepEqual(plan.remove, ['Ready-To-Implement']);
    assert.deepEqual(plan.vetoes, ['epic']);
  });

  it('does not strip a loser that is absent even when its winner is present', () => {
    const plan = planRepairs(['epic', 'bug'], PAIRS);
    assert.equal(plan.violated.length, 0);
    assert.deepEqual(plan.remove, []);
  });
});

// --- audit comment ---------------------------------------------------------------
describe('auditBody', () => {
  it('carries an ISO timestamp, the removed label and the vetoing label', () => {
    const body = auditBody({
      removed: [T], vetoes: ['epic'], escalated: false, now: '2026-01-02T03:04:05.000Z',
    });
    assert.match(body, /2026-01-02T03:04:05\.000Z/);
    assert.match(body, /`ready-to-implement`/);
    assert.match(body, /`epic`/);
    assert.ok(body.includes(MARKER));
  });

  it('names the drift-loop cause when escalated', () => {
    const body = auditBody({ removed: [T], vetoes: ['epic'], escalated: true });
    assert.match(body, new RegExp(DRIFT_CAUSE));
    assert.match(body, /`needs-human`/);
  });
});

// --- mocked GitHub API -------------------------------------------------------------
describe('repairIssue — mocked gh', () => {
  // Records every gh invocation; serves canned responses. The script unlinks
  // its --body-file after commenting, so the comment body is captured here at
  // call time into `bodies`.
  function mockGh({ priorComments = 0, failEdit = false } = {}) {
    const calls = [];
    const bodies = [];
    const exec = (argv) => {
      calls.push(argv);
      const key = argv.slice(0, 2).join(' ');
      if (argv[0] === 'api') return JSON.stringify(Array(priorComments).fill(`x ${MARKER}`));
      if (key === 'issue edit') {
        if (failEdit) throw new Error('label not found');
        return '';
      }
      if (key === 'issue comment') {
        bodies.push(fs.readFileSync(argv[argv.indexOf('--body-file') + 1], 'utf8'));
        return '';
      }
      if (key === 'label create') return '';
      throw new Error(`unexpected gh call: ${argv.join(' ')}`);
    };
    return { calls, bodies, exec };
  }
  const mutations = (calls) => calls.filter((c) => ['issue edit', 'issue comment', 'label create'].includes(c.slice(0, 2).join(' ')));

  it('strips the trigger and posts the audit comment naming the veto', () => {
    const { calls, bodies, exec } = mockGh();
    const r = repairIssue({
      repo: 'o/r', issue: 7, labels: [T, 'epic', 'bug'], pairs: PAIRS,
      apply: true, exec, now: '2026-01-02T03:04:05.000Z',
    });
    assert.equal(r.repaired, true);
    assert.deepEqual(r.removed, [T]);
    const edit = calls.find((c) => c.slice(0, 2).join(' ') === 'issue edit');
    assert.ok(edit.includes('--remove-label') && edit.includes(T));
    const comment = calls.find((c) => c.slice(0, 2).join(' ') === 'issue comment');
    assert.ok(comment.includes('--body-file'));
    assert.equal(bodies.length, 1);
    assert.match(bodies[0], /2026-01-02T03:04:05\.000Z/);
    assert.match(bodies[0], /`epic`/);
    // No side effects: unrelated labels are never touched.
    assert.ok(!edit.includes('bug') && !edit.includes('epic') && !edit.includes('--add-label'));
  });

  it('no-ops when only one of the pair is present — zero mutations', () => {
    for (const labels of [[T, 'bug'], ['epic', 'bug'], ['needs-human', 'scc:in-progress'], [T, 'scc:failed']]) {
      const { calls, exec } = mockGh();
      const r = repairIssue({ repo: 'o/r', issue: 1, labels, pairs: PAIRS, apply: true, exec });
      assert.equal(r.repaired, false);
      assert.equal(mutations(calls).length, 0, `expected no gh mutations for ${labels}`);
    }
  });

  it('dry-run reports the plan without mutating', () => {
    const { calls, exec } = mockGh();
    const r = repairIssue({ repo: 'o/r', issue: 1, labels: [T, 'epic'], pairs: PAIRS, apply: false, exec });
    assert.equal(r.dry, true);
    assert.equal(r.repaired, false);
    assert.equal(mutations(calls).length, 0);
  });

  it('a failed label strip records the error and posts no comment', () => {
    const { calls, exec } = mockGh({ failEdit: true });
    const r = repairIssue({ repo: 'o/r', issue: 1, labels: [T, 'epic'], pairs: PAIRS, apply: true, exec });
    assert.equal(r.repaired, false);
    assert.match(r.error, /label strip failed/);
    assert.equal(calls.filter((c) => c.slice(0, 2).join(' ') === 'issue comment').length, 0);
  });

  it('escalates to needs-human after maxRepairs prior repairs (label-drift-loop)', () => {
    const { calls, bodies, exec } = mockGh({ priorComments: 2 });
    const r = repairIssue({
      repo: 'o/r', issue: 1, labels: [T, 'epic'], pairs: PAIRS,
      apply: true, exec, maxRepairs: 2, holdLabel: 'needs-human',
    });
    assert.equal(r.escalated, true);
    const edit = calls.find((c) => c.slice(0, 2).join(' ') === 'issue edit');
    assert.ok(edit.includes('--add-label') && edit.includes('needs-human'));
    const create = calls.find((c) => c.slice(0, 2).join(' ') === 'label create');
    assert.ok(create.includes('needs-human'));
    assert.equal(bodies.length, 1);
    assert.match(bodies[0], new RegExp(DRIFT_CAUSE));
  });

  it('does not re-apply the hold when it is already the vetoing label', () => {
    const { calls, exec } = mockGh({ priorComments: 5 });
    const r = repairIssue({
      repo: 'o/r', issue: 1, labels: [T, 'needs-human'], pairs: PAIRS,
      apply: true, exec, maxRepairs: 2, holdLabel: 'needs-human',
    });
    assert.equal(r.escalated, true);
    const edit = calls.find((c) => c.slice(0, 2).join(' ') === 'issue edit');
    assert.ok(!edit.includes('--add-label'));
    assert.equal(calls.filter((c) => c.slice(0, 2).join(' ') === 'label create').length, 0);
  });

  it('fetches labels from the issue when not supplied', () => {
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      if (argv.slice(0, 2).join(' ') === 'issue view') {
        return JSON.stringify({ number: 3, labels: [{ name: T }, { name: 'duplicate' }] });
      }
      if (argv[0] === 'api') return '[]';
      return '';
    };
    const r = repairIssue({ repo: 'o/r', issue: 3, pairs: PAIRS, apply: true, exec });
    assert.equal(r.repaired, true);
    assert.deepEqual(r.vetoes, ['duplicate']);
  });
});

// --- sweep -------------------------------------------------------------------------
describe('sweepRepo — mocked gh', () => {
  it('repairs only contradictory issues and leaves everything else alone', () => {
    const issues = [
      { number: 1, labels: [{ name: T }, { name: 'epic' }] },
      { number: 2, labels: [{ name: 'bug' }] },
      { number: 3, labels: [{ name: T }] },
      { number: 4, labels: [{ name: T }, { name: 'needs-human' }, { name: 'bug' }] },
    ];
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      if (argv.slice(0, 2).join(' ') === 'issue list') return JSON.stringify(issues);
      if (argv[0] === 'api') return '[]';
      return '';
    };
    const r = sweepRepo({ repo: 'o/r', pairs: PAIRS, apply: true, exec });
    assert.equal(r.scanned, 4);
    assert.equal(r.repaired, 2);
    assert.deepEqual(r.repairs.map((x) => x.issue).sort(), [1, 4]);
    const edits = calls.filter((c) => c.slice(0, 2).join(' ') === 'issue edit');
    assert.equal(edits.length, 2);
    for (const e of edits) {
      assert.ok(e.includes(T) && !e.includes('bug'));
    }
  });
});

// --- CLI ----------------------------------------------------------------------------
describe('cli', () => {
  it('--labels evaluates without touching GitHub', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lc-'));
    const empty = path.join(dir, 'empty.json');
    fs.writeFileSync(empty, '{}');
    let out = '';
    const r = main(['--labels', `${T},epic,bug`, '--contract', empty, '--json'],
      { stdout: (s) => { out = s; } });
    // empty contract → canonical-name defaults still catch epic
    assert.deepEqual(r.remove, [T]);
    assert.deepEqual(JSON.parse(out).vetoes, ['epic']);
  });

  it('--contract JSON drives bindings end-to-end', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lc-'));
    const cfg = path.join(dir, 'contract.json');
    fs.writeFileSync(cfg, JSON.stringify({ triggers: { issue_label: 'arm', skip_labels: ['hold-x'] } }));
    const r = main(['--labels', 'arm,hold-x', '--contract', cfg], { stdout: () => {} });
    assert.deepEqual(r.remove, ['arm']);
    assert.deepEqual(r.vetoes, ['hold-x']);
  });

  it('errors when neither --issue/--sweep/--labels is given', () => {
    assert.throws(() => main([], { stdout: () => {} }), /nothing to do/);
  });
});
