#!/usr/bin/env node
// scripts/label-contradiction.js — issue-label contradiction repair (issue #200)
//
// Canonical GHA-native port of the Hermes janitor duty
// (issue_watcher.findLabelVeto / janitor.sweepLabelContradictions, ref
// Axel-DaMage/hermes#376): a forbidden label pair on an issue is a pipeline
// defect, never a legitimate state. The repair is veto + strip + log —
// the blocker always outranks the dispatch trigger
// (spec/labels.md → "Forbidden combinations" / "Drift rule").
//
// The effective forbidden set is computed from the resolved contract
// (.ai-sdlc.yaml):
//   - every declared labels.state_machine.veto_pairs {winner, loser}
//   - (s → T) for every s in triggers.skip_labels                    (F4)
//   - escalation.label → trigger and → the not-ready binding        (F1/F3)
//   - not-ready binding → trigger                                   (F2)
//   - queued/in-progress/pr-opened bindings → trigger               (F5)
// Declared pairs win first in evaluation order; derived pairs follow.
// Deliberately legal co-occurrences (hold + in-flight run, trigger +
// terminal run-state) are simply absent from the set — never repaired.
//
// Bounded: one issue list/view call plus, only when a contradiction is
// found, one comments-page call + one edit + one comment. Drift that
// survives --max-repairs audits is escalated to escalation.label with
// cause `label-drift-loop` instead of fought silently (spec/labels.md).
//
// CLI:
//   node scripts/label-contradiction.js --repo OWNER/NAME --sweep [--apply]
//   node scripts/label-contradiction.js --repo OWNER/NAME --issue N [--apply]
//   node scripts/label-contradiction.js --labels a,b,c [--json]   (eval only)
//
// Options:
//   --repo OWNER/NAME   target repo (required for live reads/mutations)
//   --issue N           single issue
//   --sweep             enumerate all open issues (bounded by --max)
//   --labels a,b,c      label set to evaluate instead of fetching
//   --contract FILE     resolved contract; .json parsed directly,
//                       .yaml/.yml converted via `yq -o=json`
//                       (default: .ai-sdlc.yaml when present, else defaults)
//   --apply             perform the repair (default: report only)
//   --max N             sweep cap, hard ceiling 1000 (default 200)
//   --max-repairs N     tolerated prior repairs per issue before the
//                       label-drift-loop escalation (default 2)
//   --json              print the result object to stdout
//   --github-output     append scanned/repaired counters to $GITHUB_OUTPUT
'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');
const { execFileSync } = require('child_process');

const MARKER = '<!-- ai-sdlc:label-veto -->';
const DRIFT_CAUSE = 'label-drift-loop';
const DEFAULT_MAX_ISSUES = 200;
const HARD_MAX_ISSUES = 1000;
const DEFAULT_MAX_REPAIRS = 2;

// Canonical bindings used when no contract resolves a name
// (spec/labels.md → States; matches the runtime defaults in load-config).
const DEFAULTS = {
  trigger: 'ready-to-implement',
  hold: 'needs-human',
  notReady: 'needs-refinement',
  runStates: ['scc:queued', 'scc:in-progress', 'scc:pr-opened'],
  skipLabels: [
    'epic', 'wontfix', 'invalid', 'duplicate', 'question',
    'dependencies', 'umbrella', 'scc:child',
  ],
};

// --- Contract → forbidden set --------------------------------------------------

// Resolves the contract's label bindings to concrete names, falling back to
// the canonical names in DEFAULTS (spec/labels.md → States).
function resolveBindings(contract) {
  const c = contract && typeof contract === 'object' ? contract : {};
  const labels = c.labels && typeof c.labels === 'object' ? c.labels : {};
  return {
    trigger: (c.triggers && c.triggers.issue_label) || DEFAULTS.trigger,
    hold: (c.escalation && c.escalation.label) || DEFAULTS.hold,
    notReady: labels['not-ready'] || DEFAULTS.notReady,
    runStates: ['queued', 'in-progress', 'pr-opened']
      .map((k, i) => labels[k] || DEFAULTS.runStates[i])
      .filter(Boolean),
    skipLabels: Array.isArray(c.triggers && c.triggers.skip_labels)
      ? c.triggers.skip_labels
      : DEFAULTS.skipLabels,
    declaredPairs: (labels.state_machine && Array.isArray(labels.state_machine.veto_pairs))
      ? labels.state_machine.veto_pairs
      : [],
  };
}

