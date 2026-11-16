# Source identity at the build boundary

The outerloop must build the commit that actually reached `main`. A PR head or
the base used to validate it is not the resulting merge commit. Rebuilding either
would produce an artifact with incorrect release traceability.

`store.ReadMergedBuildSource` reads the completed immutable merge record and
returns `ai-sdlc.merged-build-source/v1`. It supplies the repository, issue, PR,
actual merge commit, verified tree, both original parents, completion time and a
SHA-256 binding of the entire record. The reader reuses the terminal journal's
signature, receipt, digest and completion-time checks. Missing, pending, corrupt
or unauthorized records cannot become build inputs.

This is a historical projection, not a build command. Expired merge authority
does not erase the source identity or become renewable through this reader. No
new database permission, Kubernetes resource, registry write or controller is
enabled by the projection.

The next operational build stage must bind this projection to fresh signed
authority, an installed recipe digest, an owned PipelineRun UID, the pinned
builder and publisher images, the permitted registry and finite budgets. It must
independently verify the resulting artifact digest and Chains signing identity
before promotion. Neither a PipelineRun result nor generated model output may
select the expected source, recipe or signing key.

The existing isolated Konflux fixture builds a pinned baseline. It does not
consume this projection and is not evidence of an autonomous merged-source
build. Wiring the build controller, provenance gate and test-production deployer
remains necessary to complete issue-to-production delivery.
