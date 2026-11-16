package store

import (
	"context"
	"database/sql"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"time"
)

// Selection grants no effect and does not change the original claim or expiry.
// The controller must hold WithDeploymentController across recovery and effects.
func (j *DeploymentJournal) NextDeploymentRecovery(ctx context.Context) (DeploymentClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var c DeploymentClaim
	if j == nil || j.DB == nil || AssertComponent(ctx, j.Pool, "deployer") != nil {
		return c, ErrExecution
	}
	e := j.Pool.QueryRowContext(ctx, `SELECT id,claim_token,expires_at FROM deployment_requests WHERE state='claimed' ORDER BY created_at,id LIMIT 1`).Scan(&c.ID, &c.Token, &c.Until)
	if e == sql.ErrNoRows {
		return DeploymentClaim{}, nil
	}
	if e != nil {
		return DeploymentClaim{}, ErrExecution
	}
	return c, nil
}

type DeploymentRecovery struct {
	Configuration releasing.Configuration
	Advance       releasing.Receipt
	Rollback      releasing.Receipt
	RolledBack    bool
}

// Only an authenticated durable API acknowledgement can be recovered. An
// unsent intent and a sent-but-unacknowledged effect both remain uncertain:
// current image equality can never manufacture a missing receipt.
func (j *DeploymentJournal) ReadDeploymentRecovery(ctx context.Context, c DeploymentClaim) (DeploymentRecovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out DeploymentRecovery
	e := j.WithTx(ctx, func(tx *sql.Tx) error {
		cfg, input, e := j.check(ctx, tx, c, true)
		if e != nil {
			return e
		}
		out.Configuration = cfg
		out.Advance, e = readDeploymentReceipt(ctx, tx, c, "advance", cfg, input)
		if e != nil {
			return e
		}
		var intent, receipt bool
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_intents WHERE request_id=$1 AND kind='rollback'),EXISTS(SELECT 1 FROM deployment_receipts WHERE request_id=$1 AND kind='rollback')`, c.ID).Scan(&intent, &receipt) != nil {
			return ErrExecution
		}
		if intent != receipt {
			return ErrExecution
		}
		if receipt {
			out.Rollback, e = readDeploymentReceipt(ctx, tx, c, "rollback", cfg, input)
			if e != nil {
				return e
			}
			out.RolledBack = true
		}
		return nil
	})
	if e != nil || ctx.Err() != nil {
		return DeploymentRecovery{}, ErrExecution
	}
	return out, nil
}