// Builds the effective forbidden {winner, loser} set:
// declared veto_pairs first, then the derived F-rules. All comparisons are
// lowercase — label names are bindings, not constants (spec/labels.md), so
// the resolved contract is the source of truth and nothing is hardcoded
// beyond the canonical-name fallbacks in DEFAULTS.
function forbiddenPairs(contract) {
  const b = resolveBindings(contract);
  const pairs = [];
  const push = (winner, loser) => {
    const w = String(winner || '').toLowerCase();
    const l = String(loser || '').toLowerCase();
    if (!w || !l || w === l) return;
    if (!pairs.some((p) => p.winner === w && p.loser === l)) pairs.push({ winner: w, loser: l });
  };
  for (const p of b.declaredPairs) push(p.winner, p.loser);
  push(b.hold, b.trigger);              // F1
  push(b.notReady, b.trigger);          // F2
  push(b.hold, b.notReady);             // F3
  for (const s of b.skipLabels) push(s, b.trigger); // F4
  for (const q of b.runStates) push(q, b.trigger);  // F5
  return pairs;
}

// --- Pure evaluation ------------------------------------------------------------

// Returns every forbidden pair currently violated by the label set.
// `labelNames` may be strings or {name} objects (gh --json labels shape).
function findContradictions(labelNames, pairs) {
  const set = new Set(
    (Array.isArray(labelNames) ? labelNames : [])
      .map((l) => String(typeof l === 'string' ? l : (l && l.name) || '').toLowerCase())
      .filter(Boolean),
  );
  return (pairs || []).filter((p) => set.has(p.winner) && set.has(p.loser));
}

// Distills violated pairs into a repair plan:
//   remove  — every losing label of a violated pair (a winner elsewhere is
//             still stripped when it loses to a higher-precedence blocker,
//             e.g. needs-refinement losing to needs-human — F3)
//   vetoes  — the surviving winners, i.e. winners not themselves losing to
//             anything present; these are named in the audit comment
// `labels` are the actual label names on the issue (case preserved).
function planRepairs(labelNames, pairs) {
  const actual = new Map();
  for (const l of Array.isArray(labelNames) ? labelNames : []) {
    const name = String(typeof l === 'string' ? l : (l && l.name) || '');
    if (name) actual.set(name.toLowerCase(), name);
  }
  const violated = findContradictions([...actual.keys()], pairs);
  const loserSet = new Set(violated.map((p) => p.loser));
  const remove = [...loserSet].map((l) => actual.get(l) || l);
  const vetoes = [];
  for (const p of violated) {
    if (!loserSet.has(p.winner) && !vetoes.includes(p.winner)) vetoes.push(p.winner);
  }
  return { violated, remove, vetoes };
}

// --- Audit comment ---------------------------------------------------------------

function auditBody({ removed, vetoes, escalated, now }) {
  const ts = now || new Date().toISOString();
  const lines = [
    `⚖️ **AI-SDLC label-contradiction repair** — ${ts}`,
    '',
    'A forbidden label combination was repaired (spec/labels.md → Forbidden combinations):',
    '',
    `- removed: ${removed.map((l) => `\`${l}\``).join(', ')}`,
    `- vetoed by: ${vetoes.map((l) => `\`${l}\``).join(', ')}`,
    '',
    'Blocker labels always outrank the dispatch trigger. Resolve or remove',
    `${vetoes.map((l) => `\`${l}\``).join('/')} first, then re-apply the trigger.`,
  ];
  if (escalated) {
    lines.push(
      '',
      `This forbidden pair reappeared after prior repairs — applied \`needs-human\``,
      `(cause: \`${DRIFT_CAUSE}\`) instead of fighting silently.`,
    );
  }
  lines.push('', MARKER);
  return `${lines.join('\n')}\n`;
}

// --- GitHub operations -------------------------------------------------------------

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function fetchIssueLabels(repo, issue, exec) {
  const run = exec || defaultExec;
  const doc = JSON.parse(run(['issue', 'view', String(issue), '-R', repo, '--json', 'number,labels']) || '{}');
  return (doc.labels || []).map((l) => l.name);
}

