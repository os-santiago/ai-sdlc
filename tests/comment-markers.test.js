// tests/comment-markers.test.js — golden fixtures for the pipeline's
// comment markers (issue #131).
//
// The control surface is regex-on-comment: `<!-- ai-sdlc:review sha=…
// verdict=… -->`, `<!-- ai-sdlc:review-fix attempt=… sha=… -->`,
// `<!-- ai-sdlc:repair-signal sha=… -->`, `<!-- ai-sdlc:ci-repair … -->`,
// `<!-- ai-sdlc:ledger-head … -->`, plus the `<untrusted-input>` escape
// wrapping every untrusted field handed to an engine. A refactor that
// changes marker syntax (or drops the trusted-author filter on a consumer)
// breaks gating silently — merges stall or proceed un-reviewed with no
// failing check to explain why.
//
// Three layers, same posture as tests/regression_harness.js — verify the
// SHIPPED text, never a copy:
//   1. site parity   — every emit/parse pin in scripts/comment-markers.js
//                      must appear verbatim in its workflow/action file,
//                      the shipped JS regex must equal the canonical pattern
//                      source, and trusted consumers must keep the
//                      Bot/OWNER/MEMBER/COLLABORATOR comment filter.
//   2. round-trip    — emit(fields) → the marker lands inside the fixture
//                      body → the canonical parser recovers the fields.
//   3. negatives     — malformed / wrong-sha-length / unknown-verdict /
//                      cross-marker-collision bodies must NOT parse; fence
//                      fixtures pin escapeFence output for `</untrusted`
//                      injection attempts.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const ROOT = path.join(__dirname, '..');
const CASES_DIR = path.join(__dirname, 'markers', 'cases');
const cm = require('../scripts/comment-markers');
const san = require('../scripts/injection_defense');

function readRepo(rel) {
  return fs.readFileSync(path.join(ROOT, rel), 'utf8');
}

function loadCases() {
  return fs.readdirSync(CASES_DIR)
    .filter((f) => f.endsWith('.json'))
    .sort()
    .map((f) => {
      const c = JSON.parse(fs.readFileSync(path.join(CASES_DIR, f), 'utf8'));
      return { file: f, ...c };
    });
}

// --- 1. Registry ↔ shipped-site parity ----------------------------------------

test('emit sites: producer text is present verbatim and the skeleton matches canonical emit', async (t) => {
  for (const [id, def] of Object.entries(cm.MARKERS)) {
    assert.ok(def.emitSites.length > 0, `${id}: no emit site declared`);
    for (const site of def.emitSites) {
      await t.test(`${id} @ ${site.file}`, () => {
        const text = readRepo(site.file);
        assert.ok(
          text.includes(site.mustContain),
          `${id}: emit template not found in ${site.file} — the producer moved or the marker format drifted; update the pin in scripts/comment-markers.js intentionally`);
        assert.equal(
          cm.maskPlaceholders(site.template),
          cm.canonicalSkeleton(id),
          `${id}: shipped emit shape drifted from canonical emit() — marker keyword order/spacing changed`);
      });
    }
  }
});

test('parse sites: consumer pattern is present verbatim and equals the canonical regex', async (t) => {
  for (const [id, def] of Object.entries(cm.MARKERS)) {
    for (const site of def.parseSites) {
      await t.test(`${id} @ ${site.file} (${site.kind})`, () => {
        const text = readRepo(site.file);
        assert.ok(
          text.includes(site.mustContain),
          `${id}: parse-site pin not found in ${site.file} — the consumer moved or was restructured`);
        if (site.kind === 'js-regex') {
          assert.ok(
            text.includes(def.parse.regex.source),
            `${id}: shipped JS regex in ${site.file} drifted from the canonical pattern ${def.parse.regex}`);
        } else {
          assert.ok(def.parse.jq, `${id}: ${site.kind} site declared without a canonical jq pattern`);
          assert.ok(
            text.includes(def.parse.jq),
            `${id}: shipped jq pattern in ${site.file} drifted from canonical ${def.parse.jq}`);
          assert.equal(
            cm.jqToJsSource(def.parse.jq),
            def.parse.regex.source,
            `${id}: canonical jq and JS patterns are no longer equivalent`);
        }
        if (site.trusted) {
          assert.ok(
            text.includes(cm.TRUSTED_AUTHOR_FILTER),
            `${id}: ${site.file} consumes markers but the trusted-author filter (Bot/OWNER/MEMBER/COLLABORATOR) is gone — a forged comment could steer the gate`);
        }
      });
    }
  }
});

