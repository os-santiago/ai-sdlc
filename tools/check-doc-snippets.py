#!/usr/bin/env python3
"""tools/check-doc-snippets.py — keep documented caller snippets honest.

Issue #138: docs/runtime.md and README.md carry hand-maintained caller
blocks (uses/with/secrets) for the reusable stages. When a `workflow_call`
contract changes the docs drift — consumers copy stale caller blocks that
fail or silently skip inputs. This check fails the stale doc, not the
consumer.

Two passes, zero dependencies (indentation parsing — no PyYAML required):

A. Caller blocks
   - `.github/workflows/*.yml`: every job-level `uses: ./.github/workflows/X`
     call is checked against X's `on.workflow_call` contract.
   - `docs/runtime.md` + `README.md`: every ```yaml fenced caller block whose
     `uses:` names `*/.github/workflows/X.yml` is checked the same way.
   For each call site:
     * the callee file must exist under .github/workflows/
     * every `with:` key must be a declared workflow_call input
     * every `required: true` input must be mapped
     * `secrets: inherit` is rejected — explicit per-stage maps only (#137)
     * every `secrets:` key must be a declared workflow_call secret
     * every `required: true` secret must be mapped

B. Referenced paths
   Backticked tokens and bash-fence commands that name repo-rooted paths
   (`scripts/…`, `tools/…`, `docs/…`, `spec/…`, `tests/…`, `examples/…`,
   `config/…`, `runtime/…`, `.github/…`, `.ai-sdlc.yaml`) must resolve —
   a documented file that does not exist is drift. Bare `ai-sdlc-*.yml`
   names resolve to `.github/workflows/`. References to files that live in
   OTHER repos (noted EXTERNAL below) are allowlisted.

Exit 0 = clean, 1 = violations.
"""
import glob
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DOCS = ['docs/runtime.md', 'README.md']
WF_DIR = '.github/workflows'

# Backticked paths that legitimately point at other repositories — never
# resolved against this tree. Each entry needs a comment justifying it.
EXTERNAL_REFS = {
    'scripts/alert_router.js',   # Hermes upstream file (Axel-DaMage/hermes)
                                 # — correct spelling of THEIR path, not ours
}

NS_PREFIX = ('scripts/', 'tools/', 'docs/', 'spec/', 'tests/', 'examples/',
             'config/', 'runtime/', '.github/')
WF_NAME_RE = re.compile(r'^ai-sdlc[\w.-]*\.yml$')
NS_TOKEN_RE = re.compile(
    r'^((?:scripts|tools|docs|spec|tests|examples|config|runtime|\.github)/'
    r'[\w./@-]+|\.ai-sdlc\.yaml)$')
KEY_RE = re.compile(r'^(\s*)([A-Za-z0-9_-]+)\s*:\s*(.*?)\s*$')
USES_RE = re.compile(r'^(\s*)uses:\s*([^#\s]+)')
PLACEHOLDER = set('<>*${}')


def err(msg):
    print(f"[X] {msg}", file=sys.stderr)


def note(msg):
    print(f"[✓] {msg}")


# ---------------------------------------------------------------- contract
def workflow_call_contract(path):
    """Indentation-parse on.workflow_call.{inputs,secrets} → {name: required}.

    Line-oriented (no YAML lib): stops being relevant once `jobs:` starts —
    run: blocks can carry arbitrary text, but nothing under `on:` can.
    """
    inputs, secrets = {}, {}
    stack = []  # (indent, key)
    for raw in open(path, encoding='utf-8'):
        line = raw.rstrip('\n')
        s = line.strip()
        if not s or s.startswith('#'):
            continue
        ind = len(line) - len(line.lstrip(' '))
        m = KEY_RE.match(line)
        if not m:
            continue
        key, val = m.group(2), m.group(3)
        while stack and stack[-1][0] >= ind:
            stack.pop()
        path_keys = [k for _, k in stack]
        in_wc = path_keys[:2] == ['on', 'workflow_call']
        if in_wc and len(path_keys) == 2 and key in ('inputs', 'secrets'):
            stack.append((ind, key))
        elif (len(path_keys) == 3 and in_wc and
              path_keys[2] in ('inputs', 'secrets')):
            tgt = inputs if path_keys[2] == 'inputs' else secrets
            if val == '':
                tgt[key] = False
                stack.append((ind, key))
            elif val.startswith('{') and val.endswith('}'):
                # flow-map member: {required: true, ...}
                tgt[key] = bool(re.search(r'required\s*:\s*true', val))
        elif (len(path_keys) == 4 and in_wc and
              path_keys[2] in ('inputs', 'secrets') and key == 'required'):
            tgt = inputs if path_keys[2] == 'inputs' else secrets
            tgt[path_keys[3]] = val.split('#')[0].strip().lower() == 'true'
        else:
            stack.append((ind, key))
    return {'inputs': inputs, 'secrets': secrets}


