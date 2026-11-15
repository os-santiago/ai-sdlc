# Real-model Homedir component benchmark

`tools/verify_generative_pilot.py` is an explicit developer conformance test. It
combines the fixed SCC executor, gVisor runtime, Kubernetes mailbox transport,
PostgreSQL model ledger and authenticated local Codex completion adapter. It uses
synthetic authority in a disposable namespace; it does not admit a live issue,
create a business execution request, publish a PR, release or deploy software.
A passing result is component evidence, not autonomous E2E or an enterprise SLA.

## Fixed task and independent acceptance

The benchmark uses Homedir commit
`0622cdaeb59a11a27bd5f3d7e18183c401a5e412`. Its original date formatter hides dates
when the page language is malformed. The requested change selects a valid page
language, then browser language, then English, while preserving existing date,
preset and invalid-options behavior. Only `utils.js` and a new locale regression
test may change. Every other original file identity must remain identical.

Before calling the model, the harness runs existing date tests and reproduces
the defect. After observing the worker container terminated successfully, it
exports the candidate through the fixed helper and checks the file boundary.
Candidate evaluation and trusted Intl reference evaluation run in separate
offline containers. Expected locales and reference code are mounted only into
the reference container. The candidate receives browser-like globals and public
inputs, with no harness `input` global. Python compares 15 bounded string results
outside both processes. An empty output or exit zero is insufficient.

The Node VM restrictions reduce accidental exposure; VM is not a security
boundary. OCI isolation, absent credentials/network, read-only mounts, dropped
capabilities and resource limits bound test execution. Generated tests and their
reported counts are auxiliary, untrusted evidence; they cannot replace the
external comparison. Passing a finite case set does not prove correctness for
all inputs or absence of malicious special-casing. Review remains necessary.

## Bounds and ownership

The reviewed fixture profile sets 24 SCC tool steps, 180 seconds and 8,192
requested tokens per call. SCC counts individual tool operations rather than
model turns and checks this step budget between batches; it is not a strict
per-operation security quota. The durable allocation separately allows eight
provider calls, 1 MiB cumulative request bytes, 512 KiB response reservations and
64 KiB per response. Provider/adapter deadlines and no-retry behavior remain in
force. Each completion's confirmed duplicate delivery is metadata-only.

A model response with `finish_reason=stop` does not end SCC: SCC may produce
validation feedback and request another completion. The fixture observes the
actual fixed Pod/container identity and termination before ending dispatch.
Missing or uncertain handoffs fail; the harness does not replay them.

The host starts a fresh authenticated loopback wrapper with an in-memory token.
Its control file is private and temporary; credentials never enter the worker.
On shutdown SIGINT allows the adapter's child process-group cleanup to run. A
forced wrapper kill makes the report fail. The overall fixture has a ten-minute
Linux signal deadline, followed by bounded cleanup. Test containers have
30-second deadlines and 1 MiB output-file caps; timeout cleanup checks a random
ownership label and removes the observed ID. The caller supplies a disposable
PostgreSQL fixture and remains responsible for removing that database.

The completion policy distinguishes the read-only model host from SCC's writable
isolated workspace. Proposed tool calls are JSON data; Codex has no action tools.
Host startup paths and host filesystem permissions are not SCC workspace facts.
The reviewed A-Dev policy and supplied repository context still govern proposals.
This clarification changes neither host permissions nor executor privileges.

## Running

Use a trusted checkout on Linux, the supported pinned Codex CLI authenticated on
the host, a task-owned kind cluster with `ai-sdlc-offline`, and a disposable
PostgreSQL database. Build the worker/mailbox images and register their immutable
platform digests as described in [Kubernetes transport](pod-helper-transport.md).
The local engine may identify an exported image by a different digest than the
kind registry manifest; pass its immutable local image ID for rendering and
validation, and the registered platform references for the Pod.

Build the fixture driver from `runtime/k8s`:

```bash
go test -c ./internal/podtransport -o /tmp/pod-transport.test
```

