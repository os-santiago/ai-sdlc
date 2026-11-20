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
  orchestrator, worker, verifier, release manager, build controller and
  deployer each run as their own Deployment, with:
  - their own Ed25519 signing identity;
  - their own database role;
  - their own Kubernetes service account.

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
| Model provider | Generation and review reach a model only through the model gateway (OpenAI-compatible, Anthropic or an agent CLI wrapper). The worker talks to its local gateway, not to the provider. A startup benchmark gates readiness. See [model gateway adapters](operations/model-gateway-adapters.md). |
| Generated code and tests | Executor and verification pods run in the `ai-sdlc-offline` RuntimeClass, a pinned gVisor handler with `network=none`. They use no service account token, no host namespaces, fixed resources and short deadlines ([fixed workload resources](security/fixed-workload-resources.md)). |
| Reviewer | A read-only workspace and read-only tools. Each answer passes a guard that limits tools, arguments and the verdict format. |
| Repository writes | Only the release manager publishes, using a scoped GitHub credential, and only paths allowed for the project. Merge requires the repository's required checks and the runtime's merge policy. |
| Build and supply chain | Builds run as Tekton PipelineRuns in a dedicated namespace under admission policies. Tekton Chains signs images. Provenance verification checks the signature and attestation against reviewed trust before promotion. |
| Cluster network | NetworkPolicies restrict each component's ingress and egress. Intake egress is limited to GitHub. The managed registry admits only the runtime, build, production and Chains controller identities. |

## Credentials

| Credential | Holder | Notes |
|---|---|---|
| GitHub credential | Intake (read) and release manager (write) | Imported with `./install.sh credentials`. Today this is a personal session token; a GitHub App with least-privilege installation scopes is planned. |
| Model provider credential | Model gateway only | Mounted from a Secret named in the reviewed upstream configuration. Executor and reviewer pods never receive it. |
| Database credentials | One per component | Created by the migrator; a component can access only its own grants. |
| Signing identities | One per component | Ed25519 keys in per-component Secrets. Public keys are bound into the installed policy. |
| Registry credentials | Build publisher (push), provenance verifier (read) | Separate identities in the managed registry. |
| Approval documents | Installation (provenance trust, release target) | Reviewed separately and time-bounded; activation refuses them once expired. |

## Data sent to a model provider

The model provider receives:
- the generation and review prompts;
- the issue title, body and acceptance criteria;
- the content of repository files the agent reads through its tools;
- tool results such as test output;
- the candidate diff that the reviewer inspects.

Treat the model provider as a processor of your source code and issue text.
Choose a provider and data-retention terms that are acceptable for the
repository. Provider credentials and runtime secrets are not part of prompts,
and executor pods cannot reach the network directly.

## Approval and authorization gates

- **Intake:** only issue events from configured, authorized actors start work.
- **Admission:** acceptance criteria must be reviewable by the independent
  reviewer, or admission proposes a revised issue.
- **Installation changes:** closed, journaled transitions with backups and
  fences. Approval documents are reviewed independently and expire.

## Known limits

- Validated on kind (Linux amd64) only. The gVisor handler is installed inside
  the kind node; OpenShift and managed clusters need equivalent sandboxing (see
  the roadmap, M5).
- Generic onboarding of new repositories is not available yet (roadmap, M1).
- GitHub access uses a personal token today.

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's private vulnerability
reporting for this repository (Security, then "Report a vulnerability"). See
[SECURITY.md](../SECURITY.md).
