// audit-query.test.js — node --test coverage for the ledger audit query
// (issue #174). Fixture ledgers are produced by the real run-ledger CLI —
// the same writer the runtime stages use — then queried end-to-end via the
// audit-query CLI in a temp dir per test.
'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const CLI = path.join(__dirname, 'audit-query.js');
const LEDGER_CLI = path.join(__dirname, '..', '.github', 'actions', 'run-ledger', 'run-ledger.js');

function tmpdir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'audit-query-test-'));
}

function run(args, opts = {}) {
  return execFileSync('node', [CLI, ...args], { encoding: 'utf8', env: { ...process.env, ...opts.env } });
}

function runFail(args, opts = {}) {
  try {
    const stdout = run(args, opts);
    return { code: 0, stdout, stderr: '' };
  } catch (e) {
    return { code: e.status, stdout: e.stdout || '', stderr: e.stderr || '' };
  }
}

function ledger(args, opts = {}) {
  return execFileSync('node', [LEDGER_CLI, ...args], { encoding: 'utf8', env: { ...process.env, ...opts.env } });
}

function readJsonl(file) {
  return fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l));
}

function writeJsonl(file, recs) {
  fs.writeFileSync(file, recs.map((r) => JSON.stringify(r)).join('\n') + '\n');
}

let keySeq = 0;
function keypair(dir) {
  const { privateKey, publicKey } = crypto.generateKeyPairSync('ed25519');
  const n = keySeq++;
  const priv = path.join(dir, `k${n}.pem`);
  const pub = path.join(dir, `k${n}.pub.pem`);
  fs.writeFileSync(priv, privateKey.export({ format: 'pem', type: 'pkcs8' }));
  fs.writeFileSync(pub, publicKey.export({ format: 'pem', type: 'spki' }));
  return { priv, pub };
}

const RUN_ENV = {
  GITHUB_RUN_ID: '99123',
  GITHUB_RUN_ATTEMPT: '1',
  GITHUB_WORKFLOW: 'ai-sdlc',
  GITHUB_JOB: 'stage',
  GITHUB_EVENT_NAME: 'workflow_call',
  GITHUB_SHA: 'deadbeef',
  GITHUB_ACTOR: 'ai-sdlc-runtime[bot]',
  GITHUB_SERVER_URL: 'https://github.com',
  GITHUB_REPOSITORY: 'o/r',
};

/* Full pipeline fixture for issue 7 / pr 42: intake (dor) → implement
 * (engine outputs) → automerge (escalated), cross-file chained via heads. */
function pipelineFixture(dir, { sign = true, automergeData = '{"action":"escalated","reason":"repairs-exhausted:ci,build"}' } = {}) {
  const { priv, pub } = sign ? keypair(dir) : { priv: null, pub: null };
  const files = {};
  const mk = (name, stage, extra, prev) => {
    const f = path.join(dir, `ledger-${name}.jsonl`);
    const args = ['append', '--ledger', f, '--stage', stage, '--repo', 'o/r', ...extra];
    if (prev) args.push('--prev', prev);
    ledger(args, { env: RUN_ENV });
    const sealArgs = ['seal', '--ledger', f];
    if (priv) sealArgs.push('--key-file', priv);
    ledger(sealArgs, { env: RUN_ENV });
    files[name] = f;
    return readJsonl(f).find((r) => r.type === 'seal').head;
  };
  mk('intake', 'intake', ['--issue', '7', '--data', '{"armed":"true","dispatch":"true","failed":""}']);
  const implHead = mk('implement', 'implement', ['--issue', '7', '--pr', '42',
    '--data', '{"model":"gpt-x","exit_code":0,"primary_exit":0,"fallback_used":false}']);
  mk('automerge', 'automerge', ['--pr', '42', '--data', automergeData], implHead);
  return { files, priv, pub };
}

