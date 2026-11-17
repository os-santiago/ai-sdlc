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
the entire change. A second rejection cannot create another repair.

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
