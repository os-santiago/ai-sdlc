# Frozen runtime on the owned Konflux platform

The single `install.sh` entrypoint can add AI-SDLC's six runtime services and
PostgreSQL to an existing Konflux kind cluster. This capability currently accepts
only Saferus, `ai-sdlc-production`, and maintenance mode. Issue ingestion,
generation, publication, merge and delivery remain disabled. It does not import
GitHub or model credentials and does not claim an autonomous E2E.

```bash
./install.sh supply-chain local --cluster ai-sdlc-konflux-reviewed --provider podman
./install.sh plan --config examples/saferus.konflux-frozen.json
./install.sh local --config examples/saferus.konflux-frozen.json
./install.sh status --config examples/saferus.konflux-frozen.json
```

The supply-chain step creates the platform only when it is absent and unrecorded.
The runtime step requires that original platform to exist. It never recreates
it, manufactures a second ownership record, or adopts another cluster. The
reviewed Kubernetes version and native admission API must match the supply-chain
lock. To use newer reviewed component images, copy the example to your local
configuration directory and set `images` to the immutable lock prepared by the
[image publication resolver](image-lock-upgrades.md). Configuration and CLI scope
must agree; a conflicting override fails before any API access.

Reapply the runtime command to reconcile the same Helm release. A per-cluster
installer lock prevents concurrent runtime installers. The original namespace
UID, restricted Pod Security labels, cluster UID and creation nonce are checked
on each invocation. The initial create intent is durably written before POST.
After a lost response, reconciliation may bind only the existing namespace with
that exact intent. An absent namespace with a previous intent is ambiguous and
is not recreated. Do not delete ownership/capability records to bypass this
check; retain evidence and investigate the original cluster.

Existing runtime configuration must already be maintenance and its authority
policy frozen. This capability does not convert an active installation into a
safe maintenance window. A future active integration must first implement
quiescence, original-UID workload containment, signed outerloop dispatch,
provenance policy, promotion and acceptance. Do not activate individual resources
manually to bridge those missing components.

## Validation evidence

On 2026-11-16 the initial install and reapply succeeded on the owned
`ai-sdlc-konflux-reviewed` Kubernetes 1.36.4 cluster, using published main revision
`431fbccbcab7126c630a016cc52125fd3dc84ef7` (container workflow run `43818019592`).
All six runtime Deployments and PostgreSQL became ready. Both runs passed actual
ingress deny, egress deny, selective egress and positive network controls, with
probe resources removed. Authority stayed frozen and issue intake disabled.
The namespace remained `d7dc570d-6814-432a-a469-060905ab1852` within original cluster
`73198ec7-6964-45d9-bf51-6512e96c2452`.

Local evidence: `/tmp/ai-sdlc-frozen-konflux-install-v1.log`,
`/tmp/ai-sdlc-frozen-konflux-reapply-v1.log`, and the owned state's
`installation.json` / `runtime-capability.json`. These are installation proofs,
not proof of issue-to-production delivery or production-grade isolation.
