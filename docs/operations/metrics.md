# Runtime metrics

The runtime components expose a minimal set of Prometheus metrics: queue depth,
safe stops by diagnostic class, model gateway calls and delivery attempts. They
answer the first operational questions: is work piling up, which stops repeat,
and are model calls failing or slow. Traces, alert rules and dashboards are not
part of this set ([release plan](../releases/release-plan.md) item R4).

## Exposure

Every component that runs the shared HTTP service serves `GET /metrics` in the
Prometheus text format, version 0.0.4. The metrics come from a small in-tree
registry (`runtime/k8s/internal/platform/metrics`), built on the Go standard
library only, so no dependency was added.

- **Component port.** `/metrics` is served on the component's `http` port,
  next to `/healthz` and `/readyz`, in every runtime mode, maintenance
  included. It is read-only: `GET` (and `HEAD` outside maintenance mode,
  where the maintenance boundary refuses every method but `GET`). With
  `networkPolicy.enabled`, the chart's NetworkPolicy admits that port only
  from the relay, so nothing else can scrape it there; with it disabled, the
  components are not isolated and the port is reachable from any pod. Use it
  for debugging with `kubectl port-forward`.
- **Metrics listener.** With `metrics.enabled`, each component also listens
  on `metrics.port` (default 9090), and that listener serves **only**
  `GET /metrics` (Go's pattern matching also answers `HEAD`). A separate
  NetworkPolicy admits the `metrics.scrapeFrom` peers to that port alone. A
  scraper therefore never reaches a component's delivery routes
  (`/internal/...`), which a shared port would expose to it, even though
  those routes check signatures. The chart requires `networkPolicy.enabled`
  for this: NetworkPolicies are additive and only restrict pods that some
  policy selects, so without the internal policy the metrics port could not
  be limited to `scrapeFrom`. The restriction also depends on a network
  plugin that enforces NetworkPolicy, and on any other policy in the
  namespace that admits more.

Scraping needs no authentication change. The endpoints carry only the bounded
series below: no issue text, prompts, model output, repository names,
credentials or error text.

## Metrics

| Metric | Type | Labels | Source |
|---|---|---|---|
| `ai_sdlc_queue_depth` | gauge | `component`, `stage` | Waiting work, sampled from the store every 30 s |
| `ai_sdlc_stops_total` | counter | `component`, `loop`, `stage` | Each "stopped safely" log line |
| `ai_sdlc_model_calls_total` | counter | `role`, `outcome` | Each model gateway call made by the broker |
| `ai_sdlc_model_call_duration_seconds` | histogram | `role`, `outcome` | Duration of those calls |
| `ai_sdlc_deliveries_total` | counter | `outcome` | Each claimed publication or merge attempt |
| `ai_sdlc_publication_reconciliations_total` | counter | `effect`, `outcome` | Each read-only reconciliation of an uncertain publication effect |
| `ai_sdlc_merge_reconciliations_total` | counter | `outcome` | Each read-only reconciliation of an uncertain merge effect |
| `ai_sdlc_quarantined` | gauge | `journal` | Whether an uncertain effect quarantines the release manager's publication or merge journal, sampled each poll |

### Queue depth

`ai_sdlc_queue_depth{component,stage}` counts queued items that have not
expired. Each component reads only its own queues, with privileges its
database role already holds. Each count reads at most 10,001 rows and is
capped at 10,000. A queue that cannot be read is removed from the output
rather than left at its last value. Check each expected queue on its own:
`absent(ai_sdlc_queue_depth)` stays empty while any other queue is still
reported. For example, for the verifier with independent review:

```promql
absent(ai_sdlc_queue_depth{component="verifier",stage="verification"})
absent(ai_sdlc_queue_depth{component="verifier",stage="review"})
absent(ai_sdlc_queue_depth{component="worker",stage="execution"})
```

Add the scrape target's labels (for example `pod` or `job`) when several
installations share one Prometheus. The sampler logs
`queue depth unavailable: <component> <stage>` once when a queue starts
failing, and `queue depth restored: ...` once when it recovers.

| `stage` | Component | Counts |
|---|---|---|
| `execution` | worker (candidate and verification modes) | `execution_requests` waiting for a claim |
| `verification` | verifier | `verification_requests` waiting for a claim |
| `review` | verifier (independent review enabled) | `review_runs` waiting for a claim |
| `publication` | release manager (publication enabled) | `publication_requests` waiting for a claim |
| `outbox` | orchestrator, worker, verifier, release manager | Signed messages the component produced that the relay has not delivered yet |

### Stops

`ai_sdlc_stops_total{component,loop,stage}` goes up by one wherever a
reconciler logs a "stopped safely" line. `stage` is the same bounded value the
line names:

- worker `candidate`: `execution.DiagnosticStage`;
- verifier `review`: `reviewflow.DiagnosticStage`;
- verifier `verification`: `verificationflow.DiagnosticStage`, including
  `refused-<reason>` for a correct refusal of the candidate;
- release manager `merge`: the merge controller's fixed state, for example
  `merge-quarantined`, `merge-stopped`, `merge-authority-denied`,
  `merge-observation-unavailable`, `merge-observation-expired` or
  `merge-queue-denied`, and `unclassified` when it names none.

Model stages embed `modelbroker.SafeFailureStage`. The other loops log no
diagnostic and use `stage="unclassified"`. They are: verifier
`publication-handoff` and `repair-handoff`; provenance verifier `provenance`
and `promotion-dispatch`; release manager `publication`,
`merged-source-dispatch`, `terminal`, `production-dispatch`,
`promotion` and `planned-release-dispatch`; build controller `build`; and
deployer `deployment` and `deployment-completion-dispatch`.

A refusal is counted as a stop because the log treats it as one. To separate
controller faults from verdicts, filter `stage=~"refused.*"`.

### Model calls

`ai_sdlc_model_calls_total{role,outcome}` and
`ai_sdlc_model_call_duration_seconds{role,outcome}` record each request the
broker sends to the model gateway sidecar. `role` is `generator` (candidate
and repair generation, in the worker) or `reviewer` (independent review, in
the verifier). The histogram buckets are 1, 2.5, 5, 10, 20, 30, 60, 90, 120,
180, 300, 450 and 600 seconds.

| `outcome` | Meaning |
|---|---|
| `success` | A valid assistant completion |
| `timeout` | The call deadline or a transport timeout ended the call |
| `canceled` | The caller canceled the call before its deadline (for example on shutdown) |
| `transport` | Connection or transport failure |
| `status-4xx`, `status-5xx`, `status-other` | The gateway answered a non-200 status |
| `invalid-response` | A 200 answer the broker refused (media type, size, encoding or shape) |
| `rejected` | Refused before any request was sent |

The durable record of each call stays in `model_calls` and
`review_model_calls`. The gateway's own `model-call` log events add token
counts and the gateway-side reason; those are not metrics.

### Deliveries

`ai_sdlc_deliveries_total{outcome}` counts the release manager's claimed
attempts: `published` (a pull request was opened), `publication-stopped`,
`merged` and `merge-stopped`. Idle cycles and standing quarantine are polled
states, not attempts, and are not counted.

### Publication reconciliations

`ai_sdlc_publication_reconciliations_total{effect,outcome}` counts each
[reconciliation](publication-reconciliation.md) step of an uncertain
publication effect. `effect` is `tree`, `commit`, `branch` or `pull-request`.
`outcome` is `completed`, `not-sent`, `mismatch`, `unavailable`, `unplanned` or
`unproven`.
Together with the bounded log line, this counter is the record of attempts that
leave an effect quarantined. To alert on a quarantine that keeps failing
reconciliation:

```promql
increase(ai_sdlc_publication_reconciliations_total{outcome=~"mismatch|unplanned|unproven"}[1h]) > 0
```

### Merge reconciliations

`ai_sdlc_merge_reconciliations_total{outcome}` counts each
[reconciliation](merge-reconciliation.md) step of an uncertain merge effect.
`outcome` is `completed`, `not-sent`, `mismatch`, `unavailable`, `unplanned` or
`resolved` (another replica or an operator resolved the effect first).

Every outcome except `unavailable` and `resolved` needs an operator.
`mismatch` and `unplanned` keep the quarantine. `completed` and `not-sent` lift
it, but leave a work item and an issue to follow up: after `completed` the work
item stays `running` and the change has no build of its own; after `not-sent` the
pull request stays open and the issue needs re-delivery (see
[follow-up after a resolution](merge-reconciliation.md#follow-up-after-a-resolution)).
One alert covers all four:

```promql
increase(ai_sdlc_merge_reconciliations_total{outcome=~"completed|not-sent|mismatch|unplanned"}[1h]) > 0
```

A persistent `unavailable` shows as the quarantine gauge staying at `1` (below).

### Quarantine

`ai_sdlc_quarantined{journal}` is `1` while an uncertain effect of a failed
request quarantines the journal, and `0` when none does. `journal` is
`publication` (sampled every publication poll, 5 s) or `merge` (every merge poll,
60 s, only when merge is enabled). A journal that cannot be read, or whose loop
cannot run because the credential or the merge runtime is unavailable, is removed
from the output rather than left at its last value. A merge quarantine does not make
the release manager unready, so this gauge, not the readiness probe, is the
signal:

```promql
max_over_time(ai_sdlc_quarantined{journal="merge"}[15m]) == 1
```

## Cardinality

Every label value is bounded twice:

1. **Allowlists.** `component`, `loop`, `role`, `effect`, `outcome`, `journal`
   and the queue `stage` accept only fixed values. The stop `stage` accepts only the output
   of the diagnostic functions above, each of which returns a fixed constant
   or `unknown`.
2. **Registry.** Any value outside its allowlist, or outside
   `^[a-z0-9][a-z0-9._-]{0,79}$`, is recorded as `other`. Each metric also
   holds at most 256 label sets and folds any further set into the all-`other`
   one.

Expected series per component:

| Metric | Upper bound | Typical |
|---|---|---|
| `ai_sdlc_queue_depth` | 3 | 1 to 3 |
| `ai_sdlc_stops_total` | 256 | Fewer than 20: one per stop class actually seen |
| `ai_sdlc_model_calls_total` | 18 | Fewer than 8 |
| `ai_sdlc_model_call_duration_seconds` | 18 label sets × 16 lines | Fewer than 8 label sets |
| `ai_sdlc_deliveries_total` | 4 | 4 |
| `ai_sdlc_publication_reconciliations_total` | 20 | Fewer than 4 |
| `ai_sdlc_merge_reconciliations_total` | 6 | Fewer than 3 |
| `ai_sdlc_quarantined` | 2 | 2 (release manager only) |

Counters reset when a process starts: use `rate()` and `increase()`. A
labelled series appears only at its first increment, with no earlier zero
sample, so `increase()` does not count that first event: a single stop after
a restart shows only as a new series with value 1. To catch it, also alert on
series that were absent a window ago, for example
`ai_sdlc_stops_total unless ai_sdlc_stops_total offset 10m`.

## Enabling scraping

Metrics stay off by default. With the default values nothing renders, and
installations recorded before this setting render byte-identically. The
installer does not set these values yet; add them to the chart values of the
installation.

```yaml
metrics:
  enabled: true
  port: 9090                 # must differ from every component port
  scrapeFrom:                # required: the peers allowed to scrape
    - namespaceSelector:
        matchLabels: {kubernetes.io/metadata.name: monitoring}
  podMonitor: {enabled: false, interval: 30s}
```

Enabling metrics adds `AI_SDLC_METRICS_ADDRESS` and a `metrics` container port
to each runtime Deployment, so those pods restart once. It also adds the
`<release>-metrics` NetworkPolicy. It requires `networkPolicy.enabled`
(the chart refuses metrics without it), so every pod it selects is already
isolated for ingress, and it adds the metrics port and nothing else. The chart refuses an empty `scrapeFrom`, because a NetworkPolicy rule
with no peers admits every source. It also refuses a `port` that collides with
a component or sidecar port. It cannot judge a peer's breadth: an entry such as
`{namespaceSelector: {}}` admits every pod in every namespace, so name the
monitoring namespace and pods explicitly.

### kind

A plain Prometheus in a `monitoring` namespace can discover the pods by their
labels and the `metrics` port name:

```yaml
scrape_configs:
  - job_name: ai-sdlc
    kubernetes_sd_configs:
      - role: pod
        namespaces: {names: [ai-sdlc-runtime]}
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_name]
        regex: ai-sdlc-k8s-runtime
        action: keep
      - source_labels: [__meta_kubernetes_pod_container_port_name]
        regex: metrics
        action: keep
```

`ai-sdlc-k8s-runtime` is the pod label `app.kubernetes.io/name` (the chart
name, unless `nameOverride` is set). If
the Prometheus Operator is installed, set `metrics.podMonitor.enabled: true`
instead. To check by hand without enabling anything, use the kubeconfig the
local installer wrote (it does not change the default context; see
[installation](../installation.md)):

```bash
kubectl --kubeconfig ~/.local/state/ai-sdlc/<cluster>/kubeconfig \
  -n ai-sdlc-runtime port-forward deploy/ai-sdlc-worker 8083
curl -s localhost:8083/metrics
```

### OpenShift

Enable [monitoring for user-defined projects][uwm] (`enableUserWorkload: true`
in the `cluster-monitoring-config` ConfigMap). Then install with:

```yaml
metrics:
  enabled: true
  scrapeFrom:
    - namespaceSelector:
        matchLabels: {kubernetes.io/metadata.name: openshift-user-workload-monitoring}
  podMonitor: {enabled: true}
```

User workload monitoring scrapes the PodMonitor from the
`openshift-user-workload-monitoring` namespace, which is the peer the
NetworkPolicy admits. The metrics then appear under **Observe → Metrics** for
the runtime namespace.

[uwm]: https://docs.openshift.com/container-platform/latest/observability/monitoring/enabling-monitoring-for-user-defined-projects.html

## Not covered yet

- Lease expiries, gate denials and budget consumption (audit gap 3.2).
- Traces with correlation IDs, alert rules and dashboards (R4).
- No end-to-end scrape has been recorded on a live cluster yet; the chart
  output and the endpoints are covered by tests only.
