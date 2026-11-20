# Capability matrix

What ai-sdlc does today, how general each capability is, and what remains
unproven. "Generic" means it works for a repository without code changes.
Every candidate must pass behaviour verification, so "Generic" for planning or
generation does not mean a new issue can complete the loop yet.
Milestones refer to the [roadmap](roadmap.md). Last reviewed 2026-11-20.

| Stage / capability | Status | Notes |
|---|---|---|
| Issue intake (authorized label event) | Generic | Authorized actor IDs are configured per installation. |
| Admission (reviewable criteria, proposed revision) | Generic | Rejects unreviewable criteria and proposes a ready revision. |
| Planning and work items | Generic | |
| Generation through a model service | Generic | OpenAI-compatible, Anthropic and agent-CLI adapters. A startup benchmark derives timeouts. The executor step budget (24) is exhausted in a share of runs. |
| Behaviour verification | **Single predefined issue per repository** | Two embedded oracles (reference implementation plus cases), each selected by an exact marker in the issue body and mapped to one reference repository. A new issue, even on a reference repository, cannot pass verification. The repository's own test suite is not used yet (M1). |
| Independent review | Generic | Read-only reviewer bound by a guarded answer contract. |
| Bounded repair after review | Generic (opt-in) | One repair attempt from the original authority. |
| Publication (pull request) | **Project-specific** | Two supported repositories with fixed file allowlists (M1). |
| Gated merge | **Project-specific** | Merge policy compiled per repository (M1). |
| Signed build | **Project-specific** | Tekton PipelineRun with a fixed script for the reference project. Single-use build slots, and only one is provisioned by default (M3, [design](architecture/build-slot-provisioning.md)). |
| Provenance verification | Generic mechanism, project-bound policy | Tekton Chains signature and attestation in the managed registry. The policy names the reference compiler (M3). |
| Promotion, deployment, health, production record | **Project-specific** | Deploys the reference console application only (M3). |
| Installation (maintenance mode) | Generic | `./install.sh local` on kind, Linux amd64. |
| Installation (delivery) | Manual, multi-step | About 15 commands with separate reviewed approvals (M2). |
| Upgrades | Bespoke per change | Closed, journaled transitions. The closest to generic is `release-refresh` (images only) (M2). |
| Approval windows | Expire (12 to 24 h) | Renewal needs reviewed documents and a newer publication (M2). |
| Retry after a fail-closed stop | Not supported | Build and provenance are one attempt per merge (M2). |
| Operator visibility | Limited | Several components log "stopped safely" without a reason (M2). |
| Platforms | kind on Linux amd64 | Build slots use node tmpfs mounts through the container runtime, the registry is a managed local one, and installation needs host access. Remote clusters and OpenShift are not validated (M5). |

## Proven so far

- A single authorized issue event drove admission, planning, generation,
  behaviour verification, independent review, publication and a merge with no
  human step. The signed build ran. Provenance and deployment have not yet
  completed in one run ([evidence](evidence/2026-11-20-issue-only-e2e.md)).
- Every observed stop was fail-closed, with retained evidence.

## Not yet proven

- Onboarding a repository other than the two references.
- Repeated, continuous operation over many issues.
- A complete run through provenance, deployment and a production record.
- Success rate, cost and lead time over a corpus of issues.
