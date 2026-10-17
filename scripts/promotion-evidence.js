#!/usr/bin/env node
// scripts/promotion-evidence.js — trust-ladder promotion gate (issue #201)
//
// Canonical implementation of the promotion-evidence check in
// spec/trust-ladder.md: evaluates recent merge history plus escalation
// records and decides whether the promotion thresholds declared in
// .ai-sdlc.yaml (`promotion.clean_merges_threshold`, `promotion.window`)
// are satisfied — N consecutive clean merges with zero escalations inside
// the rolling window. Part of epic #151 (trust machinery); the promotion
// contract-PR generator (issue #211) consumes this verdict.
//
// A merge is "clean" when it carries no escalation label and every
// recorded check passed. Unknown check state is NOT clean — evidence is
// fail-closed: unverifiable merges never promote.
//
// Window semantics: the rolling window is the last `window` merged PRs
// (newest first, as `gh pr list --state merged` returns them). The merged
// PR is the observable unit of a completed pipeline run; run-ledger
// escalations can be folded in via `--escalations FILE` (already
// window-scoped by the caller) or per-record `escalated: true`.
//
// CLI:
//   node scripts/promotion-evidence.js --repo OWNER/NAME [--limit N]
//        [--window N] [--threshold N] [--escalation-label L ...] [--json]
//   node scripts/promotion-evidence.js --merges FILE [--window N] ...
//
// Inputs (one of):
//   --repo OWNER/NAME    fetch merged-PR history via `gh` (bounded: --limit)
//   --merges FILE        JSON array of merge records — ledger replay / tests.
//                        Shape: [{number, mergedAt, labels: [..|{name}],
//                        checksPassed?, statusCheckRollup?, escalated?}]
// Options:
//   --window N           merges examined, newest first (default 30 —
//                        schema `promotion.window`; spec floor is 10)
//   --threshold N        consecutive clean merges required from the most
//                        recent merge backwards (default 3 — schema
//                        `promotion.clean_merges_threshold`; <=0 reduces
//                        the gate to "zero escalations in a non-empty
//                        window")
//   --escalation-label L label marking an escalated merge; repeatable
//                        (default: needs-human — `escalation.label` binding)
//   --escalations FILE   JSON array of escalation records already scoped
//                        to the window — any entry fails the gate
//   --limit N            merged PRs fetched via gh (default: --window,
//                        hard cap 200)
//   --json               print the full evidence object (default output)
//   --github-output      append satisfied/consecutive_clean/escalations/
//                        examined to $GITHUB_OUTPUT
//
// Exit: 0 = promotion gate satisfied, 1 = not satisfied, 2 = usage/error.
'use strict';

const fs = require('fs');
const { execFileSync } = require('child_process');

const DEFAULT_WINDOW = 30; // schema default; spec floor for promotion is 10
const DEFAULT_THRESHOLD = 3;
const DEFAULT_ESCALATION_LABELS = ['needs-human'];
const MAX_FETCH_LIMIT = 200;

// Check conclusions that count as "passed" for both GraphQL check runs
// (`conclusion`) and commit-status contexts (`state`). Everything else —
// FAILURE, PENDING, IN_PROGRESS, EXPECTED, null — fails closed.
const PASSING_CHECK_STATES = new Set(['SUCCESS', 'NEUTRAL', 'SKIPPED']);

// --- Record normalization ----------------------------------------------------

function labelNames(record) {
  const labels = Array.isArray(record && record.labels) ? record.labels : [];
  return labels
    .map((l) => (typeof l === 'string' ? l : l && l.name))
    .filter((n) => typeof n === 'string' && n.length > 0);
}

// `gh pr list --json statusCheckRollup` rows: check runs carry
// {status, conclusion}; status contexts carry {state}. A record with NO
// check entries is unverifiable, not clean (fail-closed).
function rollupPassed(entries) {
  const list = Array.isArray(entries) ? entries : [];
  if (!list.length) return false;
  for (const c of list) {
    if (!c || typeof c !== 'object') return false;
    const verdict = String(c.conclusion || c.state || c.status || '').toUpperCase();
    if (!PASSING_CHECK_STATES.has(verdict)) return false;
  }
  return true;
}

