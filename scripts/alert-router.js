#!/usr/bin/env node
// scripts/alert-router.js — failure-class → notification-route resolution (issue #175)
//
// Port of Hermes scripts/alert_router.js minus the Discord transport:
// a failure signature classifies into a canonical failure class
// (spec/risk-taxonomy.md "Failure classes and alert routing"), the resolved
// contract's escalation.routes map decides WHO is notified
// (mentions/labels/channel), and every routing decision is auditable — the
// emitted notification carries an `<!-- ai-sdlc:alert-route class=… key=… -->`
// marker that doubles as the dedup key, so a repeated signature on the same
// thread never pages twice.
//
// Route resolution (never silent — an unmatched class always lands on the
// default route):
//   escalation.routes.<class> → escalation.routes.default
//   → escalation.contacts/notify_channels (synthesized default)
//   → bare issue-comment route carrying only the escalation label
//
// Trust boundary: this file must only ever execute from a trusted ref
// (the runtime repo pin / base branch) — the route decides who gets
// @-mentioned on escalations. Callers pass PRE-FILTERED comments
// (bots/collaborators only) to --comments: a forged marker in an untrusted
// comment must never suppress a real alert.
//
// CLI:
//   node scripts/alert-router.js --repo OWNER/NAME --pr N \
//        --reason "repairs-exhausted:ci,build" [--class repairs-exhausted] \
//        [--signature S] [--contract contract.json|--escalation esc.json] \
//        [--comments trusted-comments.json] [--dedup-window-minutes N] \
//        [--audit-file decisions.jsonl] [--apply] [--json] [--github-output]
//
// --contract accepts either a resolved contract.json (reads .escalation)
// or a bare escalation section. --apply posts the notification comment and
// applies the route's extra labels via `gh` (bounded: label create + pr edit
// + one comment per target).
'use strict';

const fs = require('fs');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

// Canonical failure-class vocabulary (spec/risk-taxonomy.md). The contract
// map is open — escalation.routes may declare classes beyond this list and
// an explicit --class trusts the caller — but classifyFailure() only emits
// these ids or 'unknown'.
const FAILURE_CLASSES = [
  'conflict',
  'checks-stalled',
  'repair-stalled',
  'repairs-exhausted',
  'repair-failed',
  'review-blocked',
  'review-fix-failed',
  'supply-chain',
  'contract-error',
  'verify-failure',
  'security',
  'policy-hold',
  'unknown',
];
const DEFAULT_ROUTE_KEY = 'default';
const DEFAULT_CHANNEL = 'issue-comment';
const DEFAULT_ESCALATION_LABEL = 'needs-human';
// 0 = dedup for the whole thread; >0 bounds re-notification by minutes.
const DEFAULT_DEDUP_WINDOW_MINUTES = 0;

// Ordered classification patterns — first match wins, so the most specific
// signatures sit above the generic ones. Inputs are stage reason strings
// (automerge `reason`, ci-repair cause/outcome, verify context).
const CLASS_PATTERNS = [
  { cls: 'contract-error', re: /scc:config-error|base-contract-error|contract.{0,40}(invalid|error|failed)|invalid.{0,20}contract/i },
  { cls: 'security', re: /secret[\s-]?scan|credential|leaked?|gitleaks|private[\s_-]?key|token.{0,15}(exposed|committed)/i },
  { cls: 'supply-chain', re: /supply[\s-]?chain|dep(?:endenc(?:y|ies)|s)?[\s-]?violat|dep[\s-]?update/i },
  { cls: 'repairs-exhausted', re: /repairs?-exhausted|repair budget exhausted/i },
  { cls: 'review-fix-failed', re: /review-fix-(stalled|not-dispatched|failed)|fix pass.{0,20}(stalled|failed)/i },
  { cls: 'review-blocked', re: /external-review-blocking|review[\s-]?blocked|changes[\s_-]?requested/i },
  { cls: 'repair-stalled', re: /repair-stalled/i },
  { cls: 'checks-stalled', re: /pending-timeout|checks?[\s-]?(stalled|pending)|pending.{0,15}(>|timeout|cap)/i },
  { cls: 'conflict', re: /conflict/i },
  { cls: 'verify-failure', re: /post-merge|verify[\s-]?(fail|check|command)/i },
  { cls: 'policy-hold', re: /risk-critical|policy[\s-]?hold|unaccepted/i },
  { cls: 'repair-failed', re: /engine-exit|provider|quota|no-changes|push-rejected|commit-failed|no pushable fix|repair attempt/i },
];

