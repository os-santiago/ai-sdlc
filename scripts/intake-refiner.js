#!/usr/bin/env node
// scripts/intake-refiner.js — conversational intake refinement (issue #197,
// epic #147 intake intelligence).
//
// Canonical deterministic implementation of the DoR escalation contract
// (spec/definition-of-ready.md "Escalation semantics", spec/labels.md T2/T3
// transitions): when a new issue is under-specified the refiner posts ONE
// upserted comment with clarifying questions and applies the
// refinement-hold label; when the spec satisfies the checks it posts the
// derived acceptance criteria and clears the hold — restoring the dispatch
// trigger only when the refiner itself stripped it from an armed issue.
//
// Idempotency: every refinement comment carries a stable marker
//   <!-- ai-sdlc:refinement fp:"<sha16>" status:"<questions|ready>" armed:"<bool>" -->
// where `fp` fingerprints title+body+the issue author's replies. The same
// fingerprint → no-op (one comment ever, never spam); an edit or author
// reply → the marker comment is PATCHed in place. `armed` latches whether
// the trigger label was present when the hold was applied, so a passing
// re-check can restore it in the same label mutation (labels.md F2: the
// winning hold strips the trigger in the same op where feasible; T3: the
// refiner re-arms after a full re-check pass).
//
// The refiner NEVER edits the issue body — the author's spec stays
// authoritative; proposals live in the single refinement comment. Skip
// labels (contract `triggers.skip_labels`) and veto labels (skip ∪ active
// run-state bindings) are honored: a label that would veto dispatch also
// vetoes trigger restoration.
//
// Trust boundary: this file must only execute from the runtime repo pin —
// the intake workflow fetches it via `gh api .../contents/...?ref=<pin>`,
// never from the consumer repo (same posture as risk-classifier.js).
//
// Bounded: no engine, no model. A fixed handful of `gh` calls — one issue
// view, one comments page list, at most one label create + one issue edit +
// one comment upsert per run.
//
// CLI:
//   node scripts/intake-refiner.js --repo OWNER/NAME --issue N [--apply]
//       [--trigger ready-to-implement] [--hold scc:not-ready]
//       [--skip-labels '["epic","wontfix"]'] [--veto-labels '[...]']
//       [--min-body 120] [--json] [--github-output]
//   node scripts/intake-refiner.js --issue-file issue.json \
//       [--comments-file comments.json] [--json]   # offline preview
'use strict';

const crypto = require('crypto');
const fs = require('fs');
const { execFileSync } = require('child_process');

const MARKER_RE = /<!--\s*ai-sdlc:refinement\s+fp:"([a-f0-9]+)"\s+status:"([a-z-]+)"\s+armed:"(true|false)"\s*-->/;
const MARKER_SEARCH_RE = /<!--\s*ai-sdlc:refinement\s/;
// Any ai-sdlc pipeline comment — never counts as an author reply.
const PIPELINE_COMMENT_RE = /<!--\s*ai-sdlc:/;

const DEFAULT_TRIGGER = 'ready-to-implement';
const DEFAULT_HOLD = 'scc:not-ready';
const DEFAULT_MIN_BODY = 120;
const MAX_AUTHOR_REPLIES = 6;
const MAX_COMMENTS_SCANNED = 100;
const MAX_AC_ITEMS = 8;
const HOLD_LABEL_COLOR = 'D93F0B';

// Conventional-commit title — mirrors R1 in the intake DoR gate.
const TITLE_RE = /^(feat|fix|docs|chore|refactor|perf|test|build|ci|security)(\(.+\))?: .{5,}/;

// --- Spec extraction ----------------------------------------------------------

