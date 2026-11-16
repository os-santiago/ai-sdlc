package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"time"
)

// Original authenticated authority is used for recording facts after a freeze.
// Recording a fact grants no new effect authority. Expired/uncertain claims are
// quarantined, not replayed or adopted from current Kubernetes image equality.
func deploymentArchived(ctx context.Context, q Querier, c DeploymentClaim) (releasing.Configuration, DeploymentInput, error) {
	var cfg releasing.Configuration
	var r DeploymentInput
	if !deploymentID.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || AssertComponent(ctx, q, "deployer") != nil {
		return cfg, r, ErrExecution
	}
	var raw, signed, config []byte
	var hash string
	var expires time.Time
	var command deployment.Command
	if q.QueryRowContext(ctx, `SELECT payload,receipt,configuration,payload_digest,expires_at FROM deployment_requests WHERE id=$1 AND claim_token=$2 AND state IN('claimed','deployed','rolled-back','quarantined')`, c.ID, c.Token).Scan(&raw, &signed, &config, &hash, &expires) != nil || hash != authority.Digest(raw) || !expires.Equal(c.Until) || authority.DecodeStrict(raw, &command) != nil || authority.DecodeStrict(config, &cfg) != nil {
		return cfg, r, ErrExecution
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(signed, &receipt) != nil {
		return cfg, r, ErrExecution
	}
	verified, e := deployment.VerifyCommand(cfg, receipt, raw, time.UnixMilli(command.IssuedAt))
	if e != nil || verified != command || command.ID() != c.ID || !expires.Equal(time.UnixMilli(command.ExpiresAt)) {
		return cfg, r, ErrExecution
	}
	// History reader authenticates every completed upstream stage. Bind at the
	// original issuance time so recording an ACK cannot renew execution authority.
	r.Command = command
	r.Promotion, e = readPromotionRecord(ctx, q, command.PromotionID, "deployer")
	if e != nil {
		return cfg, r, ErrExecution
	}
	r.Provenance, e = readProvenanceRecord(ctx, q, r.Promotion.Command.ProvenanceID, "deployer")
	if e != nil {
		return cfg, r, ErrExecution
	}
	r.Build, e = readBuiltRecord(ctx, q, r.Provenance.Command.BuildID, "deployer")
	if e != nil || command.BindRecords(cfg, r.Promotion, r.Provenance, r.Build, time.UnixMilli(command.IssuedAt)) != nil {
		return cfg, r, ErrExecution
	}
	return cfg, r, nil
}
func readDeploymentIntent(ctx context.Context, q Querier, c DeploymentClaim, kind string) (releasing.Snapshot, string, time.Time, error) {
	var before releasing.Snapshot
	var raw []byte
	var hash string
	var at time.Time
	if q.QueryRowContext(ctx, `SELECT i.before_snapshot,i.request_digest,s.created_at FROM deployment_intents i JOIN deployment_sends s ON s.request_id=i.request_id AND s.kind=i.kind AND s.claim_token=i.claim_token WHERE i.request_id=$1 AND i.kind=$2 AND i.claim_token=$3 AND s.created_at>=i.created_at AND s.created_at<=clock_timestamp()`, c.ID, kind, c.Token).Scan(&raw, &hash, &at) != nil || authority.DecodeStrict(raw, &before) != nil {
		return before, "", at, ErrExecution
	}
	return before, hash, at, nil
}
func readDeploymentReceipt(ctx context.Context, q Querier, c DeploymentClaim, kind string, cfg releasing.Configuration, r DeploymentInput) (releasing.Receipt, error) {
	var result releasing.Receipt
	var raw []byte
	var hash string
	var at time.Time
	before, request, sent, e := readDeploymentIntent(ctx, q, c, kind)
	if e != nil {
		return result, e
	}
	if q.QueryRowContext(ctx, `SELECT receipt,digest,recorded_at FROM deployment_receipts WHERE request_id=$1 AND kind=$2 AND recorded_at<=clock_timestamp()`, c.ID, kind).Scan(&raw, &hash, &at) != nil || authority.Digest(raw) != hash || authority.DecodeStrict(raw, &result) != nil || at.Before(sent) || deployment.ValidateReceipt(cfg.Target, r.Promotion.Plan, before, request, kind == "rollback", result, sent, at) != nil {
		return releasing.Receipt{}, ErrExecution
	}
	return result, nil
}
func (j *DeploymentJournal) RecordDeploymentReceipt(ctx context.Context, c DeploymentClaim, r releasing.Receipt) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.WithTx(ctx, func(tx *sql.Tx) error {
		var state string
		if tx.QueryRowContext(ctx, `SELECT state FROM deployment_requests WHERE id=$1 AND claim_token=$2 AND state='claimed' FOR UPDATE`, c.ID, c.Token).Scan(&state) != nil {
			return ErrExecution
		}
		cfg, input, e := deploymentArchived(ctx, tx, c)
		if e != nil {
			return e
		}
		kind := effectKind(r.Rollback)
		before, hash, sent, e := readDeploymentIntent(ctx, tx, c, kind)
		if e != nil || deployment.ValidateReceipt(cfg.Target, input.Promotion.Plan, before, hash, r.Rollback, r, sent, time.Now()) != nil {
			return ErrExecution
		}
		raw, _ := json.Marshal(r)
		if _, e = tx.ExecContext(ctx, `INSERT INTO deployment_receipts(request_id,kind,digest,receipt)VALUES($1,$2,$3,$4)`, c.ID, kind, authority.Digest(raw), raw); e != nil {
			return ErrExecution
		}
		return nil
	})
}
func (j *DeploymentJournal) ReadDeploymentReceipt(ctx context.Context, c DeploymentClaim, rollback bool) (releasing.Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, r, e := deploymentArchived(ctx, j.Pool, c)
	if e != nil {
		return releasing.Receipt{}, e
	}
	return readDeploymentReceipt(ctx, j.Pool, c, effectKind(rollback), cfg, r)
}
func (j *DeploymentJournal) FinishDeployment(ctx context.Context, c DeploymentClaim, h releasing.Health, rollback bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.WithTx(ctx, func(tx *sql.Tx) error {
		cfg, r, e := j.check(ctx, tx, c, rollback)
		if e != nil {
			return e
		}
		kind := effectKind(rollback)
		receipt, e := readDeploymentReceipt(ctx, tx, c, kind, cfg, r)
		if e != nil || deployment.ValidateHealth(cfg.Target, receipt, h, time.Now()) != nil {
			return ErrExecution
		}
		if !rollback {
			var exists bool
			if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_intents WHERE request_id=$1 AND kind='rollback')`, c.ID).Scan(&exists) != nil || exists {
				return ErrExecution
			}
		}
		raw, _ := json.Marshal(h)
		if _, e = tx.ExecContext(ctx, `INSERT INTO deployment_health(request_id,kind,digest,health)VALUES($1,$2,$3,$4)`, c.ID, kind, authority.Digest(raw), raw); e != nil {
			return ErrExecution
		}
		if _, _, e = j.check(ctx, tx, c, rollback); e != nil {
			return e
		}
		state := "deployed"
		if rollback {
			state = "rolled-back"
		}
		result, e := tx.ExecContext(ctx, `UPDATE deployment_requests SET state=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed' AND expires_at>clock_timestamp()`, c.ID, c.Token, state)
		if e != nil {
			return ErrExecution
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		if rollback {
			return j.containmentCurrent(ctx, cfg, r.Command)
		}
		return j.stillCurrent(ctx, r)
	})
}
func (j *DeploymentJournal) QuarantineDeployment(ctx context.Context, c DeploymentClaim, reason string) error {
	if reason != "expired" && reason != "revoked" && reason != "uncertain" && reason != "rejected" && reason != "unhealthy" {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if !deploymentID.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || AssertComponent(ctx, j.Pool, "deployer") != nil {
		return ErrExecution
	}
	result, e := j.Pool.ExecContext(ctx, `UPDATE deployment_requests SET state='quarantined',reason=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token, reason)
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (j *DeploymentJournal) ExpireDeployments(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "deployer") != nil {
		return ErrExecution
	}
	_, e := j.Pool.ExecContext(ctx, `UPDATE deployment_requests SET state='quarantined',reason='expired' WHERE state IN('queued','claimed') AND expires_at<=clock_timestamp()`)
	if e != nil {
		return ErrExecution
	}
	return nil
}

// DeploymentRecord is historical evidence, not authority for another effect.
// The authenticated reader can resolve lost terminal COMMIT acknowledgements.
type DeploymentRecord struct {
	Command       deployment.Command
	Configuration releasing.Configuration
	Plan          releasing.Plan
	Receipt       releasing.Receipt
	Health        releasing.Health
	Outcome       string
	RecordedAt    time.Time
}

func (r DeploymentRecord) Digest() string { raw, _ := json.Marshal(r); return authority.Digest(raw) }
func (j *DeploymentJournal) ReadDeploymentRecord(ctx context.Context, c DeploymentClaim) (DeploymentRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, input, e := deploymentArchived(ctx, j.Pool, c)
	if e != nil {
		return DeploymentRecord{}, e
	}
	var state, kind, hash string
	var raw []byte
	var at time.Time
	if j.Pool.QueryRowContext(ctx, `SELECT q.state,h.kind,h.digest,h.health,h.recorded_at FROM deployment_requests q JOIN deployment_health h ON h.request_id=q.id WHERE q.id=$1 AND q.claim_token=$2 AND q.state IN('deployed','rolled-back') AND h.recorded_at<q.expires_at AND h.recorded_at>=q.created_at AND h.recorded_at<=clock_timestamp()`, c.ID, c.Token).Scan(&state, &kind, &hash, &raw, &at) != nil || authority.Digest(raw) != hash || (state == "deployed" && kind != "advance") || (state == "rolled-back" && kind != "rollback") {
		return DeploymentRecord{}, ErrExecution
	}
	receipt, e := readDeploymentReceipt(ctx, j.Pool, c, kind, cfg, input)
	var health releasing.Health
	if e != nil || authority.DecodeStrict(raw, &health) != nil || deployment.ValidateHealth(cfg.Target, receipt, health, at) != nil {
		return DeploymentRecord{}, ErrExecution
	}
	return DeploymentRecord{input.Command, cfg, input.Promotion.Plan, receipt, health, state, at}, nil
}
