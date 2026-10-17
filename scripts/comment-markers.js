// scripts/comment-markers.js — canonical registry of the pipeline's
// comment markers (issue #131).
//
// The merge/review gate is steered by HTML-comment markers left on PRs and
// issues (`<!-- ai-sdlc:* -->`). Emitters live inside workflow `run:` blocks
// and composite actions; parsers are inline JS regexes and jq `match` /
// `capture` patterns. Nothing today pins those formats: a refactor that
// changes marker syntax breaks gating silently — merges stall or proceed
// un-reviewed with no failing check to explain why.
//
// This module is the single declarative description of every gating marker:
//   - template / emit()      canonical output shape (what producers write)
//   - parse                  canonical parser (JS regex + ordered fields)
//   - emitSites[]            verbatim text pins locating each producer
//   - parseSites[]           verbatim pins locating each consumer:
//       kind 'js-regex'   — the regex literal must appear in the file
//       kind 'jq-capture' — a jq capture("…") pattern, verbatim
//       kind 'jq-match'   — a jq match("…") pattern, verbatim
//       trusted           — the site's comment feed is filtered to
//                           Bot / OWNER / MEMBER / COLLABORATOR authors;
//                           the test pins the jq select clause so a forged
//                           comment can never steer the gate
//   - codePins[]           verbatim pins for code DUPLICATED across inline
//                          workflow scripts — a one-sided refactor of a
//                          shared recipe (e.g. the DoR surface hash) is a
//                          silent security drift, so both copies are pinned
//                          byte-for-byte here
//
// tests/comment-markers.test.js replays emit→parse round-trips and negative
// cases from tests/markers/cases/*.json against THIS module while asserting
// the shipped emit/parse text in the workflows matches byte-for-byte — the
// same "replay the real code, never a copy" posture as
// tests/regression_harness.js.
//
// Scope: the review/verdict gating surface (issue #131) plus the
// intake→implement DoR surface pin (`ai-sdlc:dor`, issue #122). Other marker
// families live in their own modules with dedicated unit tests —
// `ai-sdlc:refinement` (scripts/intake-refiner.js), `ai-sdlc:dup-scan`
// (scripts/dup-detector.js), `ai-sdlc:alert-route` (scripts/alert-router.js),
// `ai-sdlc:main-guard*` (scripts/main-guard.js), `ai-sdlc:label-veto`
// (scripts/label-contradiction.js), `ai-sdlc:dep-flagged` (dep-update dedup),
// `ai-sdlc:runbook-entry` (scripts/failure-runbook.js).
'use strict';

const REVIEW_RUN_YML = '.github/workflows/ai-sdlc-review-run.yml';
const REVIEW_FIX_YML = '.github/workflows/ai-sdlc-review-fix.yml';
const AUTOMERGE_YML = '.github/workflows/ai-sdlc-automerge.yml';
const CI_REPAIR_YML = '.github/workflows/ai-sdlc-ci-repair.yml';
const INTAKE_YML = '.github/workflows/ai-sdlc-intake.yml';
const IMPLEMENT_YML = '.github/workflows/ai-sdlc-implement.yml';
const RUN_LEDGER_ACTION = '.github/actions/run-ledger/action.yml';

// jq clause every trusted-author comment feed carries — forged comments
// from arbitrary authors must never steer gating state.
const TRUSTED_AUTHOR_FILTER = 'IN("OWNER","MEMBER","COLLABORATOR")';

// --- Placeholder masking ------------------------------------------------------

/**
 * Mask substitution placeholders in a marker template to `{*}` so the
 * *static shape* of two templates can be compared:
 *   `${expr}` / `${expr ? `inner` : ''}` / `$VAR` / `%s` / `%d` / `{name}`
 * The ternary `X ? `tpl` : ''` (optional-segment idiom used for the
 * ci-repair `sig` field) expands to the masked inner template — the
 * optional segment still has to keep its literal keywords.
 */
