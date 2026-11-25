# Evaluation harness

The evaluation harness turns battery reports into per-cell delivery rates with
95% intervals, and checks them against the release gates in the
[release plan](../releases/release-plan.md#exit-criteria) (item B7). It has two
parts, both in [`tools/evaluation`](../../tools/evaluation/README.md):

- `run_battery.py` runs one battery against an installed runtime and writes a
  JSON report: per issue, its attempts with outcome, stage, reason, the
  components' safe stop lines, seconds and model calls.
- `summarize.py` reads one or more reports, partial ones included, and writes
  a deterministic JSON and Markdown summary per cell.

Both use only the Python standard library.

## Cells

A **cell** is one repository and one model path. The Beta gate is measured per
cell; a rate pooled across cells does not count.

| Field | Source |
|---|---|
| Repository | `cell.repository`, or the report's `repository` |
| Model path | `cell.modelPath`, set with `run_battery.py --cell-model-path`. A short lowercase label: `local-llama` (a model on local hardware behind an OpenAI-compatible server), `agent-cli` (the agent-CLI service), or `hosted-api`. |

Reports written before the label existed have no model path. Give one with
`summarize.py --model-path LABEL` for all of them, or `--label REPORT=LABEL`
per report. A label recorded in the report wins over both. A cell without a
model path is reported as `unspecified` and never passes the gate.

The report also records, when available, `installation.revision` (from
`--installation-revision`, or the tag in `--image-lock`),
`installation.imageLockDigest` (the sha256 of the image lock file) and
`installation.images` (the installed deployments' images, read best effort).
The summary lists the revisions in each cell and warns when a cell spans more
than one. A gate claim for a release should use batteries of that release's
revision only: pass just those reports.

## Trials and dedupe

The unit is the **issue**, not the attempt. Each battery run of an issue is a
**trial**: up to the attempt budget, ending delivered (a pull request from the
runtime's branch merged) or not.

- **Same trial read twice** (same cell, issue and first-attempt start, for
  example the same report passed twice) counts once.
- **`--dedupe trial`** (default): every battery run of an issue counts. Issues
  that failed are often run again, so this rate mixes first exposures with
  retries of hard issues.
- **`--dedupe first`**: only each issue's earliest complete run counts. This
  avoids the selection effect of re-running failed issues, and gives one trial
  per distinct issue.

Whatever the rule, the gate counts **distinct issues**: repeated trials of the
same issue are correlated and do not stand in for new issues.

**Partial reports.** A report written by a battery that stopped mid-issue
carries `inProgress`. That trial counts if it already succeeded or used its
whole budget; otherwise it is **incomplete**, excluded from the rates, and
reported. A trial that ended with no success after fewer than two attempts
(for example `runtime-blocked`, or an attempt budget of one) counts as not
delivered, and is reported as truncated.

## Statistics

For each cell, over its complete trials:

| Measure | Definition |
|---|---|
| Delivered within two attempts | Trials delivered at attempt 1 or 2, over trials. A success at a third attempt does not count. The gate rate. |
| First attempt | Trials delivered at attempt 1, over trials |
| pass@1, pass@2 | For each distinct issue, the fraction of its trials delivered within *k* attempts; then the mean over issues. Each issue weighs the same however often it ran. Equal to the two rates above under `--dedupe first`. |
| Attempt success | Successful attempts over all attempts |
| Second-attempt recovery | Trials delivered at attempt 2, over trials whose first attempt failed and that made a second one. This is the value of the retry. |

Each rate comes with a **Wilson score 95% interval** (z = 1.95996). With *x*
successes in *n* trials and p̂ = x/n:

```text
centre = (p̂ + z²/2n) / (1 + z²/n)
half   = z · sqrt(p̂(1 − p̂)/n + z²/4n²) / (1 + z²/n)
```

The Wilson interval stays inside [0, 1] and keeps its coverage at small *n*
and at rates near 0 or 1, where the normal (Wald) interval fails. The tests
check it against published references (Newcombe 1998).

The interval assumes independent trials. Issues in one battery share an
installation and time window (battery 12's degradation affected most of its
issues at once), so the true uncertainty is wider than the interval when
batteries differ in conditions. Running several batteries on fresh
installations, and keeping one revision per gate claim, limits this.

## Stops

Every attempt that did not succeed has a stop class: the runner's stage and
reason, and the component's **bounded diagnostic name**, the first word after
`stopped safely:` in that stage's component line (`candidate reconciliation`
for execution and handoff, `verification reconciliation`, or `independent
review`). A line without an allowlisted name is `unnamed`; an attempt with no
lines is `none`. A timeout has stage `timeout`. The summary counts stop
classes per attempt, and separately the final stop of each undelivered trial.

## Reviewer verdicts

The summary crosses each attempt's last reviewer verdict with its outcome
(delivered, failure, timeout).

| Verdict | Meaning |
|---|---|
| `approved` | A review run passed |
| `changes-requested` | The reviewer asked for changes, leading to a repair |
| `rejected` | The reviewer rejected the candidate |
| `policy-stop` | The reviewer's answer was refused by the response policy (`model-response-policy`, for example a final answer without a `VERDICT` line) |
| `review-stopped` | The review stopped without a verdict (model transport, cleanup, authority) |
| `not-reached` | The attempt stopped before review |
| `not-observed` | The report does not say |

**Reports from now on** record each attempt's review runs (`reviews`, oldest
first, as `state|reason` from the runtime's review journal) and the furthest
stage reached (`stageReached`), so the verdict is observed. The summary also
counts attempts in which changes were requested, by outcome.

**Older reports** do not record reviews, so the verdict is derived, and the
summary says how many attempts were derived:

- a merge implies an approval, because the merge gate requires one;
- a review-stage stop is a `policy-stop` or `review-stopped` by its
  diagnostic name;
- an attempt whose lines show `request-changes` or a repair handoff had
  changes requested;
- any other failure with no reviewer line did not reach review;
- a timeout with no reviewer line is not observed.

The final outcome is not independent ground truth about the candidate, so this
is not a full confusion matrix: a reviewer approving a wrong change is only
visible with an outside check (for example hidden tests or a later revert).

## The gate

A cell meets the **Beta functional gate** when all of these hold:

- its model path is known;
- it has at least **60 distinct issues**;
- delivered within two attempts is at least **80%**;
- the 95% Wilson lower bound of that rate is at least **70%**.

Each failing condition is listed as a reason. With 60 issues, 48 deliveries
(80%) are not enough: the lower bound is 68.2%. The smallest passing counts
are:

| Distinct issues | Deliveries needed | Rate |
|---|---|---|
| 60 | 49 | 81.7% |
| 70 | 57 | 81.4% |
| 80 | 65 | 81.3% |
| 100 | 80 | 80.0% |

The summary's `overall` section passes when the passing cells include two
repositories crossed with two model paths, all four passing. The release plan
also asks for repositories in different languages, which the reports do not
record; check that by hand.

## Usage

```bash
python3 tools/evaluation/run_battery.py --repository OWNER/REPO --issues 4,5,6 \
  --kubectl KUBECTL --kubeconfig KUBECONFIG --attempts 2 --timeout-minutes 15 \
  --cell-model-path local-llama --image-lock runtime/k8s/deploy/local/images.lock.json \
  --output battery.json

python3 tools/evaluation/summarize.py battery-*.json \
  --json summary.json --markdown summary.md
```

The same reports always give byte-identical output. The
[`evaluation` workflow](../../.github/workflows/evaluation.yml) runs the tests
and summarizes the committed reports in `docs/evidence` on every change to
`tools/evaluation`.
