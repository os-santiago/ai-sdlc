"""Pin complete history under write exclusion, after ordinary intake polling."""

POLLING = {'intake_cursors', 'intake_observations'}

QUERY = """SELECT NOT (
EXISTS(SELECT 1 FROM intake_cursors c WHERE NOT EXISTS(
 SELECT 1 FROM intake_revisions r JOIN intake_heads h USING(repository,issue_number,generation)
 WHERE r.repository=c.repository AND r.config_digest=c.config_digest))
OR EXISTS(SELECT 1 FROM intake_observations o
 LEFT JOIN intake_heads h USING(repository,issue_number)
 LEFT JOIN intake_revisions r ON r.repository=o.repository AND r.issue_number=o.issue_number AND r.generation=o.generation
 WHERE h.generation IS DISTINCT FROM o.generation OR h.content_key IS DISTINCT FROM o.content_key
 OR r.config_digest IS DISTINCT FROM o.config_digest
 OR (convert_from(r.payload,'UTF8')::jsonb->>'authorityRevision') IS DISTINCT FROM o.authority_revision)
OR EXISTS(SELECT 1 FROM intake_heads h WHERE h.generation>0 AND NOT EXISTS(
 SELECT 1 FROM intake_observations o WHERE o.repository=h.repository AND o.issue_number=h.issue_number))
);"""


def validate(preliminary, current, database):
    if (set(preliminary) != set(current)
            or any(preliminary[key] != current[key] for key in preliminary if key not in POLLING)
            or any(preliminary[key]['rows'] != current[key]['rows'] for key in POLLING)):
        raise ValueError('Original non-polling history changed before the first pause')
    if database.sql(QUERY) != 't\n':
        raise ValueError('Polling identities differ from original retained issue history')
