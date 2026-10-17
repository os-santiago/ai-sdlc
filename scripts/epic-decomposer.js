#!/usr/bin/env node
// scripts/epic-decomposer.js — epic → scc:child sub-issues (issue #198, epic #147)
//
// Deterministic decomposition for the GHA-native runtime: an issue carrying
// the `epic` label is split into a BOUNDED set of child issues, each filed
// with `scc:child`, a Problem/Context/Acceptance-criteria body, and a
// hidden parent marker. The epic body gains a live managed checklist that
// links every child and tracks open/closed state on re-runs.
//
// Child sources (in precedence order):
//   1. --children-file JSON — an explicit plan produced upstream (e.g. an
//      engine-driven refinement): [{title, problem?, context?, ac?[]}].
//   2. Work-item sections in the epic body — `## Scope`, `## Children`,
//      `## Child issues`, `## Sub-issues`, `## Work items`, `## Tasks`,
//      `## Deliverables`, `## Decomposition`, `## Breakdown`, `## Plan`,
//      `## Implementation plan`. Each top-level list item becomes a child;
//      deeper-indented bullets under it become that child's AC items.
//      Items that are already issue refs (`#N`, `owner/repo#N`, or a link
//      to /issues/N) are ADOPTED into the checklist — never re-filed.
//   3. Fallback: the epic's own `## Acceptance criteria` checklist — one
//      child per item — so a DoR-complete epic always decomposes.
//
// Idempotency: every filed child body ends with
//   <!-- hermes:epic-child key:"<slug>" parent:#<epic> -->
// the same marker convention the Hermes control plane already stamps on
// this repo's children (see the scc:child issues under #147/#220). Re-runs
// index markers across open+closed issues and never re-file a key that
// exists — so the decomposer is safe to run on every `labeled` event.
//
// Live checklist: a managed block delimited by
//   <!-- hermes:epic-decomposition --> … <!-- /hermes:epic-decomposition -->
// is upserted into the epic body. Marked checklist lines are kept verbatim
// (human edits inside the block survive), unmarked lines are preserved,
// checkbox state follows each child's open/closed state, and lines for
// newly-filed keys are appended.
//
// Children never carry a dispatch trigger label — `scc:child` is itself a
// skip label (spec/labels.md: terminal, never dispatch); a human arms each
// child independently.
//
// Trust boundary: fetch and run this script only from a trusted runtime
// ref (ai-sdlc-intake.yml pins inputs.runtime_ref) — never from a PR head
// or the consumer checkout.
//
// CLI:
//   node scripts/epic-decomposer.js --repo OWNER/NAME --issue N \
//     [--children-file F] [--max-children N] [--epic-label L] \
//     [--dry-run] [--json] [--github-output]
'use strict';

const fs = require('fs');
const { execFileSync } = require('child_process');

const EPIC_LABEL = 'epic';
const CHILD_LABEL = 'scc:child';
const DEFAULT_MAX_CHILDREN = 8;
const MAX_ISSUES = 100;            // per-state scan bound (bounded reads)
const KEY_MAX = 48;                // matches existing hermes-stamped keys
const TITLE_MAX = 200;             // GitHub caps titles at 256 — keep margin

const BLOCK_START = '<!-- hermes:epic-decomposition -->';
const BLOCK_END = '<!-- /hermes:epic-decomposition -->';
const BLOCK_HEADING = '### 🔗 Sub-issues (auto-decomposed — edits inside this block survive regeneration)';

// Marker formats — MUST match the strings already stamped on this repo's
// scc:child issues and epics (continuity with the Hermes-filed batches).
const CHILD_MARKER_RE = /<!--\s*hermes:epic-child\s+key:"?([A-Za-z0-9_.-]+)"?\s+parent:#(\d+)\s*-->/g;
const SUB_MARKER_RE = /<!--\s*hermes:sub:([A-Za-z0-9_.-]+)\s*-->/;
const ISSUE_NUM_RE = /#(\d+)\b/;