// The "## Acceptance criteria" (or "## AC") section body — same shape the
// intake DoR validator uses, so refinement and dispatch agree.
function acSection(body) {
  const text = String(body || '');
  if (!/##\s*(acceptance criteria|ac)\b/i.test(text)) return '';
  return text.split(/##\s*(?:acceptance criteria|ac)\b/i)[1] || '';
}

// Bullet/checklist items inside the AC section, normalized to plain text.
function acItems(issue) {
  const sec = acSection(issue && issue.body);
  if (!sec) return [];
  return sec
    .split('\n')
    .filter((l) => /^\s*(?:-|\*|\d+\.)\s+/.test(l))
    .map((l) => l.replace(/^\s*(?:-|\*|\d+\.)\s+(?:\[[ xX]\]\s+)?/, '').trim())
    .filter(Boolean)
    .slice(0, MAX_AC_ITEMS);
}

// Spec-completeness check — the R1/R2/R3/R4/R6 subset of the DoR gate that
// describes the *spec* (R5 label conflicts are the dispatch gate's, not a
// question for the author). Returns {checks, failed[]}.
function specCheck(issue, { minBody = DEFAULT_MIN_BODY } = {}) {
  const body = String((issue && issue.body) || '');
  const sec = acSection(body);
  const items = (sec.match(/^\s*(?:-|\*|\d+\.)\s+/gm) || []).length;
  const checks = {
    'R1 conventional title': TITLE_RE.test(String((issue && issue.title) || '')),
    'R2 AC section present': sec.trim().length > 0,
    'R3 >=1 AC item': items >= 1,
    'R4 non-trivial body': body.trim().length >= minBody,
    'R6 no TBD in AC': !/\bTBD\b|\bTODO\b/i.test(sec),
  };
  const failed = Object.entries(checks).filter(([, ok]) => !ok).map(([k]) => k);
  return { checks, failed };
}

// Failed rule → the question the author can answer. Deterministic — this
// stage runs without a model, so questions map 1:1 onto the failed checks.
const RULE_QUESTIONS = {
  'R1 conventional title': 'Restate the title in conventional-commit form — what `type(scope): summary` applies to this change?',
  'R2 AC section present': 'What acceptance criteria define "done"? Add an `## Acceptance criteria` section with falsifiable checks the implementer can verify.',
  'R3 >=1 AC item': 'List the acceptance criteria as concrete checklist items — what must be true when this lands?',
  'R4 non-trivial body': 'Describe the problem and expected behavior — what should change, where, and why?',
  'R6 no TBD in AC': 'Replace the placeholder markers (TBD/TODO) in the acceptance criteria with concrete checks.',
};

function questionsFor(failed) {
  return failed.map((r) => RULE_QUESTIONS[r]).filter(Boolean).slice(0, 4);
}

// --- Fingerprint + replies ----------------------------------------------------

// Issue author's comments are part of the spec source: a reply answering
// the refinement questions must re-trigger the check. Pipeline comments
// (any `<!-- ai-sdlc:` marker) never count. When the author is unknown
// every non-pipeline comment counts. Oldest→newest, capped.
function authorReplies(issue, comments) {
  const author = issue && issue.author && issue.author.login;
  const authorLc = author ? String(author).toLowerCase() : null;
  return (Array.isArray(comments) ? comments : [])
    .filter((c) => c && c.body && !PIPELINE_COMMENT_RE.test(String(c.body)))
    .filter((c) => {
      if (!authorLc) return true;
      const login = c.author && c.author.login ? c.author.login : c.user && c.user.login;
      return String(login || '').toLowerCase() === authorLc;
    })
    .map((c) => String(c.body))
    .slice(-MAX_AUTHOR_REPLIES);
}

function issueFingerprint(issue, replies) {
  let input = `${(issue && issue.title) || ''}\n${(issue && issue.body) || ''}`;
  if (replies && replies.length) input += `\n--- comments ---\n${replies.join('\n---\n')}`;
  return crypto.createHash('sha256').update(input).digest('hex').slice(0, 16);
}

// --- Marker + comment rendering ------------------------------------------------

function parseMarker(body) {
  const m = MARKER_RE.exec(String(body || ''));
  return m ? { fp: m[1], status: m[2], armed: m[3] === 'true' } : null;
}

function markerLine(fp, status, armed) {
  return `<!-- ai-sdlc:refinement fp:"${fp}" status:"${status}" armed:"${armed ? 'true' : 'false'}" -->`;
}

function renderQuestionsComment({ fp, armed, failed, questions, hold }) {
  return [
    markerLine(fp, 'questions', armed),
    `\u{1F6E1}️ **AI-SDLC intake — refinement needed** (${failed.map((f) => `\`${f.split(' ')[0]}\``).join(', ')}):`,
    'this issue is under-specified for agent dispatch (spec/definition-of-ready.md).',
    'Please refine it — edit the issue body or reply in-thread:',
    '',
    ...questions.map((q) => `- ${q}`),
    '',
    `The check re-runs on the next intake event (edit or reply) and clears \`${hold}\` once the spec passes.`,
  ].join('\n');
}

