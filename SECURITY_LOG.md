# SECURITY_LOG.md — tamper-evident security event log (issue #106, epic #89)
#
# Append-only audit trail of security-relevant events (sanitization blocks,
# kill-switch toggles, escalations, policy overrides, vulnerability handling).
# One event per line:
#
#   <prev-sha256> | <ISO-8601 ts> | <actor> | <type> | <description>[ | sig=<b64>]
#
# `prev-sha256` is the SHA-256 of the previous event line; the first event
# chains from 64 zeroes. `seal` lines are periodic (optionally Ed25519-signed)
# attestations of the chain head and event count. Never edit this file by
# hand — append via `node scripts/security-log.js log`.
#
# Verify:  node scripts/security-log.js verify --log SECURITY_LOG.md
# Docs:    docs/security-log.md
0000000000000000000000000000000000000000000000000000000000000000 | 2026-10-17T18:42:58.598Z | ai-sdlc-runtime[bot] | log-initialized | security event log initialized (issue #106)
