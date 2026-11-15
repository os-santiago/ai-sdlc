package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"time"
)

var lifecycleDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var lifecycleUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// LifecycleBinding is immutable persistence input, never authorization. The
// execution boundary derives it from a freshly verified Prepared value.
type LifecycleBinding struct {
	BindingDigest, SourceDigest, Namespace, PodName, ConfigName string
	MaxSeconds, SourceBytes                                     int
}
type Lifecycle struct {
	Claim   ExecutionClaim
	Binding LifecycleBinding
}
type LifecycleEffect struct {
	Lifecycle         Lifecycle
	Kind, Token       string
	ConfigUID, PodUID string
}

// LifecycleResult records observed evidence, never acceptance or release.
type LifecycleResult struct {
	UID      string `json:"uid"`
	Digest   string `json:"digest"`
	Bytes    int    `json:"bytes"`
	ExitCode *int32 `json:"exitCode"`
}

var lifecycleBefore = map[string]string{"config": "reserved", "pod": "config-ready", "source": "pod-ready", "start": "source-ready", "export": "start-ready"}

func lifecycleCurrent(ctx context.Context, tx *sql.Tx, claim ExecutionClaim) error {
	var valid bool
	if ctx.Err() != nil || tx.QueryRowContext(ctx, `SELECT state='claimed' AND claim_token=$2 AND claim_until=$3 AND claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM execution_requests WHERE id=$1`, claim.ID, claim.Token, claim.Until).Scan(&valid) != nil || !valid {
		return ErrExecution
	}
	return nil
}

func lockLifecycleClaim(ctx context.Context, tx *sql.Tx, claim ExecutionClaim) error {
	if AssertComponent(ctx, tx, "worker") != nil {
		return ErrExecution
	}
	var id string
	if tx.QueryRowContext(ctx, `SELECT id FROM execution_requests WHERE id=$1 AND claim_token=$2 AND claim_until=$3 AND state='claimed' FOR UPDATE`, claim.ID, claim.Token, claim.Until).Scan(&id) != nil {
		return ErrExecution
	}
	// This second statement checks database time after any row-lock wait.
	var valid bool
	if tx.QueryRowContext(ctx, `SELECT claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM execution_requests WHERE id=$1`, claim.ID).Scan(&valid) != nil || !valid {
		return ErrExecution
	}
	return nil
}

// ReserveLifecycle transfers a live preparation claim exactly once. The signed
// command and reviewed profile bound the nonrenewable lifetime. No existing
// reservation is adopted, even by the same token or after a lost COMMIT ACK.
func (db *DB) ReserveLifecycle(ctx context.Context, claim ExecutionClaim, binding LifecycleBinding) (Lifecycle, error) {
	if binding.SourceBytes < 1 || binding.SourceBytes > 32*1024*1024 || binding.MaxSeconds < 1 || binding.MaxSeconds > 900 || !lifecycleDigest.MatchString(binding.BindingDigest) || !lifecycleDigest.MatchString(binding.SourceDigest) {
		return Lifecycle{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := Lifecycle{Claim: claim, Binding: binding}
	result.Claim.Token = NewID()
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockLifecycleClaim(ctx, tx, claim) != nil {
			return ErrExecution
		}
		// The uniqueness constraint below prevents a second lifetime extension;
		// transaction rollback also restores this request update on any conflict.
		if tx.QueryRowContext(ctx, `UPDATE execution_requests SET claim_token=$3,claim_until=LEAST(expires_at,clock_timestamp()+$2*interval '1 second'),updated_at=clock_timestamp() WHERE id=$1 AND claim_until>clock_timestamp() AND expires_at>clock_timestamp() RETURNING claim_until`, claim.ID, binding.MaxSeconds+120, result.Claim.Token).Scan(&result.Claim.Until) != nil {
			return ErrExecution
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO execution_lifecycles(request_id,claim_token,claim_until,binding_digest,source_digest,namespace,pod_name,config_name,source_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, claim.ID, result.Claim.Token, result.Claim.Until, binding.BindingDigest, binding.SourceDigest, binding.Namespace, binding.PodName, binding.ConfigName, binding.SourceBytes)
		if err != nil {
			return ErrExecution
		}
		return lifecycleCurrent(ctx, tx, result.Claim)
	})
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = db.StopExecutionClaim(cleanup, result.Claim, "uncertain")
		return Lifecycle{}, ErrExecution
	}
	return result, nil
}