function renderReadyComment({ fp, armed, items, trigger }) {
  const tail = armed
    ? `The \`${trigger}\` arm was restored — this issue re-entered the dispatch queue.`
    : `Apply \`${trigger}\` to queue this issue for implementation.`;
  return [
    markerLine(fp, 'ready', armed),
    '✅ **AI-SDLC intake — spec satisfies the Definition of Ready.** Derived acceptance criteria:',
    '',
    ...items.map((i) => `- [ ] ${i}`),
    '',
    tail,
  ].join('\n');
}

// --- Decision (pure) ------------------------------------------------------------

function labelNames(issue) {
  return ((issue && issue.labels) || [])
    .map((l) => (typeof l === 'string' ? l : l && l.name) || '')
    .map((s) => String(s).toLowerCase())
    .filter(Boolean);
}

// The latest refinement comment on the issue (marker + REST comment id —
// needed for the PATCH upsert). `comments` rows accept either REST shape
// ({id, user:{login}}) or gh --json shape ({id, author:{login}}).
function findRefinementComment(comments) {
  const list = (Array.isArray(comments) ? comments : []).slice(-MAX_COMMENTS_SCANNED);
  for (let i = list.length - 1; i >= 0; i -= 1) {
    const c = list[i];
    if (c && c.body && MARKER_SEARCH_RE.test(String(c.body))) {
      return { id: c.id, body: String(c.body), marker: parseMarker(c.body) };
    }
  }
  return null;
}

/**
 * Decide the refinement action for one issue snapshot.
 *
 * @param {object} opts.issue   {number,title,body,labels,author}
 * @param {Array}  opts.comments issue comments (REST or gh --json shape)
 * @param {string} opts.trigger  dispatch trigger label binding
 * @param {string} opts.hold     refinement-hold label binding (labels.not-ready)
 * @param {Array}  opts.skipLabels   labels that make the issue non-actionable
 * @param {Array}  opts.vetoLabels   labels that veto trigger restoration
 * @param {number} opts.minBody
 * @returns {object} {action, fingerprint, ...}
 *   action: 'skip' | 'unchanged' | 'questions' | 'ready'
 */
