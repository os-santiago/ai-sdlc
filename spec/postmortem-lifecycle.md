# Postmortem Issue Lifecycle — dedup, reopen, runbook ingest (spec)

Normative contract for how recurring failure findings become issues —
and how those issues come back. Postmortem producers (verify-stage
failures, ci-repair manifests, run-ledger analysis) emit *occurrences*;
the tracker keeps **one canonical issue per `signatureHash`** so a
recurring failure mode has exactly one observable home, recurrences
reopen that home instead of spawning duplicates, and every close feeds
the failure-mode runbook. Without this contract the tracker fills with
same-bug issues and resolved regressions go silent — the two failure
modes the lifecycle exists to prevent.

The canonical state names in this document are fixed. Per-deployment
bindings (label strings, env knobs, runbook stores) MAY be renamed; the
semantics MUST NOT change.

## Terms

| Term | Definition |
|---|---|
| `occurrence` | One observed failure instance emitted by a postmortem producer. Carries at minimum `component`, `class`, `location`. |
| `signatureHash` | Canonical identity of a failure mode — see below. |
| `canonical issue` | The single issue that owns a `signatureHash`; every occurrence of that signature attaches to it. |
| `postmortem issue` | An issue filed through this lifecycle (machine-authored). A human-filed issue carrying the marker is governed identically. |

## Canonical identity — `signatureHash`

```
signatureHash = normalize(component + class + location)
```

- `component` — the pipeline stage or subsystem that produced the
  occurrence (`verify`, `ci-repair`, `automerge`, `implement`, …).
- `class` — the failure class (`test-failure`, `provider-auth`,
  `merge-conflict`, `lint-error`, …).
- `location` — the stable locus: file path, check-run name, module, or
  endpoint. Volatile detail — line numbers, timestamps, run ids, commit
  SHAs, PR/issue numbers — is **not** part of `location`.

`normalize()` MUST be:

1. **Deterministic** — identical inputs hash identically across runs and
   processes.
2. **Stable** — unchanged across runtime versions. Changing
   normalization re-keys every signature in flight: a spec-breaking
   (MAJOR) change.
3. **Insensitive to volatile text** — lowercase, whitespace-collapsed,
   stripped of timestamps/ids/pointer detail, so the same failure mode
   hashes identically every time it recurs.

Rules:

- **One canonical issue per `signatureHash` per component.** A second
  issue carrying an existing signature is a defect: the later issue is
  closed `duplicate` and linked to the canonical — the lowest-numbered
  carrier wins.
- The canonical issue MUST carry its `signatureHash` in a
  machine-readable marker so dedup can find it by search. The marker
  follows the runtime's fenced-comment convention
  (`<!-- ai-sdlc:review sha=… -->`, `<!-- ai-sdlc:ci-repair … -->`):
  `<!-- ai-sdlc:postmortem signature=<signatureHash> -->` in the body.
- A human MAY file an issue colliding with a live signature; it is
  treated like any duplicate — closed in favor of the canonical, or
  adopted as the canonical when it is the lowest-numbered carrier.

## Lifecycle — states and transitions

One signature, one state machine:

```
occurrence ──dedup miss──▶ filed/open ──resolve──▶ closed ──count ≥ MIN──▶ reopened
                                                      │  ▲                        │
                                                      │  └─ count < MIN:         │ resolve
                                                      │     comment-only entry,  ▼
                                                      │     stays closed      (closed)
                                                      │
                                                      └─ closedAt > retention:
                                                        expires from dedup scope —
                                                        next occurrence is a miss

deferred ──hold clears──▶ filed      entry lane: an occurrence recorded while
                                     filing is held (autonomy ceiling, transient
                                     create failure); dedup re-runs on flush —
                                     never a blind create

reopened ≡ open: subsequent occurrences are comment-only entries until
the next close starts a fresh count.
```

| # | From → To | Trigger | Effect |
|---|---|---|---|
| L1 | (none) → `filed`/`open` | occurrence, dedup miss | new canonical issue created carrying the signature marker |
| L2 | `open`/`reopened` → `closed` | resolution (human or pipeline) | **failure-mode runbook ingest fires** |
| L3 | `closed` → `closed` | occurrence, post-close count < `POSTMORTEM_REOPEN_MIN` | comment-only occurrence entry appended |
| L4 | `closed` → `reopened` | post-close count reaches `POSTMORTEM_REOPEN_MIN` | issue reopened + recurrence comment; count resets |
| L5 | `open`/`reopened` → unchanged | occurrence while active | comment-only occurrence entry — never a re-reopen |
| L6 | (occurrence) → `deferred` | filing held: autonomy ceiling forbids issue creation, or the create call failed | occurrence persisted; retry is bounded |
| L7 | `deferred` → `filed` | hold clears | dedup re-runs first (a canonical may have appeared); miss → create, hit → attach as occurrence |

`closedAt` aging past the retention window is not a state transition —
the issue stays `closed`; it simply leaves dedup scope (below).

## Dedup

Before any create, the producer MUST search for a live canonical:

- **Scope:** issues in the same repo carrying the signature marker,
  filtered to `component`; match is exact on `signatureHash`.
