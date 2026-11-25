"""Model backend adapters behind one loopback completion contract.

The worker and verifier always speak the same authenticated, bounded
OpenAI-style chat-completions contract to their loopback gateway. The gateway
validates every request with the shared contract, then hands it to exactly one
adapter selected by the installation:

  agent-cli  a local agent CLI used as a completion model (Codex CLI today)
  openai     any OpenAI-compatible Chat Completions endpoint (llama.cpp,
             vLLM, Ollama, LM Studio, LiteLLM, hosted OpenAI-compatible APIs)
  anthropic  the Anthropic Messages API (or a compatible proxy)

Adapters never execute tools: proposed calls are normalized and bounded by
``assistant_message`` and executed only by SCC. The hunk counts of proposed
``edit_file`` patches, redundant metadata models often get wrong, are then
recomputed from the hunk bodies (``recount_edit_patches``). An adapter receives only the
validated messages and tool schemas, plus the single upstream URL, model alias
and optional credential the installation configured. Adding a provider means
adding one class to ``ADAPTERS``; the gateway, worker and chart are unchanged.

The ``openai`` adapter streams from its upstream (``stream: true``) and
assembles the same final message the non-streaming contract returns, so it can
stop a degenerate generation (``DegenerateGuard``) as soon as it starts,
instead of waiting for it to run to ``max_tokens``. It then retries the same
request at most once, inside the same call budget.

Every adapter also refuses, once per call, a ``write_file`` that would replace
a whole file which existed before the conversation (``RewriteGuard``), judged
only from the request's own messages. The ``openai`` adapter stops such a
stream as soon as the call's path is known. It also refuses a ``write_file``
to a new path whose content copies a file the model read (the copy guard),
as soon as enough content has streamed. The request is then repeated with one
short correction appended, inside the same call budget; one such retry per
call in total, and a second refusal is passed through unchanged (fail open).
"""
import http.client
import ipaddress
import json
import os
import re
import socket
import threading
import time
import urllib.error
import urllib.request
from collections import namedtuple
from dataclasses import dataclass, field
from urllib.parse import urlsplit

from codexcli_openai_wrapper import MAX_RESPONSE_BYTES, assistant_message, validate_request

