#!/usr/bin/env node
// scripts/review-comment.js — verdict comment renderer (issue #127)
//
// Extraction of the `Post verdict on PR` heredoc from
// .github/workflows/ai-sdlc-review-run.yml: renders the canonical review
// comment — the `<!-- ai-sdlc:review sha=… verdict=… -->` marker the merge
// gate and the fix pass parse — plus the embedded machine-readable verdict
// block the fix prompt reuses.
//
// CLI:
//   node scripts/review-comment.js [--dir $WORK] [--out FILE] [--json]
// Env (same names the step sets): COUNTS (findings count line override).
'use strict';

const fs = require('fs');
const path = require('path');

function renderVerdictComment({ record, env } = {}) {
  const e = env || process.env;
  const rec = record || {};
  const ext = rec.external || {};
  const extLine = ext.reviewer
    ? `\`${ext.reviewer}\` — blocking: ${ext.blocking}${ext.blocking_checks.length ? ` (${ext.blocking_checks.join(', ')})` : ''}${ext.changes_requested_by.length ? `, CHANGES_REQUESTED by ${ext.changes_requested_by.join(', ')}` : ''}, inline comments: ${ext.inline_comments}`
    : 'not configured — AI-only path';
  const adv = rec.adversarial;
  const advLine = !adv ? 'not run'
    : adv.ran ? `\`${adv.verdict}\` — ${adv.attacks} probes embedded, resisted: ${adv.probes_resisted === null ? 'unknown' : adv.probes_resisted}`
    : `skipped (${adv.reason})`;
  // Markdown-table cell escape: backslashes first (CodeQL
  // js/incomplete-sanitization), then pipes — both are metacharacters in
  // the table row the summary lands in.
  const escCell = (s) => String(s || '').replace(/\\/g, '\\\\').replace(/\|/g, '\\|');
  const rows = rec.findings.map((f) => `| ${f.severity}${f.source === 'adversarial' ? ' ⚔' : ''} | \`${f.path}${f.line ? ':' + f.line : ''}\` | ${escCell(f.summary)} |`);
  const body = [
    `<!-- ai-sdlc:review sha=${rec.head_sha} verdict=${rec.verdict} -->`,
    `🔍 **AI-SDLC review — verdict \`${rec.verdict}\`** on head \`${rec.head_sha.slice(0, 8)}\``,
    '',
    `- risk tier: \`pr:risk-${rec.risk_tier}\` — ${rec.risk_why}`,
    `- findings: ${e.COUNTS || `${rec.findings.length}`}`,
    `- external reviewer: ${extLine}`,
    `- engine: \`${rec.engine || 'scc'}\` · model: \`${rec.model}\` exit ${rec.engine_exit} · run: ${rec.run}`,
    `- adversarial pass: ${advLine}`,
    `- engine: \`${rec.model}\` exit ${rec.engine_exit} · run: ${rec.run}`,
    '',
    rec.summary ? `> ${rec.summary}` : '',
    rec.findings.length ? '**Findings**\n\n| severity | location | summary |\n|---|---|---|\n' + rows.join('\n') : '',
    '',
    '<details><summary>Machine-readable verdict (reused by the fix pass)</summary>',
    '',
    '```json',
    JSON.stringify({ verdict: rec.verdict, findings: rec.findings }, null, 2),
    '```',
    '',
    '</details>',
    '',
    rec.verdict === 'blocked' ? '_A bounded fix pass is dispatched on this branch; the push re-triggers review._' : '',
  ].filter((l) => l !== '').join('\n');
  return body + '\n';
}

function parseArgs(argv) {
  const opts = { dir: undefined, out: undefined, json: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--dir') opts.dir = next();
    else if (a === '--out') opts.out = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--help' || a === '-h') { console.log('node scripts/review-comment.js [--dir D] [--out F] [--json]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const W = opts.dir || e.WORK;
  if (!W) throw new Error('no work dir: pass --dir or set $WORK');
  const rec = JSON.parse(fs.readFileSync(path.join(W, 'verdict.json'), 'utf8'));
  const body = renderVerdictComment({ record: rec, env: e });
  fs.writeFileSync(opts.out || path.join(W, 'verdict-comment.md'), body);
  if (opts.json) console.log(JSON.stringify({ bytes: Buffer.byteLength(body) }));
  return body;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`review-comment: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { renderVerdictComment, parseArgs, main };
