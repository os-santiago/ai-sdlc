#!/usr/bin/env node
// scripts/main-guard.js — main-guard sentinel (issue #189)
//
// Ports the Hermes pr_merger MAIN_GUARD block (issues #386/#423) to the
// GHA runtime: evaluate the newest *completed* run of each allowlisted
// gate workflow on the base branch — union semantics: 'red' when ANY
// gate's freshest in-window run failed (failure|cancelled|timed_out),
// 'green' when every gate is green-or-unknown, and when ALL gates lack a
// fresh signal the verdict falls back to the newest completed run across
// ALL workflows in-window; absent/stale → 'unknown' (fail-open — a stale
// signal never latches, never stalls merges).
//
// Durable state is the sentinel issue itself: a red verdict files (once,
// deduplicated by open-title search) a postmortem issue carrying
// `<!-- ai-sdlc:main-guard -->`; that open issue is the halt latch. A
// green verdict while the latch is held closes the issue with the clear
// notice — comments fire only on the red↔green transition, never on a
// repeated verdict (the flap-guard from Hermes issue #423).
//
// Trust boundary: this file must only ever execute from a trusted ref
// (the runtime repo pin). A PR must never supply the sentinel that gates
// its own merge.
//
// CLI:
//   node scripts/main-guard.js --repo OWNER/NAME [--base main] [--pr N]
//       [--gate-workflows ci,build] [--max-age-hours 72]
//       [--disabled] [--apply] [--json] [--github-output]
//
// Mutations (issue file/close, PR comment) require --apply; without it the
// verdict and gate action are still computed (dry-run). `MAIN_GUARD=0`
// in the environment is an ops kill-switch equivalent to --disabled.
'use strict';

const fs = require('fs');
const { execFileSync } = require('child_process');

const DEFAULT_WORKFLOWS = 'ci';
const DEFAULT_MAX_AGE_H = 72;
const RED_CONCLUSIONS = new Set(['failure', 'cancelled', 'timed_out']);
const RUN_FIELDS = 'conclusion,status,workflowName,headSha,databaseId,updatedAt,createdAt';
// The dedup contract with spec/postmortem-lifecycle.md: one open sentinel
// issue per repo, found by title search.
const TITLE_SEARCH = 'main red in:title';
const ISSUE_MARKER = '<!-- ai-sdlc:main-guard -->';
const PR_MARK_RE = /<!-- ai-sdlc:main-guard-halt issue=(\d+) -->/;
const ISSUE_LABELS = ['postmortem', 'ai-sdlc', 'priority:P1'];
const RUN_LIST_LIMIT = 30;

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function parseCsvList(s) {
  return String(s || '').split(',').map((x) => x.trim().toLowerCase()).filter(Boolean);
}

function isSafeRef(ref) {
  return /^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$/.test(String(ref || ''));
}

// --- Health verdict -----------------------------------------------------------

// Newest completed run per allowlisted gate workflow, inside the recency
// window. Union semantics; all-unknown falls back to the newest completed
// run across ALL workflows in-window; absent/stale → 'unknown' (fail-open).
function checkMainHealth(repo, base, opts = {}) {
  const run = opts.exec || defaultExec;
  const now = opts.now !== undefined ? opts.now : Date.now();
  const workflows = (opts.workflows && opts.workflows.length ? opts.workflows : parseCsvList(DEFAULT_WORKFLOWS))
    .map((w) => String(w).toLowerCase()).filter(Boolean);
  const maxAgeH = opts.maxAgeH !== undefined ? Number(opts.maxAgeH) : DEFAULT_MAX_AGE_H;
  const maxAgeMs = maxAgeH > 0 ? maxAgeH * 3600000 : Infinity;
  if (!isSafeRef(base)) return { status: 'unknown', error: 'unsafe base ref name' };
  let runs;
  try {
    runs = JSON.parse(run([
      'run', 'list', '--repo', repo, '--branch', base,
      '--limit', String(RUN_LIST_LIMIT), '--json', RUN_FIELDS,
    ]) || '[]') || [];
  } catch (e) {
    return { status: 'unknown', error: String(e.message || e).split('\n')[0] };
  }
  // A completed run counts only inside the recency window — weeks-old runs
  // of dormant workflows are history, not current signal. Undated runs
  // (stubbed responses) count as current rather than silently discarding
  // the only signal. Pending runs never count.
  const inWindow = (r) => {
    const ts = Date.parse(r.updatedAt || r.createdAt || '');
    return !Number.isFinite(ts) || (now - ts) <= maxAgeMs;
  };
  const verdictOf = (r) =>
    (RED_CONCLUSIONS.has(String(r.conclusion || '').toLowerCase()) ? 'red' : 'green');
  const completed = runs.filter((r) => r.status === 'completed');

  // `gh run list` returns newest first — first match per gate is freshest.
  const gates = {};
  for (const wf of workflows) {
    const hit = completed.find((r) => String(r.workflowName || '').toLowerCase() === wf);
    gates[wf] = hit && inWindow(hit)
      ? { verdict: verdictOf(hit), run: hit }
      : { verdict: 'unknown', stale: !!hit };
  }
  const gateVerdicts = Object.values(gates).map((g) => g.verdict);
  if (gateVerdicts.includes('red')) {
    return { status: 'red', run: Object.values(gates).find((g) => g.verdict === 'red').run, gates };
  }
  if (gateVerdicts.includes('green')) {
    return { status: 'green', run: Object.values(gates).find((g) => g.verdict === 'green').run, gates };
  }

  // Every gate unknown — fall back to the newest completed run across ALL
  // workflows, still inside the window; absent/stale → 'unknown'.
  const latest = completed.find(inWindow);
  if (latest) return { status: verdictOf(latest), run: latest, gates, fallback: true };
  return { status: 'unknown', gates, fallback: true };
}