// Normalizes one merge record to {number, mergedAt, labels, escalated,
// checksPassed}. `escalated` may be asserted explicitly (run-ledger
// escalation folded onto the merge) or derived from the label bindings.
// `checksPassed` honors an explicit boolean first, then a check rollup;
// neither present → false (unverifiable → not clean).
function normalizeRecord(record, escalationLabels) {
  const r = record && typeof record === 'object' ? record : {};
  const esc = escalationLabels && escalationLabels.length
    ? escalationLabels
    : DEFAULT_ESCALATION_LABELS;
  const labels = labelNames(r);
  const escalated = r.escalated === true || labels.some((l) => esc.includes(l));

  let checksPassed;
  if (typeof r.checksPassed === 'boolean') {
    checksPassed = r.checksPassed;
  } else if (r.statusCheckRollup !== undefined || r.checks !== undefined) {
    checksPassed = rollupPassed(r.statusCheckRollup !== undefined ? r.statusCheckRollup : r.checks);
  } else {
    checksPassed = false;
  }

  return {
    number: r.number ?? null,
    mergedAt: r.mergedAt || r.merged_at || null,
    labels,
    escalated,
    checksPassed,
  };
}

// --- Evidence evaluation -----------------------------------------------------

// records: newest-first merge records. Returns
//   {satisfied, window, threshold, examined, consecutiveClean, escalations,
//    escalationLabels, reasons}
// Gate (spec/trust-ladder.md + contract `promotion:` block):
//   - window must be non-empty — no evidence, no promotion;
//   - zero escalation-labeled (or escalation-record) entries in the window;
//   - the streak of clean merges counting back from the newest entry must
//     reach `threshold` (threshold <= 0 vacates the streak requirement).
function evaluatePromotionEvidence(records, opts = {}) {
  const window = Number.isInteger(opts.window) && opts.window > 0 ? opts.window : DEFAULT_WINDOW;
  const threshold = Number.isInteger(opts.threshold) ? opts.threshold : DEFAULT_THRESHOLD;
  const escalationLabels = Array.isArray(opts.escalationLabels) && opts.escalationLabels.length
    ? opts.escalationLabels
    : DEFAULT_ESCALATION_LABELS;

  const list = Array.isArray(records) ? records.slice(0, window) : [];
  const windowed = list.map((r) => normalizeRecord(r, escalationLabels));

  const extra = Array.isArray(opts.escalations) ? opts.escalations : [];
  const escalations = [
    ...windowed.filter((r) => r.escalated).map((r) => ({
      number: r.number, labels: r.labels.filter((l) => escalationLabels.includes(l)),
    })),
    ...extra.map((e) => ({
      number: e && (e.number ?? e.issue ?? e.pr) !== undefined ? (e.number ?? e.issue ?? e.pr) : null,
      labels: labelNames(e).filter((l) => escalationLabels.includes(l)),
    })),
  ];

  let consecutiveClean = 0;
  for (const r of windowed) {
    if (r.checksPassed && !r.escalated) consecutiveClean += 1;
    else break;
  }

  const reasons = [];
  if (!windowed.length) reasons.push('empty window — no merge evidence to promote on');
  if (escalations.length) {
    reasons.push(
      `${escalations.length} escalation record(s) inside the window` +
      ` (${escalations.map((e) => `#${e.number}`).join(', ')})`,
    );
  }
  if (windowed.length && consecutiveClean < Math.max(0, threshold)) {
    reasons.push(
      `only ${consecutiveClean} consecutive clean merge(s); threshold requires ${threshold}`,
    );
  }

  const satisfied = windowed.length > 0
    && escalations.length === 0
    && consecutiveClean >= Math.max(0, threshold);

  return {
    satisfied,
    window,
    threshold,
    examined: windowed.length,
    consecutiveClean,
    cleanMerges: windowed.filter((r) => r.checksPassed && !r.escalated).length,
    escalations,
    escalationLabels,
    reasons,
  };
}

