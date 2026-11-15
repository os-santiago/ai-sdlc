# Independent candidate verification

The explicit `verification` profile runs a separate service that consumes signed
worker candidates and records independent, bounded behavior evidence. Its
activation requires matching reviewed published images. The `candidate` profile
still stops at durable, unvalidated capture. Neither a passing recipe nor a
fixture report authorizes PR publication, merge or deployment.

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

## Activation boundary and remaining outerloop

Migrations 0012 and 0013 define candidate read policies and the verification
ledger. They grant no operational privileges by themselves. The reviewed
`verification` profile provisions the independent role's minimum reads and
ledger writes; returning to `candidate`, `shadow` or maintenance revokes them.
The actual nonprivileged identity is exercised in the composed fixture below.

Signed handoff, current issue/policy proof, bounded claims, durable effects and
UID-based recovery are implemented. Independent generative audit, PR publication,
current forge merge gates, Konflux policy and same-digest production-test
promotion remain separate outerloop stages. An installation or behavior proof
does not establish completion of those stages.

## Signed handoff and bounded reconciliation

The worker can derive a `verify-artifact` command only from a captured candidate
whose executor cleanup is complete, whose original signed execution request is
unexpired, and whose current issue evidence still selects the reviewed recipe.
The command retains the original expiry. Its disposition and signed outbox entry
are committed together; an unsupported or revoked candidate is terminally rejected.

The independent verifier inbox records only a matching signed worker handoff.
Before each effect, the controller rechecks the original request, candidate delta,
source, generation, issue criteria, authority, intake configuration and generation
profile. A nonrenewable exclusive claim lasts at most 180 seconds and never beyond
the signed command expiry. Each configuration/Pod creation has a durable intent
followed by an immutable UID acknowledgement. Results from four separate offline
Pods must agree with the reviewed oracle, and all confirmed resources must be
removed before the behavior proof is committed. That proof explicitly grants no
publication authority; it is neither an independent agent audit nor a merge gate.

A failed or expired claim cannot be adopted. Recovery deletes only acknowledged
name/UID pairs, with at most three reserved cleanup attempts and a 30-second
cooldown. A lost create reply leaves an unknown UID: the controller neither
retries creation nor deletes by name, and blocks further claims. Operator-directed
infrastructure recovery must establish ownership separately; it cannot manufacture
a successful verification record.

The `verification` installation profile activates this controller through a
separate service, database role and restricted Kubernetes service account.
`candidate` mode retains its three operations and unvalidated-artifact boundary.

## Install the independent verifier

First publish the reviewed verifier implementation and refresh the image lock
and adjacent evidence from that same successful main publication. The installer
rejects verification mode if its image evidence omits the verifier or names a
different source, platform or registry. An older candidate lock does not enable
this new profile merely because the source checkout changed.

```bash
./install.sh plan --config examples/homedir-verification.local-install.json
./install.sh local --config examples/homedir-verification.local-install.json \
  --github-auth gh-cli --codex-auth-file /private/path/auth.json
```

This reconciles the existing Homedir kind scope and keeps the production test
target `ai-sdlc-production`. The verifier has a dedicated container, identity,
database role and service on internal port 8087. Its only command is the signed
worker `verify-artifact` message delivered by the relay. Its Kubernetes role can
create/get/delete Pods and ConfigMaps, read Pod logs and inspect NetworkPolicies
only in `ai-sdlc-verifiers`; it cannot exec into Pods, read Secrets or modify
policies. Generated code runs in separate gVisor Pods without credentials or
network access. Only the controller can fetch the pinned public source.

Switching back to `candidate` revokes verifier and worker-handoff SQL privileges
and removes the fourth authority grant. The public installation report exposes
`independentVerificationEnabled`; readiness reports `verification-only`.
A passing behavior proof remains insufficient for publication, agent audit,
merge, Konflux policy acceptance, release or promotion.

## Reproduce the composed runtime fixture

Build the two test executables from `runtime/k8s` using the pinned Go toolchain:

```bash
go test -c -o /private/fixtures/candidate.test ./internal/execution
go test -c -o /private/fixtures/verifier.test ./internal/verificationflow
```

Use the existing disposable `ai_sdlc_test` database and an offline-runtime kind
cluster. Supply `AI_SDLC_TEST_DATABASE_URL` through the local test environment;
never put credentials in a command or report. The fixture resets that database
and refuses to adopt either existing executor or verifier namespace.

```bash
python3 tools/verify_verification_flow.py \
  --kubectl /path/to/kubectl --kubeconfig /private/fixture-kubeconfig \
  --candidate-binary /private/fixtures/candidate.test \
  --verifier-binary /private/fixtures/verifier.test \
  --worker-image REGISTRY/WORKER@sha256:DIGEST \
  --mailbox-image REGISTRY/MAILBOX@sha256:DIGEST \
  --source-sha PINNED_HOMEDIR_COMMIT \
  --candidate-directory /private/supplied-fixture-candidate \
  --report /private/verification-report.json
```

Use `python3 tools/prepare_verification_fixture.py --source-repository /path/to/isolated-homedir --output /private/new-fixture-candidate` to reproduce the committed synthetic candidate patch for the pinned source. The output directory must be new; the source checkout is read-only. CI runs the same five scenarios as installation conformance.

The candidate directory supplies only the two recipe files. Model responses and
the issue are synthetic; SCC, source fetch, PostgreSQL, signed inbox/outbox,
restricted service accounts, gVisor Pods, comparison and cleanup are real.
Run `--scenario revoke-config`, `revoke-results`, `expire-config`, and
`lost-config-reply` separately to exercise revocation, injected disposable claim
expiry and a real API create whose UID response is discarded. The lost-reply
scenario preserves the unknown resource until the harness deletes its own
namespace by the namespace UID; runtime recovery remains uncertain and blocked.
These fixtures do not demonstrate a live issue-to-production E2E.