function listOpenIssues(repo, max, exec) {
  const run = exec || defaultExec;
  const limit = Math.min(Math.max(1, Number(max) || DEFAULT_MAX_ISSUES), HARD_MAX_ISSUES);
  const doc = JSON.parse(
    run(['issue', 'list', '-R', repo, '--state', 'open', '--limit', String(limit), '--json', 'number,labels']) || '[]',
  );
  return Array.isArray(doc) ? doc : [];
}

// Prior repairs are counted from marker comments — the issue thread is the
// durable ledger in a GHA world (no local state survives between runs).
function countPriorRepairs(repo, issue, exec) {
  const run = exec || defaultExec;
  let doc;
  try {
    doc = JSON.parse(
      run(['api', `repos/${repo}/issues/${issue}/comments?per_page=100`, '--jq', '[.[].body]']) || '[]',
    );
  } catch (e) {
    return 0; // unreadable thread must never block a needed repair
  }
  return (Array.isArray(doc) ? doc : []).filter((b) => String(b).includes(MARKER)).length;
}

// Repairs one issue when its labels violate the forbidden set. Label-only:
// never dispatches, never picks a winner outside the precedence pairs.
// `holdLabel` is the resolved escalation.label binding used by the
// label-drift-loop escalation. Returns
// {issue, contradictions, removed, vetoes, repaired, escalated, error}.
function repairIssue({ repo, issue, labels, pairs, apply, exec, now, maxRepairs, holdLabel }) {
  const run = exec || defaultExec;
  const labelList = Array.isArray(labels) ? labels : fetchIssueLabels(repo, issue, exec);
  const plan = planRepairs(labelList, pairs);
  const hold = String(holdLabel || DEFAULTS.hold).toLowerCase();
  const result = {
    issue, contradictions: plan.violated.length, removed: plan.remove,
    vetoes: plan.vetoes, repaired: false, escalated: false,
  };
  if (!plan.violated.length) return result;

  if (!apply) {
    result.dry = true;
    return result;
  }

  const prior = countPriorRepairs(repo, issue, run);
  const cap = Number.isInteger(maxRepairs) ? maxRepairs : DEFAULT_MAX_REPAIRS;
  result.escalated = prior >= cap;

  // Best-effort: the escalation label may not exist on a fresh consumer repo.
  if (result.escalated && !labelList.some((l) => l.toLowerCase() === hold)) {
    try {
      run(['label', 'create', hold, '-R', repo, '--color', 'B60205',
        '--description', `AI-SDLC: human hold (${DRIFT_CAUSE})`, '--force']);
    } catch (e) { /* label exists or no admin — the edit below still applies it */ }
  }

  const args = ['issue', 'edit', String(issue), '-R', repo];
  for (const l of plan.remove) args.push('--remove-label', l);
  if (result.escalated && !labelList.some((l) => l.toLowerCase() === hold)) {
    args.push('--add-label', hold);
  }
  try {
    run(args);
  } catch (e) {
    result.error = `label strip failed: ${String(e.message || e).split('\n')[0]}`;
    return result;
  }

  const body = auditBody({ removed: plan.remove, vetoes: plan.vetoes, escalated: result.escalated, now });
  const tmp = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-veto-')), 'comment.md');
  try {
    fs.writeFileSync(tmp, body);
    run(['issue', 'comment', String(issue), '-R', repo, '--body-file', tmp]);
  } catch (e) {
    // The label repair already landed — a lost comment must not undo it.
    result.commentError = String(e.message || e).split('\n')[0];
  } finally {
    try { fs.unlinkSync(tmp); } catch (e) { /* tmp cleanup is best-effort */ }
  }
  result.repaired = true;
  return result;
}

// Sweeps open issues of a repo; repairs each contradiction found.
function sweepRepo({ repo, pairs, max, apply, exec, now, maxRepairs, holdLabel }) {
  const run = exec || defaultExec;
  const issues = listOpenIssues(repo, max, run);
  const repairs = [];
  for (const i of issues) {
    const r = repairIssue({
      repo, issue: i.number, labels: (i.labels || []).map((l) => l.name),
      pairs, apply, exec: run, now, maxRepairs, holdLabel,
    });
    if (r.contradictions || r.error) repairs.push(r);
  }
  return {
    repo,
    scanned: issues.length,
    contradictions: repairs.length,
    repaired: repairs.filter((r) => r.repaired).length,
    repairs,
  };
}

