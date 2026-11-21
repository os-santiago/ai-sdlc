# Capability matrix

What ai-sdlc does today, how general each capability is, and what remains
unproven. "Generic" means it works for a repository without code changes.
Every candidate must pass behaviour verification, so "Generic" for planning or
generation does not mean a new issue can complete the loop yet.
Milestones refer to the [roadmap](roadmap.md). Last reviewed 2026-11-21 (alpha).

| Stage / capability | Status | Notes |
|---|---|---|
| Issue intake (authorized label event) | Generic | Authorized actor IDs are configured per installation. |
| Admission (reviewable criteria, proposed revision) | Generic | Rejects unreviewable criteria and proposes a ready revision. |
| Planning and work items | Generic | |
| Generation through a model service | Generic | OpenAI-compatible, Anthropic and agent-CLI adapters, selectable at installation (`--model-upstream`). A startup benchmark derives timeouts. The generator's workspace cannot build or run the repository's tests; the step budget (24) is still exhausted in a share of runs. |
| Behaviour verification | Generic (alpha) | `repository-tests/v1`: the repository's own test command runs on the base, cross (base plus the new tests) and candidate trees in offline gVisor pods, with the toolchain image pinned by the project manifest. A JUnit report gives per-test evidence; without one the decision is *weak* and the reviewer judges it. The two reference repositories keep their embedded oracles. |
| Independent review | Generic | Read-only reviewer bound by a guarded answer contract. |
| Bounded repair after review | Generic (opt-in) | One repair attempt from the original authority. |
| Publication (pull request) | Generic (alpha) | Allowed paths and limits from the project manifest; the authority policy binds the manifest's digest. |
| Gated merge | Generic (alpha) | Required checks, strict checks, resolved threads and zero approvals from the manifest's merge section; the installer creates the native rule. Base branch `main` only. |
| Signed build | **Project-specific** | Tekton PipelineRun with a fixed script for the reference project. Single-use build slots, and only one is provisioned by default (M3, [design](architecture/build-slot-provisioning.md)). |
| Provenance verification | Generic mechanism, project-bound policy | Tekton Chains signature and attestation in the managed registry. The policy names the reference compiler (M3). |
| Promotion, deployment, health, production record | **Project-specific** | Deploys the reference console application only (M3). |
| Installation (maintenance mode) | Generic | `./install.sh local` on kind, Linux amd64. |
| Installation (delivery) | One command for the inner loop | `./install.sh local … --project-manifest … --model-upstream …` (first run creates the merge rule and asks for its ID). The outer loop still needs separate reviewed transitions (M2, M3). |
| Upgrades | Bespoke per change | Closed, journaled transitions. The closest to generic is `release-refresh` (images only). Existing installations cannot yet apply migration 0038 (the tools fail closed) (M2). |
| Approval windows | Expire (12 to 24 h) | Renewal needs reviewed documents and a newer publication (M2). |
| Retry after a fail-closed stop | Not supported | Build and provenance are one attempt per merge (M2). |
| Operator visibility | Limited | Several components log "stopped safely" without a reason (M2). |
| Platforms | kind on Linux amd64 | Build slots use node tmpfs mounts through the container runtime, the registry is a managed local one, and installation needs host access. Remote clusters and OpenShift are not validated (M5). |

## Proven so far

- A repository that is not a reference project went from one authorized issue
  label to a merged pull request, using only a reviewed project manifest: its
  own tests verified the change offline and an independent review approved it
  ([evidence](evidence/2026-11-21-generic-inner-loop-e2e.md)).
- On a reference project, a single issue event drove the loop through a merge
  and a signed build; provenance and deployment have not yet completed in one
  run ([evidence](evidence/2026-11-20-issue-only-e2e.md)).
- Every observed stop was fail-closed, with retained evidence.

## Not yet proven

- Success rate, cost and lead time over a corpus of issues (M4; the alpha
  evaluation battery is in progress).
- Repositories with dependencies or builds beyond a small module.
- OpenShift (M5) and remote clusters.
- Repeated, continuous operation over many issues.
- A complete run through provenance, deployment and a production record.
