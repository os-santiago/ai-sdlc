# Flow console

The flow console is a read-only web page that shows the autonomous flow as a
live timeline. It is meant for administrators who need to see how the flow
behaves: which stages ran, which stopped and why, where work waits, how long
each stage and model call takes, and what is blocked or quarantined.

It is read-only: it writes nothing, sends nothing to any component and grants
no workflow authority. It is off by default.

![Flow console with the evaluation-battery fixture](flow-console.png)

In Spanish, with the detail panel: [flow-console-es.png](flow-console-es.png); light theme at 1280×800: [English](flow-console-1280.png), [Spanish](flow-console-es-1280.png).

## What it shows

- **One swimlane per issue.** Each lane holds that issue's stage bars in flow
  order: intake → admission → planning → generation (execution, with its
  model calls) → verification → review → publication → merge. A retried
  attempt continues on the same lane. Only lanes with activity in the visible
  time window are listed, most recent activity first, or *problems first*
  (blocked, then failed, then in flight, then delivered).
- **A label column with a summary line**: issue, attempt count and status
  (the repository is named once next to the title when every lane shares it,
  and per lane, truncated with the full name on hover, when they do not), and under it where the work is
  now (`verifying · 3 min`, `queued for review · 40 s`) or why it ended. The
  final stop of a failed or blocked lane always shows its stage and
  diagnostic there (`generate · worker-exit-22`) and next to its last bar.
- **Stage and outcome are encoded separately.** The stage is the bar's hue,
  one fixed hue per stage (generate blue, verify orange, review green, publish
  amber, merge magenta; intake, admission and planning grey), with its name
  inside the bar when it fits and its initial (G, V, R, P, M) otherwise. The
  work-now strip above the timeline is also the stage colour key. The outcome is the bar's form: solid
  with ✓ for completed, striped with an open end for running, a dashed outline
  for waiting (and a dashed lead-in for the queue time before a claim), a hatch
  for blocked or quarantined, and an end badge for failed (✕), stopped safely
  (■), blocked (⏸) and quarantined (⛔). A run too short to see at the
  current zoom (often publish and merge) keeps its true width (at least 2 px)
  and gets a small chip with its initial above it; neighbouring chips line up
  to the right with a hairline to their run, so the time axis is never
  stretched. A dashed rule labelled `#2`, `#3`, ... marks where a later attempt
  begins inside a lane.
- **What is wrong now**: a box above the timeline, shown only when there is
  something to show, lists current problems newest first: blocked or
  quarantined work items with their reason, diagnostic and age; lanes in a
  stage (or its queue) longer than that stage's p90 over the visible window
  (`#226 generating 10 min · p90 6 min`, only once three runs of that stage
  are visible); and degradation bursts. Click a work item to open it, or a
  burst to zoom to it. Four items show by default; *show all* expands.
- **The blocked tile names what blocks**, for example `1 publication
  quarantined · 1 needs clarification · 1 lease lapsed`.
- **Work now**: a strip above the timeline counts the work items currently
  queued, generating, verifying, reviewing, publishing, merging and blocked
  or quarantined. Click a cell to show only those lanes; click it again to
  clear the filter.
- **Trends**: sparklines in the tiles over the visible window (24 buckets):
  model call latency p50 and p90, stops and refusals per hour, and
  deliveries (merges) per hour; hover a bucket for its values, or open
  *Trend values (table)* under the tiles for the same buckets as a table.
- **Degradation periods**: a red band across the lanes wherever stops,
  degradations, retries, blocks or quarantines cluster within about 15
  minutes: at least three of them, and at least twice the visible window's
  own average for 15 minutes, so a busy day is not shaded everywhere. The
  count is shown above the axis. No band does not mean no problems: the
  markers and the failed and blocked lanes still show every one.
- **Live events**: the *Live events* button opens a side panel with the
  newest activity across all work items, filterable by text and by kind
  (problems only, or stage starts and ends). By default the model calls and
  effect steps of one stage of one attempt fold into one updating row
  (`#213 generate · 14 model calls · p50 20 s · last 01:16:54`); stage
  transitions, waits, retries, stops, failures, quarantine and degradation
  keep their own rows, problems highlighted. *every model call* shows each
  event on its own row (newest 300). It refreshes on its own while *Follow now* is on;
  otherwise it offers the count of new events and keeps your position. Click
  or press Enter on an event to open its work item.
