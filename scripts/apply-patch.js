#!/usr/bin/env node
// scripts/apply-patch.js — deterministic validator + applier for engine
// output (issue #212, credential isolation).
//
// Stage-privilege contract: the engine runs without forge write credentials
// and its ONLY deliverable is a git-format patch artifact. Everything the
// engine produced must pass through this validator before it can land on a
// branch — a compromised agent can emit output, never push it. Validation
// is bounded (size/file caps), auditable (JSON report of every accepted and
// rejected path), and fail-closed (any parse ambiguity rejects).
//
// Checks, in order:
//   1. size/file bounds         — oversized or empty input rejected
//   2. git-format parse         — non-patch input, malformed sections and
//                                 trailing garbage rejected
//   3. path hygiene             — absolute paths, '..' segments, '.git'
//                                 components, backslashes, control chars,
//                                 empty segments rejected (defense in depth;
//                                 `git apply` alone already refuses most)
//   4. forbidden paths          — .github/workflows/** denied by default
//                                 (the App token has Workflows:No-access —
//                                 this layer makes the deny deterministic
//                                 and auditable); extensible via --deny
//   5. optional allowlist       — --allow GLOB: every path must match
//   6. mode guards              — symlink (120000) and gitlink/submodule
//                                 (160000) modes rejected; only regular
//                                 blob modes 100644/100755 allowed
//   7. binary guard             — 'GIT binary patch' / 'Binary files'
//                                 sections rejected: opaque bytes are not
//                                 auditable
//   8. apply                    — `git apply --check` then `git apply`;
//                                 a failed check is a rejection (stale or
//                                 forged context), never a partial apply
//
// Trust boundary: this file must only ever execute from a trusted ref
// (the runtime repo pin) — engine output must never supply or weaken the
// gate that applies it. The implement workflow fetches it from
// os-santiago/ai-sdlc@main, same posture as scripts/risk-classifier.js.
//
// CLI:
//   node scripts/apply-patch.js --patch FILE [options]
//
// Options:
//   --patch FILE     git-format unified diff to validate and apply (required)
//   --repo DIR       worktree the patch applies to (default: cwd)
//   --max-bytes N    patch size cap (default 2000000)
//   --max-files N    touched-path cap  (default 2000)
//   --deny GLOB      extra denied path glob; repeatable (case-insensitive)
//   --allow GLOB     required path glob; repeatable — every path must match one
//   --check-only     validate + `git apply --check`, do not mutate the worktree
//   --audit FILE     write the JSON audit report to FILE
//   --json           print the audit report to stdout
//   --github-output  append applied/files/rejected to $GITHUB_OUTPUT
//
// Exit: 0 applied (or validated with --check-only) · 2 validation rejection
//       · 1 operational error (usage, IO, git apply failure).
'use strict';

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const VERSION = 1;
const DEFAULT_MAX_BYTES = 2000000;
const DEFAULT_MAX_FILES = 2000;
// Hard deny — workflow-file writes can never come from engine output. The
// ai-sdlc GitHub App holds Workflows:No-access, so a patch touching this
// surface could never push anyway; rejecting here keeps the contract
// deterministic instead of failing opaque at push time. Case-insensitive:
// a differently-cased path still lands on case-insensitive checkouts.
const BUILTIN_DENY = ['.github/workflows', '.github/workflows/**'];
// Regular blob modes only — no symlink (120000), no gitlink/submodule
// (160000), no legacy oddities.
const ALLOWED_MODES = new Set(['100644', '100755']);

// --- glob → RegExp -----------------------------------------------------------
// Minimal gitignore-style glob: `**` spans directories, `*` and `?` never
// cross `/`. Anchored, case-insensitive full-match.
function globToRegExp(glob) {
  let re = '';
  for (let i = 0; i < glob.length; i++) {
    const c = glob[i];
    if (c === '*') {
      if (glob[i + 1] === '*') {
        // `**/x` also matches `x` at depth 0; trailing `**` eats the rest.
        if (glob[i + 2] === '/') { re += '(?:[^/]+/)*'; i += 2; }
        else { re += '.*'; i += 1; }
      } else {
        re += '[^/]*';
      }
    } else if (c === '?') {
      re += '[^/]';
    } else {
      re += c.replace(/[.+^${}()|[\]\\]/g, '\\$&');
    }
  }
  return new RegExp(`^${re}$`, 'i');
}

