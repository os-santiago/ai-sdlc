# Switch the model gateways to an OpenAI-compatible upstream

`runtime-upgrade model-upstream` switches the worker (generation) and verifier
(independent review) loopback model gateways of a completed
[repair installation](review-repair-upgrade.md) from the bundled Codex CLI to one
fixed OpenAI-compatible upstream, for example a local llama.cpp server exposed
only on the kind bridge.

There is no schema change. The operation is a uniform publication with
separately reviewed approval renewal (described below). The target chart must
render the unchanged installation byte-identically before the switch.

Supply a separately reviewed upstream document:

```json
{"adapter": "openai", "url": "http://10.89.0.1:18080/v1/chat/completions",
 "model": "gemma-4-26b-a4b", "cidr": "10.89.0.1/32", "port": 18080,
 "seconds": 300, "directAnswers": true, "callSeconds": 330, "reviewCallSeconds": 170,
 "credentialSecret": ""}
```

The URL must be plain HTTP to one literal RFC 1918/ULA host whose `/32` (or `/128`)
CIDR and port match the egress rule. `seconds` (30-570) is the per-call ceiling
the gateways' startup benchmark must meet (see `model-gateway-adapters.md`).
`callSeconds` (at least `seconds + 20`, at most 600 and the executor profile's
`maxSeconds`) becomes the executor profile's per-call window, and
`reviewCallSeconds` (60-180) becomes the reviewer profile's window.

`credentialSecret` optionally names an Opaque Secret in `ai-sdlc-runtime` with
exactly one `credential` key: the model service's bearer token, for example
the local model-as-a-service simulator in `tools/local_model_service/`. The
switch verifies the Secret's type and key names (never its value) before the
journal exists and again before every rollout phase.
Credentials embedded in the URL are refused. A bearer token is supported only
through `credentialSecret` (below). Plain HTTP neither encrypts the token nor the
requests, so the endpoint must stay on an isolated link such as the kind bridge.

Run the start and resume commands from a clean checkout (no changes or
untracked files) whose `HEAD` is exactly `--target-revision`; the installer
refuses any other tree.

```bash
./install.sh runtime-upgrade model-upstream \
  --cluster <original-cluster> --provider podman \
  --target-run <successful-main-publication-run> --target-revision <full-main-commit> \
  --model-upstream <reviewed-upstream.json> \
  --provenance-trust <reviewed-provenance.json> --release-target <reviewed-release.json>

./install.sh runtime-upgrade model-upstream-status --cluster <original-cluster> --provider podman
```

The operation is a uniform same-schema publication. Every component image
comes from the selected main publication, and the executor and reviewer profiles
point at the published worker and model-mailbox images. Because the verifier
image changes, the separately reviewed provenance trust document must select it,
and the separately reviewed release target renews its finite window. The CA
cannot rotate here. Authority policy, build configuration, budgets and scope are
inherited unchanged, which a closed value delta enforces.

The operation has its own journal, pauses all consumers under the database
write fence, exports and restores a verified backup, then stages, warms, arms
and finally re-enables intake. It cannot select another publication or document
on resume, and it never renews historical workflow authority.

There is no migration, so completed history of every stage (feedback, repair,
browser and delivery records) is retained and admitted. The operation uses the
dedicated `ai-sdlc.configuration-upgrade-fence/v1` protocol (exact current
catalog, writers excluded, history admitted). It refuses only in-flight
model-dependent work: claimed or uncleaned executions, verifications or
reviews, open model budgets or calls, and unresolved verification or review
effects. That predicate is checked before the operation journal is created, so
a refusal leaves nothing to resume; retry when the work settles. Separately,
the installer's initial observation refuses the switch while residual
execution, review, browser or build resources remain in the cluster, even when
the SQL predicate finds no in-flight model work: let their cleanup finish first. Work that
starts between that check and the fence makes the fence refuse after the
journal exists. The operation is then still unpaused and can be resumed, but
it must complete within its administrative deadline. Open repair
budgets and model responses not yet handed off also count as in flight. A
historical `uncertain` cleanup blocks the switch on purpose (containment):
resolve it through the runtime's cleanup recovery first. Under the fence, the
first pause adopts the fenced snapshot as the retained proof, because queued
work may legitimately progress between the preliminary observation and the
fence. The paused database must equal that proof until the backup is verified.
From `ready` on, the restarted runtime may progress queued or downstream work,
so later phases re-prove only database authority (schema, roles, memberships,
role settings, catalog and role identities).

A renewed console TLS leaf certificate, even under the same CA, changes the
recorded release identity, and the switch refuses it. Install such a renewal
through its own reviewed operation first.