- **Markers on their own row** above the bars for retries, stale waits,
  degradations (a failed or uncertain model call, a cleanup retry), blocks and
  quarantine. Markers closer than about 15 px merge into one badge with a
  count; its tooltip lists each of them.
- **Model calls** as ticks along the generation and review bars.
- **Hover** on a bar or marker for its outcome, diagnostic, wait, start, end,
  duration, model call count and latency (median and maximum), and bounded
  attributes such as the lifecycle phase or exit code.
- **Zoom and scroll**: the 1 h, 6 h, 24 h and Fit buttons, Ctrl or ⌘ with the
  mouse wheel to zoom around the pointer, and drag (or a horizontal wheel) to
  pan. *Follow now* keeps the dashed "now" line in view as events arrive.
  Times are in the browser's local time zone, named above the label column;
  axis ticks, bars and panel times show UTC on hover.
- **Filters** by lane status (in flight, delivered, failed, blocked), by
  component (lanes that component touched; its bars stay at full opacity),
  and by issue number or repository.
- **Detail panel**: click a lane for its ordered events, grouped by attempt,
  with consecutive model calls folded into one row.
- **Summary tiles**: work in flight (running and waiting stages), delivered
  (first attempt or after a retry), failed, blocked or quarantined (with
  quarantine and stale-wait counts), median time from intake to merge, and the
  model call latency median and 90th percentile.
