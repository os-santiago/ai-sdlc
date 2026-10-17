#!/usr/bin/env node
// scripts/dor-check.js — Definition-of-Ready gate R1–R6 (issue #127)
//
// Extraction of the `Validate DoR` heredoc from
// .github/workflows/ai-sdlc-intake.yml — the intake dispatch gate was
// untestable inside YAML. Pure function over the issue record + label
// config; the workflow step collects issue.json and sets the env knobs.
//
// Rules (spec/definition-of-ready.md):
//   R1 conventional title        feat|fix|docs|chore|refactor|perf|test|
//                                build|ci|security(scope)?: description
//   R2 acceptance-criteria section present (## Acceptance criteria|AC)
//   R3 at least one AC list item
//   R4 non-trivial body (>= 120 chars)
//   R5 no skip-label (contract skip labels + not-ready binding)
//   R6 no TBD/TODO inside the AC section
// Unarmed issues (no trigger label) short-circuit to dispatch=false
// without evaluating — the refinement evidence rides along either way.
//
// CLI:
//   node scripts/dor-check.js [--issue issue.json] [--json]
//
// Scalars come from env (same names the workflow step sets):
//   TRIGGER, SKIP_LABELS (JSON array), NOT_READY, REFINE_STATUS, REFINE_FP
// Outputs append to $GITHUB_OUTPUT when set:
//   dispatch, armed, failed, dor_sha, refine, refine_fp
'use strict';

const crypto = require('crypto');
const fs = require('fs');

const TITLE_RE = /^(feat|fix|docs|chore|refactor|perf|test|build|ci|security)(\(.+\))?: .{5,}/;
const AC_RE = /##\s*(acceptance criteria|ac)\b/i;
const AC_ITEM_RE = /^\s*(?:-|\*|\d+\.)\s+/gm;
const TBD_RE = /\bTBD\b|\bTODO\b/i;
const MIN_BODY = 120;

// Core DoR evaluation. `env` defaults to process.env for test injection.
// Returns {dispatch, armed, failed, checks, refine, refine_fp}; `failed`
// is the '|'-joined list of failed rule names ('' when all pass).
function evaluateDor({ issue, env } = {}) {
  const e = env || process.env;
  const i = issue || {};
  const labels = (i.labels || []).map((l) => (l.name || l).toLowerCase());
  const trigger = String(e.TRIGGER || '').toLowerCase();
  const skip = [...JSON.parse(e.SKIP_LABELS || '[]'), e.NOT_READY]
    .filter(Boolean).map((l) => l.toLowerCase());
  const refine = e.REFINE_STATUS || 'skipped';
  const refineFp = e.REFINE_FP || '';

  if (!labels.includes(trigger)) {
    return { dispatch: 'false', armed: 'false', failed: '', dor_sha: '', checks: null, refine, refine_fp: refineFp, trigger };
  }
  const body = i.body || '';
  const acSec = AC_RE.test(body) ? body.split(/##\s*(?:acceptance criteria|ac)\b/i)[1] || '' : '';
  const acItems = (acSec.match(AC_ITEM_RE) || []).length;
  const checks = {
    'R1 conventional title': TITLE_RE.test(i.title),
    'R2 AC section present': acSec.trim().length > 0,
    'R3 ≥1 AC item': acItems >= 1,
    'R4 non-trivial body': body.trim().length >= MIN_BODY,
    'R5 no skip-label': !labels.some((l) => skip.includes(l)),
    'R6 no TBD in AC': !TBD_RE.test(acSec),
  };
  const failed = Object.entries(checks).filter(([, ok]) => !ok).map(([k]) => k);
  return {
    dispatch: failed.length ? 'false' : 'true',
    armed: 'true',
    failed: failed.join('|'),
    // DoR surface pin (issue #122): sha256 of the exact title+body this
    // verdict evaluated. The outcome step stamps it on the issue as a
    // trusted-author marker (`<!-- ai-sdlc:dor sha256=… -->`); implement
    // re-hashes its dispatch-time fetch and holds the run on drift, so a
    // body edited after approval can never reach the engine prompt.
    // Recipe pinned by scripts/comment-markers.js — keep byte-identical
    // to the verify side in scripts/implement-prompt.js.
    dor_sha: crypto.createHash('sha256').update(`${i.title || ''}\n${i.body || ''}`).digest('hex'),
    checks, refine, refine_fp: refineFp, trigger,
  };
}

function parseArgs(argv) {
  const opts = { issue: 'issue.json', json: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--issue') opts.issue = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--help' || a === '-h') { console.log('node scripts/dor-check.js [--issue F] [--json]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const issue = JSON.parse(fs.readFileSync(opts.issue, 'utf8'));
  const r = evaluateDor({ issue, env: e });
  if (r.armed !== 'true') {
    console.log(`issue does not carry the trigger label "${e.TRIGGER}" — not armed, skipping`);
  } else {
    console.log('DoR:', JSON.stringify(r.checks, null, 0));
  }
  if (e.GITHUB_OUTPUT) {
    fs.appendFileSync(e.GITHUB_OUTPUT, [
      `dispatch=${r.dispatch}`,
      `armed=${r.armed}`,
      `failed=${r.failed}`,
      `dor_sha=${r.dor_sha}`,
      // Refinement evidence rides the same ledger record (issue #197).
      `refine=${r.refine}`,
      `refine_fp=${r.refine_fp}`,
    ].join('\n') + '\n');
  }
  if (opts.json) console.log(JSON.stringify(r));
  return r;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`dor-check: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { evaluateDor, parseArgs, main, TITLE_RE, AC_RE, TBD_RE, MIN_BODY };
