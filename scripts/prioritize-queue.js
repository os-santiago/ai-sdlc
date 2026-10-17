#!/usr/bin/env node
// scripts/prioritize-queue.js — value-scored ready queue (issue #162, epic #147)
//
// The dispatch trigger (`triggers.issue_label`, default `ready-to-implement`)
// is event-driven FIFO by label application — high-value issues wait behind
// trivia. This stage scores every armed issue with the rubric declared in
// the repo contract (`prioritize.*` in .ai-sdlc.yaml) and emits an ordered
// dispatch queue as JSON + markdown evidence.
//
// Queue membership (spec/labels.md): an issue is armed when it carries the
// trigger label and NO label that vetoes the trigger — the effective veto
// set is the same forbidden-pair derivation the label-repair janitor uses
// (scripts/label-contradiction.js): declared labels.state_machine.veto_pairs
// ∪ triggers.skip_labels ∪ the precedence rules (hold > refinement-hold >
// run-state > trigger). A vetoed-but-armed issue is drift — excluded here,
// repaired by label-contradiction.js, never ranked.
//
// Scoring rubric (all weights per-repo in `prioritize.weights`):
//   score = priority[tier]                                 (priority:P* label)
//         + age_per_day × min(age_days, age_cap_days)      (waiting-time decay)
//         + epic_linked   × [epic/umbrella child]          (epic momentum)
//         + blocking      × #(open issues this blocks)     (unblocks others)
//         + blocked       × #(open issues blocking this)   (default negative)
//
// Ordering: with `prioritize.priority_override` (default true) the queue is
// sorted tier-first — priority labels override raw score and P0 always
// dispatches first; within a tier, score desc then age asc. Without the
// override ordering is pure score-first (tier as tiebreak). Deterministic:
// equal inputs produce byte-identical output (`--now` pins generatedAt).
//
// Dependency data comes from the issue-list payload (`blockedBy`/`blocking`
// connections — GitHub issue dependencies); when the fields are absent the
// components score 0 and `deps: false` marks the evidence (fail-open).
//
// Trust boundary: read-only stage — no label/comment mutations ever. This
// file must only execute from a trusted ref (the runtime pin or a vendored
// copy), never from a PR head.
//
// CLI:
//   node scripts/prioritize-queue.js --repo OWNER/NAME [--contract FILE]
//       [--max N] [--format json|markdown|both] [--out PREFIX]
//       [--disabled] [--now ISO] [--json] [--github-output]
//   node scripts/prioritize-queue.js --issues FILE ...   (offline replay —
//       FILE is the same JSON shape as `gh issue list --json …`)
//
// Mutations: none. `AI_SDLC_PRIORITIZE=0` is an ops kill-switch equivalent
// to --disabled.
'use strict';

const fs = require('fs');
const { execFileSync } = require('child_process');
const { forbiddenPairs, resolveBindings, loadContract } = require('./label-contradiction');

const VERSION = 1;
const DEFAULT_MAX = 50;
const HARD_MAX = 200;
const PRIORITY_RE = /^priority:P(\d+)$/i;
// Epic/umbrella linkage: the scc:child binding or the decomposer's parent
// marker in the body (`parent:#147`, `<!-- hermes:epic-child … -->`).
const EPIC_CHILD_LABEL = 'scc:child';
const EPIC_BODY_RE = /parent:#\d+|epic-child/i;

const DEFAULT_WEIGHTS = {
  priority: { P0: 1000, P1: 300, P2: 100, P3: 25 },
  unlabeled: 0,
  age_per_day: 2,
  age_cap_days: 30,
  epic_linked: 25,
  blocking: 15,
  blocked: -20,
};

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

// --- Rubric resolution ----------------------------------------------------------

