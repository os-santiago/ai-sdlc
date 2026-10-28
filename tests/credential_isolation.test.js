// tests/credential_isolation.test.js
// Structural contract for the forge credential trust boundary in
// ai-sdlc-implement.yml (issue #210). The runtime/engine must never reach a
//   forge write credential — asserted here against the workflow source:
//
//   - two scoped installation tokens are minted (read: contents/issues/
//     pull-requests read; write: same scopes at write) — never one token
//     with the App's full permission set;
//   - the write token is minted strictly AFTER the engine step;
//   - the engine step environment carries FORGE_TOKEN_READ and cannot carry
//     FORGE_TOKEN_WRITE or the write-mint output;
//   - no pre-engine step may reference the write token;
//   - every forge write goes through deterministic post-script steps
//     (apply-publish composite action + FORGE_TOKEN_WRITE env), never an
//     inline engine-adjacent run step;
//   - the live negative probe (a real label write that must 403/404) runs
//     before the engine;
//   - the job-level GITHUB_TOKEN stays at models: read (no write grants).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const WF = path.join(__dirname, '..', '.github', 'workflows', 'ai-sdlc-implement.yml');
const src = fs.readFileSync(WF, 'utf8');

// Job-level steps sit at 6-space indent "- name:"/"— uses:". Split the file
// into per-step chunks so each assertion can be scoped to one step.
const chunks = src.split(/\n(?=      - )/);
const steps = chunks.slice(1); // chunk 0 = everything before the first step
// Comment lines belong to no step — strip them before token assertions so
// explanatory prose can name FORGE_TOKEN_* without tripping the boundary.
const body = (chunk) =>
  chunk.split('\n').filter((l) => !/^\s*#/.test(l)).join('\n');
const indexOf = (needle) => {
  const i = steps.findIndex((c) => c.includes(needle));
  assert.notEqual(i, -1, `no step contains ${needle}`);
  return i;
};

const engineIdx = indexOf('id: engine');
const readMintIdx = indexOf('id: app-token-read');
const writeMintIdx = indexOf('id: app-token-write');
const APPLY_PUBLISH_RE = /uses: (?:\.\/|os-santiago\/ai-sdlc\/)\.github\/actions\/apply-publish/;
const applyIdx = steps.findIndex((c) => APPLY_PUBLISH_RE.test(c));
assert.ok(applyIdx !== -1, 'no step references the apply-publish action');
const probeIdx = indexOf('Verify read-only forge credential');

test('read-scoped token minted before checkout/engine; write-scoped minted only after the engine', () => {
  assert.ok(readMintIdx < engineIdx, 'read mint must precede the engine step');
  assert.ok(writeMintIdx > engineIdx,
    'FORGE_TOKEN_WRITE must not exist while the engine runs');
  assert.ok(applyIdx > writeMintIdx, 'apply-publish consumes the write mint');
  assert.match(steps[applyIdx],
    /token: \$\{\{ steps\.app-token-write\.outputs\.token \}\}/);
});

test('token mints are permission-scoped (least privilege)', () => {
  const readMint = steps[readMintIdx];
  assert.match(readMint, /permission-contents: read/);
  assert.match(readMint, /permission-issues: read/);
  assert.match(readMint, /permission-pull-requests: read/);
  assert.doesNotMatch(readMint, /permission-\w+: write/);

  const writeMint = steps[writeMintIdx];
  assert.match(writeMint, /permission-contents: write/);
  assert.match(writeMint, /permission-issues: write/);
  assert.match(writeMint, /permission-pull-requests: write/);
});

test('the legacy full-scope mint (id: app-token) is gone — no unscoped forge token', () => {
  assert.doesNotMatch(src, /steps\.app-token\.outputs\.token/);
  assert.ok(!steps.some((c) => /\bid: app-token\s*$/.test(c)),
    'a plain `app-token` mint inherits the whole App permission set');
});

test('engine step env: FORGE_TOKEN_READ set, FORGE_TOKEN_WRITE absent', () => {
  const engine = body(steps[engineIdx]);
  assert.match(engine,
    /FORGE_TOKEN_READ: \$\{\{ steps\.app-token-read\.outputs\.token \}\}/);
  assert.doesNotMatch(engine, /FORGE_TOKEN_WRITE/);
  assert.doesNotMatch(engine, /app-token-write/);
});

test('no pre-engine step can reference the write token', () => {
  for (const c of steps.slice(0, writeMintIdx)) {
    const b = body(c);
    assert.doesNotMatch(b, /app-token-write/,
      `pre-mint step must not reference the write token:\n${c.slice(0, 120)}`);
    // No env binding and no shell expansion of the write credential —
    // the name alone in a log string is prose, not a reference.
    assert.doesNotMatch(b, /FORGE_TOKEN_WRITE\s*[:=]/);
    assert.doesNotMatch(b, /\$\{?FORGE_TOKEN_WRITE\}?/);
  }
});

test('every forge write goes through a deterministic post-script step under FORGE_TOKEN_WRITE', () => {
  for (const c of steps.slice(writeMintIdx)) {
    const b = body(c);
    if (!/gh (issue|pr|label) |git .*push/.test(b)) continue; // forge-mutating chunk
    assert.ok(
      /FORGE_TOKEN_WRITE: \$\{\{ steps\.app-token-write\.outputs\.token \}\}/.test(b) ||
        APPLY_PUBLISH_RE.test(b),
      `forge write outside the post-script boundary:\n${c.slice(0, 200)}`);
  }
});

test('a live negative probe asserts the engine credential cannot write, before the engine runs', () => {
  assert.ok(probeIdx > readMintIdx && probeIdx < engineIdx,
    'probe must run between the read mint and the engine step');
  const probe = steps[probeIdx];
  assert.match(probe, /GH_TOKEN: \$\{\{ steps\.app-token-read\.outputs\.token \}\}/);
  assert.match(probe, /gh api -X POST "repos\/\$REPO\/issues\/\$ISSUE\/labels"/);
  assert.match(probe, /exit 1/); // 2xx → fail closed
});

test('job-level GITHUB_TOKEN keeps least privilege (models: read only)', () => {
  const perms = src.match(/\npermissions:\n((?: {2}.+\n)+)/);
  assert.ok(perms, 'workflow must declare an explicit permissions block');
  assert.match(perms[1], /models: read/);
  assert.doesNotMatch(perms[1], /write/);
});