// --- Classification ----------------------------------------------------------

// Free-text reason/signature → canonical class. `explicitClass` (CLI --class)
// wins and may be any contract-declared id; pattern-miss falls to 'unknown'.
function classifyFailure({ reason, signature, text, explicitClass } = {}) {
  if (explicitClass && String(explicitClass).trim()) {
    return String(explicitClass).trim();
  }
  const hay = [reason, signature, text].filter(Boolean).join('\n');
  for (const { cls, re } of CLASS_PATTERNS) {
    if (re.test(hay)) return cls;
  }
  return 'unknown';
}

// Normalize a free-text signature the same way ci-repair normalizes its log
// lines (issue #27): hex blobs → H, numbers → N — so dedup keys are stable
// across runs that only differ in SHAs/timestamps.
function normalizeSignature(text) {
  return String(text || '')
    .replace(/\b[0-9a-f]{7,40}\b/gi, 'H')
    .replace(/\d+/g, 'N')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, 300);
}

// Deterministic dedup key: same repo + target + class + normalized signature
// = same alert. sha256 truncated to 16 hex chars (same width as the ci-repair
// failure signature).
function dedupKey({ repo, target, cls, signature }) {
  return crypto
    .createHash('sha256')
    .update([repo || '', String(target || ''), cls || '', normalizeSignature(signature)].join(' '))
    .digest('hex')
    .slice(0, 16);
}

// --- Route resolution --------------------------------------------------------

function normalizeMention(m) {
  const s = String(m || '').trim();
  if (!s) return '';
  if (s.startsWith('@') || s.startsWith('#')) return s;
  return `@${s}`;
}

function normalizeRoute(route) {
  const r = route && typeof route === 'object' ? route : {};
  return {
    mentions: (Array.isArray(r.mentions) ? r.mentions : []).map(normalizeMention).filter(Boolean),
    labels: (Array.isArray(r.labels) ? r.labels : []).map((l) => String(l || '').trim()).filter(Boolean),
    channel: String(r.channel || DEFAULT_CHANNEL).trim() || DEFAULT_CHANNEL,
  };
}

// escalation: the resolved contract's `escalation` section (label, contacts,
// notify_channels, routes). Returns {class, matched, source, route} where
// `source` records WHICH rule produced the route — the audit trail.
function resolveRoute(cls, escalation) {
  const esc = escalation && typeof escalation === 'object' ? escalation : {};
  const routes = esc.routes && typeof esc.routes === 'object' ? esc.routes : {};
  if (routes[cls]) {
    return { class: cls, matched: cls, source: `routes.${cls}`, route: normalizeRoute(routes[cls]) };
  }
  if (routes[DEFAULT_ROUTE_KEY]) {
    return { class: cls, matched: DEFAULT_ROUTE_KEY, source: `routes.${DEFAULT_ROUTE_KEY}`, route: normalizeRoute(routes[DEFAULT_ROUTE_KEY]) };
  }
  // Synthesized default: legacy contacts/notify_channels still notify —
  // the escalation label + issue comment channel are the floor, so an
  // empty contract still produces a non-silent route.
  const contacts = Array.isArray(esc.contacts) ? esc.contacts : [];
  const channels = Array.isArray(esc.notify_channels) ? esc.notify_channels : [];
  return {
    class: cls,
    matched: 'contacts',
    source: contacts.length ? 'escalation.contacts' : 'escalation.fallback',
    route: normalizeRoute({
      mentions: contacts,
      labels: [],
      channel: channels[0] || DEFAULT_CHANNEL,
    }),
  };
}

// --- Dedup -------------------------------------------------------------------

const MARKER_RE = /<!-- ai-sdlc:alert-route class=(\S+) key=([0-9a-f]{8,64}) -->/;

function markerFor(cls, key) {
  return `<!-- ai-sdlc:alert-route class=${cls} key=${key} -->`;
}