With `AI_SDLC_TEST_DATABASE_URL` pointing only to the disposable fixture:

```bash
python3 tools/verify_generative_pilot.py \
  --provider podman --kubectl "$kubectl_path" --kubeconfig "$fixture_kubeconfig" \
  --render-image "$mailbox_local_image_id" \
  --validation-image "$worker_local_image_id" \
  --mailbox-image "$mailbox_platform_reference" \
  --worker-image "$worker_platform_reference" \
  --test-binary /tmp/pod-transport.test \
  --source-repository "$clean_homedir_checkout" --output "$new_report_directory"
```

The output directory must not exist. A successful run retains only the two
candidate files and sanitized identity/timing/result metadata. Failure remains
`status: failed` and returns a nonzero CLI exit, including cleanup or malformed
trace failures; no business transition is inferred. The harness never edits
the source checkout or retries a failed model run automatically.

Run false-success regressions without a model, Kubernetes or PostgreSQL:

```bash
AI_SDLC_TEST_ORACLE_IMAGE="$worker_local_image_id" \
AI_SDLC_TEST_ORACLE_PROVIDER=podman \
AI_SDLC_TEST_HOMEDIR_SOURCE="$clean_homedir_checkout" \
python3 -m unittest discover -s tools -p test_generative_pilot.py -v
```

Installer CI runs these offline regressions and the existing synthetic transport
fixture. CI does not invoke a real model or orchestrate business delivery.

## Measured result and corrected failures

The [2026-11-15 report](../evidence/2026-11-15-homedir-generative-component.json)
records one successful working-tree run: six real completions, six durable
confirmed handoffs, zero uncertain calls, SCC exit zero, 16 date tests and 15
external comparison cases. The complete fixture took 75.429 seconds. Its
slowest completion took 18.740 seconds, which also exercises the corrected
broker header allowance. The two candidate files remain local artifacts;
they were not published into Homedir by the supervising session.

Earlier runs exposed real integration failures: completion `stop` was mistaken
for worker termination; the model confused host read-only permissions with SCC
proposal authority; a large completion hit the 24-second model deadline; and an
eight-tool budget was incorrectly treated as eight model turns. The final
profile and small-proposal guidance address those observed cases. Separately,
review found false-success and expected-answer leakage in the original test
oracle; the external comparison and adversarial regressions replaced it before
accepting this result. Failed runs are not counted as successful delivery.

Nine oracle/telemetry regressions and 19 wrapper tests passed locally, including
actual pinned-CLI no-action-tool and no-upstream-retry checks. The fixture is a
single fixed task, so adoption, sustained reliability, costs, security across
arbitrary repositories and comparative Fullsend performance remain unproven.

### Cleanup confirmation correction (issue #498)

A later Docker CI run rejected excessive output correctly but failed during
explicit removal while `--rm` could also remove the container. Cleanup now
validates the recorded ID and ownership label, sends at most one explicit rm,
and requires bounded read-only confirmation of exact-ID absence from a healthy
engine. Without a recorded ID, it still requires healthy absence of the unique
fixture label. A failed engine, persistent ID or changed identity fails closed.

That stronger check exposed a separate local Podman timeout problem: its default
10-second stop grace matched the CLI cleanup timeout. The old timeout regression
could mistake the cleanup's TimeoutExpired for the expected execution timeout.
Six historical owned timeout fixtures remained in Stopping state and were
removed only after verifying their exact IDs, labels, names, creation times and
fixture command. This corrects earlier cleanup assumptions; it does not change
the independently validated candidate content or model ledger evidence.

Podman cleanup now uses an explicit zero-second stop grace for these disposable
untrusted test processes. Docker retains its normal forced removal arguments.
The timeout regression requires that the exception came from `run`, then
independently queries the fixture label to verify absence. The revised 13-test
suite, including actual Podman output-cap and timeout cases, passed in 3.979
seconds with no surviving locale-check fixture containers. Docker is validated
separately through CI; local Podman evidence is not presented as Docker proof.
