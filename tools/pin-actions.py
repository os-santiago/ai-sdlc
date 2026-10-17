#!/usr/bin/env python3
"""tools/pin-actions.py — pin third-party `uses:` refs to commit SHAs
(issues #118/#123, epic #87).

Tag refs (`actions/checkout@v7`) are mutable — a retagged or hijacked
release runs inside jobs holding the App private key and minted tokens.
This tool resolves each `owner/repo[/path]@<tag>` to the commit SHA the
tag currently points at and rewrites it in place:

    uses: actions/checkout@v7
      -> uses: actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683  # v7

The tag is kept as a trailing comment: Dependabot's `github-actions`
ecosystem updates SHA-pinned refs and refreshes the comment
(.github/dependabot.yml), so pinning does not freeze versions.

Resolution goes through the GitHub API (`gh` CLI):
`repos/{owner}/{repo}/git/ref/tags/{tag}` — annotated tags
(`object.type == "tag"`) are dereferenced once via `git/tags/{sha}` to
the underlying commit. Branch-style refs fall back to `git/ref/heads/*`.

Exempt by policy (never pinned, never flagged):
  * `os-santiago/ai-sdlc` self-refs — internal `uses:` stay on the
    `runtime_ref` convention: every reusable stage declares a
    `runtime_ref` input (default `main`) so the workflow ref and the
    scripts it fetches move together; consumers pin via their own
    caller workflow.
  * local `./.github/...` refs — the checkout is already the pin.
  * `${{ }}` expressions and `docker:` images — not `owner/repo@ref`.

Modes:
  (default)    resolve + rewrite every unpinned third-party ref
  --dry-run    print planned rewrites, touch nothing
  --check      offline audit: every third-party `uses:` must be a
               40-hex SHA carrying a `# <version>` comment. Exit 1 on
               violations — wired into tools/validate-repo.sh.

Zero dependencies beyond `gh` (apply/dry-run only; --check is offline).
Exit 0 = clean, 1 = violations or resolution failure.
"""
import argparse
import glob
import json
import os
import re
import subprocess
import sys
import urllib.parse

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCAN = ['.github/workflows/*.yml', '.github/actions/*/action.yml']
SELF_REPO = 'os-santiago/ai-sdlc'
SHA_RE = re.compile(r'^[0-9a-f]{40}$')
VERSION_TOKEN = re.compile(r'v?\d+(?:\.\d+)*')

# A `uses:` line is anchored: optional `- ` step marker, then the key.
# Lines starting with `#` (commented examples) can never match, and the
# ref charset excludes `${{ }}` expressions, which skip naturally.
USES_RE = re.compile(
    r'^(?P<lead>\s*-?\s*uses:\s*)'
    r'(?P<q>["\']?)'
    r'(?P<target>[^\s"\'#]+@[^\s"\'#]+)'
    r'(?P=q)'
    r'(?P<rest>[^\n]*)$')


def err(msg):
    print(f"[X] {msg}", file=sys.stderr)


def note(msg):
    print(f"[✓] {msg}")


def scan_files():
    out = []
    for pat in SCAN:
        out.extend(sorted(glob.glob(os.path.join(ROOT, pat))))
    return out


def iter_uses(path):
    """Yield (lineno, match) for every line carrying a `uses:` ref."""
    with open(path) as fh:
        for i, line in enumerate(fh, 1):
            m = USES_RE.match(line.rstrip('\n'))
            if m:
                yield i, m


def split_target(target):
    """`owner/repo[/sub/path]@ref` -> (api_repo, sub_path, ref)."""
    path, _, ref = target.partition('@')
    parts = path.split('/')
    return '/'.join(parts[:2]), '/'.join(parts[2:]), ref


def is_exempt(api_repo, target, ref):
    if api_repo == SELF_REPO:
        return True          # runtime_ref convention — see docstring
    if target.startswith(('.', '/')):
        return True          # local action — the checkout is the pin
    if not ref or '${{' in ref or ref.startswith('$'):
        return True          # dynamic ref — nothing to pin
    return False


def gh_json(endpoint):
    """gh api <endpoint> -> parsed JSON, or None on failure."""
    try:
        out = subprocess.run(
            ['gh', 'api', endpoint], capture_output=True, text=True,
            check=True)
    except FileNotFoundError:
        sys.exit("[X] `gh` CLI not found — needed for ref resolution "
                 "(--check runs offline)")
    except subprocess.CalledProcessError:
        return None
    try:
        return json.loads(out.stdout)
    except ValueError:
        return None


