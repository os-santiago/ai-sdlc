Improve the demo's token-lifetime presentation so a person can read the duration immediately. The current console displays `60 seconds`; display `1 minute` while preserving the numeric lifetime and server-side enforcement.

AI-SDLC validation recipe: saferus-token-lifetime/v1

## Acceptance criteria

- [ ] Format nonnegative safe integer seconds as English days, hours, minutes and seconds, omitting zero units and using singular/plural correctly; zero remains `0 seconds`.
- [ ] Cover `1 second`, `59 seconds`, `1 minute`, `1 minute 1 second`, `2 minutes`, `1 hour`, `1 hour 1 minute 1 second`, `1 day`, and `1 day 1 hour 1 minute 1 second` in unit tests.
- [ ] Preserve `Unavailable` for negative, fractional, nonnumeric, nonfinite and unsafe integer values; correctly format `Number.MAX_SAFE_INTEGER`.
- [ ] Update the browser expectation for `#ttl` from `60 seconds` to `1 minute`, preserving every other line of that file byte-for-byte.
- [ ] Limit the change to `web/static/ttl.js`, `tests/ttl.test.mjs`, and that single presentation assertion in `tools/verify_browser.py`; every other file and every non-presentation behavior is out of scope.
- [ ] Obtain independent behavior verification and review, passing required checks, then follow AI-SDLC's own signed build/provenance/promotion/deployment components to the isolated kind test-production environment. Record the deployed artifact and browser result through those components.

## Delivery readiness

Earlier autonomous attempts remain separate historical outcomes. A later attempt produced a candidate and passed independent behavior verification, but its review ended without an accepted verdict. Preserve those records. The reviewed reviewer-image update and the schema-35 browser-evidence upgrade are now active from AI-SDLC main bcea310226d4e204253c6a88408f2d03a09509c3. The fixed desktop/mobile browser check is part of the installed deployment gate. Current approvals still end at 2026-11-17T17:14:55Z; this issue does not renew authority or change budgets. Process this unchanged software request through normal issue intake and the installed policies. The destination remains the original isolated kind test-production environment. Only AI-SDLC components may publish, merge, build, verify provenance, promote, deploy or close this request.