function maskPlaceholders(s) {
  const str = String(s);
  let out = '';
  for (let i = 0; i < str.length; i++) {
    if (str.startsWith('${', i)) {
      // Balance braces; nested `${` increments depth.
      let depth = 1;
      let j = i + 2;
      for (; j < str.length && depth > 0; j++) {
        if (str.startsWith('${', j)) { depth++; j++; } else if (str[j] === '}') depth--;
      }
      const inner = str.slice(i + 2, j - 1);
      const ternary = /^\s*\S+\s*\?\s*`([\s\S]*)`\s*:\s*''\s*$/.exec(inner);
      out += ternary ? maskPlaceholders(ternary[1]) : '{*}';
      i = j - 1;
      continue;
    }
    const rest = str.slice(i);
    const shell = /^\$[A-Za-z_][A-Za-z0-9_]*/.exec(rest);
    if (shell) { out += '{*}'; i += shell[0].length - 1; continue; }
    if (rest.startsWith('%s') || rest.startsWith('%d')) { out += '{*}'; i++; continue; }
    const brace = /^\{[A-Za-z_][A-Za-z0-9_]*\}/.exec(rest);
    if (brace) { out += '{*}'; i += brace[0].length - 1; continue; }
    out += str[i];
  }
  return out;
}

/** Render a `{field}` template with an object of substitutions. */
function render(template, fields) {
  return template.replace(/\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (m, k) => (
    fields[k] === undefined || fields[k] === null ? '' : String(fields[k])));
}