- **Text legend** for every colour, texture and marker.
- **Language**: English and Spanish (*ES*/*EN* button), remembered in the
  browser and defaulting to its language; `?lang=es` or `?lang=en` selects
  one in a link. Every UI string is in `internal/flowconsole/ui/i18n.js`.
  Contract values (event kinds, diagnostics, steps, components) are shown
  as they are; their human labels (kinds, outcomes, stages) are translated.
- **Issue links**: issue numbers in the lanes and the detail panel title
  link to `https://github.com/<repository>/issues/<n>`, built only from a
  repository matching the contract's pattern and the integer issue number,
  opened with `rel="noopener noreferrer"`. Clicking the number opens the
  issue; clicking the rest of the row opens the details.
- **Light theme and printing**: the ☀/☾ button switches between the dark
  and a light theme (remembered; the default follows the operating system,
  `?theme=light` in a link). Both keep text at WCAG AA. Printing (or saving
  as PDF) uses the light colours, hides the controls and panels, and draws
  every lane, not only those scrolled into view.
- **Time per stage**: the detail panel shows the latest attempt as one bar,
  each stage's queue wait (hatched) and run (solid) in order and to scale,
  with the longest part named, for example `Longest: generate 6 min 43 s
  (51% of 13 min 17 s)`.
- **Contrast**: all text meets WCAG AA (4.5:1) against every surface it sits
  on; the muted grey (`#9c9b93`) measures 6.2:1 on the panels and 4.9:1 on the
  hovered or pressed controls.
- **Keyboard**: the lane list is one tab stop; the arrow keys, Page Up/Down,
  Home and End move between lanes (scrolling as needed) and Enter or Space
  opens the details. Live updates keep the focused lane or event focused. In the detail panel and the live-events list the arrow
  keys move between events; Escape closes the panel. Every control shows a
  focus ring.
- **Reduce motion**: the button stops the running-bar animation. It follows
  the operating system's reduced-motion setting until you choose, and the
  choice is remembered in the browser.
- **Source state**: the badge at the top right shows whether the stream is
  live, when the journal was last read, and whether the journal is unavailable
  (the page then keeps the last known events).

The view is shareable: `?range=<hours>`, `?status=inflight|delivered|failed|blocked`,
`?issue=<text>`, `?sort=problems`, `?lang=es|en`, `?theme=light|dark`, `?now=queued|execution|verification|review|publication|merge|blocked`, `?feed=1` and `?open=<owner>/<repository>%23<issue>` set the initial
window, filters and detail panel.

## Event contract: `ai-sdlc.flow-event/v1`

The console consumes and serves events in one bounded format, defined by
[`spec/flow-event.schema.json`](../../spec/flow-event.schema.json) and enforced
by `runtime/k8s/internal/flowevent`:

| Field | Meaning |
|---|---|
| `version` | Always `ai-sdlc.flow-event/v1`. |
| `id` | 24 hex characters derived from the journal identity of the fact (table, key, kind). The same fact always has the same id, so consumers de-duplicate by id. |
| `time` | When the fact happened (RFC 3339, UTC), not when it was observed. |
| `component` | The component that owns `stage`: `intake`, `admission-controller`, `orchestrator`, `worker`, `verifier` (verification and review) or `release-manager` (publication and merge). |
| `repository`, `issue` | The issue the work belongs to; `issue` or `workItem` is always present. |
| `workItem`, `attempt` | The work item UUID and attempt number, when the stage has one. |
| `stage` | `intake`, `admission`, `planning`, `execution`, `verification`, `review`, `publication` or `merge`. |
| `step` | A bounded name inside the stage: an effect kind (`config`, `pod`, `tests`, `pull-request`, ...), `model-call`, `queued`, `claim`, `verdict`, `stop`, `cleanup`, `lease`. |
| `kind` | `started`, `progressed`, `completed`, `failed` (a verdict against the work), `stopped-safely` (stopped without a verdict), `waiting`, `retried`, `quarantined`, `degraded` or `blocked`. |
| `diagnostic` | A bounded name (`^[a-z0-9][a-z0-9.-]{0,79}$`), for example `worker-exit-22`, `comparison-failed`, `stale-wait`, `model-status-503`, `cleanup-uncertain`, `unresolved-effect`, `lease-lapsed`. |
| `durationMs`, `latencyMs` | Duration of the stage or step, and latency of a model call, when known. |
| `attributes` | At most eight values from a closed key set (`phase`, `exitCode`, `reason`, `decision`, `risk`, `status`, `changes`, `modelCalls`, `sequence`, `inputBytes`, `outputBytes`, `responseStatus`, `cleanup`, `cleanupAttempts`, `generation`, `previousAttempt`, `waitedMs`, `effects`, `source`), each a short name or number. |

An event never carries source code, diffs, prompts, model output, issue text,
review text, tokens, digests, Kubernetes object names or credentials. Unknown
fields are rejected. A journal value outside the contract (for example a new
state name) is dropped from the attributes, never passed through.

## Where the events come from

The producer is a collector inside the console. Components already record
durable, timestamped state in the PostgreSQL journal; logs are not a reliable
correlated source. Every few seconds the collector reads a bounded window of
the journal in one read-only snapshot, derives the events and publishes only
ids it has not published before.

| Journal | Events |
|---|---|
| `intake_revisions` | intake `completed` for every observed revision (step `observed`, attribute `generation`); only the repository, issue, generation and time are readable, never the payload |
| `admissions` | admission `completed` (`accepted`), `failed` (`rejected`) or `blocked` (`needs-clarification`, `needs-human`, `requires-decomposition`); the first rejection after each merge is the closed issue's revision and becomes a `completed` step `issue-closed`; every later rejection (for example of a reopened issue) stays a refusal; the UI draws a refusal that follows the issue's own merge with a muted badge and an "after delivery" note |
| `work_items`, `plans` | planning `completed`; they also map work items to issues |
| `execution_requests` | `waiting` (queued), `stale-wait`, `retried` (attempt > 1), `completed` (candidate captured), `stopped-safely` (stop reason, or `worker-exit-<code>` with an observed exit code), `blocked` (`lease-lapsed`) |
| `execution_lifecycles` | `started` (claim), lifecycle `phase` on stops, `quarantined` (`cleanup-uncertain`), `degraded` (`cleanup-retried`) |
| `execution_effects` | `progressed` for each acknowledged effect, with its duration |
| `execution_failures` | the exit code of a stop (`worker-exit-22`, `worker-exit-25`) |
| `execution_candidates` | `changes` and `modelCalls` of the candidate |
| `execution_models`, `model_calls` | generator model calls: `progressed` with latency, `degraded` for a non-200 status or an `ambiguous` or `stopped` call |
| `verification_requests`, `verification_effects` | verification `waiting`, `started`, `progressed`, `completed`, `failed` (`comparison-failed`, `rejected`) or `stopped-safely` (`expired`, `revoked`, `uncertain`), `quarantined` (`cleanup-uncertain`) |
| `review_runs`, `review_effects`, `review_model_calls` | the same for review (`request-changes` is a verdict), with reviewer model call latency |
| `publication_requests`, `publication_effects`, `published_records` | publication, with `quarantined` (`unresolved-effect`) when a failed publication has an effect that was neither acknowledged nor sealed as not sent: the same condition that blocks the release manager |
| `merge_requests`, `merge_effects` | merge, with the same quarantine rule |

The journal records state, not a log, so a few timings are derived:

- A stage starts at its claim (the lifecycle row, or its first effect or
  model call); before that it is waiting.
- Verification and review record no finish time, and their effects are
  Kubernetes object creations acknowledged long before the pods finish (in
  a real journal, verification's last effect came about 55 s before its
  verdict). A passed verification or review therefore ends when the next
  stage's row for the same candidate was created (review, publication),
  which happens from the passed verdict, but never past its own claim lease
  (or expiry), so a long queue before the next stage does not stretch it;
  otherwise, and for a refusal or a
  stop, at its last observed effect or model call.
- Generator model calls record only their hand-off, so their latency is the
  interval since the previous hand-off (or the model allocation): it includes
  the executor's tool time between calls. Reviewer calls record their own
  start, so their latency is exact.
- A stale wait is a queued stage that has waited longer than
  `console.staleWaitMinutes` (default 10).

## HTTP interface

The console accepts only `GET` and `HEAD`:

| Path | Response |
|---|---|
| `/` | The embedded UI (HTML, CSS and vanilla JavaScript; no external requests). |
| `/events` | Server-Sent Events. First a replay of the most recent history (up to 5,000 events, `-replay` to change it), then a `ready` event with the cursor, then live events. Events travel in `flows` messages: a JSON array of up to 250 `{cursor, event}` elements (the snapshot's element shape), whose SSE `id` is the cursor of its last element. Browsers dispatch every SSE message as a separate task, so batching keeps a 20,000-event replay under a second; a reconnecting client sends `Last-Event-ID` (or `?since=<cursor>`) and receives only later events. A `gap` event says events after the cursor were already evicted (also on a live stream that fell behind), and the page then keeps a warning that its view is incomplete; `status` events report the source state. Each write batch has a 10-second deadline, so a client that stops reading frees its slot. A resumed stream replays its backlog in batches of at most 1,000 events. At most 32 concurrent streams; a refused page reconnects with a growing delay from its last cursor. |
| `/api/snapshot` | JSON (`ai-sdlc.flow-snapshot/v1`): one page of retained events after `?since=<cursor>` (from the oldest without it), at most `?limit=` events (default 1,000, maximum 5,000), with `next` (the cursor for the following page), `more`, the newest `cursor` and the source status. At most four snapshot requests run at once; further ones get 503. |
| `/api/contract` | The contract version, stages and kinds. |
| `/healthz`, `/readyz` | Liveness; readiness after the first successful journal read. |

Every response carries a strict Content-Security-Policy (`default-src 'none'`;
scripts, styles and connections only from the console itself), `nosniff`,
`no-referrer` and frame denial. The UI inserts event text only as text, never
as HTML.

## Security model

- **Dedicated read-only role.** The console connects as `aisdlc_flow_console`.
  At startup and in every read it checks that it is exactly that role, with no
  inherited role and no superuser, role-creation or row-security bypass; it
  refuses the bootstrap owner and every component credential.
- **Least privilege by column.** The role holds `USAGE` on the schema and
  column `SELECT` on timeline fields only (`store.flowConsoleColumns`):
  identifiers, states, bounded reasons, counts and timestamps. It cannot read
  payloads, receipts, proofs, digests, claim tokens, deltas, model
  observations, pod identities, issue text, review feedback, the outbox or the
  inbox, and it holds no `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE` or `CREATE`
  privilege.
- **Forced row security.** Journal tables force row security per role.
  Migration `0039_flow_console_reader.sql` adds one `SELECT`-only policy for
  the console role on each such table it reads; tables without row security
  need only the column grant. A policy grants nothing by itself.
- **Why columns and policies, not views.** The upgrade fence and the initial
  installation inspection accept a closed inventory of ordinary tables only
  (`bootstrapTables`, `upgradeFenceInventory`): a view would make every later
  upgrade and bootstrap fail closed. Column grants plus policies give the same
  exposure without adding a relation.
- **Optional, and withdrawn when off.** The migrator provisions the role only
  when its credential document holds a `flow-console` entry. Removing the
  entry and running the migrator again sets the role `NOLOGIN` and revokes
  every privilege.
- **Network.** The chart refuses `console.enabled` without
  `networkPolicy.enabled`, because no policy would then select the console
  pod and any pod could reach it. The console pod accepts traffic only from
  `console.accessFrom` peers on its port (none by default) and reaches only
  DNS and the database. The internal component policy excludes
  it, so it never reaches a component port.
- **Pod.** Non-root (UID and GID 65532, the distroless `nonroot` user; on OpenShift the platform assigns the UID), `RuntimeDefault` seccomp, no privilege escalation, every
  capability dropped, read-only root filesystem, no volumes and no service
  account token. The image is distroless with the UI embedded in the binary.

## Enable it

### With the installer

1. Resolve the image pins with the console included. The console image comes
   from the same signed `containers` run on `main` as the installation's image
   lock, and its signature is verified with the others:

   ```bash
   python3 tools/install_images.py --run RUN_ID --revision FULL_COMMIT_SHA \
     --include-console --output /tmp/reviewed-ai-sdlc-images
   ```

   Besides the unchanged `images.lock.json` this writes
   `console-images.json` (`ai-sdlc.console-images/v1`: the revision and the
   console digest).
2. Install with the flag; the installer reads `console-images.json` beside
   the image lock (or `--console-images PATH`) and refuses a pin from a
   different revision:

   ```bash
   ./install.sh local --images /tmp/reviewed-ai-sdlc-images/images.lock.json --flow-console ...
   ```

   It generates the console's database password with the same generator and
   custody as the component passwords, adds the `flow-console` entry to the
   migrator's `passwords.json`, creates `ai-sdlc-flow-console-database`
   (`uri`, plus `password` for its own consistency checks), and sets
   `console.enabled` and `console.digest` in the Helm values. No credential
   reaches the values, a command line or the output. At the end it prints a
   one-line `kubectl port-forward` hint (see [Reach it](#reach-it)).
3. Running a later install **without** `--flow-console` disables the
   console: the entry leaves `passwords.json`, the migrator sets the role
   `NOLOGIN` and revokes its privileges, and the installer then deletes the
   console's Secret.

The bootstrap `install.sh` forwards the same options, so a remote or
versioned bootstrap accepts `--flow-console` too. The Konflux platform does
not support it yet.

### Manually (Helm only)

1. Add a password for the console to the migrator's credential document
   (`migrator.credentialsSecret`, default `ai-sdlc-database-roles`, key
   `passwords.json`): a `flow-console` entry with 32 to 128 characters of
   `[A-Za-z0-9_-]`, distinct from every component password. Keep every other
   entry unchanged.
2. Create the console's database Secret (`console.databaseSecret`, default
   `ai-sdlc-flow-console-database`) with key `uri`, for example:

   ```bash
   password="$(openssl rand -base64 48 | tr '+/' '-_' | tr -d '=\n')"
   # Add "flow-console": "$password" to passwords.json in ai-sdlc-database-roles, then:
   kubectl -n ai-sdlc-runtime create secret generic ai-sdlc-flow-console-database \
     --from-literal=uri="postgresql://aisdlc_flow_console:${password}@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable"
   ```

   Never put these values in Git, Helm values, `--set` arguments or CI logs.
3. Enable the console in the installation's chart values and upgrade the
   release. The migrator Job applies migration 0039 (if it is missing) and
   provisions the role:

   ```yaml
   console:
     enabled: true
     digest: sha256:...      # recommended: the flow-console image digest; empty uses :flow-console-<image.tag>
     accessFrom: []          # NetworkPolicy peers allowed to reach it; empty: port-forward only
     pollSeconds: 3
     windowHours: 24
     staleWaitMinutes: 10
   ```

With `console.enabled: false` (the default) nothing renders and recorded
installations render byte-identically.

Pin `console.digest` to the digest of a `flow-console` image published by the
`containers` workflow on `main` (its identity artifact is
`console-image-flow-console`). The tag fallback exists for local development;
a tag is mutable, and the installer's image lock does not cover the console
(the installer pins it from `console-images.json`).

### Disable or roll back

To turn the console off with the installer, run the next install without
`--flow-console`. By hand: set `console.enabled: false`, remove the
`flow-console` entry from `passwords.json` and upgrade: the migrator sets the
role `NOLOGIN` and revokes its privileges. Then delete the console's database
Secret.

A migrator image older than this change does not know the observer role, so
restoring one neither disables nor removes it. Before or after such a
rollback, withdraw the role by hand as the bootstrap owner:

```sql
ALTER ROLE aisdlc_flow_console NOLOGIN;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM aisdlc_flow_console;
REVOKE USAGE ON SCHEMA public FROM aisdlc_flow_console;
```

`NOLOGIN` stops new sessions; the revokes withdraw the privileges. In
PostgreSQL, revoking a table privilege also revokes the matching column
privileges granted by the same grantor (the bootstrap owner, who ran the
migrator), so no column grant remains. Check with
`SELECT count(*) FROM information_schema.column_privileges WHERE grantee='aisdlc_flow_console'`
(expect 0). Leave the role and the
migration 0039 policies in place (migrations are not reverted): the policies
admit nothing to a role that cannot log in and holds no grants.

## Reach it

The Service is `ClusterIP` only. From a workstation with access to the cluster:

```bash
kubectl -n ai-sdlc-runtime port-forward svc/ai-sdlc-flow-console 8080:8091
# open http://127.0.0.1:8080/
```

To expose it further, add explicit `console.accessFrom` peers (for example an
authenticating proxy's namespace) and put authentication in front of it. The
console itself has no login: anyone who reaches it can read the timeline.

## Development and demo mode

The same binary replays a recorded fixture of `ai-sdlc.flow-event/v1` JSON
lines instead of reading the journal, so the UI can be developed and
screenshotted without a cluster:

```bash
cd runtime/k8s
go run ./components/flow-console/cmd \
  -fixture components/flow-console/fixtures/evaluation-batteries-29-30.jsonl \
  -fixture-live 10m -listen 127.0.0.1:8091
```

`-fixture-live` shifts the recording to end that long after start and releases
the rest as wall time reaches it, so the "now" line, open bars and live updates
behave as in production. Without it, the fixture is shown at its recorded
times.

For load testing, `-fixture-synthetic N` generates N work items over the last
day instead of reading a file (deterministic, with the batteries' outcome mix
and `source=synthetic` on terminal events), and `-replay` raises how many
events a new page receives:

```bash
go run ./components/flow-console/cmd -fixture-synthetic 500 -replay 50000 -listen 127.0.0.1:8091
# open http://127.0.0.1:8091/?perf=1 for a timing readout under the timeline
```

Measured on the reference laptop with headless Chrome at 1600×1000
(software rendering): 500 work items and 23,452 events load in about 0.1 s
after navigation, the first full render takes about 45 ms (60 ms with the
live-events panel open), and scrolling the lane list renders a frame in
about 27 ms on average. Only the lanes in view (plus a small margin) are
drawn, and during the replay the page updates a counter instead of
rendering.

The committed fixture is generated by `tools/flow_console_fixture.py` from the
[battery 29](../evidence/2026-11-25-evaluation-battery-29.json) and
[battery 30](../evidence/2026-11-25-evaluation-battery-30.json) reports. Each
attempt keeps its documented start, total time, model call count, the stage it
reached, and its stop or refusal; the timing inside an attempt is synthesised
deterministically, and stops are named the way the journal collector names
them (for example a log's `model-provider` stop appears as `uncertain` at phase
`start-ready` with a `model-status-502` degradation). Lanes with the attribute
`source=illustrative` (issues 223–228) add states the batteries did not reach:
a quarantined publication, a stale queue wait with uncertain cleanup, a
person-blocked admission and in-flight work.

## Real data

![Flow console on a restored evaluation journal](flow-console-real.png)

The screenshots `flow-console-real.png` (1600×1000, live-events panel open)
and `flow-console-real-1280.png` (1280×800) show the console in journal mode
on a local copy of a real evaluation journal: battery 34 on 2026-11-26, nine
issues of the public sample repository `os-santiago/ai-sdlc-sample`
(258–266), eight delivered and the ninth stopped in generation when the
journal was copied. The copy was taken read-only (`pg_dump`, roles without
passwords), restored into a disposable PostgreSQL, brought to this schema by
the migrator with throwaway local credentials, and read as the
`aisdlc_flow_console` role; the copy was deleted afterwards. They contain
only issue numbers, the public repository name, stage times and counts.

Checked against the battery log: every merge time is within 18 s of the
battery's measured end, and the generator model-call counts match exactly
(5 or 6 per issue). The battery log's pull request numbers are not in the
journal's readable columns, so the console does not show them. The ninth
lane appears as `generating` beyond its stage's p90 in *What is wrong now*,
because the copy froze it mid-run.

Three derivation issues were visible only on real data and are fixed:
the post-merge `issue-closed` admission had shown as a refusal on every
delivered lane, intake showed only the closing revision, and a passed
verification looked like a 6-second step (see
[Where the events come from](#where-the-events-come-from)).

## Limits

- The console shows work that started within `console.windowHours` (default
  24 h), at most 5,000 rows per journal table per read; the source badge says
  when a read was truncated. It retains at most 50,000 events in memory and
  rebuilds its history from the journal after a restart. Cursors are local to
  one console process. An open page keeps at most 60,000 events and, for the
  journal source, nothing older than the window plus two hours.
- Stops are named from the journal: the stop reason, the lifecycle phase and an
  observed exit code. The richer names in component logs (for example
  `model-provider` or `export`) are not in the journal and are not shown.
- Pauses that are installation settings (`runtime.paused`, an intake pause or
  a policy freeze) are not journal rows and are not shown as events.
- Repair, build, provenance, promotion and deployment stages are not derived
  yet; their journals can be added to the contract as new stages.
- Generator model call latency is an interval between hand-offs (see above).
- The console has no authentication of its own; rely on port-forward or an
  authenticating proxy admitted by `console.accessFrom`.
- Each poll re-reads the whole window. That is cheap at the runtime's current
  scale (tens of work items per day); a much larger installation would need
  narrower windows or an incremental cursor per table.

## Upgrade and configuration impact

- **Migration `0039_flow_console_reader.sql`** adds only `SELECT` policies that
  name the console role: no table, column, role, grant or row. Like the 0038
  constraint relaxation, it is listed in `store.RelaxationMigrations`, so the
  configuration upgrade fence admits a source catalog without it and the
  upgrade's own migrator Job applies it. Every bridge that admits a fixed
  historical catalog is unchanged.
- **Role provisioning** accepts the optional `flow-console` credential; with
  it absent nothing changes for existing installations.
- **Upgrade order**: a console image that reads new columns reports the
  journal as unavailable (the source badge, `journal-unavailable`) until the
  upgrade's migrator Job has re-provisioned the roles and granted them; it
  recovers on the next poll without a restart.
- **Images**: the `containers` workflow builds `flow-console` with the other
  components and publishes its identity as a separate `console-image-`
  artifact class, which `tools/install_images.py` never selects, so installer
  image locks are unchanged.