// --- C-quoted path unquoting --------------------------------------------------
// git quotes paths containing specials as "a/x\ty" (C-style octal/escapes).
function unquoteGitPath(raw) {
  if (raw[0] !== '"') return raw;
  if (raw[raw.length - 1] !== '"') throw new Error('unterminated quoted path');
  const body = raw.slice(1, -1);
  // Octal escapes are raw BYTES (git quotes non-ASCII as UTF-8 octal) —
  // accumulate bytes and decode UTF-8 at the end so multibyte chars survive.
  const bytes = [];
  const pushChar = (c) => { for (const b of Buffer.from(c, 'utf8')) bytes.push(b); };
  const simple = { n: '\n', t: '\t', r: '\r', b: '\b', f: '\f', v: '\v', a: '\x07', '"': '"', '\\': '\\' };
  for (let i = 0; i < body.length; i++) {
    if (body[i] !== '\\') { pushChar(body[i]); continue; }
    const e = body[++i];
    if (e === undefined) throw new Error('dangling escape in quoted path');
    if (simple[e] !== undefined) { pushChar(simple[e]); continue; }
    if (/[0-7]/.test(e)) {
      const oct = body.slice(i, i + 3);
      if (!/^[0-7]{3}$/.test(oct)) throw new Error('bad octal escape in quoted path');
      bytes.push(parseInt(oct, 8));
      i += 2;
      continue;
    }
    throw new Error(`unknown escape \\${e} in quoted path`);
  }
  return Buffer.from(bytes).toString('utf8');
}

// --- path hygiene -------------------------------------------------------------
// Returns a reason string on violation, null when the path is clean.
function pathViolation(p) {
  if (!p) return 'empty-path';
  // eslint-disable-next-line no-control-regex
  if (/[\x00-\x1f\x7f]/.test(p)) return 'control-character-in-path';
  if (p.includes('\\')) return 'backslash-in-path';
  if (p.startsWith('/')) return 'absolute-path';
  if (/^[A-Za-z]:/.test(p)) return 'windows-drive-path';
  const segs = p.split('/');
  for (const s of segs) {
    if (s === '') return 'empty-path-segment';
    if (s === '.') return 'dot-path-segment';
    if (s === '..') return 'dotdot-traversal';
    if (s.toLowerCase() === '.git') return 'git-internal-path';
  }
  return null;
}

