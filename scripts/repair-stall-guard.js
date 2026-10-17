#!/usr/bin/env node
// scripts/repair-stall-guard.js — repair-stall guard for the bounded
// ci-repair loop (issue #190).
//
// Port of the Hermes pr_merger `repairSigs` rule (hermes issue #199): a
// repair that keeps hitting the SAME failing-check signature is not making
// progress — the signature of what is red did not move. Count repair
// attempts per signature; once a signature has burned `maxStall` attempts,
// escalate to `needs-human` instead of spending another engine run that can
// only terminate `no_changes` or push a commit that leaves the identical
// checks red. `maxStall <= 0` disables the guard (PR_REPAIR_STALL_GUARD=0
// equivalent).
//
// Signature shape: sha256 over the sorted, deduped failing check names
// joined by '|' (Hermes failingCheckSignature), truncated to 16 hex chars —
// the same length as the content-based failure signature the repair prompt
// already records. Names are hashed (not embedded raw) so the value is safe
// inside the `<!-- ai-sdlc:ci-repair ... sig=... -->` comment marker that
// persists the per-signature count on the PR — the same trusted-author
// bookkeeping the attempt budget already uses.
//
// Trust boundary: fetched from the runtime repo pin (inputs.runtime_ref),
// never from the PR head — a change must not weaken the guard that bounds
// its own repair loop. Unavailable/invalid source degrades to
// guard-disabled with a warning (fail-open), never blocks a legitimate
// repair.
//
// Bounded: pure functions over the markers + failing-name inputs the guard
// step already collects — no engine, no extra API calls. `applyNeedsHuman`
// performs a fixed, small number of `gh` calls (label create + pr edit +
// optional comment).
//
// CLI (manual / other callers):
//   node scripts/repair-stall-guard.js --markers markers.json \
//        --failing failing.json --max-stall 3 --json
//   node scripts/repair-stall-guard.js --checks "build,test" --max-stall 1 \
//        --repo OWNER/NAME --pr 42 --apply
//
// Inputs:
//   --markers  FILE  JSON array of parsed attempt markers
//                    [{attempt, sha, sig}] — sig may be absent on markers
//                    written before this guard shipped
//   --failing  FILE  JSON array of failing checks [{name, ...}]
//                    (ci-repair failing.json shape)
//   --checks   CSV   alternative to --failing: failing check names
//   --max-stall N    attempts tolerated per signature (default 3; <=0 off)
// Outputs:
//   --json           print the result object to stdout
//   --github-output  append stall_sig/stall_count/stall_escalated to
//                    $GITHUB_OUTPUT
//   --repo/--pr/--apply  apply `needs-human` (+ optional --comment FILE)
//                    when the evaluation escalates
'use strict';

const fs = require('fs');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const DEFAULT_MAX_STALL = 3;
const DEFAULT_LABEL = 'needs-human';
const MARKER_PREFIX = 'ai-sdlc:ci-repair';
const SIG_LEN = 16;

// Failing-check signature (Hermes failingCheckSignature, issue #199 there):
// sorted, deduped check names joined by '|', sha256-truncated so it is
// bounded and marker-safe. Empty input → '' (never escalates).
function signatureFor(checkNames) {
  const names = [...new Set((checkNames || []).map((n) => String(n || '').trim()).filter(Boolean))].sort();
  if (!names.length) return '';
  return crypto.createHash('sha256').update(names.join('|')).digest('hex').slice(0, SIG_LEN);
}

// Build the marker value stamped on each attempt comment
// (`<!-- ai-sdlc:ci-repair attempt=N sha=… sig=… -->`).
function markerSignatureLine({ attempt, sha, signature }) {
  const sig = signature ? ` sig=${signature}` : '';
  return `<!-- ${MARKER_PREFIX} attempt=${attempt} sha=${sha}${sig} -->`;
}

// Core evaluation. `markers` are prior attempt markers already filtered to
// trusted authors by the caller (bots/collaborators — a forged comment must
// never feed the guard). A marker counts toward `signature` only when it
// carries that exact sig; legacy markers without sig are unattributable and
// count toward nothing (the flat max_repairs budget still bounds them).
// Returns {signature, count, maxStall, escalated, reason}.
function evaluateStall({ markers, checkNames, signature, maxStall } = {}) {
  const sig = signature !== undefined ? String(signature) : signatureFor(checkNames);
  const cap = Number.isFinite(Number(maxStall)) ? Number(maxStall) : DEFAULT_MAX_STALL;
  const list = Array.isArray(markers) ? markers : [];
  const count = sig ? list.filter((m) => m && m.sig === sig).length : 0;
  const enabled = cap > 0;
  const escalated = Boolean(enabled && sig && count >= cap);
  return {
    signature: sig,
    count,
    maxStall: cap,
    enabled,
    escalated,
    reason: escalated
      ? `repair-stall:${sig}`
      : !enabled
        ? 'stall-guard-disabled'
        : !sig
          ? 'no-failing-checks'
          : 'below-threshold',
  };
}

