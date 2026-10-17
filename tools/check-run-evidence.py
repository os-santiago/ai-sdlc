#!/usr/bin/env python3
"""tools/check-run-evidence.py — lint upload-artifact names/paths against
the canonical run-evidence layout (issue #141).

spec/run-evidence.md carries the machine-readable layout in a ```json
fence marked `<!-- ai-sdlc:run-evidence -->`. Every
`uses: actions/upload-artifact` step in `.github/workflows/*.yml` and
`.github/actions/*/action.yml` must:

  * name the upload after a declared entry — `match` templates use `<e>`
    for one `${{ }}` expression; `regex` entries search the raw name text;
  * upload only from the declared path root — `runner_temp` requires every
    path under `${{ runner.temp }}/ai-sdlc-`; `workspace` requires every
    path to match the entry's declared `paths` globs;
  * declare `retention-days` equal to the declared retention when the
    entry has one.

Zero dependencies — indentation parsing, no YAML lib required.
Exit 0 = clean, 1 = violations.
"""
import fnmatch
import glob
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SPEC = 'spec/run-evidence.md'
SCAN = ['.github/workflows/*.yml', '.github/actions/*/action.yml']
EXPR = re.compile(r'\$\{\{[^}]*\}\}')


def err(msg):
    print(f"[X] {msg}", file=sys.stderr)


def note(msg):
    print(f"[✓] {msg}")


def layout(root):
    """Parse the marked ```json block out of spec/run-evidence.md."""
    lines = open(os.path.join(root, SPEC), encoding='utf-8').read().splitlines()
    in_mark, in_json, buf = False, False, []
    for l in lines:
        if l.strip() == '<!-- ai-sdlc:run-evidence -->':
            in_mark = True
            continue
        if in_mark and not in_json and l.strip().startswith('```'):
            in_json = True
            continue
        if in_json and l.strip().startswith('```'):
            break
        if in_json:
            buf.append(l)
    if not buf:
        sys.exit(f"[X] {SPEC}: no machine block after "
                 f"'<!-- ai-sdlc:run-evidence -->'")
    return json.loads('\n'.join(buf))['artifacts']


def upload_steps(path):
    """Yield (lineno, name, [paths], retention) per upload-artifact step."""
    lines = open(path, encoding='utf-8').read().splitlines()
    i = 0
    while i < len(lines):
        l = lines[i]
        if 'uses:' not in l or 'actions/upload-artifact' not in l or \
                l.lstrip().startswith('#'):
            i += 1
            continue
        ucol = l.index('uses')
        j, name, paths, retention, wcol = i + 1, None, [], None, None
        while j < len(lines):
            s = lines[j]
            if s.strip() == '' or s.lstrip().startswith('#'):
                j += 1
                continue
            ind = len(s) - len(s.lstrip(' '))
            if ind < ucol or (ind == ucol and s.lstrip().startswith('- ')):
                break
            m = re.match(r'^\s*([A-Za-z0-9_-]+)\s*:\s*(.*?)\s*$', s)
            if m and ind == ucol and m.group(1) == 'with':
                wcol = None  # next deeper indent defines the with-map
            elif m and wcol is None and ind > ucol:
                wcol = ind
            if m and wcol is not None and ind == wcol:
                key, val = m.group(1), m.group(2).split('#')[0].strip()
                if key == 'name':
                    name = val.strip('\'"')
                elif key == 'retention-days':
                    retention = val
                elif key == 'path':
                    if val and val not in ('|', '>', '|-', '>-'):
                        paths.append(val)
                    else:
                        k = j + 1
                        while k < len(lines):
                            p = lines[k]
                            if p.strip() == '':
                                k += 1
                                continue
                            pind = len(p) - len(p.lstrip(' '))
                            if pind <= ind:
                                break
                            t = p.strip().lstrip('- ').strip()
                            t = t.split('#')[0].strip()
                            if t:
                                paths.append(t)
                            k += 1
                        j = k - 1
            j += 1
        yield i + 1, name, paths, retention
        i = j


def normalized(name):
    return EXPR.sub('<e>', name or '').strip()


def match_entry(entries, f, name):
    """Return (entry, reason): entry when a file-scoped declaration matches,
    else None + failure reason."""
    scoped = [e for e in entries if f in (e.get('files') or [f])]
    for e in scoped:
        if e.get('match') and normalized(name) == e['match']:
            return e, None
        if e.get('regex') and re.search(e['regex'], name or ''):
            return e, None
    # did it match an entry declared for other files?
    for e in entries:
        if (e.get('match') and normalized(name) == e['match']) or \
                (e.get('regex') and re.search(e['regex'], name or '')):
            return None, (f"name matches '{e['id']}' but is not declared "
                          f"for this file")
    return None, "no declared artifact matches this name"


def main():
    root = ROOT
    if len(sys.argv) > 2 and sys.argv[1] == '--root':
        root = os.path.abspath(sys.argv[2])
    os.chdir(root)
    entries = layout(root)
    fails, checks = [], 0
    for pat in SCAN:
        for af in sorted(glob.glob(os.path.join(root, pat))):
            f = os.path.relpath(af, root)
            for ln, name, paths, retention in upload_steps(af):
                checks += 1
                loc = f"{f}:{ln}"
                e, why = match_entry(entries, f, name)
                if not e:
                    fails.append(f"{loc}: artifact name `{name}` — {why} "
                                 f"(declare it in {SPEC})")
                    continue
                proot = e.get('path_root')
                for p in paths:
                    if proot == 'runner_temp':
                        if not p.startswith('${{ runner.temp }}/ai-sdlc-'):
                            fails.append(f"{loc}: path `{p}` — canonical "
                                         f"evidence lives under "
                                         f"${{{{ runner.temp }}}}/ai-sdlc-<stage>/")
                    elif proot == 'workspace':
                        p2 = p[2:] if p.startswith('./') else p
                        if not any(fnmatch.fnmatch(p2, g)
                                   for g in e.get('paths', [])):
                            fails.append(f"{loc}: path `{p}` — workspace "
                                         f"uploads are a closed set; declare "
                                         f"it in the '{e['id']}' paths globs")
                if e.get('retention') is not None:
                    if retention is None:
                        fails.append(f"{loc}: missing retention-days — "
                                     f"declare {e['retention']} per the layout")
                    elif retention != str(e['retention']):
                        fails.append(f"{loc}: retention-days {retention} — "
                                     f"layout declares {e['retention']}")
    if fails:
        for f in fails:
            err(f)
        sys.exit(1)
    note(f"run-evidence layout: {checks} upload steps conform")


if __name__ == '__main__':
    main()