// --- Sentinel issue (durable latch) -------------------------------------------

// The open postmortem issue is the persisted halt state — survives across
// runs with no artifact store. Deduped by the canonical title search; a
// strict client-side title filter keeps fuzzy search matches from
// suppressing a genuine filing.
function findSentinelIssue(repo, exec) {
  const run = exec || defaultExec;
  const out = run([
    'issue', 'list', '--repo', repo, '--state', 'open',
    '--search', TITLE_SEARCH, '--json', 'number,title', '--limit', '5',
  ]);
  const open = JSON.parse(out || '[]') || [];
  const hit = open.find((i) => /main red/i.test(i.title || '')) || open[0];
  return hit ? hit.number : null;
}

function sentinelTitle(repo) {
  return `fix(ci): main red — sentinel halt on ${repo}`;
}

function filePostmortem(repo, health, exec) {
  const run = exec || defaultExec;
  const wf = (health.run && health.run.workflowName) || 'ci';
  const runId = health.run && health.run.databaseId;
  const body = [
    'Auto-filed by the AI-SDLC main-guard sentinel (issue #189).',
    '',
    `Newest completed run of a gate workflow on the base branch is failing (\`${wf}\`). Auto-merge is halted for this repo until the base is green again — merges on a red base pile breakage.`,
    ...(runId ? ['', `Failing run: https://github.com/${repo}/actions/runs/${runId}`] : []),
    '',
    'The guard clears automatically and closes this issue when the base recovers.',
    '',
    ISSUE_MARKER,
  ].join('\n');
  let out;
  try {
    out = run(['issue', 'create', '--repo', repo, '--title', sentinelTitle(repo), '--body', body,
      ...ISSUE_LABELS.flatMap((l) => ['--label', l])]);
  } catch (e) {
    // Missing labels must not block the filing — retry bare.
    out = run(['issue', 'create', '--repo', repo, '--title', sentinelTitle(repo), '--body', body]);
  }
  const m = String(out).match(/issues\/(\d+)/);
  return { filed: true, issue: m ? parseInt(m[1], 10) : null };
}

function closeSentinelIssue(repo, issue, exec) {
  const run = exec || defaultExec;
  run(['issue', 'close', String(issue), '--repo', repo,
    '--comment', 'main-guard cleared: base branch is green again — auto-merge resumed (issue #189 sentinel).']);
  return { closed: true, issue };
}

// One halt comment per (PR, sentinel issue) — a fresh halt episode (new
// issue number) comments again; a repeated verdict on the same episode
// never does (transition-only, Hermes #423 flap-guard).
function commentHeldPr(repo, pr, issue, health, exec) {
  if (!issue) return { skipped: 'no-issue' };
  const run = exec || defaultExec;
  const out = run(['api', '--paginate', `repos/${repo}/issues/${pr}/comments?per_page=100`, '--jq', '.[] | .body']);
  if (String(out).includes(`<!-- ai-sdlc:main-guard-halt issue=${issue} -->`)) return { skipped: 'dedup' };
  const wf = (health.run && health.run.workflowName) || 'gate workflow';
  const body = [
    `🛑 **AI-SDLC main-guard halt** — base branch \`main\` is red (\`${wf}\`); auto-merge is paused until the base is green again. Postmortem: #${issue}.`,
    `<!-- ai-sdlc:main-guard-halt issue=${issue} -->`,
  ].join('\n');
  run(['pr', 'comment', String(pr), '--repo', repo, '--body', body]);
  return { commented: true };
}

// --- Sentinel decision ---------------------------------------------------------

