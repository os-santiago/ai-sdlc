# Reviewed image-lock upgrades

The installer consumes immutable image digests from Git. A newer source checkout
alone does not change the installed images. Prepare the next lock from an exact,
successful `containers` publication on `main`:

```bash
python3 tools/install_images.py --run RUN_ID --revision FULL_COMMIT_SHA --output /tmp/reviewed-ai-sdlc-images
```

Use Python 3 on Linux and an authenticated `gh` CLI with read access to the
repository's Actions artifacts. Both identifiers are explicit: the tool never
selects an implicit latest build or accepts PR/build-branch artifacts. The output
directory must be new. The command reads GitHub and writes local files only.

The resolver requires all eight component identity artifacts from the same
successful push run, exact source revision, repository, workflow and platform.
It verifies each downloaded ZIP against GitHub's reported SHA-256 and size,
accepts one bounded JSON entry, rejects duplicate fields and checks all identity
fields. GitHub responses have a 1 MiB file limit, 30-second request limit and
180-second overall work limit. Missing, duplicate, expired or mismatched
artifacts fail before an installer lock is written.

`images.lock.json` contains the six installed services and migrator, in the
installer's existing schema. `evidence.json` also records the model-mailbox image,
which remains a hidden executor prerequisite rather than an installed service.
Review the generated lock and evidence in a focused PR, retaining the workflow
URL and source revision. Commit the reviewed lock to
`runtime/k8s/deploy/local/images.lock.json` and its public publication record.

## Apply and verify the desired state

Keep the installation's existing repository, environment and profile. For the
Homedir pilot, the scope remains shadow intake in its dedicated kind cluster:

```bash
./install.sh plan --config examples/homedir.local-install.json --images /tmp/reviewed-ai-sdlc-images/images.lock.json
./install.sh local --config examples/homedir.local-install.json --images /tmp/reviewed-ai-sdlc-images/images.lock.json
python3 tools/verify_shadow_intake.py --cluster ai-sdlc-homedir --provider podman --repository os-santiago/homedir --issue 1576 --expected needs-human --timeout 20
```

Before an existing database upgrade, retain a private recovery copy and verify
restoration against a disposable database. Do not commit database dumps or
Secrets. Check the migrated version list, rollout readiness and retained signed
intake evidence. New per-issue observations must be written by intake itself;
never manufacture them through SQL. Reapply the same installer inputs to verify
convergence and preserve both attempted runs' logs.

Migration scripts currently track applied filenames; they do not attest hashes
of previously executed SQL. Downgrading images does not reverse a database
migration. Recovery needs a reviewed compatible image/schema combination and
a tested backup, not an assumed automatic Helm rollback.

## Trust and evidence limits

This resolver trusts the authenticated GitHub API and the reviewed main-branch
publication workflow. Artifact integrity and matching metadata do not prove
that Quay still serves the image, validate a signature, establish trusted-builder
provenance, or demonstrate a deployment. Actual installer pulls and observed
runtime image identities are separate evidence. Konflux/Conforma verification
remains required by the enterprise promotion roadmap; this helper grants no
SLSA level and does not replace that gate.

The helper does not enable AI-SDLC execution, modify issue eligibility, dispatch
work, create releases or advance business state. Installation readiness and a
successful shadow upgrade remain separate from issue-to-production delivery.