// --- GitHub application ------------------------------------------------------

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

// Applies the escalation label to the PR (label create is idempotent via
// --force; the pr edit no-ops when the label is already present). A comment
// with the escalation cause is posted when `comment` is provided.
function applyNeedsHuman({ repo, pr, label, comment, exec }) {
  const run = exec || defaultExec;
  const name = label || DEFAULT_LABEL;
  if (!repo || !pr) throw new Error('applyNeedsHuman requires repo and pr');
  try {
    run(['label', 'create', name, '-R', repo,
      '--color', 'D93F0B', '--description', 'AI-SDLC escalation: human action required', '--force']);
  } catch (e) {
    // Label exists and cannot be updated, or the app lacks label admin —
    // the pr edit below still applies the name.
  }
  run(['pr', 'edit', String(pr), '-R', repo, '--add-label', name]);
  let commented = false;
  if (comment) {
    run(['pr', 'comment', String(pr), '-R', repo, '--body', String(comment)]);
    commented = true;
  }
  return { label: name, applied: true, commented };
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: repair-stall-guard.js (--markers F) (--failing F|--checks csv)',
    '                             [--signature HEX] [--max-stall N] [--json]',
    '                             [--github-output]',
    '                             [--repo OWNER/NAME --pr N --apply]',
    '                             [--comment F]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = { json: false, apply: false, githubOutput: false, maxStall: DEFAULT_MAX_STALL, label: DEFAULT_LABEL };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--markers') opts.markers = next();
    else if (a === '--failing') opts.failing = next();
    else if (a === '--checks') opts.checks = next();
    else if (a === '--signature') opts.signature = next();
    else if (a === '--max-stall') opts.maxStall = Number(next());
    else if (a === '--repo') opts.repo = next();
    else if (a === '--pr') opts.pr = next();
    else if (a === '--apply') opts.apply = true;
    else if (a === '--label') opts.label = next();
    else if (a === '--comment') opts.comment = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { exec, stdout } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));

  let markers = [];
  if (opts.markers) markers = JSON.parse(fs.readFileSync(opts.markers, 'utf8') || '[]');

  let checkNames;
  if (opts.failing) {
    checkNames = (JSON.parse(fs.readFileSync(opts.failing, 'utf8') || '[]'))
      .map((c) => c && c.name).filter(Boolean);
  } else if (opts.checks !== undefined) {
    checkNames = String(opts.checks).split(',').map((s) => s.trim()).filter(Boolean);
  } else if (opts.signature === undefined) {
    throw new Error(`no input: pass --failing, --checks, or --signature\n${usage()}`);
  }

  const result = evaluateStall({
    markers, checkNames, signature: opts.signature, maxStall: opts.maxStall,
  });

  if (opts.apply && result.escalated) {
    if (!opts.repo || !opts.pr) throw new Error('--apply requires --repo and --pr');
    const comment = opts.comment
      ? fs.readFileSync(opts.comment, 'utf8')
      : `⚠️ **AI-SDLC ci-repair escalated** — the identical failing-check signature (\`${result.signature}\`) persisted across ${result.count} repair attempts; the loop is not making progress. Human review required; remove \`${opts.label || DEFAULT_LABEL}\` to hand control back.`;
    result.escalation = applyNeedsHuman({
      repo: opts.repo, pr: opts.pr, label: opts.label, comment, exec,
    });
  }

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      `stall_sig=${result.signature}`,
      `stall_count=${result.count}`,
      `stall_escalated=${result.escalated}`,
    ];
    fs.appendFileSync(process.env.GITHUB_OUTPUT, lines.join('\n') + '\n');
  }

  if (opts.json) emit(JSON.stringify(result));
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`repair-stall-guard: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  DEFAULT_MAX_STALL,
  DEFAULT_LABEL,
  MARKER_PREFIX,
  signatureFor,
  markerSignatureLine,
  evaluateStall,
  applyNeedsHuman,
  parseArgs,
  main,
};
