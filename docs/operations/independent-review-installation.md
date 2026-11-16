# Install independent review

Add `"independentReview": true` to a reviewed local installation JSON whose
`profile` is `verification`. The equivalent explicit CLI option is
`--profile verification --independent-review`. Existing installations default to
review disabled. `./install.sh plan --config <path>` previews the scope without
reading credentials. `./install.sh local --config <path>` applies it through Helm.

The installer needs matching published worker/mailbox/verifier/migrator images
that include independent review. Old image locks do not acquire this capability
by changing a flag. Import the model account explicitly with `--codex-auth-file`
on the first installation, as for candidate mode. Credentials stay in owned
Kubernetes Secrets, never the installation JSON or Helm values.

The verifier first reconciles already behavior-verified candidates through an
independent read-only SCC session. Review has its own namespace, model client
Secret, sidecar process, filesystem and durable call journal. The model account
is mounted only into that sidecar; the verifier receives only the loopback client
token. Generated code receives neither. The review namespace has denied ingress
and egress, bounded quotas and restricted Pod security. Only the verifier service
account can create/read/delete review Pods and ConfigMaps or invoke their fixed
transport helpers. It cannot mutate generation resources or read Secrets via the
API. No publication or deployment route is added.

Review uses a distinct eight-step, 180-second profile and up to eight calls. Its
approval proof requires current signed authority, unchanged candidate source,
matching model evidence and confirmed cleanup. An unknown create or ambiguous
model attempt blocks further work; the installer does not adopt or replay it.

Removing the option (or setting it false) removes review model mounts/RBAC and
atomically revokes review table and column privileges during the migration Job.
The journal remains as evidence. The reviewer namespace, deny-all policy and
resource quota carry Helm keep annotations so disabling cannot delete uncertain
resources or remove their isolation. Re-enabling uses the same retained boundary.
Do not disable during an active review: first
observe that known resources are retired and no uncertain cleanup remains.
Disabling does not rewrite claims or discard uncertain resources.

Current evidence is in `docs/evidence/2026-11-15-review-installation.json`.
The E2E demonstration additionally requires publication, merge gates, artifact
provenance and promotion to the separate kind production namespace.

## Reproduce installation behavior

Create the dedicated disposable `ai-sdlc-review-install-test` kind cluster using
the ordinary maintenance installer. Build and import the worker, model-mailbox,
verifier and migrator OCI images locally. Supply their immutable references as
a JSON object keyed by those four component names to
`python3 tools/verify_review_install.py --help`. The tool requires explicit
credential import options when its owned test Secrets do not exist. It freezes
authority and uses a fixture-only trigger label, then enables, reapplies, disables
and re-enables review through Helm. It checks zero workflow requests, actual
PostgreSQL grants, removed model mounts/RBAC, and retained namespace, marker UID,
deny-all policy and quota. It removes its marker with an exact UID precondition.
Locally imported fixture images are not publication evidence. Retire the owned
disposable cluster after inspecting the sanitized report.