// Resolves `prioritize.*` from the contract into a concrete rubric. Missing
// keys fall back to DEFAULT_WEIGHTS; the stage defaults to DISABLED when the
// contract doesn't declare the block (opt-in — spec additions never change
// behavior for undeclared repos).
function resolveRubric(contract) {
  const c = contract && typeof contract === 'object' ? contract : {};
  const p = c.prioritize && typeof c.prioritize === 'object' ? c.prioritize : {};
  const w = p.weights && typeof p.weights === 'object' ? p.weights : {};
  const num = (v, d) => (Number.isFinite(Number(v)) ? Number(v) : d);
  const priority = { ...DEFAULT_WEIGHTS.priority };
  if (w.priority && typeof w.priority === 'object') {
    for (const [k, v] of Object.entries(w.priority)) {
      const key = String(k).toUpperCase();
      if (/^P\d+$/.test(key) && Number.isFinite(Number(v))) priority[key] = Number(v);
    }
  }
  return {
    enabled: p.enabled !== undefined ? Boolean(p.enabled) : false,
    priorityOverride: p.priority_override !== undefined
      ? Boolean(p.priority_override) : true,
    max: Math.min(HARD_MAX, Math.max(1, Math.trunc(num(p.max, DEFAULT_MAX)))),
    epicChildLabel: String(p.epic_child_label || EPIC_CHILD_LABEL).toLowerCase(),
    weights: {
      priority,
      unlabeled: num(w.unlabeled, DEFAULT_WEIGHTS.unlabeled),
      age_per_day: num(w.age_per_day, DEFAULT_WEIGHTS.age_per_day),
      age_cap_days: Math.max(0, Math.trunc(num(w.age_cap_days, DEFAULT_WEIGHTS.age_cap_days))),
      epic_linked: num(w.epic_linked, DEFAULT_WEIGHTS.epic_linked),
      blocking: num(w.blocking, DEFAULT_WEIGHTS.blocking),
      blocked: num(w.blocked, DEFAULT_WEIGHTS.blocked),
    },
  };
}

// --- Queue membership -------------------------------------------------------------

// Labels that veto the dispatch trigger: every forbidden pair whose loser is
// the trigger, resolved from the contract (labels are bindings — spec/labels.md).
function vetoLabels(contract) {
  const trigger = resolveBindings(contract).trigger.toLowerCase();
  return new Set(
    forbiddenPairs(contract)
      .filter((pr) => pr.loser === trigger)
      .map((pr) => pr.winner),
  );
}

// Armed = carries the trigger AND carries no veto label. Returns
// {armed, vetoes[]} — vetoes are the observed veto labels (drift evidence).
function armedStatus(issue, contract) {
  const trigger = resolveBindings(contract).trigger.toLowerCase();
  const vetoes = vetoLabels(contract);
  const names = (issue.labels || []).map((l) => String(l.name || l).toLowerCase());
  if (!names.includes(trigger)) return { armed: false, vetoes: [] };
  return { armed: true, vetoes: names.filter((n) => vetoes.has(n)) };
}

// --- Scoring ----------------------------------------------------------------------

// Lowest priority tier on the issue (`priority:P0` → 0); null when unlabeled.
function priorityTier(issue) {
  let best = null;
  for (const l of issue.labels || []) {
    const m = PRIORITY_RE.exec(String(l.name || l));
    if (m && (best === null || Number(m[1]) < best)) best = Number(m[1]);
  }
  return best;
}

function isEpicLinked(issue, rubric) {
  const names = (issue.labels || []).map((l) => String(l.name || l).toLowerCase());
  if (names.includes(rubric.epicChildLabel)) return true;
  return EPIC_BODY_RE.test(String(issue.body || ''));
}

function openNodes(conn) {
  const nodes = (conn && Array.isArray(conn.nodes)) ? conn.nodes : [];
  return nodes.filter((n) => String(n.state || '').toUpperCase() !== 'CLOSED');
}