function refineIssue(opts) {
  const issue = opts.issue || {};
  const comments = opts.comments || [];
  const trigger = String(opts.trigger || DEFAULT_TRIGGER).toLowerCase();
  const hold = String(opts.hold || DEFAULT_HOLD).toLowerCase();
  const skip = new Set((opts.skipLabels || []).map((l) => String(l).toLowerCase()));
  const veto = new Set((opts.vetoLabels || opts.skipLabels || []).map((l) => String(l).toLowerCase()));

  const labels = labelNames(issue);
  const fp = issueFingerprint(issue, authorReplies(issue, comments));
  const prior = findRefinementComment(comments);
  const base = { fingerprint: fp, issue: issue.number, priorCommentId: prior ? prior.id : null };

  // Non-actionable — a skip label makes the issue ineligible for refinement
  // entirely (epic, wontfix, needs-human, …). The hold itself is NOT in the
  // skip set by contract: carrying it is exactly what makes an issue
  // eligible for re-check.
  const skippedBy = labels.find((l) => skip.has(l) && l !== hold);
  if (skippedBy) return { ...base, action: 'skip', why: `skip label present: ${skippedBy}` };

  // Idempotent — same spec source as the last refinement pass.
  if (prior && prior.marker && prior.marker.fp === fp) {
    return { ...base, action: 'unchanged', status: prior.marker.status };
  }

  const armed = labels.includes(trigger);
  const { failed } = specCheck(issue, { minBody: opts.minBody });

  if (failed.length) {
    // Under-specified → questions + hold. F2 repair: applying the hold
    // strips the trigger in the same operation where feasible; `armed`
    // latches the arm so a later pass can restore it.
    return {
      ...base,
      action: 'questions',
      failed,
      questions: questionsFor(failed),
      armed,
      addLabels: [hold],
      removeLabels: armed ? [trigger] : [],
      comment: renderQuestionsComment({ fp, armed, failed, questions: questionsFor(failed), hold }),
    };
  }

  // Spec-ready → derived acceptance criteria. Clear the hold when present;
  // restore the trigger only when the refiner stripped it (prior marker
  // armed=true) and no veto label survives on the post-hold label set.
  const held = labels.includes(hold);
  const postHold = labels.filter((l) => l !== hold);
  const vetoedBy = postHold.find((l) => veto.has(l));
  const restore = held && !armed && prior && prior.marker && prior.marker.armed === true && !vetoedBy;
  return {
    ...base,
    action: 'ready',
    items: acItems(issue),
    armed,
    held,
    restored: Boolean(restore),
    vetoedBy: held && prior && prior.marker && prior.marker.armed === true && !armed ? vetoedBy || null : null,
    addLabels: restore ? [trigger] : [],
    removeLabels: held ? [hold] : [],
    comment: renderReadyComment({ fp, armed: Boolean(restore), items: acItems(issue), trigger }),
  };
}

// --- GitHub I/O -----------------------------------------------------------------

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function fetchIssue(repo, issueNumber, exec) {
  const run = exec || defaultExec;
  const out = run(['issue', 'view', String(issueNumber), '-R', repo, '--json', 'number,title,body,labels,author']);
  return JSON.parse(out || '{}');
}

// REST issue comments — the numeric `id` is required for the PATCH upsert
// (gh --json comments expose node ids, unusable here). Paginated, capped.
function fetchComments(repo, issueNumber, exec) {
  const run = exec || defaultExec;
  const out = run(['api', '--paginate', `repos/${repo}/issues/${issueNumber}/comments?per_page=100`]);
  return String(out || '[]')
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.startsWith('[') || l.startsWith('{'))
    .flatMap((l) => {
      try {
        const doc = JSON.parse(l);
        return Array.isArray(doc) ? doc : [doc];
      } catch (e) {
        return [];
      }
    })
    .map((c) => ({ id: c.id, body: c.body, author: { login: c.user && c.user.login } }));
}

// One upsert: PATCH the existing marker comment or POST a new one.
function upsertComment({ repo, issue, commentId, body, exec }) {
  const run = exec || defaultExec;
  if (commentId) {
    run(['api', `repos/${repo}/issues/comments/${commentId}`, '-X', 'PATCH', '-f', `body=${body}`]);
    return { op: 'patch', id: commentId };
  }
  run(['issue', 'comment', String(issue), '-R', repo, '--body', body]);
  return { op: 'create' };
}

