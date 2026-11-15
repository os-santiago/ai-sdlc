package store

import (
	"context"
	"time"
)

// ModelScope contains immutable bindings, not execution authority. Operational
// callers must still hold reviewed controller authority; no role is granted
// access to this hidden ledger by this read method.
type ModelScope struct {
	AllocationID, WorkItem, PodUID, Repository, SourceSHA, PolicyDigest, Model string
	Attempt, ResponseLimit                                                     int
	ExpiresAt                                                                  time.Time
}

func (db *DB) ReadModelScope(ctx context.Context, allocation string) (ModelScope, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !modelUUID.MatchString(allocation) {
		return ModelScope{}, errModelPolicy
	}
	var s ModelScope
	err := db.Pool.QueryRowContext(ctx, `SELECT a.id,a.work_item_uuid,a.pod_uid,b.repository,b.source_sha,b.policy_digest,b.model,a.attempt,a.response_limit,LEAST(a.expires_at,b.expires_at) FROM model_allocations a JOIN model_budgets b ON b.work_item_uuid=a.work_item_uuid WHERE a.id=$1`, allocation).Scan(&s.AllocationID, &s.WorkItem, &s.PodUID, &s.Repository, &s.SourceSHA, &s.PolicyDigest, &s.Model, &s.Attempt, &s.ResponseLimit, &s.ExpiresAt)
	if err != nil {
		return ModelScope{}, err
	}
	return s, nil
}
