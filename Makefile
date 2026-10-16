# Makefile — entry points over the repo's test surfaces.
#
# The canonical suite is tools/validate-repo.sh (backed by `npm test` /
# `npm run build` and the contract job in .github/workflows/ai-sdlc.yml).
# `test-conformance` runs the engine-adapter conformance harness directly
# (issue #217) — the same checks also run under `node --test` via
# tests/engine_conformance.test.js.

.PHONY: test validate test-conformance

test validate:
	bash tools/validate-repo.sh

# Static certification: every adapter in config/engine_adapters.json must
# satisfy the declared contract surface. `--live` additionally executes each
# adapter's probe against the installed engine binary:
#   node tests/engine_conformance_harness.js --live
test-conformance:
	node tests/engine_conformance_harness.js
