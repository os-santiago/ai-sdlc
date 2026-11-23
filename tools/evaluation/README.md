# Evaluation battery runner

`run_battery.py` measures how often an installed ai-sdlc runtime delivers a
set of issues on its own. For each issue the only input is the trigger label:
the runner removes it, waits 40 seconds, adds it back as the authorized actor,
and then only observes.

| Outcome | Meaning |
|---|---|
| `success` | A pull request from the runtime's branch for the issue (`ai-sdlc/issue-N-*`) was merged |
| `failure` | The runtime stopped the attempt; the report records the stage, the reason and the components' safe stop lines |
| `timeout` | Neither happened within `--timeout-minutes` |

An execution stop is final unless that execution captured a candidate that can
still be handed off. After a stop following capture (for example a failed pod
cleanup), the runtime hands the candidate to verification once cleanup
completes; that verification, review and merge then decide the attempt, and the
report keeps the stop as `executionStop`. The stop is final after all when the
handoff can no longer happen: cleanup ended `uncertain` or exhausted its three
attempts, the handoff was rejected, or the request expired without one.
Handoffs are compared per execution, so a repair execution that stops after an
earlier verification is still a failure. A request for changes is not final
either, because it leads to a repair.

A failed attempt is retried up to `--attempts`. When an issue is done, the runner removes its label, so another installation watching the repository does not pick it up. The report gives the
first-attempt rate and the rate within that budget. The runner stops with
`runtime-blocked` when a failed verification's cleanup, or an execution cleanup
that will not complete, blocks all later work,
because further attempts would only measure that block.

```bash
python3 tools/evaluation/run_battery.py --repository OWNER/REPO --issues 4,5,6 \
  --kubectl KUBECTL --kubeconfig KUBECONFIG --attempts 2 --timeout-minutes 15 \
  --output battery.json
```

It needs an authenticated `gh` CLI that can label issues as the installation's
authorized actor, and a kubeconfig for the installation. It reads the
runtime's database with `psql` through `kubectl exec`, and the worker and
verifier logs. It writes nothing to the cluster.

Tests: `cd tools/evaluation && python3 -m unittest test_run_battery`.