// Strip the a/ or b/ prefix from `diff --git`, `---`, `+++` header paths.
// `rename from`/`rename to`/`copy from`/`copy to` lines carry no prefix —
// those go through unquoteGitPath directly.
function stripDiffPrefix(raw) {
  if (raw === '/dev/null') return null;
  const p = raw.startsWith('"') ? unquoteGitPath(raw) : raw;
  if (!/^[ab]\//.test(p)) throw new Error(`header path '${raw}' lacks a/ or b/ prefix`);
  return p.slice(2);
}

// Split the `diff --git <old> <new>` operand pair. Paths containing spaces
// are always C-quoted by git, so the separator is ` <new>` where new is
// `b/...` or `"b/..."`; the old operand's closing quote is found by an
// escape-aware scan (a filename may itself contain ` b/` when quoted).
function splitDiffHeader(q) {
  if (q.startsWith('"')) {
    for (let i = 1; i < q.length; i++) {
      if (q[i] === '\\') { i++; continue; }
      if (q[i] === '"') {
        if (q[i + 1] !== ' ') throw new Error('malformed-diff-header');
        return [q.slice(0, i + 1), q.slice(i + 2)];
      }
    }
    throw new Error('malformed-diff-header');
  }
  const sep = q.indexOf(' b/');
  if (sep < 0) throw new Error('malformed-diff-header');
  return [q.slice(0, sep), q.slice(sep + 1)];
}

// --- patch parser -------------------------------------------------------------
// Parses `git diff` output (NOT git format-patch email format — the pipeline
// generates the artifact with `git diff --cached`, anything else is not the
// deterministic input contract). Returns { files, rejects } where files is
// [{path, oldPath, op, hunks, added, deleted, modes}] and rejects is
// [{where, path, reason}]. Parse problems never throw — they record a reject
// so the caller reports the full audit instead of dying on the first line.
function parsePatch(text) {
  const files = [];
  const rejects = [];
  const lines = text.split('\n');
  // A trailing newline produces a final '' element — drop only that.
  if (lines.length && lines[lines.length - 1] === '') lines.pop();

  const fail = (where, p, reason) => rejects.push({ where, path: p ?? null, reason });
  const checkPath = (where, p) => {
    const v = pathViolation(p);
    if (v) { fail(where, p, v); return false; }
    return true;
  };

  if (!lines.length || !lines[0].startsWith('diff --git ')) {
    fail('header', null, 'not-a-git-patch');
    return { files, rejects };
  }

  let i = 0;
  while (i < lines.length) {
    if (!lines[i].startsWith('diff --git ')) {
      fail('line ' + (i + 1), null, 'malformed-patch');
      break;
    }
    let oldRaw, newRaw;
    try {
      [oldRaw, newRaw] = splitDiffHeader(lines[i].slice('diff --git '.length));
    } catch (e) {
      fail('line ' + (i + 1), null, 'malformed-diff-header');
      break;
    }
    i++;

    // extraPaths: ---/+++ operand paths — cross-checked against the header
    // pair AND fed to the deny list so a mismatched operand can't smuggle a
    // forbidden path past audit.
    const rec = { path: null, oldPath: null, op: 'modified', hunks: 0, added: 0, deleted: 0, modes: {}, extraPaths: [] };
    let bad = false;
    try { rec.oldPath = stripDiffPrefix(oldRaw); } catch (e) { fail('line ' + i, oldRaw, 'malformed-path'); bad = true; }
    try { rec.path = stripDiffPrefix(newRaw); } catch (e) { fail('line ' + i, newRaw, 'malformed-path'); bad = true; }

    // --- extended headers (until ---/+++/Binary/GIT binary/next diff) ---
    let sawOld = false;  // whether a `---` line was seen
    let sawNew = false;  // whether a `+++` line was seen
    while (i < lines.length && !lines[i].startsWith('diff --git ')) {
      const l = lines[i];
      let mm;
      if ((mm = /^(old mode|new mode|new file mode|deleted file mode) (\d+)$/.exec(l))) {
        const [kind, mode] = [mm[1], mm[2]];
        rec.modes[kind] = mode;
        // Only forward-state modes are gated: deleting a symlink or turning
        // one into a blob is benign — creating one is not.
        if ((kind === 'new file mode' || kind === 'new mode') && !ALLOWED_MODES.has(mode)) {
          fail('line ' + (i + 1), rec.path, `forbidden-mode-${mode}` +
            (mode === '120000' ? '-symlink' : mode === '160000' ? '-gitlink' : ''));
          bad = true;
        }
        if (kind === 'new file mode') rec.op = 'added';
        if (kind === 'deleted file mode') rec.op = 'deleted';
        i++;
      } else if (/^(similarity index|dissimilarity index) \d+%$/.test(l)) {
        i++;
      } else if ((mm = /^(rename from|rename to|copy from|copy to) (.+)$/.exec(l))) {
        let p = null;
        try { p = unquoteGitPath(mm[2].trim()); } catch (e) { fail('line ' + (i + 1), mm[2], 'malformed-path'); bad = true; }
        if (p !== null) {
          if (!checkPath('line ' + (i + 1), p)) bad = true;
          else if (mm[1] === 'rename to' || mm[1] === 'copy to') rec.path = p;
          else rec.oldPath = p;
        }
        if (mm[1] === 'rename to') rec.op = 'renamed';
        if (mm[1] === 'copy to') rec.op = 'copied';
        i++;
      } else if ((mm = /^index [0-9a-z]+\.\.[0-9a-z]+(?: (\d+))?$/.exec(l))) {
        // Abbreviated blob ids are hex but git tolerates a-z; the optional
        // trailing mode is the resulting file mode — gate it too.
        if (mm[1] !== undefined && !ALLOWED_MODES.has(mm[1])) {
          fail('line ' + (i + 1), rec.path, `forbidden-mode-${mm[1]}`);
          bad = true;
        }
        i++;
      } else if (l.startsWith('GIT binary patch') || l.startsWith('Binary files ')) {
        fail('line ' + (i + 1), rec.path, 'binary-patch-not-allowed');
        bad = true;
        i++;
        // Consume the binary body so it can't desync the section loop.
        while (i < lines.length && !lines[i].startsWith('diff --git ')) i++;
      } else if (l.startsWith('--- ')) {
        let p = null;
        try { p = stripDiffPrefix(l.slice(4).trim()); } catch (e) { fail('line ' + (i + 1), l, 'malformed-path'); bad = true; }
        if (p !== null) {
          rec.extraPaths.push(p);
          if (!checkPath('line ' + (i + 1), p)) bad = true;
          else if (p !== rec.oldPath) { fail('line ' + (i + 1), p, 'old-path-mismatch'); bad = true; }
        }
        sawOld = true;
        i++;
      } else if (l.startsWith('+++ ')) {
        let p = null;
        try { p = stripDiffPrefix(l.slice(4).trim()); } catch (e) { fail('line ' + (i + 1), l, 'malformed-path'); bad = true; }
        if (p !== null) {
          rec.extraPaths.push(p);
          if (!checkPath('line ' + (i + 1), p)) bad = true;
          else if (p !== rec.path) { fail('line ' + (i + 1), p, 'new-path-mismatch'); bad = true; }
        }
        sawNew = true;
        i++;
        // Hunks follow until the next section/EOF.
        while (i < lines.length && !lines[i].startsWith('diff --git ')) {
          const h = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/.exec(lines[i]);
          if (!h) {
            fail('line ' + (i + 1), rec.path, 'malformed-hunk-header');
            bad = true; i++; continue;
          }
          const oldCount = h[2] === undefined ? 1 : +h[2];
          const newCount = h[4] === undefined ? 1 : +h[4];
          rec.hunks++;
          i++;
          let minus = 0, plus = 0, ctx = 0;
          while (i < lines.length) {
            const b = lines[i];
            if (b.startsWith('@@ ') || b.startsWith('diff --git ')) break;
            // git emits empty context lines as '' or ' ' — both count.
            if (b === '' || b[0] === ' ') { ctx++; }
            else if (b[0] === '-') { minus++; }
            else if (b[0] === '+') { plus++; }
            else if (b.startsWith('\\ No newline at end of file')) { /* marker — not a line */ }
            else { fail('line ' + (i + 1), rec.path, 'malformed-hunk-body'); bad = true; }
            i++;
          }
          if (minus + ctx !== oldCount || plus + ctx !== newCount) {
            fail('line ' + (i + 1), rec.path,
              `hunk-count-mismatch(-want ${oldCount} got ${minus + ctx},+want ${newCount} got ${plus + ctx})`);
            bad = true;
          }
          rec.added += plus;
          rec.deleted += minus;
        }
      } else if (l.trim() === '') {
        fail('line ' + (i + 1), rec.path, 'malformed-patch');
        bad = true; i++;
      } else {
        fail('line ' + (i + 1), rec.path, 'unrecognized-section-line');
        bad = true; i++;
      }
    }
    void sawOld; void sawNew; // operands cross-checked inline above

    if (rec.path !== null && !checkPath('diff --git', rec.path)) bad = true;
    if (rec.oldPath !== null && !checkPath('diff --git', rec.oldPath)) bad = true;
    files.push(rec);
  }
  return { files, rejects };
}

// --- validation ---------------------------------------------------------------
function validate(text, opts = {}) {
  const deny = [...BUILTIN_DENY, ...(opts.deny || [])].map(globToRegExp);
  const allow = (opts.allow || []).map(globToRegExp);
  const maxFiles = opts.maxFiles ?? DEFAULT_MAX_FILES;

  const report = {
    v: VERSION,
    bytes: Buffer.byteLength(text),
    files: [],
    rejects: [],
    verdict: 'rejected',
  };

  const { files, rejects } = parsePatch(text);
  report.rejects.push(...rejects);

  if (!files.length && !rejects.length) {
    report.rejects.push({ where: 'input', path: null, reason: 'empty-patch' });
  }
  if (files.length > maxFiles) {
    report.rejects.push({ where: 'input', path: null, reason: `too-many-files(${files.length}>${maxFiles})` });
  }

  for (const f of files) {
    const touched = [f.path, f.oldPath, ...(f.extraPaths || [])].filter((p) => p && p !== '/dev/null');
    for (const p of new Set(touched)) {
      for (const g of deny) {
        if (g.test(p)) {
          report.rejects.push({ where: 'deny-glob', path: p, reason: `forbidden-path(${g.source})` });
          break;
        }
      }
      if (allow.length && !allow.some((g) => g.test(p))) {
        report.rejects.push({ where: 'allow-glob', path: p, reason: 'path-not-in-allowlist' });
      }
    }
  }

  report.files = files.map((f) => ({
    path: f.path, oldPath: f.oldPath, op: f.op,
    hunks: f.hunks, added: f.added, deleted: f.deleted,
  }));
  // Deduplicate reject rows (a path can trip hygiene AND a deny glob).
  const seen = new Set();
  report.rejects = report.rejects.filter((r) => {
    const k = `${r.where}|${r.path}|${r.reason}`;
    if (seen.has(k)) return false;
    seen.add(k); return true;
  });
  if (!report.rejects.length) report.verdict = 'validated';
  return report;
}

// --- apply --------------------------------------------------------------------
function gitApply(patchFile, repo, checkOnly) {
  const args = ['-C', repo, 'apply', '--whitespace=nowarn'];
  if (checkOnly) args.push('--check');
  args.push(patchFile);
  return execFileSync('git', args, { stdio: ['ignore', 'pipe', 'pipe'] });
}

// --- CLI ----------------------------------------------------------------------
function usage() {
  return 'usage: apply-patch.js --patch FILE [--repo DIR] [--max-bytes N] [--max-files N] ' +
    '[--deny GLOB]... [--allow GLOB]... [--check-only] [--audit FILE] [--json] [--github-output]';
}

function parseArgs(argv) {
  const o = { deny: [], allow: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const next = () => {
      if (i + 1 >= argv.length) throw new Error(`${a} needs a value — ${usage()}`);
      return argv[++i];
    };
    if (a === '--patch') o.patch = next();
    else if (a === '--repo') o.repo = next();
    else if (a === '--max-bytes') o.maxBytes = +next();
    else if (a === '--max-files') o.maxFiles = +next();
    else if (a === '--deny') o.deny.push(next());
    else if (a === '--allow') o.allow.push(next());
    else if (a === '--audit') o.audit = next();
    else if (a === '--check-only') o.checkOnly = true;
    else if (a === '--json') o.json = true;
    else if (a === '--github-output') o.githubOutput = true;
    else throw new Error(`unknown flag ${a} — ${usage()}`);
  }
  if (!o.patch) throw new Error(`--patch required — ${usage()}`);
  return o;
}

// Returns the audit report; throws on operational error (exit 1). Validation
// rejections do not throw — inspect report.verdict / report.rejects.
function main(argv, io = {}) {
  const stdout = io.stdout || ((s) => process.stdout.write(s + '\n'));
  const o = parseArgs(argv);
  const repo = o.repo || process.cwd();

  const st = fs.statSync(o.patch);
  if (!st.isFile()) { const e = new Error(`${o.patch}: not a regular file`); e.exit = 1; throw e; }
  const maxBytes = o.maxBytes ?? DEFAULT_MAX_BYTES;

  let report;
  if (st.size > maxBytes) {
    report = {
      v: VERSION, bytes: st.size, files: [], sha256: null, patch: path.basename(o.patch),
      rejects: [{ where: 'input', path: null, reason: `patch-too-large(${st.size}>${maxBytes})` }],
      verdict: 'rejected',
    };
  } else {
    const text = fs.readFileSync(o.patch, 'utf8');
    report = validate(text, o);
    report.sha256 = crypto.createHash('sha256').update(text).digest('hex');
    report.patch = path.basename(o.patch);

    if (report.verdict === 'validated') {
      // Dry-run first: the parser gates structure, git gates applicability
      // against the actual worktree (context mismatch = stale/forged patch).
      // A failed --check is a validation rejection, not an operational error.
      try {
        gitApply(o.patch, repo, true);
      } catch (e) {
        report.rejects.push({
          where: 'git apply --check', path: null,
          reason: `apply-check-failed(${String(e.stderr || e.message).trim().split('\n')[0]})`,
        });
        report.verdict = 'rejected';
      }
      if (report.verdict === 'validated' && !o.checkOnly) {
        gitApply(o.patch, repo, false); // throws → exit 1 (operational)
        report.verdict = 'applied';
      }
    }
  }

  if (o.audit) fs.writeFileSync(o.audit, JSON.stringify(report, null, 2) + '\n');
  if (o.json) stdout(JSON.stringify(report));
  if (o.githubOutput && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT,
      `applied=${report.verdict === 'applied'}\n` +
      `files=${report.files.length}\n` +
      `rejected=${report.rejects.length}\n`);
  }
  return report;
}

if (require.main === module) {
  let report;
  try {
    report = main(process.argv.slice(2));
  } catch (e) {
    process.stderr.write(`apply-patch: ${e.message}\n`);
    process.exit(e.exit || 1);
  }
  if (report.verdict === 'rejected') {
    for (const r of report.rejects) {
      process.stderr.write(`apply-patch: rejected ${r.where}: ${r.path ?? '-'} — ${r.reason}\n`);
    }
    process.exit(2);
  }
  process.exit(0);
}

module.exports = {
  globToRegExp,
  unquoteGitPath,
  pathViolation,
  stripDiffPrefix,
  splitDiffHeader,
  parsePatch,
  validate,
  parseArgs,
  main,
  BUILTIN_DENY,
  ALLOWED_MODES,
};