func lockLifecycle(ctx context.Context, tx *sql.Tx, l Lifecycle) (string, string, string, error) {
	if lockLifecycleClaim(ctx, tx, l.Claim) != nil {
		return "", "", "", ErrExecution
	}
	var phase, configUID, podUID string
	err := tx.QueryRowContext(ctx, `SELECT phase,COALESCE(config_uid::text,''),COALESCE(pod_uid::text,'') FROM execution_lifecycles WHERE request_id=$1 AND claim_token=$2 AND claim_until=$3 AND binding_digest=$4 AND source_digest=$5 AND namespace=$6 AND pod_name=$7 AND config_name=$8 AND source_bytes=$9 FOR UPDATE`, l.Claim.ID, l.Claim.Token, l.Claim.Until, l.Binding.BindingDigest, l.Binding.SourceDigest, l.Binding.Namespace, l.Binding.PodName, l.Binding.ConfigName, l.Binding.SourceBytes).Scan(&phase, &configUID, &podUID)
	if err != nil {
		return "", "", "", ErrExecution
	}
	// A lifecycle row can itself have been locked by another transaction.
	var valid bool
	if tx.QueryRowContext(ctx, `SELECT claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM execution_requests WHERE id=$1`, l.Claim.ID).Scan(&valid) != nil || !valid {
		return "", "", "", ErrExecution
	}
	return phase, configUID, podUID, nil
}

func (db *DB) BeginLifecycleEffect(ctx context.Context, l Lifecycle, kind string) (LifecycleEffect, error) {
	before, ok := lifecycleBefore[kind]
	if !ok {
		return LifecycleEffect{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	effect := LifecycleEffect{Lifecycle: l, Kind: kind, Token: NewID()}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		phase, configUID, podUID, err := lockLifecycle(ctx, tx, l)
		if err != nil || phase != before {
			return ErrExecution
		}
		effect.ConfigUID = configUID
		effect.PodUID = podUID
		if _, err = tx.ExecContext(ctx, `INSERT INTO execution_effects(request_id,kind,token) VALUES($1,$2,$3)`, l.Claim.ID, kind, effect.Token); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE execution_lifecycles SET phase=$2,updated_at=clock_timestamp() WHERE request_id=$1`, l.Claim.ID, kind+"-issued")
		if err != nil {
			return ErrExecution
		}
		return lifecycleCurrent(ctx, tx, l.Claim)
	})
	if err != nil {
		return LifecycleEffect{}, ErrExecution
	}
	return effect, nil
}

func (db *DB) CompleteLifecycleEffect(ctx context.Context, e LifecycleEffect, result LifecycleResult) error {
	if _, ok := lifecycleBefore[e.Kind]; !ok || !lifecycleUUID.MatchString(e.Token) || !lifecycleUUID.MatchString(result.UID) {
		return ErrExecution
	}
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > 2048 {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		phase, _, podUID, err := lockLifecycle(ctx, tx, e.Lifecycle)
		if err != nil || phase != e.Kind+"-issued" {
			return ErrExecution
		}
		switch e.Kind {
		case "config", "pod":
			if result.Digest != "" || result.Bytes != 0 || result.ExitCode != nil {
				return ErrExecution
			}
		case "source":
			if result.UID != podUID || result.Digest != e.Lifecycle.Binding.SourceDigest || result.Bytes != e.Lifecycle.Binding.SourceBytes || result.ExitCode != nil {
				return ErrExecution
			}
		case "start":
			if result.UID != podUID || result.Digest != "" || result.Bytes != 0 || result.ExitCode != nil {
				return ErrExecution
			}
		case "export":
			if result.UID != podUID || !lifecycleDigest.MatchString(result.Digest) || result.Bytes < 1 || result.Bytes > 32*1024*1024 || result.ExitCode == nil {
				return ErrExecution
			}
		}
		r, err := tx.ExecContext(ctx, `UPDATE execution_effects SET result=$4,completed_at=clock_timestamp() WHERE request_id=$1 AND kind=$2 AND token=$3 AND result IS NULL`, e.Lifecycle.Claim.ID, e.Kind, e.Token, raw)
		if err != nil {
			return ErrExecution
		}
		n, err := r.RowsAffected()
		if err != nil || n != 1 {
			return ErrExecution
		}
		_, err = tx.ExecContext(ctx, `UPDATE execution_lifecycles SET phase=$2,config_uid=CASE WHEN $3='config' THEN $4::uuid ELSE config_uid END,pod_uid=CASE WHEN $3='pod' THEN $4::uuid ELSE pod_uid END,updated_at=clock_timestamp() WHERE request_id=$1`, e.Lifecycle.Claim.ID, e.Kind+"-ready", e.Kind, result.UID)
		if err != nil {
			return ErrExecution
		}
		return lifecycleCurrent(ctx, tx, e.Lifecycle.Claim)
	})
}
