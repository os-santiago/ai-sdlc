# Security overview

How ai-sdlc bounds an autonomous delivery system. This page summarizes the trust
boundaries, credentials, the data that reaches a model provider, and the gates
that must pass before consequential actions. The detailed controls are in
`docs/security/`, `docs/operations/` and the normative
[threat model](../spec/threat-model.md).

## Principles

- **The issue is untrusted input.** Issue text, repository content and model
  output are data, never authority. Admission evaluates the issue; prompts mark
  issue and candidate data as untrusted
  ([injection defense](../spec/injection-defense.md)).
- **Separate authority per component.** Intake, admission controller,
  orchestrator, worker, verifier, release manager, relay, build controller and
  deployer each run as their own Deployment, with their own Ed25519 signing
  identity and their own database role.
  - The worker (in candidate and verification mode), verifier, build
    controller, deployer and provenance verifier have dedicated Kubernetes
    service accounts.
  - Intake, admission controller, orchestrator, release manager and relay share
    the chart's default service account, which has no workload-creation
    permissions.

  A component accepts work only as a request signed by the expected producer
  ([component authority](../spec/component-authority.md),
  [signed execution requests](security/signed-execution-requests.md)).
- **Fail closed, keep evidence.** An uncertain state stops the stage. Effects
  are journaled before they are sent, cleanup is acknowledged, and no stop
  silently retries a consequential action.
- **Independent checks before consequential actions.** A candidate must pass
  behaviour verification and an independent read-only review before it can be
  published. Merge, build, provenance, promotion and deployment are each gated
  separately.

## Trust boundaries

| Boundary | Control |
|---|---|
| Model provider | Generation and review reach a model only through the model gateway (OpenAI-compatible, Anthropic or an agent CLI wrapper), a sidecar in the worker and verifier pods. The executor and reviewer pods are offline: they reach the model only through a mailbox sidecar and a shared bridge volume. The gateway alone holds the provider credential. The worker and verifier pods themselves have public HTTPS egress, so the provider is kept from them by the missing credential, not by the network. A startup benchmark gates readiness. See [model gateway adapters](operations/model-gateway-adapters.md). |
| Generated code and tests | Executor and verification pods run in the configured sandbox RuntimeClass, verified before the controllers become ready, with no unsandboxed fallback ([per-platform classes](security/offline-executor-runtime.md#sandbox-runtimeclass-per-platform)). On kind it is `ai-sdlc-offline`, a pinned gVisor handler with `network=none`. They use no service account token, no host namespaces, fixed resources and bounded deadlines (17 minutes for an executor, 60 seconds for a verification run) ([fixed workload resources](security/fixed-workload-resources.md)). |
| Reviewer | A read-only workspace and read-only tools. Each answer passes a guard that limits tools, arguments and the verdict format. |
| Repository writes | Only the release manager publishes, using a scoped GitHub credential, and only paths allowed for the project. Merge requires the repository's required checks and the runtime's merge policy. |
| Build and supply chain | Builds run as Tekton PipelineRuns in a dedicated namespace under admission policies. Tekton Chains signs images. Provenance verification checks the signature and attestation against reviewed trust before promotion. |
| AI-SDLC runtime images | The `containers` workflow signs each main image digest keyless with cosign (Fulcio and Rekor) and attests its SBOM. The image-lock resolver and the upgrade tools verify each signature against that workflow on `main` and the exact revision, and fail closed. Revisions from before the signing cutover may be unsigned ([signature verification](operations/image-lock-upgrades.md#signature-verification)). Known gaps: installation from an already-committed lock, cluster admission and SBOM attestation verification do not yet check signatures. |
| Runtime Helm chart | The `chart` workflow publishes the chart to `quay.io/os-santiago/ai-sdlc-k8s-runtime` on main pushes and signs its manifest digest keyless with cosign in a job that runs no third-party action ([verification](operations/helm-chart-install.md#verify-and-install-the-published-chart)). The chart creates no Secret and takes no secret value through Helm values ([Secret contract](operations/helm-chart-secret-contract.md)). Known gaps: publication waits for the owner to create the public repository, and nothing verifies the chart signature automatically at install. |
| Cluster network | NetworkPolicies restrict each component's ingress and egress. Intake egress is limited to public HTTPS (no cluster-internal or private ranges). The managed registry admits only network peers in the runtime, build and production namespaces and the Tekton Chains controller pods ([registry boundary](operations/registry-boundary.md)). |

## Credentials

| Credential | Holder | Notes |
|---|---|---|
| GitHub credential | Intake (read) and release manager (write) | Imported with `./install.sh credentials`. Today this is a personal session token; a GitHub App with least-privilege installation scopes is planned. |
| Source read credential (optional) | Verifier controller container only | `ai-sdlc-source-reader`, supplied by the administrator and mounted with `verifier.sourceCredential`. A read-only token scoped to the configured repository, used only for the source reads of verification and review. Never mounted into the model gateway sidecar or any verification, test or reviewer pod, and never placed in prompts, logs, proofs or reports. Without it those reads are anonymous ([source availability](operations/source-availability.md#authenticated-verification-and-review-reads)). |
| Model provider credential | Model gateway only | Mounted from a Secret named in the reviewed upstream configuration. Executor and reviewer pods never receive it. |
| Database credentials | One per component | Created by the migrator; a component can access only its own grants. |
| Signing identities | One per component | Ed25519 keys in per-component Secrets. Public keys are bound into the installed policy. |
| Registry credentials | Build publisher (push), provenance verifier (read) | Separate identities in the managed registry. |
| Approval documents | Installation (provenance trust, release target) | Reviewed separately and time-bounded; activation refuses them once expired. |

## Data sent to a model provider

The model provider receives:
- the generation and review prompts, including the reviewed A-Dev delivery
  policy text;
- the repository name, issue numbers, issue title, body and acceptance
  criteria;
- on a repair attempt, the earlier review feedback;
- the content of repository files the agent reads through its tools;
- tool results such as test output;
- the candidate diff that the reviewer inspects.

Treat the model provider as a processor of your source code and issue text.
Choose a provider and data-retention terms that are acceptable for the
repository. Provider credentials and runtime secrets are not part of prompts,
and executor pods cannot reach the network directly.

## Approval and authorization gates

- **Intake:** only the configured trigger label, applied by a configured
  authorized actor, starts work.
- **Admission:** acceptance criteria must be reviewable by the independent
  reviewer, or admission proposes a revised issue.
- **Installation changes:** closed, journaled transitions with backups and
  fences. Approval documents are reviewed independently and expire.

## Known limits

- Validated on kind (Linux amd64) only. The gVisor handler is installed inside
  the kind node; OpenShift and managed clusters need equivalent sandboxing (see
  M5 in the [roadmap](roadmap.md)).
- Generic onboarding of new repositories is not available yet (M1 in the
  [roadmap](roadmap.md)).
- GitHub access uses a personal token today.

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's private vulnerability
reporting for this repository (Security, then "Report a vulnerability"). See
[SECURITY.md](../SECURITY.md).
