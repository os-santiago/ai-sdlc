# Candidate runtime

The opt-in `candidate` profile connects signed issue intake to isolated SCC
execution and durable, **unvalidated** candidate capture. It does not publish a
PR, merge, build a release or deploy Homedir. The default remains `maintenance`;
the existing Homedir example remains `shadow` until the candidate rollout is
validated against published images.

## Install

Use Linux amd64, the same kind/Podman prerequisites as the
[local installation](../installation.md), a working Codex authentication file,
and a reviewed image lock containing the candidate implementation. The installer
requires matching publication evidence for both worker and model-mailbox images.
A source checkout with an older image lock does not enable the new runtime.

```bash
./install.sh plan --config examples/homedir-candidate.local-install.json
./install.sh local --config examples/homedir-candidate.local-install.json \
  --github-auth gh-cli --codex-auth-file "$HOME/.codex/auth.json"
```

The installer configures gVisor, an offline executor namespace, least-privilege
controller RBAC, immutable image identities and bounded model budgets. It imports
credentials into owned Kubernetes Secrets; credentials never enter Helm values,
Git, candidate artifacts or executor Pods. Reapplying preserves the existing
identities and model Secret. To deliberately replace model authentication:

```bash
./install.sh local --config examples/homedir-candidate.local-install.json \
  --codex-auth-file "$HOME/.codex/auth.json" --refresh-model-credential
```

The Secret's public resource version changes the worker Pod template. A new Pod
receives a new model home; restarting only a container preserves CLI refreshes
within the current Pod. Credential rotation can interrupt an in-flight attempt;
that attempt is terminalized rather than replayed.

## Work and boundaries

Only eligible issues with the configured `ai-sdlc:shadow` trigger label enter the
runtime. The label name is retained for compatibility; the installation profile
controls whether accepted low-risk plans may execute. A new authority revision
causes intake to record current issue eligibility again. The orchestrator emits
an expiring signed command bound to repository, issue generation, admission,
source SHA and profile digest. The worker rechecks current observations and
policy before every effect. There is no public run/claim/advance endpoint.

The worker Pod contains a controller and a completion-only Codex gateway, using
the same reviewed worker image. Only the gateway mounts model authentication;
only the controller mounts database, component and Kubernetes credentials. The
separate gVisor executor Pod contains SCC and the model mailbox, with no network
access or service account token. The gateway cannot run workspace tools; SCC
executes its requested tools inside the offline executor.

The installation permits only `admit-issue`, `plan-admission` and `execute-work`
authority. It gives the worker narrow database columns and executor namespace
Pod/ConfigMap operations. No verifier, publisher, merge or deploy authority is
introduced by this profile. Candidate capture is never software acceptance.

## Failure recovery

Claims are nonrenewable and cannot be adopted by another process. On expiry or
a stopped controller, the reconciler closes model budgets without refund,
records uncertain provider outcomes, and fails work without a captured candidate.
It deletes only resource UIDs acknowledged in the durable lifecycle ledger.

Cleanup uses at most three bounded attempts. Pending cleanup blocks new claims,
including the cooldown between attempts. A lost create acknowledgement is
recorded as uncertain: the controller does not discover/adopt a resource by
name. Uncertain or exhausted cleanup keeps readiness false and requires operator
investigation. Pod active deadlines and namespace quotas bound abandoned
resources. Recovery does not replay a provider call or generated execution.

Returning to maintenance freezes authority and revokes execution database grants.
Finish recovery before downgrading if there are outstanding executor resources;
a maintenance installation does not have cleanup privileges.

## Validation and remaining gates

`tools/verify_candidate_runtime.py` runs `Runtime.PollOnce` with real Kubernetes,
SCC, PostgreSQL and a public pinned Homedir revision. Its issue authority and
model replies are synthetic. It checks candidate contents, no replay on a second
poll and removal of acknowledged resources. The `crash-after-model-start`
scenario exits the controller process abruptly, starts a fresh process, waits
for actual claim expiry and checks terminal budgets, ambiguous model outcome
and UID-based resource cleanup. These fixtures refuse an existing executor
namespace and require an explicitly disposable database.

The gateway also requires a separate real-model container validation. Neither
fixture nor gateway success establishes autonomous issue-to-production delivery.
Independent verification, authenticated audit consumption, PR publication,
current merge gates, Konflux release policy and test-production promotion remain
separate outerloop work. See the [architecture](../innerloop-outerloop.md).

The [recorded validation](../evidence/2026-11-15-candidate-runtime.json) separates
real infrastructure/model observations from synthetic issue/model fixtures and
lists the unverified delivery boundaries.
