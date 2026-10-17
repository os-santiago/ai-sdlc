// scripts/self-heal.test.js — unit + mocked-client tests for the unified
// self-heal orchestrator (issue #187): signature extraction, runbook
// consult, bounded repair decision, stall classification → retry policy,
// postmortem escalation through the forge-adapter seam, sentinel poll.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const selfHeal = require('./self-heal');
const stallGuard = require('./repair-stall-guard');

const {
  STALL_CLASS_MAP,
  OUTCOME_CLASS_MAP,
  RETRY_POLICY,
  classifyStall,
  extractFailure,
  consultRunbook,
  planRepair,
  escalate,
  sentinel,
  sequence,
} = selfHeal;

const tmpRunbook = (entries) => {
  const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'sh-rb-')), 'failure_modes.md');
  fs.writeFileSync(f, entries);
  return f;
};

const ENTRY = (id, sig, fix) => `### \`${id}\` — sample failure mode\n\n- **Signatures:** \`${sig}\`\n- **Fix:** ${fix}\n\n<!-- ai-sdlc:runbook-entry -->\n`;

// --- extractFailure: canonical artifact ---------------------------------------
describe('extractFailure', () => {
  it('emits the same {signature, signatureParts, rootCause, checks} artifact as ci-repair', () => {
    const a = extractFailure({
      checks: ['build', 'test'],
      logLines: [{ name: 'build', line: 'Error: boom at 9f3c1d' }, { name: 'test', line: 'FAIL x' }],
      rootCause: 'boom',
    });
    assert.match(a.signature, /^[0-9a-f]{16}$/);
    assert.equal(a.signatureParts.length, 2);
    assert.equal(a.rootCause, 'boom');
    assert.deepEqual(a.checks, ['build', 'test']);
    assert.equal(a.baseFault, false);
  });

  it('falls back to check-name parts when no log lines are given', () => {
    const a = extractFailure({ checks: ['build'] });
    assert.match(a.signature, /^[0-9a-f]{16}$/);
    assert.deepEqual(a.signatureParts, ['build:']);
  });

  it('honors a precomputed signature', () => {
    const a = extractFailure({ checks: ['build'], signature: 'deadbeef12345678' });
    assert.equal(a.signature, 'deadbeef12345678');
  });
});

// --- consultRunbook: signature → advisory ---------------------------------------
describe('consultRunbook', () => {
  it('injects the recorded fix on a signature hit', () => {
    const sig = 'abc123def4567890';
    const rb = tmpRunbook(ENTRY('ci-red', sig, 'pin the dep'));
    const r = consultRunbook({ signature: sig, runbookPath: rb, brief: 'BRIEF' });
    assert.equal(r.hit, true);
    assert.equal(r.entry, 'ci-red');
    assert.match(r.advisory, /pin the dep/);
    assert.match(r.brief, /Runbook hit/);
    assert.match(r.brief, /BRIEF$/);
  });

  it('returns the brief byte-identical on a miss (no noise)', () => {
    const rb = tmpRunbook(ENTRY('ci-red', 'abc123', 'fix'));
    const r = consultRunbook({ signature: 'ffffffffffffffff', runbookPath: rb, brief: 'BRIEF' });
    assert.equal(r.hit, false);
    assert.equal(r.brief, 'BRIEF');
  });

  it('unreadable store degrades to a graceful miss', () => {
    const r = consultRunbook({ signature: 'abc123', runbookPath: '/nonexistent/rb.md', brief: 'B' });
    assert.equal(r.hit, false);
    assert.ok(r.runbookError);
  });
});

// --- planRepair: bounded decision --------------------------------------------------
describe('planRepair', () => {
  const checks = ['build', 'test'];
  const SIG = stallGuard.signatureFor(checks);

  it('decides repair while budget is available', () => {
    const r = planRepair({ markers: [], checkNames: checks, attempts: 0, maxRepairs: 3, stallMax: 3 });
    assert.equal(r.decision, 'repair');
    assert.equal(r.reason, 'budget-available');
  });

  it('escalates on stall signature burn', () => {
    const markers = [1, 2, 3].map((i) => ({ attempt: i, sha: `s${i}`, sig: SIG }));
    const r = planRepair({ markers, checkNames: checks, attempts: 3, maxRepairs: 9, stallMax: 3 });
    assert.equal(r.decision, 'escalate');
    assert.match(r.reason, /^repair-stall:/);
  });

  it('escalates when the flat budget is exhausted', () => {
    const r = planRepair({ markers: [], checkNames: checks, attempts: 3, maxRepairs: 3, stallMax: 9 });
    assert.equal(r.decision, 'escalate');
    assert.match(r.reason, /repairs-exhausted/);
  });

  it('base fault escalates as base-contract-error before spending an engine run', () => {
    const r = planRepair({ markers: [], checkNames: checks, attempts: 0, maxRepairs: 3, baseFault: true });
    assert.equal(r.decision, 'escalate');
    assert.equal(r.reason, 'base-contract-error');
  });

  it('sanitize-blocked quarantines before any other decision', () => {
    const r = planRepair({ markers: [], checkNames: checks, attempts: 0, sanitizeBlocked: true, baseFault: true });
    assert.equal(r.decision, 'quarantine');
  });
});