def callee_file(ref):
    """Resolve a uses: ref to a local workflow basename, or None."""
    ref = ref.strip().strip('\'"').split('@')[0]
    if '/.github/workflows/' in ref:
        return os.path.join(WF_DIR, ref.rsplit('/.github/workflows/', 1)[1])
    if ref.startswith('./.github/workflows/'):
        return ref[2:]
    return None


# ------------------------------------------------------------- call sites
def collect_map(lines, j, parent_ind):
    """Collect `key:` entry names of a map whose parent key sits at
    parent_ind. Returns (names, next_index)."""
    names, ind0 = [], None
    while j < len(lines):
        l = lines[j]
        if l.strip() == '' or l.lstrip().startswith('#'):
            j += 1
            continue
        ind = len(l) - len(l.lstrip(' '))
        if ind <= parent_ind:
            break
        if ind0 is None:
            ind0 = ind
        if ind == ind0:
            m = re.match(r'^\s*([A-Za-z0-9_-]+)\s*:', l)
            if m:
                names.append(m.group(1))
        j += 1
    return names, j


def call_sites(lines):
    """Yield (lineno, callee_ref, with_keys, secrets|'inherit'|None) for every
    job-level `uses:` that targets a .github/workflows/*.yml path."""
    i = 0
    while i < len(lines):
        line = lines[i]
        m = None if line.lstrip().startswith('#') else USES_RE.match(line)
        if not m:
            i += 1
            continue
        ref = m.group(2).strip()
        if '/.github/workflows/' not in ref and \
                not ref.startswith('./.github/workflows/'):
            i += 1
            continue
        ind = len(m.group(1))
        withs, secrets = [], None
        j = i + 1
        while j < len(lines):
            l = lines[j]
            if l.strip() == '' or l.lstrip().startswith('#'):
                j += 1
                continue
            lind = len(l) - len(l.lstrip(' '))
            # with:/secrets: are SIBLINGS of uses: — same indent. The job
            # block ends only on a dedent below it (next job/top-level key).
            if lind < ind:
                break
            km = KEY_RE.match(l)
            if km and lind == ind:
                k, v = km.group(2), km.group(3)
                # value sans trailing comment — `secrets:   # note` is an
                # empty map header, `secrets: inherit` is the banned form
                v = v.split('#')[0].strip()
                if k == 'with' and v == '':
                    withs, j = collect_map(lines, j + 1, ind)
                    continue
                if k == 'secrets':
                    if v == 'inherit':
                        secrets = 'inherit'
                    elif v == '':
                        secrets, j = collect_map(lines, j + 1, ind)
                        continue
            j += 1
        yield i + 1, ref, withs, secrets
        i += 1


def check_site(src, lineno, ref, withs, secrets, contracts, fails):
    cf = callee_file(ref)
    if not cf or not os.path.isfile(cf):
        fails.append(f"{src}:{lineno}: uses {ref} — callee not found in "
                     f"{WF_DIR}/ (stale doc example)")
        return
    contract = contracts.get(cf) or workflow_call_contract(cf)
    contracts[cf] = contract
    decl_in, decl_sec = contract['inputs'], contract['secrets']
    loc = f"{src}:{lineno} → {os.path.basename(cf)}"
    for w in withs:
        if w not in decl_in:
            fails.append(f"{loc}: with `{w}` is not a declared "
                         f"workflow_call input (typo or drift)")
    for name, req in decl_in.items():
        if req and name not in withs:
            fails.append(f"{loc}: required input `{name}` not mapped")
    if secrets == 'inherit':
        fails.append(f"{loc}: `secrets: inherit` — map secrets explicitly, "
                     f"only what the callee declares (issue #137)")
    elif isinstance(secrets, list):
        for s in secrets:
            if s not in decl_sec:
                fails.append(f"{loc}: secret `{s}` is not declared in the "
                             f"callee's workflow_call secrets block")
        for name, req in decl_sec.items():
            if req and name not in secrets:
                fails.append(f"{loc}: required secret `{name}` not mapped")
    else:  # no secrets: block at all
        miss = [n for n, r in decl_sec.items() if r]
        if miss:
            fails.append(f"{loc}: required secret(s) {', '.join(miss)} not "
                         f"mapped — callee fails closed without them")


