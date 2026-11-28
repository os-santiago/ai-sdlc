package store

import (
	"context"
	"database/sql"
	"strings"
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

// MaxMergeRotation bounds the publications a caller may order or park in
// one selection.
const MaxMergeRotation = 256

// MergeRotation orders selection so one waiting publication cannot hold every
// later one. Recent lists publications in the order they were last observed,
// least recently first; a candidate not listed has never been observed and
// is selected before any listed one. Parked publications (for example one
// whose pull request was closed) are not selected. Both hold publication IDs
// only; an empty rotation selects the oldest publication, as before.
type MergeRotation struct {
	Recent, Parked []string
}

func (r MergeRotation) arrays() (string, string, bool) {
	if len(r.Recent) > MaxMergeRotation || len(r.Parked) > MaxMergeRotation {
		return "", "", false
	}
	for _, list := range [][]string{r.Recent, r.Parked} {
		for _, id := range list {
			if !publicationIDPattern.MatchString(id) {
				return "", "", false
			}
		}
	}
	// IDs are validated to [a-z0-9-], so the array literal needs no quoting.
	return "{" + strings.Join(r.Recent, ",") + "}", "{" + strings.Join(r.Parked, ",") + "}", true
}

// NextMergePublication selects the oldest eligible publication. It is
// NextRotatedMergePublication with an empty rotation.
func (db *DB) NextMergePublication(ctx context.Context, selection MergeSelection) (string, error) {
	return db.NextRotatedMergePublication(ctx, selection, MergeRotation{})
}

// NextRotatedMergePublication selects a historical publication without any merge
// attempt, or whose every attempt failed "not-sent" (provably nothing sent),
// fewer than MergeAttempts times and with the last command expired at least
// the scheduled wait (MergeRetryWaits) ago. It never queues authority, replays a signed command or selects a
// publication with a queued, claimed, merged, uncertain or expired attempt.
// Among eligible publications it takes one never observed by the rotation
// first, then the least recently observed, and skips parked ones; ties keep
// publication order.
func (db *DB) NextRotatedMergePublication(ctx context.Context, selection MergeSelection, rotation MergeRotation) (string, error) {
	recent, parked, ok := rotation.arrays()
	if !ok {
		return "", ErrExecution
	}
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
  AND NOT (f.publication_id = ANY($8::text[]))
 ORDER BY array_position($7::text[], f.publication_id) NULLS FIRST, f.published_at, f.publication_id LIMIT 1`, selection.AuthorityRevision, selection.IntakeConfiguration, selection.ExecutionProfile, selection.ReviewProfile, selection.ReviewConfiguration, selection.RecipeRevision, recent, parked).Scan(&id)
	if e == sql.ErrNoRows {
		return "", nil
	}
	if e != nil {
		return "", observationError(e)
	}
	return id, nil
}
