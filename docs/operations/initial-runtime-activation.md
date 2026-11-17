# Activate a prepared initial runtime

`install.sh initial-runtime activate` starts the already configured Saferus
installation in three finite phases. It accepts no issue number, workflow stage,
artifact, release request or database update. After activation, GitHub issues
remain the only external work input.

Prerequisites are the complete empty, configured-paused installation with the
release capability, independently approved target and provenance trust, original
registry/build custody and current schema/component roles. Complete
[release target preparation](release-target-preparation.md) and
[paused deployer installation](deployer-installation.md) first.

```bash
./install.sh initial-runtime activate --cluster ai-sdlc-demo --provider podman
./install.sh initial-runtime status --cluster ai-sdlc-demo --provider podman
```

The installer retains `bootstrap-preparation.json` and
`prepared-configuration.json`. A separate protected `initial-activation.json`
binds their current configuration, the reviewed chart, all three rendered targets
and the original approval window. It locks the installation throughout the
transition. The deadline is the earliest of 30 minutes, provenance approval
expiry and target approval expiry; reapplication never renews it.

| Phase | Inner/outer policy | Downstream replicas | Intake | Acceptance |
| --- | --- | --- | --- | --- |
| Prepared | Frozen | 0 | 0 | Original empty configuration |
| Warming | Frozen | 1 each | 0 | Exact original controllers and running containers |
| Armed | Active | 1 each | 0 | Canonical policies match the recompiled build snapshot; all downstream containers ready |
| Active | Active | 1 each | 1 | Only intake replica changes |

Frozen provenance/release validators deliberately reject operational readiness.
Warming does not bypass those validators or claim Ready. Arming must pass their
ordinary readiness checks before intake starts. Every pre-intake transition
checks the empty database, original physical/database/credential identities,
original build profile, registry reader custody and approved release target.
The inspector remains non-root and uses the existing original-Pod API tunnel.

`runtime.intakePaused` controls only intake; `runtime.paused` continues to exclude
all delivery producers. `migrator.enabled` defaults to true for installation.
Activation sets it false only after the original schema/roles pass inspection,
so its Helm revisions cannot reprovision roles concurrently with the first issue.
No direct Deployment scaling, SQL write or component workflow endpoint is used.

Each Helm transition records intent before applying its exact reviewed chart and
values. It accepts only the complete before or after render with original UIDs.
An interrupted operation whose target is complete is observed without replay;
a partial inventory stops before the next effect. A changed chart or expired
window rejects further writes. Preserve records/resources when blocked; deleting
custody files is not a recovery procedure.

After intake starts, legitimate workflow history is expected. Status and recovery
of a completed final transition do not demand an empty database, repeat admission
or modify existing records. Status requires a complete recorded phase and is
read-only. Successful activation proves installation readiness and opens ingress;
it does not prove that any issue completed the autonomous E2E or qualify the
system for enterprise production.

## Validation

Unit and Helm tests cover phase-specific replica/freeze boundaries, suppression
of concurrent migrations, the final intake-only diff, preserved defaults,
changed charts, original approval expiry, lost final responses, partial inventory
and read-only status. The actual managed installation rejects activation before
its release capability is installed, without creating an activation intent.
Positive managed startup, network enforcement and an issue-driven E2E must be
validated separately on the published revision.

## Installation readiness report

The prepared installer records `namespace: ai-sdlc-runtime` and
`installationReady: false`. After the complete `active` phase is freshly observed,
activation writes an `installation.json` snapshot containing the original cluster
and namespace UIDs, exact image pins, custody digests and observation timestamp.
Only that complete active observation sets `installationReady: true`, allowing
Saferus's existing-environment verifier to recognize the activated installation.

This report is a compatibility snapshot, not delivery authority or proof of an
issue reaching production. `prepared-configuration.json`, `initial-activation.json`
and the original runtime capability retain their custody semantics. If the final
report write fails or loses its acknowledgement, repeating `activate` can repair
only that snapshot after observing the existing active state; it neither replays
Helm nor renews the activation deadline. `status` never writes or repairs reports.
A paused, partially active, replaced or unready installation cannot publish a
successful readiness snapshot.
