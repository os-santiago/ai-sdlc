-- Large repositories: the workspace bundle wire bound rose from 32 MiB to
-- 192 MiB (workspacebundle.MaxWireBytes; docs/security/offline-workspace-bundles.md).
-- The executor's source bundle and its exported candidate are recorded by
-- size only; their bytes are never stored. This only widens the two size
-- checks to the new bound: it adds no table, column, role, grant or row, so
-- configuration upgrades may apply it (see store.RelaxationMigrations).
ALTER TABLE execution_lifecycles DROP CONSTRAINT execution_lifecycles_source_bytes_check;
ALTER TABLE execution_lifecycles ADD CONSTRAINT execution_lifecycles_source_bytes_check
    CHECK(source_bytes BETWEEN 1 AND 201326592);
ALTER TABLE execution_candidates DROP CONSTRAINT execution_candidates_export_bytes_check;
ALTER TABLE execution_candidates ADD CONSTRAINT execution_candidates_export_bytes_check
    CHECK(export_bytes BETWEEN 1 AND 201326592);
