#!/usr/bin/env node
// scripts/kill-switch.js — org kill-switch: one action revokes autonomy
// fleet-wide (issue #172, epic #149).
//
// Port of Hermes scripts/kill_switch.js semantics to the GHA-native
// runtime: a single pre-dispatch authorization gate that every autonomous
// stage runs before scheduling work — routing is never permission
// (spec/kill-switch.md). Engaged channels, checked in order:
//
//   1. AI_SDLC_KILL / AI_SDLC_OFF in the environment — workflows pass
//      vars.AI_SDLC_KILL / vars.AI_SDLC_OFF, so a repo Actions variable AND
//      an org-level variable engage the switch with zero API calls.
//   2. An open issue labeled `ai-sdlc:off` in the control repo
//      (--control-repo / AI_SDLC_CONTROL_REPO / vars.AI_SDLC_CONTROL_REPO;
//      default: the repo itself). One issue halts every enrolled repo
//      pointing at that control point — the fleet-wide channel.
//   3. An open issue labeled `ai-sdlc:off` in the repo being processed —
//      the per-repo kill.
//
// Fail-closed: a channel that cannot be read makes the verdict
// `unreadable` and autonomy defaults OFF. Only an all-`clear` verdict
// allows dispatch; `engaged` and `unreadable` both deny.
//
// CLI:
//   node scripts/kill-switch.js check   --repo OWNER/NAME [--control-repo OWNER/NAME]
//                                       [--json] [--github-output] [--soft]
//   node scripts/kill-switch.js engage  --repo OWNER/NAME --actor LOGIN [--reason TEXT] [--json]
//   node scripts/kill-switch.js release --repo OWNER/NAME --actor LOGIN [--reason TEXT] [--json]
//
// `check` exits 0 when autonomy is allowed, 1 when revoked or unreadable
// (`--soft` exits 0 either way — the verdict rides on the outputs for
// callers that soft-skip instead of hard-failing the step).
// `engage`/`release` mutate the control repo (open/comment/close the
// `ai-sdlc:off` issue) recording actor + ISO-8601 timestamp — the audit
// trail lives on the issue timeline and in the caller's run-ledger entry.
'use strict';

const { execFileSync } = require('child_process');

const OFF_LABEL = 'ai-sdlc:off';
const TRUTHY = /^(1|true|yes|on|engaged|kill)$/i;
const FALSY = /^(0|false|no|off|clear|disengaged)$/i;
const GH_TIMEOUT_MS = 20000;
const FETCH_TIMEOUT_MS = 15000;
const MAX_OFF_ISSUES = 10;
const ANNOTATION_MAX = 200;

const USAGE = `usage:
  node scripts/kill-switch.js check   --repo OWNER/NAME [--control-repo OWNER/NAME] [--json] [--github-output] [--soft]
  node scripts/kill-switch.js engage  --repo OWNER/NAME --actor LOGIN [--reason TEXT] [--json]
  node scripts/kill-switch.js release --repo OWNER/NAME --actor LOGIN [--reason TEXT] [--json]`;

// --- env channel -------------------------------------------------------------

// An unrecognized non-empty value is treated as engaged — an operator
// fumbling the flag must never silently disarm the switch.
function envEngaged(env) {
  for (const key of ['AI_SDLC_KILL', 'AI_SDLC_OFF']) {
    const v = String(env[key] == null ? '' : env[key]).trim();
    if (!v) continue;
    if (TRUTHY.test(v)) return `${key}=${v}`;
    if (!FALSY.test(v)) return `${key}=${v} (unrecognized value — fail-closed)`;
  }
  return null;
}

// --- issue-label channel -----------------------------------------------------