// Scores one armed issue under the rubric. Returns the score plus the
// per-component rationale — `parts` is the auditable evidence trail.
function scoreIssue(issue, rubric, now) {
  const w = rubric.weights;
  const tier = priorityTier(issue);
  const priorityWeight = tier === null
    ? w.unlabeled
    : (w.priority[`P${tier}`] !== undefined ? w.priority[`P${tier}`] : w.unlabeled);

  const created = Date.parse(issue.createdAt || '');
  const ageDays = Number.isFinite(created)
    ? Math.max(0, Math.floor((now - created) / 86400000))
    : 0;
  const cappedAge = Math.min(ageDays, w.age_cap_days);

  const epicLinked = isEpicLinked(issue, rubric);
  const blockingCount = openNodes(issue.blocking).length;
  const blockedCount = openNodes(issue.blockedBy).length;

  const parts = [
    `priority:${tier === null ? 'none' : `P${tier}`}=${priorityWeight}`,
    `age=${cappedAge}d×${w.age_per_day}=${cappedAge * w.age_per_day}`,
  ];
  if (epicLinked) parts.push(`epic_linked=${w.epic_linked}`);
  if (blockingCount) parts.push(`blocking(${blockingCount})=${blockingCount * w.blocking}`);
  if (blockedCount) parts.push(`blocked(${blockedCount})=${blockedCount * w.blocked}`);

  const score = priorityWeight + cappedAge * w.age_per_day
    + (epicLinked ? w.epic_linked : 0)
    + blockingCount * w.blocking
    + blockedCount * w.blocked;

  return {
    number: issue.number,
    title: issue.title || '',
    url: issue.url || '',
    score,
    priority: tier === null ? null : `P${tier}`,
    tier: tier === null ? Number.MAX_SAFE_INTEGER : tier,
    ageDays,
    epicLinked,
    blocking: blockingCount,
    blockedBy: blockedCount,
    labels: (issue.labels || []).map((l) => String(l.name || l)),
    reason: parts.join(' + '),
  };
}

// Ranks the armed set. priority_override → tier asc, score desc, age asc,
// number asc (P0 always first); otherwise score-first with tier as tiebreak.
function rankQueue(scored, rubric) {
  const arr = scored.slice();
  const cmp = rubric.priorityOverride
    ? (a, b) => (a.tier - b.tier) || (b.score - a.score)
    : (a, b) => (b.score - a.score) || (a.tier - b.tier);
  arr.sort((a, b) => cmp(a, b)
    || (a.ageDays - b.ageDays)
    || (a.number - b.number));
  arr.forEach((e, i) => { e.rank = i + 1; });
  return arr;
}

// --- Fetch / collect ---------------------------------------------------------------

// One bounded list call: open issues carrying the trigger label.
function listArmedIssues(repo, trigger, max, exec) {
  const run = exec || defaultExec;
  const out = run([
    'issue', 'list', '-R', repo, '--state', 'open',
    '--label', trigger, '--limit', String(max),
    '--json', 'number,title,url,labels,createdAt,body,blockedBy,blocking',
  ]);
  return JSON.parse(out || '[]');
}

// Full pass: fetch (or replay) → membership filter → score → rank.
function buildQueue({ repo, issues, contract, rubric, max, now }) {
  const trigger = resolveBindings(contract).trigger;
  const armed = [];
  const excluded = [];
  for (const i of issues.slice(0, Math.min(max, rubric.max))) {
    const st = armedStatus(i, contract);
    if (!st.armed) continue; // defensive — the list call already filtered
    if (st.vetoes.length) {
      excluded.push({ number: i.number, vetoes: st.vetoes });
      continue;
    }
    armed.push(scoreIssue(i, rubric, now));
  }
  const queue = rankQueue(armed, rubric);
  const deps = issues.some((i) => i.blockedBy !== undefined || i.blocking !== undefined);
  return {
    version: VERSION,
    stage: 'prioritize',
    repo: repo || null,
    generatedAt: new Date(now).toISOString(),
    trigger,
    rubric: {
      enabled: rubric.enabled,
      priority_override: rubric.priorityOverride,
      max: rubric.max,
      weights: rubric.weights,
    },
    deps,
    stats: { scanned: issues.length, armed: queue.length, excluded: excluded.length },
    queue,
    excluded,
  };
}

// --- Rendering ---------------------------------------------------------------------

