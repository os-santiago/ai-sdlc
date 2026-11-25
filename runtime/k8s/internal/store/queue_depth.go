package store

import (
	"context"
	"errors"
	"time"
)

// ErrQueueDepth hides the cause of a failed or refused queue sample.
var ErrQueueDepth = errors.New("queue depth unavailable")

// MaxQueueDepth caps a sample: each count reads at most this many rows plus one.
const MaxQueueDepth = 10000

// queueDepthQueries are the fixed, read-only counts each component may run on
// tables it already reads. Live queued rows only; never a row's content.
var queueDepthQueries = map[string]map[string]string{
	"worker":          {"execution": `SELECT count(*) FROM (SELECT 1 FROM execution_requests WHERE state='queued' AND expires_at>clock_timestamp() LIMIT 10001) q`},
	"verifier":        {"verification": `SELECT count(*) FROM (SELECT 1 FROM verification_requests WHERE state='queued' AND expires_at>clock_timestamp() LIMIT 10001) q`, "review": `SELECT count(*) FROM (SELECT 1 FROM review_runs WHERE state='queued' AND expires_at>clock_timestamp() LIMIT 10001) q`},
	"release-manager": {"publication": `SELECT count(*) FROM (SELECT 1 FROM publication_requests WHERE state='queued' AND expires_at>clock_timestamp() LIMIT 10001) q`},
}

// outboxDepthQuery counts the caller's own undelivered signed messages; row
// security already limits a producer to its own rows.
const outboxDepthQuery = `SELECT count(*) FROM (SELECT 1 FROM authorized_outbox WHERE producer=$1 AND published_at IS NULL AND NOT exhausted LIMIT 10001) q`

// QueueDepth counts the work waiting in one of component's queues: stage
// "outbox" for every component, or a queue the component owns. The count is
// capped at MaxQueueDepth. It confers no authority and reads no content.
func (db *DB) QueueDepth(ctx context.Context, component, stage string) (int64, error) {
	query, args := "", []any{}
	if stage == "outbox" {
		if _, err := ComponentRole(component); err != nil || component == "relay" {
			return 0, ErrQueueDepth
		}
		query, args = outboxDepthQuery, []any{component}
	} else if query = queueDepthQueries[component][stage]; query == "" {
		return 0, ErrQueueDepth
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, component) != nil {
		return 0, ErrQueueDepth
	}
	var depth int64
	if err := db.Pool.QueryRowContext(ctx, query, args...).Scan(&depth); err != nil {
		return 0, ErrQueueDepth
	}
	if depth > MaxQueueDepth {
		depth = MaxQueueDepth
	}
	return depth, nil
}
