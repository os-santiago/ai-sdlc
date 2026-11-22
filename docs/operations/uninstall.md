# Uninstall owned runtime resources

`./install.sh uninstall` removes only the resources this installer created and
retains durable data and recovery metadata by default. It never deletes the
recorded kind/Konflux cluster, the `ai-sdlc-runtime` namespace, unrelated
resources, imported or generated credentials, or the Helm release history.

```bash
./install.sh uninstall --cluster ai-sdlc            # read-only plan; changes nothing
./install.sh uninstall --cluster ai-sdlc --apply    # perform the planned removal
./install.sh uninstall --cluster ai-sdlc --apply \
    --delete-data data-ai-sdlc-postgres-0           # also purge the named owned claim
```

## Ownership proof

Removal requires the recorded ownership marker for the selected `--platform`
(`ownership.json`, `konflux-ownership.json` or `openshift-ownership.json` in
`~/.local/state/ai-sdlc/<cluster>`) to match the live cluster identity, exactly
as `status` and `credentials` prove it. A missing, mismatched or foreign-platform
record, an unreachable cluster, or any resource carrying a different
installation's `ai-sdlc.io/cluster-uid` annotation refuses before any read of
the removal scope. The recorded Helm release manifest must additionally bind to
this installation through a live resource carrying the recorded cluster UID —
concretely the retained `ai-sdlc-authority` ConfigMap. State directories owned
by another platform are rejected; concurrent installer operations are excluded
by the per-cluster state lock.

## Plan and removal scope

Without `--apply` the command only emits the plan: `remove`, `purge`,
`retained` and `untouched` inventories in JSON. Removal covers resources
recorded by the `ai-sdlc` Helm release manifest (Deployments, StatefulSets,
Jobs, ConfigMaps, Services, ServiceAccounts, Roles/RoleBindings,
ClusterRoles/ClusterRoleBindings, NetworkPolicies, ResourceQuotas, Routes and
chart-created auxiliary namespaces) plus installer-created resources carrying
the recorded cluster-UID annotation, such as the offline `RuntimeClass`.
Record kinds the uninstaller does not handle, for example operator-managed
database clusters, are reported as retained rather than deleted. Each planned
object is re-read immediately before deletion and removed only while its UID
still matches the plan.

`--apply` additionally requires that new work intake is already stopped through
the maintenance control plane: a frozen `ai-sdlc-authority` policy with no
active runtime mode, the same gate `local --profile maintenance` installs. An
active installation must first be reconciled to maintenance. Build-enabled
installations are refused; their teardown requires a containment-preserving
procedure that is not implemented.

## Retained state and explicit data purge

Secrets (component, database, GitHub, model and publication credentials), the
`ai-sdlc-authority` policy ConfigMap, Helm history Secrets, PersistentVolumeClaims
and the runtime namespace are always retained. They let a later `local`
installation adopt the previous state after its usual compatibility checks.

Claims created by an owned StatefulSet carry no ownership labels; the
uninstaller derives their names from the recorded StatefulSet's
`volumeClaimTemplates`. Only those derived claims are eligible for
`--delete-data <pvc>`, which must name each claim explicitly — it cannot be
implied by a missing flag, a failed lookup or a wildcard. A name outside the
owned set refuses before any deletion.

Re-running the command recomputes the plan from live state, so it is resumable
and idempotent: resources already absent are skipped, and a second `--apply`
deletes nothing. A completed removal writes `uninstall.json` in the cluster
state directory recording the deleted resources, any purged claims and the
retained scope. Do not delete ownership, capability or uninstall records to
bypass these checks; retain evidence and investigate instead.
