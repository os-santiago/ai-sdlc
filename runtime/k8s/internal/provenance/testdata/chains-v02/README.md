# Archived Tekton Chains format fixture

Public DSSE bytes and public key from the isolated closed-build validation on
2026-11-16 (source e4fc0085051a9c4dee088b5cc164d8b51b0fae67). The original
workloads and fixture namespace were removed after the archive was verified.
`binding.json` contains selected independent compiler/report expectations; it
is deliberately **not** a complete authenticated BuiltRecord. The test checks
cryptographic and format compatibility after workload cleanup, not current
registry availability, installed trust, permissions, or autonomous delivery.

The key is a fixture public key, never an operational trust anchor. No private
key, credentials, or generated authority receipts are included. The original
statement is unmodified because modifying it would invalidate the signature.
