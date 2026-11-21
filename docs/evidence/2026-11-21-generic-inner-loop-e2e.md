# Generic inner loop end-to-end run, 2026-11-21

**Scope:** a local kind installation of ai-sdlc for a repository that is
**not** a built-in reference, `os-santiago/ai-sdlc-sample`: a small Go module
with one seeded bug. The installation used only a reviewed project manifest
(`ai-sdlc.project/v1`), with no repository-specific code in ai-sdlc. The only
external input was the authorized trigger label on one issue. No delivery stage
was advanced by hand.

**Model service:** the local OpenAI-compatible simulator in front of an agent
CLI ([local model service](../../tools/local_model_service/README.md)), selected
at installation with `--model-upstream`.

## Result

The issue asked `textutil.Reverse` to reverse Unicode code points instead of
bytes, with a multibyte test. The run that delivered it:

| Stage | Result |
|---|---|
| Intake, admission, planning | Within seconds of the label |
| Generation | Candidate captured in about 3.5 minutes |
| Verification with the repository's own tests (`repository-tests/v1`) | **Passed**: baseline, cross and candidate pods ran `go test ./...` offline in gVisor; v2 behaviour proof recorded |
| Independent review | **Approved**, with the verification summary in the reviewer's input |
| Publication | The runtime opened the pull request |
| Merge | The runtime merged it after the repository's required check passed |

The issue label went on at 19:34 UTC and the merge landed at 19:43 UTC. The
merged change (`[]rune` reversal plus a `TestReverseMultibyte` case) passes the
repository's tests on its main branch.

## Defects found and their resolution

Earlier attempts in this session stopped fail-closed at stages the generic path
had not reached before. Each stop exposed a defect, now fixed:

| Stage | Cause | Resolution |
|---|---|---|
| Model provider | The bundled model credential reached its spend cap; a fresh installation could not choose another model service | `--model-upstream` at installation (#689) |
| Execution dispatch | Command delivery gave up after about 4 minutes, while the worker's model gateway was still benchmarking at startup | Delivery attempts spread over the command lifetime (#691) |
| Verification | One candidate contained invisible U+200B characters from the model path and did not compile | Correctly refused; no change needed |
| Generation budget | 16 model calls were fewer than SCC's 24 steps | One call per step for model upstreams (#692) |
| Generation budget | Agents spent their steps trying to build and test in a workspace without a toolchain | Trusted workspace guidance in the prompt (#694) |
| Verification | `FinishVerification` required the reference recipes' five resources | Per-recipe effect sets (#684) |
| Publication | A schema allowlist limited publication to the two reference repositories | Migration 0038, with scope decided by the policy (#693) |

The image pins came from exact `main` publications (#688, then later
publications for the fixes above). The installer dry run also caught a chart
guard that blocked publication for the manifest's repository (#684).

Every runtime change was merged with green CI and resolved review comments,
after an independent audit.

## Limits of this evidence

- One small issue on one small repository. The decision was *weak*: the
  repository has no JUnit report, so verification compared exit codes and the
  reviewer judged whether the new test exercises the behaviour.
- Generation is variable with this model path. Before the workspace guidance,
  about half of the generations ran out of SCC steps.
- The generator's workspace cannot run the repository's tests. Running them
  there with the manifest's toolchain is the next design step.
- Existing installations cannot yet apply migration 0038: the runtime upgrade
  tools still require the 37-migration catalog (they fail closed).
- The issue stays open after the merge, because the generated pull request
  does not reference it with a closing keyword.
