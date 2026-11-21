# Try ai-sdlc on your repository (alpha)

This guide installs ai-sdlc on a local kind cluster and delivers one issue in
your repository. It takes about 30 minutes on Linux amd64 with Podman or
Docker, Python 3.11 or later, and about 16 GB of free memory.

## 1. Prerequisites

- **GitHub:** a repository you administer, with one CI workflow whose check
  must pass before a merge (for example a `test` job). The `gh` CLI must be
  logged in as an account with admin rights on it. That account is the
  authorized issue actor, and the runtime publishes with its credential.
- **Model service:** a local Codex login file (`--codex-auth-file`). A fresh
  installation always needs it, because it creates the model credential
  Secret. You can omit it only when that Secret already exists and you do not
  pass `--refresh-model-credential`. By default the runtime uses the bundled
  Codex CLI with that login. To use another model instead, also provide an
  OpenAI-compatible endpoint on a private address, such as vLLM, llama.cpp,
  LiteLLM or the [local simulator](../tools/local_model_service/README.md).
- **Toolchain:** a container image pinned by digest that runs your tests
  offline. Dependencies must be vendored or present in the image.

## 2. Write the project manifest

Start from [`examples/ai-sdlc-sample.project.json`](../examples/ai-sdlc-sample.project.json)
and read [project manifest installation](operations/project-manifest-installation.md).
Set the repository, the toolchain image and test command, the test file globs,
the paths a change may touch, and your required check (context and GitHub App
ID, which is `15368` for GitHub Actions). Add a JUnit report path if your tests
can produce one; it makes verification decisions strong instead of weak.

## 3. Describe the model service (OpenAI-compatible endpoint only)

Skip this step if you use the bundled Codex CLI. For an OpenAI-compatible
endpoint, write a document like this one:

```json
{"adapter": "openai", "url": "http://10.89.0.1:18090/v1/chat/completions", "model": "your-model",
 "cidr": "10.89.0.1/32", "port": 18090, "seconds": 300, "directAnswers": false,
 "callSeconds": 330, "reviewCallSeconds": 170, "credentialSecret": ""}
```

The endpoint must be a literal private address reachable from the kind node.
At startup each model gateway benchmarks the model and derives a call timeout
`T` (at most `seconds`). Work starts only when `T + 20` fits the window:
`T + 20 <= callSeconds` for generation, and `T + 20 <= reviewCallSeconds` for
review. With the values above, a model measured at more than 150 s can
generate but will never be reviewed. Check the `model-benchmark` line in each
gateway's log (`timeoutSeconds`), then choose a faster model or raise the
windows. See [model gateway adapters](operations/model-gateway-adapters.md).

## 4. Install

With an OpenAI-compatible endpoint:

```bash
./install.sh local --cluster my-eval --repository OWNER/REPO \
  --environment ai-sdlc-production --profile verification \
  --independent-review --publication --merge --review-repair \
  --github-auth gh-cli --codex-auth-file ~/.codex/auth.json \
  --model-upstream upstream.json --project-manifest manifest.json
```

With the bundled Codex CLI, run the same command without
`--model-upstream upstream.json`.

The first run creates the native merge rule on GitHub, then stops and prints
its ID. Set `merge.rulesetID` in the manifest to that ID and run the same
command again. Wait until both model gateways report their startup benchmark
before you file work.

## 5. Deliver an issue

1. Create the trigger label `ai-sdlc:shadow` in your repository.
2. Open an issue with an `## Acceptance criteria` section of bullet points.
3. Add the label to the issue, as the authorized actor.

The runtime admits and plans the issue, then generates, verifies and reviews a
candidate. It opens a pull request and merges it once your required check
passes. Every stop is fail-closed. To retry after a stop, remove the label,
wait about 30 seconds, and add it again as the authorized actor. The new label
event is admitted and planned as a new work item, which gets a fresh execution.

## 6. Know the limits

Read the [alpha release notes](releases/v0.1.0-alpha.1.md) before relying on a
result.
