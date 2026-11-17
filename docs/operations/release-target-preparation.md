# Prepare the original release target

Use the same installer entry point to prepare Saferus for the opt-in outerloop.
This administrative step runs only against the existing, empty,
`configured-paused` AI-SDLC installation and the separately owned Saferus baseline.
It never admits an issue, changes the application image, rotates TLS material or
writes delivery-stage records.

```bash
./install.sh release-target prepare \
  --cluster ai-sdlc-demo --provider podman \
  --images /path/to/reviewed-publication/images.lock.json \
  --saferus-state /path/to/original-saferus-state

./install.sh release-target status \
  --cluster ai-sdlc-demo --provider podman \
  --images /path/to/reviewed-publication/images.lock.json \
  --saferus-state /path/to/original-saferus-state
```

The selected exact-revision migrator must provide `describe-release-specs`.
Select its pins through `tools/install_images.py` after normal reviewed `main`
publication. Compiler compatibility and the original application's security
specification are checked before creating any credential or patch intent.

Both commands lock the original installation and application state directories.
They verify the complete paused configuration, zero producers, empty durable
workflow state, original namespace/resource identities, registry custody and the
installed reader credential. The registry credential retains its existing
read-only scope; preparation copies it into a new immutable Docker pull Secret.
No candidate, model or generative component receives it.

The existing console Deployment receives only the pull-Secret reference and an
administrative creation nonce. A JSON Patch tests its original UID, resource
version, generation and entire spec. A durable intent precedes the only send.
If its response is lost, reapplication can accept only the same original UID,
next generation, exact intended spec and nonce; it never sends that patch again.
An absent or changed resource stops preparation. Do not remove custody records
to bypass an unresolved operation.

The command verifies that the actual ready baseline Pod belongs to the original
Deployment, then reads hash-selected content from the original owned node. It
checks descriptor sizes, every referenced config/layer hash, Linux/amd64 and the
single-manifest relationship between the requested image and runtime ImageID.
It does not import images or change the node's trust/storage configuration.
After the setup rollout, the image and original OCI content must still match.

`release-target-candidate.json` contains public target information, including the
API-defaulted spec hashes, original UIDs, public CA and baseline runtime digest.
Its approval timestamps are **zero**. It is deliberately unusable as release
authority until independently reviewed and copied into a separate approved
input with a finite window. `status` only checks an already completed
preparation and unchanged candidate; it cannot create or resolve an incomplete
intent and performs no durable writes.

Review the public target, registry reader scope, exact workload/service spec,
TLS identity, image binding and intended test-production namespace. Record the
approved target separately, then supply it to
[paused deployer installation](deployer-installation.md). Neither preparation
nor configuration enables intake. Network enforcement, application health,
recovery and the issue-driven product demonstration remain separate validations.

## Evidence and limits

Unit tests exercise original-identity drift, complete-spec conditional tests,
lost response without replay, foreign nonce rejection, strict read-only status,
reader credential scope and OCI tampering/platform rejection. The actual
`verify_console_deployment.py --installed-deployer --prepare-release-target`
fixture applies the conditional setup patch in a disposable kind, injects a lost
response, reobserves it without a second PATCH, then exercises restricted
ServiceAccount deployment/rollback and TLS. Its preparation guard is synthetic;
it does not establish the managed runtime's exclusion or empty database.

The managed `status` rejection check separately exercises the real paused
runtime guard and verifies that no target intent or original-custody change is
created. Successful managed preparation and subsequent consumer installation
must be validated on the selected published revision before claiming the target
is ready. All fixtures remain distinct from autonomous issue-to-production E2E.
