# Claude CLI as the generative agent: what works in the autonomous flow, 2026-11-26

**Question:** with the local model, batteries 30–32 delivered 16 of 21 distinct issues (76.2% [54.9%, 89.4%], counting each issue's first trial). Do the remaining failures come from the generative part (SCC driven by the local model), or from the rest of the flow?

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
| 30–32 | SCC + local model (Qwen3-Coder-30B-A3B) | 16/21 = 76.2% [54.9%, 89.4%] | 9/21 | 884 | 15–32 |
| 33–34 | Claude CLI through the bridge | **18/18 = 100% [82.4%, 100%]** | 18/18 | 402 | 5–6 |

The reports are attached: battery [30](2026-11-25-evaluation-battery-30.json), [31](2026-11-25-evaluation-battery-31.json), [32](2026-11-26-evaluation-battery-32.json), [33](2026-11-26-evaluation-battery-33.json), [34](2026-11-26-evaluation-battery-34.json) and, for the bridge rerun on install d22e8b4 (issues 276–281, 6/6 on the first attempt, median 403 s, 5 model calls each), [35](2026-11-26-evaluation-battery-35.json). Batteries 30–32 are counted by the first trial of each distinct issue (`summarize.py --dedupe first`). Intervals are Wilson 95% intervals from the evaluation harness (#786).

The bridge's log recorded 18 Claude CLI runs for the battery issues:
- median 101 s, at most 194 s, with a median of 21.5 agent turns;
- 4.14 USD in total;
- every replay applied cleanly, and `go test ./...` passed inside the sandbox before export.

A smoke run before battery 33, and the setup runs before it (3 of which failed), are excluded.

Twelve changes only added files. Six (248, 232, 233, 259, 263 and 266) also modified existing files, replayed through `edit_file`.

## Findings

1. **The flow outside generation works end to end on these issues.** With the bridge's candidates, every stage after generation delivered every issue on the first attempt, with no human step: label intake, admission, planning, the sandboxed executor, candidate export, verification, the independent review (still on the local model), publication and merge. None of the transport, authority or review stops seen with the local model appeared. The host was less loaded, though, because the local model served only reviews, so this does not show those stops are gone.
2. **SCC's tool execution and the gateway are not the bottleneck.** SCC applied every replayed change: new files, patches to existing files and test runs. The gateway's guards (#797, #806, #809) did not fire. The replay read no files, so they had nothing to judge; this is not a false-positive test.
3. **The generative part is.** With the local model, most failures came from inside SCC's loop. Four of the five undelivered issues ended on the generation budget, and the fifth (227) at a review stop (`model-request-binding`):
   - the generation budget ran out (exit 22): at the local model's speed, 32 steps do not fit in 900 s;
   - candidates failed their own tests;
   - the model damaged existing files (whole rewrites, copies, destructive patches), now limited by gateway guards.

   Claude's own loop took a median of about 100 s per issue.
4. **The local reviewer did not block delivery.** It approved all 18 candidates. No weak candidates were in this set, so this says nothing about whether it rejects bad ones.

## What this suggests

- **Make generation fit the local model's speed.** Tune the time and step budgets together, and reduce the turns a change needs: fewer exploratory reads, and apply the change once tests pass. Then measure again.
- **Isolate SCC's loop from the model.** Run SCC with a hosted model through the existing agent-CLI model path. That path is also the Beta gate's second model path. If it delivers close to 100%, the local model is the likely limit. If not, the loop needs work. Battery 36 ran it afterwards (see the next section): SCC with Claude Sonnet as its model delivered 12 of 12 issues, so the local model is the likely limit.
- **Treat an external agent as a possible product mode, not a default.** A hosted-model agent outside the cluster delivered every issue, but generation would then run outside the executor sandbox and send source to a provider. Adopting it needs an explicit design (the source boundary, the credential custody, and replay into the sandbox as here) and a maintainer decision.

## Follow-up: SCC with a hosted model (battery 36)

After the maintainer allowed the kind node, battery 36 ran the product path: SCC's own loop inside the executor sandbox, with Claude Sonnet as its model through the agent-CLI OpenAI-compatible service on the host (`tools/local_model_service`). No bridge and no replay. Review also ran on Claude Sonnet.

- **Install:** d22e8b4, in place on the same kind cluster, with `--model-upstream` pointing at the service and `--flow-console`.
- **Corpus:** 12 new issues (288–299) in `os-santiago/ai-sdlc-sample`, label trigger, two attempts each.
- **Result:** 12/12 delivered within two attempts, **100% [75.8%, 100%]**; 11/12 on the first attempt (91.7% [64.6%, 98.5%]). Median 390 s to delivery (at most 541 s, counting every attempt of an issue as the table above does), 5–15 model calls per delivering attempt (median 7). [Report](2026-11-26-evaluation-battery-36.json).
- **The one failed attempt** (288, first attempt) stopped in the independent review with `model-mailbox-read-helper-transport`: the pod exec exchange reading the review mailbox failed (an exec error, its 5 s read limit or the session expiring; the report does not distinguish them). This is most likely not a model or SCC failure, and the retry delivered.
- **Setup finding:** the host service had been installed from an older revision. It rejected the gateway's `stream` and `stream_options` parameters with a 400. It logs only completions, so the gateways reported just "model unavailable". Updating the service from main fixed it. A host-side copy of the service must come from the same revision as the cluster.

**Reading:** SCC's loop and tools, with the guards on, deliver on this corpus when the model is fast and capable: about 7 calls instead of 15–32. The local model's speed and quality are the likely binding limit on the local path, not the loop (different issues and revisions, so not a controlled comparison).

**Beta gate:** this is a first measurement of the `os-santiago/ai-sdlc-sample` × agent-CLI cell: 12 of the 60 distinct issues that cell needs, and not yet gate evidence, because a gate claim uses only batteries of the release's revision. The same model path on the Python sample (`os-santiago/ai-sdlc-sample-python`) is a separate cell that also needs 60. The report records the cell's model path as `agent-cli-claude-sonnet`; later batteries must record the same label to pool into this cell.

## Follow-up: the same path on the Python sample (battery 37)

Battery 37 repeated the product path on the second sample repository, in another language: `os-santiago/ai-sdlc-sample-python` (Python 3.12, standard library, `unittest`, the manifest from #821).

- **Install:** 32321e1 (main, with #821 and #822), on a fresh kind cluster `ai-sdlc-python`. The installer refuses to change the repository of an existing installation, so the Go cluster could not be reused.
- **Corpus:** the repository's 12 seed issues (1–12), label trigger, two attempts each, cell label `agent-cli-claude-sonnet`.
- **Result:** 12/12 delivered within two attempts, **100% [75.8%, 100%]**; 10/12 on the first attempt (83.3% [55.2%, 95.3%]). Median 281 s to delivery (at most 411 s, counting every attempt), 5–9 model calls per delivering attempt (median 7). [Report](2026-11-26-evaluation-battery-37.json).
- **Two failed first attempts:**
  - Issue 4: verification refused the candidate because its own tests failed (`refused-candidate-failed`, reason `comparison-failed`). That is the verifier working as intended, though the candidate was not inspected; the retry delivered.
  - Issue 5: the review's mailbox read hit the same transport stop as issue 288 in battery 36. It is tracked in #826.

**Reading:** the path delivers on a second language with the same median call count as on Go (7; 5–9 here, 5–15 on Go). Each repository now has a first 12-issue measurement on this model path: 12 of the 60 distinct issues each cell needs. They are not gate evidence, because they ran on development revisions.

## Follow-up: credential, generator guidance and two forge stops (batteries 38–40)

All three ran on the Python sample with the same cell label (`agent-cli-claude-sonnet`).

| Battery | Install | Issues | Within two attempts | First attempt | What it tested |
|---|---|---|---|---|---|
| [38](2026-11-26-evaluation-battery-38.json) | 32321e1 | 26–31 (new) | 5/6 (83.3% [43.6%, 97.0%]) | 4/6 | The read-only source credential (#796) mounted in the verifier |
| [39](2026-11-26-evaluation-battery-39.json) | 0694b2f | 37–46 (new) | 3/10 (30.0% [10.8%, 60.3%]) | 3/10 | The generator guidance from #828 |
| [40](2026-11-26-evaluation-battery-40.json) | 127e220 | 26, 38, 41–46 (re-delivery) | 7/8 (87.5% [52.9%, 97.8%]) | 7/8 | The merge deadline and stop diagnostics from #829, on a fresh cluster |

- **Battery 38:** verification and review read the source with the read-only credential, and no stop involved it. Two first attempts were refused because the candidates' own tests failed. In one the expectation was hand-computed wrong. In two other refused candidates (here and in battery 37) the tests imported `pytest`, which the offline verification image lacks. Issue 26's retry timed out after the review requested changes.
- **Battery 39:** #828 tells a generator without a toolchain to use the repository's test framework and names the reviewed verification command. All 16 candidates used `unittest` (none used `pytest`). Delivery still fell to 3/10. Six of the seven undelivered issues stopped for two reasons outside generation; the seventh (46) was refused because its own test was wrong (see battery 40):
  - **A merge left uncertain.** The merge transport waited only 5 s for GitHub's response. GitHub merged PR 49 about 5.1 s after the effect began, so the merge was journaled as uncertain. With no merge reconciler (#830), every later merge was quarantined, and release-manager readiness failed with it. Five issues passed review and then timed out waiting to publish. The second attempt of one of them (43) stopped earlier, in candidate execution, on a transport stop (`execution-read-helper-transport`).
  - **A publication stop.** One issue's tree write stopped about 3 s after it began, twice, with the HTTP status discarded. Replaying the same digest-verified request later returned 201 with the planned tree SHA, so the content (which included non-ASCII text) was not the cause.
- **Battery 40:** after #829 (a longer merge response deadline, and stop logs that name the stage and HTTP class), on a fresh cluster, it re-delivered the eight issues battery 38 or 39 left undelivered. Seven delivered on the first attempt, including the one whose tree write had stopped. The eighth (46) was refused in all four attempts across batteries 39 and 40. Each time the code was right and the candidate's own test expected a wrong count.

**Reading:**
- The merge stop came from a pre-existing 5 s response deadline, not from #828. The tree-write stop's cause is unknown, because its HTTP status was discarded. The replay rules out the content, and the stop did not recur in battery 40.
- Battery 40 re-delivers issues that failed before, so it is not a fresh sample, and the harness counts those issues from their earlier trials. With #829, its seven merges completed with no uncertain merge, and the tree-write stop did not recur. A slower merge would still quarantine merges until #830 adds a reconciler.
- In these batteries, the remaining refusals were wrong expectations in candidates' own tests, four of them on a single issue (46). The generator cannot run tests there, because the Python image cannot be a generation toolchain (#831).

## Limits

- **Easy issues.** The corpus is small Go functions in one sample repository (and, for batteries 37–40, small Python functions in a second).
- **Not a product path.** The bridge is evaluation-only, so batteries 33–35 are not Beta gate evidence. Battery 36 is a product path but shares the easy-issue, single-Go-repository limit, and ran on an in-place install rather than a fresh cluster.
- **Not a controlled comparison.** The local-model figures come from different issues and from three revisions (7f50d07, 4f51e42 and 24c3190); only 230, 232 and 233 overlap.
- **Host load.** The local model served only reviews, so the cluster was less loaded than in the local-model batteries.
- **Guards not exercised.** The replay read no files, so the gateway guards had nothing to judge.
- **No weak candidates.** The reviewer saw none.
- **Excluded runs.** Setup and smoke runs before battery 33 are excluded from the bridge figures.
