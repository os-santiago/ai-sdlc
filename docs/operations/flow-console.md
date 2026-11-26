# Flow console

The flow console is a read-only web page that shows the autonomous flow as a
live timeline. It is meant for administrators who need to see how the flow
behaves: which stages ran, which stopped and why, where work waits, how long
each stage and model call takes, and what is blocked or quarantined.

It is read-only: it writes nothing, sends nothing to any component and grants
no workflow authority. It is off by default.

![Flow console with the evaluation-battery fixture](flow-console.png)

## What it shows

- **One swimlane per issue.** Each lane holds that issue's stage bars in flow
  order: intake → admission → planning → generation (execution, with its
  model calls) → verification → review → publication → merge. A retried
  attempt continues on the same lane. The newest work is at the top, and only
  lanes with activity in the visible time window are listed.
- **Bars coloured by outcome**, each with an end glyph so colour is never the
  only signal: completed (✓), running (striped, open end), waiting in queue
  (thin grey lead-in before the claim), failed with a verdict against the work
  (✕), stopped safely (■, with the diagnostic name next to the bar), blocked
  and quarantined (hatched).
- **Markers** above the bars for retries, stale waits, degradations (a failed
  or uncertain model call, a cleanup retry), blocks and quarantine.
- **Model calls** as ticks along the generation and review bars.
- **Hover** on a bar or marker for its outcome, diagnostic, wait, start, end,
  duration, model call count and latency (median and maximum), and bounded
  attributes such as the lifecycle phase or exit code.
- **Zoom and scroll**: the 1 h, 6 h, 24 h and Fit buttons, Ctrl or ⌘ with the
  mouse wheel to zoom around the pointer, and drag (or a horizontal wheel) to
  pan. *Follow now* keeps the dashed "now" line in view as events arrive.
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
- **Source state**: the badge at the top right shows whether the stream is
  live, when the journal was last read, and whether the journal is unavailable
  (the page then keeps the last known events).

The view is shareable: `?range=<hours>`, `?status=inflight|delivered|failed|blocked`,
`?issue=<text>` and `?open=<owner>/<repository>%23<issue>` set the initial
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
| `intake_observations` | intake `completed` (step `observed`, attribute `generation`) |
| `admissions` | admission `completed` (`accepted`), `failed` (`rejected`) or `blocked` (`needs-clarification`, `needs-human`, `requires-decomposition`) |
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
- Verification and review record no finish time: their terminal event is
  placed at their last observed effect or model call.
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
| `/events` | Server-Sent Events. First a replay of the most recent history (up to 5,000 events), then a `ready` event with the cursor, then live events. Each `flow` event's SSE `id` is its cursor; a reconnecting client sends `Last-Event-ID` (or `?since=<cursor>`) and receives only later events. A `gap` event says events after the cursor were already evicted; `status` events report the source state. At most 32 concurrent streams. |
| `/api/snapshot` | JSON (`ai-sdlc.flow-snapshot/v1`): the cursor, the source status and the retained events, or only those after `?since=<cursor>`. |
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
- **Network.** With `networkPolicy.enabled`, the console pod accepts traffic
  only from `console.accessFrom` peers on its port (none by default) and
  reaches only DNS and the database. The internal component policy excludes
  it, so it never reaches a component port.
- **Pod.** Non-root, `RuntimeDefault` seccomp, no privilege escalation, every
  capability dropped, read-only root filesystem, no volumes and no service
  account token. The image is distroless with the UI embedded in the binary.

## Enable it

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
   The installer does not manage this credential yet.
3. Enable the console in the installation's chart values and upgrade the
   release. The migrator Job applies migration 0039 (if it is missing) and
   provisions the role:

   ```yaml
   console:
     enabled: true
     digest: sha256:...      # the flow-console image digest; empty uses :flow-console-<image.tag>
     accessFrom: []          # NetworkPolicy peers allowed to reach it; empty: port-forward only
     pollSeconds: 3
     windowHours: 24
     staleWaitMinutes: 10
   ```

With `console.enabled: false` (the default) nothing renders and recorded
installations render byte-identically.

## Reach it

The Service is `ClusterIP` only. From a workstation with access to the cluster:

```bash
kubectl -n ai-sdlc-runtime port-forward svc/ai-sdlc-flow-console 8091:8091
# open http://127.0.0.1:8091/
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

## Limits

- The console shows work that started within `console.windowHours` (default
  24 h), at most 5,000 rows per journal table per read; the source badge says
  when a read was truncated. It retains at most 50,000 events in memory and
  rebuilds its history from the journal after a restart. Cursors are local to
  one console process.
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
- **Images**: the `containers` workflow builds `flow-console` with the other
  components and publishes its identity as a separate `console-image-`
  artifact class, which `tools/install_images.py` never selects, so installer
  image locks are unchanged.
