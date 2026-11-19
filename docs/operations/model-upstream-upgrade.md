# Switch the model gateways to an OpenAI-compatible upstream

`runtime-upgrade model-upstream` switches the worker (generation) and verifier
(independent review) loopback model gateways of a completed
[repair installation](review-repair-upgrade.md) from the bundled Codex CLI to one
fixed OpenAI-compatible upstream, for example a local llama.cpp server exposed
only on the kind bridge.

There is no schema change and no approval renewal: the parent's approvals must
still be valid. Only the worker image, which ships the upstream gateway, and the
reviewed `modelGateway` values change. The target chart must render the unchanged
installation byte-identically before the switch.

Supply a separately reviewed upstream document:

```json
{"adapter": "openai", "url": "http://10.89.0.1:18080/v1/chat/completions",
 "model": "gemma-4-26b-a4b", "cidr": "10.89.0.1/32", "port": 18080,
 "seconds": 300, "directAnswers": true, "callSeconds": 330, "reviewCallSeconds": 170}
```

The URL must be plain HTTP to one literal RFC 1918/ULA host whose `/32` (or `/128`)
CIDR and port match the egress rule. `seconds` (30-570) is the per-call ceiling
the gateways' startup benchmark must meet (see `model-gateway-adapters.md`).
`callSeconds` (at least `seconds + 20`, at most 600 and the executor profile's
`maxSeconds`) becomes the executor profile's per-call window, and
`reviewCallSeconds` (60-180) becomes the reviewer profile's window.
Upstream credentials are not supported by this mode; restrict exposure to the
bridge address.

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
a refusal leaves nothing to resume; retry when the work settles. Open repair
budgets and model responses not yet handed off also count as in flight. A
historical `uncertain` cleanup blocks the switch on purpose (containment):
resolve it through the runtime's cleanup recovery first. Under the fence, the
first pause adopts the fenced snapshot as the retained proof, because queued
work may legitimately progress between the preliminary observation and the
fence.