// --- 2 + 3. Fixture replay: round-trip and negatives ---------------------------

test('marker fixtures: emit → parse round-trip plus negative cases', async (t) => {
  const cases = loadCases().filter((c) => c.kind !== 'fence');
  assert.ok(cases.length > 0, 'no marker parse cases found');
  for (const c of cases) {
    await t.test(`${c.id} (${c.marker})`, () => {
      const def = cm.MARKERS[c.marker];
      assert.ok(def, `case ${c.id}: unknown marker ${c.marker}`);
      if (c.fields) {
        const emitted = cm.emit(c.marker, c.fields);
        assert.ok(
          c.body.includes(emitted),
          `${c.id}: fixture body does not contain emit(fields) output ${JSON.stringify(emitted)} — producer shape and fixture drifted`);
      }
      const parsed = cm.parse(c.marker, c.body);
      if (c.expect.match) {
        assert.ok(parsed, `${c.id}: expected a match, parser returned null`);
        assert.deepEqual(parsed, c.expect.captures);
      } else {
        assert.equal(
          parsed, null,
          `${c.id}: expected no match (${c.expect.reason || 'negative case'}), got ${JSON.stringify(parsed)}`);
      }
      for (const other of c.expect.notMatch || []) {
        assert.equal(
          cm.parse(other, c.body), null,
          `${c.id}: body unexpectedly matches marker ${other} — cross-marker collision`);
      }
    });
  }
});

test('fixture coverage: every marker has at least one positive and one negative case', () => {
  const cases = loadCases().filter((c) => c.kind !== 'fence');
  for (const id of Object.keys(cm.MARKERS)) {
    const mine = cases.filter((c) => c.marker === id);
    assert.ok(mine.some((c) => c.expect.match), `${id}: no positive fixture`);
    assert.ok(mine.some((c) => !c.expect.match), `${id}: no negative fixture`);
  }
});

// --- untrusted-input fence fixtures --------------------------------------------

test('untrusted-input fixtures: escapeFence output pinned, fence structure intact', async (t) => {
  const cases = loadCases().filter((c) => c.kind === 'fence');
  assert.ok(cases.length > 0, 'no fence cases found');
  for (const c of cases) {
    await t.test(c.id, () => {
      const escaped = san.escapeFence(c.content);
      assert.equal(
        escaped, c.expect.escaped,
        `${c.id}: escapeFence output drifted — the untrusted-input escape contract changed`);
      if (c.expect.verdict) {
        const r = san.sanitizeUntrusted(c.content, { env: {} });
        assert.equal(r.verdict, c.expect.verdict,
          `${c.id}: sanitizeUntrusted verdict drifted (expected ${c.expect.verdict}, got ${r.verdict}: ${r.reasons.join(',') || 'none'})`);
      }
      // Structural: the fenced composition must carry the directive, one
      // real opener and one real closer — and the content inside must hold
      // no live fence tag an engine could read as structure.
      const fenced = san.fenceUntrusted(c.content, c.source || 'issue');
      assert.ok(fenced.startsWith(san.FENCE_DIRECTIVE));
      assert.equal(fenced.match(/<\/untrusted-input>/g).length, 1,
        'exactly one real closing tag — content must never inject its own');
      assert.ok(fenced.endsWith('</untrusted-input>'));
    });
  }
});

// --- maskPlaceholders internals -------------------------------------------------

test('maskPlaceholders normalizes every placeholder idiom', () => {
  assert.equal(
    cm.maskPlaceholders('<!-- ai-sdlc:review sha=${rec.head_sha} verdict=${rec.verdict} -->'),
    '<!-- ai-sdlc:review sha={*} verdict={*} -->');
  assert.equal(
    cm.maskPlaceholders('<!-- ai-sdlc:review-fix attempt=%s sha=%s -->'),
    '<!-- ai-sdlc:review-fix attempt={*} sha={*} -->');
  assert.equal(
    cm.maskPlaceholders('<!-- ai-sdlc:repair-signal sha=$SHA -->'),
    '<!-- ai-sdlc:repair-signal sha={*} -->');
  assert.equal(
    cm.maskPlaceholders('<!-- ai-sdlc:ci-repair attempt=${e.ATTEMPT} sha=${e.HEAD_SHA}${e.STALL_SIG ? ` sig=${e.STALL_SIG}` : \'\'} -->'),
    '<!-- ai-sdlc:ci-repair attempt={*} sha={*} sig={*} -->');
});
