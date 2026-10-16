// scripts/gha/engine-adapter-resolver.js
// GHA-side import location for the canonical engine adapter resolver.
//
// The implementation lives in `scripts/engine_adapter.js`: one declarative
// registry (`config/engine_adapters.json`), one interpreter, shared by the
// container worker and the GHA-native path. This file is kept so existing
// GHA consumers resolve to the same contract instead of a second registry.
module.exports = require('../engine_adapter').EngineAdapterResolver;
