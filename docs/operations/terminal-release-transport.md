# Terminal release transport

After an authenticated successful deployment, the completion command derives a
closed plan for Saferus kind test production. The tag is unique to the completion
command and points to the exact deployed source commit. Release notes contain
only the source, image digest, issue generation and authenticated record hashes.
They do not accept generated text, a mutable branch or a caller-selected URL.

Four separately journaled effects are required: create the tag, create a draft,
publish that same draft, and close the original issue. The transport has no
retry, redirect or proxy support. Each mutation consumes a caller-provided
durable send fence. An existing tag is rejected by create-reference; an equal
SHA does not authorize adoption. Before publishing or closing, the transport
checks the exact ref and release contents. It checks the ref again after each
effect. An already closed issue is rejected, not credited as successful delivery.

The pure plan, bounded GitHub transport and durable controller are implemented.
Authenticated runtime inboxes and pollers connect the deployment, terminal
release and final orchestrator recording stages. Keep intake paused until the
installed images, reviewed policies, target and complete local validation match
this capability. Source-level integration alone is not an E2E demonstration. A transport receipt without its
original durable intent and one-use send is not evidence of delivery.

Validation uses a real PostgreSQL authenticated upstream fixture for plan
compilation and local TLS servers for API response, authority and ambiguous-ACK
cases. These tests do not claim a GitHub release or an issue-to-production run.

API contracts: [Git references](https://docs.github.com/en/rest/git/refs),
[releases](https://docs.github.com/en/rest/releases/releases), and
[issues](https://docs.github.com/en/rest/issues/issues). `target_commitish` does
not replace checking an existing tag. Drafts deliberately do not produce a
published release URL. Publication uses `make_latest: false` for the test target.

## Durable controller

The terminal journal stores immutable command/configuration/plan snapshots and
four ordered intent/send/ACK facts under the release-manager database role.
`record-deployment` must be authenticated against the complete successful
historical deployment chain. A new external mutation rechecks current policy,
issue generation, eligibility, observation freshness, target and original expiry.
A dedicated PostgreSQL advisory session excludes a second controller and cancels
network work if the original backend disappears.

Recovery may continue an unsent step or proceed from its original acknowledged
predecessor. A consumed send without its ACK is quarantined. If a database COMMIT
reply is lost, the controller reads only the exact original durable ACK; it does
not infer success from a matching GitHub object. Once all four ACKs are durable,
completion can record the historical fact after issue closure or command expiry.
This does not extend authority to perform any new mutation.

GitHub and PostgreSQL do not share a transaction. A concurrent external edit can
race the last observation and mutation; ambiguous outcomes remain quarantined.
The pre-send fence uses the latest authenticated intake observation, rather than
claiming a cross-system compare-and-swap. The orchestrator independently reads the completed chain, including original
publication and merge, and records its own production fact and original work-item
completion in one authenticated inbox transaction.


## Final production fact and issue ownership

`record-production` is a distinct outerloop capability from release-manager to
orchestrator at `auto-deploy`. It carries the terminal record digest and exact
original work UUID, issue generation/revision, deployed source, image and release
URL. The orchestrator receives read-only historical journal grants; each producer
retains ownership of its writes. Its existing inner identity is reused in the
outer policy, and only that public policy is additionally mounted. No Kubernetes
API token, release target or registry credential is added to the orchestrator.

Issue ingestion can mark an older work item blocked after observing its closure.
The authenticated final fact can complete that original running/blocked UUID.
It cannot complete a different generation's UUID, adopt an unrelated completed
status, or override failed/ready work. Replays require the same immutable final
record. Concurrent planner withdrawal preserves an already completed item.

Saferus merge validation requires an empty, complete
[`closingIssuesReferences`](https://docs.github.com/en/graphql/reference/pulls)
connection, explicitly including manually linked issues. This prevents native
PR auto-closure from replacing the later acknowledged release/issue operation.
The guard is checked with review state and again before merge. Cross-system
concurrent changes remain bounded by the documented observation/mutation race.

The installed relay now requires eleven exact routes when release is enabled,
including `record-deployment` and `record-production`. Both routes remain closed
when release capability is disabled. Historical fact delivery uses current
recording policy and its original component identity, without requiring an
unexpired deployment plan, release target or an open issue.
