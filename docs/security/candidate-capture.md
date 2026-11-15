# Unvalidated candidate capture

The execution lifecycle can preserve a bounded delta after the original worker
has exited and its exported bundle has been acknowledged. This is an internal
controller prerequisite. It does not validate generated code, complete a work
item, open a pull request or authorize release. Operational roles receive no
new database grants and no live endpoint is installed.

## Exact artifact identity

`Lifecycle.CaptureCandidate` takes the exported bundle bytes. It copies them,
validates the entire source and candidate bundles and computes both tree hashes
outside the executor. The source is the immutable prepared Git revision. The
candidate must retain that source revision, contain a real change and fit the
existing workspace limits.

The persisted delta has at most 64 changes and its complete canonical JSON is
at most 2 MiB. Each change identifies one sorted, unique repository path:

- An absent preimage represents creation; the path must not exist in the source.
- An existing preimage binds the original file's mode, byte count and SHA-256.
- The replacement contains the exact bytes and mode; absence means deletion.

Mode-only changes are retained. Duplicate paths, unsafe paths, no-op entries,
invalid modes, wrong preimages, empty deltas and oversized serialized changes
are rejected. Independent reconstruction must match the candidate tree hash,
including every unchanged file. Delta decoding rejects duplicate or unknown JSON
keys and noncanonical encodings.

Changed file content remains untrusted. Capturing it does not establish that it
is safe, free of secrets, within an approved change scope or correct for users.
Those are separate validation and publishing responsibilities.

## Durable ownership and model closure

The database transaction locks the current request and lifecycle, verifies the
completed export's original Pod UID, zero worker exit, exact SHA-256 and byte
length, and locks the parent model budget. The original allocation must still
belong to the same request, attempt, source and running work item.

At least one model call must exist. Every call across all allocations of the
work item must be completed with confirmed response handoff. Pending, ambiguous,
stopped or undelivered calls prevent capture. The same budget lock serializes
new reservations and handoffs. Capture inserts one immutable record, changes
the lifecycle phase to `candidate-captured`, and stops the budget with that exact
reason. It never refunds call or byte counters. Expiry is checked using database
time after all lock/constraint waits, so an expired transaction rolls back.

Reading a sealed model binding for the post-commit check requires the captured
phase, captured stop reason and matching candidate request/allocation/Pod/source.
An unrelated stopped budget cannot become a valid captured lifecycle. Broker
authorization still requires `start-ready`; neither export nor capture permits
further model invocation.

Current policy, intake generation, readiness, preparation, placement and model
configuration are checked before and after persistence. All artifact/model
database reads precede the final current preparation check. These are successive
bounded observations, not a globally atomic revocation transaction across all
systems. A later policy change still needs the next component's own current
authority check.

A failed or ambiguous commit exposes no candidate result and stops only the
original claim. Durable evidence can remain after a lost acknowledgement; it is
never adopted or replayed as successful work. Local duplicate/concurrent calls
cannot replace a winning candidate. The work item remains `running`, and the
candidate has no validation/approval flag granting onward authority.

## Validation

Pure delta tests cover reconstruction, file ownership, create/update/delete,
mode changes, malformed identities, unsafe paths and serialized limits.
PostgreSQL tests cover exact export binding, immutable records, revoked inputs,
pending handoffs, concurrent capture, capture versus model reservation, expiry
after each persistence lock, and a lost real server COMMIT acknowledgement.

The opt-in `tools/verify_lifecycle_model.py` fixture runs the actual
kind/gVisor/SCC/PostgreSQL path against the pinned Homedir tree. Its authority and
two provider replies are synthetic. It exports the generated fixture file,
captures the delta and reconstructs it independently in Python, comparing all
original files and modes. Owned Pod, ConfigMap and namespace cleanup must pass.
This establishes composed component behavior, not autonomous Homedir delivery.

Run the complete local runtime/database validation through:

```bash
python3 tools/validate_runtime.py --provider podman
```

For the isolated Kubernetes fixture prerequisites and command, see
[lifecycle model activation](lifecycle-model-activation.md).