// --- classifyStall: transport / tooling / spec --------------------------------------
describe('classifyStall', () => {
  it('provider-class exit → transport with bounded retries', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 20 });
    assert.equal(r.class, 'transport');
    assert.equal(r.retry, RETRY_POLICY.transport);
    assert.equal(r.retry.retries, 3);
  });

  it('auth-class exit → transport', () => {
    assert.equal(classifyStall({ engine: 'scc', exitCode: 21 }).class, 'transport');
  });

  it('timeout exit → transport', () => {
    assert.equal(classifyStall({ engine: 'scc', exitCode: 24 }).class, 'transport');
  });

  it('engine-budget exits → tooling with a single requeue', () => {
    for (const code of [22, 23]) {
      const r = classifyStall({ engine: 'scc', exitCode: code });
      assert.equal(r.class, 'tooling', `exit ${code}`);
      assert.equal(r.retry.retries, 1);
    }
  });

  it('validation exit → spec with zero retries', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 10 });
    assert.equal(r.class, 'spec');
    assert.equal(r.retry.retries, 0);
  });

  it('success exit is not a stall', () => {
    const r = classifyStall({ engine: 'scc', exitCode: 0 });
    assert.equal(r.class, null);
    assert.equal(r.retry, null);
  });

  it('engine-exit-N outcome strings resolve through the exit map', () => {
    assert.equal(classifyStall({ engine: 'scc', outcome: 'engine-exit-20' }).class, 'transport');
  });

  it('pipeline outcomes classify without an exit code', () => {
    assert.equal(classifyStall({ outcome: 'no-changes' }).class, 'spec');
    assert.equal(classifyStall({ outcome: 'base-contract-error' }).class, 'spec');
    assert.equal(classifyStall({ outcome: 'push-rejected' }).class, 'transport');
    assert.equal(classifyStall({ outcome: 'commit-failed' }).class, 'tooling');
  });

  it('unmapped classes fall back to spec (fail-closed)', () => {
    assert.equal(selfHeal.stallClassOf('Wut'), 'spec');
    assert.equal(selfHeal.stallClassOf('AgentError'), 'transport');
    assert.equal(selfHeal.stallClassOf('MaxStepsExceeded'), 'tooling');
    assert.equal(selfHeal.stallClassOf('ValidationError'), 'spec');
  });

  it('unknown engine degrades to spec via exit-N class name', () => {
    const r = classifyStall({ engine: 'nope', exitCode: 9 });
    assert.equal(r.class, 'spec');
  });

  it('every mapped class name lands on a declared stall class', () => {
    for (const [name, cls] of Object.entries(STALL_CLASS_MAP)) {
      assert.ok(['transport', 'tooling', 'spec'].includes(cls), `${name} → ${cls}`);
    }
  });
});

// --- escalate: classify + postmortem via forge client --------------------------------
describe('escalate', () => {
  const fakeClient = (open = [], closed = []) => {
    const calls = { created: [], comments: [], reopens: [] };
    return {
      calls,
      listIssues: ({ state }) => (state === 'open' ? open : closed),
      listComments: () => [],
      addComment: (c) => calls.comments.push(c),
      reopenIssue: (r) => calls.reopens.push(r),
      ensureLabel: () => {},
      createIssue: (i) => { calls.created.push(i); return { number: 900, url: 'u/900' }; },
    };
  };

  it('files a canonical postmortem through the injected forge client', () => {
    const client = fakeClient();
    const r = escalate({
      postmortemOpts: {
        repo: 'o/n', component: 'ci-repair', class: 'repair-stalled',
        location: 'gh checks', title: 'pm', signatureHash: 'deadbeef12345678',
      },
      outcome: 'engine-exit-20',
    }, client);
    assert.equal(r.postmortem.action, 'filed');
    assert.equal(r.stallClass.class, 'transport');
    assert.equal(client.calls.created.length, 1);
    assert.match(client.calls.created[0].body, /deadbeef12345678/);
    assert.equal(r.failureClass, 'repair-stalled');
  });

  it('recurrence comments on the open canonical instead of refiling', () => {
    const body = `x <!-- ai-sdlc-failure-signature: deadbeef12345678 -->`;
    const client = fakeClient([{ number: 7, title: 'pm', body, url: 'u/7' }]);
    const r = escalate({
      postmortemOpts: {
        repo: 'o/n', component: 'ci-repair', class: 'repair-stalled',
        location: 'gh checks', title: 'pm', signatureHash: 'deadbeef12345678',
      },
    }, client);
    assert.equal(r.postmortem.action, 'commented');
    assert.equal(r.postmortem.issueNumber, 7);
    assert.equal(client.calls.created.length, 0);
  });

  it('applies needs-human through the injected exec', () => {
    const client = fakeClient();
    const calls = [];
    const exec = (argv) => { calls.push(argv.join(' ')); return ''; };
    const r = escalate({
      postmortemOpts: null,
      labelOpts: { repo: 'o/n', pr: 5 },
      outcome: 'engine-exit-23',
    }, client, exec);
    assert.equal(r.label.applied, true);
    assert.ok(calls.some((c) => c.includes('needs-human')));
    assert.equal(r.postmortem, null);
  });
});

