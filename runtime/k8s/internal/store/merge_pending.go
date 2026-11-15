package store

import (
	"context"
	"database/sql"
	"time"
)

// NextMergePublication selects a historical publication without any merge
// attempt. It never queues authority or replays a failed/uncertain attempt.
func (db *DB) NextMergePublication(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return "", ErrExecution
	}
	var id string
	e := db.Pool.QueryRowContext(ctx, `SELECT f.publication_id FROM published_records f JOIN publication_requests p ON p.id=f.publication_id WHERE p.state='published' AND NOT EXISTS(SELECT 1 FROM merge_requests m WHERE m.publication_id=f.publication_id) ORDER BY f.published_at,f.publication_id LIMIT 1`).Scan(&id)
	if e == sql.ErrNoRows {
		return "", nil
	}
	if e != nil {
		return "", observationError(e)
	}
	return id, nil
}
