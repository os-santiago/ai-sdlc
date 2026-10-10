# AGENTS.md — os-santiago/ai-sdlc

Canonical, repo-agnostic AI-SDLC: normative spec + GitHub-Actions-native
runtime. Read this file before doing anything in the repository — it is the
operating context for every agent (Devin sessions, scc workers, maintainers).

## What this repo is

- `spec/` — the normative contract surface: `ai-sdlc.schema.json`,
  `trust-ladder.md`, `definition-of-ready.md`, `risk-taxonomy.md`,
  `injection-defense.md`, `org-policy.md`, `labels` taxonomy
- `.github/workflows/` — reusable `workflow_call` runtime stages:
  `ai-sdlc-intake` (DoR), `ai-sdlc-implement` (engine run → PR),
  `ai-sdlc-automerge` (merge gate), `ai-sdlc-verify` (post-merge smoke),
  plus the self-caller `ai-sdlc.yml` (this repo dogfoods itself)
- `.ai-sdlc.yaml` — this repo's own contract; validate with
  `tools/validate-ai-sdlc-config.sh`
- `docs/` — `runtime.md` (workflow reference) + `architecture/adr/` +
  `problems/` (options/trade-offs analyses feeding ADRs)
- `examples/` — archetype contracts (library = conservative, internal-app
  = aggressive)

## Ecosystem layering (do not violate boundaries)

```
Axel-DaMage/adev          doctrine (methodology, book, evidence)
os-santiago/ai-sdlc       THIS REPO — spec + runtime
os-santiago/sc-agent-cli  engine (upstream; co-maintained — PRs, human review)
deployments               Axel-DaMage/hermes (control plane: Discord,
                          OmniRoute, memory, orchestration), consumer repos
                          carrying only .ai-sdlc.yaml + thin caller
```

File things in the right repo: spec/runtime work → here; engine changes →
`os-santiago/sc-agent-cli` (fork `Axel-DaMage/sc-agent-cli` for the branch);
deployment/personal-infra → `Axel-DaMage/hermes`.

## Active initiative: homedir cutover (epic #30)

`os-santiago/homedir-ai-sdlc` (VPS bash worker) is being sunset; `homedir`
migrates to this runtime. **ADR-0001** (`docs/architecture/adr/`) is the
binding decision record — read it before touching anything cutover-related.
The cutover executes only after the evidence gate in epic #30 passes.

## Hard rules

- **English only** for code, commits, PRs, issues, docs.
- **No credentials in the repo** — never put API keys/tokens in
  `.ai-sdlc.yaml` or workflows. Secrets live in GitHub secrets
  (`AI_SDLC_APP_ID`, `AI_SDLC_APP_PRIVATE_KEY`, `MODEL_API_KEY`).
- **The pipeline never edits `.github/workflows/` in consumer repos** — the
  implement stage strips workflow paths from agent diffs, and the
  `ai-sdlc-runtime` GitHub App holds `Workflows: No access`. Changes to THIS
  repo's workflows go through normal PRs.
- **OmniRoute route names (`auto/*`, `devin/*`) are Hermes control-plane
  only** — unreachable from GHA runners by design (ADR-0001). The GHA
  default provider is **GitHub Models** (`models.github.ai`, `GITHUB_TOKEN`
  + `models: read`); `MODEL_API_KEY` switches to a direct provider.
- **scc config.json is the engine's single source of truth** on the runner —
  never rely on env/CLI-flag overrides for provider config (the VPS worker's
  iteration-#13 lesson).
- **Every mutable step is bounded**: `timeout-minutes`, `max_seconds`,
  `max_steps`, `max_repairs`, bounded scan sizes. No unbounded loops.
- **Identity**: pipeline automation runs as `ai-sdlc-runtime[bot]` (GitHub
  App installation tokens via `actions/create-github-app-token`).
  `GITHUB_TOKEN` pushes/PRs do not fire downstream workflows — anything that
  must trigger CI needs the App token (minted per-run from
  `AI_SDLC_APP_ID` + `AI_SDLC_APP_PRIVATE_KEY`; fail-closed, no PAT fallback).
- **Golden rule**: no pipeline PR stays open unresolved — merged, or
  `needs-human` with an explicit cause.

## How work flows here (bootstrap stage)

- Issue labels: `ready-to-implement` dispatches the self-dogfood GHA
  pipeline; skip labels (`epic`, `wontfix`, `needs-human`,
  `needs-refinement`, …) veto dispatch; `scc:*` = run states,
  `pr:risk-*` = risk taxonomy.
- During bootstrap (pre-pilot, epic #30 gate): **do not label runtime issues
  `ready-to-implement`** — the implement loop is still proving itself.
  Dispatch work via Devin cloud sessions or manual PRs instead.
- Branch names: `fix/issue-N-<slug>` / `feat/issue-N-<slug>` /
  `docs/<slug>`. PRs reference issues (`Closes #N` / `Refs #N`), squash
  merge, delete branch.
- Spec changes are semver-sensitive: a breaking schema change = MAJOR bump
  of `spec/v*` tags + migration note.

## Contribution conventions (DCO · vouch · problem docs)

Governance conventions every agent run must honor — full text in
`CONTRIBUTING.md`, format rules in `docs/problems/README.md`.

- **DCO**: human-authored commits carry a `Signed-off-by:` trailer
  (Developer Certificate of Origin 1.1). Bot/App-authored commits —
  `ai-sdlc-runtime[bot]` and other `*[bot]` identities — are exempt: the
  bot identity plus run evidence is the authorship trail. Agents MUST NOT
  add `Signed-off-by` on a human's behalf — a sign-off the author did not
  make is a false attestation.
- **Vouch gate**: a first-time contributor's PR is not merge-eligible
  until a maintainer vouches (`/vouch` or explicit endorsement comment).
  Vouching is a human act — agents never vouch and never treat an
  unvouched first-timer PR as mergeable. Pipeline-authored PRs are exempt;
  the merge gate governs them.
- **Problem docs**: open design questions get
  `docs/problems/NNNN-<slug>.md` — problem, options with trade-offs, an
  **Open questions** section — *before* any ADR. When a decision lands,
  the ADR links the problem doc (`Resolves:`) and the problem doc links
  back (`Resolved by:`); the reversal pair is mandatory. Agents write
  problem docs for open questions; ADRs only for decisions actually
  taken.
- **Commit conventions**: per `CONTRIBUTING.md` — English only,
  conventional-commit subjects (`type(scope): summary`; squash merge
  makes the PR title the subject), `Closes #N` / `Refs #N` in the body.

## Validate before opening a PR

```bash
# Full repo check — also what `npm test` / `npm run build` run
bash tools/validate-repo.sh

# Workflow lint — same pinned release CI runs (wrapper fetches it on demand;
# `bash actionlint -color` if a checkout lost the exec bit)
./actionlint -color

# Contract validation only
bash tools/validate-ai-sdlc-config.sh .ai-sdlc.yaml spec/ai-sdlc.schema.json
```

## Pointers

- ADR-0001 (cutover): `docs/architecture/adr/0001-homedir-ai-sdlc-sunset-and-cutover.md`
- Contribution conventions: `CONTRIBUTING.md` · problem docs: `docs/problems/`
- Cutover epic: issue #30 · Runtime epic: issue #7
- Board: org project **AI-SDLC Board — Runtime & Cutover**
- Engine: `os-santiago/sc-agent-cli` · Control plane: `Axel-DaMage/hermes`
