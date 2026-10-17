// scripts/failure-runbook.test.js — unit + file-level e2e tests for the
// known-signature → runbook fix injection (issue #188).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');

const {
  ENTRY_MARKER,
  normalizeLogLine,
  failureSignature,
  parseRunbook,
  matchEntry,
  buildAdvisory,
  injectAdvisory,
  contextText,
  main,
} = require('./failure-runbook');

const entryMd = (id, fields) => [
  `### \`${id}\` — a title for ${id}`,
  ENTRY_MARKER,
  ...fields,
].join('\n');

const RUNBOOK = [
  '# Runbook',
  '',
  '## Not an entry',
  '- **Fix:** this section lacks the marker and must be ignored',
  '',
  entryMd('known-crash', [
    '- **Signatures:** `deadbeef12345678`, `cafe99`',
    '- **Match:** `scc:config-error`, `/boom|kaput/`',
    '- **Root cause:** the adapter exploded',
    '- **Fix:** tighten the adapter bolt',
    '- **Prevention:** add a regression test',
    '- **Resolved by:** pr:o/r#7',
  ]),
  '',
  entryMd('no-fix-entry', [
    '- **Match:** `never-injectable`',
    '- **Root cause:** documented but never remediated',
  ]),
  '',
].join('\n');

const tmpdir = () => fs.mkdtempSync(path.join(os.tmpdir(), 'fr-'));

// --- signature helpers (lockstep with the ci-repair prompt step) --------------
describe('signature helpers', () => {
  it('normalizeLogLine matches the workflow transform', () => {
    assert.equal(
      normalizeLogLine('##[error] failed at sha abcdef1234 after 42 retries'),
      'failed at sha H after N retries',
    );
    assert.equal(normalizeLogLine('x'.repeat(300)).length, 200);
  });

  it('failureSignature matches the workflow formula (sorted parts, sha256[:16])', () => {
    const parts = ['lint:not ok N', 'test:failure at sha H'];
    const expected = crypto.createHash('sha256')
      .update([...parts].sort().join('\n')).digest('hex').slice(0, 16);
    assert.equal(failureSignature(parts), expected);
    // Order-independent: parts are sorted before hashing.
    assert.equal(failureSignature([...parts].reverse()), expected);
  });

  it('failureSignature normalizes {name, line} objects', () => {
    const raw = failureSignature([{ name: 'ci', line: '##[error] boom 123' }]);
    const ref = failureSignature(['ci:boom N']);
    assert.equal(raw, ref);
  });
});

// --- store parsing -------------------------------------------------------------
describe('parseRunbook', () => {
  it('parses marked entries with all fields; unmarked sections are ignored', () => {
    const { entries } = parseRunbook(RUNBOOK);
    assert.equal(entries.length, 2);
    const e = entries[0];
    assert.equal(e.id, 'known-crash');
    assert.equal(e.title, 'a title for known-crash');
    assert.deepEqual(e.signatures, ['deadbeef12345678', 'cafe99']);
    assert.equal(e.fix, 'tighten the adapter bolt');
    assert.equal(e.rootCause, 'the adapter exploded');
    assert.equal(e.prevention, 'add a regression test');
    assert.equal(e.resolvedBy, 'pr:o/r#7');
    assert.deepEqual(e.match.map((t) => t.token), ['scc:config-error', '/boom|kaput/']);
    assert.ok(e.match[0].literal && !e.match[0].regex);
    assert.ok(e.match[1].regex && !e.match[1].literal);
  });

  it('empty / garbage docs yield no entries and never throw', () => {
    assert.deepEqual(parseRunbook('').entries, []);
    assert.deepEqual(parseRunbook('no headings here').entries, []);
    assert.deepEqual(parseRunbook('### `x` — t\n- **Fix:** f\n').entries, []);
  });

  it('invalid /regex/ tokens degrade to literal matching', () => {
    const { entries } = parseRunbook(entryMd('badre', [
      '- **Match:** `/unclosed[/`',
      '- **Fix:** f',
    ]));
    assert.equal(entries[0].match[0].literal, '/unclosed[/');
  });
});

