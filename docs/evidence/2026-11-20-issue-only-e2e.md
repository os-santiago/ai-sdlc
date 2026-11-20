# Issue-only end-to-end run, 2026-11-20

**Scope:** a local kind installation of ai-sdlc, the reference project, and one
issue (token lifetime formatting). The only external input was an authorized
label event on the issue. No delivery stage was advanced by hand.
**Model service:** a local OpenAI-compatible simulator in front of an agent
CLI, reached only through the runtime's gateway adapters
([local model service](../../tools/local_model_service/README.md)).

## Result

The first run in which the runtime itself reviewed, published and merged the
change:

| Stage | Result |
|---|---|
| Admission and planning | Passed within seconds of the label event |
| Generation (model service) | Candidate in about 2 minutes, no invalid model turns |
| Behaviour verification | Passed: exactly the three intended files |
| Independent review | Approved through the model service |
| Publication and merge | The runtime opened the pull request and merged it after green checks |
| Signed build | Ran; Tekton Chains signed the image |
| Provenance | **Refused** (fail-closed): the signature could not be uploaded |
| Second delivery | Merged; the build was never claimed (no free build slot) |

## Defects found and their resolution

Each stop was a correct fail-closed outcome that exposed a defect at a stage
the installation had not reached before:

| Stage | Cause | Resolution |
|---|---|---|
| Verification | Build by-products (Python bytecode) in the candidate | Executor environment fix (#659) |
| Review readiness | Benchmark extrapolated fixed per-call latency | Latency-aware benchmark (#662) |
| Review | The reviewer was not told the limits the review guard enforces | Guarded contract stated in the reviewer prompt (#665) |
| Upgrade fence | An abandoned review call was treated as in flight | In-flight predicate corrected (#666) |
| Provenance | Registry boundary did not admit Tekton Chains | Chart fix and a closed registry-boundary transition (#667) |
| Build capacity | One single-use build slot per installation | [Build slot provisioning](../architecture/build-slot-provisioning.md) (proposed, #668) |

The images-only upgrade path used to install these fixes is the chained
release refresh (#660, #663). The model service fixes were the local model
service (#656), restating the answer format (#661) and diagnostics (#664).

Every runtime change was merged with green CI and resolved review comments,
after an independent audit. Every installation change went through a closed,
journaled transition with independently reviewed approval documents.

## Limits of this evidence

- One issue on one reference project. Behaviour verification used a reviewed
  per-issue oracle.
- Provenance, deployment and the production record did not complete in this
  run.
- To re-run the issue, a fixture reset reverted the first merged delivery. This
  was an operator-approved reset of the demonstration repository.
