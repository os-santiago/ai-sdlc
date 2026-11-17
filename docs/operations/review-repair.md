# One bounded review repair

An independent `request_changes` result can derive one repair only when the
original generation profile included `maxRepairs: 1`. New installations opt in
with `reviewRepair: true` in `LocalInstallation`, or `--review-repair` alongside
`--independent-review`. The default is disabled. Changing configuration later
does not authorize repair of an old candidate. Schema 37 and matching component
images are required; an existing schema-36 installation needs a separately
reviewed upgrade procedure.

The verifier preserves the exact authenticated feedback privately, then sends
only signed references to the orchestrator. The orchestrator repeats the original
generation, behavior, review, current issue and policy checks. In one transaction
it derives a single child allowance and appends the signed worker request.
Calls and bytes come from the minimum remaining capacity of the original budget
and allocation. No refund or deadline extension is possible; the original budget,
candidate and review remain historical facts.

The worker starts from the rejected candidate and treats the feedback as
untrusted data. Issue acceptance criteria, reviewed policy and model settings
remain unchanged. It exports a cumulative delta against the original repository
tree, so deterministic verification and a fresh independent review can evaluate
the entire change. Exporting the rejected tree unchanged is refused before another
review. A second rejection cannot create another repair.

Ineligible, expired or exhausted repair decisions produce a separate signed
rejection fact. Only the orchestrator can close the matching running work as
failed, after checking retired resources, current issue ownership, absence of
downstream publication and the exact child lineage. Existing blocked/completed
states remain intact. The durable rejection fact does not renew any execution;
its transport still requires fresh authentication. An expired reservation that
never reached the worker is eligible for the same bounded closure.

Duplicate delivery and lost commit acknowledgements use the existing inbox and
outbox. They cannot create a second child or replenish capacity. Missing evidence,
uncertain cleanup and unavailable database observations stop progress; they are
not fabricated exhaustion or permission to retry an external effect.

Validation must distinguish actual PostgreSQL transactions and signed component
delivery from synthetic model/Kubernetes effects in journal fixtures. An accepted
repair reservation or unit test does not establish an issue-to-production E2E.

## Validate the repair boundary

`python3 tools/validate_runtime.py --provider podman` checks the signed handoffs,
remaining capacity, cumulative capture, second rejection, delayed terminal facts,
lost commit replies and concurrent planner withdrawal against disposable
PostgreSQL. Expired queued attempts retire even when the model gateway is down.

For a disposable kind cluster with the offline gVisor runtime and immutable
worker/mailbox images already imported, compile the execution test binary and
run:

```bash
python3 tools/verify_review_repair.py \
  --kubectl /path/to/kubectl --kubeconfig /private/disposable-kubeconfig \
  --worker-image REGISTRY/IMAGE@sha256:DIGEST \
  --mailbox-image REGISTRY/IMAGE@sha256:DIGEST \
  --test-binary /path/to/execution.test --report /private/repair-report.json
```

`AI_SDLC_TEST_DATABASE_URL` must name a disposable loopback `ai_sdlc_test`
database; the fixture resets it. The `ai-sdlc-executors` namespace must not exist.
This fixture uses synthetic predecessor history, source and model responses, then
executes the child with actual SCC, Kubernetes and PostgreSQL. It checks imported
parent content, private feedback, remaining calls, cumulative capture, unchanged
parent budget and resource removal. It is not a demonstration of a fresh live
issue reaching production.

The broader composition fixture runs the predecessor's behavior verification and
SCC `request_changes`, derives the child through signed inboxes, runs actual SCC
repair, verifies the cumulative candidate, obtains a new independent SCC approval
and emits exactly one normal publication handoff. It retains synthetic initial
generation history and deterministic model replies; it makes no forge writes.
The `review-kind` quality check runs this fixture and retains its report.

```bash
python3 tools/verify_review_repair_composition.py \
  --kubectl /path/to/kubectl --kubeconfig /private/disposable-kubeconfig \
  --worker-image REGISTRY/IMAGE@sha256:DIGEST \
  --mailbox-image REGISTRY/IMAGE@sha256:DIGEST \
  --verifier-image REGISTRY/IMAGE@sha256:DIGEST \
  --test-binary /path/to/execution.test --repository /path/to/homedir \
  --source-sha 0622cdaeb59a11a27bd5f3d7e18183c401a5e412 \
  --candidate-directory /private/prepared-locale-candidate \
  --report /private/repair-composition-report.json
```

The disposable database requirement is the same. All three namespaces
`ai-sdlc-executors`, `ai-sdlc-verifiers` and `ai-sdlc-reviewers` must be absent.
Prepare candidate files with `tools/prepare_verification_fixture.py` as the CI
workflow does. The report binds the binary hash, source commit, runtime revision,
image digests and whether the runtime checkout was dirty. Prefer a clean frozen
checkout when collecting release evidence.