function toMarkdown(result) {
  const lines = [
    '### ai-sdlc prioritize — scored ready queue',
    '',
    `repo: \`${result.repo}\` · trigger: \`${result.trigger}\` · generated: ${result.generatedAt}`,
    `scanned: ${result.stats.scanned} · armed: ${result.stats.armed}` +
      ` · excluded (vetoed): ${result.stats.excluded}` +
      (result.deps ? '' : ' · dependency data unavailable (scored 0)'),
    '',
  ];
  if (!result.queue.length) {
    lines.push('_queue empty — no armed issues_');
  } else {
    lines.push('| rank | issue | score | priority | age (d) | epic | blocking | blocked | rationale |');
    lines.push('|---:|---|---:|:--:|---:|:--:|---:|---:|---|');
    for (const e of result.queue) {
      const title = String(e.title).replace(/\|/g, '\\|').slice(0, 80);
      lines.push(
        `| ${e.rank} | #${e.number} ${title} | ${e.score} | ${e.priority || '—'}` +
        ` | ${e.ageDays} | ${e.epicLinked ? 'yes' : '—'} | ${e.blocking || '—'}` +
        ` | ${e.blockedBy || '—'} | ${e.reason} |`,
      );
    }
  }
  if (result.excluded.length) {
    lines.push('', '**Excluded (armed but vetoed — label-repair owns the drift):**');
    for (const x of result.excluded) {
      lines.push(`- #${x.number}: vetoed by \`${x.vetoes.join('`, `')}\``);
    }
  }
  return `${lines.join('\n')}\n`;
}

// --- CLI ----------------------------------------------------------------------------

function usage() {
  return [
    'usage: prioritize-queue.js (--repo OWNER/NAME | --issues FILE)',
    '                           [--contract FILE] [--max N] [--disabled]',
    '                           [--format json|markdown|both] [--out PREFIX]',
    '                           [--now ISO] [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = {
    format: 'both', json: false, githubOutput: false,
    disabled: false, max: DEFAULT_MAX,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--issues') opts.issues = next();
    else if (a === '--contract') opts.contract = next();
    else if (a === '--max') opts.max = Number(next());
    else if (a === '--disabled') opts.disabled = true;
    else if (a === '--format') opts.format = next();
    else if (a === '--out') opts.out = next();
    else if (a === '--now') opts.now = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  if (!['json', 'markdown', 'both'].includes(opts.format)) {
    throw new Error(`--format must be json|markdown|both, got ${opts.format}`);
  }
  return opts;
}

function main(argv, { exec, stdout, now } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  if (!opts.repo && !opts.issues) {
    throw new Error(`nothing to do: pass --repo or --issues\n${usage()}`);
  }
  const disabled = opts.disabled || process.env.AI_SDLC_PRIORITIZE === '0';
  const contract = loadContract(opts.contract, exec);
  const rubric = resolveRubric(contract);
  const nowMs = now !== undefined ? now : (opts.now ? Date.parse(opts.now) : Date.now());

  if (disabled || !rubric.enabled) {
    const off = {
      version: VERSION, stage: 'prioritize', repo: opts.repo || null,
      generatedAt: new Date(nowMs).toISOString(),
      enabled: false, reason: disabled ? 'disabled' : 'prioritize.enabled is not true in the contract',
      stats: { scanned: 0, armed: 0, excluded: 0 }, queue: [], excluded: [],
    };
    if (opts.json) emit(JSON.stringify(off));
    return off;
  }

  const cap = Math.min(HARD_MAX, Math.max(1, Math.trunc(opts.max)));
  const trigger = resolveBindings(contract).trigger;
  const issues = opts.issues
    ? JSON.parse(fs.readFileSync(opts.issues, 'utf8'))
    : listArmedIssues(opts.repo, trigger, cap, exec);

  const result = buildQueue({ repo: opts.repo, issues, contract, rubric, max: cap, now: nowMs });

  if (opts.out) {
    if (opts.format !== 'markdown') {
      fs.writeFileSync(`${opts.out}.json`, `${JSON.stringify(result, null, 2)}\n`);
    }
    if (opts.format !== 'json') {
      fs.writeFileSync(`${opts.out}.md`, toMarkdown(result));
    }
  }
  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      `queue_size=${result.queue.length}`,
      `queue_head=${result.queue.length ? result.queue[0].number : ''}`,
      `excluded=${result.excluded.length}`,
    ];
    fs.appendFileSync(process.env.GITHUB_OUTPUT, `${lines.join('\n')}\n`);
  }

  if (opts.json || (!opts.out && opts.format !== 'markdown')) emit(JSON.stringify(result));
  else if (opts.format !== 'json' && !opts.out) emit(toMarkdown(result));
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`prioritize-queue: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  VERSION,
  DEFAULT_WEIGHTS,
  resolveRubric,
  vetoLabels,
  armedStatus,
  priorityTier,
  isEpicLinked,
  scoreIssue,
  rankQueue,
  buildQueue,
  listArmedIssues,
  toMarkdown,
  main,
};