// Title-overlap adoption (Hermes #381 lesson): children filed by an earlier
// tool or an upstream engine may carry different marker keys for the same
// work item. Before filing, a spec is adopted when its title's content
// words are ≥ ADOPT_THRESHOLD contained in an existing tracked child title
// (managed-block entry or parent-marked issue). 0 disables fuzzy adoption.
const DEFAULT_ADOPT_THRESHOLD = 0.4;
const STOPWORDS = new Set(['a', 'an', 'the', 'to', 'of', 'in', 'on', 'for', 'with', 'and', 'or', 'per', 'via', 'into', 'over', 'get']);

// Work-item section headings — an epic's decomposition surface.
const SECTION_HEADING_RE = /^#{1,6}\s+(?:\*\*)?(scope|children|child issues|sub-?issues?|work items|task list|tasks|deliverables|decomposition|breakdown|implementation plan|plan)\b/i;
const AC_HEADING_RE = /^#{1,6}\s+(?:\*\*)?(acceptance criteria|ac)\b/i;
const HEADING_RE = /^(#{1,6})\s+/;
const LIST_ITEM_RE = /^(\s*)(?:[-*+]|\d+\.)\s+(?:\[(?: |x|X)\]\s+)?(.*\S)\s*$/;
// Items that already point at a filed issue are adopted, not re-filed.
const ISSUE_REF_RE = /^(?:\[[^\]]*\]\([^)]*\/issues\/(\d+)[^)]*\)|[-A-Za-z0-9_.]+\/[-A-Za-z0-9_.]+#(\d+)|#(\d+))\b/;

function hasLabel(issue, name) {
  const want = String(name || '').toLowerCase();
  return ((issue && issue.labels) || []).some(
    (l) => String((l && l.name) || l).toLowerCase() === want);
}

/** Lowercase hyphen slug, ≤KEY_MAX chars, no leading/trailing dash. */
function slugifyKey(text) {
  const key = String(text || '')
    .toLowerCase()
    .replace(/\[[^\]]*\]\([^)]*\)/g, '$1')   // [label](url) → label
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/-{2,}/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, KEY_MAX)
    .replace(/-+$/g, '');
  return key || 'child';
}

/** The canonical parent marker stamped on every filed child. */
function childMarker(key, parent) {
  return `<!-- hermes:epic-child key:"${key}" parent:#${parent} -->`;
}

function subMarker(key) {
  return `<!-- hermes:sub:${key} -->`;
}

/** All {key, parent} child markers in a body — [{key, parent}] in order. */
function parseChildMarkers(body) {
  const out = [];
  const text = String(body || '');
  let m;
  CHILD_MARKER_RE.lastIndex = 0;
  while ((m = CHILD_MARKER_RE.exec(text)) !== null) {
    out.push({ key: m[1], parent: parseInt(m[2], 10) });
  }
  return out;
}

/** Marker key a known child issue carries for `parent`, or null. */
function markerKeyOf(issue, parent) {
  const hit = parseChildMarkers(issue && issue.body)
    .find((m) => m.parent === parent);
  return hit ? hit.key : null;
}

/** Content words of a title: lowercase alnum tokens minus stopwords. */
function titleWords(text) {
  return new Set(
    stripMarkdown(text).toLowerCase()
      .replace(/[^a-z0-9]+/g, ' ')
      .split(' ')
      .filter((w) => w.length > 1 && !STOPWORDS.has(w)));
}

/** Fraction of `a`'s content words present in `b` — 0..1. */
function wordContainment(a, b) {
  const A = titleWords(a);
  if (!A.size) return 0;
  const B = titleWords(b);
  let hit = 0;
  for (const w of A) if (B.has(w)) hit += 1;
  return hit / A.size;
}

/**
 * Checklist entries already tracked in the epic's managed block —
 * [{number?, key?, title}] — the authoritative "already decomposed"
 * surface across tools (Hermes and this decomposer share the format).
 */
