package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type PolicyLoader func() (authority.Policy, error)

// Effect performs only durable inbox work. It is deliberately not registered
// on the installed worker while controller integration remains incomplete.
func Effect(load PolicyLoader) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, receipt authority.Receipt, raw []byte) ([]byte, error) {
		p, err := load()
		if err != nil {
			return nil, ErrAuthority
		}
		c, err := Verify(p, receipt, raw, time.Now())
		if err != nil {
			return nil, err
		}
		sealed, err := json.Marshal(receipt)
		if err != nil {
			return nil, ErrAuthority
		}
		err = store.InsertExecution(ctx, tx, store.ExecutionRequest{ID: c.ID(), WorkItem: c.WorkItemID, Attempt: c.Attempt,
			Repository: c.Repository, AuthorityRevision: c.AuthorityRevision, PayloadDigest: authority.Digest(raw),
			Payload: raw, Receipt: sealed, ExpiresAt: time.UnixMilli(c.ExpiresAt)})
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"executionId": c.ID(), "state": "queued"})
	}
}

// Claim revalidates stored signed authority against the current policy before
// requesting a single durable claim. Current observed admission/readiness is
// required too; a later controller must revalidate before each external effect.
func Claim(ctx context.Context, db *store.DB, load ReadinessLoader, id string) (store.ExecutionClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := db.ReadExecution(ctx, id)
	if err != nil {
		return store.ExecutionClaim{}, err
	}
	p, config, err := load()
	if err != nil {
		return store.ExecutionClaim{}, ErrAuthority
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(r.Receipt, &receipt) != nil || authority.Digest(r.Payload) != r.PayloadDigest {
		return store.ExecutionClaim{}, ErrAuthority
	}
	c, err := Verify(p, receipt, r.Payload, time.Now())
	if err != nil || c.ID() != r.ID || c.WorkItemID != r.WorkItem || c.Attempt != r.Attempt || c.Repository != r.Repository ||
		c.AuthorityRevision != r.AuthorityRevision || !time.UnixMilli(c.ExpiresAt).Equal(r.ExpiresAt) {
		return store.ExecutionClaim{}, ErrAuthority
	}
	if readiness(ctx, db, p, config, c) != nil {
		return store.ExecutionClaim{}, ErrAuthority
	}
	claim, err := db.ClaimExecution(ctx, id, r.PayloadDigest)
	if err != nil {
		return store.ExecutionClaim{}, err
	}
	// Locks and COMMIT acknowledgement can delay the transaction. Recheck the
	// mounted policy and lifetime after that final wait, before returning any
	// capability to a controller. Future effects need their own current check.
	current, currentConfig, policyErr := load()
	currentConfigDigest := currentConfig.Digest() // Immutable across a loader reusing its actor slice.
	_, verifyErr := Verify(current, receipt, r.Payload, time.Now())
	readyErr := readiness(ctx, db, current, currentConfig, c)
	// Policy and readiness checks are adjacent but not a lock on future changes.
	// Reload policy once more after the bounded database read.
	latest, latestConfig, latestErr := load()
	_, latestVerify := Verify(latest, receipt, r.Payload, time.Now())
	if policyErr != nil || verifyErr != nil || readyErr != nil || latestErr != nil || latestVerify != nil || latestConfig.Digest() != currentConfigDigest || !claim.Until.After(time.Now()) || ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = db.StopExecutionClaim(cleanup, claim, "authority-revoked")
		return store.ExecutionClaim{}, ErrAuthority
	}
	return claim, nil
}