# The worker/verifier broker allows the gateway timeout + 30 s (bounded at 600).
UPSTREAM_SECONDS_MAX = 570
# The startup benchmark derives timeouts of at least 30 s.
UPSTREAM_SECONDS_MIN = 30
# Explicit RFC 1918 / ULA ranges; ipaddress.is_private also admits
# documentation and other special ranges.
PRIVATE_NETWORKS = tuple(ipaddress.ip_network(n) for n in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"))
MODEL_ALIAS = re.compile(r"[A-Za-z0-9._:/-]{1,128}\Z")
# Some models emit an inline reasoning block even with thinking disabled.
THINK = re.compile(r"\A\s*<think>.*?</think>\s*", re.S)
MAX_CREDENTIAL_BYTES = 4096
# Hunk-count repair (``recount_edit_patches``). The tool-argument bound of
# ``assistant_message`` is 32 KiB, so this never skips a call it admitted.
EDIT_TOOL = "edit_file"
MAX_EDIT_ARGUMENT_BYTES = 32 * 1024
MAX_EDIT_HUNKS = 512
# SCC's own header check; starts and counts are bounded to nine digits.
HUNK_HEADER = re.compile(r"@@ -(\d{1,9})(?:,(\d{1,9}))? \+(\d{1,9})(?:,(\d{1,9}))? @@((?: .*)?)\Z", re.A | re.S)
FILE_HEADER = re.compile(r"(?:---|\+\+\+)\s")
NO_NEWLINE = "\\ No newline at end of file"
# Streaming (``openai`` adapter). Each SSE event carries one token or so with
# its JSON envelope (about 300 bytes against llama.cpp), so the raw stream of a
# 8,192-token answer is about 2.5 MiB; the assembled message keeps the
# non-streaming bound (MAX_RESPONSE_BYTES).
MAX_STREAM_BYTES = 32 * MAX_RESPONSE_BYTES
MAX_STREAM_LINE_BYTES = MAX_RESPONSE_BYTES
MAX_STREAM_CALLS = 8
# One retry after a degenerate answer, only when at least this much of the
# call budget remains: a shorter retry could not produce a useful answer, and
# failing as ``degenerate`` names the cause better than a later ``deadline``.
DEGENERATE_RETRIES = 1
DEGENERATE_RETRY_MIN_SECONDS = 10
# Rewrite guard (``RewriteGuard``): SCC's tool names and argument keys.
READ_TOOL, WRITE_TOOL = "read_file", "write_file"
REWRITE_GUARD_ENV = "AI_SDLC_MODEL_GATEWAY_REWRITE_GUARD"
REWRITE_RETRIES = 1
REWRITE_RETRY_MIN_SECONDS = DEGENERATE_RETRY_MIN_SECONDS
MAX_GUARD_PATH_CHARS = 1024
# The path text repeated to the model in the correction.
MAX_CORRECTION_PATH_CHARS = 200
# A streamed write_file whose "path" is not among the leading scalar keys of
# its first bytes is judged on the final message instead.
REWRITE_SCAN_CHARS = 4096
_JSON_STRING = r'"(?:[^"\\]|\\.)*"'
_JSON_SCALAR = _JSON_STRING + r"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null"
STREAMED_PATH = re.compile(r'\s*\{\s*(?:%s\s*:\s*(?:%s)\s*,\s*)*"path"\s*:\s*(%s)\s*[,}]' % (_JSON_STRING, _JSON_SCALAR, _JSON_STRING), re.S)
# SCC reports a failed tool as "Error: ..." or "[ERROR] ...". These failures
# happen only after SCC has opened the file, so they still prove it exists.
TOOL_ERROR_PREFIXES = ("Error:", "[ERROR]")
EXISTING_FILE_ERRORS = ("Patch does not match file content", "Patch produces no changes",
                        "File too large", "exceeds file length")
CORRECTION = ("write_file on the existing file %s was refused: it would replace the whole file and drop "
              "what you do not repeat. Change it with edit_file (a unified diff), or put new code and new "
              "tests in a new file.")
# Copy guard (``_CopyWatch``). A new file's content is judged on its distinct
# non-trivial lines (whitespace runs collapsed; at least 4 letters or digits,
# so braces, blank lines and short imports such as "os" never count). Once it
# has at least COPY_MIN_LINES of them, it is a copy of one read file when that
# file holds at least COPY_MIN_MATCHED of them and at least COPY_FRACTION of
# all of them. A new test file shares its package clause, imports and a few
# table-test lines with its neighbours (about 10 to 15 distinct lines), far
# below both bounds; a copied file matches nearly every line.
COPY_MIN_LINES = 40
COPY_MIN_MATCHED = 30
COPY_FRACTION = 0.6
COPY_LINE_MIN_ALNUM = 4
COPY_LINE_MAX_CHARS = 1024
# Distinct lines tracked per streamed file; past it, the content is left alone.
COPY_MAX_LINES = 4096
COPY_CONTENT = re.compile(r'"content"\s*:\s*"')
# SCC's note after a read: "[N lines total]" or "[N lines shown, M total. ...]".
READ_NOTE = re.compile(r"\n\n\[\d+ lines (?:total|shown, \d+ total\. [^\n]*)\]\Z")
COPY_CORRECTION = ("write_file to %s was refused: it repeats content of %s that already exists. Write only "
                   "the new code and tests the issue asks for; never copy existing tests or code into a new file.")


@dataclass
class Completion:
    """One bounded assistant message plus what the backend reported about it."""
    message: dict
    prompt_tokens: int = 0
    completion_tokens: int = 0
    estimated: bool = True
    usage: dict = field(default_factory=dict)
    edit_patches_recounted: int = 0
    degenerate_retries: int = 0
    rewrite_retries: int = 0
    rewrite_passed: int = 0
    copy_retries: int = 0
    copy_passed: int = 0


def _hunks(lines):
    """One pass over a patch's lines: each hunk's header match and its counted
    body (old, new, trailing blank lines). ``None`` when anything is ambiguous."""
    hunks, preamble_headers, current = [], 0, None
    for index, line in enumerate(lines):
        if line.startswith("@@"):
            match = HUNK_HEADER.match(line)
            if match is None or len(hunks) == MAX_EDIT_HUNKS:
                return None
            current = {"index": index, "match": match, "old": 0, "new": 0, "blank": 0}
            hunks.append(current)
        elif current is None:
            # Before the first hunk: optional metadata and one file's header.
            preamble_headers += bool(FILE_HEADER.match(line))
        elif line == "":
            current["blank"] += 1
        elif line[0] == "\\":
            if line != NO_NEWLINE or current["blank"]:
                return None
        elif line[0] not in " +-":
            return None
        elif FILE_HEADER.match(line):
            # jsdiff starts a new file at any "--- " or "+++ " line; a removed
            # "-- x" or added "++ x" line is ambiguous with it, so leave it.
            return None
        else:
            # Blank lines followed by more body are context lines.
            current["old"] += current["blank"] + (line[0] != "+")
            current["new"] += current["blank"] + (line[0] != "-")
            current["blank"] = 0
    if not hunks or preamble_headers > 2:
        return None
    return hunks


def recount_patch(patch):
    """Returns ``patch`` with its hunk headers' counts recomputed from the hunk
    bodies, or ``None`` when nothing needs repair or anything is ambiguous.

    The counts are redundant metadata: SCC's parser (jsdiff ``parsePatch`` and
    a strict header check) reads exactly as many body lines as they declare,
    so a wrong count makes a correct edit unusable. Context lines (' ', or a
    bare empty line, a common model omission) count on both sides, '-' on the
    old side, '+' on the new side, the no-newline marker on neither. Starts,
    section text and every other line are kept byte for byte. Blank lines
    ending a hunk are counted only when the declared counts already include
    them: the parser skips them otherwise, and context after the last change
    never alters the result.

    ``None`` (leave the patch unchanged) when: the patch is over the bound or
    holds a carriage return; it has no hunk or more than one file; a line
    starting with ``@@`` is not a well-formed header; a body line has an
    unknown prefix or is an unknown backslash line; a hunk counts no line;
    the declared counts are already consistent; or a recount would change
    whether a side is empty (which changes the meaning of its start) or
    contradict a start of 0. Linear in the patch size."""
    if (not isinstance(patch, str) or len(patch.encode()) > MAX_EDIT_ARGUMENT_BYTES
            or "\r" in patch):
        return None
    lines = patch.split("\n")
    if lines[-1] == "":
        lines.pop()  # the patch's final newline, not an empty line
    hunks = _hunks(lines)
    if hunks is None:
        return None
    headers = {}
    for hunk in hunks:
        match, old, new = hunk["match"], hunk["old"], hunk["new"]
        old_start, new_start = int(match.group(1)), int(match.group(3))
        declared_old = int(match.group(2)) if match.group(2) is not None else 1
        declared_new = int(match.group(4)) if match.group(4) is not None else 1
        if old == 0 and new == 0:
            return None
        surplus = declared_old - old
        if surplus == declared_new - new and 0 <= surplus <= hunk["blank"]:
            continue  # consistent: the parser reads `surplus` trailing blanks
        if ((old == 0) != (declared_old == 0) or (new == 0) != (declared_new == 0)
                or (old_start == 0 and old != 0) or (new_start == 0 and new != 0)):
            return None
        headers[hunk["index"]] = "@@ -%d,%d +%d,%d @@%s" % (old_start, old, new_start, new, match.group(5))
    if not headers:
        return None
    for index, header in headers.items():
        lines[index] = header
    return "\n".join(lines) + ("\n" if patch.endswith("\n") else "")


def recount_edit_patches(message):
    """Repairs the hunk counts of ``edit_file`` proposals in a bounded assistant
    message, in place, and returns how many calls were repaired. Every other
    call, and every call whose patch is not repaired, keeps its exact argument
    bytes; a repaired call keeps all its other keys and values."""
    repaired = 0
    for call in message.get("tool_calls") or []:
        try:
            repaired += _recount_call(call)
        except Exception:  # a repair never fails the call; the call stays as sent
            continue
    return repaired


def _recount_call(call):
    """Repairs one call in place; 1 when repaired, else 0 with no change."""
    function = call.get("function") if isinstance(call, dict) else None
    if not isinstance(function, dict) or function.get("name") != EDIT_TOOL:
        return 0
    raw = function.get("arguments")
    if not isinstance(raw, str) or len(raw.encode()) > MAX_EDIT_ARGUMENT_BYTES:
        return 0
    try:
        arguments = json.loads(raw)
    except (ValueError, RecursionError):
        return 0
    patch = recount_patch(arguments.get("patch")) if isinstance(arguments, dict) else None
    if patch is None:
        return 0
    arguments["patch"] = patch
    encoded = json.dumps(arguments, ensure_ascii=False)
    if len(encoded.encode()) > MAX_EDIT_ARGUMENT_BYTES:
        return 0
    function["arguments"] = encoded
    return 1


class DegenerateCompletion(ValueError):
    """A generation that degenerated into one repeated character or short
    unit. ``degenerate_retries`` is how many retries were spent on it."""
    def __init__(self, retries=0):
        super().__init__("degenerate completion")
        self.degenerate_retries = retries


class DegenerateGuard:
    r"""Detects a generation that has degenerated into repetition, while it
    streams, from a bounded tail of each generated text (the content, any
    reasoning text, and each tool call's arguments, kept separately).

    Observed fault: a local GPU backend turned the content into ``?`` (and, in
    another capture, ``G``) repeated until ``max_tokens``. A text is
    degenerate when it ends with:

      * one non-whitespace character repeated: 256 times (``RUN``, e.g.
        ``?``), 512 times for an ASCII letter or digit (``ALNUM_RUN``), 1,024
        times for a separator (``SEPARATOR_RUN``); or
      * a unit of 2 to 4 characters, not only whitespace, repeated over the
        last ``WINDOW`` (1,024) characters with no line break in them.

    Code and data repeat, so the bounds are chosen above what one line of
    source plausibly holds. A line break (a raw newline, or the ``\n`` escape
    inside JSON tool arguments) clears both checks, so a separator line of any
    length followed by a newline never accumulates. Letters and digits get 512
    because one-line data literals repeat them: base64 of zero bytes
    (``AAAA``), hex of zeros or ``ff``, a ``"a" * n`` literal written out; the
    cost is that a letter loop (the observed ``G``) is stopped after 512
    single-character tokens instead of 256. Separators (``-=#*/_~.+|`` and
    the Unicode box-drawing block, e.g. ``─═│║┄┈``) get 1,024: comment rules
    and table borders. Units get 1,024 so minified literals (``0,`` repeated,
    ``|---`` table rules) up to that length pass. Runs of whitespace are never
    flagged (indentation and alignment repeat legitimately). Each check is a
    constant amount of work per streamed delta."""
    RUN = 256
    ALNUM_RUN = 512
    SEPARATOR_RUN = 1024
    WINDOW = 1024
    UNITS = (2, 3, 4)
    SEPARATORS = frozenset("-=#*/_~.+|") | frozenset(map(chr, range(0x2500, 0x2580)))
    ALNUM = frozenset("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")
    KEEP = 1024

    def __init__(self):
        self.tails = {}

    def feed(self, channel, text):
        """Appends ``text`` to ``channel``; True when it is now degenerate."""
        if not text:
            return False
        tail = (self.tails.get(channel, "") + text)[-self.KEEP:]
        self.tails[channel] = tail
        return self.degenerate(tail)

    @classmethod
    def degenerate(cls, tail):
        last = tail[-1:]
        if not last:
            return False
        if not last.isspace():
            run = len(tail) - len(tail.rstrip(last))
            bound = cls.SEPARATOR_RUN if last in cls.SEPARATORS else cls.ALNUM_RUN if last in cls.ALNUM else cls.RUN
            if run >= bound:
                return True
        if len(tail) < cls.WINDOW:
            return False
        window = tail[-cls.WINDOW:]
        if "\n" in window or "\\n" in window:
            return False
        for size in cls.UNITS:
            unit = tail[-size:]
            if not unit.strip() or len(set(unit)) == 1:
                continue  # whitespace, or a single character (checked above)
            if window[size:] == window[:-size]:
                return True
        return False


class RewriteRefused(ValueError):
    """A proposed ``write_file`` the guard refused while it streamed: a whole
    rewrite of a pre-existing file, or a new file copying one the model read
    (``refusal``, a ``Refusal``)."""
    def __init__(self, refusal):
        super().__init__("rewrite refused")
        self.refusal = refusal
        self.path = refusal.path


# kind: "rewrite" or "copy"; source: the read file a copy repeats (or None).
Refusal = namedtuple("Refusal", "kind path source")


def guard_path(path):
    """The path the rewrite guard compares: "." segments (a leading "./") and
    repeated separators removed, nothing else resolved. ``None`` when the
    value is not a usable path."""
    if not isinstance(path, str) or not path or len(path) > MAX_GUARD_PATH_CHARS or "\x00" in path:
        return None
    parts = [part for part in path.split("/") if part not in ("", ".")]
    if not parts:
        return None
    return ("/" if path.startswith("/") else "") + "/".join(parts)


def _call_arguments(call):
    """(tool name, normalized path, arguments) of one OpenAI-shaped tool
    call, or (None, None, None) when it has no usable name and path."""
    function = call.get("function") if isinstance(call, dict) else None
    raw = function.get("arguments") if isinstance(function, dict) else None
    if not isinstance(raw, str) or not isinstance(function.get("name"), str):
        return None, None, None
    try:
        arguments = json.loads(raw)
    except (ValueError, RecursionError):
        return None, None, None
    path = guard_path(arguments.get("path")) if isinstance(arguments, dict) else None
    return (function["name"], path, arguments) if path is not None else (None, None, None)


def _call_target(call):
    """(tool name, normalized path) of one tool call, or (None, None)."""
    return _call_arguments(call)[:2]


def _shown(path):
    """A path bounded, its non-printable characters replaced, and quoted as a
    JSON string so it reads as data, never as instructions."""
    shown = "".join(c if c.isprintable() else "?" for c in path[:MAX_CORRECTION_PATH_CHARS])
    shown += "..." if len(path) > MAX_CORRECTION_PATH_CHARS else ""
    return json.dumps(shown, ensure_ascii=False)


def correction_text(path):
    """The fixed correction for a refused rewrite of ``path``."""
    return CORRECTION % _shown(path)


def copy_correction_text(path, source):
    """The fixed correction for a new file ``path`` that copies ``source``."""
    return COPY_CORRECTION % (_shown(path), _shown(source))


def copy_line(line):
    """A line as the copy guard compares it: whitespace runs collapsed and the
    ends stripped. ``None`` for a trivial line (fewer than
    ``COPY_LINE_MIN_ALNUM`` letters or digits: braces, blank lines, ``"os"``)."""
    line = " ".join(line[:COPY_LINE_MAX_CHARS].split())
    return line if sum(c.isalnum() for c in line) >= COPY_LINE_MIN_ALNUM else None


def copy_lines(text):
    """The distinct non-trivial lines of ``text``."""
    return {line for line in map(copy_line, text.split("\n")) if line is not None}


class _CopyWatch:
    """Counts, over the distinct non-trivial lines of a new file's content,
    how many each read file already holds. ``add`` returns the source a copy
    repeats as soon as the content qualifies (see ``COPY_*``)."""
    def __init__(self, target, sources):
        self.target, self.sources = target, sources
        self.seen = set()
        self.matched = dict.fromkeys(sources, 0)

    def add(self, line):
        line = copy_line(line)
        if line is None or line in self.seen or len(self.seen) >= COPY_MAX_LINES:
            return None
        self.seen.add(line)
        for source, lines in self.sources.items():
            if line in lines:
                self.matched[source] += 1
        if len(self.seen) < COPY_MIN_LINES:
            return None
        for source in sorted(self.sources):
            matched = self.matched[source]
            if matched >= COPY_MIN_MATCHED and matched >= COPY_FRACTION * len(self.seen):
                return source
        return None


class _StreamedString:
    """Decodes a JSON string value incrementally from the raw arguments of a
    streamed call, from ``pos`` on, and returns each completed line. An
    escape split across deltas waits for its remaining characters."""
    ESCAPES = {'"': '"', "\\": "\\", "/": "/", "b": "\b", "f": "\f", "n": "\n", "r": "\r", "t": "\t"}

    def __init__(self, pos):
        self.pos, self.line, self.done = pos, [], False

    def feed(self, raw):
        lines, i, end = [], self.pos, len(raw)
        while i < end and not self.done:
            char = raw[i]
            if char == '"':
                self.done = True
                lines.append("".join(self.line))
                break
            if char == "\\":
                if i + 1 >= end:
                    break
                escape = raw[i + 1]
                if escape == "u":
                    if i + 6 > end:
                        break
                    try:
                        char = chr(int(raw[i + 2:i + 6], 16))
                    except ValueError:
                        self.done = True
                        break
                    i += 6
                elif escape in self.ESCAPES:
                    char, i = self.ESCAPES[escape], i + 2
                else:
                    self.done = True
                    break
            else:
                i += 1
            if char == "\n":
                lines.append("".join(self.line))
                self.line = []
            elif len(self.line) < COPY_LINE_MAX_CHARS:
                self.line.append(char)
        self.pos = i
        return lines


class _Call:
    """What one ``complete`` shares across its upstream attempts: one deadline,
    one degenerate-retry allowance, and the rewrite guard while it may still
    stop a stream."""
    def __init__(self, deadline):
        self.deadline = deadline
        self.degenerate_retries = 0
        self.rewrite = None


class RewriteGuard:
    """Refuses, once per call, a ``write_file`` that either replaces a whole
    file which existed before the conversation (a rewrite) or creates a new
    file that copies a file the model read (a copy).

    Observed faults: a local model rewrote an 834-line test file in about 100
    lines with ``write_file``, dropping most of its tests and a helper; every
    candidate for those issues then failed verification. Told to put new tests
    in a new file, the same model then copied every existing test into it
    (about 6,000 tokens, until the call's ceiling); duplicate test functions
    do not even compile in one package.

    The gateway reads no workspace, so the guard decides only from the
    request's own conversation. A path existed before it when an earlier
    assistant ``read_file`` or ``edit_file`` on it succeeded (or failed only
    after SCC opened the file) before any ``write_file`` of the model created
    it. A ``write_file`` to a new path, or to a path the model itself created
    earlier, is never refused as a rewrite. It is refused as a copy when its
    content repeats a successful ``read_file`` result of another path (all
    reads of one path together, partial reads included) by the ``COPY_*``
    bounds. Paths are compared after ``guard_path``."""

    def __init__(self, request, messages, deadline, can_retry=True, copies=True):
        self.request, self.deadline, self.can_retry = request, deadline, can_retry
        self.existing = self.preexisting(messages)
        self.reads = self.read_lines(messages) if copies else {}
        self.streams = {}
        self.retries = {}

    @staticmethod
    def _results(messages):
        results = {}
        for message in messages:
            if message.get("role") == "tool" and isinstance(message.get("tool_call_id"), str):
                results[message["tool_call_id"]] = message.get("content") or ""
        return results

    @staticmethod
    def _calls(messages, results):
        """Each assistant tool call in order: (name, path, arguments, result)."""
        for message in messages:
            calls = message.get("tool_calls") if message.get("role") == "assistant" else None
            for call in calls if isinstance(calls, list) else []:
                name, path, arguments = _call_arguments(call)
                call_id = call.get("id") if name is not None else None
                yield name, path, arguments, results.get(call_id) if isinstance(call_id, str) else None

    @staticmethod
    def preexisting(messages):
        existing, created = set(), set()
        for name, path, _, result in RewriteGuard._calls(messages, RewriteGuard._results(messages)):
            if name in (READ_TOOL, EDIT_TOOL) and path not in created and RewriteGuard.opened(result):
                existing.add(path)
            elif name == WRITE_TOOL and path not in existing:
                created.add(path)
        return frozenset(existing)

    @staticmethod
    def read_lines(messages):
        """The distinct non-trivial lines each path's successful reads hold.
        SCC's trailing "[N lines ...]" note is not file content."""
        reads = {}
        for name, path, _, result in RewriteGuard._calls(messages, RewriteGuard._results(messages)):
            if name == READ_TOOL and isinstance(result, str) and not result.startswith(TOOL_ERROR_PREFIXES):
                reads.setdefault(path, set()).update(copy_lines(READ_NOTE.sub("", result)))
        return {path: frozenset(lines) for path, lines in reads.items() if lines}

    @staticmethod
    def opened(result):
        """Whether SCC's result for a read or edit shows the file existed."""
        if not isinstance(result, str):
            return False  # no result in the conversation: nothing is known
        if not result.startswith(TOOL_ERROR_PREFIXES):
            return True
        return any(marker in result for marker in EXISTING_FILE_ERRORS)

    def watch(self, path):
        """A copy watch for a new file at ``path``, or ``None`` when no other
        read file could be its source."""
        sources = {source: lines for source, lines in self.reads.items() if source != path}
        return _CopyWatch(path, sources) if sources else None

    def refused(self, message):
        """The first refusal a final message holds, or ``None``."""
        for call in message.get("tool_calls") or []:
            name, path, arguments = _call_arguments(call)
            if name != WRITE_TOOL:
                continue
            if path in self.existing:
                return Refusal("rewrite", path, None)
            watch = self.watch(path)
            content = arguments.get("content")
            if watch is not None and isinstance(content, str):
                for line in content.split("\n"):
                    source = watch.add(line)
                    if source is not None:
                        return Refusal("copy", path, source)
        return None

    def retry(self, refusal):
        """The validated retry request for ``refusal``, or ``None`` when the
        corrected request leaves the contract (too many messages or bytes)
        or too little of the call budget remains, or the adapter cannot keep
        a retry inside the deadline."""
        if not self.can_retry or self.deadline - time.monotonic() < REWRITE_RETRY_MIN_SECONDS:
            return None
        if refusal not in self.retries:
            text = (correction_text(refusal.path) if refusal.kind == "rewrite"
                    else copy_correction_text(refusal.path, refusal.source))
            request = dict(self.request, messages=list(self.request["messages"]) + [{"role": "user", "content": text}])
            try:
                validate_request(request)
            except ValueError:
                request = None
            self.retries[refusal] = request
        return self.retries[refusal]

    def _stop(self, index, refusal):
        """Decides call ``index``; stops it only when a retry is ready."""
        self.streams[index] = None
        return refusal if self.retry(refusal) is not None else None

    def streamed(self, index, name, arguments):
        """The refusal to stop the stream for, as soon as it is known and a
        retry is possible; otherwise ``None``. A rewrite is known when a
        streamed ``write_file`` call's complete ``path`` value is a
        pre-existing file: its content is never waited for. A copy is known
        when the content streamed so far qualifies. Each call is decided
        once; what streaming cannot decide is left to the final message."""
        if name != WRITE_TOOL or (index in self.streams and self.streams[index] is None):
            return None
        state = self.streams.get(index)
        if state is None:
            match = STREAMED_PATH.match(arguments[:REWRITE_SCAN_CHARS])
            if match is None:
                if len(arguments) >= REWRITE_SCAN_CHARS:
                    self.streams[index] = None  # left to the final message
                return None
            try:
                path = guard_path(json.loads(match.group(1)))
            except (ValueError, RecursionError):
                path = None
            if path in self.existing:
                return self._stop(index, Refusal("rewrite", path, None))
            watch = self.watch(path) if path is not None else None
            if watch is None:
                self.streams[index] = None
                return None
            state = self.streams[index] = [watch, None]
        watch, content = state
        if content is None:
            start = COPY_CONTENT.search(arguments, 0, REWRITE_SCAN_CHARS)
            if start is None:
                if len(arguments) >= REWRITE_SCAN_CHARS:
                    self.streams[index] = None
                return None
            content = state[1] = _StreamedString(start.end())
        for line in content.feed(arguments):
            source = watch.add(line)
            if source is not None:
                return self._stop(index, Refusal("copy", watch.target, source))
        if content.done:
            self.streams[index] = None
        return None


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def validate_parameters(request, names):
    """Optional sampling controls are typed and bounded before any backend sees them."""
    temperature = request.get("temperature")
    if temperature is not None and (type(temperature) not in (int, float) or not 0 <= temperature <= 2):
        raise ValueError("invalid temperature")
    choice = request.get("tool_choice")
    if choice is None or choice in ("auto", "none", "required"):
        return
    if (isinstance(choice, dict) and set(choice) == {"type", "function"} and choice["type"] == "function"
            and isinstance(choice["function"], dict) and set(choice["function"]) == {"name"}
            and choice["function"]["name"] in names):
        return
    raise ValueError("invalid tool choice")


def fixed_url(url, path):
    """One installation URL: https anywhere, or plain http only to a literal
    private address (a local bridge), never a hostname DNS could redirect."""
    parts = urlsplit(url or "")
    if (not url or len(url) > 512 or parts.path != path or parts.query or parts.fragment
            or parts.username or parts.password or parts.scheme not in ("http", "https") or not parts.hostname):
        raise RuntimeError("model upstream unavailable")
    if parts.scheme == "http":
        try:
            address = ipaddress.ip_address(parts.hostname)
        except ValueError:
            raise RuntimeError("model upstream unavailable") from None
        if not any(address in network for network in PRIVATE_NETWORKS):
            raise RuntimeError("model upstream unavailable")
    return url


def read_credential(path):
    if not path:
        return ""
    try:
        with open(path, "rb") as handle:
            raw = handle.read(MAX_CREDENTIAL_BYTES + 1)
    except OSError:
        raise RuntimeError("model upstream credential unavailable") from None
    value = raw.decode("ascii", errors="strict").strip() if len(raw) <= MAX_CREDENTIAL_BYTES else ""
    if not value or any(c.isspace() or not c.isprintable() for c in value):
        raise RuntimeError("model upstream credential unavailable")
    return value


class _Deadline:
    """Shuts down the upstream socket at one total deadline, covering connect,
    TLS, headers and a slowly dripped body."""
    def __init__(self, seconds):
        self.expired = threading.Event()
        self.sockets = []
        self.lock = threading.Lock()
        self.timer = threading.Timer(seconds, self.fire)
        self.timer.daemon = True

    def track(self, sock):
        with self.lock:
            self.sockets.append(sock)
            if self.expired.is_set():
                self._close(sock)

    @staticmethod
    def _close(sock):
        try:
            sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass

    def fire(self):
        with self.lock:
            self.expired.set()
            for sock in self.sockets:
                self._close(sock)

    def abort(self):
        """Disconnects now (a degenerate answer) without marking expiry."""
        with self.lock:
            for sock in self.sockets:
                self._close(sock)


def _tracked(base, deadline):
    class Connection(base):
        def connect(self):
            super().connect()
            deadline.track(self.sock)
    return Connection


class _HTTP(urllib.request.HTTPHandler):
    def __init__(self, deadline):
        super().__init__()
        self.deadline = deadline

    def http_open(self, req):
        return self.do_open(_tracked(http.client.HTTPConnection, self.deadline), req)


class _HTTPS(urllib.request.HTTPSHandler):
    def __init__(self, deadline):
        super().__init__()
        self.deadline = deadline

    def https_open(self, req):
        return self.do_open(_tracked(http.client.HTTPSConnection, self.deadline), req,
                            context=self._context)


class _Stop(Exception):
    """Ends reading a response early with ``error``, which is raised as is."""
    def __init__(self, error):
        super().__init__()
        self.error = error


def _exchange(url, payload, headers, seconds, read):
    """POST once with no proxy and no redirects, then ``read(response,
    deadline)``. One total deadline covers the whole call: a watchdog armed
    before connecting shuts the socket down. ``read`` ends early by raising
    ``_Stop``; the connection is then shut down before the error is raised."""
    raw = json.dumps(payload).encode()
    call = urllib.request.Request(url, data=raw, method="POST", headers=dict(headers, **{"Content-Type": "application/json"}))
    deadline = _Deadline(seconds)
    opener = urllib.request.build_opener(_NoRedirect(), urllib.request.ProxyHandler({}), _HTTP(deadline), _HTTPS(deadline))
    deadline.timer.start()
    try:
        with opener.open(call, timeout=seconds) as response:
            if response.status != 200:
                raise RuntimeError("upstream status")
            result = read(response, deadline)
    except _Stop as stop:
        deadline.abort()
        if deadline.expired.is_set():  # a body cut short by the deadline
            raise RuntimeError("completion deadline exceeded") from None
        raise stop.error from None
    except (urllib.error.URLError, OSError, TimeoutError, ValueError, http.client.HTTPException, AttributeError):
        if deadline.expired.is_set():
            raise RuntimeError("completion deadline exceeded") from None
        raise RuntimeError("completion backend failed") from None
    finally:
        deadline.timer.cancel()
    if deadline.expired.is_set():
        raise RuntimeError("completion deadline exceeded")
    return result


def _read_body(response, deadline):
    body = b""
    while len(body) <= MAX_RESPONSE_BYTES and not deadline.expired.is_set():
        chunk = response.read1(16384)
        if not chunk:
            break
        body += chunk
    if len(body) > MAX_RESPONSE_BYTES:
        raise _Stop(ValueError("completion response exceeded bound"))
    return body


def post_json(url, payload, headers, seconds):
    """POST once and return the bounded JSON answer (see ``_exchange``)."""
    return json.loads(_exchange(url, payload, headers, seconds, _read_body))


class _Stream:
    """Assembles one streamed Chat Completions answer (``chat.completion.chunk``
    events) into what the non-streaming answer holds: the content, the tool
    calls (by ``index``; string fields concatenate, as the OpenAI SDKs
    accumulate them), the last finish reason and the last usage object.
    Every delta is fed to the guard before it is kept. With a ``rewrite``
    guard, a tool call is also checked as its arguments arrive."""
    def __init__(self, guard, rewrite=None):
        self.guard, self.rewrite, self.content, self.calls = guard, rewrite, [], {}
        self.finish, self.usage, self.done, self.size = None, None, False, 0
        self.data, self.data_size = [], 0  # the current event's data lines

    def keep(self, channel, text):
        # A lone surrogate (a JSON "\ud800" escape) is counted, not refused
        # here: assistant_message judges it exactly as for a JSON answer.
        self.size += len(text.encode("utf-8", "surrogatepass"))
        if self.size > MAX_RESPONSE_BYTES:
            raise _Stop(ValueError("completion response exceeded bound"))
        if self.guard.feed(channel, text):
            raise _Stop(DegenerateCompletion())

    def line(self, line):
        """One SSE line (its LF removed; a CR before it is removed here).
        ``data:`` fields accumulate until a blank line ends the event, as the
        SSE format defines: several ``data:`` lines join with a line feed, and
        one leading space is removed from each value. Comments (``:``, such
        as keep-alive pings) and other fields are skipped; an event without
        data is not dispatched. Lines ended by a bare CR alone are not split,
        so such a stream fails closed (``invalid``, or ``oversized`` past the
        line bound)."""
        line = line[:-1] if line.endswith(b"\r") else line
        if not line:
            self.dispatch()
            return
        field, _, value = line.partition(b":")
        if field != b"data":
            return  # a comment (empty field name) or a field the chunks do not use
        value = value[1:] if value.startswith(b" ") else value
        self.data_size += len(value) + 1
        if self.data_size > MAX_STREAM_LINE_BYTES:
            raise _Stop(ValueError("completion response exceeded bound"))
        self.data.append(value)

    def dispatch(self):
        """Parses the accumulated event; ``[DONE]`` ends the stream."""
        if not self.data:
            return
        data = b"\n".join(self.data)
        self.data, self.data_size = [], 0
        if not data.strip():
            return  # empty data: nothing to dispatch
        if data.strip() == b"[DONE]":
            self.done = True
            return
        try:
            chunk = json.loads(data)
        except (ValueError, RecursionError):
            raise _Stop(ValueError("invalid completion")) from None
        try:
            self.chunk(chunk)
        except (AttributeError, TypeError):
            raise _Stop(ValueError("invalid completion")) from None

    def chunk(self, chunk):
        if not isinstance(chunk, dict):
            raise _Stop(ValueError("invalid completion"))
        if "error" in chunk:  # an upstream failure reported mid-stream
            raise _Stop(RuntimeError("completion backend failed"))
        if isinstance(chunk.get("usage"), dict):
            self.usage = chunk["usage"]
        choices = chunk.get("choices") or []
        if not isinstance(choices, list) or len(choices) > 1:
            raise _Stop(ValueError("invalid completion"))
        for choice in choices:
            delta = choice.get("delta") or {}
            if choice.get("index", 0) != 0 or not isinstance(delta, dict) or delta.get("role", "assistant") not in ("assistant", None):
                raise _Stop(ValueError("invalid completion"))
            # Reasoning text (llama.cpp ``reasoning_content``, vLLM ``reasoning``)
            # is watched, never kept.
            for key, channel in (("content", "content"), ("reasoning_content", "reasoning"), ("reasoning", "reasoning")):
                text = delta.get(key)
                if text is not None and not isinstance(text, str):
                    raise _Stop(ValueError("invalid completion"))
                if key == "content" and text:
                    self.keep(channel, text)
                    self.content.append(text)
                elif text and self.guard.feed(channel, text):
                    raise _Stop(DegenerateCompletion())
            for call in delta.get("tool_calls") or []:
                self.call(call)
            if choice.get("finish_reason") is not None:
                self.finish = choice["finish_reason"]

    def call(self, call):
        function = call.get("function") or {} if isinstance(call, dict) else None
        index = call.get("index") if isinstance(call, dict) else None
        if index is None and isinstance(call, dict):
            # Servers that omit the index: a new id starts the next call.
            new = call.get("id") and all(c["id"] != call["id"] for c in self.calls.values())
            index = len(self.calls) if new or not self.calls else max(self.calls)
        if (not isinstance(function, dict) or type(index) is not int or not 0 <= index < MAX_STREAM_CALLS
                or call.get("type", "function") not in ("function", None)):
            raise _Stop(ValueError("invalid proposed tool"))
        entry = self.calls.setdefault(index, {"id": "", "name": "", "arguments": ""})
        for key, value in (("id", call.get("id")), ("name", function.get("name")), ("arguments", function.get("arguments"))):
            if value is None:
                continue
            if not isinstance(value, str):
                raise _Stop(ValueError("invalid proposed tool"))
            if key == "id":
                entry["id"] = entry["id"] or value
            elif key == "name":
                self.keep(("name", index), value)
                entry["name"] += value
            else:
                self.keep(("arguments", index), value)
                entry["arguments"] += value
        if self.rewrite is not None:
            try:
                path = self.rewrite.streamed(index, entry["name"], entry["arguments"])
            except Exception:
                path, self.rewrite = None, None  # the guard never fails a stream
            if path is not None:
                raise _Stop(RewriteRefused(path))

    def read(self, response, deadline):
        buffer, total = b"", 0
        while not self.done and not deadline.expired.is_set():
            chunk = response.read1(16384)
            if not chunk:
                break
            total += len(chunk)
            if total > MAX_STREAM_BYTES:
                raise _Stop(ValueError("completion response exceeded bound"))
            *lines, buffer = (buffer + chunk).split(b"\n")
            if len(buffer) > MAX_STREAM_LINE_BYTES:
                raise _Stop(ValueError("completion response exceeded bound"))
            for line in lines:
                self.line(line)
                if self.done:
                    break
        if not self.done:
            # The end of the stream ends the last line and event.
            if buffer:
                self.line(buffer)
            self.dispatch()
        return self

    def answer(self):
        """The assembled answer in the non-streaming shape. Every finished
        stream reports a finish reason; one without it (even after ``[DONE]``)
        was cut off or is malformed, and is never taken as complete."""
        if self.finish is None:
            raise RuntimeError("completion backend failed")
        message = {"role": "assistant", "content": "".join(self.content)}
        if self.calls:
            message["tool_calls"] = [{"id": c["id"], "type": "function", "function": {"name": c["name"] or None, "arguments": c["arguments"]}}
                                     for _, c in sorted(self.calls.items())]
        answer = {"choices": [{"index": 0, "message": message, "finish_reason": self.finish}]}
        if self.usage is not None:
            answer["usage"] = self.usage
        return answer


def _read_completion(response, deadline, rewrite=None):
    """An SSE stream is assembled while it is guarded; any other answer (a
    server that ignored ``stream``) is read as the bounded JSON body."""
    media = (response.headers.get("Content-Type") or "").split(";")[0].strip().lower()
    if media != "text/event-stream":
        body = _read_body(response, deadline)
        try:
            return json.loads(body)
        except (ValueError, RecursionError):
            raise _Stop(ValueError("invalid completion")) from None
    return _Stream(DegenerateGuard(), rewrite).read(response, deadline)


def post_completion(url, payload, headers, seconds, rewrite=None):
    """POST once and return the answer in the non-streaming shape, streamed
    and guarded when the upstream streams (see ``_exchange``)."""
    answer = _exchange(url, payload, headers, seconds,
                       lambda response, deadline: _read_completion(response, deadline, rewrite))
    return answer.answer() if isinstance(answer, _Stream) else answer


def _count(value):
    return value if type(value) is int and 0 <= value <= 10_000_000 else None


def _estimate(text):
    return max(1, len(text) // 4)


class Adapter:
    """Base adapter: ``complete`` takes a raw request, validates it with the
    shared contract and returns a bounded ``Completion``."""
    name = ""

    def __init__(self, url="", model="", seconds=300, credential="", direct_answers=False, rewrite_guard=True):
        self.url, self.model, self.seconds, self.credential = url, model, seconds, credential
        self.direct_answers, self.rewrite_guard = direct_answers, rewrite_guard

    def complete(self, request, seconds=None, probe=False):
        """``probe`` is used only by the startup benchmark, whose prefill and
        generation probes are truncated on purpose; SCC traffic never is.

        When the guard refuses an answer (``RewriteGuard``: a rewrite or a
        copy, one retry per call for both together), the same
        request is repeated once with one correction appended, inside the same
        ``seconds``: the streaming adapter stops the first answer as soon as
        the refused path is known, the others judge the final message. A
        retry that rewrites again is returned unchanged (fail open) and
        counted in ``rewrite_passed``, as is a refused answer when no retry
        fits. When a refused final message was complete and its retry fails,
        that first answer is returned the same way; a stream stopped early
        has no complete answer, so there the retry's failure is the call's.
        A guard that cannot judge the request is disabled for the call. One
        degenerate retry is shared by all attempts."""
        messages, tools, names, tokens = validate_request(request)
        validate_parameters(request, names)
        call = _Call(time.monotonic() + (seconds or self.seconds))
        guard = None
        # Inert when write_file is not offered (a narrowed review gateway).
        if self.rewrite_guard and not probe and WRITE_TOOL in names:
            try:
                guard = RewriteGuard(request, messages, call.deadline, can_retry=self.honors_deadline)
            except Exception:
                guard = None  # never fail a call because the guard could not judge it
        retried = passed = None  # the kind ("rewrite" or "copy") of a retry spent / a refusal returned
        kept = None  # a complete answer refused on its final message, and its refusal
        self.truncation_ok, self._call = probe, call
        try:
            while True:
                call.rewrite = guard if retried is None else None
                try:
                    completion = self.backend(request, messages, tools, names, tokens,
                                              max(call.deadline - time.monotonic(), 0.001))
                except RewriteRefused as refused:
                    # A stream is stopped only once its retry request is ready.
                    request, retried = guard.retries[refused.refusal], refused.refusal.kind
                    messages, tools, names, tokens = validate_request(request)
                    continue
                except Exception:
                    if kept is None:
                        raise
                    (completion, refusal), retry = kept, None  # fail open: no worse than without the guard
                else:
                    try:
                        refusal = guard.refused(completion.message) if guard is not None else None
                        retry = guard.retry(refusal) if refusal is not None and retried is None else None
                    except Exception:
                        guard, refusal, retry = None, None, None
                if retry is None:
                    passed = refusal.kind if refusal is not None else None
                    break
                kept, request, retried = (completion, refusal), retry, refusal.kind
                messages, tools, names, tokens = validate_request(request)
        except Exception as error:
            error.degenerate_retries = call.degenerate_retries
            error.rewrite_retries, error.copy_retries = int(retried == "rewrite"), int(retried == "copy")
            raise
        finally:
            self.truncation_ok, self._call = False, None
        completion.degenerate_retries = call.degenerate_retries
        completion.rewrite_retries, completion.rewrite_passed = int(retried == "rewrite"), int(passed == "rewrite")
        completion.copy_retries, completion.copy_passed = int(retried == "copy"), int(passed == "copy")
        # Every adapter's message is normalized here, before the gateway
        # returns it and so before the broker records or digests anything.
        completion.edit_patches_recounted = recount_edit_patches(completion.message)
        return completion

    truncation_ok = False
    _call = None
    # Whether ``backend`` keeps to the ``seconds`` it is given. An adapter
    # that does not could overrun the call's deadline on a rewrite retry, so
    # its refused rewrites are passed through instead of retried.
    honors_deadline = True

    def backend(self, request, messages, tools, names, tokens, seconds):
        raise NotImplementedError


class AgentCLIAdapter(Adapter):
    """A local agent CLI acting only as a completion model (no tool execution)."""
    name = "agent-cli"
    reports_usage = False
    # The CLI backend runs on its own fixed timeout (24 s), not ``seconds``.
    honors_deadline = False

    def backend(self, request, messages, tools, names, tokens, seconds):
        from codexcli_openai_wrapper import complete
        message = complete(request)
        text = json.dumps(messages)
        return Completion(message, _estimate(text), _estimate(message.get("content") or ""), True)


class OpenAIAdapter(Adapter):
    """OpenAI-compatible Chat Completions (``/v1/chat/completions``)."""
    name = "openai"
    path = "/v1/chat/completions"
    reports_usage = True

    def backend(self, request, messages, tools, names, tokens, seconds):
        """Streams the answer and retries once, inside the same ``seconds``
        budget, when the guard stops a degenerate generation. A rewrite the
        rewrite guard stops (``RewriteRefused``) is retried by ``complete``,
        which shares that one degenerate retry across its attempts. Any other
        failure is returned at once: retrying it is the broker's decision."""
        payload = {"model": self.model, "messages": messages, "max_tokens": tokens,
                   "stream": True, "stream_options": {"include_usage": True}}
        if self.direct_answers:
            # Direct answers: reasoning traces are not part of the contract.
            # Only for servers that template thinking (llama.cpp, vLLM).
            payload["chat_template_kwargs"] = {"enable_thinking": False}
        if tools:
            payload["tools"] = tools
        for key in ("temperature", "tool_choice"):
            if key in request:
                payload[key] = request[key]
        headers = {"Authorization": "Bearer " + self.credential} if self.credential else {}
        deadline = time.monotonic() + seconds
        # ``complete`` shares one degenerate retry across rewrite retries.
        call = self._call or _Call(deadline)
        while True:
            try:
                answer = post_completion(self.url, payload, headers, max(deadline - time.monotonic(), 0.001), call.rewrite)
            except DegenerateCompletion:
                if call.degenerate_retries < DEGENERATE_RETRIES and deadline - time.monotonic() >= DEGENERATE_RETRY_MIN_SECONDS:
                    call.degenerate_retries += 1
                    continue
                raise
            return self.parse(answer, messages, names)

    def parse(self, answer, messages, names):
        """The bounded completion from one answer in the non-streaming shape."""
        choices = answer.get("choices") if isinstance(answer, dict) else None
        if not isinstance(choices, list) or len(choices) != 1 or not isinstance(choices[0], dict):
            raise ValueError("invalid completion")
        if choices[0].get("finish_reason") == "length" and not self.truncation_ok:
            raise ValueError("truncated completion")
        message = choices[0].get("message")
        if not isinstance(message, dict) or message.get("role", "assistant") != "assistant":
            raise ValueError("invalid completion")
        content = message.get("content") or ""
        if not isinstance(content, str):
            raise ValueError("invalid completion")
        proposed = []
        for item in message.get("tool_calls") or []:
            function = item.get("function") if isinstance(item, dict) else None
            if not isinstance(function, dict) or item.get("type", "function") != "function":
                raise ValueError("invalid proposed tool")
            proposed.append({"name": function.get("name"), "arguments": function.get("arguments")})
        result = assistant_message(THINK.sub("", content), proposed, names)
        usage = answer.get("usage") if isinstance(answer.get("usage"), dict) else {}
        prompt, completion = _count(usage.get("prompt_tokens")), _count(usage.get("completion_tokens"))
        if prompt is None or completion is None:
            return Completion(result, _estimate(json.dumps(messages)), _estimate(content), True)
        return Completion(result, prompt, completion, False)


class AnthropicAdapter(Adapter):
    """Anthropic Messages API (``/v1/messages``)."""
    name = "anthropic"
    path = "/v1/messages"
    version = "2023-06-01"
    reports_usage = True

    @staticmethod
    def translate(messages, tools, request):
        system, turns = [], []

        def add(role, blocks):
            if turns and turns[-1]["role"] == role:
                turns[-1]["content"].extend(blocks)
            else:
                turns.append({"role": role, "content": list(blocks)})

        for message in messages:
            role, content = message["role"], message.get("content") or ""
            if role in ("system", "developer"):
                system.append(content)
            elif role == "user":
                add("user", [{"type": "text", "text": content}] if content else [])
            elif role == "assistant":
                blocks = [{"type": "text", "text": content}] if content else []
                calls = message.get("tool_calls") or []
                if not isinstance(calls, list) or len(calls) > 8:
                    raise ValueError("invalid assistant tool calls")
                for call in calls:
                    function = call.get("function") if isinstance(call, dict) else None
                    if (not isinstance(function, dict) or not isinstance(call.get("id"), str)
                            or not isinstance(function.get("name"), str) or not isinstance(function.get("arguments"), str)):
                        raise ValueError("invalid assistant tool call")
                    arguments = json.loads(function["arguments"])
                    if not isinstance(arguments, dict):
                        raise ValueError("invalid assistant tool call")
                    blocks.append({"type": "tool_use", "id": call["id"], "name": function["name"], "input": arguments})
                add("assistant", blocks)
            else:  # tool result
                if not isinstance(message.get("tool_call_id"), str):
                    raise ValueError("tool result without call id")
                add("user", [{"type": "tool_result", "tool_use_id": message["tool_call_id"], "content": content}])
        turns = [turn for turn in turns if turn["content"]]
        if not turns or turns[0]["role"] != "user":
            turns.insert(0, {"role": "user", "content": [{"type": "text", "text": "Continue."}]})
        payload = {"messages": turns}
        if system:
            payload["system"] = "\n\n".join(system)
        if tools:
            payload["tools"] = [{"name": t["function"]["name"], "description": t["function"].get("description", ""),
                                 "input_schema": t["function"].get("parameters") or {"type": "object"}} for t in tools]
        choice = request.get("tool_choice")
        if choice == "none" and tools:
            # Keep tools: history may hold tool_use/tool_result blocks.
            payload["tool_choice"] = {"type": "none"}
        elif choice == "required":
            payload["tool_choice"] = {"type": "any"}
        elif isinstance(choice, dict):
            payload["tool_choice"] = {"type": "tool", "name": choice["function"]["name"]}
        if "temperature" in request:
            payload["temperature"] = min(float(request["temperature"]), 1.0)
        return payload

    def backend(self, request, messages, tools, names, tokens, seconds):
        payload = dict(self.translate(messages, tools, request), model=self.model, max_tokens=tokens)
        answer = post_json(self.url, payload, {"x-api-key": self.credential, "anthropic-version": self.version}, seconds)
        if not isinstance(answer, dict) or answer.get("role") != "assistant" or not isinstance(answer.get("content"), list):
            raise ValueError("invalid completion")
        if answer.get("stop_reason") == "max_tokens" and not self.truncation_ok:
            raise ValueError("truncated completion")
        text, proposed = [], []
        for block in answer["content"]:
            kind = block.get("type") if isinstance(block, dict) else None
            if kind == "text" and isinstance(block.get("text"), str):
                text.append(block["text"])
            elif kind == "tool_use" and isinstance(block.get("input"), dict):
                proposed.append({"name": block.get("name"), "arguments": json.dumps(block["input"])})
            elif kind not in ("thinking", "redacted_thinking"):
                raise ValueError("invalid completion block")
        content = "".join(text)
        result = assistant_message(THINK.sub("", content), proposed, names)
        usage = answer.get("usage") if isinstance(answer.get("usage"), dict) else {}
        prompt, completion = _count(usage.get("input_tokens")), _count(usage.get("output_tokens"))
        if prompt is None or completion is None:
            return Completion(result, _estimate(json.dumps(messages)), _estimate(content), True)
        return Completion(result, prompt, completion, False)


ADAPTERS = {adapter.name: adapter for adapter in (AgentCLIAdapter, OpenAIAdapter, AnthropicAdapter)}


def adapter_from_environ(environ=os.environ):
    """Builds the single installation-selected adapter or refuses to start."""
    name = environ.get("AI_SDLC_MODEL_ADAPTER", "openai")
    cls = ADAPTERS.get(name)
    if cls is None:
        raise RuntimeError("model adapter unavailable")
    seconds = environ.get("AI_SDLC_MODEL_UPSTREAM_SECONDS", "300")
    if not seconds.isdigit() or not UPSTREAM_SECONDS_MIN <= int(seconds) <= UPSTREAM_SECONDS_MAX:
        raise RuntimeError("model upstream unavailable")
    # On unless the installation turns it off (the chart does so on the
    # independent review gateway: the reviewer never writes files).
    guard = environ.get(REWRITE_GUARD_ENV, "true")
    if guard not in ("true", "false"):
        raise RuntimeError("model gateway rewrite guard invalid")
    if cls is AgentCLIAdapter:
        return cls(seconds=int(seconds), rewrite_guard=guard == "true")
    url = fixed_url(environ.get("AI_SDLC_MODEL_UPSTREAM_URL", ""), cls.path)
    model = environ.get("AI_SDLC_MODEL_UPSTREAM_MODEL", "")
    if not MODEL_ALIAS.fullmatch(model):
        raise RuntimeError("model upstream unavailable")
    credential = read_credential(environ.get("AI_SDLC_MODEL_UPSTREAM_CREDENTIAL_FILE", ""))
    if cls is AnthropicAdapter and not credential:
        raise RuntimeError("model upstream credential unavailable")
    direct = environ.get("AI_SDLC_MODEL_UPSTREAM_DIRECT_ANSWERS", "false")
    if direct not in ("true", "false"):
        raise RuntimeError("model upstream unavailable")
    return cls(url, model, int(seconds), credential, direct == "true", guard == "true")
