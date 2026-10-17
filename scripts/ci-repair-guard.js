#!/usr/bin/env node
// scripts/ci-repair-guard.js — guard + attempt-budget decision for the
// bounded ci-repair loop (issue #127)
//
// Extraction of the `Guard + attempt budget` heredoc from
// .github/workflows/ai-sdlc-ci-repair.yml. The decision is a pure function
// over the JSON inputs the step collects (pr.json, markers.json,
// check-runs.json, statuses.json); the repair-stall memory stays in the
// canonical scripts/repair-stall-guard.js — loaded from the trusted-pin
// fetch dir, absent → guard disabled (fail-open, the flat max_repairs
// budget still bounds the loop).
//
// Decision table:
//   pr closed/merged          → skip pr-<state>
//   draft                     → skip draft
//   skip label                → skip skip-label
//   fork head                 → skip fork-head
//   branch misses pattern     → skip non-pipeline-branch
//   no failing checks         → skip no-failing-checks
//   marker on this head       → skip head-already-attempted
//   stall signature burned    → escalate repair-stall:<sig>
//   attempts >= max_repairs   → escalate repairs-exhausted
//   else                      → repair failing-checks
//
// CLI:
//   node scripts/ci-repair-guard.js [--dir $WORK]
//        [--stall-guard PATH] [--json]
// Scalars come from env (same names the step sets):
//   REPO, BRANCH_PATTERN, MAX_REPAIRS, MAX_CHECKS, STALL_MAX
// Writes failing.json + pending.json into --dir; outputs append to
// $GITHUB_OUTPUT when set: decision, reason, attempt, attempts, head_ref,
// head_sha, base_ref, failing, stall_sig, stall_count.
'use strict';

const fs = require('fs');
const path = require('path');

const SKIP_LABELS = ['needs-human', 'hold', 'do-not-merge', 'wip'];
const RED = ['failure', 'timed_out', 'startup_failure'];

const readJson = (dir, f) => { try { return JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8') || '[]'); } catch (e) { return []; } };

// Latest check-run per name wins (re-runs append new runs); statuses are a
// supplementary red signal the App token may not be able to read.
function collectChecks({ runs, statuses }) {
  const latest = new Map();
  for (const r of runs) if (!latest.has(r.name) || r.id > latest.get(r.name).id) latest.set(r.name, r);
  const failing = [
    ...[...latest.values()].filter((r) => RED.includes(r.conclusion))
      .map((r) => ({ kind: 'check', id: String(r.id), name: r.name, conclusion: r.conclusion, app: r.app, url: r.url, output: r.output })),
    ...statuses.filter((s) => s.state === 'failure' || s.state === 'error')
      .map((s, i) => ({ kind: 'status', id: `status-${i}`, name: s.context, conclusion: s.state, app: '', url: s.url, output: { title: '', summary: s.description, text: '' } })),
  ];
  const pending = [...latest.values()].filter((r) => r.status !== 'completed').map((r) => r.name);
  return { failing, pending };
}

// Repair-stall guard (issue #190): canonical script from the trusted pin —
// absent/unreadable → guard disabled (fail-open), never blocks a repair.
function evalStall({ stallGuardPath, markers, failing, env, warn } = {}) {
  let stall = { signature: '', count: 0, escalated: false };
  if (stallGuardPath && fs.existsSync(stallGuardPath)) {
    try {
      stall = require(stallGuardPath).evaluateStall({
        markers, checkNames: failing.map((f) => f.name), maxStall: Number(env.STALL_MAX),
      });
    } catch (e) {
      (warn || console.log)(`::warning::stall guard eval failed (${e.message.split('\n')[0]}) — continuing without it`);
    }
  }
  return stall;
}

function evaluateGuard({ pr, markers, runs, statuses, env, stallGuardPath, warn } = {}) {
  const e = env || process.env;
  const [owner] = String(e.REPO || '/').split('/');
  const labels = (pr.labels || []).map((l) => l.name);
  const pattern = e.BRANCH_PATTERN;
  const max = Number(e.MAX_REPAIRS);
  const { failing, pending } = collectChecks({ runs, statuses });
  const attempts = markers.length;
  const stall = evalStall({ stallGuardPath, markers, failing, env: e, warn });

  let decision = 'skip', reason = '';
  if (pr.state !== 'OPEN') reason = `pr-${String(pr.state).toLowerCase()}`;
  else if (pr.isDraft) reason = 'draft';
  else if (labels.some((l) => SKIP_LABELS.includes(l))) reason = 'skip-label';
  else if (pr.isCrossRepository || (pr.headRepositoryOwner || {}).login !== owner) reason = 'fork-head';
  else if (pattern && !new RegExp(pattern).test(pr.headRefName)) reason = 'non-pipeline-branch';
  else if (!failing.length) reason = 'no-failing-checks';
  else if (markers.some((m) => m.sha === pr.headRefOid)) reason = 'head-already-attempted';
  else if (stall.escalated) { decision = 'escalate'; reason = `repair-stall:${stall.signature}`; }
  else if (attempts >= max) { decision = 'escalate'; reason = 'repairs-exhausted'; }
  else { decision = 'repair'; reason = 'failing-checks'; }

  const names = failing.map((f) => f.name).join(', ').replace(/[\r\n]/g, ' ').slice(0, 500);
  return {
    decision, reason, failing, pending, stall,
    outputs: {
      decision, reason,
      attempt: attempts + 1,
      attempts,
      head_ref: pr.headRefName,
      head_sha: pr.headRefOid,
      base_ref: pr.baseRefName,
      failing: names,
      stall_sig: stall.signature,
      stall_count: stall.count,
    },
  };
}

function parseArgs(argv) {
  const opts = { dir: undefined, json: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--dir') opts.dir = next();
    else if (a === '--stall-guard') opts.stallGuard = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--help' || a === '-h') { console.log('node scripts/ci-repair-guard.js [--dir D] [--stall-guard F] [--json]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const dir = opts.dir || e.WORK;
  if (!dir) throw new Error('no work dir: pass --dir or set $WORK');
  const maxChecks = Number(e.MAX_CHECKS);
  const r = evaluateGuard({
    pr: readJson(dir, 'pr.json'),
    markers: readJson(dir, 'markers.json'),
    runs: readJson(dir, 'check-runs.json'),
    statuses: readJson(dir, 'statuses.json'),
    env: e,
    stallGuardPath: opts.stallGuard || path.join(dir, 'scripts', 'repair-stall-guard.js'),
  });
  fs.writeFileSync(path.join(dir, 'failing.json'), JSON.stringify(r.failing.slice(0, maxChecks), null, 2));
  fs.writeFileSync(path.join(dir, 'pending.json'), JSON.stringify(r.pending));
  console.log(`decision: ${r.decision} (${r.reason}) attempts=${r.outputs.attempts}/${e.MAX_REPAIRS} stall=${r.stall.count}/${e.STALL_MAX} failing=[${r.outputs.failing}]`);
  if (e.GITHUB_OUTPUT) {
    fs.appendFileSync(e.GITHUB_OUTPUT,
      Object.entries(r.outputs).map(([k, v]) => `${k}=${v}`).join('\n') + '\n');
  }
  if (opts.json) console.log(JSON.stringify(r.outputs));
  return r;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`ci-repair-guard: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { SKIP_LABELS, RED, collectChecks, evalStall, evaluateGuard, parseArgs, main };