// --- GitHub fetch ------------------------------------------------------------

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

// Fetches the `limit` most recently merged PRs. `statusCheckRollup` is the
// preferred check source; on older `gh` versions (or restricted tokens)
// that field can fail — degrade to labels-only, after which unknown check
// state fails closed (records are not clean).
function fetchMergedPrs(repo, limit, exec) {
  const run = exec || defaultExec;
  const n = Math.min(Math.max(1, Number(limit) > 0 ? Number(limit) : DEFAULT_WINDOW), MAX_FETCH_LIMIT);
  const base = ['pr', 'list', '-R', repo, '--state', 'merged', '--limit', String(n), '--json'];
  let out;
  try {
    out = run([...base, 'number,title,mergedAt,labels,statusCheckRollup']);
  } catch (e) {
    out = run([...base, 'number,title,mergedAt,labels']);
  }
  const doc = JSON.parse(String(out || '[]'));
  return Array.isArray(doc) ? doc : [];
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: promotion-evidence.js (--repo OWNER/NAME | --merges FILE)',
    '         [--limit N] [--window N] [--threshold N]',
    '         [--escalation-label L ...] [--escalations FILE]',
    '         [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = {
    window: DEFAULT_WINDOW,
    threshold: DEFAULT_THRESHOLD,
    escalationLabels: [],
    json: false,
    githubOutput: false,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--merges') opts.merges = next();
    else if (a === '--escalations') opts.escalationsFile = next();
    else if (a === '--window') opts.window = Number(next());
    else if (a === '--threshold') opts.threshold = Number(next());
    else if (a === '--limit') opts.limit = Number(next());
    else if (a === '--escalation-label') opts.escalationLabels.push(next());
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

  let records;
  if (opts.merges) {
    const doc = JSON.parse(fs.readFileSync(opts.merges, 'utf8'));
    if (!Array.isArray(doc)) throw new Error('--merges file must contain a JSON array');
    records = doc;
  } else if (opts.repo) {
    records = fetchMergedPrs(opts.repo, opts.limit || opts.window, exec);
  } else {
    throw new Error(`no input: pass --repo or --merges\n${usage()}`);
  }

  let extraEscalations = [];
  if (opts.escalationsFile) {
    const doc = JSON.parse(fs.readFileSync(opts.escalationsFile, 'utf8'));
    if (!Array.isArray(doc)) throw new Error('--escalations file must contain a JSON array');
    extraEscalations = doc;
  }

  const result = evaluatePromotionEvidence(records, {
    window: opts.window,
    threshold: opts.threshold,
    escalationLabels: opts.escalationLabels.length ? opts.escalationLabels : undefined,
    escalations: extraEscalations,
  });

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      `promotion_satisfied=${result.satisfied}`,
      `promotion_consecutive_clean=${result.consecutiveClean}`,
      `promotion_escalations=${result.escalations.length}`,
      `promotion_examined=${result.examined}`,
    ];
    fs.appendFileSync(process.env.GITHUB_OUTPUT, lines.join('\n') + '\n');
  }

  emit(JSON.stringify(result));
  return result;
}

if (require.main === module) {
  try {
    const result = main(process.argv.slice(2));
    process.exit(result.satisfied ? 0 : 1);
  } catch (e) {
    console.error(`promotion-evidence: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  DEFAULT_WINDOW,
  DEFAULT_THRESHOLD,
  DEFAULT_ESCALATION_LABELS,
  MAX_FETCH_LIMIT,
  labelNames,
  rollupPassed,
  normalizeRecord,
  evaluatePromotionEvidence,
  fetchMergedPrs,
  parseArgs,
  main,
};