/** Translate a jq named-group pattern to its equivalent JS regex source. */
function jqToJsSource(jqPattern) {
  return jqPattern.replace(/\(\?<[A-Za-z_][A-Za-z0-9_]*>/g, '(');
}

// --- Registry -----------------------------------------------------------------

const MARKERS = {
  // Review verdict — the stateless review gate (issue #28). Written by the
  // review stage; consumed by re-review dedup and the merge gate.
  'review-verdict': {
    summary: 'AI review verdict for a head SHA — gates merge in automerge and dedups re-review',
    fields: ['sha', 'verdict'],
    template: '<!-- ai-sdlc:review sha={sha} verdict={verdict} -->',
    parse: {
      regex: /<!-- ai-sdlc:review sha=([0-9a-f]{7,40}) verdict=(clean|blocked|inconclusive) -->/,
      fields: ['sha', 'verdict'],
    },
    emitSites: [
      {
        file: REVIEW_RUN_YML,
        mustContain: '`<!-- ai-sdlc:review sha=${rec.head_sha} verdict=${rec.verdict} -->`',
        template: '<!-- ai-sdlc:review sha=${rec.head_sha} verdict=${rec.verdict} -->',
      },
    ],
    parseSites: [
      { file: REVIEW_RUN_YML, kind: 'js-regex', mustContain: 'const VMARK = ', trusted: true },
      { file: AUTOMERGE_YML, kind: 'js-regex', mustContain: 'const VMARK = ', trusted: true },
    ],
  },

  // Review-fix dispatch — records a bounded fix attempt was consumed for a
  // head; the fix-budget counter and dedup key off it.
  'review-fix': {
    summary: 'bounded review-fix attempt dispatched on a head — budget accounting + dedup',
    fields: ['attempt', 'sha'],
    template: '<!-- ai-sdlc:review-fix attempt={attempt} sha={sha} -->',
    // Shipped regex is unanchored (no trailing `-->`): the dispatch comment
    // continues past the marker. It must NOT match review-fix-outcome —
    // `review-fix ` (space) vs `review-fix-outcome` keeps them disjoint.
    parse: {
      regex: /<!-- ai-sdlc:review-fix attempt=(\d+) sha=([0-9a-f]{7,40})/,
      fields: ['attempt', 'sha'],
    },
    emitSites: [
      {
        file: REVIEW_FIX_YML,
        mustContain: "printf '<!-- ai-sdlc:review-fix attempt=%s sha=%s -->",
        template: '<!-- ai-sdlc:review-fix attempt=%s sha=%s -->',
      },
    ],
    parseSites: [
      { file: REVIEW_RUN_YML, kind: 'js-regex', mustContain: 'const FMARK = ', trusted: true },
      { file: AUTOMERGE_YML, kind: 'js-regex', mustContain: 'const FMARK = ', trusted: true },
    ],
  },

  // Review-fix outcome — informational record of a finished attempt. No
  // consumer parses it today; pinned anyway because its prefix must never
  // collide with `review-fix` above.
  'review-fix-outcome': {
    summary: 'review-fix attempt finished — audit record (not consumed; prefix must stay disjoint from review-fix)',
    fields: ['attempt', 'sha'],
    template: '<!-- ai-sdlc:review-fix-outcome attempt={attempt} sha={sha} -->',
    parse: {
      regex: /<!-- ai-sdlc:review-fix-outcome attempt=(\d+) sha=([0-9a-f]{7,40}) -->/,
      fields: ['attempt', 'sha'],
    },
    emitSites: [
      {
        file: REVIEW_FIX_YML,
        mustContain: '`<!-- ai-sdlc:review-fix-outcome attempt=${e.ATTEMPT} sha=${e.HEAD_SHA} -->`',
        template: '<!-- ai-sdlc:review-fix-outcome attempt=${e.ATTEMPT} sha=${e.HEAD_SHA} -->',
      },
    ],
    parseSites: [],
  },

  // Repair signal — the merge gate's per-head repair dispatch record; a
  // re-poll on the same head neither spams nor loops (dedup + budget).
  'repair-signal': {
    summary: 'per-head CI-repair dispatch signal — dedup and max-repairs accounting in the merge gate',
    fields: ['sha'],
    template: '<!-- ai-sdlc:repair-signal sha={sha} -->',
    parse: {
      regex: /<!-- ai-sdlc:repair-signal sha=([0-9a-f]+) -->/,
      fields: ['sha'],
    },
    emitSites: [
      {
        file: AUTOMERGE_YML,
        mustContain: '<!-- ai-sdlc:repair-signal sha=$SHA -->',
        template: '<!-- ai-sdlc:repair-signal sha=$SHA -->',
      },
    ],
    // Consumed from the trusted-author comment feed (issue #121) — the
    // same source as the verdict markers: a forged signal could stall the
    // merge indefinitely (`repair-awaiting`) or exhaust the repair budget
    // (`repairs-exhausted`) on demand, so arbitrary commenters must never
    // steer it.
    parseSites: [
      { file: AUTOMERGE_YML, kind: 'js-regex', mustContain: 'const MARK = ', trusted: true },
    ],
  },

  // DoR surface snapshot — the intake→implement TOCTOU pin (issue #122).
  // Intake stamps the sha256 of the exact title+body it approved as a
  // trusted-author comment; implement re-hashes its dispatch-time fetch
  // and holds the engine on drift, so a body edited after approval can
  // never smuggle post-approval content into the engine prompt.
  'dor-snapshot': {
    summary: 'sha256 pin of the DoR-approved issue title+body — dispatch-time drift detection (issue #122)',
    fields: ['sha256'],
    template: '<!-- ai-sdlc:dor sha256={sha256} -->',
    parse: {
      regex: /<!-- ai-sdlc:dor sha256=([0-9a-f]{64}) -->/,
      fields: ['sha256'],
    },
    emitSites: [
      {
        file: INTAKE_YML,
        mustContain: '<!-- ai-sdlc:dor sha256=$DORSHA -->',
        template: '<!-- ai-sdlc:dor sha256=$DORSHA -->',
      },
    ],
    // The verify side reads ONLY the trusted-author issue-comment feed —
    // a forged `ai-sdlc:dor` stamp from an arbitrary commenter must never
    // pass the drift check.
    parseSites: [
      { file: IMPLEMENT_YML, kind: 'js-regex', mustContain: 'const DORMARK = ', trusted: true },
    ],
    // The hash recipe is duplicated across two inline node scripts —
    // intake stamps, implement verifies. Both copies are pinned verbatim:
    // a one-sided change would make every dispatch read as drift (or
    // silently pass it), with no failing check to explain why.
    codePins: [
      {
        file: INTAKE_YML,
        mustContain: "createHash('sha256').update(`${i.title || ''}\\n${i.body || ''}`).digest('hex')",
      },
      {
        file: IMPLEMENT_YML,
        mustContain: "createHash('sha256').update(`${i.title || ''}\\n${i.body || ''}`).digest('hex')",
      },
    ],
  },

  // CI-repair attempt — the repair loop's attempt ledger entry; `sig` is the
  // failing-check signature stamped since issue #190 (pre-guard markers
  // parse with sig=null).
  'ci-repair-attempt': {
    summary: 'bounded CI-repair attempt record — attempt budget + stall signature (issue #190)',
    fields: ['attempt', 'sha', 'sig'],
    template: '<!-- ai-sdlc:ci-repair attempt={attempt} sha={sha}{sig} -->',
    emit(fields) {
      return `<!-- ai-sdlc:ci-repair attempt=${fields.attempt} sha=${fields.sha}${fields.sig ? ` sig=${fields.sig}` : ''} -->`;
    },
    parse: {
      // jq `capture` has named groups; the JS equivalent keeps them as
      // positional captures (group 3 is the optional wrapper, skipped).
      regex: /<!-- ai-sdlc:ci-repair attempt=([0-9]+) sha=([0-9a-f]+)( sig=([0-9a-f]+))? -->/,
      fields: ['attempt', 'sha', null, 'sig'],
      jq: '<!-- ai-sdlc:ci-repair attempt=(?<attempt>[0-9]+) sha=(?<sha>[0-9a-f]+)( sig=(?<sig>[0-9a-f]+))? -->',
    },
    emitSites: [
      {
        file: CI_REPAIR_YML,
        mustContain: '`<!-- ai-sdlc:ci-repair attempt=${e.ATTEMPT} sha=${e.HEAD_SHA}${e.STALL_SIG ? ` sig=${e.STALL_SIG}` : \'\'} -->`',
        template: '<!-- ai-sdlc:ci-repair attempt=${e.ATTEMPT} sha=${e.HEAD_SHA}${e.STALL_SIG ? ` sig=${e.STALL_SIG}` : \'\'} -->',
      },
    ],
    parseSites: [
      {
        file: CI_REPAIR_YML,
        kind: 'jq-capture',
        mustContain: 'capture("<!-- ai-sdlc:ci-repair attempt=',
        trusted: true,
      },
    ],
  },

  // Run-ledger chain head — tamper-evident evidence chain (issue #196);
  // downstream stages recover `prev` from the latest marker on the PR.
  'ledger-head': {
    summary: 'sealed run-ledger chain head on the PR — seeds downstream stage evidence chain',
    fields: ['head'],
    template: '<!-- ai-sdlc:ledger-head {head} -->',
    parse: {
      // jq `match` is unanchored and does not require the comment brackets —
      // the shipped parser accepts the marker anywhere in the body.
      regex: /ai-sdlc:ledger-head ([0-9a-f]{64})/,
      fields: ['head'],
      jq: 'ai-sdlc:ledger-head ([0-9a-f]{64})',
    },
    emitSites: [
      {
        file: IMPLEMENT_YML,
        mustContain: '<!-- ai-sdlc:ledger-head ${LEDGER_HEAD} -->',
        template: '<!-- ai-sdlc:ledger-head ${LEDGER_HEAD} -->',
      },
    ],
    parseSites: [
      {
        file: RUN_LEDGER_ACTION,
        kind: 'jq-match',
        mustContain: 'match("ai-sdlc:ledger-head ([0-9a-f]{64})")',
        trusted: true,
      },
    ],
  },
};

/** Emit the canonical marker body for a marker id + fields. */
function emit(markerId, fields) {
  const def = MARKERS[markerId];
  if (!def) throw new Error(`unknown marker: ${markerId}`);
  if (def.emit) return def.emit(fields);
  return render(def.template, fields);
}

/** The marker's canonical masked skeleton (`{*}` per substituted field). */
function canonicalSkeleton(markerId) {
  const def = MARKERS[markerId];
  const sentinels = Object.fromEntries(def.fields.map((f) => [f, '{*}']));
  return maskPlaceholders(emit(markerId, sentinels));
}

/** Parse a comment body with the canonical regex → {field: value} or null. */
function parse(markerId, body) {
  const def = MARKERS[markerId];
  if (!def) throw new Error(`unknown marker: ${markerId}`);
  const m = def.parse.regex.exec(String(body));
  if (!m) return null;
  const out = {};
  def.parse.fields.forEach((name, i) => {
    if (name !== null) out[name] = m[i + 1] === undefined ? null : m[i + 1];
  });
  return out;
}

module.exports = {
  MARKERS,
  emit,
  parse,
  render,
  maskPlaceholders,
  canonicalSkeleton,
  jqToJsSource,
  TRUSTED_AUTHOR_FILTER,
};
