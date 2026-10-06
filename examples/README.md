# examples/ — Contract archetypes

Ready-to-copy `.ai-sdlc.yaml` starting points. Pick the archetype closest to
your repo, copy it to the repo root as `.ai-sdlc.yaml`, and trim.

- [`internal-app.ai-sdlc.yaml`](internal-app.ai-sdlc.yaml) — internal app
  (`repo.archetype: app`) with an aggressive ceiling: `auto-merge-all`, so
  every non-critical PR self-merges on green.
- [`library.ai-sdlc.yaml`](library.ai-sdlc.yaml) — public library
  (`repo.archetype: library`) with a conservative ceiling: `auto-merge-low`,
  so only `pr:risk-low` PRs self-merge.
- [`org-baseline.ai-sdlc.yaml`](org-baseline.ai-sdlc.yaml) — org-wide floor
  template (not a repo contract): pins `autonomy.level: auto-merge-low` as the
  ceiling consumers inherit via the `org-config` input (see
  [`../spec/org-policy.md`](../spec/org-policy.md)).