- **Window:** **all open issues** (open canonicals never age out) **∪
  closed issues with `closedAt ≥ now − 90 days`**. The 90-day retention
  window is measured on `closedAt` — a canonical closed longer ago is
  history, not identity, and a new occurrence files a new canonical
  (the old issue is untouched).
- **Re-filing an identical `signatureHash` while a canonical is in scope
  is forbidden.** A duplicate observed anyway is drift, handled like the
  label state machine's forbidden pairs: close it `duplicate`, link the
  canonical, log a defect — never leave two live carriers.
- The search is bounded (most-recent-first, capped result set) — every
  mutable step in this lifecycle is bounded per the repo's hard rules.

## Reopen vs comment-only

Post-close occurrences accumulate on the closed canonical. Let `count`
be occurrences observed since the latest `closedAt` (0 while open):

- `count < POSTMORTEM_REOPEN_MIN` → **comment-only**: each occurrence
  appends an entry to the closed issue — timestamp, producer,
  occurrence reference (run id / check run). The issue stays closed.
- `count ≥ POSTMORTEM_REOPEN_MIN` → **reopen**: the canonical returns to
  `open` and a recurrence comment is posted (count, window covered,
  occurrence refs) so a human sees the evidence trail, not just a flip.
- `POSTMORTEM_REOPEN_MIN` default **2**, configurable per deployment
  (integer ≥ 1). `1` reopens on the first recurrence; higher values
  trade signal latency for tracker quiet.
- The count resets on every close→reopen cycle. Occurrences landing on
  an `open`/`reopened` canonical are always comment-only (L5).
- Occurrence and recurrence comments SHOULD carry the marker
  (`<!-- ai-sdlc:postmortem signature=… count=… -->`) so `count` is
  re-derivable from the API rather than trusted to producer memory.

## Runbook ingest on close

Every `closed` transition of a canonical issue — first close and each
close after a reopen — triggers **failure-mode runbook ingest**:

1. The runtime extracts the failure-mode entry
   `{signatureHash, component, class, location, rootCause, resolution}`
   from the issue thread.
2. The entry is ingested into the deployment's runbook store, keyed on
   `signatureHash`.
3. Subsequent occurrences of the signature SHOULD match the ingested
   entry, so future postmortems and repair prompts start from a known
   failure mode instead of re-diagnosing from zero.

Ingest is **idempotent on `signatureHash`** — a close after reopen
updates the existing entry, never creates a second. Ingest failure MUST
NOT block or reverse the close: it logs a defect and retries bounded;
the issue stays closed. The producer shape is already emitted by
ci-repair manifests as `failure: {signature, rootCause}` "for future
runbook matching" (`docs/runtime.md`) — this lifecycle is the consumer
that record was designed for.

## Near-identical signatures

Distinct hashes that plausibly describe the same failure mode —
different `location` spelling, adjacent `class` — are **linked as
related issues** between their canonicals ("related to #N"), never
merged:

- `signatureHash` values are **never merged and never rewritten** —
  identity is append-only; two near-identical signatures remain two
  canonicals.
- **Fuzzy text similarity is out of scope** for dedup: matching is
  exact after `normalize()` — no edit distance, embeddings, or
  clustering in the dedup path. If normalization proves too coarse, fix
  `normalize()` (a MAJOR spec change), not the matcher.

## Machine-checkable decision table

```
on_occurrence(o):
  sig = normalize(o.component + o.class + o.location)
  c   = search(signature=sig, component=o.component,
               state=open ∪ (closed AND closedAt ≥ now − 90d))
  if c is none:
      if filing_allowed: file canonical (marker in body) → open
      else:              record deferred; bounded retry → L7
  elif c.state ∈ {open, reopened}:
      append occurrence comment                          # L5
  else:                                                  # closed, in window
      append occurrence comment                          # L3
      c.count += 1
      if c.count ≥ POSTMORTEM_REOPEN_MIN (default 2):    # L4
          reopen c + recurrence comment; c.count = 0

on_close(issue):
  if issue carries a signature marker:
      runbook.ingest({signatureHash, component, class, location,
                      rootCause, resolution})            # idempotent
```

## Parameters

| Parameter | Default | Meaning |
|---|---|---|
| `POSTMORTEM_REOPEN_MIN` | `2` | post-close occurrences that reopen the canonical; integer ≥ 1, configurable per deployment |
| dedup retention window | 90 days on `closedAt` | how long a closed canonical still dedup-matches |
| `signatureHash` | `normalize(component + class + location)` | canonical identity; exact match only |

## Notes for implementers

- **Dedup before create, always.** There is no "file first, dedup later"
  path — a deferred flush re-runs the search, and a duplicate observed
  anyway is repaired drift, not an accepted state.
- **Markers make state re-derivable.** Signature, occurrence, and
  recurrence comments all carry `ai-sdlc:postmortem` markers so a fresh
  runtime reconstructs `count` and canonicality from the API alone.
- **Bounded like everything else.** Dedup scans, deferred retries, and
  ingest retries are all capped; a permanently unfiled occurrence
  escalates (`needs-human` path) rather than vanishing — the golden rule
  applies to postmortems too.
- Divergences between a runtime's postmortem behavior and this contract
  are bugs — report them, don't accommodate them.
