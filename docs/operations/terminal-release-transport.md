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

This change supplies the pure plan and bounded GitHub transport. It does **not**
yet install the terminal journal, connect the runtime inbox/controller, or mark
an orchestrator work item complete. Until those components are implemented and
validated, keep autonomous intake paused. A transport receipt without its
original durable intent and one-use send is not evidence of delivery.

Validation uses a real PostgreSQL authenticated upstream fixture for plan
compilation and local TLS servers for API response, authority and ambiguous-ACK
cases. These tests do not claim a GitHub release or an issue-to-production run.

API contracts: [Git references](https://docs.github.com/en/rest/git/refs),
[releases](https://docs.github.com/en/rest/releases/releases), and
[issues](https://docs.github.com/en/rest/issues/issues). `target_commitish` does
not replace checking an existing tag. Drafts deliberately do not produce a
published release URL. Publication uses `make_latest: false` for the test target.
