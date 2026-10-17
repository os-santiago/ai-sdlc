#!/usr/bin/env node
// scripts/umbrella-coordinator.js — umbrella coordination for
// contract-spanning work (issue #199, epic #147).
//
// A change that must land coherently across several repos (API contract
// bumps, shared protocol fields, dependency couples) is represented by ONE
// `umbrella`-labeled issue on a hub repo; every participating repo gets a
// scoped child issue carrying a machine-checkable marker + parent link, and
// the umbrella carries a single upserted status comment so per-repo state
// rolls up in one place.
//
// Declaration model — the umbrella issue declares its participating
// repositories in either form (explicit flags win over body declarations):
//
//   ```ai-sdlc:umbrella                      (the hermes:umbrella fence is
//   {"items": [{"repo": "o/a", "title": "feat(api): ..."},    also accepted
//                {"repo": "o/b", "issue": 12},               for control-plane
//                "o/c — feat(cli): ..."]}                     interop)
//   ```
//
//   or plain bullets anywhere in the body:
//
//   - o/a — feat(api): accept X-Contract-Version header
//   - o/b#12 — adopt an existing issue instead of filing a new one
//   - o/c    (bare repo: tracks the repo slice under the umbrella title)
//
// Per pass the coordinator:
//   1. resolves the umbrella key (identity marker or a slug of the title —
//      deterministic either way),
//   2. merges declared items (--repos/--spec > fenced block > body bullets),
//      deduped one-child-per-repo and bounded by --max-repos,
//   3. scans each participating repo (open + closed, bounded) for issues
//      carrying the `ai-sdlc-umbrella-child` marker that points back at this
//      umbrella — adoption, never re-filing,
//   4. files the missing children (parent link + marker + `scc:child`,
//      NO dispatch trigger — a human applies `ready-to-implement` per repo,
//      same contract as epic-decomposer children),
//   5. upserts the single status comment on the umbrella issue — per-repo
//      checklist rows track pending → open → closed; re-runs update the
//      comment in place rather than stacking.
//
// Idempotent by construction: every write is keyed on a marker that the
// next pass reads back, so a repeated run converges to zero mutations.
//
// Ported from the Hermes control plane (scripts/umbrella_coordinator.js):
// same marker/adoption/never-resurrect model; the hub checklist lives in a
// status comment (not a managed body block) per this runtime's contract.
//
// CLI:
//   node scripts/umbrella-coordinator.js --repo OWNER/NAME --issue N \
//     [--repos o/a,o/b | --spec spec.json] [--child-labels csv] \
//     [--max-repos N] [--dry-run] [--json] [--github-output]
//
// Env: UMBRELLA_LABEL (default 'umbrella'), UMBRELLA_CHILD_LABEL
//      (default 'scc:child'), UMBRELLA_MAX_REPOS (default 8).
'use strict';

const fs = require('fs');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const UMBRELLA_LABEL = (process.env.UMBRELLA_LABEL || 'umbrella').toLowerCase();
const CHILD_LABEL = process.env.UMBRELLA_CHILD_LABEL || 'scc:child';
const DEFAULT_MAX_REPOS = parseInt(process.env.UMBRELLA_MAX_REPOS || '8', 10);
const MAX_ISSUES = 100;          // per-repo per-state child scan bound
const MAX_COMMENT_PAGES = 3;     // <=300 comments scanned per umbrella
const COMMENTS_PER_PAGE = 100;
const MAX_TITLE = 90;

