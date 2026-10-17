// .github/actions/apply-publish/apply_publish.test.js
// node --test suite for the deterministic forge-write post-script
// (issue #210). Covers three surfaces:
//   1. render_pr_body.js — PR body is a pure function of the run manifest.
//   2. apply_and_publish.sh — env contract (FORGE_TOKEN_WRITE required) and
//      the DRY_RUN apply path in a real temp git repo (stage → exclude
//      engine artifacts + .github/workflows → commit → branch slug).
//   3. The script is the only forge-write entry point: it refuses to run
//      without the write credential in its environment.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const ACTION_DIR = __dirname;
const SCRIPT = path.join(ACTION_DIR, 'apply_and_publish.sh');
const RENDER = path.join(ACTION_DIR, 'render_pr_body.js');
const { render } = require(RENDER);

// --- render_pr_body.js ------------------------------------------------------

test('render: clean primary run', () => {
  const body = render({
    manifest: { tool_calls_total: 17, model: 'scc-model' },
    run: { model: 'openai/gpt-x', fallback_used: false },
    issue: '42',
    runId: '123456',
  });
  assert.match(body, /model=`openai\/gpt-x`/);
  assert.match(body, /fallback=no/);
  assert.match(body, /steps=17/);
  assert.match(body, /Closes #42/);
  assert.match(body, /\(run 123456\)/);
});

test('render: fallback run reports primary exit + reason', () => {
  const body = render({
    manifest: {},
    run: {
      model: 'fallback-model',
      fallback_used: true,
      primary_model: 'primary-model',
      primary_exit: 20,
      fallback_reason: 'provider',
    },
    issue: '7',
    runId: '9',
  });
  assert.match(body, /fallback=yes — primary `primary-model` exited 20 \(provider\)/);
  assert.match(body, /steps=n\/a/);
  assert.match(body, /Closes #7/);
});

test('render: missing artifacts degrade to n/a instead of throwing', () => {
  const body = render({ issue: '1' });
  assert.match(body, /model=`n\/a`/);
  assert.match(body, /Closes #1/);
});

test('render CLI: reads artifacts from cwd, honours ISSUE/RUN_ID env', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ap-render-'));
  fs.writeFileSync(path.join(dir, 'scc-manifest.json'),
    JSON.stringify({ tool_calls_total: 3 }));
  fs.writeFileSync(path.join(dir, 'ai-sdlc-run.json'),
    JSON.stringify({ model: 'cli-model', fallback_used: false }));
  const res = spawnSync(process.execPath, [RENDER], {
    cwd: dir,
    env: { ...process.env, ISSUE: '55', RUN_ID: '77' },
    encoding: 'utf8',
  });
  assert.equal(res.status, 0, res.stderr);
  assert.match(res.stdout, /model=`cli-model`/);
  assert.match(res.stdout, /steps=3/);
  assert.match(res.stdout, /Closes #55/);
  assert.match(res.stdout, /\(run 77\)/);
});

// --- apply_and_publish.sh — env contract ------------------------------------

function runScript(dir, env) {
  return spawnSync('bash', [SCRIPT], {
    cwd: dir,
    env: { ...process.env, ...env },
    encoding: 'utf8',
  });
}

test('script fails closed when FORGE_TOKEN_WRITE is absent', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ap-env-'));
  const out = path.join(dir, 'gh-out');
  const res = runScript(dir, {
    REPO: 'owner/repo', ISSUE: '1', GITHUB_OUTPUT: out,
    FORGE_TOKEN_WRITE: '',
  });
  assert.notEqual(res.status, 0, 'expected non-zero exit without the write token');
  assert.match(res.stderr, /FORGE_TOKEN_WRITE/);
});

test('script fails closed when REPO or ISSUE are absent', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ap-env-'));
  const out = path.join(dir, 'gh-out');
  const res = runScript(dir, {
    FORGE_TOKEN_WRITE: 't', REPO: '', ISSUE: '', GITHUB_OUTPUT: out,
  });
  assert.notEqual(res.status, 0);
  assert.match(res.stderr, /REPO|required/);
});

// --- apply_and_publish.sh — DRY_RUN apply path -------------------------------

function makeRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ap-repo-'));
  const git = (args) => {
    const r = spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
    assert.equal(r.status, 0, `git ${args.join(' ')}: ${r.stderr}`);
    return r;
  };
  git(['init', '-b', 'main']);
  git(['config', 'user.email', 'test@example.com']);
  git(['config', 'user.name', 'test']);
  fs.writeFileSync(path.join(dir, 'seed.txt'), 'seed\n');
  git(['add', '-A']);
  git(['commit', '-q', '-m', 'seed']);
  return { dir, git };
}

