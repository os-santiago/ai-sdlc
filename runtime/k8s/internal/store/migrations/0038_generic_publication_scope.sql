-- Publication accepts any well-formed repository: which repository an
-- installation may publish to is decided by the authenticated authority
-- policy (a reference repository or its bound project manifest), never by
-- this schema. Relaxing a constraint adds no table, column or row, so
-- configuration upgrades may apply it (see store.RelaxationMigrations).
ALTER TABLE publication_requests DROP CONSTRAINT publication_requests_repository_check;
ALTER TABLE publication_requests ADD CONSTRAINT publication_requests_repository_check
    CHECK(repository ~ '^[a-z0-9][a-z0-9._-]{0,99}/[a-z0-9][a-z0-9._-]{0,99}$');