// comments: [{body, created_at}] or [string] — PRE-FILTERED by the caller to
// trusted authors. Returns the newest marker matching `key` (same
// class+signature+target), honoring dedupWindowMinutes (0 = whole thread).
function findPriorAlert(comments, { key, dedupWindowMinutes = DEFAULT_DEDUP_WINDOW_MINUTES, now } = {}) {
  const list = Array.isArray(comments) ? comments : [];
  const nowMs = now === undefined ? Date.now() : now;
  let newest = null;
  for (const c of list) {
    const body = typeof c === 'string' ? c : (c && c.body) || '';
    const at = typeof c === 'object' && c ? Date.parse(c.created_at || c.createdAt || '') || 0 : 0;
    const m = MARKER_RE.exec(body);
    if (!m || m[2] !== key) continue;
    if (dedupWindowMinutes > 0 && nowMs - at > dedupWindowMinutes * 60000) continue;
    if (!newest || at > newest.at) newest = { class: m[1], key: m[2], at };
  }
  return newest;
}

// --- Notification ------------------------------------------------------------

// Render the notification comment body: routed mentions first (they are the
// delivery), then the failure context and the audit/dedup marker. Mentions
// are suppressed when deduped — the record still posts so escalation state
// stays visible, but nobody is re-paged for the same signature.
function renderNotification({ cls, route, title, reason, signature, deduped } = {}) {
  const r = (route && route.route) || route || { mentions: [], labels: [], channel: DEFAULT_CHANNEL };
  const lines = [];
  if (!deduped && r.mentions.length) lines.push(`cc ${r.mentions.join(' ')}`);
  lines.push(`⚠️ **${title || 'AI-SDLC alert'}** — failure class: \`${cls}\` (route: \`${(route && route.source) || 'default'}\`)`);
  if (reason) lines.push(`reason: \`${String(reason).replace(/`/g, "'").slice(0, 300)}\``);
  if (signature) lines.push(`signature: \`${signature}\``);
  if (deduped) lines.push('_(alert deduplicated — a notification for this failure signature already exists on this thread)_');
  return lines.join('\n');
}

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

// Apply a route: ensure the extra labels exist, add them to the target, and
// post the rendered comment. `target` is an issue/PR number. Deduped alerts
// still apply labels (idempotent, cheap) but skip the comment entirely —
// mentions included, so nothing re-pages.
function applyAlert({ repo, target, labels, body, deduped, exec }) {
  const run = exec || defaultExec;
  const applied = { labels: [], commented: false };
  for (const label of labels || []) {
    try {
      run(['label', 'create', label, '-R', repo, '--force']);
    } catch (e) {
      // Label exists or the app lacks label admin — the edit below still
      // applies the name; gh ignores unknown labels noisily, hence the try.
    }
    try {
      run(['issue', 'edit', String(target), '-R', repo, '--add-label', label]);
      applied.labels.push(label);
    } catch (e) {
      // A single unroutable label must not swallow the notification itself.
    }
  }
  if (!deduped) {
    const tmp = `${process.env.RUNNER_TEMP || '/tmp'}/ai-sdlc-alert-body-${Date.now()}.md`;
    fs.writeFileSync(tmp, body);
    try {
      run(['issue', 'comment', String(target), '-R', repo, '--body-file', tmp]);
      applied.commented = true;
    } finally {
      try { fs.unlinkSync(tmp); } catch (e) {}
    }
  }
  return applied;
}

