#!/usr/bin/env node
// scripts/ci-repair-record.js — attempt manifest + PR comment for the
// bounded ci-repair loop (issue #127)
//
// Extraction of the `Record attempt on PR` heredoc from
// .github/workflows/ai-sdlc-ci-repair.yml. Builds the per-attempt manifest
// (artifact evidence) and the trusted-author marker comment
// (`<!-- ai-sdlc:ci-repair attempt=N sha=… sig=… -->`) the guard counts on
// the next poll.
//
// Outcome mapping:
//   base_fault (scc:config-error on base) → base-contract-error
//   engine exit 0                        → PUSH_OUTCOME | commit-failed
//   engine exit 10                       → no-changes
//   else                                 → engine-exit-<code>
//
// CLI:
//   node scripts/ci-repair-record.js [--dir $WORK] [--comment-out FILE]
//        [--manifest-out FILE] [--json]
// Scalars come from env (same names the step sets):
//   PR, ATTEMPT, MAX_REPAIRS, HEAD_SHA, STALL_SIG, FAILING, BASE_FAULT,
//   ENGINE_CODE, PUSH_OUTCOME, COMMIT, RUN_URL
// Output appends to $GITHUB_OUTPUT when set: outcome.
'use strict';

const fs = require('fs');
const path = require('path');

const readJson = (dir, f, fb) => { try { return JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8')); } catch (e) { return fb; } };

// Outcome for the attempt ledger — base faults short-circuit before the
// engine runs (issue #255), exit 10 is the engine's no-mutations code.
function outcomeFor(e) {
  const code = e.ENGINE_CODE || 'not-run';
  const baseFault = e.BASE_FAULT === 'true';
  let outcome;
  if (baseFault) outcome = 'base-contract-error';
  else if (code === '0') outcome = e.PUSH_OUTCOME || 'commit-failed';
  else if (code === '10') outcome = 'no-changes';
  else outcome = `engine-exit-${code}`;
  return { outcome, code, baseFault };
}

function buildManifest({ e, failure, engine, stall, runbook, outcome, code, baseFault }) {
  return {
    pr: Number(e.PR), attempt: Number(e.ATTEMPT), maxRepairs: Number(e.MAX_REPAIRS),
    headSha: e.HEAD_SHA, commit: e.COMMIT || null, engineExitCode: code, outcome,
    failure: {
      signature: failure.signature, rootCause: failure.rootCause,
      baseFault: failure.baseFault || false, baseFaultChecks: failure.baseFaultChecks || [],
      runbookHit: runbook.hit ? runbook.entry : null,
    },
    failingChecks: failure.checks, engineToolCalls: engine.tool_calls_total ?? null, engineIterations: engine.iterations ?? null,
    // Engine-stall classification (issue #158): the engine run's own
    // failure class + terminal route, when the classifier was available at
    // the runtime pin.
    engineStall: stall ? { class: stall.class, route: stall.route, terminal: stall.terminal, reason: stall.reason } : null,
    run: e.RUN_URL,
  };
}

function buildComment({ e, failure, stall, runbook, outcome, baseFault }) {
  const lines = [
    `<!-- ai-sdlc:ci-repair attempt=${e.ATTEMPT} sha=${e.HEAD_SHA}${e.STALL_SIG ? ` sig=${e.STALL_SIG}` : ''} -->`,
    `🔧 **AI-SDLC ci-repair — attempt ${e.ATTEMPT}/${e.MAX_REPAIRS}**`,
    '',
    `- failing: \`${e.FAILING || 'n/a'}\``,
    `- outcome: \`${outcome}\`${e.COMMIT ? ` → ${e.COMMIT}` : ''}`,
    `- failure signature: \`${failure.signature || 'n/a'}\``,
    `- run: ${e.RUN_URL} (prompt, audit log + manifest in artifacts)`,
  ];
  if (runbook.hit && runbook.entry) {
    lines.push(`- runbook: \`${runbook.entry}\` — prior fix injected into the repair brief`);
  }
  if (stall) lines.push(`- engine stall class: \`${stall.class}\` → \`${stall.route}\` (${stall.terminal})`);
  if (baseFault) lines.push('- diagnosis: `scc:config-error` in the failing check — the contract failed validation on the **base** ref (every stage resolves `.ai-sdlc.yaml` from base, never this head), so no push here can green the run. Escalated without spending an engine run.');
  return lines.join('\n') + '\n';
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
    else if (a === '--comment-out') opts.commentOut = next();
    else if (a === '--manifest-out') opts.manifestOut = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--help' || a === '-h') { console.log('node scripts/ci-repair-record.js [--dir D] [--comment-out F] [--manifest-out F] [--json]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const W = opts.dir || e.WORK;
  if (!W) throw new Error('no work dir: pass --dir or set $WORK');
  const failure = readJson(W, 'failure.json', { signature: null, rootCause: null, checks: [] });
  const engine = readJson(W, 'scc-manifest.json', {});
  const stall = readJson(W, 'stall.json', null);
  const runbook = readJson(W, 'runbook.json', {});
  const { outcome, code, baseFault } = outcomeFor(e);
  const manifest = buildManifest({ e, failure, engine, stall, runbook, outcome, code, baseFault });
  fs.writeFileSync(opts.manifestOut || path.join(W, 'ci-repair-manifest.json'), JSON.stringify(manifest, null, 2));
  const comment = buildComment({ e, failure, stall, runbook, outcome, baseFault });
  fs.writeFileSync(opts.commentOut || path.join(W, 'attempt-comment.md'), comment);
  if (e.GITHUB_OUTPUT) fs.appendFileSync(e.GITHUB_OUTPUT, `outcome=${outcome}\n`);
  console.log(`attempt ${e.ATTEMPT}/${e.MAX_REPAIRS}: ${outcome}`);
  if (opts.json) console.log(JSON.stringify({ outcome, manifest }));
  return { outcome, manifest, comment };
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`ci-repair-record: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { outcomeFor, buildManifest, buildComment, parseArgs, main };