// gh CLI read (authenticated — private control repos and private consumers).
function ghOffIssues(repo) {
  try {
    const out = execFileSync('gh', [
      'api', `repos/${repo}/issues?labels=${OFF_LABEL}&state=open&per_page=${MAX_OFF_ISSUES}`,
      '--jq', '[.[] | select(.pull_request == null) | {number, title, created_at, actor: (.user.login // "")}]',
    ], { encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'], timeout: GH_TIMEOUT_MS }).trim();
    return { ok: true, issues: JSON.parse(out || '[]') };
  } catch (e) {
    const err = String((e.stderr || e.message || e)).replace(/\s+/g, ' ').slice(0, ANNOTATION_MAX);
    return { ok: false, error: `gh api failed: ${err}` };
  }
}

// Anonymous REST fallback — a stage token without issues scope on a PUBLIC
// control repo must not blind the check (fail-closed is for unreadable,
// not for under-permissioned-but-public). Network/HTTP errors stay
// unreadable.
async function httpOffIssues(repo) {
  try {
    const res = await fetch(
      `https://api.github.com/repos/${repo}/issues?labels=${OFF_LABEL}&state=open&per_page=${MAX_OFF_ISSUES}`,
      {
        headers: { Accept: 'application/vnd.github+json', 'User-Agent': 'ai-sdlc-kill-switch' },
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
      });
    if (!res.ok) return { ok: false, error: `http-${res.status}` };
    const arr = await res.json();
    if (!Array.isArray(arr)) return { ok: false, error: 'unexpected payload' };
    const issues = arr
      .filter((i) => i && i.pull_request == null)
      .map((i) => ({ number: i.number, title: i.title, created_at: i.created_at, actor: (i.user || {}).login || '' }));
    return { ok: true, issues };
  } catch (e) {
    return { ok: false, error: String((e && e.message) || e).slice(0, ANNOTATION_MAX) };
  }
}

async function readOffIssues(repo) {
  const a = ghOffIssues(repo);
  if (a.ok) return a;
  const b = await httpOffIssues(repo);
  if (b.ok) return b;
  return { ok: false, error: `${a.error}; anonymous read: ${b.error}` };
}

// --- check -------------------------------------------------------------------

function verdict(state, reason, sources) {
  return {
    allowed: state === 'clear',
    state, // clear | engaged | unreadable
    reason,
    sources,
    checked_at: new Date().toISOString(),
  };
}

async function checkKillSwitch({ repo, controlRepo, env = process.env, readIssues = readOffIssues } = {}) {
  if (!repo || !/^[\w.-]+\/[\w.-]+$/.test(repo)) {
    return verdict('unreadable', `invalid --repo '${repo}' (expected OWNER/NAME)`, []);
  }
  const control = controlRepo || env.AI_SDLC_CONTROL_REPO || repo;
  const sources = [];

  // 1. env / Actions-variable channel (vars.AI_SDLC_KILL covers repo + org).
  const envHit = envEngaged(env);
  sources.push({ name: 'env', state: envHit ? 'engaged' : 'clear', reason: envHit || 'AI_SDLC_KILL/AI_SDLC_OFF unset or falsy' });

  // 2. control-repo issue channel (fleet kill). Only read when the control
  //    point is distinct — an unset control adds no channel beyond (3).
  if (control !== repo) {
    const c = await readIssues(control);
    if (!c.ok) {
      sources.push({ name: 'control-issue', state: 'unreadable', reason: `${control}: ${c.error}` });
    } else if (c.issues.length) {
      const i = c.issues[0];
      sources.push({ name: 'control-issue', state: 'engaged', reason: `open ${OFF_LABEL} issue #${i.number} in control repo ${control} (by ${i.actor || '?'} at ${i.created_at || '?'})` });
    } else {
      sources.push({ name: 'control-issue', state: 'clear', reason: `no open ${OFF_LABEL} issue in ${control}` });
    }
  }

  // 3. repo issue channel (per-repo kill).
  const r = await readIssues(repo);
  if (!r.ok) {
    sources.push({ name: 'repo-issue', state: 'unreadable', reason: `${repo}: ${r.error}` });
  } else if (r.issues.length) {
    const i = r.issues[0];
    sources.push({ name: 'repo-issue', state: 'engaged', reason: `open ${OFF_LABEL} issue #${i.number} in ${repo} (by ${i.actor || '?'} at ${i.created_at || '?'})` });
  } else {
    sources.push({ name: 'repo-issue', state: 'clear', reason: `no open ${OFF_LABEL} issue in ${repo}` });
  }

  const engaged = sources.find((s) => s.state === 'engaged');
  if (engaged) return verdict('engaged', engaged.reason, sources);
  const blind = sources.find((s) => s.state === 'unreadable');
  if (blind) return verdict('unreadable', `kill-switch state unreadable (${blind.reason}) — autonomy defaults OFF`, sources);
  return verdict('clear', 'no kill-switch signal on any channel', sources);
}

// --- engage / release --------------------------------------------------------

// Mutating gh call — throws on failure (the kill workflow must not report
// a revocation that never landed).
function ghMut(args) {
  return execFileSync('gh', args, { encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'], timeout: GH_TIMEOUT_MS }).trim();
}

async function ensureLabel(repo, gh) {
  try {
    gh(['label', 'create', OFF_LABEL, '-R', repo, '--color', 'B60205',
      '--description', 'AI-SDLC kill-switch: an open issue carrying this label revokes autonomy', '--force']);
  } catch (e) { /* label may already exist or lack perms — issue create surfaces the real error */ }
}

function auditBody(action, actor, at, reason) {
  return [
    `**AI-SDLC kill-switch — ${action}**`,
    '',
    `- actor: \`${actor || 'unknown'}\``,
    `- at: ${at}`,
    reason ? `- reason: ${reason}` : null,
    '',
    action === 'engaged'
      ? `While this issue is open, every enrolled stage gate denies dispatch (spec/kill-switch.md). Close the issue — or run \`ai-sdlc-kill.yml\` with \`action=release\` — to restore autonomy.`
      : 'Autonomy restored — the next gate check on each enrolled repo reads `clear`.',
  ].filter(Boolean).join('\n');
}

async function engage({ repo, actor, reason, gh = ghMut, readIssues = readOffIssues } = {}) {
  if (!repo || !/^[\w.-]+\/[\w.-]+$/.test(repo)) throw new Error(`invalid --repo '${repo}' (expected OWNER/NAME)`);
  await ensureLabel(repo, gh);
  const r = await readIssues(repo);
  if (!r.ok) throw new Error(`cannot read ${OFF_LABEL} issues in ${repo}: ${r.error}`);
  const at = new Date().toISOString();
  if (r.issues.length) {
    const i = r.issues[0];
    gh(['issue', 'comment', String(i.number), '-R', repo, '--body', auditBody('engaged', actor, at, reason)]);
    return { action: 'engaged', issue: i.number, reused: true, actor: actor || 'unknown', at, reason: reason || '' };
  }
  const url = gh(['issue', 'create', '-R', repo,
    '--title', `AI-SDLC autonomy OFF — engaged by ${actor || 'unknown'}`,
    '--body', auditBody('engaged', actor, at, reason),
    '--label', OFF_LABEL]);
  const num = Number((url.match(/\/issues\/(\d+)/) || [])[1]) || null;
  return { action: 'engaged', issue: num, url: url || '', reused: false, actor: actor || 'unknown', at, reason: reason || '' };
}

async function release({ repo, actor, reason, gh = ghMut, readIssues = readOffIssues } = {}) {
  if (!repo || !/^[\w.-]+\/[\w.-]+$/.test(repo)) throw new Error(`invalid --repo '${repo}' (expected OWNER/NAME)`);
  const r = await readIssues(repo);
  if (!r.ok) throw new Error(`cannot read ${OFF_LABEL} issues in ${repo}: ${r.error}`);
  const at = new Date().toISOString();
  const closed = [];
  for (const i of r.issues) {
    gh(['issue', 'comment', String(i.number), '-R', repo, '--body', auditBody('released', actor, at, reason)]);
    gh(['issue', 'close', String(i.number), '-R', repo]);
    closed.push(i.number);
  }
  return { action: 'released', closed, actor: actor || 'unknown', at, reason: reason || '' };
}

// --- CLI ---------------------------------------------------------------------

function parseArgs(argv) {
  const args = { _: [] };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === '--json' || a === '--github-output' || a === '--soft') args[a.slice(2).replace(/-/g, '_')] = true;
    else if (a.startsWith('--')) { args[a.slice(2).replace(/-/g, '_')] = argv[i + 1]; i += 1; }
    else args._.push(a);
  }
  return args;
}

function emitGithubOutput(v) {
  const file = process.env.GITHUB_OUTPUT;
  if (!file) return;
  const fs = require('fs');
  const one = (s) => String(s == null ? '' : s).replace(/[\r\n]+/g, ' ').slice(0, ANNOTATION_MAX);
  fs.appendFileSync(file, [
    `killed=${v.allowed ? 'false' : 'true'}`,
    `state=${v.state}`,
    `reason=${one(v.reason)}`,
  ].join('\n') + '\n');
}

function emitAnnotation(v, soft) {
  if (process.env.GITHUB_ACTIONS !== 'true' || v.allowed) return;
  const one = String(v.reason).replace(/[\r\n]+/g, ' ').slice(0, ANNOTATION_MAX);
  const kind = soft ? 'warning' : 'error';
  console.log(`::${kind} title=ai-sdlc kill-switch::autonomy ${v.state === 'unreadable' ? 'unreadable → OFF (fail-closed)' : 'revoked'} — ${one}`);
}

async function main(argv) {
  const args = parseArgs(argv);
  const op = args._[0];
  if (op === 'check') {
    const v = await checkKillSwitch({ repo: args.repo, controlRepo: args.control_repo });
    if (args.json) console.log(JSON.stringify(v, null, 2));
    else console.log(`kill-switch: ${v.state} — ${v.reason}`);
    if (args.github_output) emitGithubOutput(v);
    emitAnnotation(v, args.soft);
    process.exit(v.allowed || args.soft ? 0 : 1);
  }
  if (op === 'engage' || op === 'release') {
    const res = await (op === 'engage' ? engage : release)({
      repo: args.repo, actor: args.actor, reason: args.reason,
    });
    console.log(JSON.stringify(res, null, 2));
    process.exit(0);
  }
  console.error(USAGE);
  process.exit(2);
}

if (require.main === module) {
  main(process.argv.slice(2)).catch((e) => {
    console.error(`kill-switch: ${(e && e.message) || e}`);
    process.exit(1);
  });
}

module.exports = {
  OFF_LABEL,
  envEngaged,
  checkKillSwitch,
  engage,
  release,
  readOffIssues,
};