// Audit record: one JSON line per routing decision — the machine-checkable
// trail behind the marker comments.
function auditRecord(decision) {
  return {
    at: new Date().toISOString(),
    class: decision.class,
    matched: decision.matched,
    source: decision.source,
    key: decision.key,
    signature: decision.signature,
    deduped: decision.deduped,
    mentions: (decision.route && decision.route.mentions) || [],
    labels: (decision.route && decision.route.labels) || [],
    channel: (decision.route && decision.route.channel) || DEFAULT_CHANNEL,
    reason: decision.reason || '',
  };
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: alert-router.js [--repo OWNER/NAME --pr N|--issue N]',
    '                       [--class CLS|--reason TEXT] [--signature S] [--text T]',
    '                       [--contract contract.json | --escalation esc.json]',
    '                       [--comments comments.json] [--dedup-window-minutes N]',
    '                       [--audit-file decisions.jsonl] [--title T]',
    '                       [--apply] [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = {
    json: false,
    apply: false,
    githubOutput: false,
    dedupWindowMinutes: DEFAULT_DEDUP_WINDOW_MINUTES,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--pr' || a === '--issue') opts.target = next();
    else if (a === '--class') opts.class = next();
    else if (a === '--reason') opts.reason = next();
    else if (a === '--signature') opts.signature = next();
    else if (a === '--text') opts.text = next();
    else if (a === '--title') opts.title = next();
    else if (a === '--contract') opts.contract = next();
    else if (a === '--escalation') opts.escalation = next();
    else if (a === '--comments') opts.comments = next();
    else if (a === '--dedup-window-minutes') opts.dedupWindowMinutes = Number(next());
    else if (a === '--audit-file') opts.auditFile = next();
    else if (a === '--apply') opts.apply = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

// --contract resolves either shape: a full resolved contract.json (reads
// .escalation) or a bare escalation section ({routes, contacts, …}).
function loadEscalation(opts) {
  const file = opts.contract || opts.escalation;
  if (!file) return {};
  const doc = JSON.parse(fs.readFileSync(file, 'utf8'));
  if (doc && typeof doc === 'object' && doc.escalation && typeof doc.escalation === 'object') {
    return doc.escalation;
  }
  return doc && typeof doc === 'object' ? doc : {};
}

function loadComments(file) {
  if (!file) return [];
  try {
    const doc = JSON.parse(fs.readFileSync(file, 'utf8'));
    return Array.isArray(doc) ? doc : [];
  } catch (e) {
    return [];
  }
}

function decide(opts, { exec } = {}) {
  const cls = classifyFailure({
    reason: opts.reason,
    signature: opts.signature,
    text: opts.text,
    explicitClass: opts.class,
  });
  const escalation = loadEscalation(opts);
  const resolved = resolveRoute(cls, escalation);
  const signature = opts.signature || normalizeSignature(opts.reason || opts.text || cls);
  const key = dedupKey({ repo: opts.repo, target: opts.target, cls, signature });

  const comments = loadComments(opts.comments);
  const prior = findPriorAlert(comments, { key, dedupWindowMinutes: opts.dedupWindowMinutes });
  const deduped = Boolean(prior);

  const body = renderNotification({
    cls,
    route: resolved,
    title: opts.title || 'AI-SDLC alert',
    reason: opts.reason,
    signature,
    deduped,
  }) + `\n${markerFor(cls, key)}\n`;

  const decision = {
    class: cls,
    matched: resolved.matched,
    source: resolved.source,
    route: resolved.route,
    signature,
    key,
    deduped,
    dedup_prior_at: prior ? new Date(prior.at).toISOString() : null,
    body,
    escalation_label: escalation.label || DEFAULT_ESCALATION_LABEL,
  };

  if (opts.apply) {
    if (!opts.repo || !opts.target) throw new Error('--apply requires --repo and --pr/--issue');
    decision.applied = applyAlert({
      repo: opts.repo,
      target: opts.target,
      labels: resolved.route.labels,
      body,
      deduped,
      exec,
    });
  }

  if (opts.auditFile) {
    try {
      fs.appendFileSync(opts.auditFile, JSON.stringify(auditRecord(decision)) + '\n');
    } catch (e) {
      // Audit best-effort: never let an unwritable file silence the alert.
    }
  }
  return decision;
}

function main(argv, { exec, stdout } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  const decision = decide(opts, { exec });

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      `alert_class=${decision.class}`,
      `alert_matched=${decision.matched}`,
      `alert_source=${decision.source}`,
      `alert_key=${decision.key}`,
      `alert_deduped=${decision.deduped}`,
      `alert_channel=${decision.route.channel}`,
      `alert_mentions=${decision.deduped ? '' : decision.route.mentions.join(' ')}`,
      `alert_labels=${JSON.stringify(decision.route.labels)}`,
      `alert_marker=${markerFor(decision.class, decision.key)}`,
    ];
    fs.appendFileSync(process.env.GITHUB_OUTPUT, lines.join('\n') + '\n');
  }

  emit(JSON.stringify(decision));
  return decision;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`alert-router: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  FAILURE_CLASSES,
  DEFAULT_ROUTE_KEY,
  DEFAULT_CHANNEL,
  CLASS_PATTERNS,
  MARKER_RE,
  classifyFailure,
  normalizeSignature,
  dedupKey,
  normalizeMention,
  normalizeRoute,
  resolveRoute,
  markerFor,
  findPriorAlert,
  renderNotification,
  applyAlert,
  auditRecord,
  decide,
  main,
};
