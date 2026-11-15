# Independent candidate verification

The `verification` library and Kubernetes transport define a bounded verification
boundary. They are **not yet an operational verifier**: candidate installations
still stop at durable, unvalidated candidate capture. No publisher, merge or
deployment permission follows from these APIs or their fixture reports.

## Reviewed recipe

`homedir-date-locale/v1` addresses malformed browser locale settings that prevent
Homedir from displaying dates. An issue selects it with exactly one line:

```text
AI-SDLC validation recipe: homedir-date-locale/v1
```

Selection is not execution authority. The signed worker command also binds the
repository, issue revision/generation, source SHA, work item, stored candidate ID,
delta/tree digests, recipe revision, authority revision and expiry. The candidate
ID is the original execution request ID. `MatchLedger` independently checks it
against the immutable stored candidate bytes; a signed arbitrary name is not a
substitute for that database lookup.

The recipe reconstructs the candidate from its source and canonical delta. It
allows only the existing `utils.js` implementation and one new regression test;
the existing date tests must remain intact. It compares 15 behavior cases with a
reviewed reference and requires reproducing the defect on the original source.
Generated tests are additional evidence, never their own acceptance authority.

Each role runs in a distinct offline gVisor Pod: original source, candidate,
reference and tests. Each receives only its projected ConfigMap inputs, with no
credentials, service account token, writable root, shared process namespace or
network access. The reference never executes generated code. The transport
checks exact Pod templates, UIDs, immutable image IDs, zero exits/restarts and
bounded logs. It deletes only acknowledged UIDs and never adopts a name.

The namespace must be dedicated to verification. Before creation and after
reading results, the transport requires exactly one unqualified ingress/egress
deny policy and rejects additional or paginated policies. The installer must
separately establish effective offline isolation; a NetworkPolicy or RuntimeClass
name by itself does not prove enforcement.

## Reproduce the isolated transport check

Use a disposable cluster with the installer's conformance-tested
`ai-sdlc-offline` runtime and an imported immutable worker image containing Node.
Compile the `internal/podtransport` Go test binary, then run:

```bash
python3 tools/verify_verification_recipe.py \
  --kubectl "$KUBECTL" --kubeconfig "$KUBECONFIG" \
  --test-binary /path/to/podtransport.test \
  --source-repository /path/to/homedir --source-sha "$SOURCE_SHA" \
  --candidate-directory /path/to/candidate \
  --image "$WORKER_IMAGE_DIGEST" --report /path/to/report.json
```

The fixture refuses an existing `ai-sdlc-verifiers` namespace. It creates an
owned restricted namespace, deny policy and narrowly scoped service account.
The Go transport performs actual workload creation, reads and UID cleanup.
The fixture removes its namespace, including on failure. Supplied candidate
bytes and command authority are fixture inputs; this does not prove an autonomous
issue-to-release cycle or authorize publishing that candidate.

Kubernetes log proxy requests use `Accept: */*`: real kind rejected `text/plain`
with HTTP 406 before proxying. Strict response size and recipe parsing remain in
place. Tests reject overflow, redirects, forbidden responses and arbitrary roles.

## Prerequisites for activation

The candidate reader is deliberately unavailable to provisioned verifier roles.
Migration 0012 defines read policies for all six joined tables but grants no privileges. A reviewed
activation must provide and test SELECT permissions for all joined tables, including model allocations and
budgets, with the actual
nonprivileged verifier identity. No installer profile currently enables this.

Further required work: signed durable handoff, current issue/policy proof,
exclusive bounded claims and effect ledger, recovery after process loss,
installation-owned offline verifier configuration, and authenticated audit
consumption. PR publication, current forge merge gates, Konflux policy and
same-digest test-production promotion remain separate outerloop stages.