// --- lookup ---------------------------------------------------------------------
describe('matchEntry', () => {
  const { entries } = parseRunbook(RUNBOOK);

  it('hits on an exact signature hash', () => {
    const m = matchEntry({ signature: 'deadbeef12345678' }, entries);
    assert.equal(m.entry.id, 'known-crash');
    assert.equal(m.via, 'signature');
  });

  it('hits on a >=6-hex prefix of the computed signature', () => {
    const m = matchEntry({ signature: 'cafe99aabbccdd11' }, entries);
    assert.equal(m.entry.id, 'known-crash');
  });

  it('hits on literal and regex match tokens (case-insensitive)', () => {
    assert.equal(
      matchEntry({ text: 'ci / test:##[error] SCC:CONFIG-ERROR on base' }, entries).entry.id,
      'known-crash',
    );
    assert.equal(
      matchEntry({ text: 'suite / unit:went KAPUT at line N' }, entries).via,
      'match',
    );
  });

  it('signature hits outrank pattern hits regardless of entry order', () => {
    const rb = parseRunbook([
      entryMd('first', ['- **Match:** `overlap`', '- **Fix:** f1']),
      entryMd('second', ['- **Signatures:** `aabbccddeeff0011`', '- **Fix:** f2']),
    ].join('\n\n'));
    const m = matchEntry({ signature: 'aabbccddeeff0011', text: 'overlap' }, rb.entries);
    assert.equal(m.entry.id, 'second');
    assert.equal(m.via, 'signature');
  });

  it('entries without a Fix are never injected', () => {
    assert.equal(matchEntry({ text: 'never-injectable' }, entries), null);
  });

  it('misses cleanly on empty inputs', () => {
    assert.equal(matchEntry({}, entries), null);
    assert.equal(matchEntry({ signature: 'ffff0011ffff0011', text: 'unrelated' }, entries), null);
    assert.equal(matchEntry({ signature: 'zzzz' }, entries), null);
  });
});

// --- advisory rendering -----------------------------------------------------------
describe('buildAdvisory', () => {
  const { entries } = parseRunbook(RUNBOOK);
  const e = entries[0];

  it('renders the fix plus provenance', () => {
    const a = buildAdvisory(e);
    assert.ok(a.includes('`known-crash`'));
    assert.ok(a.includes('Prior fix: tighten the adapter bolt'));
    assert.ok(a.includes('Prior root cause: the adapter exploded'));
    assert.ok(a.includes('Prevention rule: add a regression test'));
    assert.ok(a.includes('Resolved by: pr:o/r#7'));
  });

  it('is byte-bounded by maxBytes and marks truncation', () => {
    const big = { ...e, fix: `x`.repeat(5000), title: 't'.repeat(500) };
    const a = buildAdvisory(big, { maxBytes: 800 });
    assert.ok(Buffer.byteLength(a) <= 800, `got ${Buffer.byteLength(a)}`);
    assert.ok(a.includes('runbook excerpt truncated'));
  });

  it('honours a small cap without going negative', () => {
    const a = buildAdvisory(e, { maxBytes: 60 });
    assert.ok(Buffer.byteLength(a) <= 60);
  });

  it('maxBytes <= 0 disables the advisory; missing fix renders nothing', () => {
    assert.equal(buildAdvisory(e, { maxBytes: 0 }), '');
    assert.equal(buildAdvisory({ id: 'x', title: 't' }), '');
    assert.equal(buildAdvisory(null), '');
  });
});

// --- injection contract -------------------------------------------------------------
describe('injectAdvisory', () => {
  it('puts the fix block at the top of the brief on a hit', () => {
    const brief = 'You are a senior engineer.\n\n<untrusted-input source="pr-ci-logs">\nlogs\n</untrusted-input>';
    const out = injectAdvisory(brief, '## Runbook hit\n- Prior fix: tighten the adapter bolt');
    assert.ok(out.startsWith('## Runbook hit'));
    assert.ok(out.indexOf('Prior fix: tighten the adapter bolt') < out.indexOf('You are a senior engineer'));
    assert.ok(out.endsWith('</untrusted-input>'));
  });

  it('returns the brief byte-identical on a miss (no noise)', () => {
    const brief = 'unchanged brief\n';
    assert.equal(injectAdvisory(brief, ''), brief);
    assert.equal(injectAdvisory(brief, '   '), brief);
    assert.equal(injectAdvisory(brief, null), brief);
  });
});

// --- context ---------------------------------------------------------------------
describe('contextText', () => {
  it('flattens failure.json fields into matchable text', () => {
    const t = contextText({
      signature: 'abc123',
      signatureParts: ['ci:not ok N'],
      rootCause: 'ci: not ok N — boom',
      checks: ['ci'],
    });
    assert.ok(t.includes('abc123') && t.includes('ci:not ok N') && t.includes('boom'));
  });
  it('passes raw text through', () => {
    assert.equal(contextText('raw log'), 'raw log');
  });
});