# ------------------------------------------------------------ fenced blocks
def fenced(lines, langs=('yaml', 'yml')):
    """Concatenate the lines of every ```lang fenced block (kept separate so
    call sites never span blocks). Yields (start_lineno, block_lines)."""
    i = 0
    while i < len(lines):
        s = lines[i].strip()
        if s.startswith('```'):
            lang = s[3:].strip()
            j = i + 1
            block = []
            while j < len(lines) and not lines[j].strip().startswith('```'):
                block.append(lines[j])
                j += 1
            if lang in langs:
                yield i + 2, block
            i = j + 1
        else:
            i += 1


# --------------------------------------------------------------- main pass
def main():
    root = ROOT
    if len(sys.argv) > 2 and sys.argv[1] == '--root':
        root = os.path.abspath(sys.argv[2])
    os.chdir(root)
    fails, checks = [], 0

    contracts = {}

    # A1 — real callers in this repo's workflows
    for wf in sorted(glob.glob(os.path.join(root, WF_DIR, '*.yml'))):
        lines = open(wf, encoding='utf-8').read().splitlines()
        for lineno, ref, withs, secrets in call_sites(lines):
            checks += 1
            check_site(wf, lineno, ref, withs, secrets, contracts, fails)

    # A2 — documented caller blocks in docs
    for doc in DOCS:
        p = os.path.join(root, doc)
        if not os.path.isfile(p):
            continue
        lines = open(p, encoding='utf-8').read().splitlines()
        for start, block in fenced(lines):
            for off, ref, withs, secrets in call_sites(block):
                checks += 1
                check_site(doc, start + off - 1, ref, withs, secrets,
                           contracts, fails)

    # B — referenced paths must resolve
    for doc in DOCS:
        p = os.path.join(root, doc)
        if not os.path.isfile(p):
            continue
        lines = open(p, encoding='utf-8').read().splitlines()
        seen = set()
        fence_lang, in_fence = None, False
        for n, l in enumerate(lines, 1):
            s = l.strip()
            if s.startswith('```'):
                if not in_fence:
                    in_fence, fence_lang = True, s[3:].strip()
                else:
                    in_fence = False
                continue
            tokens = list(re.findall(r'`([^`\s]+)`', l))
            if in_fence and fence_lang in ('bash', 'sh', 'console'):
                tokens += re.findall(
                    r'(?<![\w./-])((?:scripts|tools|docs|spec|tests|examples|'
                    r'config|runtime|\.github)/[\w./@-]+|\.ai-sdlc\.yaml)', l)
            for t in tokens:
                t = t.rstrip('.,;:')
                if any(c in PLACEHOLDER for c in t):
                    continue
                if (doc, t) in seen:
                    continue
                seen.add((doc, t))
                base = t.split('@')[0]  # strip uses:-style @ref
                if base in EXTERNAL_REFS:
                    continue
                if WF_NAME_RE.match(base):
                    checks += 1
                    if not os.path.isfile(os.path.join(WF_DIR, base)):
                        fails.append(f"{doc}:{n}: `{t}` — no such workflow "
                                     f"under {WF_DIR}/")
                    continue
                if NS_TOKEN_RE.match(base):
                    checks += 1
                    if not os.path.exists(os.path.join(root, base)):
                        fails.append(f"{doc}:{n}: `{t}` — referenced path "
                                     f"does not exist")
    if fails:
        for f in fails:
            err(f)
        sys.exit(1)
    note(f"doc snippets: {checks} caller/secret/path assertions clean")


if __name__ == '__main__':
    main()