test('trace --issue: stage-by-stage trail with citations and chain links', () => {
  const d = tmpdir();
  pipelineFixture(d);
  const out = run(['trace', '--dir', d, '--issue', '7']);
  assert.match(out, /audit trail — issue #7 \(3 entries\)/);
  const intake = out.indexOf('intake seq=1');
  const impl = out.indexOf('implement seq=1');
  assert.ok(intake !== -1 && impl !== -1 && intake < impl, 'trail is stage-ordered');
  assert.match(out, /decision: dispatched — Definition-of-Ready passed/);
  assert.match(out, /decision: implemented — engine run succeeded/);
  assert.match(out, /engine: model=gpt-x exit=0/);
  assert.match(out, /chains from ledger-implement\.jsonl/);
  assert.match(out, /hash=[0-9a-f]{16}…/, 'entries cite their hash');
  assert.doesNotMatch(out, /TAMPER/);
});

test('trace --pr: linkage expands to the dispatching issue — full causal chain', () => {
  const d = tmpdir();
  pipelineFixture(d);
  const out = run(['trace', '--dir', d, '--pr', '42']);
  // implement records issue=7 + pr=42, so a PR query also surfaces the
  // intake entry that dispatched the issue — the causal chain is complete.
  assert.match(out, /\(3 entries\)/);
  assert.match(out, /decision: dispatched — Definition-of-Ready passed/);
  assert.match(out, /decision: escalated — repairs-exhausted:ci,build/);
  assert.match(out, /label: \+needs-human/);
});

test('trace --run: selects by workflow run id', () => {
  const d = tmpdir();
  pipelineFixture(d);
  const out = run(['trace', '--dir', d, '--run', '99123']);
  assert.match(out, /\(3 entries\)/);
  const miss = run(['trace', '--dir', d, '--run', '55555']);
  assert.match(miss, /\(0 entries\)/);
});

test('why escalated: cites the automerge entry, its reason and hash', () => {
  const d = tmpdir();
  const { files } = pipelineFixture(d);
  const expectedHash = readJsonl(files.automerge)[0].hash;
  const out = run(['why', 'escalated', '--dir', d, '--pr', '42']);
  assert.match(out, /automerge: escalated — repairs-exhausted:ci,build/);
  assert.ok(out.includes(expectedHash.slice(0, 16)), 'evidence cites the entry hash');
  assert.match(out, /integrity: ok/);
});

test('why skipped: intake not-armed and automerge skipped both answer', () => {
  const d = tmpdir();
  // Fixture A: issue not armed (no trigger label).
  const a = path.join(d, 'ledger-intake.jsonl');
  ledger(['append', '--ledger', a, '--stage', 'intake', '--repo', 'o/r', '--issue', '9',
    '--data', '{"armed":"false","dispatch":"false","failed":""}'], { env: RUN_ENV });
  ledger(['seal', '--ledger', a], { env: RUN_ENV });
  // Fixture B: automerge skipped (draft PR).
  const b = path.join(d, 'ledger-automerge.jsonl');
  ledger(['append', '--ledger', b, '--stage', 'automerge', '--repo', 'o/r', '--pr', '77',
    '--data', '{"action":"skipped","reason":"draft"}'], { env: RUN_ENV });
  ledger(['seal', '--ledger', b], { env: RUN_ENV });

  const outA = run(['why', 'skipped', '--dir', d, '--issue', '9']);
  assert.match(outA, /not-armed — trigger label absent/);
  const outB = run(['why', 'skipped', '--dir', d, '--pr', '77']);
  assert.match(outB, /skipped — draft/);
});

test('why skipped via DoR failure names the failed rules', () => {
  const d = tmpdir();
  const f = path.join(d, 'ledger-intake.jsonl');
  ledger(['append', '--ledger', f, '--stage', 'intake', '--repo', 'o/r', '--issue', '11',
    '--data', '{"armed":"true","dispatch":"false","failed":"R3 ≥1 AC item|R6 no TBD in AC"}'], { env: RUN_ENV });
  ledger(['seal', '--ledger', f], { env: RUN_ENV });
  const out = run(['why', 'skipped', '--dir', d, '--issue', '11']);
  assert.match(out, /dor-failed — Definition-of-Ready failed: R3 ≥1 AC item, R6 no TBD in AC/);
});

test('labels op + why labeled: intake dispatch applies scc:queued with citation', () => {
  const d = tmpdir();
  pipelineFixture(d);
  const out = run(['labels', '--dir', d, '--issue', '7']);
  assert.match(out, /\+scc:queued — DoR passed/);
  const esc = run(['labels', '--dir', d, '--pr', '42']);
  assert.match(esc, /\+needs-human — escalated \(repairs-exhausted:ci,build\)/);
  const why = run(['why', 'labeled', '--dir', d, '--issue', '7', '--json']);
  const body = JSON.parse(why);
  assert.equal(body.answer.matched, true);
  assert.ok(body.answer.evidence.some((e) => e.label === 'scc:queued'));
});

test('miss: unknown issue answers cleanly — empty trail, exit 0', () => {
  const d = tmpdir();
  pipelineFixture(d);
  const out = run(['trace', '--dir', d, '--issue', '999']);
  assert.match(out, /\(0 entries\)/);
  assert.match(out, /no ledger entries matched/);
  const why = run(['why', 'escalated', '--dir', d, '--issue', '999']);
  assert.match(why, /no ledger entries explain "escalated"/);
});

test('malformed line: flagged as a gap, valid entries still answered', () => {
  const d = tmpdir();
  const { files } = pipelineFixture(d);
  const recs = readJsonl(files.implement);
  // Splice a corrupt line between the entry and the seal.
  const raw = [JSON.stringify(recs[0]), '{not json!!', JSON.stringify(recs[1])].join('\n') + '\n';
  fs.writeFileSync(files.implement, raw);
  const out = run(['trace', '--dir', d, '--issue', '7']);
  assert.match(out, /malformed line 2/);
  assert.match(out, /decision: dispatched/, 'other ledgers still load');
  assert.match(out, /decision: implemented/, 'entry before the gap is recovered');
});

test('tamper-evidence: modified stage data is FLAGGED and --strict exits 1', () => {
  const d = tmpdir();
  const { files, pub } = pipelineFixture(d);
  const recs = readJsonl(files.automerge);
  recs[0].data.action = 'merged'; // forged decision
  writeJsonl(files.automerge, recs);

  const out = runFail(['trace', '--dir', d, '--pr', '42', '--pubkey-file', pub]);
  assert.equal(out.code, 0, 'non-strict still answers');
  assert.match(out.stdout, /TAMPER: .*hash mismatch|data_sha256 mismatch/);
  assert.match(out.stdout, /✗ .*ledger-automerge\.jsonl/, 'file is flagged');
  // The seal itself still verifies: it signs (head, count) over the stored
  // hash field, which the forger left intact — the entry-level recompute is
  // what catches the edit. Tampering with the seal count breaks it too.
  const recs2 = readJsonl(files.intake);
  recs2[recs2.length - 1].entries = 99;
  writeJsonl(files.intake, recs2);
  const out2 = runFail(['trace', '--dir', d, '--issue', '7', '--pubkey-file', pub]);
  assert.match(out2.stdout, /seal entries mismatch/);

  const strict = runFail(['trace', '--dir', d, '--pr', '42', '--pubkey-file', pub, '--strict']);
  assert.equal(strict.code, 1);

  const why = runFail(['why', 'escalated', '--dir', d, '--pr', '42', '--pubkey-file', pub, '--json']);
  const body = JSON.parse(why.stdout);
  assert.equal(body.answer.matched, false, 'forged action must not answer the query');
});

test('signed seal verifies against the public key; wrong key flags it', () => {
  const d = tmpdir();
  const { pub } = pipelineFixture(d, { sign: true });
  const ok = run(['trace', '--dir', d, '--issue', '7', '--pubkey-file', pub]);
  assert.match(ok, /signed\(verified\)/);
  const other = keypair(d);
  const bad = runFail(['trace', '--dir', d, '--issue', '7', '--pubkey-file', other.pub, '--strict']);
  assert.equal(bad.code, 1);
  assert.match(bad.stdout + bad.stderr, /signature INVALID|different key/);
});

test('unsigned seal: chain still audited, attribution flagged as unsigned', () => {
  const d = tmpdir();
  pipelineFixture(d, { sign: false });
  const out = run(['trace', '--dir', d, '--issue', '7']);
  assert.match(out, /unsigned seal/);
  assert.match(out, /attribution not possible/);
  assert.match(out, /integrity=ok/, 'unsigned ≠ tampered');
});

test('partial bundle: unresolved cross-file prev is reported, not forged', () => {
  const d = tmpdir();
  // A review ledger seeded from a head that is not in the bundle.
  const orphan = path.join(d, 'ledger-review.jsonl');
  ledger(['append', '--ledger', orphan, '--stage', 'review', '--repo', 'o/r', '--pr', '42',
    '--prev', 'a'.repeat(64), '--data', '{"verdict":"blocked","reason":"findings","risk_tier":"high"}'], { env: RUN_ENV });
  ledger(['seal', '--ledger', orphan], { env: RUN_ENV });
  const out = run(['trace', '--dir', d, '--pr', '42']);
  assert.match(out, /link=unresolved/);
  assert.match(out, /prior-stage ledger absent/);
  assert.match(out, /decision: verdict-blocked — findings/);
});

test('missing path is a usage error (exit 2); empty dir reports no files', () => {
  const d = tmpdir();
  const bad = runFail(['trace', '--dir', path.join(d, 'nope')]);
  assert.equal(bad.code, 2);
  assert.match(bad.stderr, /path not found/);
  const empty = runFail(['trace', '--dir', d]);
  assert.equal(empty.code, 2);
  assert.match(empty.stderr, /no ledger files/);
});

test('verify stage + generic stage decisions extract correctly', () => {
  const d = tmpdir();
  const v = path.join(d, 'ledger-verify.jsonl');
  ledger(['append', '--ledger', v, '--stage', 'verify', '--repo', 'o/r', '--pr', '42',
    '--data', '{"failed":"0","base":"main","sha":"abc"}'], { env: RUN_ENV });
  ledger(['seal', '--ledger', v], { env: RUN_ENV });
  const out = run(['trace', '--dir', d, '--pr', '42']);
  assert.match(out, /decision: verify-passed/);
});

test('--json emits machine-readable events with integrity per entry', () => {
  const d = tmpdir();
  pipelineFixture(d);
  const out = run(['trace', '--dir', d, '--issue', '7', '--json']);
  const body = JSON.parse(out);
  assert.equal(body.events.length, 3);
  assert.ok(body.events.every((e) => e.integrity.ok === true));
  assert.ok(body.events.every((e) => /^[0-9a-f]{64}$/.test(e.hash)));
  assert.equal(body.files.length, 3);
  assert.equal(body.flagged, 0);
});
