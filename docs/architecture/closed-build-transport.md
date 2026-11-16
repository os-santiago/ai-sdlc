# Closed Saferus build transport

The transport verifies the exact compiled PipelineRun, TaskRun, Pod and terminal
process identities before accepting build or publication evidence. It is currently
inactive: its mutation and observation methods are private and no operational
controller or installer activates them.

## Trust and authority

`PlanBuild` authenticates the finite signed command and copies the installed
profile and slot inventory. Issue text and model output cannot supply a manifest,
image, executable argument, URL or credential. The plan binds exact request bytes.
The owning controller must persist an exclusive journal claim and create intent
before any one-shot request. A lost or ambiguous create response is quarantined;
the transport neither retries the POST nor adopts a resource by name.

The planned preflight checks the installed dependency fingerprints, immutable
`build-trust` ConfigMap, complete mutating-admission inventory, exact native
admission definitions, controller rollout readiness, actual API and registry
Service/Endpoints, and their TCP positive controls. It also checks original
cluster, namespace, PVC, PV, node and boot identities and a fresh kubelet Lease.
The local PV must name the installed bounded slot and retain its original claim.
Physical tmpfs custody is independently checked inside each workload by the
unprivileged guard; API objects alone do not prove that mount.

This is a point-in-time check under trusted administrator, API-server, kubelet,
controller and mutation-inventory assumptions. It is not continuous network
isolation or protection against an administrator changing the trust boundary.
The preflight is not yet connected to a public operational create method.

## Original execution only

The expected Pod templates pin Kubernetes 1.36.4 and Tekton 1.15 defaulting.
Only syntactic generated script basenames and credential-volume aliases are
accepted from an observation. All execution values are reconstructed from the
trusted compilation; an unknown field or changed command, image, mount, security
context, resource bound, deadline or source fails equality.

The observer lists all TaskRuns and Pods in the dedicated namespace. It rejects
extra children, changed original UIDs and other Pods mounting the same slot.
Publication may coexist only with the original stopped builder, which is checked
again. Container coverage includes init containers; restarts, replacement process
IDs, failed exits, missing prior status and terminal-state regression are denied.
The original node boot and fresh Lease are checked again at completion.

Every observation error permanently invalidates that in-memory attempt. A previous
completion cannot authorize publication after a later failure or regression.
Returned terminal evidence does not share its container slice with retained state.
Publisher results must agree between the original TaskRun and PipelineRun and
match the fixed registry, source, definition and run UID. Publication evidence
still has `releaseAuthorized: false`; independent provenance, promotion, deployment
and acceptance remain separate requirements.

## Validation scope

- TLS fixture tests cover one-shot creation, uncertain responses, replacement,
  missing or duplicate children, foreign volume users, terminal status and results.
  These use synthetic responses and are not product E2E evidence.
- `tools/verify_build_slot_api.py` creates an exclusive namespace and bound local
  PV/PVC in the owned isolated Konflux cluster, then exercises the real client.
  It creates no Pod or physical mount and deletes only original resource UIDs.
- `tools/verify_closed_build_transport.py` is the opt-in real build/publication
  integration fixture. Its authority is explicitly synthetic; it never advances
  a runtime journal or an issue. The v6 execution built and tested Saferus at
  `e4fc0085051a9c4dee088b5cc164d8b51b0fae67`, then published the artifact from
  a separate Pod after observing the original builder terminate. Cleanup passed.
  The evidence record includes preceding failed attempts and their corrections.

Neither these fixtures nor the private transport prove an autonomous
issue-to-test-production cycle, a SLSA level or production-grade vault security.

Typed Kubernetes list items may omit their type metadata; the transport fills it
only after validating the finite endpoint and the enclosing list type. Initial
TaskRun annotations and terminal Pod phase may lag controller updates. Those
observations remain pending, while conflicting versions and terminal regressions
are still rejected.
