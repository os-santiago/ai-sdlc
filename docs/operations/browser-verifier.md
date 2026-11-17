# Browser verifier component

The browser verifier runs a fixed Saferus console check in a non-root container.
It exercises desktop and mobile layouts, normal certificate validation and the
console's demo API. A health endpoint alone cannot establish these UI behaviors.

This increment supplies the component, its separate gVisor runtime and isolated
conformance tests. **It does not yet authorize or complete deployment health.**
The deployer-owned Job, durable journal and installed promotion/rollback gates
remain tracked in [issue #629](https://github.com/os-santiago/ai-sdlc/issues/629).
Neither a manually invoked browser nor a synthetic fixture is autonomous E2E
evidence. The migration catalog remains at 34.

## Closed execution contract

The image accepts one bounded JSON request at `/input/request.json` and one public
CA at `/input/ca.crt`. The request binds its ID, target and snapshot hashes, runtime
image digest, expectation and original deadline. It contains no URL, script,
credential, arbitrary command or extra field. The two supported expectations are
the original `60 seconds` display and the proposed `1 minute` display.

Firefox may request only fixed assets under `https://saferus-console:8443` and two
demo requests with an empty JSON object. Additional origins, paths, query parameters, methods,
WebSockets, unexpected API fields and incorrect TLS fail the check. The result
contains hashes and bounded observations; it does not contain API response bodies,
screenshots, cookies or credentials. The process has a 25-second hard limit within
the request's maximum 30-second window.

The workload requires `ai-sdlc-browser`, read-only root filesystem, UID 1001, no
service-account token, no additional Linux capabilities and `RuntimeDefault`
seccomp. Its only writable space is a bounded memory volume. It receives the
approved console's public CA, not application or workflow credentials.

## Separate browser runtime

The declarative files under `runtime/k8s/deploy/local/browser-runtime/` use the
same individually hash-verified gVisor binaries as the offline executors. The
browser handler uses gVisor's `sandbox` network stack and explicit Kubernetes
NetworkPolicies. The existing `ai-sdlc-offline` handler keeps `network=none`;
default `runc`, annotation overrides and unrelated settings remain unchanged.

`tools/browser_runtime.py` is an administrative bootstrap helper for an already
owned single-node kind installation. It rejects foreign ownership, modified
binaries, imported containerd overrides and conflicting handlers. It records
before/after configuration hashes and verifies reapplication. Installing this
extension into an existing delivery environment still requires its explicit
installer transition and custody checks; the normal installer does not silently
adopt it.

The component uses the browser versions in its pinned base image and hash-locked
Python dependencies. See the upstream [Playwright image contract](https://playwright.dev/python/docs/docker/)
and [gVisor networking model](https://gvisor.dev/docs/user_guide/networking/).

## Validation and publication

Run the input, runtime configuration and publication tests:

```bash
python3 -m unittest discover -s tools -p 'test_browser*.py' -v
python3 -m unittest discover -s tools -p test_install_images.py -v
```

The `browser-component` quality workflow builds the exact source image and runs
`tools/verify_browser_component.py` in an owned disposable kind installation.
Its synthetic TLS server checks baseline and readable duration, wrong UI text,
API failure, extra API data, forbidden requests and an untrusted CA. Actual socket
probes verify ingress and egress denial, then restored connectivity, against the
same live endpoint. Cleanup uses the original namespace UID. These responses are
synthetic and do not validate vault cryptography.

The existing `containers` workflow publishes this thirteenth image to Quay with
the other components. Its identity artifact is `browser-image-browser-verifier`.
Publication resolution requires explicit `--include-browser --include-release
--include-build`; consumers requesting the original twelve images continue to
select exactly those twelve. Publication identity alone grants no deployment
authority.

## Deployment health contract

The deployment controller now requires both its HTTP/TLS smoke and the fixed
browser recipe before accepting deployment health. The two checks, original-Pod
deletion and evidence recording share the existing 30-second smoke window.
Baseline verification precedes the one-use deployment send; advance and rollback
verification bind to their own acknowledged deployment receipt. PostgreSQL
migration `0035_deployment_browser.sql` stores immutable intent, send, original
Pod UID, evidence and a separate cleanup fact in five append-only tables.
Release and production readers authenticate the evidence chain.
HTTP-only or caller-constructed browser observations cannot complete a deployment.

The installed public profile binds the browser image and runtime digest, original
node name/UID, gVisor RuntimeClass/configuration, namespace, public CA ConfigMap,
and network-policy UIDs. A Pod is bound to that prepared node; a replaced or
cordoned node is rejected. The workload has no GitHub/model/vault credential,
service-account token, external URL or script input. A lost create acknowledgement
cannot be recovered by adopting a Pod later found under the same name.

Cleanup may outlive the smoke sub-context only for containment of the acknowledged
Pod. It inherits the original controller/claim context and ends at the earlier of
ten seconds or the original claim expiry. Cancellation of that authority prevents
new cleanup requests under that attempt. A later controller poll may obtain a
new, ten-second resource-containment authorization for an expired browser request
whose original Pod UID was acknowledged. That path exposes only cleanup: it
rechecks the original cluster/namespace and Pod UID, records absence, and cannot
create a Pod, patch the application, retry verification or emit health. An unknown
create outcome remains quarantined. A lost DELETE reply is resolved only by
original-scope absence, without replaying DELETE. Cleanup after the smoke deadline
never produces success.

Prepared installations provide `--browser-profile <public-profile.json>` together
with the approved release target and image selection. The offline migrator command
`compile-browser-installation` validates public scope observations with the same
validator used by the running adapter. The installer does not turn an arbitrary
profile into approval, adopt existing resources, or infer a new target from it.
Infrastructure manifests are in `runtime/k8s/deploy/local/browser-platform/`;
Helm grants the deployer Pod/log access only in that dedicated namespace and
read-only access to the bound node, class, CA and policies.

The existing schema-34 installation requires a separately validated migration and
activation transition before consuming this capability. Do not apply schema-35
images through the reviewer-only image upgrade, or reinterpret old HTTP receipts
as browser evidence.

The `verify_browser_transport.py` fixture checks the actual Kubernetes API,
Firefox/gVisor, normal TLS, original-Pod cleanup and unchanged console Deployment.
It uses the rendered deployer ServiceAccount and checks 19 allowed/denied API
permissions, including denial of exec, secrets and policy writes. Its fourth
scenario cancels the original attempt after the Pod ACK, expires the smoke and
checks resource-only recovery without late health. Its business API is synthetic
and it does not write deployment journal evidence;
it is not autonomous E2E or vault-cryptography proof.

## Existing installation transition

For the closed schema-34 installation with a completed reviewer-image upgrade,
`./install.sh runtime-upgrade browser` selects a successful immutable main
publication containing all thirteen image identities. Supply `--cluster`,
`--provider`, `--target-run`, `--target-revision` and `--provenance-trust`. The public
trust document must be independently reviewed and change only `verifierImage` to
the selected publication. Existing keys, CA, publisher, scope and expiry remain
identical. Publication or compilation alone does not approve that document.

The operation has its own journal and finite deadline. It validates the original
chart from immutable Git objects, excludes workflow writes, pauses original
producers, captures and restores a backup, prepares the browser infrastructure,
applies migration 35 and activates the new consumers with intake last. The
separate browser fence admits only schemas 34/35, fully contained failed-review
history and empty browser tables. Both legacy fences retain their earlier schema
and history boundaries.

The transition preserves every old row, role identity and privilege. It adds
browser evidence capabilities and updates all database consumers; isolated
publisher/worker/mailbox job pins and budgets remain unchanged. It never deletes
migration history, rolls schema 35 back to 34, renews an expired operation, or
repeats an uncertain migration/Helm send. Unknown outcomes retain their original
records and require observation and containment. `browser-status` reports only a
complete recorded phase; it is not proof that the software delivery E2E passed.
