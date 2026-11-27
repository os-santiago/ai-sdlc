package store

import (
	"context"
	"database/sql"
	"time"
)

type MergeSelection struct {
	AuthorityRevision, IntakeConfiguration, ExecutionProfile, ReviewProfile, ReviewConfiguration string
	// RecipeRevision is the current verification recipe revision. A fact
	// whose verification command was signed under another one (before an
	// upgrade that changed the rules) can never pass the proof check, so
	// selecting it would block every later publication.
	RecipeRevision string
}

// NextMergePublication selects a historical publication without any merge
// attempt, or whose every attempt failed "not-sent" (provably nothing sent),
// fewer than MergeAttempts times and with the last command expired more than
// 30 s ago. It never queues authority, replays a signed command or selects a
// publication with a queued, claimed, merged, uncertain or expired attempt.
func (db *DB) NextMergePublication(ctx context.Context, selection MergeSelection) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, value := range []string{selection.AuthorityRevision, selection.IntakeConfiguration, selection.ExecutionProfile, selection.ReviewProfile, selection.ReviewConfiguration, selection.RecipeRevision} {
		if !modelDigest.MatchString(value) {
			return "", ErrExecution
		}
	}
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return "", ErrExecution
	}
	var id string
	e := db.Pool.QueryRowContext(ctx, `SELECT f.publication_id FROM published_records f
 JOIN publication_requests p ON p.id=f.publication_id
 JOIN execution_requests e ON e.id=p.candidate_id
 JOIN review_runs r ON r.id=p.review_id
 JOIN verification_requests v ON v.id=p.verification_id
 JOIN work_items w ON w.uuid=e.work_item_uuid
 JOIN intake_heads h ON h.repository=e.repository
  AND h.issue_number=(convert_from(e.payload,'UTF8')::jsonb->>'issueNumber')::integer
  AND h.generation=(convert_from(e.payload,'UTF8')::jsonb->>'generation')::bigint
 JOIN intake_revisions i ON i.repository=h.repository AND i.issue_number=h.issue_number AND i.generation=h.generation
 WHERE p.state='published' AND e.authority_revision=$1 AND i.config_digest=$2
  AND convert_from(e.payload,'UTF8')::jsonb->>'profileSHA256'=$3
  AND r.profile_digest=$4 AND r.configuration_digest=$5 AND r.state='passed' AND r.cleanup_state='complete'
  AND w.status='running' AND convert_from(v.payload,'UTF8')::jsonb->>'recipeRevision'=$6
  AND `+mergeAttemptAllowed("f.publication_id")+`
 ORDER BY f.published_at,f.publication_id LIMIT 1`, selection.AuthorityRevision, selection.IntakeConfiguration, selection.ExecutionProfile, selection.ReviewProfile, selection.ReviewConfiguration, selection.RecipeRevision).Scan(&id)
	if e == sql.ErrNoRows {
		return "", nil
	}
	if e != nil {
		return "", observationError(e)
	}
	return id, nil
}