// --- CLI / e2e over real files ----------------------------------------------------
describe('cli main', () => {
  function fixture({ sig = 'deadbeef12345678', text = '' } = {}) {
    const dir = tmpdir();
    const rb = path.join(dir, 'failure_modes.md');
    const ctx = path.join(dir, 'failure.json');
    const prompt = path.join(dir, 'repair-prompt.md');
    fs.writeFileSync(rb, RUNBOOK);
    fs.writeFileSync(ctx, JSON.stringify({
      signature: sig,
      signatureParts: [text],
      rootCause: text,
      checks: ['ci'],
    }));
    fs.writeFileSync(prompt, 'ORIGINAL BRIEF\n');
    return { dir, rb, ctx, prompt };
  }

  it('hit: known signature injects the fix at the top of the prompt file', () => {
    const { rb, ctx, prompt } = fixture({ sig: 'deadbeef12345678' });
    let out = '';
    const r = main(
      ['--runbook', rb, '--context', ctx, '--prompt-file', prompt, '--json'],
      { stdout: (s) => { out = s; } },
    );
    assert.equal(r.hit, true);
    assert.equal(r.injected, true);
    const brief = fs.readFileSync(prompt, 'utf8');
    assert.ok(brief.startsWith('## Runbook hit — known failure mode `known-crash`'));
    assert.ok(brief.includes('Prior fix: tighten the adapter bolt'));
    assert.ok(brief.endsWith('ORIGINAL BRIEF\n'));
    assert.equal(JSON.parse(out).entry, 'known-crash');
  });

  it('hit via pattern match on the simulated signature text', () => {
    const { rb, ctx, prompt } = fixture({
      sig: 'ffff0000ffff0000',
      text: 'ci / validate:##[error] scc:config-error — contract invalid on base',
    });
    const r = main(['--runbook', rb, '--context', ctx, '--prompt-file', prompt, '--json'], { stdout: () => {} });
    assert.equal(r.hit, true);
    assert.equal(r.via, 'match');
    assert.ok(fs.readFileSync(prompt, 'utf8').includes('Prior fix:'));
  });

  it('miss: unknown signature leaves the brief byte-identical', () => {
    const { rb, ctx, prompt } = fixture({ sig: 'ffff0000ffff0000', text: 'ci:nothing known here' });
    const before = fs.readFileSync(prompt, 'utf8');
    let out = '';
    const r = main(
      ['--runbook', rb, '--context', ctx, '--prompt-file', prompt, '--json'],
      { stdout: (s) => { out = s; } },
    );
    assert.equal(r.hit, false);
    assert.equal(r.injected, false);
    assert.equal(fs.readFileSync(prompt, 'utf8'), before);
    assert.equal(JSON.parse(out).entry, null);
  });

  it('truncation: --max-bytes bounds the injected excerpt', () => {
    const dir = tmpdir();
    const rb = path.join(dir, 'rb.md');
    const prompt = path.join(dir, 'p.md');
    fs.writeFileSync(rb, entryMd('big', [
      '- **Match:** `bigsig`',
      `- **Fix:** ${'f'.repeat(9000)}`,
    ]));
    fs.writeFileSync(prompt, 'BRIEF\n');
    const r = main(
      ['--runbook', rb, '--text', 'bigsig', '--prompt-file', prompt, '--max-bytes', '400', '--json'],
      { stdout: () => {} },
    );
    const brief = fs.readFileSync(prompt, 'utf8');
    assert.equal(r.hit, true);
    // Advisory ≤ cap; total prompt = advisory + 2 newlines + original brief.
    assert.ok(Buffer.byteLength(brief) <= 400 + Buffer.byteLength('BRIEF\n') + 2);
    assert.ok(brief.includes('runbook excerpt truncated'));
  });

  it('missing runbook file is a graceful miss, not a crash', () => {
    const dir = tmpdir();
    const prompt = path.join(dir, 'p.md');
    fs.writeFileSync(prompt, 'BRIEF\n');
    let out = '';
    const r = main(
      ['--runbook', path.join(dir, 'absent.md'), '--signature', 'deadbeef12345678', '--prompt-file', prompt, '--json'],
      { stdout: (s) => { out = s; } },
    );
    assert.equal(r.hit, false);
    assert.equal(fs.readFileSync(prompt, 'utf8'), 'BRIEF\n');
    assert.ok(JSON.parse(out).runbookError);
  });

  it('--out writes elsewhere and leaves the source brief untouched', () => {
    const { rb, ctx, prompt, dir } = fixture({ sig: 'deadbeef12345678' });
    const outFile = path.join(dir, 'out.md');
    main(['--runbook', rb, '--context', ctx, '--prompt-file', prompt, '--out', outFile], { stdout: () => {} });
    assert.equal(fs.readFileSync(prompt, 'utf8'), 'ORIGINAL BRIEF\n');
    assert.ok(fs.readFileSync(outFile, 'utf8').includes('Prior fix:'));
  });

  it('the shipped store parses and every entry is reachable + fixed', () => {
    const store = fs.readFileSync(
      path.join(__dirname, '..', 'docs', 'runbooks', 'failure_modes.md'), 'utf8');
    const { entries } = parseRunbook(store);
    assert.ok(entries.length >= 3, `expected seeded entries, got ${entries.length}`);
    for (const e of entries) {
      assert.ok(e.fix, `entry ${e.id} has no fix`);
      assert.ok(e.signatures.length + e.match.length > 0, `entry ${e.id} is unreachable`);
    }
  });
});