def resolve_ref(api_repo, ref):
    """Tag/branch ref -> commit SHA (annotated tags dereferenced)."""
    enc = urllib.parse.quote(ref, safe='')
    doc = gh_json(f'repos/{api_repo}/git/ref/tags/{enc}')
    if doc is None:
        doc = gh_json(f'repos/{api_repo}/git/ref/heads/{enc}')
    if doc is None:
        raise RuntimeError(f"{api_repo}@{ref}: no matching tag or branch")
    sha, typ = doc['object']['sha'], doc['object']['type']
    if typ == 'tag':                     # annotated tag -> commit
        doc = gh_json(f'repos/{api_repo}/git/tags/{sha}')
        if doc is None or doc['object']['type'] != 'commit':
            raise RuntimeError(
                f"{api_repo}@{ref}: annotated tag deref failed")
        sha = doc['object']['sha']
    if not SHA_RE.match(sha):
        raise RuntimeError(f"{api_repo}@{ref}: resolved to non-commit "
                           f"{typ} {sha}")
    return sha


def existing_comment(rest):
    """Strip a trailing `# ...` comment from the uses-line remainder."""
    m = re.search(r'#', rest)
    return rest[m.start():].lstrip('# ').strip() if m else ''


def rewrite_line(m, sha):
    lead, q, target, rest = (m.group('lead'), m.group('q'),
                             m.group('target'), m.group('rest'))
    path, _, ref = target.partition('@')
    tail = existing_comment(rest)
    comment = f"# {ref}" + (f" — {tail}" if tail else "")
    return f"{lead}{q}{path}@{sha}{q}  {comment}"


def collect_plan():
    """site -> rewrite plan; unique (api_repo, ref) resolution set."""
    sites, todo = [], {}
    for f in scan_files():
        for lineno, m in iter_uses(f):
            target = m.group('target')
            api_repo, sub, ref = split_target(target)
            if is_exempt(api_repo, target, ref):
                continue
            rel = os.path.relpath(f, ROOT)
            if SHA_RE.match(ref):
                sites.append((rel, lineno, m, None))   # audit comment only
            else:
                todo.setdefault((api_repo, ref), target)
                sites.append((rel, lineno, m, target))
    return sites, todo


def check():
    """Offline audit: every third-party uses: is SHA-pinned + commented."""
    fails = 0
    for rel, lineno, m, _ in collect_plan()[0]:
        target = m.group('target')
        ref = target.rpartition('@')[2]
        if not SHA_RE.match(ref):
            err(f"{rel}:{lineno}: `{target}` — third-party uses: ref "
                "is not a commit SHA (run tools/pin-actions.py)")
            fails += 1
        elif not (m.group('rest').strip().startswith('#')
                  and VERSION_TOKEN.search(m.group('rest'))):
            err(f"{rel}:{lineno}: `{target}` — SHA-pinned but missing "
                "the `# <version>` comment Dependabot maintains")
            fails += 1
    if fails:
        sys.exit(f"[X] {fails} third-party uses: ref(s) unpinned or "
                 "uncommented — run tools/pin-actions.py")
    note("workflows/actions: every third-party uses: is a commit SHA "
         "with a version comment")


def apply(dry_run):
    sites, todo = collect_plan()
    resolved = {}
    for api_repo, ref in sorted(todo):
        resolved[(api_repo, ref)] = resolve_ref(api_repo, ref)
        note(f"{api_repo}@{ref} -> {resolved[(api_repo, ref)]}")

    edits, skipped = {}, 0
    for rel, lineno, m, target in sites:
        if target is None:               # already SHA-pinned
            continue
        api_repo, _, ref = split_target(target)
        newline = rewrite_line(m, resolved[(api_repo, ref)])
        edits.setdefault(rel, {})[lineno] = newline
        skipped += 1
        print(f"  {rel}:{lineno}: {newline.strip()}")

    if not edits:
        note("nothing to pin — all third-party uses: already SHA-pinned")
        return
    if dry_run:
        note(f"dry-run: {skipped} rewrite(s) across {len(edits)} "
             "file(s) — no files touched")
        return
    for rel, lines in edits.items():
        path = os.path.join(ROOT, rel)
        with open(path) as fh:
            src = fh.readlines()
        for lineno, newline in lines.items():
            src[lineno - 1] = newline + '\n'
        with open(path, 'w') as fh:
            fh.writelines(src)
    note(f"pinned {skipped} uses: ref(s) across {len(edits)} file(s)")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('--check', action='store_true',
                    help='offline audit: fail on unpinned/uncommented refs')
    ap.add_argument('--dry-run', action='store_true',
                    help='print planned rewrites without writing files')
    args = ap.parse_args()
    os.chdir(ROOT)
    if args.check:
        check()
    else:
        apply(args.dry_run)


if __name__ == '__main__':
    main()
