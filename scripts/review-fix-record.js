#!/usr/bin/env node
// scripts/review-fix-record.js — fix-attempt manifest + PR comment
// (issue #127)
//
// Extraction of the `Record attempt on PR` heredoc from
// .github/workflows/ai-sdlc-review-fix.yml: builds the per-attempt
// manifest (artifact evidence) and the outcome marker comment
// (`<!-- ai-sdlc:review-fix-outcome attempt=N sha=… -->`).
//
// Outcome mapping:
//   sanitize blocked → injection-quarantined
//   engine exit 0    → PUSH_OUTCOME | commit-failed
//   engine exit 10   → no-changes
//   else             → engine-exit-<code>
//
// CLI:
//   node scripts/review-fix-record.js [--dir $WORK] [--comment-out FILE]
//        [--manifest-out FILE] [--json]
// Scalars come from env (same names the step sets):
//   PR, ATTEMPT, MAX_FIX, HEAD_SHA, ENGINE_CODE, PUSH_OUTCOME, COMMIT,
//   SANITIZE, RUN_URL
// Output appends to $GITHUB_OUTPUT when set: outcome.
'use strict';

const fs = require('fs');
const path = require('path');

function outcomeFor(e) {
  const code = e.ENGINE_CODE || 'not-run';
  let outcome;
  if (e.SANITIZE === 'blocked') outcome = 'injection-quarantined';
  else if (code === '0') outcome = e.PUSH_OUTCOME || 'commit-failed';
  else if (code === '10') outcome = 'no-changes';
  else outcome = `engine-exit-${code}`;
  return { outcome, code };
}

function buildManifest({ e, outcome, code }) {
  return {
    pr: Number(e.PR), attempt: Number(e.ATTEMPT), maxFix: Number(e.MAX_FIX),
    consumedSha: e.HEAD_SHA, commit: e.COMMIT || null,
    engineExitCode: code, outcome, run: e.RUN_URL,
  };
}

function buildComment({ e, outcome }) {
  return [
    `<!-- ai-sdlc:review-fix-outcome attempt=${e.ATTEMPT} sha=${e.HEAD_SHA} -->`,
    `🛠️ **AI-SDLC review-fix — attempt ${e.ATTEMPT}/${e.MAX_FIX}** finished`,
    '',
    `- outcome: \`${outcome}\`${e.COMMIT ? ` → ${e.COMMIT}` : ''}`,
    `- consumed head: \`${String(e.HEAD_SHA).slice(0, 8)}\``,
    `- run: ${e.RUN_URL} (prompt, audit log + manifest in artifacts)`,
    '',
    outcome === 'pushed' ? '_The push re-triggers `pull_request: synchronize` → the new head is re-reviewed._' : '',
  ].filter((l) => l !== '').join('\n') + '\n';
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
    else if (a === '--help' || a === '-h') { console.log('node scripts/review-fix-record.js [--dir D] [--comment-out F] [--manifest-out F] [--json]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const W = opts.dir || e.WORK;
  if (!W) throw new Error('no work dir: pass --dir or set $WORK');
  const { outcome, code } = outcomeFor(e);
  const manifest = buildManifest({ e, outcome, code });
  fs.writeFileSync(opts.manifestOut || path.join(W, 'review-fix-manifest.json'), JSON.stringify(manifest, null, 2));
  const comment = buildComment({ e, outcome });
  fs.writeFileSync(opts.commentOut || path.join(W, 'attempt-comment.md'), comment);
  if (e.GITHUB_OUTPUT) fs.appendFileSync(e.GITHUB_OUTPUT, `outcome=${outcome}\n`);
  console.log(`review-fix attempt ${e.ATTEMPT}/${e.MAX_FIX}: ${outcome}`);
  if (opts.json) console.log(JSON.stringify({ outcome, manifest }));
  return { outcome, manifest, comment };
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`review-fix-record: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { outcomeFor, buildManifest, buildComment, parseArgs, main };