function parseManagedBlockEntries(body) {
  const text = String(body || '');
  const start = text.indexOf(BLOCK_START);
  const end = text.indexOf(BLOCK_END);
  if (start === -1 || end === -1 || end < start) return [];
  return text.slice(start + BLOCK_START.length, end).split('\n')
    .map((line) => {
      const num = line.match(ISSUE_NUM_RE);
      const key = line.match(SUB_MARKER_RE);
      if (!num && !key) return null;
      const title = line
        .replace(SUB_MARKER_RE, '')
        .replace(/^\s*[-*+]\s*\[(?: |x|X)\]\s*/, '')
        .replace(/^\s*[-*+]\s*/, '')
        .replace(/^#\d+\s*—?\s*/, '')
        .trim();
      return { number: num ? parseInt(num[1], 10) : null, key: key ? key[1] : null, title };
    })
    .filter((e) => e && (e.number || e.key));
}

function stripMarkdown(text) {
  return String(text || '')
    .replace(/\[[^\]]*\]\(([^)]*)\)/g, '$1') // links → bare target keeps the ref visible
    .replace(/[`*]/g, '')
    .replace(/\s+/g, ' ')
    .trim();
}

function cleanTitle(text) {
  let t = stripMarkdown(text).replace(/[.。…]+$/g, '').trim();
  if (t.length > TITLE_MAX) t = t.slice(0, TITLE_MAX).replace(/\s+\S*$/, '').trim();
  return t || 'decomposed work item';
}

/**
 * Strip the managed checklist block so regen never re-reads its own lines
 * as scope items. Returns body with the block removed.
 */
function stripManagedBlock(body) {
  const text = String(body || '');
  const start = text.indexOf(BLOCK_START);
  const end = text.indexOf(BLOCK_END);
  if (start === -1 || end === -1 || end < start) return text;
  return text.slice(0, start) + text.slice(end + BLOCK_END.length);
}

/**
 * Parse the epic body for work-item sections. Returns items in order:
 *   {kind:'spec', text, ac:[...], section}  — file as a child issue
 *   {kind:'ref',  number, text, section}    — adopt into the checklist
 */
function extractWorkItems(body) {
  const lines = stripManagedBlock(body).split('\n');
  const items = [];
  let section = null;        // current work-item section name
  let sectionDepth = 0;      // heading level that opened it
  let topIndent = null;      // indent of the section's first list item
  let lastSpec = null;       // spec currently collecting nested AC bullets

  const closeSection = () => { section = null; topIndent = null; lastSpec = null; };

  for (const line of lines) {
    const h = line.match(HEADING_RE);
    if (h) {
      const name = line.replace(HEADING_RE, '').trim();
      if (SECTION_HEADING_RE.test(line)) {
        section = name;
        sectionDepth = h[1].length;
      } else if (section && h[1].length <= sectionDepth) {
        closeSection();
      }
      continue;
    }
    if (!section) continue;
    if (line.trim() === '' || /^\s*---+\s*$/.test(line)) { lastSpec = null; continue; }

    const li = line.match(LIST_ITEM_RE);
    if (!li) { continue; }
    const indent = li[1].replace(/\t/g, '  ').length;
    const text = li[2].trim();
    if (!text || /\b(?:TBD|TODO)\b/i.test(text) && stripMarkdown(text).length <= 12) continue;
    if (SUB_MARKER_RE.test(text) || /hermes:epic-child/.test(text)) continue;

    if (topIndent === null) topIndent = indent;
    if (indent > topIndent && lastSpec) {
      // Nested bullet → acceptance criterion of the current spec.
      lastSpec.ac.push(text);
      continue;
    }
    lastSpec = null;
    if (indent > topIndent) continue;   // nested bullet with no live parent

    const ref = text.match(ISSUE_REF_RE);
    if (ref) {
      const number = parseInt(ref[1] || ref[2] || ref[3], 10);
      items.push({ kind: 'ref', number, text, section });
      continue;
    }
    const spec = { kind: 'spec', text, ac: [], section };
    items.push(spec);
    lastSpec = spec;
  }
  return items;
}

/** Fallback: the epic's own AC checklist — one child per item. */
function extractAcItems(body) {
  const lines = stripManagedBlock(body).split('\n');
  const items = [];
  let inAc = false;
  let acDepth = 0;
  for (const line of lines) {
    const h = line.match(HEADING_RE);
    if (h) {
      if (AC_HEADING_RE.test(line)) { inAc = true; acDepth = h[1].length; continue; }
      if (inAc && h[1].length <= acDepth) inAc = false;
      continue;
    }
    if (!inAc) continue;
    const li = line.match(LIST_ITEM_RE);
    if (!li) continue;
    const text = li[2].trim();
    if (text) items.push({ kind: 'spec', text, ac: [], section: 'Acceptance criteria' });
  }
  return items;
}

/**
 * Normalize an explicit upstream plan (--children-file): array of strings
 * or {title, problem?, context?, ac?[]}. Invalid entries are dropped.
 */
function normalizePlan(children) {
  const rows = Array.isArray(children) ? children : [];
  const items = [];
  for (const raw of rows) {
    if (typeof raw === 'string') {
      const text = raw.trim();
      if (text) items.push({ kind: 'spec', text, ac: [], section: 'plan' });
      continue;
    }
    if (!raw || typeof raw !== 'object') continue;
    const title = String(raw.title || '').trim();
    if (!title) continue;
    items.push({
      kind: 'spec',
      text: title,
      problem: String(raw.problem || '').trim(),
      context: String(raw.context || '').trim(),
      ac: (Array.isArray(raw.ac) ? raw.ac : []).map((s) => String(s).trim()).filter(Boolean),
      section: 'plan',
    });
  }
  return items;
}

/** Turn one work item into a fileable child spec {key, title, problem, context, ac[]}. */
function childSpec(item, epic) {
  const title = cleanTitle(item.text);
  const epicTitle = stripMarkdown(epic.title || '').replace(/"/g, "'");
  const partOf = `Part of epic #${epic.number} — "${epicTitle}".`;
  const ac = item.ac && item.ac.length
    ? item.ac.map(stripMarkdown).filter(Boolean)
    : [
        stripMarkdown(item.text).replace(/[.。…]+$/g, ''),
        'Changes land green against the repo validation suite (see `.ai-sdlc.yaml` verify.commands)',
      ];
  return {
    key: slugifyKey(title),
    title,
    problem: item.problem || stripMarkdown(item.text),
    context: (item.context
      || `Decomposed from the epic${item.section && item.section !== 'plan' ? `'s "${stripMarkdown(item.section)}" section` : ''} — one bounded slice; the parent tracks the live checklist.`)
      + `\n\n${partOf}`,
    ac,
  };
}

function childBody({ epic, spec }) {
  return [
    `> 🤖 Auto-decomposed by AI-SDLC — **Parent epic:** #${epic.number}. Edit freely: the decomposer never rewrites child issues.`,
    '',
    '## Problem',
    spec.problem,
    '',
    '## Context',
    spec.context,
    '',
    '## Acceptance criteria',
    ...spec.ac.map((a) => `- [ ] ${a}`),
    '',
    '---',
    childMarker(spec.key, epic.number),
  ].join('\n');
}

function checklistLine(entry) {
  const check = entry.state === 'closed' ? 'x' : ' ';
  const ref = entry.number ? `#${entry.number} — ` : '';
  return `- [${check}] ${ref}${entry.title} ${subMarker(entry.key)}`;
}

/**
 * Upsert the managed checklist block in the epic body.
 * - no block → append a fresh one
 * - block present → keep every existing line verbatim (human edits survive),
 *   refresh checkbox state on lines referencing a known child number, and
 *   append lines for entries whose key/number the block doesn't carry yet.
 */
function upsertChecklistBlock(body, entries) {
  const text = String(body || '');
  const stateByNumber = {};
  for (const e of entries) if (e.number) stateByNumber[e.number] = e.state;

  const start = text.indexOf(BLOCK_START);
  const end = text.indexOf(BLOCK_END);
  if (start === -1 || end === -1 || end < start) {
    const block = [BLOCK_START, '', BLOCK_HEADING, '',
      ...entries.map(checklistLine), '', BLOCK_END].join('\n');
    return `${text.trimEnd()}\n\n${block}\n`;
  }

  const knownKeys = new Set();
  const knownNumbers = new Set();
  const inner = text.slice(start + BLOCK_START.length, end);
  const lines = inner.split('\n').map((line) => {
    const sm = line.match(SUB_MARKER_RE);
    if (sm) knownKeys.add(sm[1]);
    const nm = line.match(ISSUE_NUM_RE);
    if (nm) {
      knownNumbers.add(parseInt(nm[1], 10));
      const state = stateByNumber[nm[1]];
      if (state && /^(\s*[-*+]\s*)\[( |x|X)\]/.test(line)) {
        return line.replace(/^(\s*[-*+]\s*)\[( |x|X)\]/, `$1[${state === 'closed' ? 'x' : ' '}]`);
      }
    }
    return line;
  });

  const additions = entries
    .filter((e) => !knownKeys.has(e.key) && !(e.number && knownNumbers.has(e.number)))
    .map(checklistLine);
  if (additions.length) {
    // Splice before the trailing blank lines so the block ends tidily.
    let idx = lines.length;
    while (idx > 0 && lines[idx - 1].trim() === '') idx -= 1;
    lines.splice(idx, 0, ...additions);
  }
  return text.slice(0, start + BLOCK_START.length) + lines.join('\n') + text.slice(end);
}

// --- Core flow ---------------------------------------------------------------

/**
 * Decompose one epic issue. `client` supplies the GitHub surface (see
 * ghClient()); tests inject a fake.
 * Returns {action, reason?, epic, children[], checklistUpdated} — actions:
 *   skipped | decomposed | synced | dry_run.
 */
function decomposeEpic(opts, client) {
  const repo = opts.repo;
  const epicLabel = opts.epicLabel || EPIC_LABEL;
  const maxChildren = Number.isFinite(opts.maxChildren) && opts.maxChildren > 0
    ? opts.maxChildren : DEFAULT_MAX_CHILDREN;
  const dryRun = !!opts.dryRun;

  const epic = opts.issue || client.getIssue({ repo, issueNumber: opts.issueNumber });
  const labels = (epic.labels || []).map((l) => String((l && l.name) || l));
  if (!hasLabel(epic, epicLabel)) {
    return { action: 'skipped', reason: `issue lacks the '${epicLabel}' label`, epic: epic.number, children: [] };
  }
  if (String(epic.state || '').toLowerCase() === 'closed') {
    return { action: 'skipped', reason: 'epic is closed', epic: epic.number, children: [] };
  }

  let items = opts.children ? normalizePlan(opts.children) : extractWorkItems(epic.body);
  if (!items.some((i) => i.kind === 'spec' || i.kind === 'ref')) {
    items = extractAcItems(epic.body);
  }
  items = items.slice(0, maxChildren);
  if (!items.length) {
    return { action: 'skipped', reason: 'no decomposition input (no work-item section and no AC items)', epic: epic.number, children: [] };
  }

  // Marker index across open + closed children — a filed key is never
  // re-filed, even if the child was since closed (bounded, 100/state).
  const existing = [
    ...client.listIssues({ repo, state: 'open' }),
    ...client.listIssues({ repo, state: 'closed' }),
  ].sort((a, b) => a.number - b.number);
  const markerIndex = {};   // key → lowest-numbered child issue
  const byNumber = {};      // number → issue
  for (const i of existing) {
    byNumber[i.number] = i;
    for (const m of parseChildMarkers(i.body)) {
      if (m.parent === epic.number && !markerIndex[m.key]) markerIndex[m.key] = i;
    }
  }

  const inherit = labels.filter((l) => /^priority:P[0-3]$/i.test(l));
  const childLabels = [...new Set([CHILD_LABEL, ...inherit])];

  // Adoption pool: entries already tracked on the epic (managed block) plus
  // parent-marked issues. Exact key wins; fuzzy title containment next.
  const adoptThreshold = Number.isFinite(opts.adoptThreshold)
    ? opts.adoptThreshold : DEFAULT_ADOPT_THRESHOLD;
  const blockEntries = parseManagedBlockEntries(epic.body);
  const adoptSpec = (spec) => {
    const byKey = blockEntries.find((e) => e.key && e.key === spec.key);
    if (byKey) return byKey;
    if (adoptThreshold <= 0 || adoptThreshold >= 1) return null;
    const pool = [
      ...blockEntries,
      ...Object.values(markerIndex).map((i) => ({ number: i.number, title: i.title })),
    ];
    return pool.find((p) => p.title && wordContainment(spec.title, p.title) >= adoptThreshold) || null;
  };
  // Keys claimed by prior filings (marker index) plus this run's new ones —
  // only consulted when filing a NEW child; an exact marker hit short-
  // circuits to 'existing' before any uniquifying (idempotent re-runs).
  const usedKeys = new Set(Object.keys(markerIndex));
  const uniqueKey = (base) => {
    let k = base;
    for (let n = 2; usedKeys.has(k); n += 1) k = `${base.slice(0, KEY_MAX - 2)}-${n}`;
    usedKeys.add(k);
    return k;
  };

  const children = [];
  let filed = 0;
  for (const item of items) {
    if (item.kind === 'ref') {
      const target = byNumber[item.number];
      children.push({
        status: 'linked', number: item.number,
        key: (target && markerKeyOf(target, epic.number))
          || slugifyKey((target && target.title) || item.text),
        title: (target && target.title) || stripMarkdown(item.text),
        state: (target && target.state) || 'unknown',
      });
      continue;
    }
    const spec = childSpec(item, epic);
    const prior = markerIndex[spec.key];
    if (prior) {
      children.push({ status: 'existing', number: prior.number, key: spec.key, title: prior.title, state: prior.state, url: prior.url });
      continue;
    }
    // Adoption before filing: an already-tracked child (managed-block entry
    // or parent-marked issue) covering this spec's title is linked, never
    // duplicated — same scope, different key lineage.
    const adopted = adoptSpec(spec);
    if (adopted) {
      const known = adopted.number ? byNumber[adopted.number] : null;
      children.push({
        status: 'adopted', number: adopted.number || null,
        key: adopted.key || spec.key,
        title: (known && known.title) || adopted.title || spec.title,
        state: (known && known.state) || 'unknown',
        url: known && known.url,
      });
      continue;
    }
    spec.key = uniqueKey(spec.key);
    if (dryRun) {
      children.push({ status: 'dry_run_file', key: spec.key, title: spec.title });
      continue;
    }
    if (filed === 0 && typeof client.ensureLabel === 'function') {
      client.ensureLabel({ repo, label: CHILD_LABEL });
    }
    const created = client.createIssue({
      repo, title: spec.title,
      body: childBody({ epic, spec }),
      labels: childLabels,
    });
    filed += 1;
    const rec = { number: created.number, title: spec.title, state: 'open', url: created.url, body: childBody({ epic, spec }) };
    byNumber[created.number] = rec;
    markerIndex[spec.key] = rec;
    children.push({ status: 'filed', number: created.number, key: spec.key, title: spec.title, state: 'open', url: created.url });
  }

  // Live checklist on the epic — refresh only when the body would change.
  // Dedupe by number and key: an adopted ref and a marker hit can resolve to
  // the same child and must render as ONE line.
  const seenNumbers = new Set();
  const seenKeys = new Set();
  const entries = [];
  for (const c of children) {
    if (c.number && seenNumbers.has(c.number)) continue;
    if (seenKeys.has(c.key)) continue;
    if (c.number) seenNumbers.add(c.number);
    seenKeys.add(c.key);
    entries.push({ key: c.key, number: c.number, title: c.title, state: c.state });
  }
  const newBody = upsertChecklistBlock(epic.body, entries);
  const changed = newBody !== String(epic.body || '');
  if (changed && !dryRun && typeof client.updateIssueBody === 'function') {
    client.updateIssueBody({ repo, issueNumber: epic.number, body: newBody });
  }

  return {
    action: dryRun ? 'dry_run' : (filed ? 'decomposed' : 'synced'),
    epic: epic.number,
    childrenFiled: filed,
    children,
    checklistUpdated: changed && !dryRun,
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
  const bodyFile = (body) => {
    const f = `/tmp/ai_sdlc_epic_${Date.now()}_${Math.random().toString(36).slice(2, 8)}.md`;
    fs.writeFileSync(f, body, 'utf8');
    return f;
  };
  return {
    getIssue({ repo, issueNumber }) {
      return JSON.parse(run([
        'issue', 'view', String(issueNumber), '--repo', repo,
        '--json', 'number,title,body,state,labels,url',
      ]));
    },
    listIssues({ repo, state }) {
      const out = run(['issue', 'list', '--repo', repo, '--state', state,
        '--limit', String(MAX_ISSUES),
        '--json', 'number,title,body,state,labels,url'], true);
      try { return out ? JSON.parse(out) : []; }
      catch (e) { console.warn(`[!] gh issue list (${state}) failed for ${repo} — treating as empty`); return []; }
    },
    ensureLabel({ repo, label }) {
      run(['label', 'create', label, '--repo', repo, '--color', '1D76DB',
        '--description', 'AI-SDLC: epic-decomposed sub-issue'], true);
    },
    createIssue({ repo, title, body, labels }) {
      const f = bodyFile(body);
      try {
        let out = '';
        const base = ['issue', 'create', '--repo', repo, '--title', title, '--body-file', f];
        try {
          out = run([...base, '--label', (labels || []).join(',')]);
        } catch (e) {
          // Label fallback: a repo missing the label must not lose the child.
          console.warn(`[!] issue create with labels failed (${String(e.message).split('\n')[0]}) — retrying without labels`);
          out = run(base);
        }
        const m = out.trim().match(/\/issues\/(\d+)/);
        return { number: m ? parseInt(m[1], 10) : null, url: out.trim() };
      } finally { try { fs.unlinkSync(f); } catch (e) {} }
    },
    updateIssueBody({ repo, issueNumber, body }) {
      const f = bodyFile(body);
      try {
        run(['issue', 'edit', String(issueNumber), '--repo', repo, '--body-file', f]);
      } finally { try { fs.unlinkSync(f); } catch (e) {} }
    },
  };
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: epic-decomposer.js --repo OWNER/NAME --issue N',
    '                          [--children-file F] [--max-children N]',
    '                          [--epic-label L] [--adopt-threshold F]',
    '                          [--dry-run] [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = {};
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--issue') opts.issue = next();
    else if (a === '--children-file') opts.childrenFile = next();
    else if (a === '--max-children') opts.maxChildren = Number(next());
    else if (a === '--epic-label') opts.epicLabel = next();
    else if (a === '--adopt-threshold') opts.adoptThreshold = Number(next());
    else if (a === '--dry-run') opts.dryRun = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv) {
  const a = parseArgs(argv);
  if (!a.repo || !/^[-A-Za-z0-9_.]+\/[-A-Za-z0-9_.]+$/.test(a.repo) || !a.issue) {
    throw new Error(usage());
  }
  let children = null;
  if (a.childrenFile) {
    children = JSON.parse(fs.readFileSync(a.childrenFile, 'utf8'));
  }
  const result = decomposeEpic({
    repo: a.repo,
    issueNumber: parseInt(a.issue, 10),
    children,
    maxChildren: a.maxChildren || parseInt(process.env.EPIC_DECOMPOSE_MAX_CHILDREN || '', 10),
    epicLabel: a.epicLabel || process.env.EPIC_LABEL || EPIC_LABEL,
    adoptThreshold: Number.isFinite(a.adoptThreshold)
      ? a.adoptThreshold
      : parseFloat(process.env.EPIC_DECOMPOSE_ADOPT_THRESHOLD || ''),
    dryRun: !!a.dryRun || process.env.EPIC_DECOMPOSER_DRY_RUN === '1',
  }, ghClient());

  console.error(`[epic-decomposer] ${result.action} epic=#${result.epic}` +
    ` filed=${result.childrenFiled || 0}` +
    ` children=${result.children.length}${result.reason ? ` reason="${result.reason}"` : ''}`);
  if (a.githubOutput && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT, [
      `epic_action=${result.action}`,
      `children_filed=${result.childrenFiled || 0}`,
      `children_total=${result.children.length}`,
    ].join('\n') + '\n');
  }
  if (a.json || a.githubOutput) console.log(JSON.stringify(result));
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`epic-decomposer: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  EPIC_LABEL,
  CHILD_LABEL,
  DEFAULT_MAX_CHILDREN,
  DEFAULT_ADOPT_THRESHOLD,
  BLOCK_START,
  BLOCK_END,
  BLOCK_HEADING,
  slugifyKey,
  childMarker,
  subMarker,
  parseChildMarkers,
  markerKeyOf,
  titleWords,
  wordContainment,
  parseManagedBlockEntries,
  stripMarkdown,
  stripManagedBlock,
  extractWorkItems,
  extractAcItems,
  normalizePlan,
  childSpec,
  childBody,
  checklistLine,
  upsertChecklistBlock,
  decomposeEpic,
  main,
};
