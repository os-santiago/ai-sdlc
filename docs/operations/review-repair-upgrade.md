# Install one bounded review repair

`runtime-upgrade review-repair` is the closed schema-36-to-37 administrative
transition after a completed [feedback installation](review-feedback-upgrade.md).
It enables the [single autonomous repair](review-repair.md) for future issue
revisions and installs a new, separately reviewed finite approval window in the
same operation. A completed schema-36 installation has no other renewal path once
its earlier approvals expire.

Use the exact clean checkout of the successful immutable main container
publication whose catalog adds only `0037_review_repair.sql`. The selected commit
must equal the local HEAD, with no tracked changes or untracked files, including
when resuming the original intent.

Supply two separately reviewed documents:

- the provenance trust, changing only `verifierImage` to the selected verifier
  digest plus a new `approvedAt`/`expiresAt` window;
- the release target, changing only `approvedAt`/`expiresAt`.

Each window must start after the previous one, be current, exceed the previous
expiry and last at most 24 hours. Keys, scope, identities and registry CA cannot
change.

### Rotated application CA

The demo application's TLS chain may expire independently of AI-SDLC approvals.
After an operator rotates it in place (same Secret, ConfigMap and Deployment UIDs;
unchanged Deployment template), the reviewed release target may also change
`tlsCA`. The new CA must be a single certificate valid for the entire approved
window, issued before `approvedAt`, and byte-identical to the installed
application `saferus-ca` ConfigMap. The browser platform's original
`ai-sdlc-browser/approved-console-ca` ConfigMap must already hold the same CA.
The browser profile then changes only `caSHA256`; the real browser compiler must
reproduce that profile for the new target before the first durable intent.

```bash
./install.sh runtime-upgrade review-repair \
  --cluster <original-cluster> --provider podman \
  --target-run <successful-main-publication-run> \
  --target-revision <full-main-commit> \
  --provenance-trust <reviewed-provenance.json> \
  --release-target <reviewed-release-target.json>

./install.sh runtime-upgrade review-repair-status \
  --cluster <original-cluster> --provider podman
```

The operation has its own journal and at most 30 minutes, bounded further by the
new approvals. Its parent is the completed feedback journal, whose bytes, backup
archives and chart must stay identical. Repeating the command resumes the same
recorded operation; it cannot select other documents or another publication,
refresh an expired intent, replay an unknown migration outcome or renew historical
workflow authority.

Expired source approvals permit observation and containment only. Before the
first durable intent the installer compiles the reviewed trust, recompiles the
browser installation against the new release target (the profile must stay
identical) and requires empty feedback and repair ledgers: no historical rejection
gains a repair. The schema-36 database authority is anchored to the completed
feedback operation's verified snapshot.

After the fenced pause and private export, the installer restores the backup into
an owned offline container, runs the exact published migrator there and records
the resulting schema, row security, functions and privileges. It removes the
rehearsal and restore containers before touching the operational database. The
operational migration must then reproduce that rehearsed result exactly while
preserving every original row, role identity and migration timestamp.

The chart change is limited to the two repair relay routes and the orchestrator's
read-only public review configuration mount. Long-running database consumers move
to the selected publication; the candidate profile gains `maxRepairs: 1` while
model limits, keys, outer policy, budgets and the browser profile remain
unchanged. Staging retains every original resource UID; warming starts consumers
frozen, arming opens policy, and the final phase changes only intake replicas.

Success establishes an installed capability, not autonomous issue-to-production
delivery. The next issue revision must traverse generation, review, repair,
publication, build, provenance, deployment and browser evidence on its own.