// --- Contract loading --------------------------------------------------------------

function loadContract(file, exec) {
  if (!file) {
    for (const cand of ['.ai-sdlc.yaml', '.ai-sdlc.yml']) {
      if (fs.existsSync(cand)) return loadContract(cand, exec);
    }
    return {}; // no contract → canonical-name defaults
  }
  const text = fs.readFileSync(file, 'utf8');
  if (/\.json$/i.test(file)) return JSON.parse(text);
  // YAML → JSON via yq (preinstalled on ubuntu-latest; the sweep's cfg
  // step already depends on it). Tests pass .json fixtures.
  const run = exec || ((argv) => execFileSync(argv[0], argv.slice(1), { encoding: 'utf8' }));
  try {
    return JSON.parse(run(['yq', '-o=json', '.', file]));
  } catch (e) {
    throw new Error(`contract ${file}: YAML→JSON conversion failed (is yq installed? pass a resolved contract.json via --contract) — ${String(e.message || e).split('\n')[0]}`);
  }
}

// --- CLI ----------------------------------------------------------------------------

function usage() {
  return [
    'usage: label-contradiction.js (--repo OWNER/NAME --issue N | --repo OWNER/NAME --sweep | --labels a,b,c)',
    '                              [--contract FILE] [--apply] [--max N] [--max-repairs N]',
    '                              [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = { apply: false, json: false, githubOutput: false, max: DEFAULT_MAX_ISSUES, maxRepairs: DEFAULT_MAX_REPAIRS };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--issue') opts.issue = next();
    else if (a === '--sweep') opts.sweep = true;
    else if (a === '--labels') opts.labels = next();
    else if (a === '--contract') opts.contract = next();
    else if (a === '--apply') opts.apply = true;
    else if (a === '--max') opts.max = Number(next());
    else if (a === '--max-repairs') opts.maxRepairs = Number(next());
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { exec, stdout, now } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  if (opts.labels === undefined && !opts.issue && !opts.sweep) {
    throw new Error(`nothing to do: pass --issue, --sweep, or --labels\n${usage()}`);
  }
  const contract = loadContract(opts.contract, exec);
  const pairs = forbiddenPairs(contract);
  const holdLabel = resolveBindings(contract).hold;
  let result;

  if (opts.labels !== undefined) {
    // Pure evaluation mode — no GitHub calls.
    const labelList = String(opts.labels).split(',').map((s) => s.trim()).filter(Boolean);
    const plan = planRepairs(labelList, pairs);
    result = {
      labels: labelList, contradictions: plan.violated.length,
      remove: plan.remove, vetoes: plan.vetoes,
    };
  } else if (opts.issue) {
    if (!opts.repo) throw new Error('--issue requires --repo');
    result = repairIssue({
      repo: opts.repo, issue: Number(opts.issue),
      labels: undefined, pairs, apply: opts.apply, exec, now,
      maxRepairs: opts.maxRepairs, holdLabel,
    });
  } else if (opts.sweep) {
    if (!opts.repo) throw new Error('--sweep requires --repo');
    result = sweepRepo({
      repo: opts.repo, pairs, max: opts.max, apply: opts.apply, exec, now,
      maxRepairs: opts.maxRepairs, holdLabel,
    });
  }

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      `issues_scanned=${result.scanned ?? (result.issue !== undefined ? 1 : 0)}`,
      `contradictions=${result.contradictions ?? 0}`,
      `repaired=${typeof result.repaired === 'number' ? result.repaired : (result.repaired ? 1 : 0)}`,
    ];
    fs.appendFileSync(process.env.GITHUB_OUTPUT, `${lines.join('\n')}\n`);
  }

  emit(JSON.stringify(result));
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`label-contradiction: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  MARKER,
  DRIFT_CAUSE,
  DEFAULTS,
  resolveBindings,
  forbiddenPairs,
  findContradictions,
  planRepairs,
  auditBody,
  countPriorRepairs,
  fetchIssueLabels,
  listOpenIssues,
  repairIssue,
  sweepRepo,
  loadContract,
  main,
};
