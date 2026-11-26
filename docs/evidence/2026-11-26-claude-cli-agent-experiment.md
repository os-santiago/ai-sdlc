# Claude CLI as the generative agent: what works in the autonomous flow, 2026-11-26

**Question:** with the local model, batteries 30–32 delivered 16 of 21 distinct issues (76.2% [54.9%, 89.4%]). Do the remaining failures come from the generative part (SCC driven by the local model), or from the rest of the flow?

**Experiment:** an evaluation-only bridge on the host replaced the generative agent. It is not part of the product.
- **Generation.** When SCC started a generation conversation, the bridge ran a headless Claude CLI session (Sonnet) on a fresh host clone of the issue's source commit. The session could read and edit files and run `go test`, `go build`, `go vet` and `gofmt` only.
- **Replay.** The bridge then answered SCC's model calls with tool calls (`write_file`, `edit_file`, `run_shell`) that applied the resulting change inside the executor sandbox.
- **Everything else unchanged.** The bridge forwarded review requests and gateway benchmark probes unchanged to the local model. The rest of the installation was untouched: admission, the sandbox and its export, verification, the independent review on the local model, publication and merge.

**Scope:**
- **Repository:** `os-santiago/ai-sdlc-sample`.
- **Install:** 24c3190, a fresh kind cluster.
- **Trigger:** the label only, with two attempts per issue.
- **Corpus:** batteries 33 and 34, 18 distinct issues. 15 were new; 3 (230, 232, 233) had failed both attempts with the local model in battery 32.

## Results

| Battery | Agent | Delivered within two attempts | First attempt | Median seconds to delivery | Model calls per delivery |
|---|---|---|---|---|---|
| 30–32 | SCC + local model (Qwen3-Coder-30B-A3B) | 16/21 = 76.2% [54.9%, 89.4%] | — | 678–1001 | 15–32 |
| 33–34 | Claude CLI through the bridge | **18/18 = 100% [82.4%, 100%]** | 18/18 | 402 | 5–6 |

The reports are attached: [battery 33](2026-11-26-evaluation-battery-33.json) and [battery 34](2026-11-26-evaluation-battery-34.json). Intervals are Wilson 95% intervals from the evaluation harness (#786).

The bridge's own log recorded 19 Claude CLI runs:
- median 100 s, at most 194 s, with a median of 21 agent turns;
- 4.27 USD in total;
- every replay applied cleanly, and `go test ./...` passed inside the sandbox before export.

Most changes added new files. Two issues (248 and 232) modified existing files, replayed through `edit_file`.

## Findings

1. **The flow outside generation works end to end on these issues.** With strong candidates, every downstream stage delivered every issue on the first attempt, with no human step: label intake, admission, planning, the sandboxed executor, candidate export, verification, the independent review (still on the local model), publication and merge. None of the transport, authority or review stops seen with the local model appeared.
2. **SCC's tool execution and the gateway are not the bottleneck.** SCC applied every replayed change: new files, multi-hunk patches and test runs. The gateway's guards (#797, #806, #809) did not fire on the replay.
3. **The generative part is.** With the local model, the same issues failed for reasons inside SCC's loop:
   - the generation budget ran out at about 26 s per call times up to 32 steps against 900 s (exit 22);
   - candidates failed their own tests;
   - the model damaged existing files (whole rewrites, copies, destructive patches), now limited by gateway guards.

   Claude's own loop solved each issue in about 100 s.
4. **The local reviewer accepts good candidates.** It passed all 18 reviews. The reviewer was not a limiting factor here.

## What this suggests

- **Make generation fit the local model's speed.** Tune the time and step budgets together, and reduce the turns a change needs: fewer exploratory reads, and apply the change once tests pass. Then measure again.
- **Isolate SCC's loop from the model.** Run SCC with a strong model through the existing agent-CLI model path. That path is also the Beta gate's second model path. If it delivers close to 100%, the loop is sound and the local model is the limit. If not, the loop needs work. That run was not possible in this session: the host service's client allowlist did not include the new kind node, and changing it was left to the maintainer.
- **Treat an external agent as a possible product mode, not a default.** A strong agent outside the cluster delivered every issue, but generation would then run outside the executor sandbox and send source to a provider. Adopting it needs an explicit design (the source boundary, the credential custody, and replay into the sandbox as here) and a maintainer decision.

## Limits

- **Easy issues.** The corpus is small Go functions in one sample repository.
- **Not a product path.** The bridge is evaluation-only, so these batteries are not Beta gate evidence.
- **Not a controlled comparison.** The local-model figures come from different issues and installs.
