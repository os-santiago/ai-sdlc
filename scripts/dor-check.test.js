// scripts/dor-check.test.js — fixture tests for the intake
// Definition-of-Ready gate R1–R6 (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { evaluateDor, main } = require('./dor-check');

const env = (over = {}) => ({
  TRIGGER: 'ready-to-implement',
  SKIP_LABELS: '["needs-refinement","needs-human","epic","wontfix"]',
  NOT_READY: 'scc:not-ready',
  REFINE_STATUS: 'refined',
  REFINE_FP: 'fp123',
  ...over,
});

const goodBody = [
  'Enough prose to clear the non-trivial body bar — this issue describes a',
  'real change with context and rationale so the worker can act on it safely.',
  '',
  '## Acceptance criteria',
  '- [ ] does the thing',
].join('\n');

const armed = (over = {}) => ({
  number: 1, title: 'fix(runtime): unbreak the thing', body: goodBody,
  labels: [{ name: 'ready-to-implement' }], ...over,
});

describe('evaluateDor — arming', () => {
  it('no trigger label → not armed, dispatch=false, refinement evidence still rides', () => {
    const r = evaluateDor({ issue: armed({ labels: [] }), env: env() });
    assert.equal(r.armed, 'false');
    assert.equal(r.dispatch, 'false');
    assert.equal(r.failed, '');
    // No approval, no surface pin (issue #122) — unarmed emits dor_sha=''.
    assert.equal(r.dor_sha, '');
    assert.equal(r.refine, 'refined');
    assert.equal(r.refine_fp, 'fp123');
  });

  it('trigger match is case-insensitive on labels and trigger name', () => {
    const r = evaluateDor({
      issue: armed({ labels: [{ name: 'Ready-To-Implement' }] }),
      env: env({ TRIGGER: 'READY-TO-IMPLEMENT' }),
    });
    assert.equal(r.dispatch, 'true');
  });
});

describe('evaluateDor — R1–R6 rows', () => {
  it('fully compliant issue → dispatch=true', () => {
    const r = evaluateDor({ issue: armed(), env: env() });
    assert.equal(r.dispatch, 'true');
    assert.equal(r.failed, '');
  });

  it('R1: non-conventional title fails', () => {
    const r = evaluateDor({ issue: armed({ title: 'unbreak the thing' }), env: env() });
    assert.match(r.failed, /R1 conventional title/);
    assert.equal(r.dispatch, 'false');
  });

  it('R1: every conventional type + scope qualifies', () => {
    for (const t of ['feat', 'fix', 'docs', 'chore', 'refactor', 'perf', 'test', 'build', 'ci', 'security']) {
      const r = evaluateDor({ issue: armed({ title: `${t}(scope): real description` }), env: env() });
      assert.equal(r.checks['R1 conventional title'], true, t);
    }
  });

  it('R2: missing AC section fails', () => {
    const r = evaluateDor({ issue: armed({ body: 'no sections here at all, but long enough to matter '.repeat(3) }), env: env() });
    assert.match(r.failed, /R2 AC section present/);
  });

  it('R3: AC header but zero list items fails', () => {
    const body = `${'context '.repeat(30)}\n\n## Acceptance criteria\nNothing itemized.`;
    const r = evaluateDor({ issue: armed({ body }), env: env() });
    assert.match(r.failed, /R3 ≥1 AC item/);
  });

  it('R4: trivially short body fails', () => {
    const r = evaluateDor({ issue: armed({ body: '## Acceptance criteria\n- do it' }), env: env() });
    assert.match(r.failed, /R4 non-trivial body/);
  });

  it('R5: a skip label (incl. not-ready binding) fails', () => {
    for (const l of ['needs-refinement', 'scc:not-ready']) {
      const r = evaluateDor({ issue: armed({ labels: [{ name: 'ready-to-implement' }, { name: l }] }), env: env() });
      assert.match(r.failed, /R5 no skip-label/, l);
    }
  });

  it('R6: TBD/TODO inside the AC section fails; outside is fine', () => {
    const bad = evaluateDor({ issue: armed({ body: `${goodBody}\n- [ ] TBD` }), env: env() });
    assert.match(bad.failed, /R6 no TBD in AC/);
    const okBody = 'TBD noted up top — ' + 'padding prose. '.repeat(10) + '\n\n## Acceptance criteria\n- [ ] done';
    const ok = evaluateDor({ issue: armed({ body: okBody }), env: env() });
    assert.equal(ok.failed, '');
  });

  it('multiple failures join with |', () => {
    const r = evaluateDor({ issue: armed({ title: 'bad', body: 'tiny' }), env: env() });
    assert.match(r.failed, /\|/);
  });
});

describe('evaluateDor — DoR surface pin (issue #122)', () => {
  it('armed verdicts carry sha256(title+body) regardless of pass/fail', () => {
    const crypto = require('crypto');
    const issue = armed();
    const expected = crypto.createHash('sha256').update(`${issue.title}\n${issue.body}`).digest('hex');
    for (const iss of [issue, armed({ title: 'bad', body: 'tiny' })]) {
      const r = evaluateDor({ issue: iss, env: env() });
      assert.match(r.dor_sha, /^[0-9a-f]{64}$/);
    }
    assert.equal(evaluateDor({ issue, env: env() }).dor_sha, expected);
  });
});

describe('main — file wiring + GITHUB_OUTPUT', () => {
  it('writes dispatch/armed/failed/refine keys', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dor-'));
    const issue = path.join(dir, 'issue.json');
    const outFile = path.join(dir, 'out.txt');
    fs.writeFileSync(issue, JSON.stringify(armed()));
    main(['--issue', issue], { env: { ...env(), GITHUB_OUTPUT: outFile } });
    const out = fs.readFileSync(outFile, 'utf8');
    assert.match(out, /dispatch=true\narmed=true\nfailed=\ndor_sha=[0-9a-f]{64}\nrefine=refined\nrefine_fp=fp123\n/);
  });
});
