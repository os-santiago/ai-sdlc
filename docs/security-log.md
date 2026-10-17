# Security event log — `SECURITY_LOG.md`

Tamper-evident, hash-chained audit trail of security-relevant events
(issue #106; epic #89 "EU CRA alignment"). Under CRA the org needs an
auditable record of vulnerability reports and handling actions; this log
is where they — and every other security-relevant runtime event — land.

## Format

One event per line, plus `#` comments and `seal` attestation lines:

```
<prev-sha256> | <ISO-8601 ts> | <actor> | <type> | <description>[ | sig=<b64>]
```

`prev-sha256` is the SHA-256 of the previous event line *verbatim*; the
first event chains from 64 zeroes (genesis). Editing, deleting, or
reordering any historical event breaks every successor's stored `prev`.
This is the same chaining discipline as the run ledger
(`.github/actions/run-ledger/run-ledger.js`) — `scripts/security-log.js`
imports `canonical`, `sha256`, and `GENESIS` from it rather than
reimplementing.

Canonical event types (`scripts/security-log.js` `EVENT_TYPES`; other
lowercase slugs are accepted with a notice):

| Type | Meaning |
|---|---|
| `sanitization-block` | Injection-defense scrubber blocked/redacted content |
| `kill-switch-engage` / `kill-switch-release` | `ai-sdlc:off` toggled |
| `escalation` | `needs-human` escalation (merge gate, repair stall, …) |
| `policy-override` | Org-policy / contract override applied or rejected |
| `vulnerability-report` | Incoming vulnerability disclosure |
| `disclosure-action` | Advisory published, fix shipped, coordination |
| `log-initialized` | Genesis event — the log file was created |

## Recording events

```bash
node scripts/security-log.js log \
  --type sanitization-block \
  --actor "ai-sdlc-runtime[bot]" \
  --description "blocked prompt injection in issue comment #142" \
  [--key-file priv.pem]     # optional Ed25519 per-event signature
```

Never edit `SECURITY_LOG.md` by hand — only `log`, `seal`, and periodic
signatures touch it. Fields may not contain `|` or newlines.

## Verifying integrity

```bash
# Recompute the whole hash chain (fails on any tampered entry)
node scripts/security-log.js verify --log SECURITY_LOG.md

# Also verify per-event + seal signatures (public key committed at
# config/security-log.pub.pem when signing is enabled)
node scripts/security-log.js verify --log SECURITY_LOG.md \
  --pubkey-file config/security-log.pub.pem --strict

# Current chain tip (sha256 of the last event line)
node scripts/security-log.js head --log SECURITY_LOG.md
```

`.github/workflows/security-log-validate.yml` runs the same `verify` on
every push and PR that touches the log and fails the job on any chain
break — an altered historical entry can never land on `main` unnoticed.

## Tail protection — seals and weekly cosign signature

The hash chain detects tampering with any entry that has a successor;
nothing stores the *last* entry's hash downstream, so a bare-chain tail
rewrite is invisible. Two controls close the gap:

- **`seal` attestations** — `node scripts/security-log.js seal --log
  SECURITY_LOG.md [--key-file priv.pem]` appends a `seal` line attesting
  the current head hash and event count, optionally Ed25519-signed.
  Seals are periodic, not terminal: events continue after them and a
  later seal attests the new tip. `verify` checks every seal against the
  chain state at its position.
- **Weekly cosign signature** — `.github/workflows/security-log-sign.yml`
  verifies the chain (`--strict`), then signs the whole file with cosign
  keyless signing (workflow OIDC identity → Fulcio → Rekor) and uploads
  the bundle `SECURITY_LOG-<YYYY-MM-DD>.sig.json` to the rolling
  `security-log-signatures` release. Verify a published signature:

  ```bash
  cosign verify-blob --bundle SECURITY_LOG-<YYYY-MM-DD>.sig.json \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp 'https://github.com/os-santiago/ai-sdlc/' \
    SECURITY_LOG.md
  ```

## Signing keys (optional)

Per-event and seal signatures use Ed25519 — `keygen` emits a pair:

```bash
node scripts/security-log.js keygen --out-dir .
# private → store as a secret (e.g. AI_SDLC_SECLOG_KEY), never commit
# public  → commit as config/security-log.pub.pem so verify picks it up
```

## Exit codes

`0` ok · `1` verification/tamper failure · `2` usage or I/O error.
