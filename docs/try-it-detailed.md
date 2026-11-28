# Try it: deliver one issue and watch it (detailed)

This guide shows the whole autonomous flow on one real issue: from the moment
you label it to a merged pull request, watched live in the read-only flow
console. The short version is [Try it (TL;DR)](try-it.md).

## What you need

- **An installation with delivery enabled** for one repository you control.
  The [adoption guide](adoption.md) covers it: a reviewed
  [project manifest](operations/project-manifest-installation.md) (how the
  repository is verified, and which files a candidate may change), a model
  endpoint, and the install command.
- **The flow console.** Add `--flow-console` to the install command. It is a
  read-only view of the runtime's own journal; it cannot change anything. See
  [flow console](operations/flow-console.md).
- **A model endpoint the gateways can use.** At startup each model gateway
  benchmarks the model. Wait until both report it before filing work: look for
  the `model-benchmark` line in the `model-gateway` container logs of the
  worker and verifier pods. A hosted model typically delivers a small issue in
  about 4 to 8 minutes (median under 5 in batteries 36–40); a local model on modest hardware is much slower (see
  [selecting the generation budget](operations/candidate-runtime.md#selecting-the-generation-budget)).
- **The GitHub account the installation authorized.** With `--github-auth
  gh-cli`, that is the account of your `gh` session. Only its label events
  count.

Reference measurements on the sample repositories are in
[the evidence notes](evidence/2026-11-26-claude-cli-agent-experiment.md).

## 1. Install with delivery and the console

```bash
./install.sh local --cluster my-eval --repository OWNER/REPO \
  --environment ai-sdlc-production --profile verification \
  --independent-review --publication --merge --review-repair \
  --github-auth gh-cli --codex-auth-file ~/.codex/auth.json \
  --model-upstream upstream.json --project-manifest manifest.json \
  --images /tmp/ai-sdlc-images/images.lock.json --flow-console
```

- **Console image pin.** The installer reads `console-images.json` beside the
  image lock. Resolve both from a signed `containers` run on `main` before the
  first install (see [enable the console](operations/flow-console.md#with-the-installer)):

  ```bash
  python3 tools/install_images.py --run RUN_ID --revision FULL_COMMIT_SHA \
    --include-console --output /tmp/ai-sdlc-images
  ```

  The tool refuses an existing output directory, so use a new one each time.

- **Merge rule ID.** The first run creates the native merge rule on GitHub,
  then stops and prints its ID. Set `merge.rulesetID` in the manifest to it
  and run the same command again.
- **Node resources.** On kind, the installer sets each node container's pid
  limit (8,192 by default, `--node-pids-limit`) and can cap its memory and
  CPUs (`--node-memory`, `--node-cpus`, off by default). It refuses a pid
  limit that cannot hold one verification run and the Tekton CI quotas; see
  [kind node resources](operations/kind-node-resources.md).
- **Worker nodes.** `--workers N` (0 to 4, default 0) creates a kind cluster
  whose sandboxed and CI pods run only on the workers (Tekton CI and the
  verification runs on one CI worker), so verification load does not starve
  the control-plane, the runtime database or the API server.
  It applies when the cluster is created. The installer prints every node IP;
  a host model service that allowlists clients must allow each one.
- **One repository per cluster.** The installer refuses to change the
  repository of an existing installation. To try another repository, use
  another `--cluster` name.
- **Optional: authenticated source reads.** A read-only, single-repository
  token for the verifier is described in
  [source availability](operations/source-availability.md#authenticated-verification-and-review-reads).
  Without it, public repositories are read anonymously.

## 2. Open the console

```bash
kubectl --kubeconfig ~/.local/state/ai-sdlc/my-eval/kubeconfig \
  --context kind-my-eval -n ai-sdlc-runtime \
  port-forward svc/ai-sdlc-flow-console 8080:8091
```

Browse to <http://127.0.0.1:8080/>. The installer keeps each cluster's
kubeconfig in its own file (above, for `--cluster my-eval`; under
`$XDG_STATE_HOME` if you set it) and prints this
command at the end of the run.

What you see:

- **The header** shows `live · journal` and the last read time while it is
  connected. It also has a language switch (ES/EN), a light/dark theme and
  "Reduce motion".
- **The cards:** work in flight, delivered (first attempt or after a retry),
  failed, blocked or quarantined, median time to delivery, and model call
  latency.
- **"Work now"** counts items per stage: queued, generating, verifying,
  reviewing, publishing, merging, blocked.
- **"What is wrong now"** lists blocked or quarantined work with its reason,
  stages running longer than usual, and bursts of problems.
- **The timeline** has one row per issue. Each stage is a bar: solid when
  completed, striped while running, outlined while waiting, hatched when
  blocked or quarantined. A cross marks a verdict against the work, and a
  square a safe stop with a diagnostic. Small ticks are model calls, and a
  flag marks the issue closing after merge. Click a row to see its ordered
  events. The legend under the timeline lists every mark.

## 3. File an issue the runtime can deliver

Write it like a small, reviewable task:

- **An `## Acceptance criteria` section** of bullet points that a test can
  check.
- **One small behavior** inside code that already exists: a function in an
  existing package or module. Issues that need a new top-level package
  (Python) are refused today. The check that the new test fails on the old
  code does not see the new package.
- **Within the manifest's publication rules.** Only the paths in
  `publication.allow` may change, within its file and size limits. In the
  Python example these are `.py` files below the root (`**/*.py`; root-level
  `*.py` is denied) and `README.md`, at most 6 files, 64 KiB in total and
  32 KiB per file.

Example:

```markdown
Report odd numbers.

## Acceptance criteria

- `is_odd(n: int) -> bool` reports whether n is odd.
- Tests cover 0, a negative odd number and a positive even number.
```

## 4. Trigger it with the label

Add the label `ai-sdlc:shadow` to the issue, as the authorized account. If the
label does not exist yet, create it in the repository. Creating or editing an
issue without the label does nothing.

## 5. Follow it

Type the issue number in the console's "Issue" filter, or keep "Follow now"
on. The row goes through these stages:

| Stage | What happens | Typical (hosted model) |
|---|---|---|
| Intake and admission | The issue and its criteria are observed and admitted | seconds |
| Generation (G) | SCC writes the change and its tests in an offline sandbox, using the model as a service | 1–3 min |
| Verification (V) | The repository's own tests run offline: on the old code, and twice on the change (`verification.repeat`); a new test must fail on the old code and pass with the change | about 1 min |
| Review (R) | An independent, read-only model review of the diff; at most one autonomous repair if it requests changes (with `--review-repair`, within the remaining budget) | about 1 min |
| Publication (P) | Branch, commit and pull request on GitHub | seconds |
| Merge (M) | Merged once the required check passes; the issue closes | under 1 min after the check |

On GitHub you will see the pull request open, its check run, the merge and the
closed issue.

## When it stops

Every stop is fail-closed. The stage and diagnostic are shown in the row's
label, next to its last bar, and in its events. Blocked or quarantined work
also appears in "What is wrong now". The runtime does not start a second attempt by itself. To
retry, remove the label, wait about 30 seconds (one intake poll) and add it
again; the new label event becomes a fresh work item.

| You see | Meaning | What to do |
|---|---|---|
| Nothing happens | No label, or the label was added by an account the installation did not authorize | Check the label and the account |
| The row stays queued before generation | The gateways have not benchmarked yet | Wait for the `model-benchmark` line in both gateways |
| Gateways never benchmark (`model-benchmark-failed`, reason `model unavailable`; the gateway restarts) | The model endpoint is unreachable or refuses the gateway | Check the endpoint's allowlist or token. With the [local model service](../tools/local_model_service/README.md), a recreated kind node gets a new address, and the service must come from the same revision as the cluster |
| `refused-candidate-failed` | The candidate's own new tests failed verification | Re-label. Without a generation toolchain (e.g. Python), the generator cannot run tests and sometimes miscomputes an expected value |
| `refused-no-failing-test` | No new test fails on the old code | Make the criteria test new behavior. Avoid new top-level packages (Python) |
| Review `request-changes` | The independent review rejected the change (after its repair, when repair is enabled and budget remains) | Re-label, or sharpen the criteria |
| `...-helper-transport` | A transient pod-exec transport failure | Re-label |
| `publication-quarantined` | A forge write's outcome is uncertain | The release manager observes GitHub and clears it itself when GitHub proves the write happened or did not (re-checked after 30 s, backing off to every 15 min). Other outcomes stay quarantined and need an operator ([publication reconciliation](operations/publication-reconciliation.md)) |
| `merge-quarantined` | A merge's outcome is uncertain. Every later merge is blocked; publication and the release manager's readiness are not affected | The [merge reconciler](operations/merge-reconciliation.md) clears it by itself, about 2 minutes after the claim, when GitHub shows the pull request merged exactly as planned or still open and untouched; disabling merge does not clear it. If it stays (`ai_sdlc_quarantined{journal="merge"}` at 1), follow the [operator procedure](operations/merge-reconciliation.md#when-it-stays-quarantined-operator-procedure) |

Every verification refusal carries the reason `comparison-failed`; the
diagnostic (`refused-…`) names the rule. The release manager's log names the
stage of a publication stop, for example
`publication reconciliation stopped safely: tree-status-422`.

## Limits

- **Easy issues.** This is an alpha. It has been measured on small functions
  in sample repositories, not on large changes. See the
  [alpha release notes](releases/v0.1.0-alpha.1.md) and the
  [capability matrix](capabilities.md).
- **Read-only console.** It never acts on the flow. Retries and fixes go
  through GitHub (the label) or the installer.