// Machine markers — ai-sdlc-* family, same convention as postmortem.js.
const IDENTITY_RE = /<!--\s*ai-sdlc-umbrella\s+key:"([A-Za-z0-9_.-]+)"\s*-->/;
const STATUS_MARKER_RE = /<!--\s*ai-sdlc-umbrella-status(?:\s+key:"([A-Za-z0-9_.-]+)")?\s*-->/;
const STATUS_END_MARKER = '<!-- /ai-sdlc-umbrella-status -->';
const CHILD_MARKER_RE = /ai-sdlc-umbrella-child\s+key:"([A-Za-z0-9_.-]+)"\s+umbrella:"([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)#(\d+)"/;
const FENCE_RE = /```\s*(?:ai-sdlc|hermes):umbrella[^\n`]*\n?([\s\S]*?)```/i;
const TITLE_PREFIX_RE = /^(feat|fix|docs|chore|refactor|perf|test|build|ci|security)(\([\w-]*\))?:\s*/i;
const REPO_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
// Ambiguous-line guard for `o/r — title` bullets: the owner segment must not
// be a known directory word and the name must not look like a file (the
// Hermes A3 foreign-repo heuristic). Explicit {"repo": ...} spec fields skip
// this guard — they are unambiguous.
const DIR_OWNERS = /^(node|usr|etc|var|opt|home|tmp|scripts|src|test|tests|config|lib|app|docs|api|web|workspace|bin|pkg|internal|cmd|evals|reports|security)$/;
const DECL_SEP_RE = /^([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)(?:#(\d+))?(?:\s+[—–-]\s+|\s*:\s+)(.+)$/;
const DECL_BARE_RE = /^([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)(?:#(\d+))?$/;
const MARKER_HINT_RE = /ai-sdlc-|hermes:/;

// --- Pure: keys, spec parsing, item normalization (ported model) ------------

/** Stable identity for a work item: slug of repo + title. */
function slugifyKey(title) {
  const stripped = String(title || '').replace(TITLE_PREFIX_RE, '');
  const slug = stripped.toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 48)
    .replace(/-+$/g, '');
  return slug || `item-${crypto.createHash('sha1').update(String(title)).digest('hex').slice(0, 8)}`;
}

function itemKeyFor(repo, title) {
  return slugifyKey(`${repo} ${title || ''}`);
}

/** Conservative repo-ref check for ambiguous line input (see DIR_OWNERS). */
function isRepoRef(s) {
  if (!REPO_RE.test(String(s || ''))) return false;
  const [owner, name] = s.toLowerCase().split('/');
  if (DIR_OWNERS.test(owner)) return false;
  if (/\.[a-z0-9]{1,6}$/.test(name)) return false; // looks like a file path
  return true;
}

/**
 * Repo work-item title: explicit title wins; bare repos inherit the umbrella
 * title. Result is always conventional-commit shaped (<=MAX_TITLE) so a
 * filed child is dispatch-ready under the DoR gate.
 */
function normalizeChildTitle(title, umbrellaTitle) {
  let t = String(title || umbrellaTitle || '').trim().replace(/\s+/g, ' ');
  if (!t) t = 'contract-spanning work item';
  if (!TITLE_PREFIX_RE.test(t)) t = `feat(fleet): ${t}`;
  return t.length > MAX_TITLE ? t.slice(0, MAX_TITLE - 3).replace(/[\s:,-]+$/, '') + '...' : t;
}

/**
 * One declaration -> normalized item {key, repo, title, summary, issue}.
 * Accepts 'o/r' | 'o/r#5' | 'o/r — title' | 'o/r: title' |
 * {repo, title?, summary?, key?, issue?}. Returns null for unusable input.
 */
function normalizeItem(raw, ctx = {}) {
  let repo = null, title = null, summary = '', key = null, issue = null;
  if (typeof raw === 'string') {
    const s = raw.trim();
    let m = s.match(DECL_SEP_RE);
    if (m && isRepoRef(m[1])) {
      repo = m[1]; issue = m[2] ? parseInt(m[2], 10) : null; title = m[3].trim() || null;
    } else {
      m = s.match(DECL_BARE_RE);
      if (!m || !isRepoRef(m[1])) return null;
      repo = m[1]; issue = m[2] ? parseInt(m[2], 10) : null;
    }
  } else if (raw && typeof raw === 'object') {
    repo = String(raw.repo || raw.repository || '').trim();
    if (!REPO_RE.test(repo)) return null;
    title = raw.title ? String(raw.title).trim() : null;
    summary = String(raw.summary || raw.problem || raw.description || '').trim();
    key = raw.key ? String(raw.key).trim() : null;
    issue = raw.issue !== undefined && raw.issue !== null && raw.issue !== ''
      ? parseInt(raw.issue, 10) : null;
  } else {
    return null;
  }
  title = normalizeChildTitle(title, ctx.umbrellaTitle);
  if (!Number.isFinite(issue)) issue = null;
  return { key: key || itemKeyFor(repo, title), repo, title, summary, issue };
}

/**
 * Declarations -> normalized items, ONE per repo (the contract: exactly one
 * child per participating repo) and bounded by maxRepos. First declaration
 * of a repo wins; overflow is reported, never silently dropped.
 */
function normalizeItems(list, ctx = {}) {
  const src = Array.isArray(list) ? list
    : (list && typeof list === 'object' ? (list.items || list.repos || list.repositories || []) : []);
  const max = Number.isFinite(ctx.maxRepos) ? ctx.maxRepos : DEFAULT_MAX_REPOS;
  const seenRepos = new Set();
  const items = [];
  let dupes = 0, dropped = 0;
  for (const raw of src) {
    const item = normalizeItem(raw, ctx);
    if (!item) { dropped++; continue; }
    if (seenRepos.has(item.repo.toLowerCase())) { dupes++; continue; }
    seenRepos.add(item.repo.toLowerCase());
    items.push(item);
  }
  const bounded = items.slice(0, max);
  return { items: bounded, truncated: items.length - bounded.length, dupes, dropped };
}

/** Extract the model-agnostic JSON payload — tolerates prose around it. */
function extractJson(text) {
  const s = String(text || '');
  const objStart = s.indexOf('{');
  const arrStart = s.indexOf('[');
  if (objStart === -1 && arrStart === -1) return null;
  const pick = (start, close) => {
    if (start === -1) return null;
    const end = s.lastIndexOf(close);
    if (end === -1 || end < start) return null;
    try { return JSON.parse(s.slice(start, end + 1)); } catch (e) { return null; }
  };
  if (arrStart !== -1 && (objStart === -1 || arrStart < objStart)) return pick(arrStart, ']');
  return pick(objStart, '}');
}

/**
 * Parse the fenced ```ai-sdlc:umbrella (or legacy ```hermes:umbrella)
 * declaration block. Content is a JSON object/array ({items|repos:[...]}) or
 * line-format `o/r — title` entries. Returns {items:[raw]} or null.
 */
function parseUmbrellaSpec(body) {
  const m = String(body || '').match(FENCE_RE);
  if (!m) return null;
  const inner = (m[1] || '').trim();
  if (!inner) return { items: [] };
  const json = extractJson(inner);
  if (json) {
    const list = Array.isArray(json) ? json : (json.items || json.repos || json.repositories || []);
    return { items: Array.isArray(list) ? list : [] };
  }
  const items = [];
  for (const line of inner.split('\n')) {
    const t = line.replace(/^\s*[-*]\s+(?:\[[ xX]\]\s+)?/, '').trim();
    if (t) items.push(t);
  }
  return { items };
}

/**
 * Unmarked `o/r — title` / `o/r` / `o/r#N` bullet declarations in arbitrary
 * text. Marker-carrying rows are ignored — they are managed lines, not
 * declarations. Bullet-gated: bare prose starting with `owner/repo` is never
 * adopted as an item.
 */
function declaredItems(text) {
  const out = [];
  for (const line of String(text || '').split('\n')) {
    const bullet = String(line).match(/^\s*[-*]\s+(?:\[[ xX]\]\s+)?(.*)$/);
    if (!bullet) continue;
    const stripped = bullet[1].trim();
    if (!stripped || MARKER_HINT_RE.test(stripped)) continue;
    let m = stripped.match(DECL_SEP_RE);
    if (m && isRepoRef(m[1])) {
      out.push({ repo: m[1], issue: m[2] ? parseInt(m[2], 10) : null, title: m[3].trim() });
      continue;
    }
    m = stripped.match(DECL_BARE_RE);
    if (m && isRepoRef(m[1])) {
      out.push({ repo: m[1], issue: m[2] ? parseInt(m[2], 10) : null, title: null });
    }
  }
  return out;
}

/** Umbrella identity: explicit identity marker, else a deterministic title slug. */
function umbrellaKey(issue) {
  const m = String((issue && issue.body) || '').match(IDENTITY_RE);
  if (m) return m[1];
  return slugifyKey((issue && issue.title) || 'umbrella');
}

function hasLabel(issue, name) {
  const want = String(name).toLowerCase();
  return ((issue && issue.labels) || []).some(l =>
    String(typeof l === 'string' ? l : (l && l.name) || '').toLowerCase() === want);
}

/**
 * Merge all declaration sources for an umbrella issue. Explicit opts.items
 * (--repos/--spec) win outright; otherwise the fenced spec is merged with
 * body bullets (fence first — it is the structured source).
 */
function resolveDeclarations(opts, issue) {
  if (Array.isArray(opts.items) && opts.items.length) {
    return normalizeItems(opts.items, { umbrellaTitle: issue.title, maxRepos: opts.maxRepos });
  }
  const spec = parseUmbrellaSpec(issue.body);
  const raw = [...(spec ? spec.items : []), ...declaredItems(issue.body)];
  return normalizeItems(raw, { umbrellaTitle: issue.title, maxRepos: opts.maxRepos });
}

// --- Pure: markers, bodies, status comment ---------------------------------

function childMarker(item, umbrellaRef) {
  return `<!-- ai-sdlc-umbrella-child key:"${item.key}" umbrella:"${umbrellaRef}" -->`;
}

function statusMarker(key) {
  return `<!-- ai-sdlc-umbrella-status key:"${key}" -->`;
}

/**
 * Find the existing child for a repo among scanned issues. Match is by
 * umbrella back-reference + item key first, then umbrella back-reference
 * alone for the same repo's scan (one child per repo is the contract — a
 * marker pointing at this umbrella inside this repo's list IS the child).
 */
function findChild(scanned, umbrellaRef, item) {
  let byUmbrella = null;
  for (const iss of scanned || []) {
    const m = String(iss.body || '').match(CHILD_MARKER_RE);
    if (!m || `${m[2]}#${m[3]}` !== umbrellaRef) continue;
    if (m[1] === item.key) return iss;
    if (!byUmbrella) byUmbrella = iss;
  }
  return byUmbrella;
}

/**
 * Repo-scoped child body — DoR-shaped so a human can dispatch it with
 * `ready-to-implement`, carrying the machine-checkable back-association.
 */
function childIssueBody(item, umbrella) {
  const ref = `${umbrella.repo}#${umbrella.number}`;
  const link = umbrella.url || ref;
  const problem = String(item.summary || '').trim()
    || `Implement this repository's slice of the contract-spanning change tracked by umbrella ${ref}.`;
  return [
    `> 🤖 Auto-filed by AI-SDLC umbrella coordination — **Umbrella:** ${link} — "${umbrella.title}". Edit freely: the coordinator never rewrites filed issues.`,
    '',
    '## Problem',
    problem,
    '',
    '## Context',
    `Part of umbrella ${ref} (key \`${umbrella.key}\`). Sibling repo slices are listed on the umbrella's status comment — keep the shared contract aligned with them.`,
    '',
    '## Acceptance criteria',
    `- [ ] Repository slice of "${item.title}" implemented`,
    `- [ ] Contract compatibility with the shared change verified (see umbrella ${ref})`,
    '- [ ] Repo validation suite passes per project conventions',
    '',
    '---',
    childMarker(item, ref),
  ].join('\n');
}

/**
 * Render the upserted status comment — the live hub checklist. Rows track
 * every declared repo: pending (no child yet) → open → closed. Closed
 * children auto-check; a declared adoption (`o/r#N`) renders its ref.
 */
function statusCommentBody(umbrella, rows, meta = {}) {
  const ref = `${umbrella.repo}#${umbrella.number}`;
  const lines = [
    '### 🌂 Umbrella coordination — per-repo status',
    '',
    `Managed by \`scripts/umbrella-coordinator.js\` — this comment is rewritten each pass; edit the umbrella body or the spec fence, not this comment.`,
    '',
    `**Umbrella:** ${ref} · key \`${umbrella.key}\` · children filed as \`${meta.childLabel || CHILD_LABEL}\` (human applies the dispatch trigger per repo)`,
    '',
  ];
  for (const r of rows) {
    const checked = r.state === 'closed' ? 'x' : ' ';
    if (r.childNumber) {
      lines.push(`- [${checked}] \`${r.repo}#${r.childNumber}\` — ${r.title} (${r.state || 'open'})`);
    } else if (r.note) {
      lines.push(`- [ ] \`${r.repo}\` — ${r.title} (${r.note})`);
    } else {
      lines.push(`- [ ] \`${r.repo}\` — ${r.title} (pending)`);
    }
  }
  if (meta.truncated) lines.push(`- ⚠️ ${meta.truncated} declared repo(s) beyond \`max-repos\` — not coordinated this pass`);
  if (meta.dupes) lines.push(`- ⚠️ ${meta.dupes} duplicate repo declaration(s) ignored (one child per repo)`);
  lines.push('', `Last pass: ${(meta.now || new Date()).toISOString()}`, '', statusMarker(umbrella.key), STATUS_END_MARKER);
  return lines.join('\n');
}

/** Locate the upserted status comment among umbrella comments. */
function findStatusComment(comments, key) {
  return (comments || []).find(c => {
    const m = String(c.body || '').match(STATUS_MARKER_RE);
    return m && (!m[1] || !key || m[1] === key);
  }) || null;
}

// --- Core decision + mutation ----------------------------------------------

/**
 * Coordinate one umbrella issue. `client` supplies the GitHub surface (see
 * ghClient()); tests inject a fake.
 *
 * opts: { repo, issue {number,title,body,labels,url?}, items?, childLabels?,
 *         maxRepos?, dryRun?, force?, now? }
 * Returns { action, key, rows, created[], adopted[], pending[],
 *           truncated, dupes, commentAction, reason? }.
 */
function coordinate(opts, client) {
  const repo = opts.repo;
  const issue = opts.issue;
  const dryRun = !!opts.dryRun;
  const umbrella = { repo, number: issue.number, title: issue.title, url: issue.url, key: umbrellaKey(issue) };
  const umbrellaRef = `${repo}#${issue.number}`;

  if (!opts.force && !hasLabel(issue, opts.umbrellaLabel || UMBRELLA_LABEL)) {
    return { action: 'not-umbrella', reason: `issue lacks the '${opts.umbrellaLabel || UMBRELLA_LABEL}' label`, key: umbrella.key, rows: [], created: [], adopted: [], pending: [] };
  }

  const { items, truncated, dupes } = resolveDeclarations(opts, issue);
  if (!items.length) {
    return { action: 'no-repos', reason: 'no participating repositories declared (fence, bullets, --repos or --spec)', key: umbrella.key, rows: [], created: [], adopted: [], pending: [] };
  }

  const childLabels = Array.isArray(opts.childLabels) ? opts.childLabels
    : String(opts.childLabels || CHILD_LABEL).split(',').map(s => s.trim()).filter(Boolean);

  const rows = [];
  const created = [], adopted = [], pending = [];

  for (const item of items) {
    // Adoption path 1 — the declaration names an existing issue (`o/r#N`).
    if (item.issue) {
      let state = 'open';
      const existing = typeof client.getIssue === 'function'
        ? client.getIssue({ repo: item.repo, number: item.issue }) : null;
      if (existing) state = String(existing.state || 'open').toLowerCase();
      rows.push({ repo: item.repo, title: item.title, childNumber: item.issue, state });
      adopted.push({ repo: item.repo, number: item.issue, declared: true });
      continue;
    }

    // Adoption path 2 — a previously filed child carries the marker.
    const scanned = [
      ...client.listIssues({ repo: item.repo, state: 'open' }),
      ...client.listIssues({ repo: item.repo, state: 'closed' }),
    ];
    const child = findChild(scanned, umbrellaRef, item);
    if (child) {
      rows.push({ repo: item.repo, title: item.title, childNumber: child.number, state: String(child.state || 'open').toLowerCase() });
      adopted.push({ repo: item.repo, number: child.number });
      continue;
    }

    // File the repo slice — marker + parent link, no dispatch trigger.
    if (!dryRun) {
      const made = client.createIssue({
        repo: item.repo,
        title: item.title,
        body: childIssueBody(item, umbrella),
        labels: childLabels,
      });
      rows.push({ repo: item.repo, title: item.title, childNumber: made && made.number, state: 'open' });
      created.push({ repo: item.repo, number: made && made.number, url: made && made.url });
    } else {
      rows.push({ repo: item.repo, title: item.title, childNumber: null, note: 'would file' });
      pending.push({ repo: item.repo });
    }
  }

  // Upsert the single status comment on the umbrella issue.
  const comments = client.listComments({ repo, issueNumber: issue.number });
  const existing = findStatusComment(comments, umbrella.key);
  const body = statusCommentBody(umbrella, rows, {
    childLabel: childLabels[0], truncated, dupes, now: opts.now,
  });
  let commentAction;
  if (existing) {
    commentAction = 'updated';
    if (!dryRun) client.updateComment({ repo, commentId: existing.id, body });
  } else {
    commentAction = 'created';
    if (!dryRun) client.addComment({ repo, issueNumber: issue.number, body });
  }

  return {
    action: dryRun ? 'dry_run' : 'coordinated',
    key: umbrella.key,
    rows, created, adopted, pending,
    truncated, dupes,
    commentAction,
    statusBody: body,
  };
}

// --- gh-backed client --------------------------------------------------------
// Thin shell over `gh` — the same call surface tests fake. All reads bounded.

function ghClient() {
  const run = (args, allowFail = false) => {
    try {
      return execFileSync('gh', args, {
        encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 30000,
      });
    } catch (e) {
      if (allowFail) return '';
      throw e;
    }
  };
  const bodyFile = (body, prefix) => {
    const f = `/tmp/ai_sdlc_umbrella_${prefix}_${Date.now()}_${Math.random().toString(36).slice(2, 8)}.md`;
    fs.writeFileSync(f, body, 'utf8');
    return f;
  };
  return {
    getIssue({ repo, number }) {
      const out = run(['issue', 'view', String(number), '--repo', repo,
        '--json', 'number,title,body,labels,url,state'], true);
      try { return out ? JSON.parse(out) : null; }
      catch (e) { console.warn(`[!] gh issue view #${number} failed for ${repo} — treating as missing`); return null; }
    },
    listIssues({ repo, state }) {
      const out = run(['issue', 'list', '--repo', repo, '--state', state,
        '--limit', String(MAX_ISSUES),
        '--json', 'number,title,body,url,state'], true);
      try { return out ? JSON.parse(out) : []; }
      catch (e) { console.warn(`[!] gh issue list (${state}) failed for ${repo} — treating as empty`); return []; }
    },
    listComments({ repo, issueNumber }) {
      const all = [];
      for (let page = 1; page <= MAX_COMMENT_PAGES; page++) {
        const out = run(['api', `repos/${repo}/issues/${issueNumber}/comments?per_page=${COMMENTS_PER_PAGE}&page=${page}`,
          '--jq', '.[] | {id, created_at, body}'], true);
        if (!out.trim()) break;
        const rows = out.trim().split('\n').map(l => { try { return JSON.parse(l); } catch (e) { return null; } }).filter(Boolean);
        all.push(...rows);
        if (rows.length < COMMENTS_PER_PAGE) break;
      }
      return all;
    },
    addComment({ repo, issueNumber, body }) {
      const f = bodyFile(body, 'comment');
      try {
        run(['issue', 'comment', String(issueNumber), '--repo', repo, '--body-file', f]);
      } finally { try { fs.unlinkSync(f); } catch (e) {} }
    },
    updateComment({ repo, commentId, body }) {
      const f = bodyFile(JSON.stringify({ body }), 'patch');
      try {
        run(['api', '-X', 'PATCH', `repos/${repo}/issues/comments/${commentId}`, '--input', f]);
      } finally { try { fs.unlinkSync(f); } catch (e) {} }
    },
    createIssue({ repo, title, body, labels }) {
      const f = bodyFile(body, 'issue');
      try {
        let out = '';
        const base = ['issue', 'create', '--repo', repo, '--title', title, '--body-file', f];
        try {
          out = run([...base, '--label', (labels || []).join(',')]);
        } catch (e) {
          // Label fallback: a repo missing a label must not lose the filing.
          console.warn(`[!] issue create with labels failed (${String(e.message).split('\n')[0]}) — retrying without labels`);
          out = run(base);
        }
        const m = out.trim().match(/\/issues\/(\d+)/);
        return { number: m ? parseInt(m[1], 10) : null, url: out.trim() };
      } finally { try { fs.unlinkSync(f); } catch (e) {} }
    },
  };
}

// --- CLI ---------------------------------------------------------------------

function parseArgs(argv) {
  const opts = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const k = a.slice(2);
      if (['dry-run', 'json', 'github-output', 'force'].includes(k)) opts[k] = true;
      else opts[k] = argv[++i];
    } else opts._.push(a);
  }
  return opts;
}