// --- sentinel: main-guard poll ---------------------------------------------------------
describe('sentinel', () => {
  it('returns ok on unknown verdict (fail-open, no latch)', () => {
    const exec = (argv) => (argv[0] === 'run' ? '[]' : '[]');
    const r = sentinel({ repo: 'o/n' }, exec);
    assert.equal(r.action, 'ok');
    assert.equal(r.verdict, 'unknown');
  });

  it('halts on a red gate run (dry-run: no issue filed)', () => {
    const exec = (argv) => {
      if (argv[0] === 'run') {
        return JSON.stringify([{ conclusion: 'failure', status: 'completed', workflowName: 'ci', databaseId: 1 }]);
      }
      return '[]';
    };
    const r = sentinel({ repo: 'o/n', apply: false }, exec);
    assert.equal(r.action, 'halt');
    assert.equal(r.verdict, 'red');
  });
});

// --- sequence: the full pipeline -------------------------------------------------------
describe('sequence', () => {
  it('runs extract → consult → decide and returns one plan object', () => {
    const rb = tmpRunbook(ENTRY('m', 'x', 'fix'));
    const r = sequence({ checks: ['build'], runbookPath: rb });
    assert.ok(r.artifact.signature);
    assert.equal(r.plan.decision, 'repair');
    assert.equal(r.runbook.hit, false);
  });

  it('advisory hit lands in the consulted brief', () => {
    const sig = 'abc123def4567890';
    const rb = tmpRunbook(ENTRY('m', sig, 'do the fix'));
    const r = sequence({ checks: ['build'], signature: sig, runbookPath: rb, brief: 'PROMPT' });
    assert.equal(r.runbook.hit, true);
    assert.match(r.runbook.brief, /do the fix/);
  });

  it('escalating sequence carries stallClass + failureClass', () => {
    const checks = ['build'];
    const sig = stallGuard.signatureFor(checks);
    const markers = [1, 2, 3].map((i) => ({ attempt: i, sha: `s${i}`, sig }));
    const r = sequence({
      checks, markers, attempts: 3, maxRepairs: 9, stallMax: 3,
      engine: 'scc', exitCode: 20,
    });
    assert.equal(r.plan.decision, 'escalate');
    assert.equal(r.stallClass.class, 'transport');
    assert.equal(r.stallClass.retry.retries, 3);
    assert.equal(r.failureClass, 'repair-stalled');
  });
});

// --- AC deltas ---------------------------------------------------------------------------
describe('issue #187 deltas', () => {
  it('artifact shape == the ci-repair failure.json contract', () => {
    const a = extractFailure({ checks: ['b'], signature: 'deadbeef12345678', rootCause: 'r', baseFault: true });
    for (const k of ['signature', 'signatureParts', 'rootCause', 'checks', 'baseFault']) {
      assert.ok(k in a, `artifact.${k}`);
    }
  });

  it('postmortem works through any client implementing the forge surface (not gh-specific)', () => {
    // A forge-adapter fake — no `gh` anywhere in the call path.
    const client = {
      listIssues: () => [],
      listComments: () => [],
      addComment: () => {},
      reopenIssue: () => {},
      createIssue: () => ({ number: 1, url: 'forge://issue/1' }),
    };
    const r = escalate({
      postmortemOpts: {
        repo: 'org/proj', component: 'verify', class: 'verify-failure',
        location: 'verify.commands', title: 't', signatureHash: 'aaaabbbbccccdddd',
      },
    }, client);
    assert.equal(r.postmortem.action, 'filed');
  });

  it('retry policy is a plain data contract a Tekton Task can map to `retries`', () => {
    for (const cls of Object.values(STALL_CLASS_MAP)) {
      const p = RETRY_POLICY[cls];
      assert.ok(Number.isInteger(p.retries) && p.retries >= 0);
      assert.ok(typeof p.backoff === 'string');
    }
  });

  it('outcome map covers the ci-repair outcome vocabulary', () => {
    for (const out of ['no-changes', 'base-contract-error', 'push-rejected', 'commit-failed']) {
      assert.ok(OUTCOME_CLASS_MAP[out], out);
    }
  });
});