test('dry-run: applies the working-tree diff, excludes engine artifacts + workflows', () => {
  const { dir, git } = makeRepo();
  // Real source change (should land), a workflow file (must be stripped),
  // and engine session artifacts (evidence only, never committed).
  fs.mkdirSync(path.join(dir, 'src'), { recursive: true });
  fs.writeFileSync(path.join(dir, 'src', 'change.txt'), 'engine output\n');
  fs.mkdirSync(path.join(dir, '.github', 'workflows'), { recursive: true });
  fs.writeFileSync(path.join(dir, '.github', 'workflows', 'evil.yml'), 'on: push\n');
  fs.writeFileSync(path.join(dir, 'scc-manifest.json'), '{}');
  fs.writeFileSync(path.join(dir, 'ai-sdlc-run.json'), '{}');
  fs.writeFileSync(path.join(dir, 'issue.json'),
    JSON.stringify({ number: 42, title: 'Fix the Widget', body: 'x' }));
  fs.writeFileSync(path.join(dir, 'task-prompt.md'), 'prompt');

  const out = path.join(dir, 'gh-out');
  const res = runScript(dir, {
    FORGE_TOKEN_WRITE: 'deadbeef',
    REPO: 'owner/repo',
    ISSUE: '42',
    BASE: 'main',
    DRY_RUN: '1',
    GITHUB_OUTPUT: out,
  });
  assert.equal(res.status, 0, res.stderr);
  assert.match(res.stdout, /branch=feat\/issue-42-fix-the-widget/);
  assert.match(res.stdout, /committed=true/);
  assert.match(res.stdout, /dry-run/);

  const committed = git(['show', '--pretty=', '--name-only', 'HEAD']).stdout
    .split('\n').filter(Boolean);
  assert.ok(committed.includes('src/change.txt'), `diff applied: ${committed}`);
  assert.ok(!committed.some((f) => f.startsWith('.github/workflows')),
    `workflow path must be stripped: ${committed}`);
  for (const artifact of ['scc-manifest.json', 'ai-sdlc-run.json', 'issue.json', 'task-prompt.md']) {
    assert.ok(!committed.includes(artifact), `${artifact} is run evidence, not diff`);
  }
  assert.match(fs.readFileSync(out, 'utf8'), /branch=feat\/issue-42-fix-the-widget/);
});

test('dry-run: ISSUE_TITLE env overrides issue.json for the branch slug', () => {
  const { dir } = makeRepo();
  fs.writeFileSync(path.join(dir, 'x.txt'), 'y\n');
  const out = path.join(dir, 'gh-out');
  const res = runScript(dir, {
    FORGE_TOKEN_WRITE: 't', REPO: 'o/r', ISSUE: '9', DRY_RUN: '1',
    ISSUE_TITLE: 'Patch: TLS/SSL handshake!!', GITHUB_OUTPUT: out,
  });
  assert.equal(res.status, 0, res.stderr);
  // sed slug: lower → non-alnum to '-' → 40 chars → trailing '-' stripped.
  assert.match(res.stdout, /branch=feat\/issue-9-patch--tls-ssl-handshake/);
});

test('dry-run: empty diff still resolves the branch (exit 0 path semantics)', () => {
  const { dir } = makeRepo();
  fs.writeFileSync(path.join(dir, 'issue.json'),
    JSON.stringify({ title: 'Noop task' }));
  const out = path.join(dir, 'gh-out');
  const res = runScript(dir, {
    FORGE_TOKEN_WRITE: 't', REPO: 'o/r', ISSUE: '3', DRY_RUN: '1',
    GITHUB_OUTPUT: out,
  });
  assert.equal(res.status, 0, res.stderr);
  assert.match(res.stdout, /committed=false/);
  assert.match(res.stdout, /branch=feat\/issue-3-noop-task/);
});
