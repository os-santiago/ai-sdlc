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
