-- Extend only the reviewed repository allowlist. Existing RLS, ownership,
-- effect journals and terminal records retain their original constraints.
ALTER TABLE publication_requests DROP CONSTRAINT publication_requests_repository_check;
ALTER TABLE publication_requests ADD CONSTRAINT publication_requests_repository_check
    CHECK(repository IN ('os-santiago/homedir', 'os-santiago/saferus'));