function specItems(a) {
  // --repos csv OR --spec file: explicit declaration wins over the body.
  if (a.repos) {
    return String(a.repos).split(',').map(s => s.trim()).filter(Boolean);
  }
  if (a.spec) {
    const doc = JSON.parse(fs.readFileSync(a.spec, 'utf8'));
    return Array.isArray(doc) ? doc : (doc.items || doc.repos || doc.repositories || []);
  }
  return null;
}

function main() {
  const a = parseArgs(process.argv.slice(2));
  const issueNum = parseInt(a.issue || '', 10);
  if (!a.repo || !REPO_RE.test(a.repo) || !Number.isFinite(issueNum)) {
    console.error('usage: umbrella-coordinator.js --repo OWNER/NAME --issue N [--repos o/a,o/b|--spec spec.json] [--child-labels csv] [--max-repos N] [--force] [--dry-run] [--json] [--github-output]');
    process.exit(2);
  }
  const client = ghClient();
  const issue = client.getIssue({ repo: a.repo, number: issueNum });
  if (!issue) {
    console.error(`[umbrella] could not fetch ${a.repo}#${issueNum}`);
    process.exit(1);
  }
  const maxRepos = parseInt(a['max-repos'] || process.env.UMBRELLA_MAX_REPOS || '', 10);
  const result = coordinate({
    repo: a.repo,
    issue,
    items: specItems(a) || undefined,
    childLabels: a['child-labels'],
    maxRepos: Number.isFinite(maxRepos) ? maxRepos : undefined,
    dryRun: !!a['dry-run'],
    force: !!a.force,
  }, client);

  console.error(`[umbrella] ${result.action} key=${result.key}` +
    ` children=${result.created.length} filed/${result.adopted.length} adopted` +
    `${result.commentAction ? ` status=${result.commentAction}` : ''}` +
    `${result.reason ? ` reason="${result.reason}"` : ''}`);
  if (a['github-output'] && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT,
      `umbrella_action=${result.action}\numbrella_key=${result.key || ''}\numbrella_children=${result.created.length}\n`);
  }
  if (a.json || a['github-output']) console.log(JSON.stringify(result));
  if (result.action === 'not-umbrella' || result.action === 'no-repos') process.exit(1);
}

if (require.main === module) {
  main();
}

module.exports = {
  UMBRELLA_LABEL,
  CHILD_LABEL,
  DEFAULT_MAX_REPOS,
  MAX_ISSUES,
  IDENTITY_RE,
  STATUS_MARKER_RE,
  CHILD_MARKER_RE,
  slugifyKey,
  itemKeyFor,
  isRepoRef,
  normalizeChildTitle,
  normalizeItem,
  normalizeItems,
  parseUmbrellaSpec,
  declaredItems,
  umbrellaKey,
  hasLabel,
  resolveDeclarations,
  childMarker,
  statusMarker,
  findChild,
  childIssueBody,
  statusCommentBody,
  findStatusComment,
  coordinate,
};
