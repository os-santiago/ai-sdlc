# Onboarding to ai-sdlc

This guide walks you through setting up the AI-SDLC runtime in a new or existing repository.

## Prerequisites

- GitHub CLI (gh) installed
- The `ai-sdlc` GitHub CLI extension installed (if not, run `gh extension install os-santiago/ai-sdlc`)

## Step 1: Initialize the repository

Run the following command in the repository you want to onboard:

```bash
gh ai-sdlc init
```

This command will:

1. Create a `.ai-sdlc.yaml` file with a conservative starting configuration (shadow level).
2. Create the caller workflow at `.github/workflows/ai-sdlc.yml`.
3. Output instructions for installing the AI-SDLC GitHub App and setting the required secrets.

## Step 2: Install the GitHub App and set secrets

Follow the instructions output by the `init` command to:

1. Install the `ai-sdlc` GitHub App on your repository (or organization).
2. Set the `AI_SDLC_APP_ID` and `AI_SDLC_APP_PRIVATE_KEY` as repository or organization secrets.
3. Set the `MODEL_API_KEY` secret for inference (required for the engine to run).

## Step 3: Verify the setup

After setting up, you can verify by labeling an issue with `ready-to-implement` (if you have the permission)
or by running the workflow manually.

## Trust ramp: Shadow to autonomy

New repositories start at the `shadow` level (L0) of the trust ladder. At this level, the AI-SDLC pipeline
only observes and reports what it would do without making any changes.

As the pipeline successfully completes runs (merging PRs without reverts, etc.), it earns trust and can
be promoted to higher levels automatically. The promotion criteria are based on metrics over a rolling window
of the last N runs (see [spec/trust-ladder.md](spec/trust-ladder.md) for details).

To manually adjust the ceiling for your repository, edit the `autonomy.level` field in `.ai-sdlc.yaml`.

## Next steps

- Explore the [examples](./examples) for different archetypes (library, internal-app, org-baseline).
- Read the [runtime documentation](docs/runtime.md) for details on each workflow stage.
- Check the [spec](./spec) for the normative contract.