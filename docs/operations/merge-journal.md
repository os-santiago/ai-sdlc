# Fresh merge authority and durable outcomes

The merge journal is inactive infrastructure for the Homedir pilot. No installed
profile grants access to its tables, and it exposes no public input, controller
or GitHub mutation. The only product input remains an eligible issue.

A completed publication is historical evidence. Its expired generation,
verification, review and publication leases cannot be renewed while CI runs.
The next stage requires a new signed `merge-publication` capability owned by
`release-manager`, with an exact `auto-merge-low` ceiling and at most two minutes
of validity. The authenticated gate imposes a tighter 30-second lifetime on the
entire command and claim; no effect or completion can outlive that observation.
It binds the immutable publication record, authenticated gate
observation, repository, issue, PR, head, base, tree and branch.

The journal allows one request per publication and one non-adoptable claim for
at most 90 seconds, further capped by the gate and command expiry. Its single external-effect intention must commit before a
transport is attempted. A missing acknowledgement quarantines subsequent merges;
only a trusted pre-send result can attest that no transport was attempted. No
HTTP retry, claim adoption or administrative replay is provided. After the claim
has settled, [merge reconciliation](merge-reconciliation.md) may resolve the
effect of a failed request through `ResolveMergeEffect`, from a read-only forge
observation only; the request stays failed and no merged record is written.

Every authority-bearing journal boundary revalidates the receipt against the
current policy loader, including freeze and the exact autonomy ceiling. Gate
bytes and the public policy are archived; terminal reads revalidate the original
signature and gate at the database completion time.

Completion atomically records the acknowledged merge and an immutable terminal
snapshot. The database assigns its timestamp; the component cannot backdate,
update or delete it. Reading this fact does not authorize deployment or renew
merge authority. Inconsistent or missing legacy records fail closed.

Before activation, the operational controller must re-read current issue,
policy and authenticated forge observations at each effect boundary, enforce
native strict required checks and resolved review feedback, and send a single
head-conditioned merge request. Its independently reviewed installer must grant
only the columns required by that capability. Artifact build, provenance,
promotion and deployment remain separately authorized stages.