// One label mutation: ensure the hold label exists, then apply the delta in
// a single `issue edit` (winner + loser stripped together, labels.md).
function applyLabels({ repo, issue, add, remove, exec }) {
  const run = exec || defaultExec;
  const adds = add || [];
  const removes = remove || [];
  for (const l of adds) {
    try {
      run(['label', 'create', l, '-R', repo, '--color', HOLD_LABEL_COLOR,
        '--description', 'AI-SDLC: Definition-of-Ready failed — refinement in progress', '--force']);
    } catch (e) { /* exists already, or no label admin — the edit still applies */ }
  }
  if (!adds.length && !removes.length) return { skipped: true };
  const args = ['issue', 'edit', String(issue), '-R', repo];
  if (adds.length) args.push('--add-label', adds.join(','));
  if (removes.length) args.push('--remove-label', removes.join(','));
  run(args);
  return { added: adds, removed: removes };
}

// Apply the decided action: upsert the refinement comment + label delta.
function applyRefinement({ repo, result, exec }) {
  const ops = { comment: null, labels: null };
  if (result.comment) {
    ops.comment = upsertComment({
      repo, issue: result.issue, commentId: result.priorCommentId, body: result.comment, exec,
    });
  }
  ops.labels = applyLabels({
    repo, issue: result.issue, add: result.addLabels, remove: result.removeLabels, exec,
  });
  return ops;
}

// --- CLI ------------------------------------------------------------------------

function usage() {
  return [
    'usage: intake-refiner.js [--repo OWNER/NAME --issue N] [--issue-file F] [--comments-file F]',
    '                         [--trigger L] [--hold L] [--skip-labels JSON] [--veto-labels JSON]',
    '                         [--min-body N] [--apply] [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = { apply: false, json: false, githubOutput: false, minBody: DEFAULT_MIN_BODY };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--issue') opts.issue = next();
    else if (a === '--issue-file') opts.issueFile = next();
    else if (a === '--comments-file') opts.commentsFile = next();
    else if (a === '--trigger') opts.trigger = next();
    else if (a === '--hold') opts.hold = next();
    else if (a === '--skip-labels') opts.skipLabels = JSON.parse(next() || '[]');
    else if (a === '--veto-labels') opts.vetoLabels = JSON.parse(next() || '[]');
    else if (a === '--min-body') opts.minBody = Number(next());
    else if (a === '--apply') opts.apply = true;
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
  const run = exec || defaultExec;

  let issue; let comments;
  if (opts.issueFile) {
    issue = JSON.parse(fs.readFileSync(opts.issueFile, 'utf8'));
    comments = opts.commentsFile ? JSON.parse(fs.readFileSync(opts.commentsFile, 'utf8')) : [];
  } else {
    if (!opts.repo || !opts.issue) throw new Error(`--repo and --issue are required (or use --issue-file)\n${usage()}`);
    issue = fetchIssue(opts.repo, opts.issue, run);
    comments = fetchComments(opts.repo, opts.issue, run);
  }

  const result = refineIssue({
    issue,
    comments,
    trigger: opts.trigger || DEFAULT_TRIGGER,
    hold: opts.hold || DEFAULT_HOLD,
    skipLabels: opts.skipLabels || [],
    vetoLabels: (opts.vetoLabels && opts.vetoLabels.length) ? opts.vetoLabels : opts.skipLabels || [],
    minBody: opts.minBody,
  });

  if (opts.apply && opts.repo && (result.action === 'questions' || result.action === 'ready')) {
    result.applied = applyRefinement({ repo: opts.repo, result, exec: run });
  } else {
    delete result.comment; // preview mode: don't emit the full body in JSON
  }

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT, [
      `status=${result.action}`,
      `fingerprint=${result.fingerprint || ''}`,
      `failed=${(result.failed || []).join('|')}`,
    ].join('\n') + '\n');
  }

  emit(JSON.stringify(result));
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`intake-refiner: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  MARKER_RE,
  DEFAULT_TRIGGER,
  DEFAULT_HOLD,
  acSection,
  acItems,
  specCheck,
  questionsFor,
  authorReplies,
  issueFingerprint,
  parseMarker,
  findRefinementComment,
  renderQuestionsComment,
  renderReadyComment,
  refineIssue,
  fetchIssue,
  fetchComments,
  upsertComment,
  applyLabels,
  applyRefinement,
  main,
};
