# Contracts

Contracts are the stable boundary between this runtime's independently
deployable components (admission-controller → orchestrator → worker →
release-manager). They are versioned independently from the Go packages
that implement them.

Ported from os-santiago/homedir-ai-sdlc (future-go/contracts) — see
[docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md](../../../docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md).

These are **internal** component-to-component contracts, distinct from
[`spec/normalized-event.schema.json`](../../../spec/normalized-event.schema.json),
which is the forge-neutral event this repo's intake stage produces at the
pipeline's outer boundary (GitHub webhook, etc.) for *either* control plane
(GHA or this one). `admission-controller` is the seam: it accepts a
normalized event in and emits `IssueAdmissionDecided.v1` on this internal
bus.

## Compatibility

- Producers must continue emitting a supported version during migrations.
- Consumers must ignore unknown JSON properties.
- Removing or changing a required property requires a new major contract
  version.
- Every command carries an idempotency key.
- Every event carries correlation, causation, aggregate and schema identifiers.
- Payloads contain references to secrets, never secret values.

Schemas under `events/v1` describe immutable facts. Schemas under `commands/v1`
describe requested work that can be accepted or rejected.