// Returns {action: 'disabled'|'halt'|'cleared'|'ok', verdict, issue,
// workflow, reason}. 'halt' holds the merge; 'cleared' marks the
// red→green transition (latch released, issue closed); 'ok' lets the gate
// proceed — including on 'unknown' (fail-open). Mutations need apply=true.
function mainGuardCheck(opts = {}) {
  const { repo, base = 'main', pr = null } = opts;
  const exec = opts.exec || defaultExec;
  const enabled = opts.enabled !== false && process.env.MAIN_GUARD !== '0';
  if (!enabled) return { action: 'disabled', verdict: null, reason: 'main-guard disabled' };
  const now = opts.now !== undefined ? opts.now : Date.now();
  const health = (opts.checkMainHealth || checkMainHealth)(repo, base, {
    exec, now, workflows: opts.workflows, maxAgeH: opts.maxAgeH,
  });
  const res = {
    action: 'ok',
    verdict: health.status,
    workflow: (health.run && health.run.workflowName) || '',
    issue: null,
    reason: '',
  };

  // Fail-open: a stale or absent signal never latches and never stalls
  // merges; a held issue latch is left for the next green verdict to close.
  if (health.status === 'unknown') {
    res.reason = `no fresh gate signal${health.fallback ? ' (fallback exhausted)' : ''} — fail-open`;
    if (health.error) res.error = health.error;
    return res;
  }

  let issue = null;
  let issueErr = null;
  try {
    issue = findSentinelIssue(repo, exec);
  } catch (e) {
    issueErr = String(e.message || e).split('\n')[0];
  }

  if (health.status === 'red') {
    res.action = 'halt';
    res.issue = issue;
    res.reason = `base red (${res.workflow || 'gate workflow'})`;
    if (opts.apply) {
      if (issue) {
        res.dedup = 'sentinel-issue-open';
      } else if (issueErr) {
        res.filed = false;
        res.fileError = issueErr; // search failed — never blind-file a dupe
      } else {
        try {
          const f = filePostmortem(repo, health, exec);
          res.filed = f.filed;
          res.issue = f.issue;
        } catch (e) {
          res.filed = false;
          res.fileError = String(e.message || e).split('\n')[0];
        }
      }
      if (pr) {
        try {
          res.prComment = commentHeldPr(repo, pr, res.issue, health, exec);
        } catch (e) {
          res.prComment = { skipped: 'failed', error: String(e.message || e).split('\n')[0] };
        }
      }
    }
    return res;
  }

  // green: a held latch releases — the close comment is the transition
  // notice; steady green without a latch is a silent ok.
  if (issue) {
    res.action = 'cleared';
    res.issue = issue;
    res.reason = 'base green — halt cleared';
    if (opts.apply) {
      try {
        closeSentinelIssue(repo, issue, exec);
        res.closed = true;
      } catch (e) {
        res.closed = false;
        res.closeError = String(e.message || e).split('\n')[0];
      }
    }
    return res;
  }
  res.reason = 'base green';
  return res;
}

// --- CLI ----------------------------------------------------------------------

function usage() {
  return [
    'usage: main-guard.js --repo OWNER/NAME [--base main] [--pr N]',
    '                     [--gate-workflows ci,build] [--max-age-hours 72]',
    '                     [--disabled] [--apply] [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = {
    base: 'main', gateWorkflows: DEFAULT_WORKFLOWS, maxAgeHours: DEFAULT_MAX_AGE_H,
    enabled: true, apply: false, json: false, githubOutput: false,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--base') opts.base = next();
    else if (a === '--pr') opts.pr = next();
    else if (a === '--gate-workflows') opts.gateWorkflows = next();
    else if (a === '--max-age-hours') opts.maxAgeHours = Number(next());
    else if (a === '--enabled') opts.enabled = true;
    else if (a === '--disabled') opts.enabled = false;
    else if (a === '--apply') opts.apply = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  if (!opts.repo) throw new Error(`--repo is required\n${usage()}`);
  return opts;
}

function main(argv, { exec, stdout } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  const result = mainGuardCheck({
    repo: opts.repo,
    base: opts.base,
    pr: opts.pr ? Number(opts.pr) : null,
    workflows: parseCsvList(opts.gateWorkflows),
    maxAgeH: opts.maxAgeHours,
    enabled: opts.enabled,
    apply: opts.apply,
    exec,
  });

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      `main_guard=${result.action === 'disabled' ? 'ok' : result.action}`,
      `main_guard_verdict=${result.verdict || ''}`,
      `main_guard_issue=${result.issue || ''}`,
      `main_guard_workflow=${result.workflow || ''}`,
      `main_guard_reason=${String(result.reason || '').replace(/\n/g, ' ')}`,
    ];
    fs.appendFileSync(process.env.GITHUB_OUTPUT, lines.join('\n') + '\n');
  }

  if (opts.json) emit(JSON.stringify(result));
  else emit(`main-guard: ${result.action} (${result.reason || result.verdict || 'n/a'})`);
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`main-guard: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  DEFAULT_WORKFLOWS,
  DEFAULT_MAX_AGE_H,
  RED_CONCLUSIONS,
  TITLE_SEARCH,
  ISSUE_MARKER,
  PR_MARK_RE,
  ISSUE_LABELS,
  parseCsvList,
  checkMainHealth,
  findSentinelIssue,
  sentinelTitle,
  filePostmortem,
  closeSentinelIssue,
  commentHeldPr,
  mainGuardCheck,
  main,
};
