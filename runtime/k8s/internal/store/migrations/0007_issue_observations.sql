-- A successful global scan is not proof that any particular issue was seen.
CREATE TABLE intake_observations (
    repository text NOT NULL,
    issue_number integer NOT NULL CHECK (issue_number>0),
    generation bigint NOT NULL CHECK (generation>0),
    config_digest text NOT NULL CHECK (config_digest ~ '^[a-f0-9]{64}$'),
    authority_revision text NOT NULL CHECK (authority_revision ~ '^[a-f0-9]{64}$'),
    content_key text NOT NULL CHECK (content_key ~ '^[a-f0-9]{64}$'),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY(repository,issue_number),
    FOREIGN KEY(repository,issue_number,generation) REFERENCES intake_revisions(repository,issue_number,generation) ON DELETE CASCADE
);
ALTER TABLE intake_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE intake_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY intake_observations_read ON intake_observations FOR SELECT
    USING(current_user IN ('aisdlc_intake','aisdlc_worker'));
CREATE POLICY intake_observations_insert ON intake_observations FOR INSERT
    WITH CHECK(current_user='aisdlc_intake');
CREATE POLICY intake_observations_update ON intake_observations FOR UPDATE
    USING(current_user='aisdlc_intake') WITH CHECK(current_user='aisdlc_intake');
